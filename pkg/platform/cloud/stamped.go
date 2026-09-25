package cloud

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ClusterKindEnvVar is the env var `oap install` stamps the resolved kind
	// into, on the operator and webd Deployments. Both binaries read it
	// fail-closed at startup, and Stamped reads it back host-side.
	//
	// It is ONE constant because the stamp and every reader of it must agree
	// byte for byte: a reader looking for a different spelling finds nothing
	// and concludes the cluster kind is unknown, which is indistinguishable
	// from a cluster installed before the stamp existed.
	ClusterKindEnvVar = "AP_CLUSTER_KIND"

	// OperatorDeploymentName is the operator's Deployment, in
	// WebdServiceNamespace. It carries the stamp Stamped reads.
	OperatorDeploymentName = "spicebox-operator"
)

// Stamped returns the Strategy for the kind `oap install` recorded on this
// cluster, read back from the operator Deployment's ClusterKindEnvVar.
//
// It is the counterpart to Detect, and it exists because Detect cannot answer
// this question: `local` and `desktop` report no providerID prefix — on purpose,
// so neither can ever be auto-selected onto durable infrastructure — so a node
// scan of a desktop cluster returns Default(), the kind with real ingress. Any
// host-side command that must know it is talking to a developer cluster
// (whether a tunnel may be opened, say) has to read what install decided rather
// than re-derive it.
//
// Every failure is returned, never softened into a fallback Strategy. "I could
// not tell which kind this is" and "this is the default kind" are different
// answers, and quietly returning the second is how a cluster ends up treated as
// production infrastructure — or as a developer box — by accident. Callers that
// can proceed without an answer say so at their own call site.
func Stamped(ctx context.Context, kc kubernetes.Interface) (Strategy, error) {
	dep, err := kc.AppsV1().Deployments(WebdServiceNamespace).Get(ctx, OperatorDeploymentName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Deployment %s/%s not found, so this cluster records no cluster kind — run `oap install` first",
				WebdServiceNamespace, OperatorDeploymentName)
		}
		return nil, fmt.Errorf("get Deployment %s/%s to read %s: %w",
			WebdServiceNamespace, OperatorDeploymentName, ClusterKindEnvVar, err)
	}

	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == ClusterKindEnvVar && e.Value != "" {
				return For(e.Value)
			}
		}
	}
	return nil, fmt.Errorf("Deployment %s/%s carries no %s: this cluster was installed before the "+
		"cluster kind was stamped, and re-running `oap install` records it",
		WebdServiceNamespace, OperatorDeploymentName, ClusterKindEnvVar)
}
