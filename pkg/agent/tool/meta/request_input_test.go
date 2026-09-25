package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

type recordedRequest struct{ slot, why string }

func TestRequestInputRecordsTheAsk(t *testing.T) {
	var rec []recordedRequest
	tl := meta.NewRequestInput(meta.RequestInputConfig{
		Record: func(_ context.Context, slot, why string) error {
			rec = append(rec, recordedRequest{slot, why})
			return nil
		},
	})
	args, _ := json.Marshal(map[string]any{"slot": "diff", "why": "the task refers to a diff I cannot see"})

	res, err := tl.Execute(context.Background(), args, nil)
	require.NoError(t, err)
	assert.False(t, res.IsError, res.Content)

	require.Len(t, rec, 1)
	assert.Equal(t, "diff", rec[0].slot)
	assert.Contains(t, rec[0].why, "cannot see")
	// It PARKS, and the result says so.
	//
	// This assertion used to require the opposite — "NOT paused" — and that
	// contract turned out to be unimplementable rather than merely undesirable.
	// `delegate` blocks the parent and returns control on four phases only
	// (Succeeded, AwaitingParent, Denied, Failed), so while a child merely runs
	// its parent's model never gets a turn and can never call send_input. A
	// non-blocking request reaches nobody who can answer it: it would sit until
	// the delegation ended.
	//
	// With no Await wired (this config), the yield is a park to Idle — the
	// request stands and the parent's response re-hydrates the session.
	assert.True(t, res.IdleExit, "the child parks; the only party who can answer is blocked until it does")
	assert.Contains(t, res.Content, "parking")
}

// TestRequestInputGrantsNothing pins that this tool is a REQUEST.
//
// The bound on what a parent may then bind is enforced at handoff, by the
// attenuation check that refuses any tag the parent cannot itself read. A
// permission here would gate the ASKING, which is not the property anyone
// wants — the property is that asking cannot widen anything.
func TestRequestInputGrantsNothing(t *testing.T) {
	tl := meta.NewRequestInput(meta.RequestInputConfig{
		Record: func(context.Context, string, string) error { return nil },
	})
	assert.Equal(t, authz.Stateless, tl.Permission().StateImpact,
		"request_input reaches no resource: it records an ask the delegating agent may act on")
	assert.Nil(t, tl.PermissionVariants())
}

func TestRequestInputRefusesIncompleteCalls(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no slot", args: map[string]any{"why": "because"}, want: "required"},
		{name: "no why", args: map[string]any{"slot": "diff"}, want: "required"},
		{name: "blank slot", args: map[string]any{"slot": "   ", "why": "because"}, want: "required"},
		{
			name: "over-long reason",
			args: map[string]any{"slot": "diff", "why": strings.Repeat("x", 2049)},
			want: "truncated reason reads as a different one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec []recordedRequest
			tl := meta.NewRequestInput(meta.RequestInputConfig{
				Record: func(_ context.Context, slot, why string) error {
					rec = append(rec, recordedRequest{slot, why})
					return nil
				},
			})
			args, _ := json.Marshal(tc.args)
			res, err := tl.Execute(context.Background(), args, nil)
			require.NoError(t, err)
			assert.True(t, res.IsError)
			assert.Contains(t, res.Content, tc.want)
			assert.Empty(t, rec, "a refused call must record nothing")
		})
	}
}

// TestRequestInputSurfacesAFailedSend keeps the child from believing it asked.
//
// A swallowed error here leaves a child waiting on data nobody was ever told
// about — the same silent shape as a refused slot that never reached status.
func TestRequestInputSurfacesAFailedSend(t *testing.T) {
	tl := meta.NewRequestInput(meta.RequestInputConfig{
		Record: func(context.Context, string, string) error { return errors.New("bus unavailable") },
	})
	args, _ := json.Marshal(map[string]any{"slot": "diff", "why": "need it"})

	res, err := tl.Execute(context.Background(), args, nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "bus unavailable")
}

// TestRequestInputWithoutAParentRefuses covers a root session that somehow
// reaches the tool: there is nobody to ask, and saying so is better than a
// silent no-op the agent reads as success.
func TestRequestInputWithoutAParentRefuses(t *testing.T) {
	tl := meta.NewRequestInput(meta.RequestInputConfig{})
	args, _ := json.Marshal(map[string]any{"slot": "diff", "why": "need it"})

	res, err := tl.Execute(context.Background(), args, nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "no delegating agent")
}
