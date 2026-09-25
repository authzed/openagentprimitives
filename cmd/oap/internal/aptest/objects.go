package aptest

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NodeWithProviderID is the single-node cluster the registry-derivation tests
// read: only spec.providerID matters, since that is what a cloud strategy
// turns into a registry root.
func NodeWithProviderID(providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// NodeWithCapacity is the node the sizing questions read: 2 CPU and the given
// allocatable memory.
func NodeWithCapacity(name, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

// OperatorDeployment is the installed operator Deployment carrying image, the
// object commands read to learn which registry an install came from.
func OperatorDeployment(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: apcmd.OperatorDeployment, Namespace: apcmd.SystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: image}}},
			},
		},
	}
}
