// Package cloud is the cloud-aware install layer for agentprimitives. It
// exposes a Strategy registry keyed on cloud provider (gke/eks/aks/default)
// and a set of shared abstractions (Clients, Reporter, TLSStrategy,
// WorkspaceStorage) that decouple per-cloud logic from cmd/oap's CLI
// orchestration.
//
// pkg/platform/cloud depends only on client-go, controller-runtime, and standard libs.
// It MUST NOT import anything under cmd/oap/internal/.
package cloud

import (
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Clients is the set of cluster clients pkg/platform/cloud needs. cmd/oap constructs it
// from its install bundle; pkg/platform/cloud depends only on standard client interfaces
// (never on cmd/oap/internal/kube).
type Clients struct {
	REST    *rest.Config
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
	Ctrl    client.Client
}
