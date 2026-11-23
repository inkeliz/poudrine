package database

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
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
