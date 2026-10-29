package agentsession

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// tokenRestoreConcurrency bounds the Secret reads the startup restore makes at
// once: one per session, each a small live GET.
const tokenRestoreConcurrency = 16

// tokenRestoreRetryCap bounds the wait between attempts when the restore's
// session list fails. A list from a synced cache does not fail in practice;
// this keeps a failure from either spinning or giving up.
const tokenRestoreRetryCap = 30 * time.Second

// setupTokenRestore puts the token registry in the restoring state and adds the
// runnable that restores it. Both happen here, together, so the registry can
// never be left waiting on a restore nothing will run. This runs during
// controller setup, before the operator's memory API starts serving, so no
// request can see the registry before it is marked restoring.
func (r *Reconciler) setupTokenRestore(mgr ctrl.Manager) error {
	if r.Tokens == nil {
		return nil
	}
	r.Tokens.BeginRestore()
	return mgr.Add(&tokenRestoreRunnable{r: r, cache: mgr.GetCache()})
}

// tokenRestoreRunnable restores every live session's memory token once the
// manager's caches have synced, then ends the registry's restoring state.
//
// It does not wait for leader election. The memory API runs in every replica
// and authenticates against that replica's own registry, so every replica
// needs it restored, and a replica that waited for the lease would spend that
// whole wait answering "retry".
type tokenRestoreRunnable struct {
	r     *Reconciler
	cache interface {
		WaitForCacheSync(ctx context.Context) bool
	}
}

func (t *tokenRestoreRunnable) NeedLeaderElection() bool { return false }

func (t *tokenRestoreRunnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("token-restore")
	ctx = log.IntoContext(ctx, logger)
	if !t.cache.WaitForCacheSync(ctx) {
		// The manager is shutting down before its caches synced. Nothing is
		// left to serve, so there is nothing to restore.
		return nil
	}
	start := time.Now()
	backoff := time.Second
	for {
		restored, err := t.r.RestoreTokenRegistry(ctx)
		if err == nil {
			t.r.Tokens.FinishRestore()
			logger.Info("memory token registry restored; unknown bearers are refused from here on",
				"sessions", restored, "took", time.Since(start).String())
			return nil
		}
		logger.Info("restoring the memory token registry failed; retrying, and unknown bearers are answered 'retry' until it succeeds",
			"err", err.Error(), "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, tokenRestoreRetryCap)
	}
}

// RestoreTokenRegistry re-registers the memory token (and audit verify key) of
// every session that is not being deleted, in one pass, and returns how many
// sessions it looked at.
//
// Before this pass existed, each session's token returned only when the
// session was next reconciled: one at a time, on the single AgentSession
// worker, behind every other session in the queue. After a restart a live
// session could wait minutes, and the memory API's fixed grace window, which
// started at process birth and so was half spent by leader election and the
// cache sync, ran out first, refusing the session's runner for good. Now the
// whole registry is back before the restoring state ends, however long that
// takes.
//
// Each session goes through reregisterMemoryToken, the same restoration the
// reconciler runs, so a restored token authorizes exactly what it did before:
// it never mints, and it skips a session whose Secret holds no token. A
// session being deleted is skipped, because its finalize may already have
// revoked the token. Reconcile waits for this pass to finish (WaitRestored)
// before doing anything, so no finalize can revoke a token in between this
// pass reading the session and re-registering it.
func (r *Reconciler) RestoreTokenRegistry(ctx context.Context) (int, error) {
	var sessions spiceboxv1alpha1.AgentSessionList
	// Read-only use of the cached objects; the deep copies would only cost.
	if err := r.Client.List(ctx, &sessions, client.UnsafeDisableDeepCopy); err != nil {
		return 0, fmt.Errorf("list AgentSessions: %w", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(tokenRestoreConcurrency)
	n := 0
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if !sess.DeletionTimestamp.IsZero() {
			continue
		}
		n++
		g.Go(func() error {
			// reregisterMemoryToken logs its own failures and never returns
			// one: a session whose Secret cannot be read stays unregistered
			// and is retried on its next reconcile, which is no worse than
			// before, and must not hold every other session's restore hostage.
			r.reregisterMemoryToken(gctx, sess)
			return nil
		})
	}
	return n, g.Wait()
}
