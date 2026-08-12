package rollout

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/health"
)

// fakeTarget stands in for a Kubernetes Deployment. It records what was done to
// it so a test can assert the sequence, not just the outcome.
type fakeTarget struct {
	mu sync.Mutex

	current  string
	setTo    []string // every digest SetImage was called with
	rolled   int      // how many times Rollback was called
	settleFn func(ctx context.Context) error

	setErr      error
	currentErr  error
	rollbackErr error
}

func (f *fakeTarget) Describe() string { return "fake/deployment" }

func (f *fakeTarget) Current(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentErr != nil {
		return "", f.currentErr
	}
	return f.current, nil
}

func (f *fakeTarget) SetImage(ctx context.Context, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.setTo = append(f.setTo, digest)
	f.current = digest
	return nil
}

func (f *fakeTarget) WaitSettled(ctx context.Context) error {
	f.mu.Lock()
	fn := f.settleFn
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (f *fakeTarget) Rollback(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolled++
	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	return nil
}

func (f *fakeTarget) rollbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rolled
}

// Compile-time proof the fake satisfies the interface.
var _ Target = (*fakeTarget)(nil)

// okCheck is a health.Check that always passes.
type okCheck struct{}

func (okCheck) Describe() string                { return "ok" }
func (okCheck) Check(ctx context.Context) error { return nil }

// badCheck always fails.
type badCheck struct{}

func (badCheck) Describe() string                { return "bad" }
func (badCheck) Check(ctx context.Context) error { return errors.New("still unhealthy") }

// phases extracts the phase sequence from a result, for readable assertions.
func phases(r Result) []Phase {
	out := make([]Phase, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Phase)
	}
	return out
}

func lastPhase(r Result) Phase {
	if len(r.Events) == 0 {
		return ""
	}
	return r.Events[len(r.Events)-1].Phase
}

// -- happy path ---------------------------------------------------------------

func TestDeploySucceeds(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v (phases %v)", res.Err, phases(res))
	}
	if lastPhase(res) != PhaseSucceeded {
		t.Errorf("last phase = %q, want %q", lastPhase(res), PhaseSucceeded)
	}
	if got := target.setTo; len(got) != 1 || got[0] != "sha256:new" {
		t.Errorf("SetImage calls = %v, want [sha256:new]", got)
	}
	if n := target.rollbackCount(); n != 0 {
		t.Errorf("rolled back %d times on a healthy deploy, want 0", n)
	}
}

func TestDeployWithPassingChecks(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	opts := Options{
		Checks:         []health.Check{okCheck{}, okCheck{}},
		HealthTimeout:  2 * time.Second,
		HealthInterval: 10 * time.Millisecond,
	}
	res := Collect(Deploy(context.Background(), target, "sha256:new", opts))

	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v", res.Err)
	}
	// The checking phase must actually have been entered.
	if !slicesContains(phases(res), PhaseChecking) {
		t.Errorf("phases %v, want a %q phase", phases(res), PhaseChecking)
	}
}

// Redeploying the digest already running is a no-op, not a pointless rollout.
func TestDeploySameDigestIsNoOp(t *testing.T) {
	target := &fakeTarget{current: "sha256:same"}

	res := Collect(Deploy(context.Background(), target, "sha256:same", Options{}))

	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v", res.Err)
	}
	if len(target.setTo) != 0 {
		t.Errorf("SetImage was called %v, want no call for an unchanged digest", target.setTo)
	}
}

// -- rollback paths -----------------------------------------------------------

func TestDeployRollsBackWhenSettleFails(t *testing.T) {
	target := &fakeTarget{
		current:  "sha256:old",
		settleFn: func(ctx context.Context) error { return errors.New("progress deadline exceeded") },
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if n := target.rollbackCount(); n != 1 {
		t.Errorf("rolled back %d times, want 1", n)
	}
	if !slicesContains(phases(res), PhaseRollingBck) {
		t.Errorf("phases %v, want a %q phase", phases(res), PhaseRollingBck)
	}
	if !strings.Contains(res.Err.Error(), "progress deadline exceeded") {
		t.Errorf("err = %v, want it to carry the settle failure", res.Err)
	}
}

func TestDeployRollsBackWhenHealthFails(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	opts := Options{
		Checks:         []health.Check{badCheck{}},
		HealthTimeout:  120 * time.Millisecond,
		HealthInterval: 20 * time.Millisecond,
	}
	res := Collect(Deploy(context.Background(), target, "sha256:new", opts))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if n := target.rollbackCount(); n != 1 {
		t.Errorf("rolled back %d times, want 1", n)
	}
	if !strings.Contains(res.Err.Error(), "still unhealthy") {
		t.Errorf("err = %v, want it to carry the health failure", res.Err)
	}
}

// A failure before SetImage must NOT roll back - nothing was changed.
func TestDeployDoesNotRollBackIfNothingChanged(t *testing.T) {
	target := &fakeTarget{current: "sha256:old", setErr: errors.New("forbidden")}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if n := target.rollbackCount(); n != 0 {
		t.Errorf("rolled back %d times, want 0 - SetImage never took effect", n)
	}
}

func TestDeployReportsFailedRollback(t *testing.T) {
	target := &fakeTarget{
		current:     "sha256:old",
		settleFn:    func(ctx context.Context) error { return errors.New("crashloop") },
		rollbackErr: errors.New("rollback rejected"),
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	// Both the original cause and the rollback failure must survive.
	if !strings.Contains(res.Err.Error(), "crashloop") {
		t.Errorf("err = %v, want the original cause", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "rollback rejected") {
		t.Errorf("err = %v, want the rollback failure too", res.Err)
	}
}

func TestDeployFailsIfCurrentUnreadable(t *testing.T) {
	target := &fakeTarget{currentErr: errors.New("no such deployment")}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if len(target.setTo) != 0 {
		t.Error("SetImage was called despite not knowing the current image")
	}
}

// -- cancellation -------------------------------------------------------------

// Cancelling mid-settle must still roll back: an abandoned half-deploy is worse
// than a rollback nobody asked for.
func TestDeployCancelledStillRollsBack(t *testing.T) {
	target := &fakeTarget{
		current: "sha256:old",
		settleFn: func(ctx context.Context) error {
			<-ctx.Done() // block until cancelled
			return ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := Deploy(ctx, target, "sha256:new", Options{})

	// Let it get as far as settling, then pull the rug.
	time.AfterFunc(50*time.Millisecond, cancel)

	res := Collect(events)

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure after cancellation")
	}
	if n := target.rollbackCount(); n != 1 {
		t.Errorf("rolled back %d times, want 1 even though ctx was cancelled", n)
	}
}

func TestDeploySettleTimeoutIsEnforced(t *testing.T) {
	target := &fakeTarget{
		current: "sha256:old",
		settleFn: func(ctx context.Context) error {
			<-ctx.Done() // never settles on its own
			return ctx.Err()
		},
	}

	opts := Options{SettleTimeout: 80 * time.Millisecond}

	start := time.Now()
	res := Collect(Deploy(context.Background(), target, "sha256:new", opts))
	elapsed := time.Since(start)

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want a timeout failure")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, want the SettleTimeout to cut it short", elapsed)
	}
	if n := target.rollbackCount(); n != 1 {
		t.Errorf("rolled back %d times, want 1", n)
	}
}

// -- events -------------------------------------------------------------------

func TestEventsAreOrderedAndStamped(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	want := []Phase{PhaseStarting, PhaseUpdating, PhaseSettling, PhaseSucceeded}
	got := phases(res)
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phase[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	for i, e := range res.Events {
		if e.At.IsZero() {
			t.Errorf("event[%d] (%s) has no timestamp", i, e.Phase)
		}
	}
}

func slicesContains(haystack []Phase, needle Phase) bool {
	for _, p := range haystack {
		if p == needle {
			return true
		}
	}
	return false
}
