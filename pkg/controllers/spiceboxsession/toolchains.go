package spiceboxsession

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolchainaudit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/resolve"
)

// errToolchainMissing and errToolchainNotValid alias the shared resolver's
// sentinels so the stderrors.Is switch in controller.go keeps compiling
// unchanged.
var (
	errToolchainMissing  = resolve.ErrToolchainMissing
	errToolchainNotValid = resolve.ErrToolchainNotValid
)

// resolveToolchains delegates to the shared resolver (pkg/tools/toolchain/resolve),
// which is a pure function of the class — its resolution is not session-scoped,
// only its storage is (frozen onto session status at bind so a catalog edit
// can't mutate a running pod).
func resolveToolchains(ctx context.Context, c client.Client, names []string) ([]spiceboxv1alpha1.ToolchainMount, string, error) {
	return resolve.Resolve(ctx, c, names)
}

// recordResolved appends the operator's `resolved` attestation: what actually
// got materialized, with frozen digests. Called only after the freeze is durably
// persisted, so a failed status write cannot leave an entry claiming a pod ran
// with toolchains it never got.
//
// Audit failure is logged, never fatal: the session is already correct, and
// refusing to start it would trade a real workload for a missing log line. The
// entry is signed transparently by the operator's SigningMemory wrapper.
func (r *Reconciler) recordResolved(ctx context.Context, sess *spiceboxv1alpha1.SpiceboxSession) {
	if r.AuditMemory == nil {
		log.FromContext(ctx).Info("toolchain audit skipped: no AuditMemory wired",
			"session", sess.Namespace+"/"+sess.Name)
		return
	}
	refs := make([]toolchainaudit.ResolvedRef, 0, len(sess.Status.ResolvedToolchains))
	for _, m := range sess.Status.ResolvedToolchains {
		refs = append(refs, toolchainaudit.ResolvedRef{Name: m.Name, Image: m.Image})
	}
	// Attribute to the parent AgentSession scope when this is a bundle session;
	// standalone SpiceboxSessions attest under their own name.
	scopeID := sess.Namespace + "/" + sess.Name
	if parent := sess.Labels["agentprimitives.authzed.com/agentsession"]; parent != "" {
		scopeID = sess.Namespace + "/" + parent
	}
	// The facade's capability door denies any caller that arrives without an
	// approval, and a reconcile context carries none of its own. The entry is
	// append-only, so the write also seeds the operator's hash chain for the
	// scope, which reads it; both halves need the mint. Because the audit
	// write is non-fatal, a denial here would cost nothing visible and simply
	// leave the attestation missing from every session's chain.
	ctx = memory.WithSystemApproval(ctx, "operator:spiceboxsession-controller")
	if err := toolchainaudit.Record(ctx, r.AuditMemory,
		memory.Scope{Kind: "session", ID: scopeID},
		toolchainaudit.Content{
			Phase:     "resolved",
			Resolved:  refs,
			SetDigest: sess.Status.ToolchainSetDigest,
			Bundle:    sess.Labels["agentprimitives.authzed.com/agentbundle"],
			Session:   sess.Name,
		}); err != nil {
		log.FromContext(ctx).Info("toolchainaudit.Record failed",
			"session", scopeID, "err", err.Error())
	}
}
