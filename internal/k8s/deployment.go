package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Deployment targets one container of one Kubernetes Deployment.
//
// It satisfies rollout.Target without importing the rollout package - the
// interface is declared by the consumer, so nothing here refers to it.
type Deployment struct {
	client    kubernetes.Interface
	namespace string
	name      string
	container string

	// mu guards previous, which SetImage records and Rollback restores.
	mu       sync.Mutex
	previous string
}

// NewDeployment returns a target for the named container in namespace/name.
func NewDeployment(client kubernetes.Interface, namespace, name, container string) *Deployment {
	return &Deployment{
		client:    client,
		namespace: namespace,
		name:      name,
		container: container,
	}
}

func (d *Deployment) Describe() string {
	return fmt.Sprintf("deployment %s/%s (container %s)", d.namespace, d.name, d.container)
}

// ErrContainerNotFound reports that the named container is not in the pod spec.
var ErrContainerNotFound = errors.New("container not found in deployment")

// Current returns the image the target container is presently running.
func (d *Deployment) Current(ctx context.Context) (string, error) {
	dep, err := d.get(ctx)
	if err != nil {
		return "", err
	}

	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == d.container {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("%s: %w", d.Describe(), ErrContainerNotFound)
}

// SetImage points the container at image, starting a rollout. It remembers the
// image being replaced so Rollback can restore it.
func (d *Deployment) SetImage(ctx context.Context, image string) error {
	previous, err := d.Current(ctx)
	if err != nil {
		return err
	}

	if err := d.patchImage(ctx, image); err != nil {
		return err
	}

	d.mu.Lock()
	d.previous = previous
	d.mu.Unlock()
	return nil
}

// Rollback restores the image that SetImage replaced.
//
// Deliberately not `kubectl rollout undo`: that walks the Deployment's
// ReplicaSet history to find a previous pod template, which is indirect and
// depends on revision history that may have been pruned. Lodestone already knows
// the exact digest it replaced, so it sets that back - the same mechanism as any
// other deploy, and unambiguous about what it lands on.
func (d *Deployment) Rollback(ctx context.Context) error {
	d.mu.Lock()
	previous := d.previous
	d.mu.Unlock()

	if previous == "" {
		return fmt.Errorf("%s: nothing to roll back to", d.Describe())
	}
	return d.patchImage(ctx, previous)
}

// patchImage sets the container's image with a strategic merge patch, so only
// that one field is sent and concurrent edits elsewhere are not clobbered.
func (d *Deployment) patchImage(ctx context.Context, image string) error {
	// Built with json.Marshal rather than string formatting, so an image
	// reference containing anything awkward cannot break the document.
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": d.container, "image": image},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build patch: %w", err)
	}

	_, err = d.client.AppsV1().Deployments(d.namespace).Patch(
		ctx, d.name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch %s to %s: %w", d.Describe(), image, err)
	}
	return nil
}

func (d *Deployment) get(ctx context.Context) (*appsv1.Deployment, error) {
	dep, err := d.client.AppsV1().Deployments(d.namespace).Get(ctx, d.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", d.Describe(), err)
	}
	return dep, nil
}

// ErrProgressDeadlineExceeded reports that Kubernetes gave up on the rollout.
var ErrProgressDeadlineExceeded = errors.New("progress deadline exceeded")

// WaitSettled blocks until every replica is updated and available, or the
// rollout fails, or ctx is done. This is what `kubectl rollout status` does.
func (d *Deployment) WaitSettled(ctx context.Context) error {
	// Check once before watching: a rollout that finished before we started
	// watching would otherwise wait for an event that never comes.
	dep, err := d.get(ctx)
	if err != nil {
		return err
	}
	if done, err := settled(dep); done || err != nil {
		return err
	}

	// A field selector so the API server sends only this Deployment's events.
	w, err := d.client.AppsV1().Deployments(d.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + d.name,
		ResourceVersion: dep.ResourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watch %s: %w", d.Describe(), err)
	}
	defer w.Stop() // release the connection however we leave

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", d.Describe(), ctx.Err())

		case event, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch %s: closed before the rollout settled", d.Describe())
			}
			if event.Type == watch.Error {
				return fmt.Errorf("watch %s: server reported an error", d.Describe())
			}

			dep, ok := event.Object.(*appsv1.Deployment)
			if !ok {
				continue // not a Deployment; ignore rather than fail
			}
			if done, err := settled(dep); done || err != nil {
				return err
			}
		}
	}
}

// settled reports whether the rollout has finished, or an error if it failed.
func settled(dep *appsv1.Deployment) (bool, error) {
	// A Progressing=False/ProgressDeadlineExceeded condition is Kubernetes
	// telling us it has given up. Detect it before declaring success.
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing &&
			c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return false, fmt.Errorf("%s/%s: %w: %s",
				dep.Namespace, dep.Name, ErrProgressDeadlineExceeded, c.Message)
		}
	}

	// Spec.Replicas is a *int32 and nil means the default of 1.
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}

	// ObservedGeneration lagging means the controller has not yet acted on our
	// patch, so the status still describes the previous spec.
	if dep.Status.ObservedGeneration < dep.Generation {
		return false, nil
	}

	return dep.Status.UpdatedReplicas == want &&
		dep.Status.Replicas == want &&
		dep.Status.AvailableReplicas == want, nil
}
