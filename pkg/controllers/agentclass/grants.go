package agentclass

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessiongrants,verbs=get;list;watch;create;update

// writeAgentSessionGrants extracts the union of (resourceType, permission)
// from every resolved tool's Permission.Check across the AgentClass's
// referenced MCPServers and writes (or updates) the owned
// "<className>-grants" AgentSessionGrants CR. Idempotent: spec.pairs is
// dedup'd and sorted.
//
// AgentClass owns the per-class declaration of needed grant pairs; the
// guardian controller observes all such CRs cluster-wide and composes them
// into a single agentsession schema block.
func (r *Reconciler) writeAgentSessionGrants(
	ctx context.Context,
	cls *spiceboxv1alpha1.AgentClass,
	mcpServers []spiceboxv1alpha1.MCPServer,
) error {
	pairs := extractGrantPairs(mcpServers)
	slots := extractSlotPairs(cls)

	name := cls.Name + "-grants"
	desired := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cls.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
					Kind:               "AgentClass",
					Name:               cls.Name,
					UID:                cls.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{Pairs: pairs, Slots: slots},
	}

	var existing spiceboxv1alpha1.AgentSessionGrants
	err := r.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: cls.Namespace}, &existing)
	switch {
	case errors.IsNotFound(err):
		if err := r.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("create AgentSessionGrants/%s: %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("get AgentSessionGrants/%s: %w", name, err)
	default:
		if pairsEqual(existing.Spec.Pairs, pairs) && pairsEqual(existing.Spec.Slots, slots) {
			return nil // no change
		}
		cp := existing.DeepCopy()
		cp.Spec.Pairs = pairs
		cp.Spec.Slots = slots
		if err := r.Client.Update(ctx, cp); err != nil {
			return fmt.Errorf("update AgentSessionGrants/%s: %w", name, err)
		}
	}
	return nil
}

// extractGrantPairs returns the dedup'd + sorted (resourceType, permission)
// tuples from every tool in mcpServers whose Permission.Check is set.
// Tools without a Permission block, or with a Permission whose StateImpact
// is stateless/passthrough (no Check), contribute nothing.
//
// Slice-4: PermissionVariants[].Check also contributes pairs. Each variant
// is an alternate Permission selected at dispatch by a CEL predicate; the
// guardian schema composer needs the union of every potentially-checked
// (resourceType, permission) pair so the AgentSession schema declares the
// matching grant_<perm>_<resType> relation for each one. Without this,
// variant-routed approvals would target a relation that doesn't exist on
// the per-session resource.
func extractGrantPairs(mcpServers []spiceboxv1alpha1.MCPServer) []spiceboxv1alpha1.GrantPair {
	var pairs []spiceboxv1alpha1.GrantPair
	for _, srv := range mcpServers {
		for _, tool := range srv.Spec.Tools {
			if tool.Permission != nil && tool.Permission.Check != nil {
				pairs = append(pairs, spiceboxv1alpha1.GrantPair{
					ResourceType: tool.Permission.Check.ResourceType,
					Permission:   tool.Permission.Check.Permission,
				})
			}
			for _, v := range tool.PermissionVariants {
				if v.Check.Check == nil {
					continue // stateless/passthrough variant; no resource to grant against
				}
				pairs = append(pairs, spiceboxv1alpha1.GrantPair{
					ResourceType: v.Check.Check.ResourceType,
					Permission:   v.Check.Check.Permission,
				})
			}
		}
	}
	return dedupGrantPairs(pairs)
}

// dedupGrantPairs uniquifies pairs and sorts them by (ResourceType,
// Permission) for deterministic spec output. Mirrors
// pkg/authz/guardian/schema.DedupAndSort but operates on the v1alpha1 GrantPair
// type (not the guardian/schema one) to keep agentclass off the
// guardian-internal import graph.
func dedupGrantPairs(pairs []spiceboxv1alpha1.GrantPair) []spiceboxv1alpha1.GrantPair {
	seen := make(map[spiceboxv1alpha1.GrantPair]struct{}, len(pairs))
	out := make([]spiceboxv1alpha1.GrantPair, 0, len(pairs))
	for _, p := range pairs {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		return out[i].Permission < out[j].Permission
	})
	return out
}

func pairsEqual(a, b []spiceboxv1alpha1.GrantPair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// extractSlotPairs returns the dedup'd + sorted (resourceType, permission)
// tuples this class declares through authz.slots.
//
// Kept separate from extractGrantPairs even though the tuple shape is identical:
// a grant pair asks the composer for a relation on the SESSION pointing at the
// resource, a slot pair for one on the RESOURCE pointing at the session. Merging
// them would erase the direction, which is the thing that decides whether the
// resulting Check evaluates the actual requester or needs a wildcard to pass.
func extractSlotPairs(cls *spiceboxv1alpha1.AgentClass) []spiceboxv1alpha1.GrantPair {
	slots := cls.Spec.GetSlots()
	if len(slots) == 0 {
		return nil
	}
	pairs := make([]spiceboxv1alpha1.GrantPair, 0, len(slots))
	for _, s := range slots {
		if s.ResourceType == "" {
			continue
		}
		// ONE pair per declared permission, not one per slot.
		//
		// The composer emits a slot_grant_<permission> relation per pair, and a
		// grant can only be written for a relation the schema actually carries.
		// A slot was a single (type, permission) pair, so approving anything else
		// on that type — an amendment adding `read` to a slot declared for
		// `push` — bound a relation the schema never emitted and failed at
		// SpiceDB with FailedPrecondition, leaving the call to escalate again on
		// its next attempt. Forever.
		//
		// EffectivePermissions is the CEILING, not a grant: emitting the relation
		// makes a grant for it writable; whether one is written still depends on
		// what an approval actually named.
		for _, perm := range s.EffectivePermissions() {
			pairs = append(pairs, spiceboxv1alpha1.GrantPair{
				ResourceType: s.ResourceType,
				Permission:   perm,
			})
		}
	}
	return dedupGrantPairs(pairs)
}
