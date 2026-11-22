package database

import (
	"context"
)

func (s DB) PushKeys() (private, public string, err error) {
	err = s.DB.QueryRow("SELECT private_key,public_key FROM push_identity WHERE id=1").Scan(&private, &public)
	return
}

func (s DB) SavePushKeys(private, public string) error {
	_, err := s.DB.Exec("INSERT INTO push_identity(id,private_key,public_key) VALUES(1,?,?) ON CONFLICT DO NOTHING", private, public)
	return err
}

func (s DB) SubscribePush(ctx context.Context, endpoint, auth, p256dh, language string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO push_subscriptions(endpoint,auth,p256dh,language) VALUES(?,?,?,?)
 ON CONFLICT(endpoint) DO UPDATE SET auth=excluded.auth,p256dh=excluded.p256dh,language=excluded.language`, endpoint, auth, p256dh, language)
	return err
}

func (s DB) UnsubscribePush(ctx context.Context, endpoint string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint=?", endpoint)
	return err
}

func (s DB) RemovePushSubscription(ctx context.Context, endpoint, auth, p256dh string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint=? AND auth=? AND p256dh=?", endpoint, auth, p256dh)
	return err
}

const pushSubscriptionsSchema = `CREATE TABLE IF NOT EXISTS push_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	private_key TEXT NOT NULL,
	public_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS push_subscriptions (
	endpoint TEXT PRIMARY KEY,
	auth TEXT NOT NULL,
	p256dh TEXT NOT NULL,
	language TEXT NOT NULL DEFAULT 'en'
);
CREATE TRIGGER IF NOT EXISTS push_watch_new AFTER INSERT ON push_subscriptions BEGIN
 INSERT INTO push_build_watch VALUES(NEW.endpoint, CAST(unixepoch('subsec') * 1000000000 AS INTEGER));
END;
CREATE TRIGGER IF NOT EXISTS push_unwatch AFTER DELETE ON push_subscriptions BEGIN
 DELETE FROM push_build_queue WHERE endpoint=OLD.endpoint;
 DELETE FROM push_build_watch WHERE endpoint=OLD.endpoint;
END;
INSERT OR IGNORE INTO push_build_watch
 SELECT endpoint, CAST(unixepoch('subsec') * 1000000000 AS INTEGER) FROM push_subscriptions;`
