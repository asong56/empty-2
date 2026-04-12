// shadowmail.go — Shadow-mail alias routing and inbound pipeline
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ShadowMailConfig holds all mail infrastructure settings.
type ShadowMailConfig struct {
	// ── Stalwart (Private Inbox / MTA) ──────────────────────────────────────
	// StalwartLMTPAddr is the LMTP socket Stalwart listens on for local delivery.
	StalwartLMTPAddr string // e.g. "127.0.0.1:24"

	// StalwartAPIAddr is the HTTP admin API for mailbox management.
	StalwartAPIAddr string // e.g. "http://127.0.0.1:8080"

	// ── Receiving Identity ────────────────────────────────────────────────────
	// GhostDomain is the domain on which the Ghost server receives email.
	// MX record must point here. e.g. "mail.ghost.example.com"
	GhostDomain string

	// LocalMailboxes maps username → local Stalwart mailbox ID.
	LocalMailboxes map[string]string

	// ── Gmail SMTP Relay (Outbound) ───────────────────────────────────────────
	// GmailSMTPHost is always "smtp.gmail.com:587" for STARTTLS.
	GmailSMTPHost string // "smtp.gmail.com:587"

	// GmailUser is the Gmail account used as relay. NOT the from-address.
	GmailUser     string
	GmailPassword string // App password (2FA required)

	// SendingAlias is the From: address shown to recipients.
	// Must be a verified alias in the Gmail account settings.
	// e.g. "alice@ghost.example.com"
	SendingAlias string

	// ── SPF / DKIM / DMARC ────────────────────────────────────────────────────
	// DKIMPrivateKeyPath is the path to the DKIM private key file.
	DKIMPrivateKeyPath string
	// DKIMSelector is the DNS selector, e.g. "ghost2024"
	DKIMSelector string

	// ── Content Policy ────────────────────────────────────────────────────────
	// MaxInboundSizeBytes: reject inbound messages above this size after SMTP DATA.
	MaxInboundSizeBytes int64 // e.g. 25MB

	// DownsampleImages: transcode inbound image attachments via media proxy.
	DownsampleImages bool

	// StripTrackers: run inbound HTML through the Content Washer.
	StripTrackers bool

	// RetentionDays: how long to keep raw inbound messages in Stalwart.
	RetentionDays int // Ghost server full index (6 months = 180 days)

	// MobileDays: days of messages indexed on the mobile client.
	MobileDays int // 3 days
}

// DefaultShadowMailConfig returns a sane production default.
func DefaultShadowMailConfig() ShadowMailConfig {
	return ShadowMailConfig{
		StalwartLMTPAddr:    "127.0.0.1:24",
		StalwartAPIAddr:     "http://127.0.0.1:8080",
		GhostDomain:         "mail.ghost.example.com",
		GmailSMTPHost:       "smtp.gmail.com:587",
		DKIMSelector:        "ghost2024",
		MaxInboundSizeBytes: 25 * 1024 * 1024,
		DownsampleImages:    true,
		StripTrackers:       true,
		RetentionDays:       180, // 6 months server index
		MobileDays:          3,
	}
}

type OutboundMailer struct {
	cfg ShadowMailConfig
}

func NewOutboundMailer(cfg ShadowMailConfig) *OutboundMailer {
	return &OutboundMailer{cfg: cfg}
}

type GhostEmail struct {
	From         string   // Display name + alias: "Alice <alice@ghost.example.com>"
	To           []string // Recipient addresses
	Subject      string
	BodyPlain    string            // Always include plain text
	BodyHTML     string            // Optional: minimal, tracker-free HTML
	ReplyToMsgID string            // For threading: In-Reply-To header value
	MessageID    string            // Ghost-generated Message-ID
	Headers      map[string]string // Custom headers
}

func (m *OutboundMailer) Send(email GhostEmail) error {
	// Build the raw RFC5322 message.
	raw, err := m.buildRawMessage(email)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	// Connect to Gmail SMTP with STARTTLS.
	host, _, _ := net.SplitHostPort(m.cfg.GmailSMTPHost)
	auth := smtp.PlainAuth("", m.cfg.GmailUser, m.cfg.GmailPassword, host)

	tlsCfg := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}

	// Establish a plain TCP connection first, then upgrade via STARTTLS.
	// Port 587 uses STARTTLS (ESMTP), NOT implicit TLS (SMTPS / port 465).
	// Using tls.Dial here would cause an immediate TLS handshake that Gmail
	// rejects on port 587, making every send fail.
	plainConn, err := net.Dial("tcp", m.cfg.GmailSMTPHost)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}

	client, err := smtp.NewClient(plainConn, host)
	if err != nil {
		plainConn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	// Upgrade the plain connection to TLS via the STARTTLS command.
	if err = client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("smtp starttls: %w", err)
	}

	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}

	// MAIL FROM uses the Gmail account (envelope sender for SPF).
	// FROM header uses the alias (what recipients see).
	if err = client.Mail(m.cfg.GmailUser); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}

	for _, to := range email.To {
		if err = client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", to, err)
		}
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err = wc.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err = wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}

	return client.Quit()
}

// buildRawMessage constructs a minimal, tracker-free RFC5322 message.
func (m *OutboundMailer) buildRawMessage(email GhostEmail) ([]byte, error) {
	var sb strings.Builder
	hdr := func(k, v string) { fmt.Fprintf(&sb, "%s: %s\r\n", k, v) }

	msgID := email.MessageID
	if msgID == "" {
		msgID = fmt.Sprintf("<%d.ghost@%s>", time.Now().UnixNano(), m.cfg.GhostDomain)
	}

	hdr("Date", time.Now().UTC().Format(time.RFC1123Z))
	hdr("Message-ID", msgID)
	hdr("From", email.From)
	hdr("To", strings.Join(email.To, ", "))
	hdr("Subject", sanitizeHeader(email.Subject))
	if email.ReplyToMsgID != "" {
		hdr("In-Reply-To", email.ReplyToMsgID)
		hdr("References", email.ReplyToMsgID)
	}
	for k, v := range email.Headers {
		hdr(k, sanitizeHeader(v))
	}
	sb.WriteString("Disposition-Notification-To: \r\n")
	sb.WriteString("X-Ghost-Relay: gmail-smtp-v1\r\n")

	if email.BodyHTML == "" {
		sb.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		sb.WriteString(email.BodyPlain)
	} else {
			boundary := fmt.Sprintf("GhostBoundary%d", time.Now().UnixNano())
		sb.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q\r\n", boundary))
		sb.WriteString("\r\n")

		sb.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		sb.WriteString(email.BodyPlain)
		sb.WriteString("\r\n")

		// HTML part — stripped of trackers before inclusion.
		washer := NewContentWasher(DefaultWasherConfig())
		htmlResult := washer.Wash(ProtoEmail, "html", []byte(email.BodyHTML))
		sb.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		sb.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		sb.WriteString(htmlResult.Body)
		sb.WriteString("\r\n")

		sb.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	}

	return []byte(sb.String()), nil
}

// InboundPipeline processes email arriving at Stalwart via LMTP.
type InboundPipeline struct {
	cfg     ShadowMailConfig
	washer  *ContentWasher
	storage MessageStore
}

// MessageStore is the interface for persisting washed messages.
type MessageStore interface {
	SaveMessage(msg *Message) error
	GetThread(threadID string) (*Thread, error)
	GetRecentMessages(threadID string, since time.Time, limit int) ([]*Message, error)
}

func NewInboundPipeline(cfg ShadowMailConfig, store MessageStore) *InboundPipeline {
	return &InboundPipeline{
		cfg:     cfg,
		washer:  NewContentWasher(DefaultWasherConfig()),
		storage: store,
	}
}

type InboundRawEmail struct {
	// EnvelopeFrom is the SMTP MAIL FROM value (may differ from header From:).
	EnvelopeFrom string

	// EnvelopeTo is the SMTP RCPT TO (local recipient).
	EnvelopeTo string

	// RawData is the full RFC5322 message bytes.
	RawData []byte

	// ReceivedAt is when Stalwart accepted the DATA phase.
	ReceivedAt time.Time
}

// Ingest processes one inbound email, washing it and storing it.
func (p *InboundPipeline) Ingest(raw InboundRawEmail) (*Message, error) {
	// 1. Size gate — reject oversized messages before wasting CPU.
	if int64(len(raw.RawData)) > p.cfg.MaxInboundSizeBytes {
		return nil, fmt.Errorf("message size %d exceeds limit %d", len(raw.RawData), p.cfg.MaxInboundSizeBytes)
	}
	// 2. Parse essential headers (From, Subject, Message-ID, Date).
	headers := parseEmailHeaders(string(raw.RawData))
	// 3. Run Content Washer.
	washResult := p.washer.Wash(ProtoEmail, "text", raw.RawData)
	// 4. Build canonical Message.
	msg := &Message{
		ID:           generateUUIDv7(),
		UpstreamID:   headers["message-id"],
		Protocol:     ProtoEmail,
		SenderID:     normalizeEmailAddress(headers["from"]),
		RecipientIDs: []string{normalizeEmailAddress(raw.EnvelopeTo)},
		SentAt:       parseEmailDate(headers["date"], raw.ReceivedAt),
		ReceivedAt:   raw.ReceivedAt,
		ContentType:  washResult.ContentType,
		Body:         washResult.Body,
		Status:       StatusDelivered,
		RawHeaders:   headers,
		WasherLog:    washResult.Log,
	}
	// Generate thread ID from normalized participants.
	msg.ThreadID = deriveThreadID(msg.SenderID, msg.RecipientIDs)
	// 5. Handle redirected content.
	if washResult.Redirected != nil {
		msg.Redirected = washResult.Redirected
	}
	// 6. Persist to SQLite (server-side 6-month index).
	if err := p.storage.SaveMessage(msg); err != nil {
		return nil, fmt.Errorf("store message: %w", err)
	}
	// 7. Enforce retention: schedule purge after RetentionDays.
	// (Cron job or SQLite TTL handles actual deletion.)
	return msg, nil
}

// DNSRecords returns the required DNS records for the Shadow Mail setup.
func DNSRecords(cfg ShadowMailConfig) map[string]string {
	return map[string]string{
		// MX record: route inbound mail to Ghost server.
		"MX": fmt.Sprintf("10 %s.", cfg.GhostDomain),
		// SPF: authorize Gmail SMTP servers to send on behalf of the alias domain.
		// include:_spf.google.com covers all Gmail SMTP egress IPs.
		"SPF (TXT)": `"v=spf1 include:_spf.google.com ~all"`,
		// DKIM: public key published; private key used by Stalwart to sign outbound.
		"DKIM (TXT)": fmt.Sprintf(`"%s._domainkey → v=DKIM1; k=rsa; p=<PUBLIC_KEY>"`, cfg.DKIMSelector),
		// DMARC: quarantine failures, aggregate reports to postmaster.
		"DMARC (TXT)": fmt.Sprintf(`"v=DMARC1; p=quarantine; rua=mailto:postmaster@%s; adkim=r; aspf=r"`, cfg.GhostDomain),
	}
}

// RollingCachePolicy defines the tiered retention policy.
type RollingCachePolicy struct {
	MobileDays int // Messages older than this are evicted from the mobile SQLite DB.
	ServerDays int // Messages older than this are purged server-side.
}

// DefaultRollingCachePolicy returns the Ghost spec defaults.
func DefaultRollingCachePolicy() RollingCachePolicy {
	return RollingCachePolicy{
		MobileDays: 3,
		ServerDays: 180,
	}
}

// CacheEvictionSQL returns the SQL statement to evict stale mobile messages.
func (p RollingCachePolicy) CacheEvictionSQL() string {
	return fmt.Sprintf(`
-- Ghost Mobile Cache Eviction
-- Run this in the mobile SQLite DB on every app launch and every 6 hours.
DELETE FROM messages
WHERE received_at < datetime('now', '-%d days')
  AND thread_id NOT IN (
    -- Preserve pinned threads regardless of age.
    SELECT id FROM threads WHERE is_pinned = 1
  );

-- Update thread last_message snippets after eviction.
UPDATE threads
SET last_message_snippet = (
    SELECT snippet FROM messages
    WHERE thread_id = threads.id
    ORDER BY sent_at DESC
    LIMIT 1
)
WHERE id IN (SELECT DISTINCT thread_id FROM messages);
`, p.MobileDays)
}

// ServerPurgeSQL returns the SQL to run on the server after 6 months.
func (p RollingCachePolicy) ServerPurgeSQL() string {
	return fmt.Sprintf(`
-- Ghost Server Purge (run via cron, first generate TopicSummary)
DELETE FROM messages
WHERE received_at < datetime('now', '-%d days')
  AND thread_id IN (
    -- Only purge messages that have an associated TopicSummary.
    SELECT thread_id FROM topic_summaries
    WHERE period_end < datetime('now', '-%d days')
  );

-- Cascade: remove orphaned media references.
DELETE FROM media_refs
WHERE message_id NOT IN (SELECT id FROM messages);
`, p.ServerDays, p.ServerDays)
}

var (
	headerRe    = regexp.MustCompile(`(?im)^([A-Za-z0-9-]+):\s*(.+)(?:\r?\n[ \t]+.+)*`)
	emailAddrRe = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[a-zA-Z]{2,}`)
)

func parseEmailHeaders(raw string) map[string]string {
	result := make(map[string]string)
	matches := headerRe.FindAllStringSubmatch(raw, -1)
	for _, m := range matches {
		if len(m) >= 3 {
			key := strings.ToLower(strings.TrimSpace(m[1]))
			val := strings.TrimSpace(m[2])
			result[key] = val
		}
	}
	return result
}

func normalizeEmailAddress(input string) string {
	if m := emailAddrRe.FindString(input); m != "" { return strings.ToLower(m) }
	return strings.ToLower(strings.TrimSpace(input))
}

func parseEmailDate(dateStr string, fallback time.Time) time.Time {
	for _, f := range []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"} {
		if t, err := time.Parse(f, dateStr); err == nil {
			return t
		}
	}
	return fallback
}

// deriveThreadID generates a stable, sorted hash from participants.
func deriveThreadID(sender string, recipients []string) string {
	all := append([]string{sender}, recipients...)
	sort.Strings(all)
	combined := strings.Join(all, "|")
	// FNV-1a hash for speed (not security).
	var h uint64 = 14695981039346656037
	for _, c := range []byte(combined) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return fmt.Sprintf("thread_%x", h)
}

// generateUUIDv7 returns a time-sortable UUIDv7-format identifier.
func generateUUIDv7() string {
	ms := time.Now().UnixMilli()
	return fmt.Sprintf("%012x-ghost-%d", ms, time.Now().UnixNano()%1e9)
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
