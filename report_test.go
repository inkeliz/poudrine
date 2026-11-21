package poudrine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func inventoryReportFixture(t *testing.T) (*repository, CatalogPackage) {
	t.Helper()
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pkg := CatalogPackage{Name: "py312-wheel", Version: "5", Origin: "devel/py-wheel", BuildOrigin: "devel/py-wheel@py312", Path: "All/wheel-5.pkg"}
	base := CatalogPackage{Name: "FreeBSD-acct", Origin: "base/FreeBSD-acct", BuildOrigin: "base/FreeBSD-acct", Path: "All/acct.pkg"}
	cat := &Catalog{}
	cat.current.Store(&catalogSnapshot{
		byPath:     map[string]CatalogPackage{pkg.Path: pkg, base.Path: base},
		pathByName: map[string]string{pkg.Name: pkg.Path, base.Name: base.Path},
	})
	// The fixture intentionally has no package cache or upstream connection.
	return &repository{catalog: cat, store: inventory{db}}, pkg
}

// All reports for the same source IP must use a stable key.
func testReportKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
}

func testReportProof() reportProof {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return reportProof{testReportKey().Public().(ed25519.PublicKey), nonce, time.Now().Unix()}
}

func newSignedReport(t *testing.T, key ed25519.PrivateKey, path, body string, at time.Time) *http.Request {
	t.Helper()
	r := signedTestRequest(t, key, http.MethodPost, path, []byte(body), at)
	r.Header.Set("Content-Type", "text/plain; charset=utf-8")
	return r
}

func inventoryPost(t *testing.T, repo *repository, body string, want int) inventoryReceipt {
	t.Helper()
	r := newSignedReport(t, testReportKey(), reportPath, body, time.Now())
	w := httptest.NewRecorder()
	repo.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("report returned %d, want %d: %s", w.Code, want, w.Body.String())
	}
	var receipt inventoryReceipt
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
	}
	return receipt
}

func TestInstalledPackageReport(t *testing.T) {
	repo, pkg := inventoryReportFixture(t)
	ctx := t.Context()
	// Import from an older client with a pkgbase package and a private port.
	report := "py312-wheel 1.4_1,2 devel/py-wheel\nFreeBSD-acct 15.1p4 base/FreeBSD-acct\ncustom 1 local/custom\npy312-wheel 1.4_1,2 devel/py-wheel\n"
	for range 2 {
		if got := inventoryPost(t, repo, report, 200); got != (inventoryReceipt{Reported: 3, Matched: 1, Unmatched: 2}) {
			t.Fatalf("receipt: %+v", got)
		}
	}
	origins, err := repo.store.origins(ctx)
	if err != nil || !reflect.DeepEqual(origins, []string{pkg.BuildOrigin}) {
		t.Fatalf("build origins: %v %v", origins, err)
	}
	getClient := func() clientRecord {
		t.Helper()
		clients, err := repo.store.clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows := clients.([]clientRecord)
		if len(rows) != 1 {
			t.Fatalf("expected one client, got %d", len(rows))
		}
		return rows[0]
	}
	getTarget := func() trackedPackageRecord {
		t.Helper()
		targets, err := repo.store.tracked(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows := targets.([]trackedPackageRecord)
		if len(rows) != 1 {
			t.Fatalf("expected one tracked target: %+v", rows)
		}
		return rows[0]
	}
	check := getClient()
	if check.ReportedAt == 0 || len(check.Packages) != 0 || !reflect.DeepEqual(check.ReportedPackages, []string{"FreeBSD-acct-15.1p4", "custom-1", "py312-wheel-1.4_1,2"}) {
		t.Fatalf("imported client: %+v", check)
	}
	target := getTarget()
	if target.Requested || !target.Reported || target.BuildOrigin != pkg.BuildOrigin {
		t.Fatalf("tracked origin: %+v", target)
	}

	// Conflicting duplicate or trailing invalid lines cannot replace any part
	// of the snapshot. Matching names alone cannot enroll another origin.
	inventoryPost(t, repo, "custom 1 local/custom\ncustom 2 local/custom\n", 400)
	inventoryPost(t, repo, "custom 1 local/custom\nbad-line\n", 400)
	if !reflect.DeepEqual(check, getClient()) {
		t.Fatal("rejected report changed inventory")
	}
	if got := inventoryPost(t, repo, "py312-wheel .98 local/other\n", 200); got != (inventoryReceipt{Reported: 1, Unmatched: 1}) {
		t.Fatalf("mismatched origin: %+v", got)
	}
	if p := getClient().ReportedPackages; !reflect.DeepEqual(p, []string{"py312-wheel-.98"}) {
		t.Fatalf("snapshot replacement: %v", p)
	}

	// An actual archive request remains attributable after the client removes
	// its installed packages. It is independent of the submitted version.
	if err := repo.store.record(ctx, check.IP, &pkg); err != nil {
		t.Fatal(err)
	}
	inventoryPost(t, repo, "", 200)
	check = getClient()
	if len(check.ReportedPackages) != 0 || !reflect.DeepEqual(check.Packages, []string{"py312-wheel-5"}) {
		t.Fatalf("empty report lost download history: %+v", check)
	}
	if target = getTarget(); !target.Requested || !target.Reported {
		t.Fatalf("target dropped provenance: %+v", target)
	}
}

func TestReportBoundaries(t *testing.T) {
	repo, _ := inventoryReportFixture(t)
	for _, tc := range []struct {
		method, contentType, origin string
		want                        int
	}{
		{"GET", "text/plain", "", 405}, {"POST", "application/json", "", 415}, {"POST", "text/plain", "null", 403}, {"POST", "text/plain", "http://external.example", 403},
	} {
		r := httptest.NewRequest(tc.method, "/api/inventory", nil)
		r.Header.Set("Content-Type", tc.contentType)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		repo.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s %s: %d", tc.method, tc.contentType, tc.origin, w.Code)
		}
	}
	inventoryPost(t, repo, strings.Repeat("x 1 local/x\n", maxReportLines+1), 413)
	inventoryPost(t, repo, strings.Repeat(" ", maxReportBytes+1), 413)

	// curl and other uploaders may also stream a body without Content-Length.
	line := "x 1 local/" + strings.Repeat("x", 240) + "\n"
	streamed := newSignedReport(t, testReportKey(), reportPath, strings.Repeat(line, 1+maxReportBytes/len(line)), time.Now())
	streamed.ContentLength = -1
	w := httptest.NewRecorder()
	repo.ServeHTTP(w, streamed)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("streamed oversized input returned %d", w.Code)
	}
	inventoryPost(t, repo, strings.Repeat("x", 256)+" 1 local/x\n", 400)
	inventoryPost(t, repo, "pkg 1 lib\x00\n", 400)
	repo.catalog.current.Store(nil)
	inventoryPost(t, repo, "x 1 local/x\n", 503)
	clients, err := repo.store.clients(t.Context())
	if err != nil || len(clients.([]clientRecord)) != 0 {
		t.Fatalf("rejected inputs should leave no entries: %v %v", clients, err)
	}
}

func TestReportAuthentication(t *testing.T) {
	repo, _ := inventoryReportFixture(t)
	key := testReportKey()
	const body = "p 1 devel/p\n"
	check := func(r *http.Request, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		repo.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d (%s)", r.Method, r.URL, w.Code, want, w.Body.String())
		}
	}
	for _, path := range []string{reportPath + "?", reportPath + "?ip=127.0.0.1", "/api/%69nventory"} {
		check(newSignedReport(t, key, path, body, time.Now()), 400)
	}
	for _, header := range []string{"X-Poudrine-Key", "X-Poudrine-Signature", "X-Poudrine-Time", "X-Poudrine-Nonce"} {
		req := newSignedReport(t, key, reportPath, body, time.Now())
		req.Header.Del(header)
		check(req, 401)
	}
	check(newSignedReport(t, key, reportPath, body, time.Now().Add(-6*time.Minute)), 401)
	req := newSignedReport(t, key, reportPath, body, time.Now())
	req.Body = io.NopCloser(strings.NewReader(body + "\n"))
	check(req, 401)

	req = newSignedReport(t, key, reportPath, body, time.Now())
	headers := req.Header.Clone()
	check(req, 200)
	pinMismatch := newSignedReport(t, newTestAdminKey(t), reportPath, "", time.Now())
	check(pinMismatch, 403)
	replay := newSignedReport(t, key, reportPath, body, time.Now())
	replay.Header = headers
	check(replay, 409)
	replay = newSignedReport(t, key, reportPath, body, time.Now())
	replay.Header = headers
	replay.RemoteAddr = "192.0.2.9:2345"
	check(replay, 409)
	var keys, clients, packages int
	if err := repo.store.db.QueryRow("SELECT (SELECT count(*) FROM client_report_keys), (SELECT count(*) FROM clients), (SELECT count(*) FROM client_report_packages)").Scan(&keys, &clients, &packages); err != nil || keys != 1 || clients != 1 || packages != 1 {
		t.Fatalf("rejected requests changed inventory: keys=%d clients=%d packages=%d, %v", keys, clients, packages, err)
	}
	check(newSignedReport(t, key, reportPath, "", time.Now()), 200)
}

func TestConcurrentInstalledPackageReports(t *testing.T) {
	repo, _ := inventoryReportFixture(t)
	results := make(chan int, 3)
	for _, version := range []string{"1", "2", "3"} {
		go func() {
			var body strings.Builder
			for i := range 50 {
				fmt.Fprintf(&body, "pkg%d %s local/pkg%d\n", i, version, i)
			}
			req := newSignedReport(t, testReportKey(), reportPath, body.String(), time.Now())
			w := httptest.NewRecorder()
			repo.ServeHTTP(w, req)
			results <- w.Code
		}()
	}
	for range 3 {
		if code := <-results; code != 200 {
			t.Errorf("concurrent report: %d", code)
		}
	}
	var count, versions int
	if err := repo.store.db.QueryRow("SELECT count(*),count(DISTINCT version) FROM client_report_packages").Scan(&count, &versions); err != nil || count != 50 || versions != 1 {
		t.Fatalf("partial snapshot: %d %d %v", count, versions, err)
	}
}
