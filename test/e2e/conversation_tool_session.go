//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ToolSessionDeltaPredicate filters captured tool_session_delta envelopes.
type ToolSessionDeltaPredicate func(channelevents.ToolSessionDeltaPayload) bool

// ToolSessionContains matches a non-terminal delta whose rendered Data
// (string form) contains every substring. Terminal deltas have empty Data —
// use Terminal() for those.
func ToolSessionContains(subs ...string) ToolSessionDeltaPredicate {
	return func(pl channelevents.ToolSessionDeltaPayload) bool {
		if pl.Terminal {
			return false
		}
		body := string(pl.Data)
		for _, s := range subs {
			if !strings.Contains(body, s) {
				return false
			}
		}
		return true
	}
}

// Terminal matches the final delta — Terminal=true. Optional ExitReason
// constraint (zero string matches any).
func Terminal(wantReason string) ToolSessionDeltaPredicate {
	return func(pl channelevents.ToolSessionDeltaPayload) bool {
		if !pl.Terminal {
			return false
		}
		return wantReason == "" || pl.ExitReason == wantReason
	}
}

// ToolCallRef restricts to deltas for a specific toolCallRef. Compose with
// other predicates via And.
func ToolCallRef(ref string) ToolSessionDeltaPredicate {
	return func(pl channelevents.ToolSessionDeltaPayload) bool {
		return pl.ToolCallRef == ref
	}
}

// And combines predicates with logical AND.
func And(preds ...ToolSessionDeltaPredicate) ToolSessionDeltaPredicate {
	return func(pl channelevents.ToolSessionDeltaPayload) bool {
		for _, p := range preds {
			if !p(pl) {
				return false
			}
		}
		return true
	}
}

// ExpectToolSessionDelta blocks until a captured KindToolSessionDelta
// envelope satisfies pred, or the harness's DefaultTimeout elapses
// (default 5s). Returns the matched payload.
//
// Mirrors ExpectAgentReply's cursor semantics: each call scans the
// harness's slice of captured envelopes from a per-test cursor forward
// and returns the first match, advancing the cursor past that envelope
// only. Envelopes that don't match the predicate stay in the queue so a
// subsequent Expect call with a different predicate can still see them.
// On timeout, every seen-but-unmatched payload since the cursor is
// logged so a debugger can tell whether the envelope arrived but the
// predicate was wrong, vs. nothing arrived at all.
func (h *Harness) ExpectToolSessionDelta(pred ToolSessionDeltaPredicate) channelevents.ToolSessionDeltaPayload {
	h.t.Helper()
	timeout := h.opts.DefaultTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		h.toolSessionDeltasMu.Lock()
		for i := h.toolSessionDeltasSeen; i < len(h.toolSessionDeltas); i++ {
			var pl channelevents.ToolSessionDeltaPayload
			if err := json.Unmarshal(h.toolSessionDeltas[i].Env.Payload, &pl); err != nil {
				h.toolSessionDeltasMu.Unlock()
				h.t.Fatalf("ExpectToolSessionDelta: unmarshal payload at index %d: %v", i, err)
			}
			if pred(pl) {
				h.toolSessionDeltasSeen = i + 1
				h.toolSessionDeltasMu.Unlock()
				return pl
			}
		}
		h.toolSessionDeltasMu.Unlock()
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Timeout: dump every seen-but-unmatched payload since the cursor so
	// the test author can see what arrived, then fatal.
	h.toolSessionDeltasMu.Lock()
	defer h.toolSessionDeltasMu.Unlock()
	for i := h.toolSessionDeltasSeen; i < len(h.toolSessionDeltas); i++ {
		var pl channelevents.ToolSessionDeltaPayload
		if err := json.Unmarshal(h.toolSessionDeltas[i].Env.Payload, &pl); err != nil {
			h.t.Logf("ExpectToolSessionDelta: unmatched [%d] subject=%s (payload unmarshal err: %v)",
				i, h.toolSessionDeltas[i].Subject, err)
			continue
		}
		h.t.Logf("ExpectToolSessionDelta: unmatched [%d] subject=%s toolCallRef=%s terminal=%t exitReason=%q data=%q",
			i, h.toolSessionDeltas[i].Subject, pl.ToolCallRef, pl.Terminal, pl.ExitReason, string(pl.Data))
	}
	h.t.Fatalf("ExpectToolSessionDelta: timeout after %s waiting for matching delta (%d unmatched envelope(s) since cursor)",
		timeout, len(h.toolSessionDeltas)-h.toolSessionDeltasSeen)
	return channelevents.ToolSessionDeltaPayload{}
}

// SendChannelInputToToolSession publishes a KindToolSessionInput envelope on
// the IN subject for the (single) AgentSession in the harness namespace,
// simulating a human reply during an interactive session. The runner's
// tool_session subscriber routes data to the bridge's Feed func.
// Infers the target session via singleSession; multi-session scenarios
// will need an explicit selector.
func (h *Harness) SendChannelInputToToolSession(toolCallRef, text string) {
	h.t.Helper()
	ns, name := h.singleSession("SendChannelInputToToolSession")
	pl := channelevents.ToolSessionInputPayload{
		ToolCallRef: toolCallRef,
		Requester: channelevents.ExternalIdentity{
			Kind: "fake", ExternalID: identity.RawExternalID(h.opts.DefaultUser),
		},
		Data: append([]byte(text), '\n'),
	}
	if err := channelevents.PublishIn(
		func(subject string, data []byte) error { return h.nc.Publish(subject, data) },
		ns, name, channelevents.KindToolSessionInput, pl,
	); err != nil {
		h.t.Fatalf("SendChannelInputToToolSession: publish: %v", err)
	}
}
