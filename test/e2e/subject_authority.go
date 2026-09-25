//go:build e2e

// Package e2e — the inbound subject-authority gate.
//
// The harness IS channelsd for a scenario: it subscribes the same cluster-wide
// "ap.session.*.*.in.<kind>" wildcards production does, over the same pipeline
// handlers. Those handlers route on env.Session and say so — see
// pkg/channels/channelsd/pipeline/interaction_decision.go's "WHICH SESSION is decided
// comes from env.Session, and that is sound only because the bus wrapper has
// already cross-checked it". The wrapper is the whole reason that is sound.
//
// Without these helpers the harness dispatched straight off env.Session, so the
// gate internal/cmd/channelsd/main.go's envelopeHandler / respondingHandler apply had
// ZERO e2e coverage — and under the harness a publisher on session A's inbound
// subject could resolve session B's parked approval, which is precisely what
// the production gate exists to refuse. A divergence here is the kind that
// makes a defect invisible in the mode everything is tested in.
//
// The decision itself is channelevents.AuthorizeInSubject / AuthorizeOutSubject
// — the same function production calls, not a re-derivation of it. Only the
// logging differs: the harness has no logr, so refusals go to stderr in the
// fmt.Fprintf idiom the rest of the harness uses (a dropped envelope with no
// trace is exactly the silent failure AGENTS.md forbids).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// decodeAuthorizedInbound decodes m into an Envelope and confirms the inbound
// subject authorizes the session it claims. Returns ok=false — after logging —
// on a malformed envelope or a refused subject; the reason string is non-empty
// exactly when ok is false, for the responding caller that must reply with it.
func decodeAuthorizedInbound(name string, m *nats.Msg) (env channelevents.Envelope, reason string, ok bool) {
	if err := json.Unmarshal(m.Data, &env); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %s decode: %v\n", name, err)
		return env, name + ": decode envelope: " + err.Error(), false
	}
	if _, _, err := channelevents.AuthorizeInSubject(m.Subject, env); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %s drop, %v (subject %q, kind %q)\n",
			name, err, m.Subject, string(env.Kind))
		return env, name + ": " + err.Error(), false
	}
	return env, "", true
}

// inboundEnvelopeHandler is the harness's envelopeHandler: decode, cross-check
// the claimed session against the authorized subject, dispatch, log whatever
// the handler returns. name labels the subscription, handler the method.
func inboundEnvelopeHandler(
	name, handler string,
	fn func(context.Context, channelevents.Envelope) error,
) func(*nats.Msg) {
	return func(m *nats.Msg) {
		env, _, ok := decodeAuthorizedInbound(name, m)
		if !ok {
			return
		}
		if err := fn(context.Background(), env); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s: %v\n", handler, err)
		}
	}
}

// respondingEnvelopeHandler is the harness's respondingHandler — the
// request/reply sibling for view_message. It ALWAYS replies, including on a
// refused subject: a caller left waiting on a silent drop gets a spinner for
// the full channelkinds.ViewMessageTimeout instead of a reason.
func respondingEnvelopeHandler(
	name, handler string,
	fn func(context.Context, channelevents.Envelope) (channelevents.ViewMessageResultPayload, error),
) func(*nats.Msg) {
	reply := func(m *nats.Msg, res channelevents.ViewMessageResultPayload) {
		b, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s marshal reply: %v\n", name, err)
			b = []byte(`{"outcome":"internal_error","error":"marshal reply failed"}`)
		}
		if err := m.Respond(b); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s respond: %v\n", name, err)
		}
	}
	return func(m *nats.Msg) {
		env, reason, ok := decodeAuthorizedInbound(name, m)
		if !ok {
			reply(m, channelevents.ViewMessageResultPayload{Outcome: "internal_error", Error: reason})
			return
		}
		res, err := fn(context.Background(), env)
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s: %v\n", handler, err)
			if res.Error == "" {
				res.Error = err.Error()
			}
			if res.Outcome == "" {
				res.Outcome = "internal_error"
			}
		}
		reply(m, res)
	}
}
