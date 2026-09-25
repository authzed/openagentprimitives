// Periodic sweeper that closes out ad-hoc join (permission) requests left
// pending past their per-class ApprovalTimeout. A join is decided by the
// session owner before the runner starts — the session is Idle and no runner
// orchestrator is parked on it — so channelsd owns its lifetime. Without this
// sweeper a forgotten join would leave a PendingRequester entry and a stuck
// PermissionRequestPending condition on AgentSession.status forever, and the
// requester's prompt would hang with no outcome.
//
// (Tool-call / leakage / content-inspection approvals block the runner, so
// their timeout is owned there: the runner's orchestrator expires them at its
// per-kind deadline and publishes a deny/timeout "applied" envelope that
// channelsd's Handle*Applied handlers consume. This sweeper is join-only.)
//
// On each tick the sweeper lists every AgentSession, compares each
// status.pendingRequesters[].requestedAt against the resolved timeout
// (AgentClass.spec.authz.approvalTimeout or 15m default), and for each expired
// entry calls the pipeline's TimeoutPermissionRequest, which clears the queue
// entry, recomputes the condition, and notifies the requester ("expired").
package main

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

const joinRequestTimeoutTickInterval = 10 * time.Second

// joinRequestTimeoutWatcher periodically expires pending join (permission)
// requests past their AgentClass.spec.approvalTimeout. now is injected so
// tests can drive the clock.
type joinRequestTimeoutWatcher struct {
	cli            client.Client
	pipeline       *pipeline.Pipeline
	now            func() time.Time
	defaultTimeout time.Duration
}

func newJoinRequestTimeoutWatcher(cli client.Client, pl *pipeline.Pipeline) *joinRequestTimeoutWatcher {
	return &joinRequestTimeoutWatcher{
		cli:            cli,
		pipeline:       pl,
		now:            time.Now,
		defaultTimeout: 15 * time.Minute,
	}
}

// Run ticks every joinRequestTimeoutTickInterval until ctx is canceled.
// Call in a dedicated goroutine.
func (w *joinRequestTimeoutWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("joinrequesttimeout")
	t := time.NewTicker(joinRequestTimeoutTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				logger.Info("join request timeout tick errored", "err", err.Error())
			}
		}
	}
}

func (w *joinRequestTimeoutWatcher) tick(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("joinrequesttimeout")
	var list spiceboxv1alpha1.AgentSessionList
	if err := w.cli.List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		sess := &list.Items[i]
		if len(sess.Status.PendingRequesters) == 0 {
			continue
		}
		timeout := w.resolveTimeout(ctx, sess)
		for _, pr := range sess.Status.PendingRequesters {
			if w.now().Sub(pr.RequestedAt.Time) <= timeout {
				continue
			}
			// TimeoutPermissionRequest re-reads the session and matches by
			// RequestRef, so a concurrent owner decision or a fresh re-request
			// is handled safely (no-op).
			if err := w.pipeline.TimeoutPermissionRequest(ctx, sess.Namespace, sess.Name, pr.RequestRef); err != nil {
				logger.Info("TimeoutPermissionRequest failed",
					"session", sess.Namespace+"/"+sess.Name,
					"requestRef", pr.RequestRef, "err", err.Error())
			}
		}
	}
	return nil
}

func (w *joinRequestTimeoutWatcher) resolveTimeout(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) time.Duration {
	if sess.Spec.Class == "" {
		return w.defaultTimeout
	}
	var cls spiceboxv1alpha1.AgentClass
	if err := w.cli.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &cls); err != nil {
		// Class missing or API error — fall back to default. Logging at every
		// tick would be too noisy; a missing class is also surfaced by other
		// controllers.
		return w.defaultTimeout
	}
	// Source the per-class approval-WAIT deadline from the consolidated
	// authz.approvalTimeout. When unset, fall back to the watcher's own default.
	if a := cls.Spec.Authz; a != nil && a.ApprovalTimeout != nil && a.ApprovalTimeout.Duration > 0 {
		return a.ApprovalTimeout.Duration
	}
	return w.defaultTimeout
}
