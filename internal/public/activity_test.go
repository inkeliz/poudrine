package public

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestRepositoryServesDuringInventoryContention(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body := "verified local archive"
	p := testutil.Package("fixture", body)
	p.Local = true
	testutil.WriteFile(t, filepath.Join(dir, p.Path), []byte(body))
	cat := &catalog.Catalog{}
	cat.Current.Store(&catalog.Snapshot{ByPath: map[string]packageinfo.Package{p.Path: p}, Metadata: map[string]catalog.MetadataFile{"meta.conf": {Body: []byte("version = 2;")}}})
	repo := &Repository{Catalog: cat, Store: *db, LocalDir: dir}
	defer repo.StartRecording(t.Context())()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO clients VALUES('::1',0,0)"); err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/" + p.Path, 200, body}, {"HEAD", "/" + p.Path, 200, ""}, {"GET", "/meta.conf", 200, "version = 2;"},
	} {
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); repo.ServeHTTP(w, httptest.NewRequest(req.method, req.path, nil)) }()
		select {
		case <-done:
			if w.Code != req.code || w.Body.String() != req.body {
				t.Fatalf("%s %s: %d %q", req.method, req.path, w.Code, w.Body.String())
			}
		case <-time.After(time.Second):
			tx.Rollback()
			<-done
			t.Fatal("inventory lock blocked the response")
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, *db, 1)
	clients, err := db.Clients(t.Context())
	if err != nil || len(clients[0].Packages) != 1 {
		t.Fatalf("activity lost after lock released: %v, %v", clients, err)
	}
}

func TestRepositoryActivityQueueIsBounded(t *testing.T) {
	repo := &Repository{activity: make(chan clientActivity, 1)}
	p := testutil.Package("original", "data")
	repo.recordActivity("::1", &p)
	p.Name = "changed"
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10000 {
			repo.recordActivity("::1", &p)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full activity queue blocked a request")
	}
	event := <-repo.activity
	if event.pkg.Name != "original" || len(repo.activity) != 0 {
		t.Fatal("activity aliases caller state or queue grew")
	}
}
