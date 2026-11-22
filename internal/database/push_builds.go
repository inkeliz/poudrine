package database

import (
	"context"
	"time"
)

const pushBuildsSchema = `CREATE TABLE IF NOT EXISTS push_build_watch (
 endpoint TEXT PRIMARY KEY, since_ns INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS push_build_queue (
 endpoint TEXT NOT NULL, started_ns INTEGER NOT NULL, package TEXT NOT NULL,
 origin TEXT NOT NULL, abi TEXT NOT NULL, finished_ns INTEGER NOT NULL,
 next_try INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(endpoint,started_ns,package)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS push_build_age ON push_build_queue(finished_ns);
CREATE INDEX IF NOT EXISTS push_build_due ON push_build_queue(next_try,finished_ns,package);
CREATE TRIGGER IF NOT EXISTS push_build_expire AFTER DELETE ON package_builds WHEN OLD.outcome='succeeded' BEGIN
 DELETE FROM push_build_queue WHERE started_ns=OLD.started_ns AND package=OLD.package;
END;`

type PushBuild struct {
	Endpoint         string `json:"-"`
	Started          int64  `json:"-"`
	Pkg, Origin, ABI string `json:"-"`
}

func (s DB) ExpireBuildPush(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM push_build_queue WHERE finished_ns < ?`, now.Add(-7*24*time.Hour).UnixNano())
	return err
}

func (s DB) PendingBuildPush(ctx context.Context, now time.Time) ([]PushBuild, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT endpoint,started_ns,package,origin,abi
	 FROM push_build_queue WHERE next_try <= ? ORDER BY next_try,finished_ns,package LIMIT 64`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []PushBuild
	for rows.Next() {
		var p PushBuild
		err = rows.Scan(&p.Endpoint, &p.Started, &p.Pkg, &p.Origin, &p.ABI)
		if err != nil {
			return nil, err
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return batch, nil
}

func (s DB) BuildPushSubscription(ctx context.Context, p PushBuild) (auth, p256dh string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT s.auth,s.p256dh FROM push_subscriptions s
 JOIN push_build_queue q USING(endpoint) WHERE q.endpoint=? AND q.started_ns=? AND q.package=?`,
		p.Endpoint, p.Started, p.Pkg).Scan(&auth, &p256dh)
	return
}

func (s DB) FinishBuildPush(ctx context.Context, p PushBuild, wait bool, now time.Time) error {
	var err error
	if wait {
		_, err = s.DB.ExecContext(ctx, `UPDATE push_build_queue SET next_try=?
 WHERE endpoint=? AND started_ns=? AND package=?`, now.Add(time.Minute).Unix(), p.Endpoint, p.Started, p.Pkg)
	} else {
		_, err = s.DB.ExecContext(ctx, `DELETE FROM push_build_queue WHERE endpoint=? AND started_ns=? AND package=?`, p.Endpoint, p.Started, p.Pkg)
	}
	return err
}
