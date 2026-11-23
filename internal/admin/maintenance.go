package admin

import (
	"context"
	"log"
	"time"
)

// CleanupNonces keeps expired replay protection out of the request write path.
func (a *Admin) CleanupNonces(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := a.cfg.DB.CleanupAdminNonces(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("clean up admin nonces: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
