package cloud

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// ValidateParams carries what Strategy.Validate needs to compare the chosen
// kind against the cluster actually connected.
type ValidateParams struct {
	Clients     Clients
	RESTConfig  *rest.Config
	ContextName string
	// AllowOverride is --allow-non-local-cluster. It relaxes ONLY `local`'s
	// kubeconfig host-pattern refusal, for the one case that check cannot tell
	// apart from a real remote cluster: a corporate dev cluster reached over a
	// private VPN, whose kubeconfig host looks non-local.
	//
	// It does NOT relax any managed-cloud refusal (unmanaged's or the big
	// three's ValidateManagedPrefix). No flag and no explicit --cluster-kind
	// waives those: installing a generic profile on a cluster whose nodes
	// identify a managed cloud would silently skip that cloud's storage,
	// artifact, and Gateway handling.
	AllowOverride bool
}

// DetectedManagedKey returns the key of the managed cloud whose providerID
// prefix the cluster's nodes carry, or "" if none does. Shared by every kind's
// Validate. A node-list failure yields ("", err) — the caller decides whether
// that is fatal.
func DetectedManagedKey(ctx context.Context, p ValidateParams) (string, error) {
	if p.Clients.Typed == nil {
		return "", nil // nothing to inspect; not an inconsistency
	}
	nodes, err := p.Clients.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 5})
	if err != nil {
		return "", fmt.Errorf("list nodes to validate cluster kind: %w", err)
	}
	for i := range nodes.Items {
		id := nodes.Items[i].Spec.ProviderID
		for key, s := range registry.byKey {
			if pref := s.ProviderIDPrefix(); pref != "" && strings.HasPrefix(id, pref) {
				return key, nil
			}
		}
	}
	return "", nil
}

// ValidateManagedPrefix is the shared Validate for the big-three managed kinds:
// the cluster's nodes must actually carry this kind's providerID prefix.
func ValidateManagedPrefix(ctx context.Context, s Strategy, p ValidateParams) error {
	got, err := DetectedManagedKey(ctx, p)
	if err != nil {
		return err
	}
	if got == "" || got == s.Key() {
		return nil
	}
	return fmt.Errorf("--cluster-kind=%s but this cluster's nodes report %s: pass --cluster-kind=%s, or omit the flag to detect it", s.Key(), got, got)
}
