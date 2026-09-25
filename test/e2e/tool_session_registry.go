//go:build e2e

// KEEP IN SYNC WITH internal/cmd/runner/nats.go:86-160

package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// toolSessionRegistry routes inbound tool-session input to the live bridge for
// a given ToolCall. Keyed by ToolCall CR name. Safe for concurrent use.
//
// Ported from internal/cmd/runner/nats.go: the in-process e2e harness can't import the
// `main` package, so the helpers below are duplicated here verbatim (modulo
// the runtime-vs-conn parameter shape difference noted on
// subscribeToolSessionInput).
type toolSessionRegistry struct {
	mu    sync.Mutex
	feeds map[string]func([]byte) error
}

func newToolSessionRegistry() *toolSessionRegistry {
	return &toolSessionRegistry{feeds: map[string]func([]byte) error{}}
}

// register installs feed for toolCallRef and returns a cancel func that
// removes it. Safe to call cancel after the registry is GCed.
func (r *toolSessionRegistry) register(ref string, feed func([]byte) error) func() {
	r.mu.Lock()
	r.feeds[ref] = feed
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.feeds, ref)
		r.mu.Unlock()
	}
}

// feed routes data to the registered Feed func for ref. Returns false when
// no bridge is registered (caller logs and drops).
func (r *toolSessionRegistry) feed(ref string, data []byte) bool {
	r.mu.Lock()
	f := r.feeds[ref]
	r.mu.Unlock()
	if f == nil {
		return false
	}
	if err := f(data); err != nil {
		slog.Info("tool_session feed errored", "toolCallRef", ref, "err", err.Error())
	}
	return true
}

// subscribeToolSessionInput routes inbound KindToolSessionInput envelopes to
// the matching bridge. Modeled on internal/cmd/runner/nats.go's subscribeToolSessionInput
// — adapted to take a *nats.Conn directly rather than the production
// natsRuntime wrapper (the harness has no equivalent struct and the connection
// is the only field actually used in production).
func subscribeToolSessionInput(ctx context.Context, nc *nats.Conn, reg *toolSessionRegistry) {
	if nc == nil || reg == nil {
		return
	}
	sub, err := nc.Subscribe(
		channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindToolSessionInput),
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				slog.Info("tool_session_input: unmarshal envelope", "err", uerr.Error())
				return
			}
			var pl channelevents.ToolSessionInputPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				slog.Info("tool_session_input: unmarshal payload", "err", uerr.Error())
				return
			}
			if !reg.feed(pl.ToolCallRef, pl.Data) {
				slog.Info("tool_session_input: no live bridge", "toolCallRef", pl.ToolCallRef)
			}
		},
	)
	if err != nil {
		slog.Info("tool_session_input subscribe failed", "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}
