package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestSupervisorValidatesRetryAndWorkerBuildsTemporaryList(t *testing.T) {
	tmp := t.TempDir()
	cfg := testutil.WorkerConfig(tmp)
	tracked := "ports-mgmt/poudriere\n"
	if err := os.WriteFile(cfg.PackagesFile, []byte(tracked), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.ConfigDir, 0755); err != nil {
		t.Fatal(err)
	}
	worker, err := builder.NewBuilder(cfg, func(context.Context) ([]string, error) {
		t.Fatal("retry unexpectedly read normal tracking inventory")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	_, authority := supervisedTestAdmin(t, a.cfg, key, worker)
	started := time.Now().UTC().Add(-time.Hour)
	dir := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports, started.Format(builder.BuildNameFormat))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{
		"failed":  "devel/abseil abseil-1 fetch network 2\nmisc/foo foo-1 build compile 5\n",
		"skipped": "misc/codex codex-1 abseil-1\ndevel/protobuf protobuf-2 abseil-1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, ".poudriere.ports."+file), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	request := func(values map[string]any) broker.Message {
		t.Helper()
		body, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		req := testutil.SignedRequest(t, key, routes.RetryBuild.Method, routes.RetryBuild.Path, body, time.Now())
		return broker.ForwardedConfigRequest(req, body)
	}
	check := func(req broker.Message, want int) broker.Reply {
		t.Helper()
		got := authority.Handle(t.Context(), req)
		if got.Status != want {
			t.Fatalf("retry %s: %d, want %d (%s)", req.Body, got.Status, want, got.Error)
		}
		return got
	}
	req := request(map[string]any{"started": started, "package": "abseil-1"})
	altered := req
	altered.Body = append(slices.Clone(req.Body), ' ')
	check(altered, 401)
	for _, bad := range []map[string]any{
		{"started": started, "package": nil}, {"started": started, "package": ""},
		{"started": started, "origin": "misc/codex"}, {"package": "codex-1"},
		{"started": started, "outcome": nil}, {"started": started, "outcome": ""},
		{"started": started, "outcome": "succeeded"}, {"started": started, "outcome": []string{"failed"}},
		{"started": started, "package": "abseil-1", "outcome": "failed"},
	} {
		check(request(bad), 400)
	}
	changedOutcome := request(map[string]any{"started": started, "outcome": "failed"})
	changedOutcome.Body = request(map[string]any{"started": started, "outcome": "skipped"}).Body
	check(changedOutcome, 401)
	check(request(map[string]any{"started": started, "package": "unknown-1"}), 404)
	check(request(map[string]any{"started": started.Add(-time.Minute)}), 404)
	clean := started.Add(time.Minute)
	testutil.WriteFile(t, filepath.Join(filepath.Dir(dir), clean.Format(builder.BuildNameFormat), ".poudriere.ports.built"), []byte("ftp/curl curl-1 5\n"))
	for _, outcome := range []string{"failed", "skipped"} {
		check(request(map[string]any{"started": clean, "outcome": outcome}), 409)
	}
	if worker.Status().RetryPending != 0 {
		t.Fatal("rejected retry changed the queue")
	}
	emptyQueue := worker.Checkpoint()
	for _, group := range []struct {
		outcome string
		want    []string
	}{
		{"failed", []string{"devel/abseil", "misc/foo"}},
		{"skipped", []string{"devel/protobuf", "misc/codex"}},
	} {
		args := map[string]any{"started": started, "outcome": group.outcome}
		signed := request(args)
		if check(signed, 202).AlreadyQueued || !slices.Equal(worker.Checkpoint().RetryOrigins, group.want) {
			t.Fatalf("retry %s queued %v, want %v", group.outcome, worker.Checkpoint().RetryOrigins, group.want)
		}
		check(signed, 409)
		if !check(request(args), 202).AlreadyQueued {
			t.Fatalf("repeated %s selection added duplicate work", group.outcome)
		}
		worker.RestoreCheckpoint(emptyQueue)
	}
	if check(req, 202).AlreadyQueued {
		t.Fatal("first request marked already queued")
	}
	check(req, 409)
	if !check(request(map[string]any{"started": started, "package": "codex-1"}), 202).AlreadyQueued {
		t.Fatal("the failed port and its skipped dependent should share one retry")
	}
	if check(request(map[string]any{"started": started}), 202).AlreadyQueued {
		t.Fatal("retry-all should merge the previously unselected failure")
	}
	if status := worker.Status(); status.ManualPending || status.RetryPending != 4 || status.NextReason != "retry" {
		t.Fatalf("unexpected retry queue state: %+v", status)
	}
	if !worker.RequestBuild() || worker.RequestBuild() {
		t.Fatal("full build must keep its own coalesced queue slot")
	}
	const fixture = `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$RETRY_TEST/args"
while [ "$1" != "-f" ]; do shift; done
cp "$2" "$RETRY_TEST/used"
`
	if err := os.WriteFile(filepath.Join(tmp, "poudriere"), []byte(fixture), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))
	t.Setenv("RETRY_TEST", tmp)
	schedule := builder.BuildSchedule{Maintenance: time.Now().UTC().Add(2 * time.Hour), LastOrigins: tracked}
	original := schedule
	worker.CheckBuild(t.Context(), &schedule)
	if schedule != original {
		t.Fatal("targeted retry changed regular maintenance")
	}
	status := worker.Status()
	if !status.ManualPending || status.Active || status.Outcome != "succeeded" || status.Trigger != "retry" || status.Origins == nil || *status.Origins != 4 {
		t.Fatalf("retry status %+v", status)
	}
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if read(cfg.PackagesFile) != tracked || read(filepath.Join(tmp, "used")) != "devel/abseil\ndevel/protobuf\nmisc/codex\nmisc/foo\n" {
		t.Fatal("incorrect tracked or retry list")
	}
	args := strings.Split(strings.TrimSpace(read(filepath.Join(tmp, "args"))), "\n")
	f := slices.Index(args, "-f")
	if f < 0 || f+1 >= len(args) || args[f+1] == cfg.PackagesFile || slices.Contains(args, "-b") || slices.Contains(args, "-C") ||
		args[len(args)-1] == started.Format(builder.BuildNameFormat) || args[len(args)-1] != status.Started.Format(builder.BuildNameFormat) {
		t.Fatalf("unexpected retry command %q", args)
	}
	if _, err := os.Stat(args[f+1]); !os.IsNotExist(err) {
		t.Fatalf("retry file remains after completion: %v", err)
	}
}
