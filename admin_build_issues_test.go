package poudrine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageFailureManagement(t *testing.T) {
	cfg := workerTestConfig(t.TempDir())
	started := time.Now().UTC().Add(-time.Hour)
	run := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(buildNameFormat))
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
	a.cfg.builder = cfg
	key := enrollAdmin(t, a)
	query := func(path string, args any, want int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return adminResponse(t, a, signedTestRequest(t, key, methodQuery, path, body, time.Now()), want)
	}
	const issuesPath = "/api/admin/build/issues"
	const logPath = "/api/admin/build/log"
	const retryPath = "/api/admin/build/retry"
	var view buildIssuesView
	if err := json.Unmarshal(query(issuesPath, struct{}{}, 200).Body.Bytes(), &view); err != nil {
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
	var page buildLogPage
	if err := json.Unmarshal(query(logPath, args, 200).Body.Bytes(), &page); err != nil || page.Text != output {
		t.Fatalf("log response %+v (%v)", page, err)
	}
	query(logPath, map[string]any{"started": started, "package": "codex-0.170.0"}, 404)
	query(logPath, map[string]any{"started": started, "package": "../repository.key"}, 400)
	query(logPath, map[string]any{"package": pkg}, 400)
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, a, signedTestRequest(t, key, http.MethodPost, retryPath, body, time.Now()), 503)
	worker := &Builder{cfg: cfg}
	a, _ = supervisedTestAdmin(t, a.cfg, key, worker)
	adminResponse(t, a, httptest.NewRequest(http.MethodPost, retryPath, strings.NewReader(string(body))), 401)
	modified := signedTestRequest(t, key, http.MethodPost, retryPath, body, time.Now())
	modified.Body = io.NopCloser(strings.NewReader("{}"))
	adminResponse(t, a, modified, 401)
	if s := worker.Status(); s.RetryPending != 0 || s.ManualPending {
		t.Fatal("unsigned or modified request queued a retry")
	}
	for _, already := range []bool{false, true} {
		req := signedTestRequest(t, key, http.MethodPost, retryPath, body, time.Now())
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
