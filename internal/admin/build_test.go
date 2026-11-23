package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestAdminBuildControls(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	b, err := builder.NewBuilder(testutil.WorkerConfig(t.TempDir()), func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, supervisor := supervisedTestAdmin(t, a.cfg, key, b)
	for _, endpoint := range []struct{ method, path string }{
		{routes.BuildStatus.Method, routes.BuildStatus.Path}, {"POST", routes.ForceBuild.Path}, {routes.BuildHistory.Method, routes.BuildHistory.Path},
	} {
		adminResponse(t, a, httptest.NewRequest(endpoint.method, endpoint.path, nil), 401)
	}
	body := []byte("{}")
	// Even over the private descriptor, only the pinned administrator can
	// request work. The service validates the original body independently.
	other := testutil.SignedRequest(t, testutil.AdminKey(t), "POST", routes.ForceBuild.Path, body, time.Now())
	if reply := supervisor.Handle(t.Context(), broker.ForwardedConfigRequest(other, body)); reply.Status != 401 {
		t.Fatalf("unexpected supervisor reply for unknown signer: %+v", reply)
	}
	signed := testutil.SignedRequest(t, key, "POST", routes.ForceBuild.Path, body, time.Now())
	if reply := supervisor.Handle(t.Context(), broker.ForwardedConfigRequest(signed, []byte("{ }"))); reply.Status != 401 {
		t.Fatalf("unexpected supervisor reply for changed request body: %+v", reply)
	}
	for _, body := range []string{`{"jobs":2}`, `{"jobs":null}`, `null`, `[]`, `false`, " \n\t", `{} {}`, `{"jobs":1,"jobs":2}`} {
		adminResponse(t, a, testutil.SignedRequest(t, key, "POST", routes.ForceBuild.Path, []byte(body), time.Now()), 400)
	}
	if b.Status().ManualPending {
		t.Fatal("invalid request queued a run")
	}

	for i, text := range []string{"{}", "", " \n{}\t"} {
		already := i != 0
		body := []byte(text)
		req := testutil.SignedRequest(t, key, "POST", routes.ForceBuild.Path, body, time.Now())
		original := broker.ForwardedConfigRequest(req, body)
		w := adminResponse(t, a, req, 202)
		var ack map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		if duplicate, ok := ack["already_queued"]; !ok || !ack["queued"] || duplicate != already {
			t.Fatalf("build acknowledgment: %s", w.Body)
		}
		if reply := supervisor.Handle(t.Context(), original); reply.Status != 409 {
			t.Fatalf("repeated supervisor request: %+v", reply)
		}
	}
	w := adminResponse(t, a, testutil.SignedRequest(t, key, routes.BuildStatus.Method, routes.BuildStatus.Path, []byte("{}"), time.Now()), 200)
	var view buildView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || !view.CanForce || !view.ManualPending || view.NextReason != "manual" {
		t.Fatalf("supervised build status: %s", w.Body)
	}
}

func TestStandaloneBuildViewAndHistory(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	w := adminResponse(t, a, testutil.SignedRequest(t, key, routes.BuildStatus.Method, routes.BuildStatus.Path, []byte("{}"), time.Now()), 200)
	var view buildView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.CanForce || view.Stage != "waiting" {
		t.Fatalf("standalone build status: %s", w.Body)
	}
	adminResponse(t, a, testutil.SignedRequest(t, key, "POST", routes.ForceBuild.Path, []byte("{}"), time.Now()), 503)

	now := time.Now().UTC()
	if _, err := a.cfg.DB.Exec(database.BuildHistorySchema); err != nil {
		t.Fatal(err)
	}
	if err := a.cfg.DB.RecordBuilds(t.Context(), []database.BuildRecord{{Started: now.Add(-time.Minute), Package: "nginx-1", Finished: now, Outcome: "failed"}}); err != nil {
		t.Fatal(err)
	}
	history := routes.BuildHistory.Path
	adminResponse(t, a, testutil.SignedRequest(t, key, broker.MethodQuery, history, []byte(`{"interval":"unknown"}`), now), 400)
	w = adminResponse(t, a, testutil.SignedRequest(t, key, broker.MethodQuery, history, []byte(`{"interval":"week"}`), now), 200)
	var result database.BuildHistory
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Interval != "week" || len(result.Buckets) != 12 || result.Totals.Failed != 1 {
		t.Fatalf("weekly build history: %s", w.Body)
	}
	selected := slices.IndexFunc(result.Buckets, func(b database.BuildBucket) bool { return b.Failed > 0 })
	if selected < 0 {
		t.Fatal("weekly history is missing its failure bucket")
	}
	query := database.BuildPeriodQuery{Interval: result.Interval, Start: result.Buckets[selected].Start, AsOf: result.AsOf}
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	route := routes.HistoryPackages
	adminResponse(t, a, testutil.SignedRequest(t, key, route.Method, route.Path, []byte(`{"interval":"week"}`), now), 400)
	w = adminResponse(t, a, testutil.SignedRequest(t, key, route.Method, route.Path, body, now), 200)
	var page database.BuildPeriodPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Packages) != 1 || page.Packages[0].Package != "nginx-1" || page.Packages[0].Outcome != "failed" {
		t.Fatalf("period package results: %s", w.Body)
	}
}
