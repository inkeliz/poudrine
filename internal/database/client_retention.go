package database

import (
	"context"
	"time"
)

func (s DB) CleanupClientActivity(ctx context.Context, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := now.Add(-ClientActivityRetention).Unix()
	// Begin with a write, then keep the expiry decisions in the same snapshot
	// so a concurrent report cannot be erased after updating its timestamp.
	for _, deletion := range []struct {
		query  string
		before int64
	}{
		{"DELETE FROM client_report_packages WHERE ip IN (SELECT ip FROM client_reports WHERE reported_at < ?)", cutoff},
		{"DELETE FROM client_reports WHERE reported_at < ?", cutoff},
		{"DELETE FROM package_requests WHERE last_seen < ?", cutoff},
		{"DELETE FROM clients WHERE last_seen < ?", cutoff},
		{"DELETE FROM client_report_nonces WHERE expires < ?", now.Unix()},
	} {
		if _, err := tx.ExecContext(ctx, deletion.query, deletion.before); err != nil {
			return err
		}
	}
	return tx.Commit()
}
