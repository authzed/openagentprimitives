package v1alpha1

import "fmt"

// OwnerSource names HOW a session's owner was resolved. It is the difference
// between "this person owns the session" and "everyone in that channel owns the
// session", which is not inferable from the subject string alone — a subject set
// may be a two-person group or an entire Slack channel.
type OwnerSource string

const (
	OwnerSourcePassthrough OwnerSource = "passthrough"
	// OwnerSourceCeiling: the class's ownerCeiling.fixed pinned the owner.
	// Distinct from Explicit so a surface describing ownership to a human can
	// say the ADMIN pinned it rather than implying the Channel chose it.
	OwnerSourceCeiling       OwnerSource = "ceiling"
	OwnerSourceExplicit      OwnerSource = "explicit"
	OwnerSourceStarter       OwnerSource = "starter"
	OwnerSourcePermission    OwnerSource = "permission"
	OwnerSourceOutputChannel OwnerSource = "outputChannel"
)

// ResolveOwnerSubject decides the SpiceDB subject that owns a session, and
// reports which rule decided it.
//
// Precedence (highest first):
//  1. identityMode=userPassthrough → starter (forced; error if absent)
//  2. policy.Explicit → explicit override
//  3. starter != "" → channel-provided starting user
//  4. policy.Ownerless.Permission → permission subject-set
//  5. outputGroupRef != "" → the output channel's membership
//  6. nothing resolvable → error (fail-closed)
//
// The ORDER is what makes channel ownership narrower than it first appears:
// starter outranks both ownerless sources, so on a channel where the kind
// attributes messages to a user (Slack), the human who summoned the agent owns
// the session and rule 5 never fires. It applies to kinds with no per-user
// attribution — which is exactly the case it was added for.
//
// Rule 5 is DERIVED, not declared. It fires for a policy that asked for it
// (spec.owner.ownerless.fromOutputChannel) and equally for a Channel that
// declared no owner at all — the two are indistinguishable by the time control
// reaches it, because every declared source has already returned above. The
// caller decides whether there is anything to derive from by supplying (or
// withholding) outputGroupRef.
//
// Shared by the operator (which writes the SpiceDB tuple) and by anything that
// needs to DESCRIBE the ownership to a human. Those must not drift: a channel
// telling people "everyone here owns me" while the operator wrote an individual
// owner is worse than saying nothing.
func ResolveOwnerSubject(identityMode string, policy *ChannelOwnerPolicy, starter, outputGroupRef string, ceiling *OwnerCeiling) (string, OwnerSource, error) {
	if identityMode == IdentityModeUserPassthrough {
		if starter == "" {
			return "", "", fmt.Errorf("owner: passthrough session has no starting user")
		}
		// Passthrough outranks even a fixed ceiling: the session ACTS as this
		// user, so naming anyone else as owner would describe a session that
		// does not exist.
		return starter, OwnerSourcePassthrough, nil
	}
	// The ceiling is the ADMIN's decision and outranks anything a Channel
	// says — including a Channel that says nothing, which is the default.
	//
	// It used to be enforced only by refusing a CONFLICTING Channel config,
	// and that refusal is guarded on the policy being non-nil with a non-empty
	// explicit owner. A Channel declaring no owner block satisfied neither
	// guard, so resolution fell through to "starter is owner" and the pin was
	// silently dropped — on a class an admin had pinned precisely so that no
	// individual could self-approve.
	if ceiling != nil && ceiling.Fixed != "" {
		return ceiling.Fixed, OwnerSourceCeiling, nil
	}
	if policy != nil && policy.Explicit != "" {
		return policy.Explicit, OwnerSourceExplicit, nil
	}
	if starter != "" {
		return starter, OwnerSourceStarter, nil
	}
	// starterOnly admits exactly one owner: the starting user. There is none,
	// so there is no owner this ceiling permits — and falling through to a
	// permission subject-set or the output channel's membership would hand the
	// session the collective owner starterOnly exists to forbid.
	if ceiling != nil && ceiling.StarterOnly {
		return "", "", fmt.Errorf("owner: ownerCeiling is starterOnly and this session has no starting user")
	}
	if policy != nil && policy.Ownerless != nil && policy.Ownerless.Permission != "" {
		return policy.Ownerless.Permission, OwnerSourcePermission, nil
	}
	// Last resort, and the only owner source that needs nothing per-install
	// written down: the people in the channel this agent's output lands in.
	// Reached with a policy that declared fromOutputChannel, and equally with
	// one that declared nothing at all — every rule above has already returned,
	// so anything an operator DID declare has already won, and what remains is
	// a Channel with no owner of its own on a kind that names no starter.
	//
	// Gated by whether the CALLER supplied a ref rather than by the flag: only
	// the caller can resolve which Channel is the output one and ask its kind
	// for a membership, and a caller that must not derive (an ownerCeiling
	// veto — see OwnerMayComeFromOutputChannel) withholds it.
	if outputGroupRef != "" {
		return outputGroupRef, OwnerSourceOutputChannel, nil
	}
	return "", "", fmt.Errorf("owner: no resolvable owner (no starter, no explicit, no ownerless source)")
}

// OwnerMayComeFromOutputChannel reports whether a class with this ownerCeiling
// permits a session's owner to be taken from the output channel's membership.
//
// The ceiling is the admin's veto over what a Channel may declare, and
// ResolveOwnerSubject never sees it: the ceiling is enforced by refusing the
// CONFIG (the Channel controller rejects an explicit/ownerless policy under
// starterOnly), not at resolve time. A derived owner is never written into a
// Channel's spec, so it would slip past that refusal entirely — starterOnly
// would silently acquire a whole-channel owner, which is the precise thing it
// exists to forbid. Hence this gate, consulted by every caller that would
// supply an outputGroupRef.
//
//   - starterOnly: the owner must resolve to the session's starting user, and a
//     session that has no starter has no owner this ceiling admits.
//   - fixed: the owner is pinned to a subject the admin named. Deriving a
//     different one would override that; deriving the pinned one is a
//     derivation nobody asked for. Neither: the Channel must say so itself.
//
// A nil ceiling (the common case) permits it.
func OwnerMayComeFromOutputChannel(ceiling *OwnerCeiling) bool {
	if ceiling == nil {
		return true
	}
	return !ceiling.StarterOnly && ceiling.Fixed == ""
}

// IsCollectiveOwnership reports whether the owner is a POPULATION rather than a
// single person — the case a channel must state out loud, because the people
// affected did not individually agree to it and cannot see the SpiceDB tuple.
//
// Both ownerless sources qualify. A permission subject-set is a group by
// construction, and an output-channel group is every member of that channel,
// growing silently as people join.
func IsCollectiveOwnership(src OwnerSource) bool {
	return src == OwnerSourcePermission || src == OwnerSourceOutputChannel
}
