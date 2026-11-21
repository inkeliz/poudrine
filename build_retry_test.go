package poudrine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRetrySelectsFailedDependenciesAndSkippedPackages(t *testing.T) {
	failed := []buildIssue{
		{Package: "abseil-1", Origin: "devel/abseil"},
		{Package: "lib-1", Origin: "devel/lib"},
		{Package: "other-1", Origin: "devel/other"},
	}
	skipped := []buildIssue{
		{Package: "codex-1", Origin: "misc/codex", BlockedBy: "protobuf-2"},
		{Package: "protobuf-2", Origin: "devel/protobuf", BlockedBy: "abseil-1"},
		{Package: "protobuf-2", Origin: "devel/protobuf", BlockedBy: "lib-1"},
	}
	want := []string{"devel/abseil", "devel/lib", "devel/protobuf", "misc/codex"}
	for _, pkg := range []string{"abseil-1", "protobuf-2", "codex-1"} {
		if got := affectedOrigins(failed, skipped, pkg); !slices.Equal(got, want) {
			t.Errorf("retry %s: got %v", pkg, got)
		}
	}
	if got := affectedOrigins(failed, skipped, "unknown-1"); len(got) != 0 {
		t.Errorf("unknown package selected %v", got)
	}
	if got := affectedOrigins(failed, skipped, ""); len(got) != 5 || !slices.Contains(got, "devel/other") {
		t.Errorf("retry entire run: %v", got)
	}
}

func TestSupervisorValidatesRetryAndWorkerBuildsTemporaryList(t *testing.T) {
	tmp := t.TempDir()
	cfg := workerTestConfig(tmp)
	tracked := "ports-mgmt/poudriere\n"
	if err := os.WriteFile(cfg.PackagesFile, []byte(tracked), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.ConfigDir, 0755); err != nil {
		t.Fatal(err)
	}
	worker, err := NewBuilder(cfg, func(context.Context) ([]string, error) {
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
	dir := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports, started.Format(buildNameFormat))
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
	request := func(values map[string]any) configMessage {
		t.Helper()
		body, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		req := signedTestRequest(t, key, http.MethodPost, retryBuildPath, body, time.Now())
		return forwardedConfigRequest(req, body)
	}
	check := func(req configMessage, want int) configReply {
		t.Helper()
		got := authority.handle(req)
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
	} {
		check(request(bad), 400)
	}
	check(request(map[string]any{"started": started, "package": "unknown-1"}), 404)
	check(request(map[string]any{"started": started.Add(-time.Minute)}), 404)
	if worker.Status().RetryPending != 0 {
		t.Fatal("rejected retry changed the queue")
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
	schedule := buildSchedule{maintenance: time.Now().UTC().Add(2 * time.Hour), lastOrigins: tracked}
	original := schedule
	worker.checkBuild(t.Context(), &schedule)
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
		args[len(args)-1] == started.Format(buildNameFormat) || args[len(args)-1] != status.Started.Format(buildNameFormat) {
		t.Fatalf("unexpected retry command %q", args)
	}
	if _, err := os.Stat(args[f+1]); !os.IsNotExist(err) {
		t.Fatalf("retry file remains after completion: %v", err)
	}
}
