package database

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
)

const adminSchema = adminIdentitySchema + pushSubscriptionsSchema

const adminIdentitySchema = `
CREATE TABLE IF NOT EXISTS admin_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	public_key BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_nonces (
	nonce BLOB PRIMARY KEY,
	expires INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS admin_nonce_expiry ON admin_nonces(expires);
`

func (s DB) PrepareAdmin(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, BuildHistorySchema+adminSchema); err != nil {
		return err
	}
	var hasLanguage bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('push_subscriptions') WHERE name='language')").Scan(&hasLanguage); err != nil {
		return err
	}
	if !hasLanguage {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE push_subscriptions ADD COLUMN language TEXT NOT NULL DEFAULT 'en'"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s DB) PinAdmin(ctx context.Context, key ed25519.PublicKey) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO admin_identity(id,public_key) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET public_key=excluded.public_key", []byte(key))
	return err
}

func (s DB) AdminEnrolled(ctx context.Context) (bool, error) {
	var enrolled bool
	err := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM admin_identity)").Scan(&enrolled)
	return enrolled, err
}

func (s DB) AdminKey(ctx context.Context) (ed25519.PublicKey, error) {
	var key []byte
	err := s.DB.QueryRowContext(ctx, "SELECT public_key FROM admin_identity WHERE id=1").Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return key, err
}

func (s DB) ExistingAdminKey(ctx context.Context) (ed25519.PublicKey, error) {
	var hasTable bool
	if err := s.DB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='admin_identity')").Scan(&hasTable); err != nil {
		return nil, err
	}
	if !hasTable {
		return nil, nil
	}
	return s.AdminKey(ctx)
}

func (s DB) EnrollAdmin(ctx context.Context, key ed25519.PublicKey) (int64, error) {
	res, err := s.DB.ExecContext(ctx, "INSERT INTO admin_identity(id,public_key) VALUES(1,?) ON CONFLICT(id) DO NOTHING", key)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s DB) ConsumeAdminNonce(ctx context.Context, nonce []byte, stamp int64) (bool, error) {
	res, err := s.DB.ExecContext(ctx, "INSERT INTO admin_nonces(nonce,expires) VALUES(?,?) ON CONFLICT DO NOTHING", nonce, stamp+requestauth.ClockSkewSeconds)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n != 0, err
}

func (s DB) CleanupAdminNonces(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM admin_nonces WHERE expires < ?", now.Unix())
	return err
}

// ReadAdminKey reads a prior enrollment without creating a database or tables.
func ReadAdminKey(ctx context.Context, dir string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(filepath.Join(dir, "inventory.sqlite")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	db, err := Open(dir, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.ExistingAdminKey(ctx)
}
