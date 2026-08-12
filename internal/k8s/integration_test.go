package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Glance-Studios/Lodestone/internal/health"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
)

// These tests wire the real rollout engine to the real Kubernetes client code,
// against a fake API server. Everything except the network is genuine.

func TestRolloutDeploysToKubernetes(t *testing.T) {
	const (
		before = "ghcr.io/x/paper@sha256:aaaa"
		after  = "ghcr.io/x/paper@sha256:bbbb"
	)
	target := newTarget(fixture(before, 2))

	res := rollout.Collect(rollout.Deploy(context.Background(), target, after, rollout.Options{
		SettleTimeout: 5 * time.Second,
	}))

	if !res.Succeeded() {
		t.Fatalf("deploy failed: %v", res.Err)
	}

	got, err := target.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != after {
		t.Errorf("deployment is on %q, want %q", got, after)
	}
}

// The rollout stalls, so the engine must roll the Deployment back to the image
// it started on - the whole promise of the tool.
func TestRolloutRollsBackAStalledDeployment(t *testing.T) {
	const (
		before = "ghcr.io/x/paper@sha256:aaaa"
		broken = "ghcr.io/x/paper@sha256:dead"
	)

	dep := fixture(before, 3)
	// Kubernetes has given up on this Deployment.
	dep.Status.UpdatedReplicas = 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:    appsv1.DeploymentProgressing,
		Status:  "False",
		Reason:  "ProgressDeadlineExceeded",
		Message: "ReplicaSet \"paper-lobby-7d9f\" has timed out progressing",
	}}
	target := newTarget(dep)

	res := rollout.Collect(rollout.Deploy(context.Background(), target, broken, rollout.Options{
		SettleTimeout: 5 * time.Second,
	}))

	if res.Succeeded() {
		t.Fatal("deploy succeeded, want failure")
	}

	// Back on the original image.
	got, err := target.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != before {
		t.Errorf("deployment left on %q, want it rolled back to %q", got, before)
	}

	// And the reason survived the whole way up.
	if !strings.Contains(res.Err.Error(), "timed out progressing") {
		t.Errorf("err = %v, want the Kubernetes message", res.Err)
	}
}

// The rollout settles but the app never becomes healthy: also a rollback.
func TestRolloutRollsBackOnFailedHealthCheck(t *testing.T) {
	const (
		before = "ghcr.io/x/paper@sha256:aaaa"
		next   = "ghcr.io/x/paper@sha256:cccc"
	)
	target := newTarget(fixture(before, 2))

	opts := rollout.Options{
		SettleTimeout: 5 * time.Second,
		// Nothing is listening here, so the gate can never pass.
		Checks:         []health.Check{health.TCPCheck{Addr: "127.0.0.1:1", Timeout: time.Second}},
		HealthTimeout:  200 * time.Millisecond,
		HealthInterval: 50 * time.Millisecond,
	}
	res := rollout.Collect(rollout.Deploy(context.Background(), target, next, opts))

	if res.Succeeded() {
		t.Fatal("deploy succeeded, want a health failure")
	}

	got, err := target.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != before {
		t.Errorf("deployment left on %q, want it rolled back to %q", got, before)
	}
}
