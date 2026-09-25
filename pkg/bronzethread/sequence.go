package bronzethread

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// CannedHandler turns ONE recorded result into an MCP stub handler that answers
// every call with it.
//
// Returns nil for an empty value so the caller registers nothing: an
// unregistered tool fails as "tool not registered: <name>", which names the
// tool, whereas a handler returning null hands the model a valid-looking empty
// result and the divergence surfaces somewhere else entirely.
func CannedHandler(v json.RawMessage) func(map[string]any) any {
	if len(v) == 0 {
		return nil
	}
	return func(map[string]any) any { return served(v) }
}

// SequencedHandler turns a recorded sequence of results into an MCP stub
// handler that answers each successive call with the next one.
//
// Empty means nil, for the reason CannedHandler gives.
//
// Guarded because the runner dispatches tool calls from its own goroutines
// while the test goroutine registered the handler.
func SequencedHandler(vals []json.RawMessage) func(map[string]any) any {
	if len(vals) == 0 {
		return nil
	}
	var (
		mu sync.Mutex
		i  int
	)
	return func(map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		// Clamp rather than wrap or panic. An overrun means the replay diverged
		// from what was captured; the step's Expect check reports that with a
		// step index, which is a far better error than either alternative.
		v := vals[min(i, len(vals)-1)]
		i++
		return served(v)
	}
}

// served is what a recorded result becomes on the wire.
//
// The recorded BYTES, handed to the stub as a json.RawMessage so its own
// json.Marshal emits them unchanged. Decoding into map[string]any first — which
// is what both handlers used to do — silently rewrites the result twice over:
// Go sorts a map's keys on the way out, and every number round-trips through
// float64, so an id past 2^53 comes back rounded. Neither survives contact with
// a real capture, whose Expect is derived from the recorded text: the replay
// takes the identical path and is reported as a divergence.
//
// The decode remains, as a CHECK. A value that will not parse is a malformed
// bundle, and the honest answer to the model is an error string it can report —
// not a silent nil that reads as an empty result, and not the broken bytes
// themselves, which would corrupt the whole JSON-RPC response rather than one
// tool's result.
func served(v json.RawMessage) any {
	if !json.Valid(v) {
		return map[string]any{"error": "steelthread: undecodable recorded result: " + string(v)}
	}
	return v
}

// ServedResult predicts the exact text a recorded result becomes once the stub
// has marshalled it, and reports whether the value was JSON at all.
//
// The capture reads this to derive a step's Expect, and the replay produces the
// same bytes through CannedHandler/SequencedHandler — one contract, read in
// both directions, so the derived assertion cannot be about text the replay can
// never produce. That is not hypothetical: the encoder rewrites <, > and &
// inside strings even when passing a RawMessage straight through, so an Expect
// copied from the transcript is wrong for any tool whose output mentions a tag.
//
// Non-JSON is returned unchanged, and ok is false. A sandbox tool's recovered
// stdout is plain text the exec binder never wraps.
func ServedResult(v json.RawMessage) (json.RawMessage, bool) {
	out, err := json.Marshal(served(v))
	if err != nil || !json.Valid(v) {
		return v, false
	}
	return out, true
}

// ValidateToolOutputs rejects the two pairings that make a bundle's canned
// answers ambiguous or dead.
//
// A tool in both ToolOutputs and ToolOutputSequence has no defensible order
// between them. A tool in both ToolOutputSequence and ToolErrors has a sequence
// the stub can never serve: OnToolError wins over OnTool for the same name and
// applies to every call, so the recorded sequence is data nothing reads.
//
// ToolOutputs paired with ToolErrors is deliberately NOT refused — see
// Bundle.ToolErrors: a tool that always errors still needs a ToolOutputs entry
// to appear in the stub's tools/list, and that is the only way to express one.
//
// Names are sorted so a bundle with two problems reports the same one on every
// run; the first failure is returned rather than all of them, matching
// Bundle.Validate.
func (b Bundle) ValidateToolOutputs() error {
	for _, name := range slices.Sorted(maps.Keys(b.ToolOutputSequence)) {
		if _, dup := b.ToolOutputs[name]; dup {
			return fmt.Errorf(
				"tool %q is declared in both toolOutputs and toolOutputSequence; "+
					"use one or the other", name)
		}
		if _, dup := b.ToolErrors[name]; dup {
			return fmt.Errorf(
				"tool %q is declared in both toolOutputSequence and toolErrors; the error is "+
					"registered per tool NAME and wins over every canned result, so the sequence "+
					"would never be served", name)
		}
	}
	return nil
}
