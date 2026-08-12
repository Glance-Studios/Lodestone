// Package k8s implements rollout.Target against a Kubernetes Deployment.
package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClientset builds a Kubernetes client. An empty kubeconfig path means
// "in-cluster if possible, otherwise the usual local rules" - so the same binary
// works as a pod and on a laptop with no code change.
func NewClientset(kubeconfig string) (*kubernetes.Clientset, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return cs, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	// Running inside the cluster: the service account token is mounted into the
	// pod and this is the only correct source.
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}

	// Otherwise defer to the standard rules: an explicit path, then $KUBECONFIG,
	// then ~/.kube/config. clientcmd already implements all of that.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}
