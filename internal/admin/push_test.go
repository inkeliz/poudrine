package admin

import (
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

func TestPushBuildSelection(t *testing.T) {
	const name = "py312-thing"
	upstream := packageinfo.Package{Name: name, Origin: "devel/thing", BuildOrigin: "devel/thing@py312", ABI: "freebsd:15:amd64", Version: "2.4_1,2"}
	snapshot := &catalog.Snapshot{Displaced: map[string]packageinfo.Package{name: upstream}, Compiled: map[string]packageinfo.Package{}}
	ahead := database.PushBuild{Pkg: name + "-2.4_2,2", Origin: upstream.BuildOrigin, ABI: upstream.ABI, Started: 123}
	result, wait := buildAlert(ahead, snapshot)
	if wait || result == nil || !strings.Contains(result.Body, ahead.Pkg) || !strings.Contains(result.Body, upstream.Version) {
		t.Fatalf("ahead build %+v, wait=%t", result, wait)
	}
	again, _ := buildAlert(ahead, snapshot)
	secondRun := ahead
	secondRun.Started++
	second, _ := buildAlert(secondRun, snapshot)
	if again.Tag != result.Tag || second.Tag == result.Tag {
		t.Fatal("unstable or shared notification tag")
	}
	for _, edit := range []func(*database.PushBuild){
		func(b *database.PushBuild) { b.Pkg = name + "-" + upstream.Version },
		func(b *database.PushBuild) { b.Pkg = name + "-50.9,1" },
		func(b *database.PushBuild) { b.Origin = "devel/thing@py311" },
		func(b *database.PushBuild) { b.ABI = "freebsd:14:amd64" },
		func(b *database.PushBuild) { b.Pkg = "local-only-3" },
	} {
		build := ahead
		edit(&build)
		if got, wait := buildAlert(build, snapshot); got != nil || wait {
			t.Errorf("unexpected alert for %+v: %+v, wait=%t", build, got, wait)
		}
	}
	ahead.ABI = ""
	if got, wait := buildAlert(ahead, snapshot); got != nil || !wait {
		t.Fatal("lost build before repository publication", got, wait)
	}
	local := upstream
	local.Local = true
	local.Version = "2.4_2,2"
	snapshot.Compiled[name] = local
	if got, wait := buildAlert(ahead, snapshot); got == nil || wait {
		t.Fatal("published build still waiting", got, wait)
	}
	for _, tc := range []struct{ arch, abi string }{{"amd64", upstream.ABI}, {"arm64", "freebsd:15:aarch64"}, {"arm", ""}} {
		log := "build started at ...\npackage name: test-2\nbuilding for: FreeBSD jail 15.1-RELEASE-p3 FreeBSD 15.1-RELEASE-p3 GENERIC " + tc.arch + "\n"
		if got := builder.PackageLogABI(strings.NewReader(log)); got != tc.abi {
			t.Errorf("jail %s: %s", tc.arch, got)
		}
	}
}

func TestPushQueueReplayAndRetry(t *testing.T) {
	a := testAdmin(t)
	ctx := t.Context()
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	raw := base64.RawURLEncoding
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		counts[r.URL.Path]++
		switch r.URL.Path {
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/bad":
			w.WriteHeader(http.StatusBadRequest)
		case "/retry":
			if counts[r.URL.Path] == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	}))
	defer server.Close()
	subscribe := func(path string) {
		t.Helper()
		_, err := a.cfg.DB.Exec(`INSERT INTO push_subscriptions(endpoint,auth,p256dh) VALUES(?,?,?)
		  ON CONFLICT(endpoint) DO UPDATE SET auth=excluded.auth`, server.URL+path, raw.EncodeToString(auth), raw.EncodeToString(device.PublicKey().Bytes()))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/good", "/retry", "/bad", "/gone"} {
		subscribe(path)
	}
	a.pushClient = server.Client()
	base := time.Now()
	batch := []database.BuildRecord{}
	record := func(offset time.Duration, pkg, outcome string) database.BuildRecord {
		at := base.Add(offset)
		return database.BuildRecord{Package: pkg, Outcome: outcome, Finished: at, Started: at.Add(-time.Minute), Origin: "devel/thing", ABI: "FreeBSD:15:amd64"}
	}
	batch = append(batch, record(-24*time.Hour, "thing-9", "succeeded"), record(0, "thing-2", "succeeded"), record(time.Millisecond, "thing-2", "succeeded"), record(2*time.Millisecond, "thing-3", "failed"))
	s := (*a.cfg.DB)
	if err := s.RecordBuilds(ctx, batch); err != nil {
		t.Fatal(err)
	}
	snapshot := &catalog.Snapshot{Displaced: map[string]packageinfo.Package{"thing": {Name: "thing", Origin: "devel/thing", ABI: "freebsd:15:amd64", Version: "1"}}}
	if err := a.deliverBuildPush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.deliverBuildPush(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := a.cfg.DB.QueryRow("SELECT count(*) FROM push_build_queue").Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("pending = %d, %v", remaining, err)
	}
	// Restart and replay the same retained logs. Re-registering an existing
	// endpoint keeps its original enrollment time and pending delivery.
	restarted, err := New(t.Context(), a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.pushClient = server.Client()
	subscribe("/retry")
	if err := s.RecordBuilds(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cfg.DB.Exec("UPDATE push_build_queue SET next_try=0"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.deliverBuildPush(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cfg.DB.Exec(`INSERT INTO push_build_queue(endpoint,started_ns,package,origin,abi,finished_ns)
        VALUES('unavailable',1,'missing-2','','',?)`, time.Now().Add(-8*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.deliverBuildPush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["/good"] != 2 || counts["/retry"] != 3 || counts["/gone"] != 1 || counts["/bad"] != 2 {
		t.Fatalf("endpoint requests: %v", counts)
	}
	if err := a.cfg.DB.QueryRow("SELECT count(*) FROM push_build_queue").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("pending after retry = %d, %v", remaining, err)
	}
}

func TestUpgradePushSubscriptions(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE push_subscriptions(endpoint TEXT PRIMARY KEY,auth TEXT NOT NULL,p256dh TEXT NOT NULL);
	  INSERT INTO push_subscriptions VALUES('https://push.invalid/old','auth','device');`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{DB: &database.DB{DB: db}, RepositoryURL: "http://repo.test:8080", DataDir: t.TempDir()}
	before := time.Now().Add(-time.Second)
	if _, err := New(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var lang string
	if err := db.QueryRow("SELECT language FROM push_subscriptions WHERE endpoint='https://push.invalid/old'").Scan(&lang); err != nil || lang != "en" {
		t.Fatalf("legacy subscription language = %q, %v", lang, err)
	}
	var since int64
	if err := db.QueryRow("SELECT since_ns FROM push_build_watch WHERE endpoint='https://push.invalid/old'").Scan(&since); err != nil || since < before.UnixNano() {
		t.Fatal("legacy enrollment cutoff", since, err)
	}
	if _, err := db.Exec("UPDATE push_build_watch SET since_ns=123"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.DB.SubscribePush(t.Context(), "https://push.invalid/old", "auth", "device", "pt-BR"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT since_ns FROM push_build_watch").Scan(&since); err != nil || since != 123 {
		t.Fatal("restart moved enrollment cutoff", since, err)
	}
	if err := db.QueryRow("SELECT language FROM push_subscriptions").Scan(&lang); err != nil || lang != "pt-BR" {
		t.Fatalf("re-enrolled subscription language = %q, %v", lang, err)
	}
}
