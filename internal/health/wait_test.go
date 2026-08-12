package health

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyCheck fails a fixed number of times, then passes - a workload that is
// still starting up.
type flakyCheck struct {
	failures int32
	attempts atomic.Int32
}

func (f *flakyCheck) Describe() string { return "flaky" }

func (f *flakyCheck) Check(ctx context.Context) error {
	if f.attempts.Add(1) <= f.failures {
		return errors.New("not ready yet")
	}
	return nil
}

func TestWaitForPassesImmediately(t *testing.T) {
	start := time.Now()

	err := WaitFor(context.Background(), []Check{fakeCheck{name: "ok"}}, time.Second)
	if err != nil {
		t.Fatalf("WaitFor() = %v, want nil", err)
	}

	// It must probe before the first tick, not after a full interval.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v - probe once before waiting an interval", elapsed)
	}
}

func TestWaitForRetriesUntilHealthy(t *testing.T) {
	flaky := &flakyCheck{failures: 3}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitFor(ctx, []Check{flaky}, 20*time.Millisecond); err != nil {
		t.Fatalf("WaitFor() = %v, want nil once it comes up", err)
	}

	// 3 failures then a pass.
	if got := flaky.attempts.Load(); got != 4 {
		t.Errorf("attempted %d times, want 4", got)
	}
}

func TestWaitForGivesUpOnDeadline(t *testing.T) {
	// Never becomes healthy.
	flaky := &flakyCheck{failures: 1 << 30}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := WaitFor(ctx, []Check{flaky}, 20*time.Millisecond)
	if err == nil {
		t.Fatal("WaitFor() = nil, want a deadline failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitFor() = %v, want context.DeadlineExceeded in the chain", err)
	}
	// The message should carry why it was unhealthy, not just "timed out".
	if !strings.Contains(err.Error(), "not ready yet") {
		t.Errorf("error %q should include the last failure", err)
	}
}

func TestWaitForNoChecks(t *testing.T) {
	if err := WaitFor(context.Background(), nil, time.Second); err != nil {
		t.Errorf("WaitFor(nil) = %v, want nil", err)
	}
}
