package meta

// Data slots: delegate's `inputs`, the PRODUCER half of the typed handoff.
//
// spec.dataSlots had a reader (the SubagentRequest controller) and no writer
// at all for its whole life, so the data-slot mechanism — and, downstream of
// it, handoff grading and the trifecta's legs A and B — was unreachable from
// an agent. Nothing failed; the chain simply never ran. These pin the contract
// that closes it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestDelegateInputsResolveByToolUseIdNeverByTagId is the security shape.
//
// The model names a CALL; the runner supplies the tag. A tag id appears
// nowhere in the schema, so a parent has no vocabulary for asking about data
// it did not produce — the bindable set is confined to this session's own
// calls by construction, rather than by a check that could be forgotten.
func TestDelegateInputsResolveByToolUseIdNeverByTagId(t *testing.T) {
	created := make(chan *v1.SubagentRequest, 1)
	var askedFor []string
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, sr *v1.SubagentRequest) error { created <- sr; return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase: v1.SubagentRequestPhaseSucceeded, Result: "done",
			}}, nil
		},
		ResolveDataTag: func(_ context.Context, id string) (string, error) {
			askedFor = append(askedFor, id)
			return "ptt-" + id, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(
		`{"agent":"demo-coder","task":"x","inputs":{"logs":{"tool_use_id":"toolu_9"},"diff":{"tool_use_id":"toolu_7"}}}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	sr := <-created
	assert.ElementsMatch(t, []string{"toolu_7", "toolu_9"}, askedFor,
		"each input resolves through the runner; the model supplies no tag")
	// Sorted by slot, so a map's iteration order cannot make two identical
	// calls emit different specs.
	assert.Equal(t, []v1.DataSlotRequest{
		{Slot: "diff", TagID: "ptt-toolu_7"},
		{Slot: "logs", TagID: "ptt-toolu_9"},
	}, sr.Spec.DataSlots)
}

// TestDelegateRefusesRatherThanHandingOverPartialInputs.
//
// Every failure refuses the WHOLE delegation. A child spawned with three of
// the four slots it was promised cannot tell that apart from a parent that
// chose to withhold one, and it proceeds on that reading — so a dropped slot
// does not degrade the handoff, it changes what the child believes it was
// told. In every case here nothing may be created at all.
func TestDelegateRefusesRatherThanHandingOverPartialInputs(t *testing.T) {
	cases := []struct {
		name    string
		resolve func(context.Context, string) (string, error)
		args    string
		wantIn  string
	}{
		{
			name:    "a call that minted no tag: refused, naming the call",
			resolve: func(context.Context, string) (string, error) { return "", nil },
			args:    `{"agent":"demo-coder","task":"x","inputs":{"diff":{"tool_use_id":"toolu_7"}}}`,
			wantIn:  "toolu_7",
		},
		{
			// The distinction that matters: reporting "no data" on a store
			// outage would blame the model for an infrastructure failure and
			// send it to paste the content into the task text instead — the
			// laundering path slots exist to prevent.
			name:    "an unreadable provenance store is UNKNOWN, never 'no data'",
			resolve: func(context.Context, string) (string, error) { return "", errors.New("store down") },
			args:    `{"agent":"demo-coder","task":"x","inputs":{"diff":{"tool_use_id":"toolu_7"}}}`,
			wantIn:  "store down",
		},
		{
			name:    "an input naming no call at all",
			resolve: func(context.Context, string) (string, error) { return "ptt-x", nil },
			args:    `{"agent":"demo-coder","task":"x","inputs":{"diff":{"tool_use_id":"  "}}}`,
			wantIn:  "diff",
		},
		{
			name:    "a session with no resolver wired cannot hand over data",
			resolve: nil,
			args:    `{"agent":"demo-coder","task":"x","inputs":{"diff":{"tool_use_id":"toolu_7"}}}`,
			wantIn:  "cannot hand over data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var creates int
			tl := NewDelegateTool(DelegateConfig{
				Namespace: "ns", SessionName: "demo-parent",
				Create:         func(context.Context, *v1.SubagentRequest) error { creates++; return nil },
				ResolveDataTag: tc.resolve,
				PollInterval:   time.Millisecond,
			})

			res, err := tl.Execute(context.Background(), json.RawMessage(tc.args), nil)
			require.NoError(t, err)
			assert.True(t, res.IsError, "must refuse rather than delegate with a missing slot")
			assert.Contains(t, res.Content, tc.wantIn)
			assert.Zero(t, creates, "nothing may be created when an input cannot be resolved")
		})
	}
}

// TestDelegateWithoutInputsNeedsNoResolver keeps the addition off the path
// every existing delegation takes: a parent handing over nothing must not
// start requiring a resolver it never had.
func TestDelegateWithoutInputsNeedsNoResolver(t *testing.T) {
	created := make(chan *v1.SubagentRequest, 1)
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, sr *v1.SubagentRequest) error { created <- sr; return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase: v1.SubagentRequestPhaseSucceeded, Result: "done",
			}}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"x"}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	assert.Empty(t, (<-created).Spec.DataSlots)
}
