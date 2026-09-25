package agentsession

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// reregisterMemoryToken re-installs this session's memory-API bearer token in
// the operator's in-process token registry, reading the value back out of the
// per-session Secret that already holds it.
//
// WHY it exists. tokens.Registry is process memory — plain maps, nothing
// rehydrates them at startup — while the <session>-memory-token Secret and
// every client holding its value are durable. The authoritative registration
// (step 4 of Reconcile) sits ~700 lines below several short-circuits, and one
// of them is permanent: a terminal session's pod reap returns before step 4 on
// EVERY subsequent reconcile, forever. So a restarted operator re-reconciled a
// Succeeded session, reaped, and never re-registered its token; the Secret and
// the CLI still presented it and the memory API answered a bare 401. A finished
// session's transcript — which the audit log keeps permanently, on purpose —
// became unreadable with no way back short of deleting the session.
//
// WHY on every reconcile rather than at one repaired call site. The reap is the
// permanent case, not the only one: the restart fork, the boot-failure
// transition, the AgentClass gate, the settings gate and the AgentIdentity
// requeue all return above step 4 too. Restoring here — first thing after the
// deletion check — covers all of them at once, and is the same bargain the
// audit public key beside step 4 already takes: read what is durable, install
// it cheaply and idempotently, so a restarted operator restores what it never
// minted.
//
// Three properties keep this a RESTORATION and not a widening:
//
//   - It never mints. A session whose Secret is gone, or whose Secret carries
//     no token, gets nothing — minting stays on the create path in Reconcile,
//     which a terminal session cannot reach. Restoring access is in scope;
//     manufacturing a new credential for a dead session is not.
//   - The extra scopes come from status.bundleSessions — the same field step 4
//     reads, pinned against the runner at admission — so the restored token
//     authorizes exactly the paths it authorized before the restart. The
//     operator stops rebuilding that list once a session is terminal, so there
//     is nothing newer to pick up.
//   - It cannot resurrect a REVOKED token. Revoke has exactly one caller,
//     finalize, and Reconcile routes to finalize on a non-zero
//     deletionTimestamp before reaching here. An informer cache never regresses
//     past a resourceVersion it has already served, and the finalize that
//     revoked read the deletion-stamped object from that same cache — so no
//     later reconcile of that key can see the session as live again.
//
// The registry lookup comes first so the steady state costs one RLock and a map
// hit: the Secret is read only when this process does not already know the
// session, which after the first pass is only true following a restart.
func (r *Reconciler) reregisterMemoryToken(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) {
	// Tests that exercise unrelated reconcile paths do not always wire a
	// registry. Step 4's Set is deliberately left unguarded, so an operator
	// genuinely running without one still fails loudly on the live path rather
	// than losing every registration to a silent skip here.
	if r.Tokens == nil {
		return
	}
	key := memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
	if r.Tokens.Registered(key) {
		return
	}
	secretName := MemoryTokenSecretName(sess)
	sec, err := r.getSecret(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: secretName})
	if err != nil {
		// NotFound is the ordinary state of a session whose Secret has not been
		// minted yet (first reconcile) — there is nothing to restore and nothing
		// to report. Any other read failure is a real one: the session's memory
		// stays unreadable until a later reconcile succeeds, so say so.
		if !errors.IsNotFound(err) {
			log.FromContext(ctx).Info("memory token: reading the per-session Secret failed; this session's memory stays unreadable until a later reconcile restores its token",
				"session", sess.Namespace+"/"+sess.Name, "secret", secretName, "err", err.Error())
		}
		return
	}
	token := string(sec.Data[agentSessionSecretMemoryToken])
	if token == "" {
		log.FromContext(ctx).Info("memory token: the per-session Secret carries no token; the memory API will refuse every bearer presented for this session",
			"session", sess.Namespace+"/"+sess.Name, "secret", secretName, "key", agentSessionSecretMemoryToken)
		return
	}
	extras := make([]memory.NamespacedName, 0, len(sess.Status.BundleSessions))
	for _, b := range sess.Status.BundleSessions {
		if b.SpiceboxSessionName == "" {
			continue
		}
		extras = append(extras, memory.NamespacedName{Namespace: sess.Namespace, Name: b.SpiceboxSessionName})
	}
	r.Tokens.Set(key, token, "", extras...)
	log.FromContext(ctx).Info("memory token: restored this session's registration from its Secret",
		"session", sess.Namespace+"/"+sess.Name, "extraScopes", len(extras))
}
