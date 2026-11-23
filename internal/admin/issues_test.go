package admin

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestPackageFailureManagement(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	started := time.Now().UTC().Add(-time.Hour)
	run := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(builder.BuildNameFormat))
	if err := os.MkdirAll(filepath.Join(run, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	const pkg = "abseil-20250127.1_3"
	const output = "=> distfile not found\nfetch: the mirror closed the connection\n"
	for name, content := range map[string]string{
		".poudriere.ports.failed":  "devel/abseil " + pkg + " fetch fetch_error 2\n",
		".poudriere.ports.skipped": "misc/codex codex-0.170.0 " + pkg + "\ndevel/protobuf protobuf-29.6_1,1 " + pkg + "\n",
		"logs/" + pkg + ".log":     output,
	} {
		if err := os.WriteFile(filepath.Join(run, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	a := testAdmin(t)
	a.cfg.Builder = cfg
	key := enrollAdmin(t, a)
	query := func(path string, args any, want int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return adminResponse(t, a, testutil.SignedRequest(t, key, broker.MethodQuery, path, body, time.Now()), want)
	}
	var view builder.BuildIssuesView
	if err := json.Unmarshal(query(routes.BuildIssues.Path, struct{}{}, 200).Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Started.Equal(started) || len(view.Failed) != 1 || view.Failed[0].Package != pkg || view.Failed[0].Phase != "fetch" || len(view.Skipped) != 2 {
		t.Fatalf("unexpected run diagnostics: %+v", view)
	}
	for _, skipped := range view.Skipped {
		if skipped.BlockedBy != pkg {
			t.Fatalf("missing failed dependency for %+v", skipped)
		}
	}
	args := map[string]any{"started": started, "package": pkg}
	var page builder.BuildLogPage
	if err := json.Unmarshal(query(routes.BuildLog.Path, args, 200).Body.Bytes(), &page); err != nil || page.Text != output {
		t.Fatalf("log response %+v (%v)", page, err)
	}
	query(routes.BuildLog.Path, map[string]any{"started": started, "package": "codex-0.170.0"}, 404)
	query(routes.BuildLog.Path, map[string]any{"started": started, "package": "../repository.key"}, 400)
	query(routes.BuildLog.Path, map[string]any{"package": pkg}, 400)

	// The default history selection follows the newest run even when all
	// packages succeeded, or the run has not recorded any packages yet.
	// An old failure remains available only when that run is selected.
	clean := started.Add(time.Minute)
	cleanDir := filepath.Join(filepath.Dir(run), clean.Format(builder.BuildNameFormat))
	testutil.WriteFile(t, filepath.Join(cleanDir, ".poudriere.ports.built"), []byte("ftp/curl curl-1 17\nftp/curl curl-1 17\nwww/old old-2\nwww/writing writing-3 4"))
	testutil.WriteFile(t, filepath.Join(cleanDir, "logs", "curl-1.log"), []byte("successful build\n"))
	readResults := func(args any) builder.BuildResultsView {
		t.Helper()
		var result builder.BuildResultsView
		if err := json.Unmarshal(query(routes.BuildResults.Path, args, 200).Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	latest := readResults(struct{}{})
	if !latest.Started.Equal(clean) || len(latest.Runs) != 2 || !latest.Runs[0].Equal(clean) || !latest.Runs[1].Equal(started) ||
		latest.Compiled[clean] != 2 || latest.Compiled[started] != 0 ||
		len(latest.Succeeded) != 2 || len(latest.Failed) != 0 || len(latest.Skipped) != 0 {
		t.Fatalf("latest run results: %+v", latest)
	}
	if built := latest.Succeeded[0]; built.Origin != "ftp/curl" || built.Package != "curl-1" || built.BuildSeconds == nil || *built.BuildSeconds != 17 || latest.Succeeded[1].BuildSeconds != nil {
		t.Fatalf("built packages: %+v", latest.Succeeded)
	}
	if err := json.Unmarshal(query(routes.BuildLog.Path, map[string]any{"started": clean, "package": "curl-1"}, 200).Body.Bytes(), &page); err != nil || page.Text != "successful build\n" {
		t.Fatalf("successful package log: %+v, %v", page, err)
	}
	preparing := clean.Add(time.Minute)
	if err := os.Mkdir(filepath.Join(filepath.Dir(run), preparing.Format(builder.BuildNameFormat)), 0755); err != nil {
		t.Fatal(err)
	}
	filtered := readResults(map[string]any{"hide_empty": true})
	if !filtered.Started.Equal(clean) || len(filtered.Runs) != 2 || !filtered.Runs[1].Equal(started) ||
		filtered.Compiled[clean] != 2 || len(filtered.Compiled) != 2 {
		t.Fatalf("hide empty lost a run with failures or included the unfinished run: %+v", filtered)
	}
	filtered = readResults(map[string]any{"started": started, "hide_empty": true})
	if !filtered.Started.Equal(started) || filtered.Compiled[clean] != 2 || len(filtered.Failed) != 1 {
		t.Fatalf("failed run or legacy unique compiled count unavailable: %+v", filtered)
	}
	testutil.WriteFile(t, filepath.Join(run, ".poudriere.ports.built"), []byte("ftp/curl curl-0 9\n"))
	old := readResults(map[string]any{"started": started})
	if !old.Started.Equal(started) || len(old.Succeeded) != 1 || old.Succeeded[0].Package != "curl-0" || len(old.Failed) != 1 || old.Failed[0].Package != pkg || len(old.Skipped) != 2 {
		t.Fatalf("selected historical run lost mixed results: %+v", old)
	}
	idle := readResults(struct{}{})
	if !idle.Started.Equal(preparing) || len(idle.Runs) != 3 || idle.Succeeded == nil || idle.Failed == nil || idle.Skipped == nil ||
		len(idle.Succeeded)+len(idle.Failed)+len(idle.Skipped) != 0 {
		t.Fatalf("run with no completed work: %+v", idle)
	}
	query(routes.BuildResults.Path, map[string]any{"started": started.Add(-time.Hour)}, 404)
	testutil.WriteFile(t, filepath.Join(cleanDir, ".poudriere.stats_built"), []byte("27\n"))
	if fast := readResults(struct{}{}); fast.Compiled[clean] != 27 {
		t.Fatalf("retained run lost the native compiled counter: %v", fast.Compiled)
	}
	// A complete skipped result belongs in the picker even with no successes
	// or failures. A writer's partial final record does not make a run visible.
	skipped := preparing.Add(time.Minute)
	skippedList := filepath.Join(filepath.Dir(run), skipped.Format(builder.BuildNameFormat), ".poudriere.ports.skipped")
	record := []byte("devel/tool tool-1 dep-1")
	testutil.WriteFile(t, skippedList, record)
	if partial := readResults(map[string]any{"hide_empty": true}); partial.Started.Equal(skipped) {
		t.Fatal("partial skipped record made its run visible")
	}
	testutil.WriteFile(t, skippedList, append(record, '\n'))
	onlySkipped := readResults(map[string]any{"hide_empty": true})
	if !onlySkipped.Started.Equal(skipped) || len(onlySkipped.Skipped) != 1 || onlySkipped.Compiled[skipped] != 0 {
		t.Fatalf("skipped-only run unavailable: %+v", onlySkipped)
	}
	query(routes.BuildResults.Path, map[string]any{"started": "../repository.key"}, 400)
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, a, testutil.SignedRequest(t, key, routes.RetryBuild.Method, routes.RetryBuild.Path, body, time.Now()), 503)
	worker := &builder.Builder{Cfg: cfg}
	a, _ = supervisedTestAdmin(t, a.cfg, key, worker)
	adminResponse(t, a, httptest.NewRequest(routes.RetryBuild.Method, routes.RetryBuild.Path, strings.NewReader(string(body))), 401)
	modified := testutil.SignedRequest(t, key, routes.RetryBuild.Method, routes.RetryBuild.Path, body, time.Now())
	modified.Body = io.NopCloser(strings.NewReader("{}"))
	adminResponse(t, a, modified, 401)
	if s := worker.Status(); s.RetryPending != 0 || s.ManualPending {
		t.Fatal("unsigned or modified request queued a retry")
	}
	for _, already := range []bool{false, true} {
		req := testutil.SignedRequest(t, key, routes.RetryBuild.Method, routes.RetryBuild.Path, body, time.Now())
		var ack struct {
			Queued        bool `json:"queued"`
			AlreadyQueued bool `json:"already_queued"`
		}
		if err := json.Unmarshal(adminResponse(t, a, req, 202).Body.Bytes(), &ack); err != nil || !ack.Queued || ack.AlreadyQueued != already {
			t.Fatalf("unexpected retry acknowledgement: %+v (%v)", ack, err)
		}
	}
	if s := worker.Status(); s.RetryPending != 3 || s.ManualPending {
		t.Fatalf("expected only the three affected origins to be queued: %+v", s)
	}
}
