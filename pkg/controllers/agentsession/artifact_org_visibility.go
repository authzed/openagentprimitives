package agentsession

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// syncArtifactOrgViewer levels the agentsession#artifact_org_viewer wildcard
// tuple to what the session's class declares RIGHT NOW in
// spec.authz.session.artifactVisibility. Level-triggered on purpose: flipping
// the class to "organization" exposes the artifacts of sessions that already
// finished — those are exactly the ones people share after the fact — and
// flipping back revokes org-wide access everywhere, so the call site must sit
// above the terminal reap short-circuit in Reconcile (the same placement
// lesson reregisterMemoryToken learned; a hook below the reap runs never, not
// later, for a completed session).
//
// A missing class levels to disabled: no class, no opt-in evidence — deleting
// the class that declared "organization" is a revocation, and a widening
// grant must not outlive its declaration. Any other class-read error, and any
// syncer error, fails the reconcile: an unapplied TOUCH is a grant the user
// believes exists, an unapplied delete is a revocation still live, and both
// need the retry, not a log line.
func (r *Reconciler) syncArtifactOrgViewer(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if r.OrgViewerSyncer == nil {
		return nil // SpiceDB disabled — there is no tuple plane to level
	}
	enabled := false
	var ac spiceboxv1alpha1.AgentClass
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac)
	switch {
	case err == nil:
		enabled = ac.Spec.GetAuthz().GetSession().OrgWideArtifactVisibility()
	case errors.IsNotFound(err):
		// enabled stays false: level to closed.
	default:
		return fmt.Errorf("sync artifact org viewer: get AgentClass %s/%s: %w", sess.Namespace, sess.Spec.Class, err)
	}
	ref := authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
	if err := authz.SyncArtifactOrgViewer(ctx, r.OrgViewerSyncer, ref, enabled); err != nil {
		return fmt.Errorf("sync artifact org viewer for %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return nil
}
