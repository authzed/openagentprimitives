// Package tokenrestore refills the operator's in-process memory token registry
// at startup, in one pass, before the registry stops answering unknown bearers
// with "retry".
//
// The registry is process memory, empty on every boot, while every bearer it
// served lives on in a durable Secret and in the client presenting it. More
// than one controller owns bearers in it (an AgentSession's runner token, a
// Workshop's sidecar token), so each contributes a Restorer and this package
// runs them all: the registry leaves the restoring state only once every
// Restorer has succeeded, so no owner's bearers are refused for being restored
// later than another's.
package tokenrestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// retryCap bounds the wait between attempts when a pass fails. A failed pass
// leaves the registry restoring, so unknown bearers keep getting "retry"; this
// keeps that failure from either spinning or giving up.
const retryCap = 30 * time.Second

// Restorer re-registers every live bearer one controller owns, reading each
// back from where it is stored durably. It must be idempotent and must never
// mint: a bearer already registered is left alone, and one whose stored value
// is gone stays gone. It returns how many bearers it registered, and an error
// if any it should have registered could not be — the pass is then retried.
type Restorer interface {
	RestoreTokens(ctx context.Context) (int, error)
}

// Setup puts reg in the restoring state and adds the runnable that restores
// it. Both happen here, together, so the registry can never be left waiting
// on a restore nothing will run. Call it before the manager starts, so no
// request served against reg can see it before it is marked restoring.
func Setup(mgr ctrl.Manager, reg *tokens.Registry, restorers ...Restorer) error {
	if reg == nil {
		return nil
	}
	reg.BeginRestore()
	return mgr.Add(&Runnable{Registry: reg, Cache: mgr.GetCache(), Restorers: restorers})
}

// Runnable runs every Restorer once the manager's caches have synced, retrying
// the whole pass until all of them succeed, and then ends the registry's
// restoring state.
//
// It does not wait for leader election. The memory API runs in every replica
// and authenticates against that replica's own registry, so every replica
// needs it restored, and a replica that waited for the lease would spend that
// whole wait answering "retry".
type Runnable struct {
	Registry  *tokens.Registry
	Restorers []Restorer
	Cache     interface {
		WaitForCacheSync(ctx context.Context) bool
	}
}

func (*Runnable) NeedLeaderElection() bool { return false }

func (t *Runnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("token-restore")
	ctx = log.IntoContext(ctx, logger)
	if !t.Cache.WaitForCacheSync(ctx) {
		// The manager is shutting down before its caches synced. Nothing is
		// left to serve, so there is nothing to restore.
		return nil
	}
	start := time.Now()
	backoff := time.Second
	total := 0
	for {
		restored, err := t.pass(ctx)
		total += restored
		if err == nil {
			t.Registry.FinishRestore()
			logger.Info("memory token registry restored; unknown bearers are refused from here on",
				"tokensRestored", total, "took", time.Since(start).String())
			return nil
		}
		if ctx.Err() != nil {
			// Shutting down mid-pass. The registry stays restoring: a partial
			// registry must never be declared complete.
			return nil
		}
		logger.Info("restoring the memory token registry failed; retrying, and unknown bearers are answered 'retry' until it succeeds",
			"err", err.Error(), "tokensRestoredSoFar", total, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, retryCap)
	}
}

// pass runs every Restorer, even after one fails, so a failure in one owner's
// bearers does not delay restoring another's. A retried pass is cheap: every
// Restorer skips what is already registered.
func (t *Runnable) pass(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	for _, r := range t.Restorers {
		got, err := r.RestoreTokens(ctx)
		n += got
		if err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", r, err))
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}
