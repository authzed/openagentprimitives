package wait

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ForDeployment polls until the named Deployment has at least wantReady ready
// replicas, or the parent context deadline elapses.
func ForDeployment(ctx context.Context, typed kubernetes.Interface, namespace, name string, wantReady int32) error {
	return Until(ctx, 2*time.Second, 2*time.Minute, func(ctx context.Context) (bool, error) {
		d, err := typed.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
		}
		return d.Status.ReadyReplicas >= wantReady, nil
	})
}
