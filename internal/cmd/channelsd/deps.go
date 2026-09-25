package main

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/platform/startup"
)

// channelsdDepCeiling bounds channelsd's in-process wait on the operator memory
// service + SpiceDB at startup. On ceiling channelsd returns a terminal error
// (the caller exits loud); CrashLoopBackoff is the last-resort backstop.
const channelsdDepCeiling = 2 * time.Minute

// waitForDeps blocks until both readiness probes pass, retrying with bounded
// backoff so a not-yet-ready operator/SpiceDB at startup does not crash channelsd
// into CrashLoopBackoff. memHealthz and azPing are injected so the loop is
// unit-testable without a live operator or SpiceDB.
func waitForDeps(ctx context.Context, memHealthz, azPing func(context.Context) error, log logr.Logger) error {
	return startup.Retry(ctx, "operator memory /healthz + SpiceDB", channelsdDepCeiling,
		func(ctx context.Context) error {
			if err := memHealthz(ctx); err != nil {
				return err
			}
			return azPing(ctx)
		},
		func(attempt int, err error, next time.Duration) {
			log.Info("channelsd: dependencies not ready, backing off",
				"attempt", attempt, "err", err, "retryIn", next.String())
		})
}
