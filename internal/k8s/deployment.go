package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
//
// Deliberately stateless: every method's inputs come from its arguments or the
// cluster. A remembered "previous image" field would be shared by concurrent
// deploys, and the second would overwrite what the first needed to undo.
type Deployment struct {
	client    kubernetes.Interface
	namespace string
	name      string
	container string
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

// SetImage points the container at image, starting a rollout.
func (d *Deployment) SetImage(ctx context.Context, image string) error {
	return d.patch(ctx, image, nil)
}

// SetImageAndReplicas points the container at image and scales to replicas in a
// single patch, so the rollout the controller starts already knows both. Two
// separate patches would begin one rollout and then supersede it, which reads as
// a stuck deploy while it happens.
func (d *Deployment) SetImageAndReplicas(ctx context.Context, image string, replicas int32) error {
	return d.patch(ctx, image, &replicas)
}

// Replicas reports the target's current desired replica count.
func (d *Deployment) Replicas(ctx context.Context) (int32, error) {
	dep, err := d.get(ctx)
	if err != nil {
		return 0, err
	}
	if dep.Spec.Replicas == nil {
		return 1, nil // nil means the Kubernetes default
	}
	return *dep.Spec.Replicas, nil
}

// Rollback points the container back at toImage, and at toReplicas when it is
// not nil.
//
// Deliberately not `kubectl rollout undo`: that walks the Deployment's
// ReplicaSet history to find a previous pod template, which is indirect and
// depends on revision history that may have been pruned. The caller already
// knows the exact image it replaced, so it hands that back - the same mechanism
// as any other deploy, and unambiguous about what it lands on.
//
// A nil toReplicas leaves the count alone: the caller only passes one when the
// deploy being undone had changed it.
func (d *Deployment) Rollback(ctx context.Context, toImage string, toReplicas *int32) error {
	if toImage == "" {
		return fmt.Errorf("%s: no image to roll back to", d.Describe())
	}
	return d.patch(ctx, toImage, toReplicas)
}

// patch sets the container's image, and optionally the replica count, with a
// strategic merge patch - so only those fields are sent and concurrent edits
// elsewhere are not clobbered.
func (d *Deployment) patch(ctx context.Context, image string, replicas *int32) error {
	spec := map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"containers": []map[string]any{
					{"name": d.container, "image": image},
				},
			},
		},
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}

	// Built with json.Marshal rather than string formatting, so an image
	// reference containing anything awkward cannot break the document.
	patch, err := json.Marshal(map[string]any{"spec": spec})
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
//
// "Finished" means the desired number of *new* pods are up and available. It
// deliberately does not wait for old pods to disappear.
//
// Status.Replicas counts terminating pods, so requiring it to equal the desired
// count would block for the whole drain - and a workload that saves state on
// SIGTERM can legitimately take minutes. Waiting would burn the settle timeout
// and roll back a perfectly good deploy. Scaling down makes this routine rather
// than rare, so the check is on updated-and-available only.
//
// The trade: success can be reported while old pods are still draining. That is
// the right call for a deploy tool - the new version is live at full strength,
// and the health gate confirms it separately.
func settled(dep *appsv1.Deployment) (bool, error) {
	// A Progressing=False/ProgressDeadlineExceeded condition is Kubernetes
	// telling us it has given up. That is the authoritative failure signal, and
	// the only one - our own timeout means "we stopped watching", not "it broke".
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
		dep.Status.AvailableReplicas == want, nil
}
