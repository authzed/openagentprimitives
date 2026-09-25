package claude_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/claude"
)

func newParser(t *testing.T) toolkitstream.Parser {
	t.Helper()
	return (&claude.Factory{}).New()
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func TestFactory_KindIsClaudeStreamJSON(t *testing.T) {
	f := claude.Factory{}
	assert.Equal(t, "claude-stream-json", f.Kind())
}

func TestParser_HappyPath(t *testing.T) {
	p := newParser(t)
	events, err := p.Parse(loadFixture(t, "happy_path.ndjson"))
	require.NoError(t, err)
	require.Len(t, events, 2, "expected 1 text + 1 result")

	assert.Equal(t, toolkitstream.EventTextDelta, events[0].Type)
	assert.Contains(t, events[0].Text, "Hello")

	assert.Equal(t, toolkitstream.EventResult, events[1].Type)
	assert.True(t, events[1].OK)
	assert.InDelta(t, 0.0123, events[1].CostUSD, 1e-6)
	assert.Equal(t, int64(4321), events[1].DurationMs)
}

func TestParser_ToolUse_StartStopPairs(t *testing.T) {
	p := newParser(t)
	events, err := p.Parse(loadFixture(t, "tool_use.ndjson"))
	require.NoError(t, err)

	require.Len(t, events, 6)

	assert.Equal(t, toolkitstream.EventTextDelta, events[0].Type)

	assert.Equal(t, toolkitstream.EventToolUseStart, events[1].Type)
	assert.Equal(t, "Read", events[1].ToolName)
	assert.Equal(t, "tu_1", events[1].ToolID)
	assert.Equal(t, "Reading foo.go", events[1].Summary)

	assert.Equal(t, toolkitstream.EventToolUseStop, events[2].Type)
	assert.Equal(t, "tu_1", events[2].ToolID)
	assert.True(t, events[2].OK)

	assert.Equal(t, toolkitstream.EventToolUseStart, events[3].Type)
	assert.Equal(t, "Edit", events[3].ToolName)

	assert.Equal(t, toolkitstream.EventToolUseStop, events[4].Type)
	assert.Equal(t, "tu_2", events[4].ToolID)
	assert.True(t, events[4].OK)

	assert.Equal(t, toolkitstream.EventResult, events[5].Type)
}

// TestParser_ToolUseStart_SummarizesRealInputShapes pins the gloss against the
// input schema Claude Code actually emits. Every file tool names its target
// `file_path`; a summarizer reading `path` matches nothing and silently drops
// each line to the raw-JSON fallback, which is what the chat renderers show.
func TestParser_ToolUseStart_SummarizesRealInputShapes(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  string
		exact bool
	}{
		{
			name:  "Bash: the command, shell-prompted",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"pnpm test","description":"run the unit tests"}}]}}`,
			want:  "$ pnpm test",
			exact: true,
		},
		{
			name:  "Read: file_path, not path",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"/workspace/internal/package.json"}}]}}`,
			want:  "Reading /workspace/internal/package.json",
			exact: true,
		},
		{
			name:  "Edit: file_path, not path",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"/workspace/a.ts","old_string":"x","new_string":"y"}}]}}`,
			want:  "Editing /workspace/a.ts",
			exact: true,
		},
		{
			name:  "Write: 'Writing', never the naive name+ing",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Write","input":{"file_path":"/workspace/b.ts","content":"hi"}}]}}`,
			want:  "Writing /workspace/b.ts",
			exact: true,
		},
		{
			name: "unknown tool: generic fallback still names it",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Grep","input":{"pattern":"needle"}}]}}`,
			want: "Grep(",
		},
		{
			name:  "no input at all: never an empty gloss",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Bash"}]}}`,
			want:  "Bash(...)",
			exact: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newParser(t)
			events, err := p.Parse([]byte(tc.line + "\n"))
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, toolkitstream.EventToolUseStart, events[0].Type)

			if tc.exact {
				assert.Equal(t, tc.want, events[0].Summary)
				return
			}
			assert.Contains(t, events[0].Summary, tc.want)
		})
	}
}

func TestParser_ErrorResult_PropagatesOKFalse(t *testing.T) {
	p := newParser(t)
	events, err := p.Parse(loadFixture(t, "error_result.ndjson"))
	require.NoError(t, err)

	var stopEv *toolkitstream.Event
	var resultEv *toolkitstream.Event
	for i := range events {
		if events[i].Type == toolkitstream.EventToolUseStop {
			stopEv = &events[i]
		}
		if events[i].Type == toolkitstream.EventResult {
			resultEv = &events[i]
		}
	}
	require.NotNil(t, stopEv)
	require.NotNil(t, resultEv)
	assert.False(t, stopEv.OK)
	assert.False(t, resultEv.OK)
}

func TestParser_SplitChunks_AssembleEvents(t *testing.T) {
	raw := loadFixture(t, "happy_path.ndjson")
	p := newParser(t)
	var all []toolkitstream.Event
	for i := 0; i < len(raw); i++ {
		evs, err := p.Parse(raw[i : i+1])
		require.NoError(t, err)
		all = append(all, evs...)
	}
	done, err := p.Done()
	require.NoError(t, err)
	all = append(all, done...)
	require.GreaterOrEqual(t, len(all), 2)
}

func TestParser_MalformedLineDropped(t *testing.T) {
	p := newParser(t)
	events, err := p.Parse([]byte("{not json}\n{\"type\":\"result\",\"subtype\":\"success\",\"total_cost_usd\":0,\"duration_ms\":0}\n"))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, toolkitstream.EventResult, events[0].Type)
}

func TestParser_BufferOverflow_TruncatesAndRecovers(t *testing.T) {
	p := newParser(t)
	var oversized [2 << 20]byte
	for i := range oversized {
		oversized[i] = 'x'
	}
	events, err := p.Parse(oversized[:])
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 1)
	assert.Equal(t, toolkitstream.EventTextDelta, events[0].Type)
	assert.Contains(t, events[0].Text, "[stream truncated]")

	events2, err := p.Parse([]byte(`{"type":"result","subtype":"success","total_cost_usd":0,"duration_ms":0}` + "\n"))
	require.NoError(t, err)
	require.Len(t, events2, 1)
	assert.Equal(t, toolkitstream.EventResult, events2[0].Type)
}

func TestParser_Outcome_CapturesResultText(t *testing.T) {
	p := newParser(t)
	_, err := p.Parse(loadFixture(t, "happy_path.ndjson"))
	require.NoError(t, err)
	_, err = p.Done()
	require.NoError(t, err)

	oc := p.Outcome()
	assert.True(t, oc.HasResult, "result event seen")
	assert.True(t, oc.OK, "subtype=success → OK")
	assert.Equal(t, "Hello! I'll help with that.", oc.Text, "result event's `result` text is the caller-facing payload")
	assert.Equal(t, int64(4321), oc.DurationMs)
	assert.InDelta(t, 0.0123, oc.CostUSD, 1e-6)
}

func TestParser_Outcome_ErrorSubtype_HasResultOKFalse(t *testing.T) {
	p := newParser(t)
	_, err := p.Parse(loadFixture(t, "error_result.ndjson"))
	require.NoError(t, err)
	_, err = p.Done()
	require.NoError(t, err)

	oc := p.Outcome()
	assert.True(t, oc.HasResult, "result event seen")
	assert.False(t, oc.OK, "subtype=error → not OK")
	assert.False(t, oc.Unbilled, "the run was billed $0.005, so it reached the provider's metered path")
	assert.False(t, oc.UnbilledFailure(),
		"a failure the provider CHARGED for is work that ran and then failed — not a credential it refused")
}

// TestParser_Outcome_UnbilledFailure_IsAuthShaped covers the run that never got
// past the provider's auth layer: a terminal result the CLI declares
// unsuccessful, with an empty billing tally.
//
// The tally is PRESENT and zero here, which is the toolkit stating that the
// provider metered nothing — the only shape that licenses the Unbilled claim.
// `CostUSD == 0` alone could not: it cannot tell "the provider charged nothing"
// from "this output does not report cost". See
// TestParser_Outcome_CostPresenceDecidesUnbilled for the absent case.
func TestParser_Outcome_UnbilledFailure_IsAuthShaped(t *testing.T) {
	p := newParser(t)
	_, err := p.Parse(loadFixture(t, "unbilled_failure.ndjson"))
	require.NoError(t, err)
	_, err = p.Done()
	require.NoError(t, err)

	oc := p.Outcome()
	assert.True(t, oc.HasResult, "result event seen")
	assert.False(t, oc.OK, "subtype=error_during_execution → not OK")
	assert.True(t, oc.Unbilled, "total_cost_usd:0 means the provider metered nothing")
	assert.True(t, oc.UnbilledFailure())
}

// TestParser_Outcome_BilledSuccess_IsNotAuthShaped is the injection control at
// the parser boundary: the happy-path fixture's own text is innocuous, so
// re-run it here with the auth-error text the CLI would have printed and check
// that the classifier still reads only the structured fields.
func TestParser_Outcome_BilledSuccess_IsNotAuthShaped(t *testing.T) {
	p := newParser(t)
	_, err := p.Parse([]byte(`{"type":"result","subtype":"success","result":"API Error: 401 invalid x-api-key","total_cost_usd":0.0123,"duration_ms":4321}` + "\n"))
	require.NoError(t, err)
	_, err = p.Done()
	require.NoError(t, err)

	oc := p.Outcome()
	require.Contains(t, oc.Text, "401", "precondition: the tool's own output reads like an auth failure")
	assert.False(t, oc.UnbilledFailure(),
		"a billed success stays a success however its output reads; the text is the agent's to influence")
}

// TestParser_Outcome_CostPresenceDecidesUnbilled separates the two statements a
// zero CostUSD can stand for, which is the whole reason Outcome.Unbilled exists
// as a field rather than as `CostUSD == 0`.
//
// "The provider metered nothing" is what a refused credential looks like. "This
// output carries no tally" is what an unrecognized `result` subtype — or a
// toolkit release that stopped emitting the field — looks like. Read the second
// as the first and two ordinary tool failures reach toolguard's
// AuthHaltThreshold, ending the session with a message telling the operator
// their sign-in is no longer being accepted: someone is sent to rotate a
// credential that was never the problem.
//
// So the claim this parser is entitled to make depends on the field being
// PRESENT, and absence has to leave Unbilled false rather than inherit
// float64's zero.
func TestParser_Outcome_CostPresenceDecidesUnbilled(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantUnbilled bool
	}{
		{
			name:         "total_cost_usd absent: says nothing about billing, so no credential halt",
			line:         `{"type":"result","subtype":"error_during_execution","result":"something went wrong","duration_ms":812}`,
			wantUnbilled: false,
		},
		{
			name:         "total_cost_usd present and zero: the toolkit states nothing was metered",
			line:         `{"type":"result","subtype":"error_during_execution","result":"something went wrong","total_cost_usd":0,"duration_ms":812}`,
			wantUnbilled: true,
		},
		{
			name:         "total_cost_usd present and non-zero: the run reached the metered path",
			line:         `{"type":"result","subtype":"error_during_execution","result":"something went wrong","total_cost_usd":0.005,"duration_ms":812}`,
			wantUnbilled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newParser(t)
			_, err := p.Parse([]byte(tc.line + "\n"))
			require.NoError(t, err)
			_, err = p.Done()
			require.NoError(t, err)

			oc := p.Outcome()
			require.True(t, oc.HasResult, "precondition: the terminal result was recognized")
			require.False(t, oc.OK, "precondition: every case here is a FAILED run, so only the tally differs")

			assert.Equal(t, tc.wantUnbilled, oc.Unbilled)
			assert.Equal(t, tc.wantUnbilled, oc.UnbilledFailure(),
				"a failed run with HasResult classifies exactly as Unbilled does")
		})
	}
}
