package service

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/public"
	"github.com/inkeliz/poudrine/internal/statusproto"
)

// The public reader only receives this allowlisted snapshot. Query successful
// worker builds independently of upstream refreshes or repository traffic.
func monitorPublicStats(ctx context.Context, cat *catalog.Catalog, store database.DB, file *os.File) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if snapshot := cat.Current.Load(); snapshot != nil {
			at := time.Now()
			counts := statusproto.Counts{
				Packages: int64(len(snapshot.ByPath)), Ahead: int64(snapshot.Ahead),
				LocalBuilds: new(int64(snapshot.LocalBuilds)),
			}
			today, yesterday, last7Days, err := store.SuccessfulBuildTotals(ctx, at)
			if err != nil {
				log.Printf("read public compilation totals: %v", err)
			} else {
				counts.Compilations = &statusproto.Compilations{
					UTCDate: statusproto.UTCDate(at), Today: today, Yesterday: yesterday, Last7Days: last7Days,
				}
			}
			if err := public.WritePublicStats(file, counts); err != nil {
				log.Printf("write public stats: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
