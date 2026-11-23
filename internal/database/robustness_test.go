package database

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/requestauth"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestAdminNonceCleanupIsIndependent(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	if err := db.PrepareAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	old := now.Unix() - requestauth.ClockSkewSeconds - 1
	for _, stamp := range []int64{old, now.Unix()} {
		nonce := []byte(time.Unix(stamp, 0).String())
		if ok, err := db.ConsumeAdminNonce(ctx, nonce, stamp); err != nil || !ok {
			t.Fatalf("first use: %v, %v", ok, err)
		}
		if ok, err := db.ConsumeAdminNonce(ctx, nonce, stamp); err != nil || ok {
			t.Fatalf("replay: %v, %v", ok, err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM admin_nonces").Scan(&count); err != nil || count != 2 {
		t.Fatalf("request cleaned nonces: %d, %v", count, err)
	}
	if err := db.CleanupAdminNonces(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM admin_nonces").Scan(&count); err != nil || count != 1 {
		t.Fatalf("cleanup: %d, %v", count, err)
	}
	nonce := []byte(now.String())
	if ok, err := db.ConsumeAdminNonce(ctx, nonce, now.Unix()); err != nil || ok {
		t.Fatalf("cleanup allowed live replay: %v, %v", ok, err)
	}
}

func TestAdminDatabaseCancellationWhileWaiting(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PrepareAdmin(t.Context()); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	operations := map[string]func(context.Context) error{
		"prepare":  db.PrepareAdmin,
		"pin":      func(ctx context.Context) error { return db.PinAdmin(ctx, key) },
		"enrolled": func(ctx context.Context) error { _, err := db.AdminEnrolled(ctx); return err },
		"key":      func(ctx context.Context) error { _, err := db.AdminKey(ctx); return err },
		"enroll":   func(ctx context.Context) error { _, err := db.EnrollAdmin(ctx, key); return err },
		"nonce": func(ctx context.Context) error {
			_, err := db.ConsumeAdminNonce(ctx, []byte("nonce"), time.Now().Unix())
			return err
		},
		"cleanup":             func(ctx context.Context) error { return db.CleanupAdminNonces(ctx, time.Now()) },
		"push keys":           func(ctx context.Context) error { _, _, err := db.PushKeys(ctx); return err },
		"save push keys":      func(ctx context.Context) error { return db.SavePushKeys(ctx, "private", "public") },
		"subscribe":           func(ctx context.Context) error { return db.SubscribePush(ctx, "endpoint", "auth", "key", "en") },
		"unsubscribe":         func(ctx context.Context) error { return db.UnsubscribePush(ctx, "endpoint") },
		"remove subscription": func(ctx context.Context) error { return db.RemovePushSubscription(ctx, "endpoint", "auth", "key") },
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- op(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(time.Second):
				// Release the connection even if a context-free operation regresses.
				conn.Close()
				<-done
				t.Fatal("database operation ignored cancellation")
			}
		})
	}
}

func TestOpenRollsBackPartialSchema(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "inventory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Fail late in initialization, after the client tables have been created.
	if _, err := raw.Exec("CREATE TABLE package_builds (broken INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(dir, false); err == nil {
		db.Close()
		t.Fatal("opened incompatible schema")
	}
	var exists bool
	if err := raw.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='clients')").Scan(&exists); err != nil || exists {
		t.Fatalf("partial schema persisted: %v, %v", exists, err)
	}
	if _, err := raw.Exec("DROP TABLE package_builds"); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

func TestInventoryValidationBeforeWrites(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	good := packageinfo.Package{Name: "curl", Version: "8.1_2,1", Origin: "ftp/curl", BuildOrigin: "ftp/curl@default"}
	for _, tc := range []struct {
		name   string
		mutate func(*packageinfo.Package)
	}{
		{"empty name", func(p *packageinfo.Package) { p.Name = "" }},
		{"long name", func(p *packageinfo.Package) { p.Name = strings.Repeat("a", 256) }},
		{"name newline", func(p *packageinfo.Package) { p.Name = "bad\nname" }},
		{"empty version", func(p *packageinfo.Package) { p.Version = "" }},
		{"long version", func(p *packageinfo.Package) { p.Version = strings.Repeat("1", 256) }},
		{"version slash", func(p *packageinfo.Package) { p.Version = "1/2" }},
		{"empty origin", func(p *packageinfo.Package) { p.Origin = "" }},
		{"long origin", func(p *packageinfo.Package) { p.Origin = "ftp/" + strings.Repeat("a", 256) }},
		{"origin newline", func(p *packageinfo.Package) { p.Origin = "ftp/curl\n" }},
		{"build injection", func(p *packageinfo.Package) { p.BuildOrigin = "ftp/curl\ndevel/other" }},
		{"long build origin", func(p *packageinfo.Package) { p.BuildOrigin = "ftp/" + strings.Repeat("a", 256) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mutate(&p)
			if err := db.Record(t.Context(), "::1", &p); err == nil {
				t.Fatal("invalid request accepted")
			}
			installed := InstalledPackage{Version: p.Version, Origin: p.Origin, BuildOrigin: p.BuildOrigin}
			if err := db.ReplaceReport(t.Context(), "::1", testutil.ReportProof(), map[string]InstalledPackage{p.Name: installed}); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	for _, ip := range []string{"", strings.Repeat("1", 65), "not-an-ip"} {
		if err := db.Record(t.Context(), ip, nil); err == nil {
			t.Fatalf("accepted address %q", ip)
		}
		if err := db.ReplaceReport(t.Context(), ip, testutil.ReportProof(), nil); err == nil {
			t.Fatalf("accepted report address %q", ip)
		}
	}
	clients, err := db.Clients(t.Context())
	if err != nil || len(clients) != 0 {
		t.Fatalf("invalid input wrote clients: %v, %v", clients, err)
	}
	if err := db.Record(t.Context(), "2001:db8::1", &good); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceReport(t.Context(), "2001:db8::1", testutil.ReportProof(), map[string]InstalledPackage{good.Name: {Version: good.Version, Origin: good.Origin}}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminWriteCancellationDuringSQLiteLock(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PrepareAdmin(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO admin_nonces VALUES('held',0)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := db.ConsumeAdminNonce(ctx, []byte("waiting"), time.Now().Unix()); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait: %v", err)
		}
	case <-time.After(time.Second):
		tx.Rollback()
		<-done
		t.Fatal("SQLite write ignored cancellation during lock wait")
	}
}
