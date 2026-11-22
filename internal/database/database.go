package database

import (
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
		_, err = db.Exec("\nPRAGMA journal_mode=WAL;\n" + clientsSchema + trackedPackagesSchema + packageRequestsSchema + reportSchema + BuildHistorySchema)
		if err == nil {
			err = upgradeReportActivity(db)
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
