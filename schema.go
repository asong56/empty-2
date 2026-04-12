// schema.go — Universal message schema (all protocols collapse into one struct).

package main

import (
	"encoding/json"
	"time"
)

// SourceProtocol identifies which upstream network a message originated from.
type SourceProtocol string

const (
	ProtoWeChat   SourceProtocol = "wechat"
	ProtoWhatsApp SourceProtocol = "whatsapp"
	ProtoSignal   SourceProtocol = "signal"
	ProtoEmail    SourceProtocol = "email"
	ProtoSMS      SourceProtocol = "sms"
	ProtoInternal SourceProtocol = "internal" // Ghost-to-Ghost native
)

// ContentType describes what kind of payload a message carries after washing.
type ContentType string

const (
	ContentText       ContentType = "text"       // Plain UTF-8 or Markdown
	ContentImage      ContentType = "image"      // Proxied WebP thumbnail
	ContentVideo      ContentType = "video"      // HLS stream URL, no local copy
	ContentAudio      ContentType = "audio"      // Transcoded opus/mp3
	ContentFile       ContentType = "file"       // Generic blob reference
	ContentLocation   ContentType = "location"   // Lat/Lon + label
	ContentRedirected ContentType = "redirected" // Blocked rich content → deep link
	ContentCall       ContentType = "call"       // VoIP event (start/end/missed)
	ContentSystem     ContentType = "system"     // Group changes, key resets, etc.
)

type DeliveryStatus string

const (
	StatusPending   DeliveryStatus = "pending"
	StatusSent      DeliveryStatus = "sent"
	StatusDelivered DeliveryStatus = "delivered"
	StatusRead      DeliveryStatus = "read"
	StatusFailed    DeliveryStatus = "failed"
)

// MediaRef is a pointer to server-side transcoded/proxied media.
// The client never stores raw media; it references these URLs.
type MediaRef struct {
	ServerPath string `json:"server_path"`
	StreamURL string `json:"stream_url,omitempty"`

	ThumbURL string `json:"thumb_url,omitempty"`
	MIMEType string `json:"mime_type"`

	OriginalSize int64 `json:"original_size_bytes"`
	TranscodedSize int64 `json:"transcoded_size_bytes"`

	ExpiresAt time.Time `json:"expires_at"`
	PurgeOnView bool `json:"purge_on_view"`
}

type LocationPayload struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	Label     string  `json:"label,omitempty"`
	Accuracy  float32 `json:"accuracy_m,omitempty"`
}

type CallEventType string

const (
	CallStarted  CallEventType = "started"
	CallAnswered CallEventType = "answered"
	CallEnded    CallEventType = "ended"
	CallMissed   CallEventType = "missed"
	CallDeclined CallEventType = "declined"
)

type CallPayload struct {
	EventType   CallEventType `json:"event"`
	DurationSec int           `json:"duration_sec,omitempty"`
	IsVideo     bool          `json:"is_video"`
	BridgeID string `json:"bridge_id,omitempty"`
}

// RedirectedPayload replaces blocked rich content (stickers, red packets, mini-apps)
// with a human-readable label and a deep link to the official app.
type RedirectedPayload struct {
	OriginalType string `json:"original_type"`

	DisplayLabel string `json:"display_label"`

	// DeepLink is the platform-specific URI to open the official app.
	// e.g. "weixin://dl/redpacket?id=xxx"
	DeepLink string `json:"deep_link,omitempty"`
}

// Message is the canonical, protocol-agnostic representation of any communication.
// One struct rules them all. The washer pipeline populates this from raw upstream data.
type Message struct {
	ID string `json:"id"`

	// Preserved for status backfilling (marking read on official clients).
	UpstreamID string `json:"upstream_id,omitempty"`
	ThreadID string `json:"thread_id"`

	Protocol SourceProtocol `json:"protocol"`
	SenderID string `json:"sender_id"`

	RecipientIDs []string `json:"recipient_ids"`
	IsSelf bool `json:"is_self"`

	SentAt time.Time `json:"sent_at"`
	ReceivedAt time.Time `json:"received_at"`

	ContentType ContentType `json:"content_type"`
	Body string `json:"body,omitempty"`

	Media *MediaRef `json:"media,omitempty"`
	Location *LocationPayload `json:"location,omitempty"`

	Call *CallPayload `json:"call,omitempty"`
	Redirected *RedirectedPayload `json:"redirected,omitempty"`

	ReplyToID *string `json:"reply_to_id,omitempty"`
	ReplySnippet *string `json:"reply_snippet,omitempty"`

	Status DeliveryStatus `json:"status"`
	IsRetracted bool `json:"is_retracted,omitempty"`

	// retracted messages (so the user can still read what they deleted).
	// Never sent to client for incoming messages.
	RetractedOriginalBody string `json:"retracted_original_body,omitempty"`

	// Never sent to client.
	RawHeaders map[string]string `json:"-"`

	// Only included in debug/admin responses.
	WasherLog []string `json:"washer_log,omitempty"`
}

// Thread represents a conversation (1:1 or group) across any protocol.
type Thread struct {
	ID       string         `json:"id"`
	Protocol SourceProtocol `json:"protocol"`
	DisplayName string `json:"display_name"`

	AvatarURL string `json:"avatar_url,omitempty"`
	Participants []string `json:"participants"`

	LastMessage *MessagePreview `json:"last_message,omitempty"`
	UnreadCount int `json:"unread_count"`

	IsPinned bool `json:"is_pinned"`

	// MuteUntil: if non-zero and in the future, suppress all notifications.
	MuteUntil *time.Time `json:"mute_until,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MessagePreview is a compact struct used in the conversation list.
// Avoids sending full message payloads for list rendering.
type MessagePreview struct {
	SenderName  string      `json:"sender_name"`
	ContentType ContentType `json:"content_type"`
	Snippet   string    `json:"snippet"`
	Timestamp time.Time `json:"timestamp"`
}

// Contact is the Ghost-normalized representation of a person or entity.
// One Contact can have multiple protocol handles (WeChat + WhatsApp + Email).
type Contact struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	IsPinned    bool   `json:"is_pinned"`

	// Handles maps each protocol to the upstream identifier for that protocol.
	// e.g. {"wechat": "wxid_abc123", "email": "alice@example.com"}
	Handles map[SourceProtocol]string `json:"handles"`
}

// TopicSummary replaces raw messages after the 6-month retention window.
// Generated entirely by local heuristics (TextRank + Regex). No external LLMs.
type TopicSummary struct {
	ThreadID    string    `json:"thread_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`

	// Topics is an ordered list of inferred conversation topics.
	// e.g. ["Project deadline", "Dinner plans", "Travel booking"]
	Topics []string `json:"topics"`
	MessageCount int `json:"message_count"`

	// GeneratedBy identifies the local algorithm used.
	// e.g. "textrank_v1", "regex_heuristic_v2"
	GeneratedBy string `json:"generated_by"`
	CreatedAt time.Time `json:"created_at"`
}

// PushEventType enumerates WebSocket events sent from server to client.
type PushEventType string

const (
	EventNewMessage     PushEventType = "new_message"
	EventStatusUpdate   PushEventType = "status_update"
	EventThreadUpdate   PushEventType = "thread_update"
	EventContactUpdate  PushEventType = "contact_update"
	EventAuthChallenge  PushEventType = "auth_challenge" // QR relay trigger
	EventServerHealth   PushEventType = "server_health"
	EventMessageRetract PushEventType = "message_retract" // sender recalled a message
	EventPlatformHealth PushEventType = "platform_health" // bridge connectivity status
)

// PushEvent is the WebSocket envelope sent from Ghost Server → Ghost Client.
type PushEvent struct {
	Type      PushEventType   `json:"type"`
	Timestamp time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload"` // Varies by Type
}

// PlatformHealthPayload is sent as EventPlatformHealth to report bridge status.
type PlatformHealthPayload struct {
	Protocol    SourceProtocol `json:"protocol"`
	Status      string         `json:"status"`       // "connected" | "disconnected" | "degraded"
	NeedsReauth bool           `json:"needs_reauth"` // true = QR scan required
	Latency     int            `json:"latency_ms,omitempty"`
	CheckedAt   time.Time      `json:"checked_at"`
}

// MessageRetractPayload is sent as EventMessageRetract when a sender recalls a message.
type MessageRetractPayload struct {
	MessageID string         `json:"message_id"`
	ThreadID  string         `json:"thread_id"`
	Protocol  SourceProtocol `json:"protocol"`
	IsSelf    bool           `json:"is_self"`
	RetractedBody string `json:"retracted_body,omitempty"`
}

// NotifyRule defines the semantic notification filter.
// Only messages matching these criteria trigger a visible push; others sync silently.
type NotifyRule struct {
	// PinnedContactsOnly: always notify for pinned contacts regardless of mentions.
	PinnedContactsOnly bool `json:"pinned_contacts_only"`

	// MentionKeywords: custom @-mention strings that trigger notification.
	// Defaults to the user's own display name.
	MentionKeywords []string `json:"mention_keywords"`

	// SuppressStickers: block sticker-type messages from ever notifying.
	SuppressStickers bool `json:"suppress_stickers"`

	// QuietHoursStart/End: 24h format strings e.g. "22:00", "07:00"
	QuietHoursStart string `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd   string `json:"quiet_hours_end,omitempty"`
}
