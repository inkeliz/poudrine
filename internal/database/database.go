package database

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func Open(dir string, readOnly bool) (*DB, error) {
	path, err := filepath.Abs(filepath.Join(dir, "inventory.sqlite"))
	if err != nil {
		return nil, err
	}
	mode := "rwc"
	if readOnly {
		mode = "ro"
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "trusted_schema(0)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if readOnly {
		err = db.Ping()
	} else {
		_, err = db.Exec("PRAGMA journal_mode=WAL")
		if err == nil {
			err = initializeSchema(db)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db}, nil
}

// DB owns the SQLite connection and all persistence operations.
type DB struct{ *sql.DB }

// ErrNotFound indicates that a requested database record does not exist.
var ErrNotFound = sql.ErrNoRows

// Serialize initialization across the worker and HTTP process. WAL mode must
// be set outside the transaction, but all schema and data migrations are atomic.
func initializeSchema(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	if _, err := conn.ExecContext(ctx, clientsSchema+trackedPackagesSchema+packageRequestsSchema+reportSchema+BuildHistorySchema); err != nil {
		return err
	}
	if err := upgradeReportActivity(ctx, conn); err != nil {
		return err
	}
	if err := upgradeReportKeyRetention(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
