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

func (s DB) PrepareAdmin() error {
	_, err := s.DB.Exec(BuildHistorySchema + adminSchema)
	return err
}

func (s DB) PinAdmin(key ed25519.PublicKey) error {
	_, err := s.DB.Exec("INSERT INTO admin_identity(id,public_key) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET public_key=excluded.public_key", []byte(key))
	return err
}

func (s DB) AdminEnrolled() (bool, error) {
	var enrolled bool
	err := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM admin_identity)").Scan(&enrolled)
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM admin_nonces WHERE expires < ?", time.Now().Unix()); err != nil {
		return false, err
	}
	res, err := tx.Exec("INSERT INTO admin_nonces(nonce,expires) VALUES(?,?) ON CONFLICT DO NOTHING", nonce, stamp+requestauth.ClockSkewSeconds)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	return true, tx.Commit()
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
