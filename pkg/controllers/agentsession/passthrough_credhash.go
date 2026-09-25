// pkg/controllers/agentsession/passthrough_credhash.go
//
// Per-credential content hashing for userPassthrough projected credentials.
// The operator hashes each projected credential value and stores the map on
// AgentSession.status.passthroughCredHashes. On the next reconcile it diffs
// the freshly-projected hashes against the stored map to detect a re-linked
// (replaced) or removed credential and emit a per-session `credential`
// invalidation on the oap.revocation bus. Added credentials are recorded
// without emitting (first-seen); no running consumer holds a stale cache for
// a brand-new credential.
package agentsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// credHashes returns a per-key SHA-256 hex digest of each value in data.
func credHashes(data map[string]string) map[string]string {
	out := make(map[string]string, len(data))
	for k, v := range data {
		sum := sha256.Sum256([]byte(v))
		out[k] = hex.EncodeToString(sum[:])
	}
	return out
}

// diffCredHashes compares the previously-observed hashes against the freshly-
// computed ones. changed lists credentials whose hash differs (replaced) or
// that vanished (removed); removed is true if any key vanished. Newly-added
// keys are intentionally excluded — a first-seen credential has no stale
// consumer cache to invalidate.
func diffCredHashes(prev, next map[string]string) (changed []string, removed bool) {
	for name, h := range next {
		if ph, ok := prev[name]; ok && ph != h {
			changed = append(changed, name) // replaced
		}
	}
	for name := range prev {
		if _, ok := next[name]; !ok {
			changed = append(changed, name) // removed
			removed = true
		}
	}
	return changed, removed
}

// credentialRevokeKind matches credential.Invalidator.Kind() ("credential"),
// the registered revocation.Invalidator that drops the broker's cached token.
const credentialRevokeKind = "credential"

// applyPassthroughCredInvalidation diffs the freshly-projected per-credential
// hashes (next) against the last-observed set on status and, on a change,
// emits ONE per-session `credential` invalidation on the oap.revocation bus so
// running consumers drop their cached token. It sets sess.Status.
// PassthroughCredHashes = next on success (and on prime/no-change/add-only).
//
// Error posture (spec §4): a REMOVAL whose emit fails is fail-closed — the
// status is NOT advanced (so the next reconcile re-detects and re-emits) and
// an error is returned to requeue. A REPLACE/ADD whose emit fails is best-
// effort — logged, status advanced (a restart re-resolves regardless).
func (r *Reconciler) applyPassthroughCredInvalidation(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, next map[string]string) error {
	logger := log.FromContext(ctx).WithName("passthrough-cred-invalidation").
		WithValues("session", sess.Namespace+"/"+sess.Name)

	prev := sess.Status.PassthroughCredHashes
	if prev == nil {
		sess.Status.PassthroughCredHashes = next // prime: no consumer holds a stale cache yet
		return nil
	}
	changed, removed := diffCredHashes(prev, next)
	if len(changed) == 0 {
		sess.Status.PassthroughCredHashes = next // records adds; nothing to invalidate
		return nil
	}

	emitErr := r.emitPassthroughInvalidate(ctx, sess)
	logger.Info("passthrough credentials changed",
		"changed", changed, "removed", removed, "emitErr", errStr(emitErr))

	if emitErr != nil && removed {
		// Fail-closed: keep prev so the next reconcile re-detects the removal.
		return fmt.Errorf("passthrough credential removal invalidation failed (will retry): %w", emitErr)
	}
	sess.Status.PassthroughCredHashes = next
	return nil
}

// emitPassthroughInvalidate emits one Secret-level credential invalidation
// keyed on the per-session projected Secret — the exact key the broker,
// mcpBroker, and MCPTools cache passthrough creds under. Dropping the whole
// per-session Secret re-resolves all co-located creds on the next call
// (harmless: it just re-reads current bytes). Emitted CLUSTER-WIDE (scope "")
// because a userPassthrough runner's subscriber namespace is the session
// namespace, so a scope-gated emit could be dropped before invalidation.
func (r *Reconciler) emitPassthroughInvalidate(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	key := sess.Namespace + "/" + spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name)
	return r.RevokePublisher.Emit(ctx, credentialRevokeKind, key, "")
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
