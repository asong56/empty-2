// storage.go — SQLite Persistence Layer
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA cache_size = -8000;
PRAGMA temp_store = MEMORY;
PRAGMA mmap_size = 268435456;
PRAGMA page_size = 4096;
PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS messages (
id TEXT PRIMARY KEY,
upstream_id TEXT,
thread_id TEXT NOT NULL,
protocol TEXT NOT NULL,
sender_id TEXT NOT NULL,
is_self INTEGER NOT NULL DEFAULT 0,
content_type TEXT NOT NULL,
body TEXT,
media_server_path TEXT,
media_stream_url TEXT,
media_thumb_url TEXT,
media_mime_type TEXT,
redirected_type TEXT,
redirected_label TEXT,
redirected_link TEXT,
reply_to_id TEXT,
reply_snippet TEXT,
status TEXT NOT NULL DEFAULT 'pending',
sent_at INTEGER NOT NULL,
received_at INTEGER NOT NULL,
washer_log TEXT
);
CREATE INDEX IF NOT EXISTS idx_messages_thread_sent ON messages (thread_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_upstream ON messages (upstream_id) WHERE upstream_id IS NOT NULL;
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
body, msg_id UNINDEXED, thread_id UNINDEXED, sender_id UNINDEXED,
tokenize = 'unicode61'
);
CREATE TABLE IF NOT EXISTS threads (
id TEXT PRIMARY KEY,
protocol TEXT NOT NULL,
display_name TEXT NOT NULL,
avatar_url TEXT,
participants TEXT NOT NULL,
unread_count INTEGER NOT NULL DEFAULT 0,
is_pinned INTEGER NOT NULL DEFAULT 0,
mute_until INTEGER,
last_snippet TEXT,
last_sender TEXT,
last_content_type TEXT,
last_message_at INTEGER,
updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads (updated_at DESC);
CREATE TABLE IF NOT EXISTS contacts (
id TEXT PRIMARY KEY,
display_name TEXT NOT NULL,
avatar_url TEXT,
is_pinned INTEGER NOT NULL DEFAULT 0,
handles TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS topic_summaries (
id TEXT PRIMARY KEY,
thread_id TEXT NOT NULL,
period_start INTEGER NOT NULL,
period_end INTEGER NOT NULL,
topics TEXT NOT NULL,
message_count INTEGER NOT NULL,
generated_by TEXT NOT NULL,
created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_topic_summaries_thread ON topic_summaries (thread_id, period_end DESC);
CREATE TABLE IF NOT EXISTS media_refs (
id TEXT PRIMARY KEY,
message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
server_path TEXT NOT NULL,
stream_url TEXT,
thumb_url TEXT,
mime_type TEXT NOT NULL,
original_size INTEGER NOT NULL,
transcoded_size INTEGER NOT NULL DEFAULT 0,
status TEXT NOT NULL DEFAULT 'pending',
expires_at INTEGER NOT NULL,
purge_on_view INTEGER NOT NULL DEFAULT 0,
created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
id TEXT PRIMARY KEY,
contact_id TEXT NOT NULL,
fcm_token TEXT NOT NULL,
platform TEXT NOT NULL DEFAULT 'android',
registered_at INTEGER NOT NULL,
last_seen INTEGER
);
CREATE TABLE IF NOT EXISTS thread_bridge_ids (
thread_id TEXT NOT NULL,
protocol TEXT NOT NULL,
matrix_room_id TEXT NOT NULL,
created_at INTEGER NOT NULL,
PRIMARY KEY (thread_id, protocol)
);
`

const msgSelectSQL = `
		SELECT id, upstream_id, thread_id, protocol, sender_id, is_self,
		content_type, body,
		media_stream_url, media_thumb_url, media_mime_type,
		redirected_type, redirected_label, redirected_link,
		reply_to_id, reply_snippet,
		status, sent_at, received_at
		FROM messages
	`

type SQLiteStore struct{ db *sql.DB }

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	dsn := fmt.Sprintf(
		"file:%s?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=on&_busy_timeout=5000",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err = db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) SaveMessage(msg *Message) error {
	var washerLog *string
	if len(msg.WasherLog) > 0 {
		b, _ := json.Marshal(msg.WasherLog)
		j := string(b)
		washerLog = &j
	}
	var (
		redirType, redirLabel, redirLink      *string
		mediaPath, mediaStream, mediaThumb, mediaMIME *string
	)
	if msg.Redirected != nil {
		redirType = &msg.Redirected.OriginalType
		redirLabel = &msg.Redirected.DisplayLabel
		if msg.Redirected.DeepLink != "" {
			redirLink = &msg.Redirected.DeepLink
		}
	}
	if msg.Media != nil {
		mediaPath = &msg.Media.ServerPath
		mediaStream = &msg.Media.StreamURL
		mediaThumb = &msg.Media.ThumbURL
		mediaMIME = &msg.Media.MIMEType
	}
	_, err := s.db.Exec(`
		INSERT INTO messages (
		id, upstream_id, thread_id, protocol, sender_id, is_self,
		content_type, body,
		media_server_path, media_stream_url, media_thumb_url, media_mime_type,
		redirected_type, redirected_label, redirected_link,
		reply_to_id, reply_snippet,
		status, sent_at, received_at, washer_log
		) VALUES (?,?,?,?,?,?, ?,?, ?,?,?,?, ?,?,?, ?,?, ?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		status = excluded.status,
		washer_log = excluded.washer_log
	`,
		msg.ID, nilStr(msg.UpstreamID), msg.ThreadID, string(msg.Protocol),
		msg.SenderID, boolInt(msg.IsSelf),
		string(msg.ContentType), nilStr(msg.Body),
		mediaPath, mediaStream, mediaThumb, mediaMIME,
		redirType, redirLabel, redirLink,
		msg.ReplyToID, msg.ReplySnippet,
		string(msg.Status), msg.SentAt.UnixMilli(), msg.ReceivedAt.UnixMilli(), washerLog,
	)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if _, err = s.db.Exec(
		`
		INSERT OR REPLACE INTO messages_fts(msg_id, body, thread_id, sender_id) VALUES (?,?,?,?)
	`,
		msg.ID, SegmentCJKForIndex(msg.Body), msg.ThreadID, msg.SenderID,
	); err != nil {
		log.Printf("[storage] FTS insert failed for msg %s: %v", msg.ID, err)
	}
	snippet := msg.Body
	if snippet == "" {
		snippet = "[" + string(msg.ContentType) + "]"
	}
	if len(snippet) > 120 {
		snippet = snippet[:120] + "…"
	}
	_, err = s.db.Exec(`
		UPDATE threads SET
		last_snippet = ?, last_sender = ?,
		last_content_type = ?, last_message_at = ?,
		updated_at = ?,
		unread_count = CASE WHEN ? = 0 THEN unread_count + 1 ELSE unread_count END
		WHERE id = ?
	`, snippet, msg.SenderID, string(msg.ContentType),
		msg.SentAt.UnixMilli(), time.Now().UnixMilli(),
		boolInt(msg.IsSelf), msg.ThreadID,
	)
	return err
}

func (s *SQLiteStore) GetThread(threadID string) (*Thread, error) {
	row := s.db.QueryRow(`
		SELECT id, protocol, display_name, avatar_url, participants,
		unread_count, is_pinned, mute_until,
		last_snippet, last_sender, last_content_type, last_message_at, updated_at
		FROM threads WHERE id = ?
	`, threadID)
	return scanThread(row)
}

func (s *SQLiteStore) GetRecentMessages(threadID string, since time.Time, limit int) ([]*Message, error) {
	rows, err := s.db.Query(
		msgSelectSQL+" WHERE thread_id = ? AND sent_at >= ? ORDER BY sent_at ASC LIMIT ?",
		threadID, since.UnixMilli(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *SQLiteStore) GetThreadList(protocol string, limit, offset int) ([]*Thread, error) {
	q := `
		SELECT id, protocol, display_name, avatar_url, participants,
		unread_count, is_pinned, mute_until,
		last_snippet, last_sender, last_content_type, last_message_at, updated_at
		FROM threads
	`
	args := []interface{}{}
	if protocol != "" && protocol != "all" {
		q += " WHERE protocol = ?"
		args = append(args, protocol)
	}
	q += " ORDER BY is_pinned DESC, updated_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var threads []*Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *SQLiteStore) UpsertThread(t *Thread) error {
	pJSON, _ := json.Marshal(t.Participants)
	_, err := s.db.Exec(`
		INSERT INTO threads (id, protocol, display_name, avatar_url, participants, unread_count, is_pinned, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		display_name = excluded.display_name, avatar_url = excluded.avatar_url,
		is_pinned = excluded.is_pinned, updated_at = excluded.updated_at
	`, t.ID, string(t.Protocol), t.DisplayName, nilStr(t.AvatarURL),
		string(pJSON), t.UnreadCount, boolInt(t.IsPinned), t.UpdatedAt.UnixMilli())
	return err
}

func (s *SQLiteStore) GetContactsByIDs(ids []string) ([]*Contact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.Repeat("?,", len(ids))
	ph = ph[:len(ph)-1]
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(`
		SELECT id, display_name, avatar_url, is_pinned, handles FROM contacts WHERE id IN (
	`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var contacts []*Contact
	for rows.Next() {
		var (
			c         Contact
			avatarURL sql.NullString
			handles   string
		)
		if err := rows.Scan(&c.ID, &c.DisplayName, &avatarURL, &c.IsPinned, &handles); err != nil {
			return nil, err
		}
		if avatarURL.Valid {
			c.AvatarURL = avatarURL.String
		}
		json.Unmarshal([]byte(handles), &c.Handles) //nolint:errcheck
		contacts = append(contacts, &c)
	}
	return contacts, rows.Err()
}

func (s *SQLiteStore) UpsertContact(c *Contact) error {
	h, _ := json.Marshal(c.Handles)
	_, err := s.db.Exec(`
		INSERT INTO contacts (id, display_name, avatar_url, is_pinned, handles) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		display_name = excluded.display_name, avatar_url = excluded.avatar_url,
		is_pinned = excluded.is_pinned, handles = excluded.handles
	`, c.ID, c.DisplayName, nilStr(c.AvatarURL), boolInt(c.IsPinned), string(h))
	return err
}

func (s *SQLiteStore) SearchMessages(query string, limit int) ([]*Message, error) {
	segmented := SegmentCJKQuery(query)
	if segmented == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT m.id, m.upstream_id, m.thread_id, m.protocol, m.sender_id, m.is_self,
		m.content_type, m.body,
		m.media_stream_url, m.media_thumb_url, m.media_mime_type,
		m.redirected_type, m.redirected_label, m.redirected_link,
		m.reply_to_id, m.reply_snippet, m.status, m.sent_at, m.received_at
		FROM messages_fts JOIN messages m ON m.id = messages_fts.msg_id
		WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?
	`, fts5EscapeTokens(segmented), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *SQLiteStore) MarkMessagesRead(threadID string) error {
	_, err := s.db.Exec(`
		UPDATE threads SET unread_count = 0 WHERE id = ?
	`, threadID)
	return err
}

func (s *SQLiteStore) UpdateMessageStatus(messageID string, status DeliveryStatus) error {
	_, err := s.db.Exec(`
		UPDATE messages SET status = ? WHERE id = ?
	`, string(status), messageID)
	return err
}

func (s *SQLiteStore) GetMatrixRoomID(threadID string, proto SourceProtocol) (string, error) {
	var roomID string
	err := s.db.QueryRow(
		`
		SELECT matrix_room_id FROM thread_bridge_ids WHERE thread_id = ? AND protocol = ?
	`,
		threadID, string(proto),
	).Scan(&roomID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return roomID, err
}

func (s *SQLiteStore) SaveMatrixRoomID(threadID string, proto SourceProtocol, roomID string) error {
	_, err := s.db.Exec(`
		INSERT INTO thread_bridge_ids (thread_id, protocol, matrix_room_id, created_at)
		VALUES (?,?,?,?)
		ON CONFLICT(thread_id, protocol) DO UPDATE SET matrix_room_id = excluded.matrix_room_id
	`, threadID, string(proto), roomID, time.Now().UnixMilli())
	return err
}

func (s *SQLiteStore) SaveTopicSummary(ts *TopicSummary) error {
	topicsJSON, err := json.Marshal(ts.Topics)
	if err != nil {
		return fmt.Errorf("marshal topics: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO topic_summaries (id, thread_id, period_start, period_end, topics, message_count, generated_by, created_at)
		VALUES (?,?,?,?,?,?,?,?)
	`, generateUUIDv7(), ts.ThreadID,
		ts.PeriodStart.UnixMilli(), ts.PeriodEnd.UnixMilli(),
		string(topicsJSON), ts.MessageCount, ts.GeneratedBy, time.Now().UnixMilli())
	return err
}

func (s *SQLiteStore) GetThreadsWithOldMessages(maxAge time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	rows, err := s.db.Query(`
		SELECT DISTINCT thread_id FROM messages
		WHERE received_at < ? AND thread_id NOT IN (SELECT id FROM threads WHERE is_pinned = 1)
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLiteStore) GetOldMessages(threadID string, maxAge time.Duration) ([]*Message, error) {
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	rows, err := s.db.Query(
		msgSelectSQL+" WHERE thread_id = ? AND received_at < ? ORDER BY sent_at ASC",
		threadID, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *SQLiteStore) HasTopicSummary(threadID string, periodEnd time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM topic_summaries WHERE thread_id = ? AND period_end >= ?
	`,
		threadID, periodEnd.UnixMilli()).Scan(&n)
	return n > 0, err
}

func (s *SQLiteStore) EvictSummarisedMessages(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	result, err := s.db.Exec(`
		DELETE FROM messages WHERE received_at < ?
		AND thread_id NOT IN (SELECT id FROM threads WHERE is_pinned = 1)
		AND thread_id IN (SELECT thread_id FROM topic_summaries WHERE period_end >= ?)
	`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	_, _ = s.db.Exec(`
		DELETE FROM media_refs WHERE message_id NOT IN (SELECT id FROM messages)
	`)
	return n, nil
}

func (s *SQLiteStore) EvictStaleMessages(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	result, err := s.db.Exec(`
		DELETE FROM messages WHERE received_at < ?
		AND thread_id NOT IN (SELECT id FROM threads WHERE is_pinned = 1)
	`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

type scanner interface{ Scan(dest ...interface{}) error }

func scanThread(row scanner) (*Thread, error) {
	var (
		t                                          Thread
		avatarURL, lastSnippet, lastSender, lastCType sql.NullString
		muteUntil, lastMsgAt                       sql.NullInt64
		participants                               string
		updatedAt                                  int64
		proto                                      string
	)
	err := row.Scan(
		&t.ID, &proto, &t.DisplayName, &avatarURL, &participants,
		&t.UnreadCount, &t.IsPinned, &muteUntil,
		&lastSnippet, &lastSender, &lastCType, &lastMsgAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.Protocol = SourceProtocol(proto)
	t.UpdatedAt = time.UnixMilli(updatedAt)
	t.AvatarURL = nullStr(avatarURL)
	if muteUntil.Valid {
		mu := time.UnixMilli(muteUntil.Int64)
		t.MuteUntil = &mu
	}
	if lastSnippet.Valid && lastMsgAt.Valid {
		ct := ContentText
		if lastCType.Valid {
			ct = ContentType(lastCType.String)
		}
		t.LastMessage = &MessagePreview{
			SenderName: lastSender.String, ContentType: ct,
			Snippet: lastSnippet.String, Timestamp: time.UnixMilli(lastMsgAt.Int64),
		}
	}
	json.Unmarshal([]byte(participants), &t.Participants) //nolint:errcheck
	return &t, nil
}

func scanMessages(rows *sql.Rows) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		var (
			m                                  Message
			upstreamID, body                   sql.NullString
			mediaStream, mediaThumb, mediaMIME sql.NullString
			redirType, redirLabel, redirLink   sql.NullString
			replyToID, replySnippet            sql.NullString
			sentAtMs, receivedAtMs             int64
			proto, ctype, status               string
			isSelf                             int
		)
		if err := rows.Scan(
			&m.ID, &upstreamID, &m.ThreadID, &proto, &m.SenderID, &isSelf,
			&ctype, &body,
			&mediaStream, &mediaThumb, &mediaMIME,
			&redirType, &redirLabel, &redirLink,
			&replyToID, &replySnippet,
			&status, &sentAtMs, &receivedAtMs,
		); err != nil {
			return nil, err
		}
		m.Protocol = SourceProtocol(proto)
		m.ContentType = ContentType(ctype)
		m.Status = DeliveryStatus(status)
		m.IsSelf = isSelf == 1
		m.SentAt = time.UnixMilli(sentAtMs)
		m.ReceivedAt = time.UnixMilli(receivedAtMs)
		m.UpstreamID = nullStr(upstreamID)
		m.Body = nullStr(body)
		if replyToID.Valid {
			rid := replyToID.String; m.ReplyToID = &rid
		}
		if replySnippet.Valid {
			rs := replySnippet.String; m.ReplySnippet = &rs
		}
		if mediaStream.Valid {
			m.Media = &MediaRef{StreamURL: mediaStream.String, ThumbURL: mediaThumb.String, MIMEType: mediaMIME.String}
		}
		if redirType.Valid {
			m.Redirected = &RedirectedPayload{OriginalType: redirType.String, DisplayLabel: redirLabel.String, DeepLink: redirLink.String}
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func fts5EscapeTokens(q string) string {
	parts := strings.Split(q, " AND ")
	escaped := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.NewReplacer(`"`, `""`, `*`, "", `^`, "").Replace(p)
		escaped = append(escaped, `"`+p+`"`)
	}
	return strings.Join(escaped, " AND ")
}
