package steelthread_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// sandboxSuccess is the text a SUCCESSFUL sandbox call composes: stdout, then
// the tool's own trailer carrying per-run artifact refs.
func sandboxSuccess(stdout string) string {
	return stdout + "[exit=0; artifacts: stdout=mem://ns/sess/8f2c/stdout, stderr=mem://ns/sess/8f2c/stderr]\n"
}

// sandboxFold folds a one-call sandbox transcript. The class declares
// toolBundle "sre" whose class tool is "sre-tool", so the LLM-facing name is
// "sre_sre-tool".
func sandboxFold(t *testing.T, result string, isError bool) steelthread.Folded {
	t.Helper()
	recs := steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "list the clusters"),
			assistantCall(1, "tu_1", "sre_sre-tool",
				`{"operation_id":"op-1","_reason":"asked","args":["list-clusters"]}`),
			toolResult(2, "tu_1", result, isError),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"done"}`),
		},
	}
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{SandboxPrefixes: []string{"sre"}})
	require.NoError(t, err, "folding a sandbox transcript")
	return folded
}

// A sandbox result is folded into the SAME ToolOutputs map an MCP result goes
// into — a bundle replays black boxes — but as a SandboxOutput, and with the
// per-run trailer removed.
func TestFold_ASandboxResultBecomesAToolOutput(t *testing.T) {
	folded := sandboxFold(t, sandboxSuccess("cluster-a\ncluster-b\n"), false)

	require.Contains(t, folded.ToolOutputs, "sre_sre-tool",
		"a sandbox result is keyed by the LLM-facing name, not the bare class tool")
	assert.JSONEq(t, `{"stdout":"cluster-a\ncluster-b\n"}`, string(folded.ToolOutputs["sre_sre-tool"]))
	assert.Empty(t, folded.UnreplayableSandboxTools)
	assert.Empty(t, folded.SecretOutputTools)
}

// The artifact refs in the trailer embed the session name and the ToolCall UID,
// so they differ on every run. Carried into the bundle they would be handed
// back as if the process had printed them; pinned in an Expect they would fail
// every replay of a correct bundle.
func TestFold_ASandboxResultDropsThePerRunTrailer(t *testing.T) {
	folded := sandboxFold(t, sandboxSuccess("cluster-a\n"), false)

	assert.NotContains(t, string(folded.ToolOutputs["sre_sre-tool"]), "mem://",
		"an artifact ref must not reach the recorded stdout")
	assert.NotContains(t, string(folded.ToolOutputs["sre_sre-tool"]), "exit=",
		"the trailer is the tool's own bookkeeping, not process output")

	require.Len(t, folded.LLM, 2, "one step per assistant turn")
	step := folded.LLM[1]
	assert.Equal(t, "sre_sre-tool", step.Expect.LastToolResult)
	assert.Equal(t, "cluster-a\n", step.Expect.LastToolResultContains,
		"the assertion is narrowed to the half a replay reproduces")
	assert.NotContains(t, step.Expect.LastToolResultContains, "8f2c",
		"pinning the ToolCall UID would fail every replay of a correct bundle")
}

// A producer's result is replaced wholesale by the runner with a description
// line and an opaque handle; the raw value never reaches the transcript. The
// handle and byte count are per-run and must not be pinned either.
func TestFold_ASecretOutputResultRecordsTheDescriptionAndIsReported(t *testing.T) {
	folded := sandboxFold(t,
		"wrote the kubeconfig for cluster-a\n<secret-output name=\"kubeconfig\" ref=\"h7\" bytes=4211>", false)

	assert.JSONEq(t, `{"stdout":"wrote the kubeconfig for cluster-a\n"}`,
		string(folded.ToolOutputs["sre_sre-tool"]))
	assert.Equal(t, []string{"sre_sre-tool"}, folded.SecretOutputTools,
		"the reader must be told this is a diversion's description, not observed process output")
	assert.Empty(t, folded.UnreplayableSandboxTools)

	step := folded.LLM[1]
	assert.Equal(t, "wrote the kubeconfig for cluster-a", step.Expect.LastToolResultContains)
	assert.NotContains(t, step.Expect.LastToolResultContains, "h7", "the handle is per-run")
	assert.NotContains(t, step.Expect.LastToolResultContains, "bytes=", "the byte count is per-run")
}

// A FAILED call composes a summary of the ToolCall's terminal CONDITION, with
// no stdout in it. The one condition a replay can be driven into from the
// process exit code alone is NonZeroExit, and that one round-trips exactly:
// the recorded exit code and stderr tail go into the bundle, the fake exec
// binder hands them back, and the replayed sandbox tool re-composes the same
// summary line from the code under test. Nothing is invented.
func TestFold_AFailedSandboxCallIsCapturedAsAFailingProcess(t *testing.T) {
	folded := sandboxFold(t,
		"ToolCall failed: NonZeroExit — exit code 3\nstderr (last 4096 bytes):\nboom\n", true)

	assert.JSONEq(t, `{"stdout":"","stderr":"boom","exitCode":3}`,
		string(folded.ToolOutputs["sre_sre-tool"]),
		"the exit code is the lever; the summary line is re-composed by the replay, not carried")
	assert.Empty(t, folded.UnreplayableSandboxTools)

	step := folded.LLM[1]
	require.NotNil(t, step.Expect.LastToolResultIsError)
	assert.True(t, *step.Expect.LastToolResultIsError,
		"a captured failure must replay as a failure, or the assertion passes on a call that succeeded")
	assert.Contains(t, step.Expect.LastToolResultContains, "exit code 3")
}

// The conditions an exit code CANNOT reach. Each is a terminal condition our
// own watchdog or the controller wrote for a reason the process's status word
// does not carry, so serving an exit code would re-compose a different summary
// than the model saw.
func TestFold_AFailureNoExitCodeCanReachIsNamedRatherThanCanned(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{
			name:   "a Timeout: our watchdog's condition, not the process's",
			result: "ToolCall failed: Timeout — exceeded 300s\n",
		},
		{
			name:   "a Cancellation",
			result: "ToolCall failed: Canceled — session ended\n",
		},
		{
			// The REASON is what is checked, not just the message shape. A
			// killed process reports an exit code like any other, so a
			// Cancellation whose message reads "exit code 143" is a plausible
			// record — and inverting it would make the replay re-compose
			// "NonZeroExit — exit code 143", which is different bytes for a
			// different reason. Without this case, dropping the reason check
			// entirely goes unnoticed.
			name:   "a condition OTHER than NonZeroExit whose message still reads like an exit code",
			result: "ToolCall failed: Canceled — exit code 143\n",
		},
		{
			name:   "an exec error: the process could not be run at all",
			result: "ToolCall failed: NonZeroExit — dial tcp: connection refused\n",
		},
		{
			// Serving 0 would replay as a SUCCESS — the composition inverts to
			// something that is not a failure at all.
			name:   "an exit code of 0",
			result: "ToolCall failed: NonZeroExit — exit code 0\n",
		},
		{
			// The original bytes are gone; feeding the truncated text back
			// yields a shorter stderr the replay reads in full.
			name: "a stderr tail the tool truncated",
			result: "ToolCall failed: NonZeroExit — exit code 3\nstderr (last 4096 bytes):\n" +
				"boom\n...(truncated)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			folded := sandboxFold(t, tc.result, true)

			assert.NotContains(t, folded.ToolOutputs, "sre_sre-tool",
				"a failure the fold cannot express must not become a canned result")
			require.Len(t, folded.UnreplayableSandboxTools, 1)
			assert.Equal(t, "sre_sre-tool", folded.UnreplayableSandboxTools[0].Tool)
			assert.True(t, folded.UnreplayableSandboxTools[0].Errored,
				"the refusal must say the result was an ERROR, or the finding describes the wrong cause")
			assert.False(t, folded.UnreplayableSandboxTools[0].Succeeded)
		})
	}
}

// A GATE refusal cans NOTHING, exactly as on the MCP path.
//
// The call never reached the sandbox — a denied tool never reaches the sandbox
// or the MCP server — so the replay boots the same fixture, with the same
// derived seed and the same gate configuration, and refuses it again. Canning
// the gate's own refusal TEXT would be worse than useless: a gate that STOPPED
// refusing would reach the exec binder, be served that text back, and satisfy
// the very assertion meant to catch it, so the bundle would pass with the
// permission boundary broken. With nothing registered, the step fails by name.
func TestFold_AGateRefusedSandboxCallCansNothing(t *testing.T) {
	const refusal = "permission denied: alice does not have read on github_repo:demo"
	recs := steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "list the clusters"),
			assistantCall(1, "tu_1", "sre_sre-tool",
				`{"operation_id":"op-1","_reason":"asked","args":["list-clusters"]}`),
			toolResult(2, "tu_1", refusal, true),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"denied"}`),
		},
		// The message anchor is what makes this a PROOF rather than an
		// inference: an authz Deny hands back res.Message verbatim, and that is
		// the same string recordAuthzDecision stored.
		DecisionsByToolCall: map[string][]authzdecision.Decision{
			"tu_1": {{
				Outcome: authzdecision.OutcomeDenied, Subject: "alice",
				ResourceType: "github_repo", ResourceID: "demo", Permission: "read",
				Message: refusal,
			}},
		},
	}
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{SandboxPrefixes: []string{"sre"}})
	require.NoError(t, err)

	assert.NotContains(t, folded.ToolOutputs, "sre_sre-tool",
		"a gate refusal must not be canned; the replay's own gate regenerates it")
	assert.Empty(t, folded.UnreplayableSandboxTools,
		"nor is it a refusal to capture: there is simply nothing to record")
}

// A SUCCESSFUL STREAMING result is carried VERBATIM and served back, because
// nothing durable holds the toolkit's wire stream and the composed text is the
// only artifact of the call that survives. The whole text, header included: the
// replay wraps it in a header of its own rather than replacing this one, so
// every recorded byte is still there to assert on.
func TestFold_ASuccessfulStreamingResultIsCarriedVerbatim(t *testing.T) {
	const recorded = "status: success (7.272s, $0.09)\nI reviewed the file and found no typos."
	folded := sandboxFold(t, recorded, false)

	assert.Empty(t, folded.UnreplayableSandboxTools,
		"an ordinary streaming result is expressible now, so nothing is refused")
	require.Contains(t, folded.ToolOutputs, "sre_sre-tool")
	so, err := bt.DecodeSandboxOutput(folded.ToolOutputs["sre_sre-tool"])
	require.NoError(t, err)
	assert.Equal(t, recorded, so.StreamResult,
		"the recorded result is carried whole, header and all")
	assert.Empty(t, so.Stdout, "it is not process output and must not be filed as any")
	assert.Equal(t, []string{"sre_sre-tool"}, folded.StreamResultTools,
		"and the tool is named, so the self-check can say the composition path went uncovered")
}

// The step's own assertion is derived from the SAME bytes, whole. A capture
// that stripped the status header here would leave the duration and the cost —
// the part of the result a reader most wants pinned — unasserted.
func TestFold_AStreamingResultStepAssertsOnTheWholeRecordedText(t *testing.T) {
	const recorded = "status: success (7.272s, $0.09)\nI reviewed the file and found no typos."
	folded := sandboxFold(t, recorded, false)

	require.Len(t, folded.LLM, 2)
	assert.Equal(t, "sre_sre-tool", folded.LLM[1].Expect.LastToolResult)
	assert.Equal(t, recorded, folded.LLM[1].Expect.LastToolResultContains)
}

// The two streaming shapes a bundle still cannot serve back, and each for a
// reason the served text could not carry.
func TestFold_AStreamingResultTheBundleCannotServeIsStillRefused(t *testing.T) {
	cases := []struct {
		name     string
		recorded string
	}{{
		// Terminal=true ends the turn; replayed as an ordinary result the run
		// would continue past where the recording stopped.
		name:     "an idle exit parks the session, so it is refused",
		recorded: "status: idle (exit 0)\nwaiting for input",
	}, {
		// The replayed tool folds a bounded stdout TAIL into its result, so an
		// over-budget recording arrives with its head missing.
		name:     "a result over the stdout-tail budget is refused",
		recorded: "status: success (7.272s, $0.09)\n" + strings.Repeat("x", bt.StreamResultBudgetBytes),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			folded := sandboxFold(t, tc.recorded, false)

			assert.NotContains(t, folded.ToolOutputs, "sre_sre-tool")
			assert.Empty(t, folded.StreamResultTools)
			require.Len(t, folded.UnreplayableSandboxTools, 1)
			assert.Equal(t, "sre_sre-tool", folded.UnreplayableSandboxTools[0].Tool)
			assert.True(t, folded.UnreplayableSandboxTools[0].Succeeded)
			assert.False(t, folded.UnreplayableSandboxTools[0].Errored,
				"this call SUCCEEDED; reporting it as a failure misdescribes it")
		})
	}
}

// ---- plan-minted operation ids ------------------------------------------

// planMintedRecords is the shape the sentinel scheme could not express, and
// which the recorded sequence handles directly: update_plan mints one operation
// per item it moves to in_progress and returns them NESTED inside its item
// array, so a table mapping one sentinel to one remembered value could not tell
// three of them apart. The capture records all three, in item order, and the
// replay's minter hands them back in the same order.
func planMintedRecords() steelthread.Records {
	return steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "do the work"),
			assistantCall(1, "tu_1", "update_plan", `{"name":"main"}`),
			toolResult(2, "tu_1", `{"items":[`+
				`{"id":"s1","operation_id":"op-1111111111111111"},`+
				`{"id":"s2","operation_id":"op-2222222222222222"},`+
				`{"id":"s3","operation_id":"op-3333333333333333"}]}`, false),
			assistantCall(3, "tu_2", "acme_list_widgets",
				`{"operation_id":"op-2222222222222222","_reason":"asked","args":{}}`),
			toolResult(4, "tu_2", `{"results":[]}`, false),
		},
	}
}

// The regression this whole change exists for: a session whose only operation
// ids come from a plan used to be REFUSED (four literal ids rode through
// unsubstituted on the session that motivated this), and now emits clean with
// the ids pinned.
func TestSelfCheck_APlanMintedOperationIDIsCleanAndPinned(t *testing.T) {
	recs := planMintedRecords()
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{MCPPrefixes: []string{"acme"}})
	require.NoError(t, err)

	assert.Equal(t,
		[]string{"op-1111111111111111", "op-2222222222222222", "op-3333333333333333"},
		folded.MintedIDs[bt.FamilyOperation],
		"every id the plan minted, in the order plans.Update minted them")

	var args map[string]any
	require.NoError(t, json.Unmarshal(folded.LLM[1].Reply[0].ToolUse.Args, &args))
	assert.Equal(t, "op-2222222222222222", args["operation_id"],
		"and the call that used the SECOND one keeps naming it literally")

	bundle := bt.Bundle{
		Name: "n", AgentDir: "testdata/n", AgentClass: "c",
		UserTurns: bundleTurns(folded.UserTurns), LLM: folded.LLM, MintedIDs: folded.MintedIDs,
	}
	require.NoError(t, bundle.Validate(), "the emitted bundle must load")

	got := steelthread.SelfCheck(steelthread.SelfCheckInput{
		Records:       recs,
		Folded:        folded,
		Bundle:        bundle,
		DeclaredTools: map[string][]string{"acme": {"list_widgets"}},
		MetaTools:     []string{"update_plan"},
		FixtureTools:  []steelthread.FixtureTool{{Name: "update_plan"}},
		LiveSecrets:   []steelthread.LiveSecret{{Name: "x/y", Value: "zzz-fixture-only"}},
	})
	for _, f := range got {
		assert.NotEqual(t, steelthread.SeverityHard, f.Severity,
			"a plan-minted operation is no longer a reason to refuse a capture: %s — %s", f.Code, f.Message)
	}
}

// The negative control for the SHAPE rule. A hand-authored bundle passes ids the
// author chose ("op-1"), and filing those into the sequence would make the
// replay claim mints that never happened — shifting every real id after them.
func TestFold_AnAuthoredOperationIDIsNotRecordedAsMinted(t *testing.T) {
	recs := planMintedRecords()
	// The plan STILL mints three ids, so the recorded sequence is non-empty and
	// the rule does real work; only the AUTHORED id in the call must be absent.
	recs.Turns[3] = assistantCall(3, "tu_2", "acme_list_widgets",
		`{"operation_id":"op-authored-9","_reason":"asked","args":{}}`)
	recs.Turns[4] = toolResult(4, "tu_2", `{"echoed_operation_id":"op-authored-9"}`, false)

	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{MCPPrefixes: []string{"acme"}})
	require.NoError(t, err)

	assert.NotContains(t, folded.MintedIDs[bt.FamilyOperation], "op-authored-9",
		"an id that is not the minted shape was chosen by an author, not minted by a run")
	assert.Len(t, folded.MintedIDs[bt.FamilyOperation], 3,
		"and the ids the run really did mint are still all there")
}

// A name that does not carry a declared sandbox prefix is not a sandbox call,
// however much it looks like one. The prefixes come from the class; guessing
// from the name is what would silently can a meta tool that runs for real.
func TestFold_AnUndeclaredPrefixIsNotASandboxCall(t *testing.T) {
	recs := steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "go"),
			assistantCall(1, "tu_1", "other_sre-tool", `{}`),
			toolResult(2, "tu_1", sandboxSuccess("out\n"), false),
		},
	}
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{SandboxPrefixes: []string{"sre"}})
	require.NoError(t, err)

	assert.Empty(t, folded.ToolOutputs, "no declared prefix matched, so nothing is canned")
	assert.Empty(t, folded.UnreplayableSandboxTools,
		"an unmatched name is checkToolCalls' business, not the sandbox fold's")
}

// TestSelfCheck_UnreplayableSandboxFindingsDescribeTheRightCause pins that the
// two refusals read differently.
//
// They used to be one message, and it said "A FAILED call is the usual cause"
// about a call that SUCCEEDED — which is how a live capture came to be reported
// as a failure that never happened, sending a reader looking for an exit code
// in a session where the tool ran fine. The remedies differ too: one names the
// conditions an exit code cannot reach, the other names a bundle-format gap.
func TestSelfCheck_UnreplayableSandboxFindingsDescribeTheRightCause(t *testing.T) {
	cases := []struct {
		name     string
		refusal  steelthread.SandboxRefusal
		contains []string
		absent   []string
	}{
		{
			name:     "a failed call names the conditions an exit code cannot reach",
			refusal:  steelthread.SandboxRefusal{Tool: "sre_sre-tool", Errored: true},
			contains: []string{"FAILED", "Timeout", "TRUNCATED", "sre_sre-tool"},
			absent:   []string{"STREAMING"},
		},
		{
			name:    "a successful call names the two streaming shapes still not servable",
			refusal: steelthread.SandboxRefusal{Tool: "codelike_claude", Succeeded: true},
			contains: []string{
				"SUCCESSFUL", "STREAMING", "IDLE", "16384", "codelike_claude",
			},
			absent: []string{"Timeout"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			in.Folded.UnreplayableSandboxTools = []steelthread.SandboxRefusal{tc.refusal}

			f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeUnreplayableSandboxResult)
			assert.Equal(t, steelthread.SeverityHard, f.Severity)
			for _, want := range tc.contains {
				assert.Contains(t, f.Message, want)
			}
			for _, notWant := range tc.absent {
				assert.NotContains(t, f.Message, notWant,
					"a message describing the other cause sends the reader after the wrong thing")
			}
		})
	}
}

// TestSelfCheck_StreamResultServedVerbatimIsAWarningThatNamesTheCost pins the
// finding a bundle carrying a black-boxed streaming result must emit.
//
// A WARNING, not a refusal: the bundle is legitimate — the toolkit is a black
// box, the recorded call goes in and the recorded result comes back, which is
// the contract every canned MCP result already takes. But it is not the same as
// canning an MCP server's response, because half of THIS boundary is ours, and
// a reader must be able to tell a bundle that exercises the stream composition
// from one that replays its output. Nothing in the emitted JSON says so on its
// own, which is exactly why the finding has to.
func TestSelfCheck_StreamResultServedVerbatimIsAWarningThatNamesTheCost(t *testing.T) {
	in := cleanCapture(t)
	in.Folded.StreamResultTools = []string{"codelike_claude-oauth"}

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeStreamResultServedVerbatim)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity,
		"the bundle is legitimate; refusing it would refuse every streaming session")
	assert.Contains(t, f.Message, "codelike_claude-oauth", "the finding names the tool")
	assert.Contains(t, f.Message, "NOT covered by this bundle",
		"and says plainly that the stream-composition path went uncovered")
}

// The collision the replay cannot route. One toolBundle is one sandbox pod, and
// the fake exec binder's streaming half is keyed by POD with no argv to
// dispatch on — so a pod holding two recorded streams would answer both tools
// with whichever was registered, and the bundle would replay green with one
// tool served the other's output.
func TestSelfCheck_TwoStreamResultsInOneToolBundleIsRefused(t *testing.T) {
	in := cleanCapture(t)
	in.SandboxTools = map[string][]string{"codelike": {"claude-oauth", "codex"}}
	in.Folded.StreamResultTools = []string{"codelike_claude-oauth", "codelike_codex"}

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeStreamResultCollision)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "codelike", "the finding names the toolBundle")
	assert.Contains(t, f.Message, "codelike_claude-oauth")
	assert.Contains(t, f.Message, "codelike_codex")
}

// The negative control the collision check needs: two streaming tools in
// DIFFERENT toolBundles are two pods, so each gets its own stream program and
// nothing is ambiguous.
func TestSelfCheck_TwoStreamResultsInDifferentToolBundlesAreFine(t *testing.T) {
	in := cleanCapture(t)
	in.SandboxTools = map[string][]string{
		"codelike": {"claude-oauth"},
		"agentic":  {"codex"},
	}
	in.Folded.StreamResultTools = []string{"agentic_codex", "codelike_claude-oauth"}

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeStreamResultCollision, f.Code,
			"one toolBundle is one pod, so two bundles are two pods and neither is ambiguous")
	}
}
