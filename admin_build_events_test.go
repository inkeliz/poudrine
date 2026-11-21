package poudrine

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveBuildEvents(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			admin := testAdmin(t)
			key := enrollAdmin(t, admin)
			initial := BuildStatus{Stage: "building", Active: true, Log: []string{"configure\ncompile"}}
			// A real large queue must pass the broker's frame limit without
			// losing jobs or mixing a control response into the event stream.
			for i := range 750 {
				initial.Jobs = append(initial.Jobs, BuildJob{ID: fmt.Sprint(i), Package: strings.Repeat("a", 120), Origin: "devel/" + strings.Repeat("b", 120), Phase: "build"})
			}
			initial.Jobs[0].ElapsedSeconds = new(int64(0))
			initial.Jobs[0].CPUPercent = new(0.0)
			initial.Jobs[749].ElapsedSeconds = new(int64(425))
			initial.Jobs[749].CPUPercent = new(287.4)
			initial.Jobs[749].MemoryBytes = new(int64(1 << 30))
			initial.Jobs[749].ProgressPercent = new(63)
			var saved atomic.Value
			saved.Store(initial)
			admin.cfg.Build = func() (BuildStatus, error) { return saved.Load().(BuildStatus), nil }
			update := func(s BuildStatus) { saved.Store(s) }
			revoke := func() error {
				_, err := admin.cfg.DB.Exec("DELETE FROM admin_identity")
				return err
			}
			if mode == "supervised" {
				worker := &Builder{status: initial}
				var authority *configAuthority
				admin, authority = supervisedTestAdmin(t, admin.cfg, key, worker)
				update = func(s BuildStatus) {
					worker.mu.Lock()
					worker.status = s
					worker.mu.Unlock()
				}
				revoke = func() error {
					return os.WriteFile(authority.path+".admin.pub", newTestAdminKey(t).Public().(ed25519.PublicKey), 0600)
				}
			}
			adminResponse(t, admin, httptest.NewRequest(methodQuery, buildEventsPath, nil), http.StatusUnauthorized)
			server := httptest.NewServer(admin.Handler())
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, methodQuery, server.URL+buildEventsPath, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header = signedTestRequest(t, key, methodQuery, buildEventsPath, []byte("{}"), time.Now()).Header
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
				t.Fatalf("stream response %s %v", resp.Status, resp.Header)
			}
			reader := bufio.NewReader(resp.Body)
			next := func() buildView {
				t.Helper()
				if line, err := reader.ReadString('\n'); err != nil || line != "event: build\n" {
					t.Fatalf("event header %q, %v", line, err)
				}
				line, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "data: ") {
					t.Fatalf("reading build event: %v", err)
				}
				var view buildView
				if err := json.Unmarshal([]byte(line[len("data: "):]), &view); err != nil {
					t.Fatal(err)
				}
				if line, err := reader.ReadString('\n'); err != nil || line != "\n" {
					t.Fatalf("event terminator %q, %v", line, err)
				}
				return view
			}
			view := next()
			if !reflect.DeepEqual(view.Jobs, initial.Jobs) || view.CanForce != (mode == "supervised") || !reflect.DeepEqual(view.Log, initial.Log) {
				t.Fatalf("incomplete initial event: %d jobs, can_force=%t", len(view.Jobs), view.CanForce)
			}
			duplicate := req.Clone(ctx)
			duplicate.Body = io.NopCloser(strings.NewReader("{}"))
			replay, err := server.Client().Do(duplicate)
			if err != nil {
				t.Fatal(err)
			}
			replay.Body.Close()
			if replay.StatusCode != http.StatusConflict {
				t.Fatalf("replayed stream request returned %s", replay.Status)
			}
			update(BuildStatus{Stage: "idle"})
			if view := next(); view.Stage != "idle" || len(view.Jobs) != 0 {
				t.Fatal("job completion failed to clear active builders")
			}
			if err := revoke(); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("stream remained open after key revocation: %v", err)
			}
		})
	}
}
