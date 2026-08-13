package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Glance-Studios/Lodestone/internal/rollout"
)

// The whole point of step 7's Target interface: this compiles only if the
// Kubernetes implementation actually satisfies it. Note that nothing in
// deployment.go imports the rollout package.
var _ rollout.Target = (*Deployment)(nil)

func int32p(n int32) *int32 { return &n }

// fixture builds a Deployment object as the API server would hold it.
func fixture(image string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paper-lobby",
			Namespace:  "game",
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32p(replicas),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sidecar", Image: "busybox:1"},
						{Name: "paper", Image: image},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			AvailableReplicas:  replicas,
		},
	}
}

// newTarget builds a Deployment target backed by client-go's fake clientset -
// the real Interface, serving in-memory objects. No cluster, no mocking library.
func newTarget(objs ...runtime.Object) *Deployment {
	return NewDeployment(fake.NewClientset(objs...), "game", "paper-lobby", "paper")
}

func TestCurrentReadsTheNamedContainer(t *testing.T) {
	d := newTarget(fixture("ghcr.io/x/paper@sha256:aaaa", 2))

	got, err := d.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	// Must read the "paper" container, not the sidecar listed before it.
	if want := "ghcr.io/x/paper@sha256:aaaa"; got != want {
		t.Errorf("Current() = %q, want %q", got, want)
	}
}

func TestCurrentMissingContainer(t *testing.T) {
	client := fake.NewSimpleClientset(fixture("img:1", 1))
	d := NewDeployment(client, "game", "paper-lobby", "nope")

	_, err := d.Current(context.Background())
	if !errors.Is(err, ErrContainerNotFound) {
		t.Errorf("Current() error = %v, want ErrContainerNotFound", err)
	}
}

func TestCurrentMissingDeployment(t *testing.T) {
	d := newTarget() // empty cluster

	if _, err := d.Current(context.Background()); err == nil {
		t.Error("Current() error = nil, want a not-found failure")
	}
}

func TestSetImagePatchesOnlyThatContainer(t *testing.T) {
	d := newTarget(fixture("ghcr.io/x/paper@sha256:aaaa", 2))

	const next = "ghcr.io/x/paper@sha256:bbbb"
	if err := d.SetImage(context.Background(), next); err != nil {
		t.Fatalf("SetImage() error = %v", err)
	}

	got, err := d.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != next {
		t.Errorf("after SetImage, Current() = %q, want %q", got, next)
	}

	// The sidecar must be untouched - that is what a strategic merge patch buys.
	dep, err := d.get(context.Background())
	if err != nil {
		t.Fatalf("get() error = %v", err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "sidecar" && c.Image != "busybox:1" {
			t.Errorf("sidecar image = %q, want it unchanged", c.Image)
		}
	}
}

func TestRollbackRestoresThePreviousImage(t *testing.T) {
	const original = "ghcr.io/x/paper@sha256:aaaa"
	d := newTarget(fixture(original, 2))

	if err := d.SetImage(context.Background(), "ghcr.io/x/paper@sha256:bbbb"); err != nil {
		t.Fatalf("SetImage() error = %v", err)
	}
	if err := d.Rollback(context.Background(), original); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	got, err := d.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != original {
		t.Errorf("after Rollback, Current() = %q, want %q", got, original)
	}
}

func TestRollbackWithNothingToRestore(t *testing.T) {
	d := newTarget(fixture("img:1", 1))

	if err := d.Rollback(context.Background(), ""); err == nil {
		t.Error("Rollback(\"\") error = nil, want a failure - there is nothing to land on")
	}
}

// -- settled() ---------------------------------------------------------------

func TestSettled(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*appsv1.Deployment)
		wantDone bool
		wantErr  error
	}{
		{
			name:     "all replicas updated and available",
			mutate:   func(d *appsv1.Deployment) {},
			wantDone: true,
		},
		{
			name: "still rolling out",
			mutate: func(d *appsv1.Deployment) {
				d.Status.UpdatedReplicas = 1 // of 3
			},
		},
		{
			name: "updated but not yet available",
			mutate: func(d *appsv1.Deployment) {
				d.Status.AvailableReplicas = 2 // of 3
			},
		},
		{
			name: "controller has not observed the patch yet",
			mutate: func(d *appsv1.Deployment) {
				d.Generation = 5 // spec moved on
			},
		},
		{
			// The requirement: a pod draining on SIGTERM must not look like a
			// stuck rollout. Status.Replicas stays high while old pods terminate,
			// and a Paper server saving worlds can take minutes over it.
			name: "old pods still terminating does not block",
			mutate: func(d *appsv1.Deployment) {
				d.Status.Replicas = 6 // 3 new + 3 draining
			},
			wantDone: true,
		},
		{
			// Scaling 3 -> 1: the one new pod is up, two old ones are draining.
			name: "scale down with draining pods is settled",
			mutate: func(d *appsv1.Deployment) {
				d.Spec.Replicas = int32p(1)
				d.Status.UpdatedReplicas = 1
				d.Status.AvailableReplicas = 1
				d.Status.Replicas = 3 // two still terminating
			},
			wantDone: true,
		},
		{
			// Scaling up is not settled until the new pods are actually available.
			name: "scale up not finished",
			mutate: func(d *appsv1.Deployment) {
				d.Spec.Replicas = int32p(5)
				d.Status.UpdatedReplicas = 5
				d.Status.AvailableReplicas = 3 // two not ready yet
				d.Status.Replicas = 5
			},
		},
		{
			name: "kubernetes gave up",
			mutate: func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type:    appsv1.DeploymentProgressing,
					Status:  corev1.ConditionFalse,
					Reason:  "ProgressDeadlineExceeded",
					Message: "ReplicaSet has timed out progressing",
				}}
			},
			wantErr: ErrProgressDeadlineExceeded,
		},
		{
			name: "progressing normally is not a failure",
			mutate: func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionTrue,
					Reason: "NewReplicaSetAvailable",
				}}
			},
			wantDone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := fixture("img:1", 3)
			tt.mutate(dep)

			done, err := settled(dep)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("settled() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("settled() error = %v, want nil", err)
			}
			if done != tt.wantDone {
				t.Errorf("settled() = %v, want %v", done, tt.wantDone)
			}
		})
	}
}

// A nil Spec.Replicas means the Kubernetes default of 1.
func TestSettledNilReplicasDefaultsToOne(t *testing.T) {
	dep := fixture("img:1", 1)
	dep.Spec.Replicas = nil
	dep.Status.Replicas = 1
	dep.Status.UpdatedReplicas = 1
	dep.Status.AvailableReplicas = 1

	done, err := settled(dep)
	if err != nil {
		t.Fatalf("settled() error = %v", err)
	}
	if !done {
		t.Error("settled() = false, want true - nil Replicas should mean 1")
	}
}

// WaitSettled must return immediately when the rollout is already complete,
// rather than blocking on a watch event that will never arrive.
func TestWaitSettledReturnsWhenAlreadyDone(t *testing.T) {
	d := newTarget(fixture("img:1", 2))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := d.WaitSettled(ctx); err != nil {
		t.Errorf("WaitSettled() error = %v, want nil", err)
	}
}

func TestWaitSettledFailsOnProgressDeadline(t *testing.T) {
	dep := fixture("img:1", 3)
	dep.Status.UpdatedReplicas = 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:    appsv1.DeploymentProgressing,
		Status:  corev1.ConditionFalse,
		Reason:  "ProgressDeadlineExceeded",
		Message: "ReplicaSet has timed out progressing",
	}}
	d := newTarget(dep)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := d.WaitSettled(ctx)
	if !errors.Is(err, ErrProgressDeadlineExceeded) {
		t.Errorf("WaitSettled() error = %v, want ErrProgressDeadlineExceeded", err)
	}
}

func TestWaitSettledRespectsContext(t *testing.T) {
	dep := fixture("img:1", 3)
	dep.Status.UpdatedReplicas = 1 // never completes
	d := newTarget(dep)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := d.WaitSettled(ctx)

	if err == nil {
		t.Fatal("WaitSettled() error = nil, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want it to give up when ctx expires", elapsed)
	}
}
