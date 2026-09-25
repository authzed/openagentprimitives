package bronzethread_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// TestSequencedHandler covers the three things the driver relies on: successive
// calls advance, an overrun CLAMPS rather than panicking, and a single-element
// sequence behaves exactly like a plain toolOutputs entry.
//
// Clamping rather than panicking is the load-bearing choice. An overrun means
// the replay diverged from the capture, and the step's own divergence check
// reports that with a step index and a diff; a nil deref in the MCP stub would
// report a goroutine trace from inside the harness instead.
func TestSequencedHandler(t *testing.T) {
	cases := []struct {
		name  string
		vals  []string
		calls int
		want  []string
	}{
		{
			name:  "successive calls advance through the sequence",
			vals:  []string{`{"n":1}`, `{"n":2}`, `{"n":3}`},
			calls: 3,
			want:  []string{`{"n":1}`, `{"n":2}`, `{"n":3}`},
		},
		{
			name:  "an overrun clamps at the last value rather than panicking",
			vals:  []string{`{"n":1}`, `{"n":2}`},
			calls: 4,
			want:  []string{`{"n":1}`, `{"n":2}`, `{"n":2}`, `{"n":2}`},
		},
		{
			name:  "a single-element sequence is a constant handler",
			vals:  []string{`{"n":1}`},
			calls: 2,
			want:  []string{`{"n":1}`, `{"n":1}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]json.RawMessage, len(tc.vals))
			for i, v := range tc.vals {
				raw[i] = json.RawMessage(v)
			}
			h := bt.SequencedHandler(raw)
			require.NotNil(t, h, "a non-empty sequence must produce a handler")

			for i := 0; i < tc.calls; i++ {
				got, err := json.Marshal(h(map[string]any{}))
				require.NoError(t, err, "call %d: marshalling the handler's result", i)
				assert.JSONEq(t, tc.want[i], string(got), "call %d", i)
			}
		})
	}
}

// TestSequencedHandler_EmptyIsNil pins that an empty sequence produces NO
// handler, so the driver registers nothing and the stub answers "tool not
// registered" — which names the tool. Registering a handler that returns null
// would instead hand the model a valid-looking empty result.
func TestSequencedHandler_EmptyIsNil(t *testing.T) {
	assert.Nil(t, bt.SequencedHandler(nil))
	assert.Nil(t, bt.SequencedHandler([]json.RawMessage{}))
}

// TestValidateToolOutputs_RejectsDoubleDeclaration pins that a tool named in
// BOTH maps is an error. There is no defensible order between a constant and a
// sequence for the same tool, so silently picking one would make the bundle
// mean something the author did not write.
func TestValidateToolOutputs_RejectsDoubleDeclaration(t *testing.T) {
	b := bt.Bundle{
		ToolOutputs:        map[string]json.RawMessage{"list_things": json.RawMessage(`{}`)},
		ToolOutputSequence: map[string][]json.RawMessage{"list_things": {json.RawMessage(`{}`)}},
	}
	err := b.ValidateToolOutputs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list_things", "the error must name the offending tool")
}

// TestValidateToolOutputs_AllowsDisjoint is the negative control: two different
// tools, one in each map, is the normal captured shape.
func TestValidateToolOutputs_AllowsDisjoint(t *testing.T) {
	b := bt.Bundle{
		ToolOutputs:        map[string]json.RawMessage{"get_thing": json.RawMessage(`{}`)},
		ToolOutputSequence: map[string][]json.RawMessage{"list_things": {json.RawMessage(`{}`)}},
	}
	assert.NoError(t, b.ValidateToolOutputs())
}

// TestValidateToolOutputs_RejectsASequenceThatCanNeverBeServed pins the one
// combination toolErrors makes representable-looking but is not.
//
// MCPStub.OnToolError wins over OnTool unconditionally, so a tool named in both
// toolErrors and toolOutputSequence answers every call with the error and the
// recorded sequence is dead data. Pairing with the CONSTANT map is the opposite
// case and must stay legal: a tool that always errors still needs an entry
// there, because the stub's tools/list is built from its registered handlers
// and a tool missing from it fails class admission as AllowlistDrift.
func TestValidateToolOutputs_RejectsASequenceThatCanNeverBeServed(t *testing.T) {
	seq := bt.Bundle{
		ToolOutputSequence: map[string][]json.RawMessage{"flaky": {json.RawMessage(`{"a":1}`)}},
		ToolErrors:         map[string]bt.ToolError{"flaky": {Message: "upstream is down"}},
	}
	err := seq.ValidateToolOutputs()
	require.Error(t, err, "a sequence the stub can never serve is a bundle error, not a merge")
	assert.Contains(t, err.Error(), "flaky")

	constant := bt.Bundle{
		ToolOutputs: map[string]json.RawMessage{"flaky": json.RawMessage(`{}`)},
		ToolErrors:  map[string]bt.ToolError{"flaky": {Message: "upstream is down"}},
	}
	assert.NoError(t, constant.ValidateToolOutputs(),
		"the placeholder-plus-error pairing is how a tool that ALWAYS errors is expressed at all")
}
