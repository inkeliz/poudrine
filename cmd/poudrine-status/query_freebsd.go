package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
)

// Statistics are public. A reader proves possession of its own key, which
// arrives in the request so the confined process never opens an identity file.
func (h *statusHandler) authorizeQuery(w http.ResponseWriter, r *http.Request) bool {
	key := requestauth.PublicKey(r.Header.Get("X-Poudrine-Key"))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		http.Error(w, "Invalid or oversized query", http.StatusBadRequest)
		return false
	}
	nonce, stamp, ok := requestauth.Verify(key, r.Method, r.URL.RequestURI(), r.Header.Get("X-Poudrine-Time"),
		r.Header.Get("X-Poudrine-Nonce"), r.Header.Get("X-Poudrine-Signature"), body)
	if !ok {
		http.Error(w, "Invalid statistics signature", http.StatusUnauthorized)
		return false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(body, &args); err != nil || args == nil || len(args) != 0 {
		http.Error(w, "Send an empty JSON object", http.StatusBadRequest)
		return false
	}
	switch h.nonces.claim([16]byte(nonce), stamp, time.Now().Unix()) {
	case http.StatusConflict:
		http.Error(w, "Query already processed", http.StatusConflict)
		return false
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Statistics query limit reached", http.StatusTooManyRequests)
		return false
	}
	return true
}

// Bound replay state on the public listener. When full, keep existing nonce
// reservations until their signatures expire. HTML visits use no entries.
type statusNonces struct {
	mu        sync.Mutex
	expires   map[[16]byte]int64
	nextSweep int64
}

func (s *statusNonces) claim(nonce [16]byte, stamp, now int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expires == nil {
		s.expires = make(map[[16]byte]int64)
	}
	if now >= s.nextSweep {
		for key, until := range s.expires {
			if until < now {
				delete(s.expires, key)
			}
		}
		s.nextSweep = now + 60
	}
	if until, exists := s.expires[nonce]; exists && until >= now {
		return http.StatusConflict
	}
	if len(s.expires) >= 65536 {
		return http.StatusTooManyRequests
	}
	s.expires[nonce] = stamp + requestauth.ClockSkewSeconds
	return http.StatusOK
}
