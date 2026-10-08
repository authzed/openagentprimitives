package authz

// AutoGrantPolicy names whose contributions to a thread may bind a slot without
// an approval. It governs the channel_thread fill source and nothing else.
type AutoGrantPolicy string

const (
	// AutoGrantOwner trusts only the session owner/requester's own messages.
	// The default, and the only defensible one for a thread the session does
	// not control: the person who invoked the agent vouches for what they
	// wrote, and everyone else's values route to an approval.
	AutoGrantOwner AutoGrantPolicy = "owner"
	// AutoGrantParticipants trusts any thread author. Only defensible for a
	// tightly-controlled channel — a thread is multi-author, so this makes
	// "someone posted a link" enough to make it reachable.
	AutoGrantParticipants AutoGrantPolicy = "participants"
	// AutoGrantNone trusts nobody: the thread may still be the SOURCE of
	// candidates, but every one of them routes to an approval.
	AutoGrantNone AutoGrantPolicy = "none"
)

// ThreadMessage is one prior message a seeder may take candidates from,
// reduced to the two facts the trust decision needs.
type ThreadMessage struct {
	// AuthorCanonical is the resolved identity of whoever wrote it. Empty when
	// the author could not be canonicalised, which is treated as untrusted.
	AuthorCanonical string
	Text            string
}

// ThreadSeedRequest is one slot's seeding inputs.
type ThreadSeedRequest struct {
	ResourceType string
	// Permission the grant confers. Part of the request because the grant tuple
	// is per-permission — a seed that omitted it could not name a relation.
	Permission string
	// ValueTransforms is the published chain from
	// AgentClass.status.resolvedSlots — the mapping a grant must reproduce.
	ValueTransforms []string
	// AutoGrantFrom is the slot's declared policy. Empty means owner.
	AutoGrantFrom []string
	// Occupancy and Rebind are the slot's single-vs-multi commitment and rebind
	// policy, carried onto every SlotBinding this seed emits so the GrantSlots
	// pinning gate sees them. Empty Occupancy reads as single.
	Occupancy string
	Rebind    string
}

// SeedFromThread selects the instances a thread may bind without an approval.
//
// The trust rule is the whole point. "Found in the thread" must never mean
// "anyone's value": a thread is multi-author, so scraping every message would
// let any participant make a target reachable by pasting it — an SSRF/exfil
// surface dressed up as convenience. autoGrantFrom names whose contributions
// count, and it defaults to the owner alone.
//
// Everything this does NOT return is not denied — it simply has no grant, so
// the tool's own Check fails and the ordinary approval flow runs. Under-seeding
// costs an approval; over-seeding grants reach nobody asked for.
//
// maxBindings caps what one thread can bind. Anything beyond the cap is
// reported in the returned dropped count rather than silently truncated, since
// a seeder that quietly stops reads as "the thread had nothing else".
func SeedFromThread(
	msgs []ThreadMessage,
	owner string,
	reqs []ThreadSeedRequest,
	maxBindings int,
) (bindings []SlotBinding, dropped int) {
	if len(msgs) == 0 || len(reqs) == 0 {
		return nil, 0
	}
	// Keyed on the grant-identifying triple rather than on SlotBinding itself:
	// SlotBinding carries a []precondition.Rule (unused on this path) and so is
	// no longer comparable. The triple is exactly what makes two bindings the
	// same grant here.
	seen := map[string]struct{}{}
	for _, req := range reqs {
		kind := ValueKindOf(req.ValueTransforms)
		if kind == ValueKindUnknown {
			// Nothing identifies the shape of this slot's values, so there is
			// no honest way to find them in text. Not seedable.
			continue
		}
		policy := autoGrantPolicyOf(req.AutoGrantFrom)
		for _, m := range msgs {
			if !trusts(policy, m.AuthorCanonical, owner) {
				continue
			}
			for _, raw := range ExtractCandidates(kind, m.Text) {
				id, err := NewObjectID(raw, req.ValueTransforms)
				if err != nil {
					// An unknown transform or an empty id names no instance.
					// Skip: a grant on "" cannot be revoked by id later.
					continue
				}
				b := SlotBinding{ResourceType: req.ResourceType, ResourceID: id, Permission: req.Permission, Occupancy: req.Occupancy, Rebind: req.Rebind}
				dedupKey := b.ResourceType + "\x00" + b.ResourceID.String() + "\x00" + b.Permission
				if _, dup := seen[dedupKey]; dup {
					continue
				}
				if maxBindings > 0 && len(bindings) >= maxBindings {
					dropped++
					continue
				}
				seen[dedupKey] = struct{}{}
				bindings = append(bindings, b)
			}
		}
	}
	return bindings, dropped
}

// autoGrantPolicyOf reads the declared list:
//
//	absent or empty  → owner. A slot that says nothing must not trust the room.
//	[none]           → none, and it WINS over anything else in the list.
//	[participants]   → participants.
//
// "Trust nobody" is the explicit value `none`, not an empty list. An empty list
// is indistinguishable from an absent one by the time it reaches here —
// omitempty drops it on the way through the API — so treating empty as "none"
// would be a rule that silently never fired, and the slot would get owner-trust
// while its YAML read as restrictive.
//
// The most restrictive declared value wins, and an unrecognised one falls back
// to owner rather than being ignored: a policy that cannot be read must never
// end up trusting more than the safe default.
func autoGrantPolicyOf(declared []string) AutoGrantPolicy {
	policy := AutoGrantOwner
	for _, d := range declared {
		switch AutoGrantPolicy(d) {
		case AutoGrantNone:
			return AutoGrantNone
		case AutoGrantParticipants:
			policy = AutoGrantParticipants
		}
	}
	return policy
}

// trusts reports whether this author's contribution may bind without approval.
func trusts(policy AutoGrantPolicy, author, owner string) bool {
	switch policy {
	case AutoGrantNone:
		return false
	case AutoGrantParticipants:
		// Still requires a resolved author: an unattributable message is not
		// "some participant", it is unknown provenance.
		return author != ""
	default: // AutoGrantOwner
		return author != "" && owner != "" && author == owner
	}
}
