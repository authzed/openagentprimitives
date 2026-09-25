// Package identityrefresh holds the RFC 6749 refresh-token policy shared by
// the AgentIdentity and UserIdentity refresh reconcilers: classify each
// type=oauth credential against its Secret, redeem the ones inside the
// expiry threshold, write the Refresh condition, and schedule the next
// reconcile.
//
// It exists because that policy was maintained as two ~370-line files whose
// only differences were the CR type and where the credential Secrets live —
// with 322 lines of tests on one side and 98 on the other, so a change made
// in one copy and forgotten in the other was caught by nothing. Same shape as
// the internal/skillspec body the Skill / ClusterSkill pair shares.
//
// The per-controller half that is genuinely NOT shared stays in the
// controller packages: SetupWithManager (distinct controller names, so
// leader election and metrics scope independently) and the Secret→identity
// watch mapping (a namespaced List for AgentIdentity, a cluster-wide List
// behind an IdentitiesNamespace pre-filter for UserIdentity).
package identityrefresh

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// Identity is the contract the refresh policy needs from an identity CR,
// satisfied by a thin adapter in each controller package over
// *v1alpha1.AgentIdentity and *v1alpha1.UserIdentity.
//
// The surface is deliberately minimal. Most of what looked scope-specific in
// the two copies is not: the adopt owner ref is ObjectKeyFromObject and the
// backoff key's namespace segment is GetNamespace() on BOTH kinds (empty for
// the cluster-scoped UserIdentity, which is what its copy hardcoded). The one
// irreducible difference is SecretNamespace.
type Identity interface {
	// Object returns the CR itself. Every client call — Get, MergeFrom,
	// Status().Patch — goes through it rather than through the adapter, so
	// nothing depends on how the adapter wraps the CR.
	Object() client.Object

	// Kind is the CR kind ("AgentIdentity" / "UserIdentity"). It is stamped
	// onto adopted Secrets as the owner kind, and lowercased for the
	// reconcile logger's key.
	Kind() string

	// SecretNamespace is where this identity's credential Secrets live: the
	// identity's own namespace for the namespaced AgentIdentity, the fixed
	// IdentitiesNamespace for the cluster-scoped UserIdentity.
	SecretNamespace() string

	// Credentials is spec.credentials.
	Credentials() []spiceboxv1alpha1.AgentCredential

	// ThresholdOverride is spec.refreshThreshold, nil when unset.
	ThresholdOverride() *metav1.Duration

	// ConditionType is the status condition this reconciler owns. Both kinds
	// spell it "Refresh" today; keeping it an accessor means neither package's
	// constant is assumed to equal the other's.
	ConditionType() string

	// StatusConditions is a pointer to status.conditions, for conditions.Set*.
	StatusConditions() *[]metav1.Condition

	// SetLastRefreshAt writes status.lastRefreshAt.
	SetLastRefreshAt(metav1.Time)

	// StatusSnapshot returns the status stanza by value, for semantic
	// comparison against a pre-mutation snapshot taken from DeepCopyIdentity.
	// Snapshotting the LIVE object is not enough: conditions.Set* mutates the
	// conditions slice in place, which a value copy would observe.
	StatusSnapshot() any

	// DeepCopyIdentity returns an independent copy, used both as the
	// MergeFrom patch base and as the source of the pre-mutation snapshot.
	DeepCopyIdentity() Identity
}

// ReferencesSecret reports whether any of creds is a REFRESHABLE credential
// whose backing Secret is secretName. It is the shared half of the two
// controllers' Secret→identity watch mappings.
//
// The narrowness is deliberate and preserved: this package exists for RFC 6749
// refresh, and a static or federated credential's Secret rotating is no reason
// to re-evaluate refresh eligibility. But "refreshable" is
// credkind.Kind.NeedsRefresh — the same predicate classify gates on — not
// "type=oauth". Reading c.OAuth directly asserted that those two are the same
// thing, which they are only while oauth is the sole refreshable kind. A
// second one would keep classify's gate (it dispatches properly) while this
// mapping silently stopped waking the identity when its Secret changed, so a
// rotated token would sit unnoticed until the next unrelated reconcile.
//
// Asking the registry keeps the set exactly as narrow as before and makes it
// track the predicate instead of shadowing it. An unregistered type answers
// no: it cannot be refreshable, and this function is a watch predicate with no
// error channel — classify logs the unknown type on the reconcile path.
func ReferencesSecret(creds []spiceboxv1alpha1.AgentCredential, secretName string) bool {
	for i := range creds {
		c := &creds[i]
		k, err := credkindregistry.Get(c.Type)
		if err != nil || !k.NeedsRefresh() {
			continue
		}
		if ref := k.SecretRef(*c); ref != nil && ref.Name == secretName {
			return true
		}
	}
	return false
}
