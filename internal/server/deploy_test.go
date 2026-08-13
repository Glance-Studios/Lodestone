package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
	"github.com/Glance-Studios/Lodestone/internal/store"
	"github.com/Glance-Studios/Lodestone/internal/target"
)

func TestDeployHappyPath(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?version=1.0.0&by=cammy", devToken, "jar bytes")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	got := decodeResult(t, rec)
	if !got.Deployed {
		t.Error("Deployed = false, want true")
	}
	if got.Target != "dev-lobby" {
		t.Errorf("Target = %q, want dev-lobby", got.Target)
	}
	// The image is this target's repository, pinned by digest.
	if !strings.HasPrefix(got.Image, "reg/dev/lobby@sha256:") {
		t.Errorf("Image = %q, want reg/dev/lobby@sha256:...", got.Image)
	}
	if len(got.Events) == 0 {
		t.Error("Events is empty")
	}

	// The packager gets the stored bytes back verbatim - the archive as uploaded,
	// read from the store rather than from the request body.
	if f.devPackager.gotBytes != zipped(t, "jar bytes") {
		t.Errorf("packager got %d bytes, want the uploaded archive", len(f.devPackager.gotBytes))
	}
	// And the other target was untouched.
	if f.prodPackager.gotBytes != "" {
		t.Error("the prod packager ran during a dev deploy")
	}
}

func TestDeployRolledBackReports409(t *testing.T) {
	f := newFixtureWith(t, false, true)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "jar")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}

	got := decodeResult(t, rec)
	if got.Deployed {
		t.Error("Deployed = true, want false")
	}
	if !strings.Contains(got.Error, "health checks failed") {
		t.Errorf("Error = %q", got.Error)
	}
}

func TestDeployPackagingFailureReports502(t *testing.T) {
	f := newFixture(t)
	f.devPackager.err = errors.New("registry unauthorized")

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "jar")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if !strings.Contains(decodeResult(t, rec).Error, "registry unauthorized") {
		t.Errorf("body = %s", rec.Body)
	}
}

// A target configured without a pipeline cannot deploy, but must not crash.
func TestDeployTargetWithoutPipeline(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}

	srv := New(Options{
		Version: "test",
		Store:   st,
		Targets: map[string]TargetSpec{
			"ledger-only": {
				Config: target.Target{Token: devToken, MaxReplicas: 3, Retain: 100},
				Ledger: newLedger(t),
			},
		},
	})

	f := &fixture{srv: srv}
	rec := f.do(t, http.MethodPost, "/deploy/ledger-only", devToken, "jar")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code = %d, want 501", rec.Code)
	}
}

// -- replica count ------------------------------------------------------------

func TestDeployPassesReplicaCount(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?replicas=3", devToken, "jar")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	if len(f.devDeployer.replicas) != 1 {
		t.Fatalf("deployer called %d times, want 1", len(f.devDeployer.replicas))
	}
	got := f.devDeployer.replicas[0]
	if got == nil || *got != 3 {
		t.Errorf("replicas = %v, want 3", got)
	}

	// And it is reported back and recorded.
	res := decodeResult(t, rec)
	if res.Replicas == nil || *res.Replicas != 3 {
		t.Errorf("result replicas = %v, want 3", res.Replicas)
	}
}

func TestDeployWithoutReplicasLeavesCountAlone(t *testing.T) {
	f := newFixture(t)

	f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "jar")

	if len(f.devDeployer.replicas) != 1 {
		t.Fatalf("deployer called %d times, want 1", len(f.devDeployer.replicas))
	}
	if got := f.devDeployer.replicas[0]; got != nil {
		t.Errorf("replicas = %v, want nil so the count is left alone", *got)
	}
}

// maxReplicas is a per-target guard against a fat-fingered request.
func TestDeployRejectsTooManyReplicas(t *testing.T) {
	f := newFixture(t)

	// dev caps at 5.
	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?replicas=50", devToken, "jar")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "maxReplicas") {
		t.Errorf("body = %s, want it to name the cap", rec.Body)
	}
	// Nothing was stored or deployed.
	if len(f.devDeployer.replicas) != 0 {
		t.Error("the deploy proceeded despite an invalid replica count")
	}

	// prod caps at 20, so the same request is fine there.
	rec = f.do(t, http.MethodPost, "/deploy/prod-lobby?replicas=15", prodToken, "jar")
	if rec.Code != http.StatusOK {
		t.Errorf("prod code = %d, want 200 - 15 is under its cap of 20", rec.Code)
	}
}

func TestDeployRejectsNonNumericReplicas(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?replicas=lots", devToken, "jar")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

// -- per-target locking -------------------------------------------------------

// blockingFixture builds a server whose dev deploy blocks until released.
func blockingFixture(t *testing.T) (*fixture, chan struct{}, chan struct{}) {
	t.Helper()

	inFlight := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once
	blocking := func(ctx context.Context, imageRef string, replicas *int32) <-chan rollout.Event {
		ch := make(chan rollout.Event, 2)
		go func() {
			defer close(ch)
			once.Do(func() {
				close(inFlight)
				<-release
			})
			ch <- rollout.Event{Phase: rollout.PhaseSucceeded, Message: "deployed", At: time.Now()}
		}()
		return ch
	}

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}

	fastDeployer := &recordingDeployer{succeed: true}

	f := &fixture{
		devPackager:  &uniquePackager{prefix: "reg/dev"},
		prodPackager: &uniquePackager{prefix: "reg/prod"},
		prodDeployer: fastDeployer,
		devLedger:    newLedger(t),
		prodLedger:   newLedger(t),
	}

	f.srv = New(Options{
		Version: "test",
		Store:   st,
		Targets: map[string]TargetSpec{
			"dev-lobby": {
				Config:   target.Target{Token: devToken, MaxReplicas: 5, Retain: 100},
				Packager: f.devPackager,
				Deployer: blocking,
				Ledger:   f.devLedger,
			},
			"prod-lobby": {
				Config:   target.Target{Token: prodToken, MaxReplicas: 20, Retain: 100},
				Packager: f.prodPackager,
				Deployer: fastDeployer.deploy,
				Ledger:   f.prodLedger,
			},
		},
	})

	return f, inFlight, release
}

// Two deploys to the SAME target must not interleave - that corrupts rollback.
func TestConcurrentDeploysToOneTargetAreRefused(t *testing.T) {
	f, inFlight, release := blockingFixture(t)

	firstDone := make(chan int, 1)
	go func() {
		rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "first")
		firstDone <- rec.Code
	}()

	<-inFlight // the first deploy holds dev's lock

	second := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "second")
	if second.Code != http.StatusLocked {
		t.Errorf("code = %d, want 423", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the refusal")
	}

	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Errorf("first deploy got %d, want 200", code)
	}
}

// Deploys to DIFFERENT targets must proceed concurrently. A global lock would
// make one developer queue behind another's ten-minute rollout.
func TestConcurrentDeploysToDifferentTargetsProceed(t *testing.T) {
	f, inFlight, release := blockingFixture(t)

	devDone := make(chan int, 1)
	go func() {
		rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "dev jar")
		devDone <- rec.Code
	}()

	<-inFlight // dev is stuck mid-rollout

	// prod must not be blocked by it.
	done := make(chan int, 1)
	go func() {
		rec := f.do(t, http.MethodPost, "/deploy/prod-lobby", prodToken, "prod jar")
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("prod deploy got %d, want 200", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prod deploy blocked behind dev - the lock is global, not per target")
	}

	close(release)
	<-devDone
}

// The lock is released for the next deploy.
func TestLockIsReleasedAfterDeploy(t *testing.T) {
	f := newFixture(t)

	for i := range 3 {
		rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "jar")
		if rec.Code != http.StatusOK {
			t.Fatalf("deploy %d got %d, want 200", i, rec.Code)
		}
	}
}

// -- streaming ----------------------------------------------------------------

func TestStreamingDeploy(t *testing.T) {
	f := newFixture(t)

	rec := f.stream(t, "/deploy/dev-lobby?replicas=2", devToken, "jar")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != api.ContentTypeNDJSON {
		t.Errorf("Content-Type = %q", ct)
	}

	events, result, found := decodeStream(t, rec.Body.String())
	if len(events) == 0 {
		t.Error("no event lines")
	}
	if !found {
		t.Fatal("no result line")
	}
	if !result.Deployed {
		t.Error("Deployed = false")
	}
	if result.Target != "dev-lobby" {
		t.Errorf("Target = %q", result.Target)
	}
	if result.Replicas == nil || *result.Replicas != 2 {
		t.Errorf("Replicas = %v, want 2", result.Replicas)
	}
}

// A streamed failure is still HTTP 200; the outcome is in the final line.
func TestStreamingFailureIsStill200(t *testing.T) {
	f := newFixtureWith(t, false, true)

	rec := f.stream(t, "/deploy/dev-lobby", devToken, "jar")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 even for a failed deploy", rec.Code)
	}

	_, result, found := decodeStream(t, rec.Body.String())
	if !found {
		t.Fatal("no result line")
	}
	if result.Deployed {
		t.Error("Deployed = true, want false")
	}
	if !strings.Contains(result.Error, "health checks failed") {
		t.Errorf("Error = %q", result.Error)
	}
}

// Without the Accept header, the single-object reply is served.
func TestNonStreamingClientGetsOneObject(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "jar")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	got := decodeResult(t, rec)
	if len(got.Events) == 0 {
		t.Error("a non-streamed reply carries its events inline")
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty when not streamed", got.Kind)
	}
}

// -- ledger recording ---------------------------------------------------------

func TestDeployRecordsReplicasInTheLedger(t *testing.T) {
	f := newFixture(t)

	f.do(t, http.MethodPost, "/deploy/dev-lobby?version=2.0.0&by=cammy&replicas=3", devToken, "jar")

	entries := f.devLedger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.Version != "2.0.0" || e.By != "cammy" {
		t.Errorf("entry = %+v", e)
	}
	if e.Replicas == nil || *e.Replicas != 3 {
		t.Errorf("entry replicas = %v, want 3", e.Replicas)
	}
	if e.Target != "dev-lobby" {
		t.Errorf("entry target = %q", e.Target)
	}
}

// A failed deploy is still recorded: the audit trail says what was attempted.
func TestFailedDeployIsStillRecorded(t *testing.T) {
	f := newFixtureWith(t, false, true)

	f.do(t, http.MethodPost, "/deploy/dev-lobby?version=bad", devToken, "jar")

	if entries := f.devLedger.Entries(); len(entries) != 1 {
		t.Errorf("ledger has %d entries, want the failed attempt recorded", len(entries))
	}
}
