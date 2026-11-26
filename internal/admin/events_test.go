package admin

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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestLiveBuildEvents(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			admin := testAdmin(t)
			const configured, lastSlot = 800, 901
			admin.cfg.Builder.Jobs = configured
			key := enrollAdmin(t, admin)
			started := time.Now().UTC()
			initial := builder.BuildStatus{Stage: "building", Active: true, Started: started, Log: []string{"configure\ncompile"},
				RebuildReasons: []string{"Deleting gettext-tools-1.0_1.pkg: changed options"}}
			// A real large queue must pass the broker's frame limit without
			// losing jobs or mixing a control response into the event stream.
			for i := range 750 {
				initial.Jobs = append(initial.Jobs, builder.BuildJob{ID: fmt.Sprint(i + 1), Package: strings.Repeat("a", 120), Origin: "devel/" + strings.Repeat("b", 120), Phase: "build"})
			}
			// A legacy standalone status lacks capacity; retain every observed slot.
			initial.Jobs[749].ID = fmt.Sprint(lastSlot)
			initial.Jobs[0].ElapsedSeconds = new(int64(0))
			initial.Jobs[0].Package, initial.Jobs[0].Origin, initial.Jobs[0].ABI = "curl-9", "ftp/curl", "freebsd:15:amd64"
			initial.Jobs[0].ActiveElapsedSeconds = new(int64(0))
			initial.Jobs[0].CPUPercent = new(0.0)
			initial.Jobs[749].ElapsedSeconds = new(int64(425))
			initial.Jobs[749].CPUPercent = new(287.4)
			initial.Jobs[749].MemoryBytes = new(int64(1 << 30))
			initial.Jobs[749].ProgressPercent = new(63)
			prior := database.BuildRecord{Started: started.Add(-time.Hour), Finished: started.Add(-time.Minute),
				Package: "curl-8", Origin: "ftp/curl", ABI: "freebsd:15:amd64", Outcome: "succeeded", BuildSeconds: new(int64(90))}
			if err := admin.cfg.DB.RecordBuilds(t.Context(), []database.BuildRecord{prior}); err != nil {
				t.Fatal(err)
			}
			wantJobs := slices.Clone(initial.Jobs)
			wantJobs[0].LastBuildSeconds = prior.BuildSeconds
			var saved atomic.Value
			saved.Store(initial)
			admin.cfg.Build = func() (builder.BuildStatus, error) { return saved.Load().(builder.BuildStatus), nil }
			update := func(s builder.BuildStatus) { saved.Store(s) }
			revoke := func() error {
				_, err := admin.cfg.DB.Exec("DELETE FROM admin_identity")
				return err
			}
			if mode == "supervised" {
				worker := &builder.Builder{Cfg: admin.cfg.Builder}
				initial.BuilderCapacity = lastSlot
				worker.RestoreCheckpoint(&builder.WorkerCheckpoint{Status: initial})
				var authority *broker.Authority
				admin, authority = supervisedTestAdmin(t, admin.cfg, key, worker)
				update = func(s builder.BuildStatus) {
					worker.RestoreCheckpoint(&builder.WorkerCheckpoint{Status: s})
				}
				revoke = func() error {
					return os.WriteFile(authority.Path+config.AdminKeySuffix, testutil.AdminKey(t).Public().(ed25519.PublicKey), 0600)
				}
			}
			adminResponse(t, admin, httptest.NewRequest(routes.BuildEvents.Method, routes.BuildEvents.Path, nil), http.StatusUnauthorized)
			snapshot := adminResponse(t, admin, testutil.SignedRequest(t, key, routes.BuildStatus.Method, routes.BuildStatus.Path, []byte("{}"), time.Now()), http.StatusOK)
			var status buildView
			if err := json.Unmarshal(snapshot.Body.Bytes(), &status); err != nil || status.BuilderCapacity != lastSlot || !reflect.DeepEqual(status.Jobs, wantJobs) {
				t.Fatalf("signed build status with package timing: %v", err)
			}
			server := httptest.NewServer(admin.Handler())
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, broker.MethodQuery, server.URL+routes.BuildEvents.Path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header = testutil.SignedRequest(t, key, routes.BuildEvents.Method, routes.BuildEvents.Path, []byte("{}"), time.Now()).Header
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
			if view.BuilderCapacity != lastSlot || !reflect.DeepEqual(view.Jobs, wantJobs) || view.CanForce != (mode == "supervised") || !reflect.DeepEqual(view.Log, initial.Log) ||
				!slices.Equal(view.RebuildReasons, initial.RebuildReasons) {
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
			update(builder.BuildStatus{Stage: "idle"})
			if view := next(); view.Stage != "idle" || len(view.Jobs) != 0 || view.BuilderCapacity != configured {
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
