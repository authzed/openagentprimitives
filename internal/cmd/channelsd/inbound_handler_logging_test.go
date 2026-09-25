package main

import (
	"context"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Every inbound pipeline handler logs through ctrllog.FromContext(ctx), and
// channelsd's global controller-runtime logger is fulfilled with a NO-OP SINK
// by a transitive init() before main runs (see the SetLogger race documented at
// the top of run). The only live logger in this process reaches code through
// the context. So a wrapper that hands a handler a bare context.Background()
// sends every line that handler emits internally to /dev/null — while still
// appearing to log, because the wrapper's own captured logger reports the
// handler's RETURNED error.
//
// That gap is invisible at build time AND at runtime: the drop paths that
// matter most are exactly the ones that return nil after logging, so nothing
// upstream ever sees them. pkg/channels/channelsd/pipeline/pipeline.go's wake-up-publish
// failure is the worked example — its own comment says a missing wakeup "can
// look like a stuck session in production", and it is reported by logging and
// returning nil.
//
// These tests therefore assert on a CAPTURED SINK reached from inside the
// handler, not on what the wrapper itself logs.
func TestInboundWrappersCarryTheLoggerIntoHandlerContext(t *testing.T) {
	// Deliberately NOT ctrllog.SetLogger(captured): fulfilling the global would
	// make a background context work by accident and hide the defect. The
	// process-global delegating logger stays unfulfilled here, exactly as it is
	// in the real channelsd process.
	const internalLine = "publishWakeup failed"

	// emitFromHandler is what a real pipeline handler does: take the logger out
	// of the context it was handed.
	emitFromHandler := func(ctx context.Context) {
		ctrllog.FromContext(ctx).Info(internalLine, "session", "ns/s")
	}

	t.Run("envelopeHandler: a line the handler logs internally reaches the live sink", func(t *testing.T) {
		logger, read := captureLogger()
		var gotCtx context.Context

		h := envelopeHandler(logger, "interaction_decision", "HandleInteractionDecision",
			func(ctx context.Context, _ channelevents.Envelope) error {
				gotCtx = ctx
				emitFromHandler(ctx)
				return nil
			})
		h(&nats.Msg{Subject: honestViewMessageSubject(), Data: mustEnvelope(t)})

		require.NotNil(t, gotCtx, "the handler must have run at all")
		assert.Contains(t, read(), internalLine,
			"a handler's own log line must reach the live logger, not the no-op global")
		assertNonCancelling(t, gotCtx)
	})

	t.Run("respondingHandler: a line the handler logs internally reaches the live sink", func(t *testing.T) {
		logger, read := captureLogger()
		var gotCtx context.Context

		h := respondingHandler(logger, "view_message", "HandleViewMessage",
			func(ctx context.Context, _ channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
				gotCtx = ctx
				emitFromHandler(ctx)
				return channelevents.ViewMessageResultPayload{Outcome: "routed"}, nil
			},
			func(*nats.Msg, []byte) error { return nil })
		h(&nats.Msg{Subject: honestViewMessageSubject(), Data: mustEnvelope(t), Reply: "_INBOX.test"})

		require.NotNil(t, gotCtx, "the handler must have run at all")
		assert.Contains(t, read(), internalLine,
			"a handler's own log line must reach the live logger, not the no-op global")
		assertNonCancelling(t, gotCtx)
	})
}

// The base context stays non-cancelling on purpose. An inbound handler writes
// memory, publishes wakeups and resolves approvals; tearing it out mid-write on
// SIGTERM would trade a logging gap for a durability one. Carrying the logger
// is strictly additive — same cancellation semantics as before (none), plus a
// live sink.
func assertNonCancelling(t *testing.T, ctx context.Context) {
	t.Helper()
	assert.NoError(t, ctx.Err(), "handler context must not arrive already cancelled")
	_, hasDeadline := ctx.Deadline()
	assert.False(t, hasDeadline, "handler context must not carry a deadline that could cut a write short")
	assert.Nil(t, ctx.Done(), "handler context must not be cancellable: shutdown must not abort an in-flight inbound write")
}

// honestViewMessageSubject is the inbound subject matching mustEnvelope's
// session, so inboundSubjectAuthorized passes and the handler actually runs.
func honestViewMessageSubject() string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix("ns", "s"), channelevents.KindViewMessage)
}

// captureLogger is a real logr sink (not logr.Discard) whose output a test can
// read back, so "the line reached a live logger" is an observable fact.
func captureLogger() (logr.Logger, func() string) {
	var mu sync.Mutex
	var sb []string
	lg := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		sb = append(sb, prefix+" "+args)
	}, funcr.Options{})
	return lg, func() string {
		mu.Lock()
		defer mu.Unlock()
		out := ""
		for _, s := range sb {
			out += s + "\n"
		}
		return out
	}
}
