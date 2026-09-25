// pkg/authz/spicedb/lookup_resources.go — one round trip for "which resources may
// this subject act on?", the forward direction LookupInteractSubjects does
// not answer (that method walks a single resource's subjects, not a
// subject's resources). Built so a session list can ask SpiceDB once instead
// of issuing one fully-consistent CheckInteract per candidate row.
package spicedb

import (
	"context"
	"fmt"
	"io"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionRef is one agentsession named by its Kubernetes coordinates. It is
// what a SpiceDB agentsession object id decodes to: the id is composed as
// ns + "/" + name at every write and check site in this package, and neither
// half may contain '/', so the split is exact rather than heuristic.
type SessionRef struct {
	Namespace string
	Name      string
}

// InteractableSessions is LookupInteractableSessions' whole answer. Every
// field but Refs describes a way the answer is PARTIAL, and each is separate
// because the caller's remedy differs: Truncated needs a "showing the first N"
// line, Unrepresentable and Conditional need an operator log. Returning them
// as fields rather than swallowing them is what keeps a short list from
// reading as a complete one.
type InteractableSessions struct {
	// Refs are the sessions the subject holds interact on, in the order
	// SpiceDB streamed them. Never nil; empty means "none", which is a real
	// and legitimate answer (see the caller's empty-state handling).
	Refs []SessionRef
	// Truncated reports that the stream reached limit results, so Refs may be a
	// prefix rather than the whole set — "may", not "is": SpiceDB's stream
	// carries no "more results exist" signal at the limit, so a true result
	// count that happens to equal limit exactly also sets this flag even
	// though Refs is complete in that case. See lookupResources' truncated
	// computation for why over-reporting is the safe direction to be wrong in.
	Truncated bool
	// Unrepresentable holds object ids that are not "<ns>/<name>". An
	// agentsession id is composed inline as ns+"/"+name and is never validated
	// through this package's objectid.go, so a Kubernetes name containing '.'
	// (legal in DNS-1123, absent from SpiceDB's object-id grammar) is written
	// as a malformed id or rejected outright. Carried out rather than dropped
	// so the caller can log it against the viewer who could not see the row.
	Unrepresentable []string
	// Conditional holds ids SpiceDB answered CONDITIONAL_PERMISSION for — it
	// needed caveat context this call does not supply. agentsession#interact
	// resolves through no caveated relation today, so a non-empty Conditional
	// means the schema changed under this method. They are NOT in Refs: an
	// undetermined answer must never read as a grant.
	Conditional []string
}

// lookupResources streams every object of resType on which the given user
// holds permission. It is the SINGLE place this package asks SpiceDB a
// "which resources?" question, the mirror of check's role for "may this
// subject?", so a freshness floor, a cursor loop, or a metric has one site to
// change rather than one per caller.
//
// limit is passed as OptionalLimit; zero means "no server-side limit", which
// this package's callers must not use — an unbounded stream against a
// browser-facing page load is an availability problem, not a correctness one.
// Pagination via OptionalCursor is deliberately not implemented: nothing needs
// a second page yet, and a cursor loop that no caller drives is untested code
// on a security path.
func (c *Client) lookupResources(ctx context.Context, resType, permission string,
	canonicalID identity.CanonicalUserID, cons *v1.Consistency, limit uint32, errCtx string,
) (ids []string, conditional []string, truncated bool, err error) {
	stream, err := c.cl.LookupResources(ctx, &v1.LookupResourcesRequest{
		Consistency:        cons,
		ResourceObjectType: resType,
		Permission:         permission,
		Subject:            &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
		OptionalLimit:      limit,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("lookup %s: %w", errCtx, err)
	}
	// DEDUPLICATED as it streams. A permission that unions several relations —
	// agentsession#interact is `owner + started_by + participant - denied`,
	// agentclass#start_session is `starter + platform->start_session` — can
	// reach the same resource by more than one arm, and LookupResources emits
	// per reachable path, not per distinct resource. The ordinary case is not
	// exotic: a session you STARTED and also OWN matches two arms, so it
	// streams twice.
	//
	// Un-deduplicated, that surfaced as the same session listed twice in the
	// browser's sidebar, which reads as two sessions rather than one row drawn
	// twice. It also quietly corrupted two other things: the Get fan-out below
	// fetched the same object once per copy, and the truncation test — a count
	// against the limit — could report a short list as truncated purely
	// because of duplicates.
	//
	// Order is preserved (first sighting wins) because the caller renders in
	// stream order and SpiceDB's is stable for a given revision.
	seen := make(map[string]bool)
	seenConditional := make(map[string]bool)
	for {
		r, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, false, fmt.Errorf("lookup %s recv: %w", errCtx, rerr)
		}
		switch r.GetPermissionship() {
		case v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION:
			id := r.GetResourceObjectId()
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		default:
			// CONDITIONAL_PERMISSION means SpiceDB needs caveat context this call
			// does not supply; UNSPECIFIED means the server said nothing at all.
			// Both are undetermined answers rather than denials, and an
			// undetermined answer must never read as a grant, so both land here
			// instead of ids — there is no default branch that appends to ids.
			//
			// Deduplicated on the same reasoning as ids: this list drives a
			// COUNT the caller shows the viewer ("N sessions could not be
			// loaded"), and one resource reached twice is still one resource
			// they cannot see.
			id := r.GetResourceObjectId()
			if seenConditional[id] {
				continue
			}
			seenConditional[id] = true
			conditional = append(conditional, id)
		}
	}
	// An id can arrive BOTH ways — one arm of the permission grants it
	// outright while another is caveated — and a definite grant outranks an
	// undetermined one. Without this, such a resource would be listed as
	// available AND counted as unavailable in the same render.
	if len(conditional) > 0 && len(seen) > 0 {
		kept := conditional[:0]
		for _, id := range conditional {
			if seen[id] {
				continue
			}
			kept = append(kept, id)
		}
		conditional = kept
	}
	// SpiceDB closes the stream at OptionalLimit with no "more results still
	// exist" signal on the response, so equality between the returned count and
	// the limit is the only signal available for truncation. A result set whose
	// true size happens to equal the limit exactly also reports Truncated —
	// a false positive that over-discloses partiality, which is the safe
	// direction to be wrong in for a page that renders "showing the first N".
	truncated = limit > 0 && uint32(len(ids)+len(conditional)) >= limit
	return ids, conditional, truncated, nil
}

// LookupInteractableSessions answers "which agentsessions may this user
// interact with?" in one round trip, where the alternative is one
// CheckInteract per candidate session.
//
// fullyConsistent=false (MinimizeLatency) is right for a LIST: a row one
// refresh stale is cosmetic, and every door the list links to re-checks
// interact fully-consistently before serving anything. Bind it true when the
// answer is being used as a GATE — a stale positive would admit a subject
// whose access was just revoked.
//
// A session the subject may interact with but whose Kubernetes object is gone,
// and a Kubernetes AgentSession the subject may NOT interact with, are both
// invisible here by construction: this method reports the authorization graph
// and nothing else. Reconciling the two stores is the caller's job.
func (c *Client) LookupInteractableSessions(ctx context.Context,
	canonicalID identity.CanonicalUserID, limit uint32, fullyConsistent bool,
) (InteractableSessions, error) {
	ids, conditional, truncated, err := c.lookupResources(ctx, "agentsession", "interact",
		canonicalID, consistencyFor(fullyConsistent), limit, "interactable sessions")
	if err != nil {
		return InteractableSessions{}, err
	}

	out := InteractableSessions{
		// Non-nil so a caller ranging over Refs never has to distinguish "no
		// sessions" from "the field was never populated".
		Refs:        make([]SessionRef, 0, len(ids)),
		Truncated:   truncated,
		Conditional: conditional,
	}
	for _, id := range ids {
		// An agentsession object id is composed as ns+"/"+name at every write
		// site in this package (TouchStartedBy, CheckInteract, ...) without
		// going through objectid.go's validation, so an id that does not split
		// cleanly is a reachable production shape, not a defensive-only case.
		ns, name, found := strings.Cut(id, "/")
		if !found || ns == "" || name == "" {
			out.Unrepresentable = append(out.Unrepresentable, id)
			continue
		}
		out.Refs = append(out.Refs, SessionRef{Namespace: ns, Name: name})
	}
	return out, nil
}

// ClassRef is one agentclass named by its Kubernetes coordinates. Same
// decoding contract as SessionRef: the id is composed as ns + "/" + name (here
// through AgentClassObjectID, which additionally REFUSES a name SpiceDB cannot
// express), and neither half may contain '/', so the split is exact.
type ClassRef struct {
	Namespace string
	Name      string
}

// StartableClasses is LookupStartableClasses' whole answer. Same partiality
// contract as InteractableSessions: every field but Refs describes a way the
// answer is short, carried out rather than swallowed so a truncated set cannot
// read as a complete one.
type StartableClasses struct {
	// Refs are the classes the subject may start, in the order SpiceDB
	// streamed them. Never nil; empty means "none", which is the ordinary
	// answer for a viewer holding no platform grant.
	Refs []ClassRef
	// Truncated reports the stream reached limit results. See
	// InteractableSessions.Truncated for why this over-reports rather than
	// under-reports.
	Truncated bool
	// Unrepresentable holds object ids that are not "<ns>/<name>". Unlike
	// agentsession ids, agentclass ids ARE validated on the write side
	// (AgentClassObjectID), so this should be empty in a cluster whose tuples
	// were all written by this operator — a non-empty value means a
	// hand-written tuple or an older writer, and is worth an operator log
	// rather than a silent drop.
	Unrepresentable []string
	// Conditional holds ids SpiceDB answered CONDITIONAL_PERMISSION for.
	// agentclass#start_session resolves through no caveated relation today, so
	// a non-empty value means the schema changed under this method. NOT in
	// Refs: an undetermined answer must never read as a grant.
	Conditional []string
}

// LookupStartableClasses answers "which agentclasses may this user start a
// session of?" in one round trip, regardless of how many classes the cluster
// holds — the property that matters here, because the browser's session list
// re-derives this on every poll for every open tab.
//
// fullyConsistent MUST be true when the answer is used as a GATE. It is not a
// stylistic choice: the agentclass#platform link is written by the very
// reconcile that creates the class, so an admin who installs an agent and
// immediately opens the picker can land inside SpiceDB's quantization window
// and read a snapshot from before the tuple existed. A stale negative is
// indistinguishable from "not permitted", which is exactly the silent refusal
// the link exists to prevent. The LIST path may pass false — a picker entry one
// refresh stale is cosmetic, and the gate recomputes fully-consistently before
// anything is created.
//
// A class the subject may start whose Kubernetes object is gone, and a
// Kubernetes AgentClass the subject may NOT start, are both invisible here by
// construction: this method reports the authorization graph and nothing else.
// Intersecting it with what exists is the caller's job.
func (c *Client) LookupStartableClasses(ctx context.Context,
	canonicalID identity.CanonicalUserID, limit uint32, fullyConsistent bool,
) (StartableClasses, error) {
	ids, conditional, truncated, err := c.lookupResources(ctx, "agentclass", "start_session",
		canonicalID, consistencyFor(fullyConsistent), limit, "startable classes")
	if err != nil {
		return StartableClasses{}, err
	}

	out := StartableClasses{
		Refs:        make([]ClassRef, 0, len(ids)),
		Truncated:   truncated,
		Conditional: conditional,
	}
	for _, id := range ids {
		ns, name, found := strings.Cut(id, "/")
		if !found || ns == "" || name == "" {
			out.Unrepresentable = append(out.Unrepresentable, id)
			continue
		}
		out.Refs = append(out.Refs, ClassRef{Namespace: ns, Name: name})
	}
	return out, nil
}

// PersonalizableClasses is LookupPersonalizableClasses' whole answer. Every
// field but Refs describes a way the answer is PARTIAL, mirroring
// StartableClasses.
type PersonalizableClasses struct {
	// Refs are the classes the subject may personalize (has interacted with),
	// in the order SpiceDB streamed them. Never nil; empty means "none", which
	// is the ordinary answer for a user who has not interacted with any class.
	Refs []ClassRef
	// Truncated reports the stream reached limit results. See
	// StartableClasses.Truncated for why this over-reports rather than
	// under-reports.
	Truncated bool
	// Unrepresentable holds object ids that are not "<ns>/<name>". agentclass
	// ids ARE validated on the write side (AgentClassObjectID), so this should
	// be empty in a cluster whose tuples were all written by this operator.
	Unrepresentable []string
	// Conditional holds ids SpiceDB answered CONDITIONAL_PERMISSION for.
	// agentclass#can_personalize resolves through no caveated relation today,
	// so a non-empty value means the schema changed under this method.
	Conditional []string
}

// Names returns the personalizable class refs as a list of "<ns>/<name>" strings.
func (p PersonalizableClasses) Names() []string {
	out := make([]string, len(p.Refs))
	for i, ref := range p.Refs {
		out[i] = ref.Namespace + "/" + ref.Name
	}
	return out
}

// LookupPersonalizableClasses answers "which agentclasses has this user
// interacted with?" in one round trip. It enumerates classes where the user
// holds the can_personalize permission (granted by TouchInteractor writing
// the interactor relation).
//
// fullyConsistent MUST be true when the answer is used as a GATE. See
// LookupStartableClasses for reasoning.
//
// A class the subject has interacted with whose Kubernetes object is gone,
// and a Kubernetes AgentClass the subject has NOT interacted with, are both
// invisible here by construction: this method reports the authorization graph
// and nothing else. Intersecting it with what exists is the caller's job.
func (c *Client) LookupPersonalizableClasses(ctx context.Context,
	canonicalID identity.CanonicalUserID, limit uint32, fullyConsistent bool,
) (PersonalizableClasses, error) {
	ids, conditional, truncated, err := c.lookupResources(ctx, "agentclass", "can_personalize",
		canonicalID, consistencyFor(fullyConsistent), limit, "personalizable classes")
	if err != nil {
		return PersonalizableClasses{}, err
	}

	out := PersonalizableClasses{
		Refs:        make([]ClassRef, 0, len(ids)),
		Truncated:   truncated,
		Conditional: conditional,
	}
	for _, id := range ids {
		ns, name, found := strings.Cut(id, "/")
		if !found || ns == "" || name == "" {
			out.Unrepresentable = append(out.Unrepresentable, id)
			continue
		}
		out.Refs = append(out.Refs, ClassRef{Namespace: ns, Name: name})
	}
	return out, nil
}
