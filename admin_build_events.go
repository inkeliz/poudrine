package poudrine

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Each connection starts with the current snapshot, so reconnecting clients
// can recover from missed updates without replaying an event log.
func (a *Admin) buildEvents(w http.ResponseWriter, r *http.Request, body []byte, signer ed25519.PublicKey) {
	if !queryArgs(w, body, &struct{}{}) {
		return
	}
	view, status, err := a.buildSnapshot(r, body)
	if err != nil {
		writeBuildError(w, status, err)
		return
	}
	data, err := json.Marshal(view)
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeBuildEvent(w, data); err != nil {
		return
	}
	previous := data
	lastWrite := time.Now()
	// The worker samples Poudriere's per-job files every two seconds.
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		// The broker checks the pinned key and signed timestamp on each read.
		// Close standalone streams when the enrolled identity changes.
		if a.cfg.broker == nil {
			key, err := a.enrolledKey(r.Context())
			if err != nil || !signer.Equal(key) {
				return
			}
		}
		view, _, err = a.buildSnapshot(r, body)
		if err != nil {
			return // The browser reconnects with a fresh signed request.
		}
		data, err = json.Marshal(view)
		if err != nil {
			return
		}
		if bytes.Equal(previous, data) {
			if time.Since(lastWrite) < 15*time.Second {
				continue
			}
			data = nil // Keep idle streams and reverse proxies connected.
		} else {
			previous = data
		}
		if err := writeBuildEvent(w, data); err != nil {
			return
		}
		lastWrite = time.Now()
	}
}

func writeBuildEvent(w http.ResponseWriter, data []byte) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err == nil {
		defer controller.SetWriteDeadline(time.Time{})
	} else if !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	var err error
	if data == nil {
		_, err = io.WriteString(w, ": keepalive\n\n")
	} else {
		_, err = fmt.Fprintf(w, "event: build\ndata: %s\n\n", data)
	}
	if err != nil {
		return err
	}
	return controller.Flush()
}
