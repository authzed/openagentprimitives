package sandbox_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// TestComposeStreamResultMarksTheResultRenderedLive pins the producer half of
// the live == reload invariant. Everything composeStreamResult returns was
// streamed to the user as it happened — the tool-session block in web chat, the
// per-ToolCallRef message in Slack — and the status line it writes repeats the
// outcome that block's footer showed. Marking it is what lets a resumed
// transcript reproduce the conversation instead of silently dropping the whole
// sub-agent run.
//
// Every exit reason is covered, the failing ones included: a failed run is the
// one the live surfaces keep EXPANDED, so hiding it on reload would lose the
// output a user most wants to re-read.
func TestComposeStreamResultMarksTheResultRenderedLive(t *testing.T) {
	cases := []struct {
		name    string
		bridge  sandbox.BridgeResult
		outcome *toolkitstream.Outcome
		stdout  []byte
		stderr  []byte
	}{
		{
			name:    "completed run with a parsed final answer",
			bridge:  sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{HasResult: true, OK: true, Text: "no typos found"},
			stdout:  []byte("done\n"),
		},
		{
			name:   "completed run with no parser: the raw stdout tail",
			bridge: sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			stdout: []byte("no typos found\n"),
		},
		{
			name:   "failed run: still shown, live kept it expanded",
			bridge: sandbox.BridgeResult{ExitCode: 4, ExitReason: "failed"},
			stdout: []byte("partial\n"),
			stderr: []byte("could not clone\n"),
		},
		{
			name:    "clean exit the parser reported as not-OK",
			bridge:  sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{HasResult: true, OK: false, Text: "the model declined"},
		},
		{
			name:   "idle park",
			bridge: sandbox.BridgeResult{ExitCode: 0, ExitReason: "idle"},
			stdout: []byte("waiting\n"),
		},
		{
			name:   "maxDuration kill",
			bridge: sandbox.BridgeResult{ExitCode: 0, ExitReason: "maxDuration"},
			stdout: []byte("still working\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sandbox.ComposeStreamResult(tc.bridge, tc.outcome, tc.stdout, tc.stderr)
			assert.True(t, res.RenderedLive,
				"the user watched this run; a resumed transcript must be able to show it")
			require.NotEmpty(t, res.Content, "precondition: the composed result carries a status line")
		})
	}
}

// TestStreamingToolPreDispatchErrorIsNotRenderedLive is why the mark is carried
// per CALL rather than declared by the tool: a streaming tool's argument
// checks fail BEFORE any stream exists, so nothing was ever on screen. Keying
// the mark on the tool's mode would resurrect these into the transcript as
// agent messages the user never saw.
func TestStreamingToolPreDispatchErrorIsNotRenderedLive(t *testing.T) {
	st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "codelike",
		Suffix:     "subagent",
		Subcommand: &toolkit.Subcommand{Mode: toolkit.SubcommandModeStream},
	})

	// No InteractiveHooks in the context: the runner is not channel-attached,
	// so the dispatch is refused before the bridge is ever dialed.
	res, err := st.Execute(context.Background(), json.RawMessage(`{}`), &tool.SessionContext{})
	require.NoError(t, err)
	require.True(t, res.IsError, "precondition: this is the refused-dispatch path")
	assert.False(t, res.RenderedLive,
		"nothing streamed, so nothing was shown; a reload must not invent an agent message here")
}
