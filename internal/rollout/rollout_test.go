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

	current          string
	setTo            []string // every digest SetImage was called with
	rolled           int      // how many times Rollback was called
	rolledTo         []string // every image Rollback was asked to restore
	rolledToReplicas []*int32 // every replica count Rollback was asked to restore
	scaledTo         []int32  // every count SetImageAndReplicas was given
	replicas         int32    // the target's present desired count
	settleFn         func(ctx context.Context) error

	replicasErr error

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

func (f *fakeTarget) SetImageAndReplicas(ctx context.Context, digest string, replicas int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.setTo = append(f.setTo, digest)
	f.current = digest
	f.scaledTo = append(f.scaledTo, replicas)
	// Take effect, as a real patch would - otherwise a later Replicas() read
	// reports the pre-deploy count and the fake lies about the cluster.
	f.replicas = replicas
	return nil
}

func (f *fakeTarget) scaleCalls() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int32(nil), f.scaledTo...)
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

func (f *fakeTarget) Replicas(ctx context.Context) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replicasErr != nil {
		return 0, f.replicasErr
	}
	return f.replicas, nil
}

func (f *fakeTarget) Rollback(ctx context.Context, toImage string, toReplicas *int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolled++
	f.rolledTo = append(f.rolledTo, toImage)
	f.rolledToReplicas = append(f.rolledToReplicas, toReplicas)

	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	f.current = toImage
	if toReplicas != nil {
		f.replicas = *toReplicas
	}
	return nil
}

// rollbackReplicas returns the replica counts Rollback was asked to restore, nil
// entries included.
func (f *fakeTarget) rollbackReplicas() []*int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*int32(nil), f.rolledToReplicas...)
}

func (f *fakeTarget) rollbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rolled
}

func (f *fakeTarget) rollbackTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rolledTo...)
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

// -- replica count as a deploy parameter --------------------------------------

func int32p(n int32) *int32 { return &n }

// Image and replica count must move in one patch, so the controller starts a
// single rollout that already knows both.
func TestDeployWithReplicaCount(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{
		Replicas: int32p(3),
	}))

	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v", res.Err)
	}
	if got := target.scaleCalls(); len(got) != 1 || got[0] != 3 {
		t.Errorf("scale calls = %v, want [3]", got)
	}
	// One call, not a SetImage followed by a scale.
	if got := target.setTo; len(got) != 1 {
		t.Errorf("SetImage-ish calls = %v, want exactly one", got)
	}
}

func TestDeployWithoutReplicaCountDoesNotScale(t *testing.T) {
	target := &fakeTarget{current: "sha256:old"}

	Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if got := target.scaleCalls(); len(got) != 0 {
		t.Errorf("scaled to %v, want no scaling when Replicas is nil", got)
	}
}

// "Same jar, three instances for load testing" must not be short-circuited by
// the already-running-this-digest check.
func TestDeploySameDigestStillScales(t *testing.T) {
	target := &fakeTarget{current: "sha256:same"}

	res := Collect(Deploy(context.Background(), target, "sha256:same", Options{
		Replicas: int32p(3),
	}))

	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v", res.Err)
	}
	if got := target.scaleCalls(); len(got) != 1 || got[0] != 3 {
		t.Errorf("scale calls = %v, want [3] - a scale-only deploy must still apply", got)
	}
}

// A rollback reverts exactly the fields the deploy set. This deploy changed the
// replica count, so the rollback restores it.
func TestRollbackRestoresReplicasTheDeploySet(t *testing.T) {
	target := &fakeTarget{
		current:  "sha256:old",
		replicas: 1,
		settleFn: func(ctx context.Context) error { return errors.New("crashloop") },
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{
		Replicas: int32p(4),
	}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if got := target.rollbackTargets(); len(got) != 1 || got[0] != "sha256:old" {
		t.Errorf("rolled back to %v, want [sha256:old]", got)
	}

	restored := target.rollbackReplicas()
	if len(restored) != 1 {
		t.Fatalf("Rollback called %d times, want 1", len(restored))
	}
	if restored[0] == nil || *restored[0] != 1 {
		t.Errorf("restored replicas = %v, want 1 - the count before this deploy", restored[0])
	}

	// And the report says so, both ends of it.
	if !strings.Contains(res.Err.Error(), "replicas restored 4 -> 1") {
		t.Errorf("err = %v, want it to state the replica change", res.Err)
	}
}

// This deploy did not touch replicas, so the rollback must not either -
// undoing a capacity decision nobody made here would be wrong.
func TestRollbackLeavesReplicasTheDeployDidNotSet(t *testing.T) {
	target := &fakeTarget{
		current:  "sha256:old",
		replicas: 3,
		settleFn: func(ctx context.Context) error { return errors.New("crashloop") },
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}

	restored := target.rollbackReplicas()
	if len(restored) != 1 {
		t.Fatalf("Rollback called %d times, want 1", len(restored))
	}
	if restored[0] != nil {
		t.Errorf("restored replicas = %d, want nil - this deploy never set them", *restored[0])
	}

	// Still reported, so an operator is never left guessing.
	if !strings.Contains(res.Err.Error(), "replicas left at 3 (not set by this deploy)") {
		t.Errorf("err = %v, want it to state the count was left alone", res.Err)
	}
}

// A deploy that did not change replicas never needs to read them.
func TestReplicasNotReadWhenNotScaling(t *testing.T) {
	target := &fakeTarget{
		current:     "sha256:old",
		replicasErr: errors.New("should not be called before SetImage"),
	}

	// A healthy deploy: no rollback, so Replicas is only read if the deploy is
	// scaling - and it is not.
	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))
	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false, err = %v - a non-scaling deploy must not need the count", res.Err)
	}
}

// A count that cannot be read is a failure before anything is changed.
func TestScalingDeployFailsIfReplicasUnreadable(t *testing.T) {
	target := &fakeTarget{
		current:     "sha256:old",
		replicasErr: errors.New("no such deployment"),
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{
		Replicas: int32p(2),
	}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}
	if len(target.setTo) != 0 {
		t.Error("the image was changed despite not knowing the replica count to undo to")
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

// Rollback must be told which image to restore, and it must be the one this
// deploy replaced - not whatever the target happens to be running by then.
func TestDeployRollsBackToTheImageItReplaced(t *testing.T) {
	target := &fakeTarget{
		current:  "sha256:original",
		settleFn: func(ctx context.Context) error { return errors.New("crashloop") },
	}

	res := Collect(Deploy(context.Background(), target, "sha256:new", Options{}))

	if res.Succeeded() {
		t.Fatal("Succeeded() = true, want failure")
	}

	got := target.rollbackTargets()
	if len(got) != 1 {
		t.Fatalf("Rollback called %d times, want 1", len(got))
	}
	if got[0] != "sha256:original" {
		t.Errorf("rolled back to %q, want %q", got[0], "sha256:original")
	}

	// And the target really is back on it.
	if cur, _ := target.Current(context.Background()); cur != "sha256:original" {
		t.Errorf("target on %q, want %q", cur, "sha256:original")
	}
}

// Two deploys in flight against one target must each roll back to the image
// *they* replaced. This is the bug that shared rollback state on the target
// caused: the second deploy overwrote the first's memory of what to undo.
func TestConcurrentDeploysRollBackIndependently(t *testing.T) {
	failing := func(ctx context.Context) error { return errors.New("did not settle") }

	a := &fakeTarget{current: "sha256:aaa", settleFn: failing}
	b := &fakeTarget{current: "sha256:bbb", settleFn: failing}

	var wg sync.WaitGroup
	for _, tc := range []struct {
		target *fakeTarget
		want   string
	}{
		{a, "sha256:aaa"},
		{b, "sha256:bbb"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Collect(Deploy(context.Background(), tc.target, "sha256:new", Options{}))
		}()
	}
	wg.Wait()

	for name, tc := range map[string]struct {
		target *fakeTarget
		want   string
	}{
		"a": {a, "sha256:aaa"},
		"b": {b, "sha256:bbb"},
	} {
		got := tc.target.rollbackTargets()
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("target %s rolled back to %v, want [%s]", name, got, tc.want)
		}
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
