package builder

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func nextHourSchedule(t *testing.T) config.WorkingHours {
	t.Helper()
	// Pick a short window well away from the current wall clock.
	start := (time.Now().Hour() + 4) % 24
	hours, err := config.ParseWorkingHours(fmt.Sprintf("%02d:00-%02d:01", start, start))
	if err != nil {
		t.Fatal(err)
	}
	return hours
}

func awaitWorker(t *testing.T, read func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	if err := testutil.Wait(ctx, read); err != nil {
		t.Fatalf("waiting for worker: %v", err)
	}
}
