package testutil

import (
	"context"
	"time"
)

// Wait checks the condition immediately and at short intervals until it holds
// or the test's context expires.
func Wait(ctx context.Context, condition func() bool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
