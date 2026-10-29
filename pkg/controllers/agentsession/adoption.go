package agentsession

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// sessionOwnerKind is the owner kind an AgentSession adopts Secrets under.
const sessionOwnerKind = "AgentSession"

// adoptCredentialSecret makes a credential Secret readable through the
// SecretReader guard before this session reads it.
//
// The credential Secrets a session reads belong to an AgentIdentity or a
// UserIdentity, whose own controllers adopt them, and a class whose identity
// has not been reconciled Valid never gets this far. So the Secret is
// normally adopted already, and this adopts only when it is not.
//
// It used to adopt unconditionally. Every session then added its own owner
// annotation and its own field manager to one Secret shared by every session
// of the class, which nothing read and nothing removed. On a long-running
// install that Secret reached 866 owners, every apply to it took the API
// server about seven seconds, and the reconcile applied it twice, so the
// single AgentSession worker spent about 13s on every live session.
func (r *Reconciler) adoptCredentialSecret(ctx context.Context, credRef, owner types.NamespacedName) error {
	_, err := adoptkit.AdoptSecretIfUnadopted(ctx, r.SecretReader.Reader, r.Client, credRef, owner, sessionOwnerKind)
	return err
}

// pruneSessionAdoptions drops AgentSession adoptions that no longer need to
// exist from every adopted Secret: those of sessions that are gone, being
// deleted, or terminal. A terminal session reads no credential, and if it
// is woken, adoptCredentialSecret adopts again before anything reads one.
// Live sessions' adoptions are left in place.
//
// It sweeps every adopted Secret, not only this session's, because nothing
// else ever will. Adoptions left by sessions deleted before this existed have
// no reconcile of their own to clean them up. Each Secret takes at most one
// write, however many owners it sheds.
//
// It reads the label-filtered Secret cache, which holds exactly the adopted
// Secrets, without deep-copying them: they are only inspected here, and the
// largest is the one this exists to shrink. Best-effort throughout: a failure
// is logged and retried on the next terminal reconcile, never returned,
// because nothing a session is waiting on depends on it.
func (r *Reconciler) pruneSessionAdoptions(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) {
	logger := log.FromContext(ctx)
	var secrets corev1.SecretList
	if err := r.Client.List(ctx, &secrets, client.UnsafeDisableDeepCopy); err != nil {
		logger.Info("adoption prune: listing adopted Secrets failed; retried on the next terminal reconcile",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return
	}
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if !adoptkit.HasOwnerOfKind(sec.Annotations, sessionOwnerKind) {
			continue
		}
		removed, err := adoptkit.PruneOwners(ctx, r.Client, sec, sessionOwnerKind, func(owner types.NamespacedName) bool {
			return r.sessionAdoptionIsStale(ctx, sess, owner)
		})
		if err != nil {
			logger.Info("adoption prune: pruning a Secret's stale AgentSession owners failed; retried on the next terminal reconcile",
				"session", sess.Namespace+"/"+sess.Name, "secret", sec.Namespace+"/"+sec.Name, "err", err.Error())
			continue
		}
		if removed > 0 {
			logger.Info("adoption prune: removed stale AgentSession owners from an adopted Secret",
				"session", sess.Namespace+"/"+sess.Name, "secret", sec.Namespace+"/"+sec.Name, "removed", removed)
		}
	}
}

// sessionAdoptionIsStale reports whether owner, an AgentSession named in an
// adoption annotation, no longer needs it. self is the session this prune runs
// for, whose own state is already in hand. A read error other than NotFound
// keeps the adoption: an owner that cannot be checked is not known to be done.
func (r *Reconciler) sessionAdoptionIsStale(ctx context.Context, self *spiceboxv1alpha1.AgentSession, owner types.NamespacedName) bool {
	if owner.Namespace == self.Namespace && owner.Name == self.Name {
		return !self.DeletionTimestamp.IsZero() || isTerminalPhase(self.Status.Phase)
	}
	var other spiceboxv1alpha1.AgentSession
	if err := r.Client.Get(ctx, owner, &other); err != nil {
		return apierrors.IsNotFound(err)
	}
	return !other.DeletionTimestamp.IsZero() || isTerminalPhase(other.Status.Phase)
}
