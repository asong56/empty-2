// fcm.go — Firebase Cloud Messaging Push Dispatcher
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	fcmLegacyEndpoint = "https://fcm.googleapis.com/fcm/send"
	fcmTimeout        = 10 * time.Second
	fcmMaxRetries     = 3
)

type fcmLegacyPayload struct {
	To          string            `json:"to"`
	Priority    string            `json:"priority"`
	Data        map[string]string `json:"data"`
	TimeToLive  int               `json:"time_to_live"`
	CollapseKey string            `json:"collapse_key,omitempty"`
}

type fcmResponse struct {
	Success int         `json:"success"`
	Failure int         `json:"failure"`
	Results []fcmResult `json:"results"`
	Error   *fcmError   `json:"error,omitempty"`
}

type fcmResult struct {
	MessageID      string `json:"message_id"`
	RegistrationID string `json:"registration_id"`
	Error          string `json:"error"`
}

type fcmError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type FCMDispatcher struct {
	db         *sql.DB
	serverKey  string
	projectID  string
	httpClient *http.Client
}

func NewFCMDispatcher(db *sql.DB) *FCMDispatcher {
	serverKey := os.Getenv("TARDI_FCM_SERVER_KEY")
	projectID := os.Getenv("TARDI_FCM_PROJECT_ID")
	if serverKey == "" && projectID == "" {
		log.Println("[fcm] FCM push disabled (no credentials)")
		return nil
	}
	return &FCMDispatcher{
		db:         db,
		serverKey:  serverKey,
		projectID:  projectID,
		httpClient: &http.Client{Timeout: fcmTimeout},
	}
}

func (d *FCMDispatcher) NotifyNewMessage(msg *Message) {
	if d == nil {
		return
	}
	tokens, err := d.getAllTokens()
	if err != nil {
		log.Printf("[fcm] load tokens: %v", err)
		return
	}
	if len(tokens) == 0 {
		return
	}
	data := buildNotificationData(msg)
	for _, tok := range tokens {
		go func(token string) {
			prefix := token
			if len(prefix) > 8 {
				prefix = prefix[:8]
			}
			if err := d.sendWithRetry(token, data); err != nil {
				log.Printf("[fcm] push to %s…: %v", prefix, err)
			}
		}(tok)
	}
}

func buildNotificationData(msg *Message) map[string]string {
	body := msg.Body
	if body == "" && msg.Redirected != nil {
		body = msg.Redirected.DisplayLabel
	}
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return map[string]string{
		"event": "new_message", "msg_id": msg.ID, "thread_id": msg.ThreadID,
		"sender_id": msg.SenderID, "protocol": string(msg.Protocol),
		"preview": body, "sent_at": fmt.Sprintf("%d", msg.SentAt.UnixMilli()),
	}
}

func (d *FCMDispatcher) sendWithRetry(token string, data map[string]string) error {
	var lastErr error
	for attempt := 0; attempt < fcmMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		result, err := d.sendOnce(token, data)
		if err != nil {
			lastErr = err
			continue
		}
		if len(result.Results) > 0 {
			res := result.Results[0]
			switch res.Error {
			case "":
				if res.RegistrationID != "" && res.RegistrationID != token {
					_ = d.replaceToken(token, res.RegistrationID)
				}
				return nil
			case "NotRegistered", "InvalidRegistration":
				_ = d.deleteToken(token)
				return fmt.Errorf("token invalidated (%s)", res.Error)
			case "Unavailable", "InternalServerError":
				lastErr = fmt.Errorf("FCM transient: %s", res.Error)
				continue
			default:
				return fmt.Errorf("FCM error: %s", res.Error)
			}
		}
		if result.Error != nil {
			lastErr = fmt.Errorf("FCM error %d: %s", result.Error.Code, result.Error.Message)
			if result.Error.Code >= 500 {
				continue
			}
			return lastErr
		}
		return nil
	}
	return lastErr
}

func (d *FCMDispatcher) sendOnce(token string, data map[string]string) (*fcmResponse, error) {
	body, err := json.Marshal(fcmLegacyPayload{
		To: token, Priority: "high", Data: data, TimeToLive: 60,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fcmTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fcmLegacyEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "key="+d.serverKey)
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, fmt.Errorf("response read: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("FCM rate-limited")
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("FCM server error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FCM status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var result fcmResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &result, nil
}

func (d *FCMDispatcher) getAllTokens() ([]string, error) {
	rows, err := d.db.Query(`SELECT fcm_token FROM devices WHERE platform IN ('android','ios')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err == nil && tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokens, rows.Err()
}

func (d *FCMDispatcher) deleteToken(token string) error {
	_, err := d.db.Exec(`DELETE FROM devices WHERE fcm_token = ?`, token); return err
}
func (d *FCMDispatcher) replaceToken(old, canonical string) error {
	_, err := d.db.Exec(`UPDATE devices SET fcm_token = ?, last_seen = ? WHERE fcm_token = ?`, canonical, time.Now().Unix(), old); return err
}

func (s *GhostServer) HandleDeviceRegister(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) { return }
	var req struct {
		DeviceID  string `json:"device_id"`
		ContactID string `json:"contact_id"`
		FCMToken  string `json:"fcm_token"`
		Platform  string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) { return }
	if req.DeviceID == "" || req.FCMToken == "" {
		http.Error(w, "device_id and fcm_token required", http.StatusBadRequest)
		return
	}
	if req.Platform == "" {
		req.Platform = "android"
	}
	if s.store == nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().Unix()
	_, err := s.store.db.Exec(`
		INSERT INTO devices (id, contact_id, fcm_token, platform, registered_at, last_seen)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET fcm_token = excluded.fcm_token, platform = excluded.platform, last_seen = excluded.last_seen
	`, req.DeviceID, req.ContactID, req.FCMToken, req.Platform, now, now)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		log.Printf("[fcm] device register error: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}
