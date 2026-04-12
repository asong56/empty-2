// main.go — TardiTalk backend server entry point.
package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var AppVersion = "dev"

type TardiServer struct {
	washer         *ContentWasher
	mailer         *OutboundMailer
	mailCfg        ShadowMailConfig
	store          *SQLiteStore
	bridges        *BridgeDispatcher
	media          *MediaWorkerPool
	deadDrop       *DeadDropManager
	summarizer     *TextRankSummarizer
	internalRouter *InternalRouter
	fcm            *FCMDispatcher
	wsClients      map[*websocket.Conn]*sync.Mutex
	wsClientsMu    sync.RWMutex
	broadcast      chan PushEvent
}

type GhostServer = TardiServer

func (s *TardiServer) addWSClient(c *websocket.Conn) {
	s.wsClientsMu.Lock()
	s.wsClients[c] = &sync.Mutex{}
	s.wsClientsMu.Unlock()
}
func (s *TardiServer) removeWSClient(c *websocket.Conn) {
	s.wsClientsMu.Lock()
	delete(s.wsClients, c)
	s.wsClientsMu.Unlock()
}
func (s *TardiServer) wsClientCount() int {
	s.wsClientsMu.RLock()
	n := len(s.wsClients)
	s.wsClientsMu.RUnlock()
	return n
}
func (s *TardiServer) copyWSClients() []*websocket.Conn {
	s.wsClientsMu.RLock()
	out := make([]*websocket.Conn, 0, len(s.wsClients))
	for c := range s.wsClients {
		out = append(out, c)
	}
	s.wsClientsMu.RUnlock()
	return out
}

func (s *TardiServer) wsLockConn(conn *websocket.Conn) (*sync.Mutex, bool) {
	s.wsClientsMu.RLock()
	mu, ok := s.wsClients[conn]
	s.wsClientsMu.RUnlock()
	return mu, ok
}
func (s *TardiServer) wsWriteJSON(conn *websocket.Conn, v interface{}) error {
	mu, ok := s.wsLockConn(conn)
	if !ok { return fmt.Errorf("ws: connection not registered") }
	mu.Lock(); defer mu.Unlock()
	return conn.WriteJSON(v)
}
func (s *TardiServer) wsWriteMessage(conn *websocket.Conn, msgType int, data []byte) error {
	mu, ok := s.wsLockConn(conn)
	if !ok { return fmt.Errorf("ws: connection not registered") }
	mu.Lock(); defer mu.Unlock()
	return conn.WriteMessage(msgType, data)
}

func NewTardiServer() *TardiServer {
	mailCfg := DefaultShadowMailConfig()
	if v := os.Getenv("TARDI_DOMAIN"); v != "" { mailCfg.GhostDomain = v }
	if v := os.Getenv("GMAIL_USER"); v != "" { mailCfg.GmailUser = v }
	if v := os.Getenv("GMAIL_PASSWORD"); v != "" { mailCfg.GmailPassword = v }
	if v := os.Getenv("SENDING_ALIAS"); v != "" { mailCfg.SendingAlias = v }

	dbPath := envOrDefault("TARDI_DB_PATH", "/var/tarditalk/tarditalk.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		log.Printf("[tardi] SQLite unavailable (%v), memory-only mode", err)
		store = nil
	}

	bridgeRegistry := NewBridgeRegistry([]BridgeConfig{
		{Protocol: ProtoWeChat, BaseURL: envOrDefault("WECHAT_BRIDGE_URL", "http://127.0.0.1:29330"), AppserviceToken: os.Getenv("WECHAT_BRIDGE_TOKEN"), Timeout: 15 * time.Second},
		{Protocol: ProtoWhatsApp, BaseURL: envOrDefault("WHATSAPP_BRIDGE_URL", "http://127.0.0.1:29318"), AppserviceToken: os.Getenv("WHATSAPP_BRIDGE_TOKEN"), Timeout: 15 * time.Second},
		{Protocol: ProtoSignal, BaseURL: envOrDefault("SIGNAL_BRIDGE_URL", "http://127.0.0.1:29328"), AppserviceToken: os.Getenv("SIGNAL_BRIDGE_TOKEN"), Timeout: 15 * time.Second},
	})

	internalSecret := os.Getenv("TARDI_INTERNAL_SECRET")
	var ir *InternalRouter
	if internalSecret != "" {
		ir, err = NewInternalRouter(internalSecret)
		if err != nil {
			log.Printf("[tardi] HMAC router init failed: %v", err)
		}
	}

	var dd *DeadDropManager
	if internalSecret != "" {
		executor := NewGhostCommandExecutor(store)
		dd, err = NewDeadDropManager(internalSecret, executor)
		if err != nil {
			log.Printf("[tardi] dead-drop init failed: %v", err)
		}
	}

	var fcmDB *sql.DB
	if store != nil {
		fcmDB = store.db
	}

	return &TardiServer{
		washer:         NewContentWasher(DefaultWasherConfig()),
		mailer:         NewOutboundMailer(mailCfg),
		mailCfg:        mailCfg,
		store:          store,
		bridges:        NewBridgeDispatcher(bridgeRegistry, store),
		media:          NewMediaWorkerPool(DefaultMediaConfig()),
		deadDrop:       dd,
		summarizer:     NewTextRankSummarizer(DefaultSummarizerConfig()),
		internalRouter: ir,
		fcm:            NewFCMDispatcher(fcmDB),
		wsClients:      make(map[*websocket.Conn]*sync.Mutex),
		broadcast:      make(chan PushEvent, 256),
	}
}

func (s *TardiServer) Run(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/threads", s.handleThreads)
	mux.HandleFunc("/api/threads/", s.handleThreadDetail)
	mux.HandleFunc("/api/contacts", s.handleContacts)
	mux.HandleFunc("/api/messages/send", s.handleSend)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/platform/health", s.handlePlatformHealth)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": AppVersion})
	})
	mux.HandleFunc("/api/device/register", s.HandleDeviceRegister)
	mux.HandleFunc("/api/decoy/feed", s.handleDecoyFeed)

	if s.internalRouter != nil {
		s.internalRouter.HandleFunc("/internal/ingest", s.handleIngest)
		s.internalRouter.HandleFunc("/internal/retract", s.handleRetract)
		s.internalRouter.HandleFunc("/internal/voip", s.VoIPHandler)
		mux.Handle("/internal/", s.internalRouter)
	} else {
		mux.HandleFunc("/internal/ingest", s.handleIngest)
		mux.HandleFunc("/internal/retract", s.handleRetract)
		mux.HandleFunc("/internal/voip", s.VoIPHandler)
	}
	mux.HandleFunc("/media/", s.media.MediaHandler)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.Handle("/", securityHeaders(http.FileServer(http.Dir("./frontend"))))

	go s.broadcastLoop()
	if s.deadDrop != nil {
		s.deadDrop.Start(EmailPollConfig{
			MailboxPath:   envOrDefault("TARDI_MAILBOX_PATH", "/var/mail/tarditalk/new"),
			PollInterval:  5 * time.Minute,
			CommandPrefix: "Re: Course Enrollment",
			AllowedSender: os.Getenv("TARDI_ADMIN_EMAIL"),
		}, envOrDefault("TARDI_DNS_ADDR", "127.0.0.1:5353"))
	}
	if s.store != nil {
		go s.purgeAndSummarizeLoop()
	}
	go s.platformHealthLoop()

	log.Printf("[tardi] listening on %s (version=%s)", addr, AppVersion)
	return http.ListenAndServe(addr, mux)
}

func (s *TardiServer) handleThreads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit, offset := 100, 0
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	if o, err := strconv.Atoi(q.Get("offset")); err == nil && o >= 0 {
		offset = o
	}
	var threads interface{}
	if s.store != nil {
		list, err := s.store.GetThreadList(q.Get("proto"), limit, offset)
		if err != nil {
			threads = sampleThreads()
		} else {
			threads = list
		}
	} else {
		threads = sampleThreads()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"threads": threads, "limit": limit, "offset": offset})
}

func (s *TardiServer) handleThreadDetail(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/threads/")
	parts := strings.SplitN(path, "/", 2)
	threadID := parts[0]
	if len(parts) > 1 && parts[1] == "messages" {
		s.handleMessages(w, r, threadID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"thread_id": threadID, "status": "ok"})
}

func (s *TardiServer) handleMessages(w http.ResponseWriter, r *http.Request, threadID string) {
	q := r.URL.Query()
	since := time.Now().Add(-3 * 24 * time.Hour)
	if afterStr := q.Get("after"); afterStr != "" {
		if ms, err := strconv.ParseInt(afterStr, 10, 64); err == nil {
			since = time.UnixMilli(ms)
		}
	}
	limit := 200
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	var msgs interface{}
	if s.store != nil {
		list, err := s.store.GetRecentMessages(threadID, since, limit)
		if err != nil {
			msgs = sampleMessages(threadID)
		} else {
			msgs = list
		}
	} else {
		msgs = sampleMessages(threadID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"thread_id": threadID, "messages": msgs})
}

func (s *TardiServer) handleContacts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"contacts": sampleContacts()})
}

func (s *TardiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) { return }
	var req struct {
		ThreadID    string         `json:"thread_id"`
		Protocol    SourceProtocol `json:"protocol"`
		Body        string         `json:"body"`
		ContentType ContentType    `json:"content_type"`
		ReplyToID   *string        `json:"reply_to_id,omitempty"`
	}
	if !decodeJSON(w, r, &req) { return }
	msg := &Message{
		ID: generateUUIDv7(), ThreadID: req.ThreadID, Protocol: req.Protocol,
		SenderID: "self", IsSelf: true, SentAt: time.Now(), ReceivedAt: time.Now(),
		ContentType: ContentText, Body: req.Body, Status: StatusPending, ReplyToID: req.ReplyToID,
	}
	if s.store != nil {
		s.store.SaveMessage(msg)
	}
	var sendErr error
	if req.Protocol == ProtoEmail {
		sendErr = s.sendEmail(msg, req.ThreadID)
	} else if s.bridges != nil {
		sendErr = s.bridges.Dispatch(msg)
	}
	if sendErr != nil {
		msg.Status = StatusFailed
	} else if msg.Status == StatusPending {
		msg.Status = StatusSent
	}
	if s.store != nil {
		s.store.UpdateMessageStatus(msg.ID, msg.Status)
	}
	payload, _ := json.Marshal(msg)
	eventType := EventNewMessage
	if sendErr != nil {
		eventType = EventStatusUpdate
	}
	s.broadcast <- PushEvent{Type: eventType, Timestamp: time.Now(), Payload: json.RawMessage(payload)}
	writeJSON(w, http.StatusOK, map[string]interface{}{"message_id": msg.ID, "status": msg.Status})
}

func (s *TardiServer) sendEmail(msg *Message, threadID string) error {
	if s.mailer == nil {
		return fmt.Errorf("email: mailer not initialised")
	}
	var recipients []string
	if s.store != nil {
		thread, err := s.store.GetThread(threadID)
		if err != nil || thread == nil {
			return fmt.Errorf("email: thread not found")
		}
		contacts, err := s.store.GetContactsByIDs(thread.Participants)
		if err != nil {
			return fmt.Errorf("email: fetch contacts: %w", err)
		}
		for _, c := range contacts {
			if addr, ok := c.Handles[ProtoEmail]; ok && addr != "" {
				if c.DisplayName != "" {
					recipients = append(recipients, c.DisplayName+" <"+addr+">")
				} else {
					recipients = append(recipients, addr)
				}
			}
		}
	}
	if len(recipients) == 0 {
		return fmt.Errorf("email: no recipients for thread %s", threadID)
	}
	if err := s.mailer.Send(GhostEmail{
		From: s.mailCfg.SendingAlias, To: recipients,
		Subject: "Re: (TardiTalk)", BodyPlain: msg.Body,
		MessageID: "<" + msg.ID + "@tarditalk>",
	}); err != nil {
		return fmt.Errorf("email: send: %w", err)
	}
	msg.Status = StatusSent
	return nil
}

func (s *TardiServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "missing query parameter 'q'", http.StatusBadRequest)
		return
	}
	var results interface{} = []interface{}{}
	if s.store != nil {
		if msgs, err := s.store.SearchMessages(query, 50); err == nil {
			results = msgs
		}
	}
	sources := []string{}
	if s.store != nil {
		sources = []string{"server_6m"}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"query": query, "results": results, "sources": sources})
}

func (s *TardiServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) { return }
	if s.internalRouter == nil {
		secret := os.Getenv("TARDI_INTERNAL_SECRET")
		if secret == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Ghost-Secret")), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	var raw struct {
		Protocol     SourceProtocol `json:"protocol"`
		UpstreamType string         `json:"upstream_type"`
		Content      []byte         `json:"content"`
		UpstreamID   string         `json:"upstream_id"`
		SenderID     string         `json:"sender_id"`
		ThreadID     string         `json:"thread_id"`
		MatrixRoomID string         `json:"matrix_room_id"`
		ReceivedAt   time.Time      `json:"received_at"`
	}
	if !decodeJSON(w, r, &raw) { return }
	if raw.ThreadID == "" || raw.SenderID == "" {
		http.Error(w, "thread_id and sender_id are required", http.StatusBadRequest)
		return
	}
	result := s.washer.Wash(raw.Protocol, raw.UpstreamType, raw.Content)
	msg := &Message{
		ID: generateUUIDv7(), UpstreamID: raw.UpstreamID, ThreadID: raw.ThreadID,
		Protocol: raw.Protocol, SenderID: raw.SenderID,
		ReceivedAt: raw.ReceivedAt, SentAt: raw.ReceivedAt,
		ContentType: result.ContentType, Body: result.Body,
		Redirected: result.Redirected, Status: StatusDelivered, WasherLog: result.Log,
	}
	if s.store != nil {
		s.store.SaveMessage(msg)
		if raw.MatrixRoomID != "" {
			s.store.SaveMatrixRoomID(raw.ThreadID, raw.Protocol, raw.MatrixRoomID)
		}
	}
	payload, _ := json.Marshal(msg)
	s.broadcast <- PushEvent{Type: EventNewMessage, Timestamp: time.Now(), Payload: json.RawMessage(payload)}
	if s.fcm != nil {
		s.fcm.NotifyNewMessage(msg)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message_id": msg.ID, "status": "ingested"})
}

func (s *TardiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	bridgeStatus := map[string]string{}
	if s.bridges != nil {
		for proto, status := range s.bridges.HealthCheck() {
			bridgeStatus[string(proto)] = bridgeStatusStr(status)
		}
	}
	dbStatus := map[bool]string{true:"sqlite",false:"memory"}[s.store!=nil]
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok", "version": AppVersion, "timestamp": time.Now().UTC().Format(time.RFC3339),
		"components": map[string]interface{}{
			"washer": "running", "mailer": "ready",
			"ws":      fmt.Sprintf("%d clients", s.wsClientCount()),
			"bridges": bridgeStatus, "db": dbStatus,
		},
	})
}

func (s *TardiServer) handlePlatformHealth(w http.ResponseWriter, r *http.Request) {
	if s.bridges == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"platforms": []interface{}{}})
		return
	}
	platforms := make([]PlatformHealthPayload, 0)
	for proto, status := range s.bridges.HealthCheck() {
		p := PlatformHealthPayload{Protocol: proto, CheckedAt: time.Now(), Status: bridgeStatusStr(status)}
		if status != nil {
			p.NeedsReauth = status.NeedsReauth
		}
		platforms = append(platforms, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"platforms": platforms})
}

func (s *TardiServer) handleRetract(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) { return }
	var req struct {
		UpstreamID string         `json:"upstream_id"`
		ThreadID   string         `json:"thread_id"`
		Protocol   SourceProtocol `json:"protocol"`
		IsSelf     bool           `json:"is_self"`
	}
	if !decodeJSON(w, r, &req) { return }
	retractPayload, _ := json.Marshal(MessageRetractPayload{
		MessageID: req.UpstreamID, ThreadID: req.ThreadID, Protocol: req.Protocol, IsSelf: req.IsSelf,
	})
	s.broadcast <- PushEvent{Type: EventMessageRetract, Timestamp: time.Now(), Payload: json.RawMessage(retractPayload)}
	writeJSON(w, http.StatusOK, map[string]string{"status": "retracted"})
}

func bridgeStatusStr(status *MautrixStatusResponse) string {
	if status != nil && status.OK {
		return "connected"
	}
	return "disconnected"
}

func (s *TardiServer) platformHealthLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if s.bridges == nil {
			continue
		}
		for proto, status := range s.bridges.HealthCheck() {
			p := PlatformHealthPayload{
				Protocol: proto, Status: bridgeStatusStr(status), CheckedAt: time.Now(),
			}
			if status != nil {
				p.NeedsReauth = status.NeedsReauth
			}
			payload, _ := json.Marshal(p)
			s.broadcast <- PushEvent{Type: EventPlatformHealth, Timestamp: time.Now(), Payload: json.RawMessage(payload)}
		}
	}
}

func (s *TardiServer) purgeAndSummarizeLoop() {
	const retentionAge = 180 * 24 * time.Hour
	firstRun := time.NewTimer(5 * time.Minute)
	defer firstRun.Stop()
	<-firstRun.C
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		s.runPurgeCycle(retentionAge)
		<-ticker.C
	}
}

func (s *TardiServer) runPurgeCycle(retentionAge time.Duration) {
	if s.store == nil || s.summarizer == nil {
		return
	}
	threadIDs, err := s.store.GetThreadsWithOldMessages(retentionAge)
	if err != nil {
		log.Printf("[tardi/purge] %v", err)
		return
	}
	if len(threadIDs) == 0 {
		return
	}
	periodEnd := time.Now().Add(-retentionAge)
	summarised := 0
	for _, threadID := range threadIDs {
		if already, _ := s.store.HasTopicSummary(threadID, periodEnd); already {
			summarised++
			continue
		}
		msgs, err := s.store.GetOldMessages(threadID, retentionAge)
		if err != nil || len(msgs) == 0 {
			continue
		}
		summary, err := s.summarizer.Summarize(threadID, msgs)
		if summary == nil {
			if err != nil {
				log.Printf("[tardi/purge] summarize error for %s: %v", threadID, err)
			}
			continue
		}
		if err != nil {
			log.Printf("[tardi/purge] partial summary for %s: %v", threadID, err)
		}
		if saveErr := s.store.SaveTopicSummary(summary); saveErr != nil {
			log.Printf("[tardi/purge] save summary failed for %s: %v", threadID, saveErr)
			continue
		}
		summarised++
	}
	evicted, _ := s.store.EvictSummarisedMessages(retentionAge)
	log.Printf("[tardi/purge] %d/%d threads summarised, %d messages evicted", summarised, len(threadIDs), evicted)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
}

func (s *TardiServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()
	s.addWSClient(conn)
	defer s.removeWSClient(conn)
	log.Printf("[ws] connected from %s (%d total)", r.RemoteAddr, s.wsClientCount())

	healthPayload, _ := json.Marshal(map[string]string{"status": "connected"})
	conn.WriteJSON(PushEvent{Type: EventServerHealth, Timestamp: time.Now(), Payload: json.RawMessage(healthPayload)})

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-readDone:
			return
		case <-ticker.C:
			if err := s.wsWriteMessage(conn, websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *TardiServer) broadcastLoop() {
	for event := range s.broadcast {
		for _, conn := range s.copyWSClients() {
			if err := s.wsWriteJSON(conn, event); err != nil {
				conn.Close()
				s.removeWSClient(conn)
			}
		}
	}
}

func sampleThreads() []Thread {
	now := time.Now()
	return []Thread{
		{ID: "thread_a1b2c3", Protocol: ProtoWeChat, DisplayName: "Alice Chen", Participants: []string{"self", "alice_wechat"}, LastMessage: &MessagePreview{SenderName: "Alice", ContentType: ContentText, Snippet: "Are we still on for Thursday?", Timestamp: now.Add(-5 * time.Minute)}, UnreadCount: 2, IsPinned: true, UpdatedAt: now.Add(-5 * time.Minute)},
		{ID: "thread_d4e5f6", Protocol: ProtoEmail, DisplayName: "Bob (Work)", Participants: []string{"self", "bob@example.com"}, LastMessage: &MessagePreview{SenderName: "Bob", ContentType: ContentText, Snippet: "Please review the Q3 proposal", Timestamp: now.Add(-2 * time.Hour)}, UnreadCount: 0, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "thread_g7h8i9", Protocol: ProtoWhatsApp, DisplayName: "Family Group", Participants: []string{"self", "mum_wa", "dad_wa"}, LastMessage: &MessagePreview{SenderName: "Mum", ContentType: ContentRedirected, Snippet: "[Sticker]", Timestamp: now.Add(-1 * time.Hour)}, UnreadCount: 7, IsPinned: true, UpdatedAt: now.Add(-1 * time.Hour)},
	}
}

func sampleMessages(threadID string) []Message {
	now := time.Now()
	return []Message{
		{ID: "msg_001", ThreadID: threadID, Protocol: ProtoWeChat, SenderID: "alice_wechat", SentAt: now.Add(-30 * time.Minute), ReceivedAt: now.Add(-30 * time.Minute), ContentType: ContentText, Body: "Hey, are you free later?", Status: StatusRead},
		{ID: "msg_002", ThreadID: threadID, Protocol: ProtoWeChat, SenderID: "self", IsSelf: true, SentAt: now.Add(-28 * time.Minute), ReceivedAt: now.Add(-28 * time.Minute), ContentType: ContentText, Body: "Yeah, what's up?", Status: StatusRead},
		{ID: "msg_003", ThreadID: threadID, Protocol: ProtoWeChat, SenderID: "alice_wechat", SentAt: now.Add(-5 * time.Minute), ReceivedAt: now.Add(-5 * time.Minute), ContentType: ContentText, Body: "Are we still on for Thursday?", Status: StatusDelivered},
	}
}

func sampleContacts() []Contact {
	return []Contact{
		{ID: "c_001", DisplayName: "Alice Chen", IsPinned: true, Handles: map[SourceProtocol]string{ProtoWeChat: "wxid_alice123", ProtoEmail: "alice@example.com"}},
		{ID: "c_002", DisplayName: "Bob (Work)", Handles: map[SourceProtocol]string{ProtoEmail: "bob@example.com"}},
		{ID: "c_003", DisplayName: "Family Group", IsPinned: true, Handles: map[SourceProtocol]string{ProtoWhatsApp: "group_family_001"}},
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' blob: data:; style-src 'self' 'unsafe-inline'; script-src 'self'; worker-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self';")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func ptr(s string) *string { return &s }

func (s *TardiServer) handleDecoyFeed(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		http.Error(w, "missing source", http.StatusBadRequest)
		return
	}
	allowlist := map[string]bool{
		"https://feeds.bbci.co.uk/news/rss.xml":                     true,
		"https://rss.nytimes.com/services/xml/rss/nyt/HomePage.xml": true,
		"https://www.theguardian.com/world/rss":                     true,
	}
	if !allowlist[source] {
		http.Error(w, "source not permitted", http.StatusForbidden)
		return
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Get(source)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, io.LimitReader(resp.Body, 512*1024))
}

func main() {
	server := NewTardiServer()
	if err := server.Run(envOrDefault("TARDI_ADDR", ":8888")); err != nil {
		log.Fatalf("[tardi] fatal: %v", err)
	}
}
