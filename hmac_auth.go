// hmac_auth.go — HMAC-SHA256 signed-request auth for /internal/* endpoints.

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	// headerNonce is the Unix timestamp of the request (seconds).
	headerNonce = "X-Ghost-Nonce"
	// headerSig is the hex-encoded HMAC-SHA256 signature.
	headerSig = "X-Ghost-Sig"
	// replayWindowSec is the maximum allowed clock skew in seconds.
	replayWindowSec = 30
)

type HMACAuthMiddleware struct {
	secret []byte
	next   http.Handler
}

func NewHMACAuthMiddleware(hexSecret string, next http.Handler) (*HMACAuthMiddleware, error) {
	if len(hexSecret) < 64 {
		return nil, fmt.Errorf("ghost: GHOST_INTERNAL_SECRET must be >= 64 hex chars (32 bytes)")
	}
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, fmt.Errorf("ghost: GHOST_INTERNAL_SECRET is not valid hex: %w", err)
	}
	return &HMACAuthMiddleware{secret: secret, next: next}, nil
}

func (m *HMACAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := m.verify(r); err != nil {
		http.Error(w, fmt.Sprintf("unauthorized: %v", err), http.StatusUnauthorized)
		return
	}
	m.next.ServeHTTP(w, r)
}

func (m *HMACAuthMiddleware) verify(r *http.Request) error {
	nonceStr, sigHex := r.Header.Get(headerNonce), r.Header.Get(headerSig)
	if nonceStr == "" || sigHex == "" {
		return fmt.Errorf("missing %s or %s header", headerNonce, headerSig)
	}
	nonce, err := strconv.ParseInt(nonceStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid nonce: %w", err)
	}
	if skew := time.Now().Unix() - nonce; skew < -replayWindowSec || skew > replayWindowSec {
		return fmt.Errorf("timestamp outside replay window (%ds skew)", skew)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	provided, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	if !hmac.Equal(computeHMACSignature(m.secret, nonceStr, body), provided) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func computeHMACSignature(secret []byte, nonce string, body []byte) []byte {
	h := sha256.Sum256(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(nonce + ":" + hex.EncodeToString(h[:])))
	return mac.Sum(nil)
}

func SignRequest(req *http.Request, hexSecret string, body []byte) error {
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return fmt.Errorf("invalid secret: %w", err)
	}

	nonce := strconv.FormatInt(time.Now().Unix(), 10)
	sig := computeHMACSignature(secret, nonce, body)

	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSig, hex.EncodeToString(sig))
	return nil
}

// InternalRouter wraps /internal/* routes with HMAC authentication.
type InternalRouter struct {
	auth *HMACAuthMiddleware
	mux  *http.ServeMux
}

// NewInternalRouter creates an HMAC-protected sub-router for internal endpoints.
func NewInternalRouter(hexSecret string) (*InternalRouter, error) {
	mux := http.NewServeMux()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	})

	auth, err := NewHMACAuthMiddleware(hexSecret, inner)
	if err != nil {
		return nil, err
	}

	return &InternalRouter{auth: auth, mux: mux}, nil
}

func (ir *InternalRouter) Handle(pattern string, handler http.Handler) {
	ir.mux.Handle(pattern, handler)
}

func (ir *InternalRouter) HandleFunc(pattern string, fn http.HandlerFunc) {
	ir.mux.HandleFunc(pattern, fn)
}

func (ir *InternalRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ir.auth.ServeHTTP(w, r)
}
