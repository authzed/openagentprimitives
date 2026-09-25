package cloud

import (
	"context"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// StatefulStorage decides the RWO block-storage class for the bundled stateful
// PVCs (Postgres data + Neo4j data) on a given cloud. Pure detection — no side
// effects. Sibling to WorkspaceStorage (which decides the RWX workspace class).
type StatefulStorage interface {
	Resolve(ctx context.Context, p StatefulParams) (StatefulDecision, error)
}

// StatefulParams carries the inputs StatefulStorage.Resolve needs.
type StatefulParams struct {
	Clients  Clients
	Reporter Reporter
}

// StatefulDecision is the per-cloud RWO-storage outcome. The side-effecting
// StorageClass create + provisioning probe are performed by the cmd/oap
// orchestration; Resolve itself is pure detection.
type StatefulDecision struct {
	// ClassName is the RWO StorageClass the bundled stateful PVCs should be
	// pinned to. "" means trust the cluster default StorageClass unchanged (the
	// portable default for every cloud but GKE-on-Hyperdisk-only nodes).
	ClassName string
	// CreateClass, when non-nil, is a StorageClass that Resolve determined is
	// absent and must be created before the stateful PVCs are applied. Its
	// .Name equals ClassName. nil when ClassName already exists (or is "").
	CreateClass *storagev1.StorageClass
	// Probe requests a provisioning probe against ClassName that waits for a
	// throwaway consumer pod to actually run (volume attached + mounted) — not
	// merely for the PVC to bind — before the real Postgres/Neo4j PVCs (whose
	// storageClassName is immutable) are applied.
	Probe bool
	// Message is a single loud info line explaining the decision (the detected
	// incompatibility + remediation), or "" when trusting the default.
	Message string
}

// NoopStatefulStorage trusts the cluster default StorageClass unchanged. It is
// the shared default for local/eks/aks (and the test fakeStrategy); only GKE
// overrides today.
type NoopStatefulStorage struct{}

// Resolve returns the trust-default decision (empty ClassName).
func (NoopStatefulStorage) Resolve(context.Context, StatefulParams) (StatefulDecision, error) {
	return StatefulDecision{}, nil
}

// DefaultStorageClass returns the cluster's default StorageClass (the one
// annotated storageclass.kubernetes.io/is-default-class="true"), or nil if no
// class is marked default. Generic across clouds; the cloud-specific predicates
// (does it provision PD? is the node pool Hyperdisk-only?) live in the cloud
// strategy that consumes this.
func DefaultStorageClass(ctx context.Context, kc kubernetes.Interface) (*storagev1.StorageClass, error) {
	scs, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range scs.Items {
		sc := &scs.Items[i]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc, nil
		}
	}
	return nil, nil
}
