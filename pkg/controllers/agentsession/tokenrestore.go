package agentsession

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// tokenRestoreConcurrency bounds the Secret reads the startup restore makes at
// once: one per session, each a small live GET.
const tokenRestoreConcurrency = 16

// RestoreTokens re-registers the memory token (and audit verify key) of every
// session that is not being deleted, in one pass, and returns how many tokens
// it registered. It is this controller's tokenrestore.Restorer.
//
// Before this pass existed, each session's token returned only when the
// session was next reconciled: one at a time, on the single AgentSession
// worker, behind every other session in the queue. After a restart a live
// session could wait minutes, and the memory API's fixed grace window, which
// started at process birth and so was half spent by leader election and the
// cache sync, ran out first, refusing the session's runner for good.
//
// Each session goes through reregisterMemoryToken, the same restoration the
// reconciler runs, so a restored token authorizes exactly what it did before:
// it never mints, and it skips a session whose Secret holds no token. A
// session being deleted is skipped, because its finalize may already have
// revoked the token. Reconcile waits for the restore to finish (WaitRestored)
// before doing anything, so no finalize can revoke a token in between this
// pass reading the session and re-registering it.
//
// A session whose Secret cannot be read does not stop the others; its error
// is returned once every session has been tried, so the pass is retried and
// the registry stays restoring until that session is back too.
func (r *Reconciler) RestoreTokens(ctx context.Context) (int, error) {
	var sessions spiceboxv1alpha1.AgentSessionList
	// Read-only use of the cached objects; the deep copies would only cost.
	if err := r.Client.List(ctx, &sessions, client.UnsafeDisableDeepCopy); err != nil {
		return 0, fmt.Errorf("list AgentSessions: %w", err)
	}
	var (
		g        errgroup.Group
		mu       sync.Mutex
		restored int
		errs     []error
	)
	g.SetLimit(tokenRestoreConcurrency)
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if !sess.DeletionTimestamp.IsZero() {
			continue
		}
		g.Go(func() error {
			ok, err := r.reregisterMemoryToken(ctx, sess)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				restored++
			}
			if err != nil {
				errs = append(errs, err)
			}
			return nil
		})
	}
	// Every goroutine returns nil, so a failed session never cancels the
	// rest; its error is collected in errs instead.
	if err := g.Wait(); err != nil {
		errs = append(errs, err)
	}
	return restored, errors.Join(errs...)
}
