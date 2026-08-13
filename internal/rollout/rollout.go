// Package rollout drives a health-gated deployment and rolls back on failure.
//
// It knows nothing about Kubernetes. Target is the seam: k8s becomes one
// implementation at step 8, and everything here is testable without a cluster.
package rollout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/health"
)

// Target is something that can be pointed at a new image and observed.
type Target interface {
	// Describe names the target for logs.
	Describe() string

	// Current returns the digest the target is presently running.
	Current(ctx context.Context) (string, error)

	// Replicas returns the target's present desired replica count.
	Replicas(ctx context.Context) (int32, error)

	// SetImage points the target at digest, starting a rollout.
	SetImage(ctx context.Context, digest string) error

	// SetImageAndReplicas does the same and scales to replicas at once, so the
	// controller starts a single rollout that already knows both.
	SetImageAndReplicas(ctx context.Context, digest string, replicas int32) error

	// WaitSettled blocks until the rollout finishes or ctx is done. It reports
	// an error if the rollout failed or timed out.
	WaitSettled(ctx context.Context) error

	// Rollback points the target back at toImage, and at toReplicas when it is
	// not nil.
	//
	// What to restore is passed in rather than remembered by the target, so
	// implementations hold no rollback state. State on the target would be shared
	// between concurrent deploys, and the second would overwrite the first one's
	// memory of what to undo.
	Rollback(ctx context.Context, toImage string, toReplicas *int32) error
}

// Phase is where a rollout has reached.
type Phase string

const (
	PhaseStarting   Phase = "starting"
	PhaseUpdating   Phase = "updating"
	PhaseSettling   Phase = "settling"
	PhaseChecking   Phase = "checking"
	PhaseSucceeded  Phase = "succeeded"
	PhaseRollingBck Phase = "rolling_back"
	PhaseFailed     Phase = "failed"
)

// Event is one step of progress, streamed as the rollout proceeds.
type Event struct {
	Phase   Phase     `json:"phase"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	Err     string    `json:"error,omitempty"`
}

// Options tunes a deployment. The zero value is usable.
type Options struct {
	// SettleTimeout bounds waiting for the rollout to finish. Zero uses 5m.
	SettleTimeout time.Duration

	// HealthTimeout bounds waiting for the checks to pass. Zero uses 2m.
	HealthTimeout time.Duration

	// HealthInterval is how often to re-probe. Zero uses 3s.
	HealthInterval time.Duration

	// Checks gate the rollout. Empty means the rollout succeeds once settled.
	Checks []health.Check

	// Replicas scales the target as part of this deploy. Nil leaves the count
	// alone, which is the common case; a developer choosing 1 for a UI change or
	// 3 for load testing sets it.
	Replicas *int32
}

func (o Options) settleTimeout() time.Duration {
	if o.SettleTimeout <= 0 {
		return 5 * time.Minute
	}
	return o.SettleTimeout
}

func (o Options) healthTimeout() time.Duration {
	if o.HealthTimeout <= 0 {
		return 2 * time.Minute
	}
	return o.HealthTimeout
}

func (o Options) healthInterval() time.Duration {
	if o.HealthInterval <= 0 {
		return 3 * time.Second
	}
	return o.HealthInterval
}

// ErrRolledBack reports that a deployment failed and the target was returned to
// its previous revision.
var ErrRolledBack = errors.New("rolled back")

// eventBuffer must exceed the most events one deployment can emit (currently 6:
// starting, updating, settling, checking, rolling_back, failed). Sized this way,
// a send never blocks, so the rollout cannot stall on a slow or absent consumer
// and no event is ever dropped.
const eventBuffer = 16

// Deploy points target at digest, waits for it to settle, gates on health, and
// rolls back if any of that fails.
//
// Events are sent on the returned channel, which is closed when the deployment
// finishes. Read it for live progress, or hand it to Collect for the outcome.
func Deploy(ctx context.Context, target Target, digest string, opts Options) <-chan Event {
	events := make(chan Event, eventBuffer)

	go func() {
		defer close(events)
		run(ctx, target, digest, opts, events)
	}()

	return events
}

// run performs the deployment, emitting events as it goes.
func run(ctx context.Context, target Target, digest string, opts Options, events chan<- Event) {
	// A plain send, deliberately: the buffer is sized so this cannot block, and
	// selecting on ctx.Done() here would randomly drop the terminal event when
	// the caller cancels - losing the very report the caller needs.
	emit := func(phase Phase, msg string, err error) {
		e := Event{Phase: phase, Message: msg, At: time.Now().UTC()}
		if err != nil {
			e.Err = err.Error()
		}
		events <- e
	}

	emit(PhaseStarting, fmt.Sprintf("deploying %s to %s", digest, target.Describe()), nil)

	// Remember where we were, so a rollback has something to aim at and the
	// event log records what was replaced.
	previous, err := target.Current(ctx)
	if err != nil {
		emit(PhaseFailed, "could not read current image", err)
		return
	}
	// Redeploying the digest already running is a no-op - unless a replica count
	// was asked for, because "same jar, three instances for load testing" is a
	// real request and must not be short-circuited away. Patching an unchanged
	// spec does not bump the generation, so the redundant case settles at once.
	if previous == digest && opts.Replicas == nil {
		emit(PhaseSucceeded, "already running "+digest, nil)
		return
	}

	// A rollback reverts exactly the fields this deploy set. So the previous
	// replica count is only worth reading - and only worth restoring - when the
	// deploy is about to change it. Leaving a count alone that we never touched
	// is not an omission; undoing someone else's capacity decision would be.
	var previousReplicas *int32

	if opts.Replicas != nil {
		n, err := target.Replicas(ctx)
		if err != nil {
			emit(PhaseFailed, "could not read current replica count", err)
			return
		}
		previousReplicas = &n

		emit(PhaseUpdating, fmt.Sprintf("replacing %s, scaling %d -> %d", previous, n, *opts.Replicas), nil)
		err = target.SetImageAndReplicas(ctx, digest, *opts.Replicas)
		if err != nil {
			emit(PhaseFailed, "could not set image", err)
			return
		}
	} else {
		emit(PhaseUpdating, fmt.Sprintf("replacing %s", previous), nil)
		if err := target.SetImage(ctx, digest); err != nil {
			emit(PhaseFailed, "could not set image", err)
			return
		}
	}

	// From here a failure means something is half-deployed, so every exit path
	// below rolls back.
	emit(PhaseSettling, "waiting for the rollout to settle", nil)

	settleCtx, cancelSettle := context.WithTimeout(ctx, opts.settleTimeout())
	err = target.WaitSettled(settleCtx)
	cancelSettle()
	if err != nil {
		rollBack(ctx, target, previous, previousReplicas, emit, "rollout did not settle", err)
		return
	}

	if len(opts.Checks) > 0 {
		emit(PhaseChecking, fmt.Sprintf("gating on %d health check(s)", len(opts.Checks)), nil)

		healthCtx, cancelHealth := context.WithTimeout(ctx, opts.healthTimeout())
		err = health.WaitFor(healthCtx, opts.Checks, opts.healthInterval())
		cancelHealth()
		if err != nil {
			rollBack(ctx, target, previous, previousReplicas, emit, "health checks failed", err)
			return
		}
	}

	emit(PhaseSucceeded, "deployed "+digest, nil)
}

// rollBack reverts exactly what this deploy set: the image always, and the
// replica count only if the deploy changed it.
//
// previousReplicas is nil when the deploy left the count alone, and the count is
// then left alone on the way out too - undoing a capacity decision nobody made
// as part of this deploy would be the wrong kind of helpful.
func rollBack(ctx context.Context, target Target, previous string, previousReplicas *int32, emit func(Phase, string, error), why string, cause error) {
	emit(PhaseRollingBck, why, cause)

	// Roll back even if ctx is already done - the caller cancelling must not
	// leave a broken revision live. A fresh, short context gives it a chance.
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	// Read the count before restoring, so the report can state both ends of it.
	current, replicasErr := target.Replicas(rbCtx)

	if err := target.Rollback(rbCtx, previous, previousReplicas); err != nil {
		emit(PhaseFailed, "rollback failed: "+why, errors.Join(cause, err))
		return
	}

	emit(PhaseFailed, why+"; "+replicaNote(current, previousReplicas, replicasErr),
		errors.Join(cause, ErrRolledBack))
}

// replicaNote states what happened to the replica count, either way. Silence
// about a count left at 2 is how an operator ends up with a degraded target and
// no idea there is anything to undo.
func replicaNote(current int32, restoredTo *int32, readErr error) string {
	if restoredTo == nil {
		if readErr != nil {
			return "replicas left alone (not set by this deploy)"
		}
		return fmt.Sprintf("replicas left at %d (not set by this deploy)", current)
	}
	if readErr != nil {
		return fmt.Sprintf("replicas restored to %d", *restoredTo)
	}
	return fmt.Sprintf("replicas restored %d -> %d", current, *restoredTo)
}
