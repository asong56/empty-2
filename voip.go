// voip.go — VoIP Bridge + Android ConnectionService Integration
package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// VoIPSignal is received from Mautrix bridges when a call event occurs.
// Ghost translates it into a ConnectionService-compatible WebSocket push.
type VoIPSignal struct {
	BridgeID          string         `json:"bridge_id"`
	Protocol          SourceProtocol `json:"protocol"`
	Event             CallEventType  `json:"event"`
	CallerID          string         `json:"caller_id"`
	CallerDisplayName string         `json:"caller_display_name"`
	IsVideo           bool           `json:"is_video"`
	SDPOffer          *string        `json:"sdp_offer,omitempty"`
	SDPAnswer         *string        `json:"sdp_answer,omitempty"`
	ICECandidates     []string       `json:"ice_candidates,omitempty"`
	EventAt           time.Time      `json:"event_at"`
}

// VoIPACK is sent from the Android client back to the server to confirm
// system UI readiness or call completion.
type VoIPACK struct {
	BridgeID  string    `json:"bridge_id"`
	Event     string    `json:"event"` // "system_ui_ready" | "answered" | "ended"
	Timestamp time.Time `json:"timestamp"`
}

// VoIPHandler handles POST /internal/voip from Mautrix bridges.
// It validates the signal and pushes it to connected WebSocket clients.
func (s *TardiServer) VoIPHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) { return }
	var signal VoIPSignal
	if !decodeJSON(w, r, &signal) { return }
	if signal.BridgeID == "" || signal.Protocol == "" {
		http.Error(w, "bridge_id and protocol required", http.StatusBadRequest)
		return
	}

	// Build a canonical call message and store it.
	msg := &Message{
		ID:          generateUUIDv7(),
		ThreadID:    "voip_" + signal.BridgeID,
		Protocol:    signal.Protocol,
		SenderID:    signal.CallerID,
		ContentType: ContentCall,
		SentAt:      signal.EventAt,
		ReceivedAt:  time.Now(),
		Status:      StatusDelivered,
		Call: &CallPayload{
			EventType: signal.Event,
			IsVideo:   signal.IsVideo,
			BridgeID:  signal.BridgeID,
		},
	}
	if s.store != nil {
		s.store.SaveMessage(msg)
	}

	payload, _ := json.Marshal(signal)
	s.broadcast <- PushEvent{
		Type:      EventNewMessage,
		Timestamp: time.Now(),
		Payload:   json.RawMessage(payload),
	}

	if s.fcm != nil {
		s.fcm.NotifyNewMessage(msg)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "queued",
		"event":    string(signal.Event),
		"protocol": string(signal.Protocol),
	})
}

// RollingCacheConfig defines the 3-day client-side rolling retention policy.
// The Android Room database evicts messages older than CacheDays days on startup.
type RollingCacheConfig struct {
	CacheDays         int           // default: 3
	MaxMessagesPerThread int        // default: 200
	PurgeInterval     time.Duration // default: 1h
}

func DefaultRollingCacheConfig() RollingCacheConfig {
	return RollingCacheConfig{
		CacheDays:            3,
		MaxMessagesPerThread: 200,
		PurgeInterval:        time.Hour,
	}
}
