// Package kube provides shared client-go construction for the oap CLI.
//
// The Scheme is loaded once and includes corev1, appsv1, rbacv1, apiextensions
// (for CRD CRUD), and agentprimitives.authzed.com/v1alpha1.
package kube

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Scheme is the global scheme used by all oap commands.
var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(apiextv1.AddToScheme(Scheme))
	utilruntime.Must(spiceboxv1alpha1.AddToScheme(Scheme))
	utilruntime.Must(gatewayv1.Install(Scheme))
}
