// mautrix.go — HTTP client for Mautrix bridge REST APIs.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BridgeConfig holds the address + auth token for one Mautrix bridge.
type BridgeConfig struct {
	BaseURL         string
	AppserviceToken string
	Protocol        SourceProtocol
	Timeout         time.Duration
}

type BridgeRegistry struct {
	configs map[SourceProtocol]BridgeConfig
}

func NewBridgeRegistry(configs []BridgeConfig) *BridgeRegistry {
	m := make(map[SourceProtocol]BridgeConfig, len(configs))
	for _, c := range configs {
		m[c.Protocol] = c
	}
	return &BridgeRegistry{configs: m}
}

func (r *BridgeRegistry) Get(proto SourceProtocol) (BridgeConfig, bool) {
	c, ok := r.configs[proto]
	return c, ok
}

// MautrixSendRequest is the payload for POST /v1/send.
type MautrixSendRequest struct {
	RoomID    string                `json:"room_id"`
	EventType string                `json:"event_type"`
	Content   MautrixMessageContent `json:"content"`
}

// MautrixMessageContent represents a Matrix m.room.message event body.
type MautrixMessageContent struct {
	// MsgType is the Matrix message type.
	// "m.text"  — plain text message
	// "m.image" — image (with url field)
	// "m.audio" — audio
	// "m.video" — video
	MsgType string `json:"msgtype"`

	// Body is the plain text representation (required for all types).
	Body string `json:"body"`

	// FormattedBody is HTML (for m.text with formatting).
	FormattedBody string `json:"formatted_body,omitempty"`
	Format        string `json:"format,omitempty"` // "org.matrix.custom.html"

	// URL is the Matrix media URL for image/audio/video messages.
	URL string `json:"url,omitempty"`

	// RelatesTo is the reply threading structure.
	RelatesTo *MautrixRelation `json:"m.relates_to,omitempty"`
}

// MautrixRelation encodes the reply threading for a message.
type MautrixRelation struct {
	InReplyTo *MautrixReplyTo `json:"m.in_reply_to,omitempty"`
}

type MautrixReplyTo struct {
	// EventID is the upstream event ID of the message being replied to.
	// Ghost stores this as UpstreamID on the Message struct.
	EventID string `json:"event_id"`
}

// MautrixSendResponse is the response from POST /v1/send.
type MautrixSendResponse struct {
	// EventID is the Matrix event ID assigned to the sent message.
	// Store this as UpstreamID on the outbound Message.
	EventID string `json:"event_id"`
}

// MautrixMarkReadRequest is the payload for POST /v1/mark_read.
type MautrixMarkReadRequest struct {
	RoomID string `json:"room_id"`
	// EventIDs are the upstream event IDs to mark as read.
	EventIDs []string `json:"event_ids"`
}

// MautrixStatusResponse is the response from GET /v1/status.
type MautrixStatusResponse struct {
	OK          bool   `json:"ok"`
	UserID      string `json:"user_id,omitempty"`      // logged-in Matrix user
	ConnectedTo string `json:"connected_to,omitempty"` // upstream protocol identity
	NeedsReauth bool   `json:"needs_reauth"`
}

type MautrixClient struct {
	cfg    BridgeConfig
	client *http.Client
}

func NewMautrixClient(cfg BridgeConfig) *MautrixClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &MautrixClient{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (c *MautrixClient) Send(req MautrixSendRequest) (string, error) {
	var resp MautrixSendResponse
	if err := c.post("/v1/send", req, &resp); err != nil {
		return "", fmt.Errorf("[%s] send: %w", c.cfg.Protocol, err)
	}
	return resp.EventID, nil
}

func (c *MautrixClient) SendText(roomID, text, replyToEventID string) (string, error) {
	req := MautrixSendRequest{
		RoomID:    roomID,
		EventType: "m.room.message",
		Content: MautrixMessageContent{
			MsgType: "m.text",
			Body:    text,
		},
	}
	if replyToEventID != "" {
		req.Content.RelatesTo = &MautrixRelation{
			InReplyTo: &MautrixReplyTo{EventID: replyToEventID},
		}
	}
	return c.Send(req)
}

func (c *MautrixClient) MarkRead(roomID string, eventIDs []string) error {
	if len(eventIDs) == 0 { return nil }
	if err := c.post("/v1/mark_read", MautrixMarkReadRequest{RoomID: roomID, EventIDs: eventIDs}, nil); err != nil {
		return fmt.Errorf("[%s] mark_read: %w", c.cfg.Protocol, err)
	}
	return nil
}

func (c *MautrixClient) Status() (*MautrixStatusResponse, error) {
	var resp MautrixStatusResponse
	if err := c.get("/v1/status", &resp); err != nil {
		return nil, fmt.Errorf("[%s] status: %w", c.cfg.Protocol, err)
	}
	return &resp, nil
}

func (c *MautrixClient) Logout() error {
	if err := c.post("/v1/logout", nil, nil); err != nil {
		return fmt.Errorf("[%s] logout: %w", c.cfg.Protocol, err)
	}
	return nil
}

func (c *MautrixClient) QRLoginStart() (string, error) {
	var resp struct {
		QRCode string `json:"qr_code"` // data:image/png;base64,...
	}
	if err := c.post("/v1/login/qr/start", nil, &resp); err != nil {
		return "", fmt.Errorf("[%s] qr_login_start: %w", c.cfg.Protocol, err)
	}
	return resp.QRCode, nil
}

func (c *MautrixClient) post(path string, body, response interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, c.cfg.BaseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AppserviceToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Ghost-Client", "ghost-communicator/0.1")
	return c.doRequest(req, response)
}

func (c *MautrixClient) get(path string, response interface{}) error {
	req, err := http.NewRequest(http.MethodGet, c.cfg.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AppserviceToken)
	req.Header.Set("X-Ghost-Client", "ghost-communicator/0.1")
	return c.doRequest(req, response)
}

func (c *MautrixClient) doRequest(req *http.Request, response interface{}) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bridge returned %d: %s", resp.StatusCode, string(body))
	}
	if response != nil && len(body) > 0 {
		if err := json.Unmarshal(body, response); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

type BridgeDispatcher struct {
	registry *BridgeRegistry
	clients  map[SourceProtocol]*MautrixClient
	store    *SQLiteStore
}

func NewBridgeDispatcher(registry *BridgeRegistry, store *SQLiteStore) *BridgeDispatcher {
	clients := make(map[SourceProtocol]*MautrixClient)
	for proto, cfg := range registry.configs {
		clients[proto] = NewMautrixClient(cfg)
	}
	return &BridgeDispatcher{
		registry: registry,
		clients:  clients,
		store:    store,
	}
}

// Dispatch sends a message through the appropriate bridge.
// Returns the upstream event ID and updates the message status in the DB.
func (d *BridgeDispatcher) Dispatch(msg *Message) error {
	if msg.Protocol == ProtoEmail {
		return fmt.Errorf("email dispatch handled by OutboundMailer, not bridge dispatcher")
	}

	client, ok := d.clients[msg.Protocol]
	if !ok {
		return fmt.Errorf("no bridge configured for protocol %s", msg.Protocol)
	}

	var roomID string
	if d.store != nil {
		var err error
		roomID, err = d.store.GetMatrixRoomID(msg.ThreadID, msg.Protocol)
		if err != nil {
			return fmt.Errorf("resolve room ID for thread %s: %w", msg.ThreadID, err)
		}
	}
	if roomID == "" {
		return fmt.Errorf("no Matrix room mapping for thread %s / protocol %s — "+
			"message cannot be dispatched until the thread has received at least one "+
			"inbound message from the bridge", msg.ThreadID, msg.Protocol)
	}

	var replyEventID string
	if msg.ReplyToID != nil && d.store != nil {
		// defer rows.Close() before checking err so that non-nil rows
		// returned alongside an error (driver-dependent behaviour) are always closed.
		rows, err := d.store.db.Query(
			`SELECT upstream_id FROM messages WHERE id = ? LIMIT 1`, *msg.ReplyToID,
		)
		if rows != nil {
			defer rows.Close()
		}
		if err == nil && rows.Next() {
			_ = rows.Scan(&replyEventID)
		}
	}

	var upstreamID string
	var err error

	switch msg.ContentType {
	case ContentText:
		upstreamID, err = client.SendText(roomID, msg.Body, replyEventID)
	default:
		return fmt.Errorf("bridge dispatch: unsupported content type %s", msg.ContentType)
	}

	if err != nil {
			if d.store != nil {
			_ = d.store.UpdateMessageStatus(msg.ID, StatusFailed)
		}
		return err
	}

	msg.UpstreamID = upstreamID
	msg.Status = StatusSent
	if d.store != nil {
		_ = d.store.UpdateMessageStatus(msg.ID, StatusSent)
	}

	return nil
}

func (d *BridgeDispatcher) HealthCheck() map[SourceProtocol]*MautrixStatusResponse {
	out := make(map[SourceProtocol]*MautrixStatusResponse)
	for proto, client := range d.clients {
		if s, err := client.Status(); err != nil {
			out[proto] = &MautrixStatusResponse{OK: false}
		} else {
			out[proto] = s
		}
	}
	return out
}

