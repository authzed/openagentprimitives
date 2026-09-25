package authz

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Granter is the SpiceDB-side dependency Touch* wraps. Implemented by
// *spicedb.Client; abstracted for tests.
// Every method is an idempotent TOUCH write, safe to repeat. An error means the
// tuple may not exist, so the caller must not proceed as though the standing it
// grants is in place.
type Granter interface {
	// TouchStartedBy writes agentsession#started_by@user:<canonicalID>, naming
	// the session's originator. Confers interact, NOT fork or approve.
	TouchStartedBy(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error

	// TouchOwner writes agentsession#owner@<subjectRef>, where subjectRef is
	// "type:id" or a subject set "type:id#relation". Ownership is what confers
	// approve and fork standing.
	TouchOwner(ctx context.Context, ns, name, subjectRef string) error

	// TouchInteractParticipant writes agentsession#participant@<subject>, where
	// subject is a subject-SET expression admitting a whole group.
	TouchInteractParticipant(ctx context.Context, ns, name, subject string) error

	// TouchInteractParticipantUser writes
	// agentsession#participant@user:<canonicalID> for one concrete user.
	TouchInteractParticipantUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error

	// TouchDeniedUser writes agentsession#denied@user:<canonicalID>, the
	// blocklist tuple that overrides any interact standing the user holds.
	TouchDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error

	// TouchInteractor writes agentclass:<classNS>/<className>#interactor@<subjectRef>,
	// recording that subjectRef has interacted with a session of this class —
	// the enumeration source for the App Home preferences pane
	// (agentclass#can_personalize = interactor).
	TouchInteractor(ctx context.Context, classNS, className, subjectRef string) error
}

// ArtifactOrgViewerSyncer levels the opt-in org-wide artifact audience —
// agentsession#artifact_org_viewer@user:* — to enabled: an idempotent TOUCH
// when true, an idempotent filtered delete when false. Implemented by
// *spicedb.Client. Kept separate from Granter for the same reason
// DeniedLister is: Granter's contract is TOUCH-only, and widening it with a
// delete would ripple into every Granter fake.
type ArtifactOrgViewerSyncer interface {
	// SyncArtifactOrgViewer levels the wildcard tuple on agentsession
	// <ns>/<name>. An error means the level may not be in place — for
	// enabled=false that is a revocation still live, so callers must retry,
	// not shrug.
	SyncArtifactOrgViewer(ctx context.Context, ns, name string, enabled bool) error
}

// SyncArtifactOrgViewer levels the session's org-wide artifact audience via
// the syncer. No-op when s is nil (SpiceDB disabled).
func SyncArtifactOrgViewer(ctx context.Context, s ArtifactOrgViewerSyncer, scope SessionRef, enabled bool) error {
	if s == nil {
		return nil
	}
	return s.SyncArtifactOrgViewer(ctx, scope.Namespace, scope.Name, enabled)
}

// DeniedLister reads the users on a session's agentsession#denied relation.
// Implemented by *spicedb.Client; kept separate from Granter (a write-only
// interface many fakes implement) so widening the read side does not ripple
// into every Granter fake.
type DeniedLister interface {
	// ListDeniedUsers returns the canonical user ids on agentsession#denied for
	// ns/name. An error must abort whatever consumes the list — CopyDeniedUsers
	// fails the fork rather than materialize a child with a shorter blocklist.
	ListDeniedUsers(ctx context.Context, ns, name string) ([]string, error)
}

// CopyDeniedUsers copies every agentsession#denied@user tuple from src to
// dst. This is load-bearing for the fork/inherit path: a child session's
// interact permission grants view over the inherited transcript, so a user
// the parent's owner denied must stay denied on the child — otherwise the
// fork silently re-grants them read access to the very history they were
// blocked from. Errors are returned (never swallowed) so the fork aborts
// fail-closed rather than materializing a child with a weaker blocklist.
func CopyDeniedUsers(ctx context.Context, l DeniedLister, g Granter, src, dst SessionRef) error {
	if l == nil || g == nil {
		return fmt.Errorf("CopyDeniedUsers: nil lister or granter (fail-closed: refusing to fork without copying the denied blocklist)")
	}
	denied, err := l.ListDeniedUsers(ctx, src.Namespace, src.Name)
	if err != nil {
		return fmt.Errorf("CopyDeniedUsers: list parent denied: %w", err)
	}
	for _, canonicalID := range denied {
		// Read back out of SpiceDB, where the platform itself wrote it: the id
		// was already canonicalized when the deny was recorded, and copying a
		// blocklist forward proves nothing new about who it names.
		denyID := identity.CanonicalFromTrusted(canonicalID, "read back from this session's own SpiceDB denied list")
		if err := g.TouchDeniedUser(ctx, dst.Namespace, dst.Name, denyID); err != nil {
			return fmt.Errorf("CopyDeniedUsers: touch child denied %q: %w", canonicalID, err)
		}
	}
	return nil
}

// TouchStartedBy writes the agentsession#started_by relation.
func TouchStartedBy(ctx context.Context, g Granter, scope SessionRef, canonicalID identity.CanonicalUserID) error {
	if g == nil {
		return nil
	}
	return g.TouchStartedBy(ctx, scope.Namespace, scope.Name, canonicalID)
}

// TouchOwner writes agentsession#owner@<subjectRef> via the Granter. subjectRef
// is "objType:objId" or "objType:objId#relation" (e.g. "user:abc",
// "group:eng#member"). No-op when g is nil (SpiceDB disabled).
func TouchOwner(ctx context.Context, g Granter, scope SessionRef, subjectRef string) error {
	if g == nil {
		return nil
	}
	return g.TouchOwner(ctx, scope.Namespace, scope.Name, subjectRef)
}

// TouchInteractor records that subjectRef has interacted with the class,
// idempotently — the enumeration source for the App Home preferences pane.
// Writes agentclass:<classNS>/<className>#interactor@<subjectRef>. A nil
// Granter is a no-op (test/degraded paths), matching TouchOwner.
func TouchInteractor(ctx context.Context, g Granter, classNS, className, subjectRef string) error {
	if g == nil {
		return nil
	}
	return g.TouchInteractor(ctx, classNS, className, subjectRef)
}

// TouchInteractParticipant writes an agentsession#interact subject-set relation.
func TouchInteractParticipant(ctx context.Context, g Granter, scope SessionRef, subject string) error {
	if g == nil {
		return nil
	}
	return g.TouchInteractParticipant(ctx, scope.Namespace, scope.Name, subject)
}

// TouchInteractParticipantUser writes agentsession#participant@user:<canonicalID>.
// Distinct from TouchInteractParticipant which takes a subject-set
// expression (e.g. "group:eng#member").
func TouchInteractParticipantUser(ctx context.Context, g Granter, scope SessionRef, canonicalID identity.CanonicalUserID) error {
	if g == nil {
		return nil
	}
	return g.TouchInteractParticipantUser(ctx, scope.Namespace, scope.Name, canonicalID)
}

// TouchDeniedUser writes the agentsession#denied relation.
func TouchDeniedUser(ctx context.Context, g Granter, scope SessionRef, canonicalID identity.CanonicalUserID) error {
	if g == nil {
		return nil
	}
	return g.TouchDeniedUser(ctx, scope.Namespace, scope.Name, canonicalID)
}

// RelWriter is the SpiceDB-side dependency Grant / Revoke wrap.
type RelWriter interface {
	// WriteRelationships batch-writes rels with TOUCH semantics, so a repeated
	// grant is a no-op rather than a conflict.
	WriteRelationships(ctx context.Context, rels []Relation) error

	// DeleteRelationships batch-deletes rels. An error means the access may
	// still be live, which is why revocation callers surface it rather than
	// treating the revoke as done.
	DeleteRelationships(ctx context.Context, rels []Relation) error
}

// Grant writes the supplied relations to SpiceDB.
func Grant(ctx context.Context, w RelWriter, rels []Relation) error {
	if w == nil || len(rels) == 0 {
		return nil
	}
	return w.WriteRelationships(ctx, rels)
}

// Revoke deletes the supplied relations from SpiceDB.
func Revoke(ctx context.Context, w RelWriter, rels []Relation) error {
	if w == nil || len(rels) == 0 {
		return nil
	}
	return w.DeleteRelationships(ctx, rels)
}
