// deadrop.go — Offline Command Relay (email dead-drop + DNS tunnel)
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type CommandType string

const (
	CmdShutdownSessions CommandType = "SHUTDOWN_SESSIONS"
	CmdWipeHistory      CommandType = "WIPE_HISTORY"
	CmdGetLastMessages  CommandType = "GET_LAST_MESSAGES"
	CmdRevokeDevice     CommandType = "REVOKE_DEVICE"
	CmdPanic            CommandType = "PANIC"
	CmdHealthCheck      CommandType = "HEALTH_CHECK"
)

type DeadDropCommand struct {
	Type     CommandType `json:"type"`
	IssuedAt time.Time   `json:"issued_at"`
	Nonce    string      `json:"nonce"`
	Source   string      `json:"source"` // "email" | "dns"
}

type DeadDropManager struct {
	secret         []byte
	commandChan    chan DeadDropCommand
	executedNonces sync.Map
	executor       CommandExecutor
}

type CommandExecutor interface{ Execute(cmd DeadDropCommand) error }

func NewDeadDropManager(hexSecret string, executor CommandExecutor) (*DeadDropManager, error) {
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, fmt.Errorf("invalid secret: %w", err)
	}
	return &DeadDropManager{
		secret:      secret,
		commandChan: make(chan DeadDropCommand, 64),
		executor:    executor,
	}, nil
}

func (m *DeadDropManager) Start(emailCfg EmailPollConfig, dnsAddr string) {
	go m.emailPoller(emailCfg)
	go m.dnsListener(dnsAddr)
	go m.commandDispatcher()
	log.Printf("[deadrop] started: email_interval=%v dns_addr=%s", emailCfg.PollInterval, dnsAddr)
}

func (m *DeadDropManager) commandDispatcher() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			m.pruneNonces()
		}
	}()
	for cmd := range m.commandChan {
		if _, loaded := m.executedNonces.LoadOrStore(cmd.Nonce, time.Now()); loaded {
			log.Printf("[deadrop] replay detected for nonce %s, dropping", cmd.Nonce[:8])
			continue
		}
		log.Printf("[deadrop] executing command: type=%s source=%s", cmd.Type, cmd.Source)
		if err := m.executor.Execute(cmd); err != nil {
			log.Printf("[deadrop] execution error: %v", err)
		}
	}
}

func (m *DeadDropManager) pruneNonces() {
	cutoff := time.Now().Add(-24 * time.Hour)
	m.executedNonces.Range(func(k, v interface{}) bool {
		if t, ok := v.(time.Time); ok && t.Before(cutoff) {
			m.executedNonces.Delete(k)
		}
		return true
	})
}

type EmailPollConfig struct {
	MailboxPath   string
	PollInterval  time.Duration
	CommandPrefix string
	AllowedSender string
}

func (m *DeadDropManager) emailPoller(cfg EmailPollConfig) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for range ticker.C {
		entries, err := os.ReadDir(cfg.MailboxPath)
		if err != nil { log.Printf("[deadrop/email] mailbox read error: %v", err); continue }
		for _, entry := range entries {
			if entry.IsDir() { continue }
			path := cfg.MailboxPath + "/" + entry.Name()
			if err := m.processEmailFile(path, cfg); err != nil {
				log.Printf("[deadrop/email] process error %s: %v", entry.Name(), err); continue
			}
			os.Rename(path, strings.Replace(path, "/new/", "/cur/", 1))
		}
	}
}

func (m *DeadDropManager) processEmailFile(path string, cfg EmailPollConfig) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	headers := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			headers[strings.ToLower(strings.TrimSpace(line[:idx]))] = strings.TrimSpace(line[idx+1:])
		}
	}

	if !strings.HasPrefix(headers["subject"], cfg.CommandPrefix) {
		return nil
	}
	if fromAddr := extractEmailAddress(headers["from"]); cfg.AllowedSender != "" && fromAddr != cfg.AllowedSender {
		log.Printf("[deadrop/email] rejected unauthorized sender: %s", fromAddr)
		return nil
	}

	cmdType := CommandType(strings.TrimSpace(headers["x-ghost-cmd"]))
	nonce := strings.TrimSpace(headers["x-ghost-nonce"])
	sigHex := strings.TrimSpace(headers["x-ghost-sig"])
	if cmdType == "" || nonce == "" || sigHex == "" {
		log.Printf("[deadrop/email] missing command headers in %s", path)
		return nil
	}

	if !m.verifyHMAC([]byte(string(cmdType)+":"+nonce), sigHex) {
		log.Printf("[deadrop/email] HMAC verification failed for cmd %s", cmdType)
		return nil
	}
	issuedAt, err := parseNonceTimestamp(nonce)
	if err != nil || time.Since(issuedAt) > 30*time.Minute {
		log.Printf("[deadrop/email] nonce expired or invalid: %s", nonce)
		return nil
	}
	m.commandChan <- DeadDropCommand{Type: cmdType, IssuedAt: issuedAt, Nonce: nonce, Source: "email"}
	return nil
}

func (m *DeadDropManager) dnsListener(addr string) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("[deadrop/dns] failed to bind %s: %v", addr, err)
		return
	}
	defer conn.Close()
	log.Printf("[deadrop/dns] listening on %s", addr)
	buf := make([]byte, 512)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			continue
		}
		domain := parseDNSQueryName(buf[:n])
		if domain == "" {
			continue
		}
		cmd, err := m.parseDNSCommand(domain)
		if err != nil {
			continue
		}
		log.Printf("[deadrop/dns] command: type=%s nonce=%s", cmd.Type, cmd.Nonce[:8])
		m.commandChan <- *cmd
	}
}

func (m *DeadDropManager) parseDNSCommand(domain string) (*DeadDropCommand, error) {
	label := strings.ToLower(strings.SplitN(domain, ".", 2)[0])
	parts := strings.SplitN(label, "-", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a command label")
	}
	cmdStr := strings.ToUpper(parts[0])
	nonce, hmacPrefix := parts[1], parts[2]

	cmdMap := map[string]CommandType{
		"PANIC": CmdPanic, "WIPE": CmdWipeHistory, "SHUTDOWN": CmdShutdownSessions,
		"REVOKE": CmdRevokeDevice, "HEALTH": CmdHealthCheck,
	}
	cmdType, ok := cmdMap[cmdStr]
	if !ok {
		return nil, fmt.Errorf("unknown command: %s", cmdStr)
	}

	msg := string(cmdType) + ":" + nonce
	expectedHMAC := m.hmacHex([]byte(msg))
	if len(hmacPrefix) < 20 || !strings.HasPrefix(expectedHMAC, hmacPrefix[:min(len(hmacPrefix), len(expectedHMAC))]) {
		return nil, fmt.Errorf("HMAC mismatch")
	}
	issuedAt, err := parseNonceTimestamp(nonce)
	if err != nil || time.Since(issuedAt) > 30*time.Minute {
		return nil, fmt.Errorf("nonce expired")
	}
	return &DeadDropCommand{Type: cmdType, IssuedAt: issuedAt, Nonce: nonce, Source: "dns"}, nil
}

type GhostCommandExecutor struct{ store *SQLiteStore }

func NewGhostCommandExecutor(store *SQLiteStore) *GhostCommandExecutor {
	return &GhostCommandExecutor{store: store}
}

func (e *GhostCommandExecutor) Execute(cmd DeadDropCommand) error {
	log.Printf("[deadrop] cmd=%s src=%s", cmd.Type, cmd.Source)
	switch cmd.Type {
	case CmdPanic:
		return e.executePanic()
	case CmdWipeHistory:
		_, err := e.store.EvictStaleMessages(24 * time.Hour); return err
	case CmdShutdownSessions, CmdRevokeDevice, CmdHealthCheck, CmdGetLastMessages:
		return nil
	default:
		return fmt.Errorf("unknown command type: %s", cmd.Type)
	}
}

func (e *GhostCommandExecutor) executePanic() error {
	var errs []string
	if _, err := e.store.EvictStaleMessages(0); err != nil {
		errs = append(errs, fmt.Sprintf("evict: %v", err))
	}
	removeIfExists := func(paths []string, label string) {
		for _, p := range paths {
			if p != "" {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Sprintf("%s %s: %v", label, p, err))
				}
			}
		}
	}
	removeIfExists([]string{"/etc/wireguard/ghost.conf", os.Getenv("WG_CONFIG_PATH")}, "remove wg config")
	removeIfExists([]string{os.Getenv("TARDI_DB_PATH"), "/var/ghost/ghost_server.db"}, "remove db")
	if len(errs) > 0 {
		return fmt.Errorf("panic incomplete: %s", strings.Join(errs, "; "))
	}
	log.Printf("[deadrop] PANIC complete — all keys and sessions wiped")
	return nil
}

func parseDNSQueryName(pkt []byte) string {
	if len(pkt) < 12 { return "" }
	var labels []string
	for off := 12; off < len(pkt); {
		l := int(pkt[off]); off++
		if l == 0 { break }
		if off+l > len(pkt) { return "" }
		labels = append(labels, string(pkt[off:off+l])); off += l
	}
	return strings.ToLower(strings.Join(labels, "."))
}

func (m *DeadDropManager) hmacHex(msg []byte) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(msg)
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *DeadDropManager) verifyHMAC(msg []byte, sigHex string) bool {
	expected := m.hmacHex(msg)
	provided, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	expectedBytes, _ := hex.DecodeString(expected)
	return hmac.Equal(expectedBytes, provided)
}

func parseNonceTimestamp(nonce string) (time.Time, error) {
	if len(nonce) < 10 {
		return time.Time{}, fmt.Errorf("nonce too short")
	}
	var ts int64
	if _, err := fmt.Sscanf(nonce[:10], "%d", &ts); err != nil {
		return time.Time{}, err
	}
	return time.Unix(ts, 0), nil
}

func extractEmailAddress(from string) string {
	if start := strings.Index(from, "<"); start >= 0 {
		if end := strings.Index(from[start:], ">"); end >= 0 {
			return strings.ToLower(from[start+1 : start+end])
		}
	}
	return strings.ToLower(strings.TrimSpace(from))
}

func GenerateEmailCommand(hexSecret string, cmdType CommandType) (map[string]string, error) {
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, err
	}
	nonceSuffix := make([]byte, 3)
	if _, err := rand.Read(nonceSuffix); err != nil {
		return nil, fmt.Errorf("rand nonce: %w", err)
	}
	nonce := fmt.Sprintf("%d%06x", time.Now().Unix(), nonceSuffix)
	msg := string(cmdType) + ":" + nonce
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return map[string]string{
		"X-Ghost-Cmd":   string(cmdType),
		"X-Ghost-Nonce": nonce,
		"X-Ghost-Sig":   hex.EncodeToString(mac.Sum(nil)),
		"Subject":       "Re: Course Enrollment Confirmation — Action Required",
	}, nil
}

func GenerateDNSCommand(hexSecret string, cmdType CommandType) (string, error) {
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return "", err
	}
	nonce := fmt.Sprintf("%d%06x", time.Now().Unix(), time.Now().UnixNano()%0xFFFFFF)
	msg := string(cmdType) + ":" + nonce
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	hmacHex := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s-%s-%s", strings.ToLower(string(cmdType)), nonce, hmacHex[:20]), nil
}
