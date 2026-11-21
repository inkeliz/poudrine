package poudrine

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminBuildControls(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	b, err := NewBuilder(workerTestConfig(t.TempDir()), func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, supervisor := supervisedTestAdmin(t, a.cfg, key, b)
	for _, endpoint := range []struct{ method, path string }{
		{methodQuery, buildStatusPath}, {"POST", forceBuildPath}, {methodQuery, buildStatusPath + "/history"},
	} {
		adminResponse(t, a, httptest.NewRequest(endpoint.method, endpoint.path, nil), 401)
	}
	body := []byte("{}")
	// Even over the private descriptor, only the pinned administrator can
	// request work. The service validates the original body independently.
	other := signedTestRequest(t, newTestAdminKey(t), "POST", forceBuildPath, body, time.Now())
	if reply := supervisor.handle(forwardedConfigRequest(other, body)); reply.Status != 401 {
		t.Fatalf("unexpected supervisor reply for unknown signer: %+v", reply)
	}
	signed := signedTestRequest(t, key, "POST", forceBuildPath, body, time.Now())
	if reply := supervisor.handle(forwardedConfigRequest(signed, []byte("{ }"))); reply.Status != 401 {
		t.Fatalf("unexpected supervisor reply for changed request body: %+v", reply)
	}
	adminResponse(t, a, signedTestRequest(t, key, "POST", forceBuildPath, []byte(`{"jobs":2}`), time.Now()), 400)
	if b.Status().ManualPending {
		t.Fatal("invalid request queued a run")
	}

	for _, already := range []bool{false, true} {
		req := signedTestRequest(t, key, "POST", forceBuildPath, body, time.Now())
		original := forwardedConfigRequest(req, body)
		w := adminResponse(t, a, req, 202)
		var ack map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		if duplicate, ok := ack["already_queued"]; !ok || !ack["queued"] || duplicate != already {
			t.Fatalf("build acknowledgment: %s", w.Body)
		}
		if reply := supervisor.handle(original); reply.Status != 409 {
			t.Fatalf("repeated supervisor request: %+v", reply)
		}
	}
	w := adminResponse(t, a, signedTestRequest(t, key, methodQuery, buildStatusPath, []byte("{}"), time.Now()), 200)
	var view buildView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || !view.CanForce || !view.ManualPending || view.NextReason != "manual" {
		t.Fatalf("supervised build status: %s", w.Body)
	}
}

func TestStandaloneBuildViewAndHistory(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	w := adminResponse(t, a, signedTestRequest(t, key, methodQuery, buildStatusPath, []byte("{}"), time.Now()), 200)
	var view buildView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.CanForce || view.Stage != "waiting" {
		t.Fatalf("standalone build status: %s", w.Body)
	}
	adminResponse(t, a, signedTestRequest(t, key, "POST", forceBuildPath, []byte("{}"), time.Now()), 503)

	now := time.Now().UTC()
	if _, err := a.cfg.DB.Exec(buildHistorySchema); err != nil {
		t.Fatal(err)
	}
	if err := (inventory{a.cfg.DB}).recordBuilds(t.Context(), []BuildRecord{{Started: now.Add(-time.Minute), Package: "nginx-1", Finished: now, Outcome: "failed"}}); err != nil {
		t.Fatal(err)
	}
	history := buildStatusPath + "/history"
	adminResponse(t, a, signedTestRequest(t, key, methodQuery, history, []byte(`{"interval":"unknown"}`), now), 400)
	w = adminResponse(t, a, signedTestRequest(t, key, methodQuery, history, []byte(`{"interval":"week"}`), now), 200)
	var result BuildHistory
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Interval != "week" || len(result.Buckets) != 12 || result.Totals.Failed != 1 {
		t.Fatalf("weekly build history: %s", w.Body)
	}
}
