package spiceboxsession

import (
	"context"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// SweepInterval is how often the TTL sweeper runs. Exported as var so tests
// can shorten it.
var SweepInterval = 30 * time.Second

// runTTLSweep deletes any sessions past their idle or max deadline.
// Signature matches manager.RunnableFunc (func(context.Context) error).
func (r *Reconciler) runTTLSweep(ctx context.Context) error {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweepOnce(ctx)
		}
	}
}

func (r *Reconciler) sweepOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("ttl-sweep")

	var list spiceboxv1alpha1.SpiceboxSessionList
	if err := r.Client.List(ctx, &list); err != nil {
		logger.Error(err, "list sessions")
		return
	}
	now := time.Now()
	for i := range list.Items {
		sess := &list.Items[i]
		if !sess.DeletionTimestamp.IsZero() {
			continue
		}
		// Bundle SpiceboxSessions owned by an AgentSession are NOT the
		// sweeper's to expire: the parent AgentSession's lifecycle
		// (sleep/reap, finalizer, owner-ref GC) is the single authority
		// for them. TTL-deleting one under a live session destroys a
		// healthy sandbox mid-conversation, which the parent then
		// misreads as a pod crash (this happened in production). The TTL
		// path applies only to standalone sessions (`oap sandbox ...`).
		if o := metav1.GetControllerOf(sess); o != nil && o.Kind == "AgentSession" &&
			strings.HasPrefix(o.APIVersion, spiceboxv1alpha1.SchemeBuilder.GroupVersion.Group+"/") {
			continue
		}
		reason, expired := isExpired(sess, now)
		if !expired {
			continue
		}

		key := client.ObjectKeyFromObject(sess)
		var fresh spiceboxv1alpha1.SpiceboxSession
		if err := r.Client.Get(ctx, key, &fresh); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "re-fetch before expire", "session", key)
			}
			continue
		}
		if !fresh.DeletionTimestamp.IsZero() {
			continue
		}

		patch := client.MergeFrom(fresh.DeepCopy())
		conditions.Set(&fresh, &fresh.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.SpiceboxSessionConditionTerminated, Status: metav1.ConditionTrue,
			Reason: reason, Message: "session expired",
		})
		if err := r.Client.Status().Patch(ctx, &fresh, patch); err != nil {
			logger.Error(err, "patch Terminated condition", "session", key)
			continue
		}

		uid := fresh.UID
		if err := r.Client.Delete(ctx, &fresh, client.Preconditions{UID: &uid}); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "delete expired session", "session", key)
			}
			continue
		}
		// A sweep delete is a user-visible lifecycle event (the sandbox
		// disappears); log it so an operator can find WHY a session
		// vanished without correlating audit logs.
		logger.Info("deleted expired session", "session", key, "reason", reason)
	}
}

func isExpired(sess *spiceboxv1alpha1.SpiceboxSession, now time.Time) (string, bool) {
	idle := sessionIdleTTL(sess)
	maxD := sessionMaxDuration(sess)

	if idle > 0 && sess.Status.LastActivityAt != nil {
		if now.Sub(sess.Status.LastActivityAt.Time) > idle {
			return spiceboxv1alpha1.ReasonIdleTTL, true
		}
	}
	if maxD > 0 {
		if now.Sub(sess.CreationTimestamp.Time) > maxD {
			return spiceboxv1alpha1.ReasonMaxDuration, true
		}
	}
	return "", false
}

func sessionIdleTTL(sess *spiceboxv1alpha1.SpiceboxSession) time.Duration {
	if sess.Spec.IdleTTL != nil {
		return sess.Spec.IdleTTL.Duration
	}
	if rc := sess.Status.ResolvedClass; rc != nil {
		return rc.SessionDefaults.IdleTTL.Duration
	}
	return 0
}

func sessionMaxDuration(sess *spiceboxv1alpha1.SpiceboxSession) time.Duration {
	if sess.Spec.MaxDuration != nil {
		return sess.Spec.MaxDuration.Duration
	}
	if rc := sess.Status.ResolvedClass; rc != nil {
		return rc.SessionDefaults.MaxDuration.Duration
	}
	return 0
}
