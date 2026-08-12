package health

import (
	"context"
	"fmt"
	"time"
)

// WaitFor polls checks until they all pass or ctx is done, whichever comes
// first. A rollout gate needs this rather than a single probe: a pod that is
// still starting is not yet unhealthy.
//
// It returns nil once everything passes, or the last failure wrapped with the
// reason it gave up.
func WaitFor(ctx context.Context, checks []Check, interval time.Duration) error {
	if len(checks) == 0 {
		return nil
	}
	if interval <= 0 {
		interval = time.Second
	}

	// Probe immediately - waiting a full interval before the first attempt is a
	// needless delay when the workload is already up.
	lastErr := CheckAll(ctx, checks)
	if lastErr == nil {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop() // a ticker that is never stopped leaks its goroutine

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for health: %w (last failure: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
			if lastErr = CheckAll(ctx, checks); lastErr == nil {
				return nil
			}
		}
	}
}
