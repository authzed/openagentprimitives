package steelthread_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

const nonce = "deadbeef12345678"

// bundleTurns is the assembly step's own lift from Folded's text turns to the
// bundle's turn type, for tests that hand a Folded straight to a bt.Bundle.
// Mirrors capture.go's userTurns: a folded turn is text, never an attachment.
func bundleTurns(in []string) []bt.UserTurn {
	out := make([]bt.UserTurn, 0, len(in))
	for _, text := range in {
		out = append(out, bt.UserTurn{Text: text})
	}
	return out
}

func userText(idx int, s string) memory.Turn {
	return memory.Turn{Index: idx, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: s}}}
}

func assistantCall(idx int, useID, name, args string) memory.Turn {
	return memory.Turn{Index: idx, Role: "assistant", Model: "demoprovider/demo-model-1",
		Content: []memory.ContentBlock{{Type: "tool_use", ToolUse: &memory.ToolUseBlock{
			ID: useID, Name: name, Input: []byte(args),
		}}}}
}

func toolResult(idx int, useID, payload string, isErr bool) memory.Turn {
	return memory.Turn{Index: idx, Role: "user",
		Content: []memory.ContentBlock{{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
			ToolUseID: useID, Content: toolenvelope.Wrap(payload, nonce), IsError: isErr,
		}}}}
}

func foldOpts() steelthread.FoldOptions {
	return steelthread.FoldOptions{MCPPrefixes: []string{"acme"}}
}

// assistantCalls builds the turn a PARALLEL tool call produces: one assistant
// turn carrying several tool_use blocks.
func assistantCalls(idx int, calls ...memory.ToolUseBlock) memory.Turn {
	turn := memory.Turn{Index: idx, Role: "assistant", Model: "demoprovider/demo-model-1"}
	for i := range calls {
		turn.Content = append(turn.Content, memory.ContentBlock{Type: "tool_use", ToolUse: &calls[i]})
	}
	return turn
}

// resultBlock is one tool_result, enveloped the way the runner persists it.
func resultBlock(useID, payload string, isErr bool) *memory.ToolResultBlock {
	return &memory.ToolResultBlock{ToolUseID: useID, Content: toolenvelope.Wrap(payload, nonce), IsError: isErr}
}

// multiToolResult builds the ONE turn the runner writes back for a parallel
// tool call — every result packed into a single turn, in tool_use order.
func multiToolResult(idx int, results ...*memory.ToolResultBlock) memory.Turn {
	turn := memory.Turn{Index: idx, Role: "user"}
	for _, r := range results {
		turn.Content = append(turn.Content, memory.ContentBlock{Type: "tool_result", ToolResult: r})
	}
	return turn
}

// TestFold_HappyPath is the shape every capture produces: a person asks, the
// model calls an MCP tool, gets a result, and answers.
func TestFold_HappyPath(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "list the widgets"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"operation_id":"op-1","_reason":"asked","args":{}}`),
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"no widgets found"}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"list the widgets"}, got.UserTurns)
	assert.Equal(t, "demoprovider/demo-model-1", got.ServedModel)
	require.Len(t, got.LLM, 2, "two assistant turns must produce two steps")

	// Step 0 answers the user's text.
	assert.Equal(t, "list the widgets", got.LLM[0].Expect.UserTextContains)
	require.Len(t, got.LLM[0].Reply, 1)
	require.NotNil(t, got.LLM[0].Reply[0].ToolUse)
	assert.Equal(t, "acme_list_widgets", got.LLM[0].Reply[0].ToolUse.Name)

	// Step 1 answers the tool result, and Expect names the tool it answers.
	assert.Equal(t, "acme_list_widgets", got.LLM[1].Expect.LastToolResult)
	assert.Equal(t, `{"results":[]}`, got.LLM[1].Expect.LastToolResultContains)
	require.NotNil(t, got.LLM[1].Expect.LastToolResultIsError)
	assert.False(t, *got.LLM[1].Expect.LastToolResultIsError,
		"isError must be pinned as false, not left unset — the difference between "+
			"'the call succeeded' and 'nobody checked' is the whole point of the field")

	// The MCP result is keyed SERVER-side, with the envelope stripped.
	require.Contains(t, got.ToolOutputs, "list_widgets",
		"toolOutputs is keyed by the server-side name, not the LLM-facing one")
	assert.JSONEq(t, `{"results":[]}`, string(got.ToolOutputs["list_widgets"]))
}

// TestFold_MetaToolsAreNotReplayed pins the disposition split. update_plan and
// the gate it drives ARE the code under test; replaying their output would make
// the bundle assert against its own input.
func TestFold_MetaToolsAreNotReplayed(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "update_plan", `{"name":"main"}`),
		toolResult(2, "tu_1", `{"ok":true}`, false),
		assistantCall(3, "tu_2", "acme_list_widgets", `{"args":{}}`),
		toolResult(4, "tu_2", `{"results":[1]}`, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.NotContains(t, got.ToolOutputs, "update_plan", "a RunReal meta tool contributes no canned output")
	assert.Contains(t, got.ToolOutputs, "list_widgets", "the MCP tool still does")
}

// TestFold_DifferingResultsBecomeASequence is the case a name->one-value map
// cannot hold, and the reason the format needed extending.
func TestFold_DifferingResultsBecomeASequence(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "acme_list_widgets", `{"args":{}}`),
		toolResult(4, "tu_2", `{"results":[{"id":"w1"}]}`, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.NotContains(t, got.ToolOutputs, "list_widgets")
	require.Len(t, got.ToolOutputSequence["list_widgets"], 2)
	assert.JSONEq(t, `{"results":[]}`, string(got.ToolOutputSequence["list_widgets"][0]))
	assert.JSONEq(t, `{"results":[{"id":"w1"}]}`, string(got.ToolOutputSequence["list_widgets"][1]))
}

// TestFold_IdenticalResultsStayConstant is the negative control: a tool called
// twice with the SAME result must not become a needless sequence, or every
// capture would be harder to read than it has to be.
func TestFold_IdenticalResultsStayConstant(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "acme_list_widgets", `{"args":{}}`),
		toolResult(4, "tu_2", `{"results":[]}`, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Contains(t, got.ToolOutputs, "list_widgets")
	assert.NotContains(t, got.ToolOutputSequence, "list_widgets")
}

// TestFold_MintedIDsAreRecordedAndArgsStayLiteral pins the inversion this
// format is built on: the fold does NOT rewrite a minted id, it RECORDS it, and
// the replay's own minters hand the same value back. The recorded argument is
// then correct by construction, with no substitution layer in between.
func TestFold_MintedIDsAreRecordedAndArgsStayLiteral(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "new_operation", `{"kind":"read"}`),
		toolResult(2, "tu_1", `{"operation_id":"op-7f3a1c0b45de9982"}`, false),
		assistantCall(3, "tu_2", "acme_list_widgets", `{"operation_id":"op-7f3a1c0b45de9982","args":{}}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"op-7f3a1c0b45de9982"}, got.MintedIDs[bt.FamilyOperation],
		"the id the run minted is recorded so the replay can mint it again")

	var args map[string]any
	require.NoError(t, json.Unmarshal(got.LLM[1].Reply[0].ToolUse.Args, &args))
	assert.Equal(t, "op-7f3a1c0b45de9982", args["operation_id"],
		"and the captured argument keeps the literal id it was called with")
}

// TestFold_MintedIDsRecordsNestedPlanIDsInItemOrder is the shape the sentinel
// scheme could not express and the reason it was replaced: update_plan mints one
// operation per item it moves to in_progress and returns them NESTED inside its
// item array. One remembered value cannot stand for three.
//
// Order is the assertion. plans.Update walks items in order, so array order IS
// mint order, and the replay draws from this list in the same order.
func TestFold_MintedIDsRecordsNestedPlanIDsInItemOrder(t *testing.T) {
	plan := `{"name":"main","items":[` +
		`{"id":"a","operation_id":"op-aaaaaaaaaaaaaaaa"},` +
		`{"id":"b","operation_id":"op-bbbbbbbbbbbbbbbb"},` +
		`{"id":"c","operation_id":"op-cccccccccccccccc"}]}`
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "update_plan", `{"name":"main"}`),
		toolResult(2, "tu_1", plan, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t,
		[]string{"op-aaaaaaaaaaaaaaaa", "op-bbbbbbbbbbbbbbbb", "op-cccccccccccccccc"},
		got.MintedIDs[bt.FamilyOperation],
		"every plan-minted id, at the depth it was returned, in the order Update minted them")
}

// TestFold_MintedIDsAreFiledByFamilyAndDeduped covers the three rules that keep
// the recorded sequence a faithful mint log: ids are filed by SHAPE (so a new
// family is a row in the table, not a new field name to read), an id seen twice
// is one mint, and a value that merely LOOKS id-adjacent is not recorded.
func TestFold_MintedIDsAreFiledByFamilyAndDeduped(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "artifact_prepare", `{"kind":"html"}`),
		toolResult(2, "tu_1", `{"handle":"ar-sess-1a2b3c","artifact_id":"artifact-1111111111111111",`+
			`"revision_id":"artrev-2222222222222222","status":"ready"}`, false),
		assistantCall(3, "tu_2", "artifact_offer_view", `{"artifact_id":"artifact-1111111111111111"}`),
		// The SAME artifact echoed back by a second tool: one mint, not two.
		toolResult(4, "tu_2", `{"artifact_id":"artifact-1111111111111111","offered":true}`, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"artifact-1111111111111111"}, got.MintedIDs[bt.FamilyArtifact],
		"an id echoed by a later result was minted once")
	assert.NotContains(t, got.MintedIDs, bt.FamilyOperation,
		"a family the run never minted for is absent, so the replay leaves it unpinned")
	// The other two values artifact_prepare hands back are pinned by their own
	// families now, and the filing is still by SHAPE — nothing here reads a
	// field name. The handle is the case the shape rule had to grow for: its
	// body is a session name plus a six-hex tail, not the sixteen-hex body the
	// memory kinds emit, so its family carries its own matcher.
	assert.Equal(t, []string{"artrev-2222222222222222"}, got.MintedIDs[bt.FamilyArtifactRevision],
		"a revision id is pinned: the replay derives it from a UID it does not control")
	assert.Equal(t, []string{"ar-sess-1a2b3c"}, got.MintedIDs[bt.FamilyRenderHandle],
		"a render handle is pinned: the run chose it, and it leaves as the model-facing handle")
}

// TestFold_MintedIDsIgnoresProseMentioningAnID is the negative control for the
// whole-string rule. A tool that NAMES an id in its message did not mint one,
// and recording it would make the replay's sequence claim a mint that never
// happened — shifting every id after it.
func TestFold_MintedIDsIgnoresProseMentioningAnID(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", `{"message":"operation op-7f3a1c0b45de9982 is not registered"}`, false),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.MintedIDs, "an id mentioned inside a sentence was not minted by this result")
}

// TestFold_RespondToUserAndBareTextAreDistinct pins the two text shapes. A real
// model narrates before it acts; the narration is a bare text block, and only
// respond_to_user is a message to a person.
func TestFold_RespondToUserAndBareTextAreDistinct(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
			{Type: "text", Text: "Let me look that up."},
			{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "tu_1", Name: "acme_list_widgets", Input: []byte(`{}`)}},
		}},
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"none found"}`),
	}}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM[0].Reply, 2)
	assert.Equal(t, "Let me look that up.", got.LLM[0].Reply[0].BareText)
	assert.Empty(t, got.LLM[0].Reply[0].Text, "narration must NOT become a respond_to_user call")
	assert.Equal(t, "none found", got.LLM[1].Reply[0].Text)
}

// TestFold_SystemNotesAreSkipped pins that runner metadata does not become a
// user turn. system_note is filtered from the replayed message history in
// production, and a capture that promoted one to a userTurn would make the
// bundle send a message nobody typed.
func TestFold_SystemNotesAreSkipped(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		{Index: 1, Role: "system_note", Content: []memory.ContentBlock{{Type: "text", Text: "runner restarted"}}},
		assistantCall(2, "tu_1", "respond_to_user", `{"text":"ok"}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"go"}, got.UserTurns)
	assert.Len(t, got.LLM, 1)
}

// TestFold_InboxBookkeepingIsSkipped pins that the raw "inbox" row and its
// paired "inbox_done" marker are bookkeeping, not conversation — the same
// reason system_note is skipped above — and so contribute neither a hole nor
// a duplicate message.
//
// drainInbox (pkg/agent/runner/loop_inbox.go) already places the content the
// model actually saw as an ordinary "user" turn, verbatim, before it marks
// the raw entry consumed; that placed turn is what the ordinary user-turn
// case below folds. The shape here mirrors what drainInbox actually writes:
// the raw "inbox" turn and its "inbox_done" marker share ONE index (the
// index channelsd assigned when it wrote the inbox entry), while the placed
// "user" turn carries a different, runner-assigned index.
func TestFold_InboxBookkeepingIsSkipped(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_0", "respond_to_user", `{"text":"on it"}`),
		{Index: 2, Role: "inbox", Content: []memory.ContentBlock{{Type: "text", Text: "also check the gadgets"}}},
		{Index: 2, Role: "inbox_done", Content: []memory.ContentBlock{{Type: "text", Text: "inbox entry consumed"}}},
		userText(3, "also check the gadgets"),
		assistantCall(4, "tu_1", "respond_to_user", `{"text":"checked"}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.UnmappedTurns, "queueing bookkeeping must not read as a hole in the replay")
	assert.Equal(t, []string{"go", "also check the gadgets"}, got.UserTurns,
		"the message the model saw comes from the PLACED user turn, once — not a second time from the raw inbox row")
	assert.Len(t, got.LLM, 2)
}

// TestFold_RefusedTurnsAreExcluded pins that a refusal does not become a step.
// The runner excludes refused turns from the reconstructed history so a retry
// regenerates cleanly; a capture that replayed one would drive the replay down
// a path the original run abandoned.
func TestFold_RefusedTurnsAreExcluded(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		{Index: 1, Role: "assistant", Refused: true, Content: []memory.ContentBlock{{Type: "text", Text: "I can't"}}},
		assistantCall(2, "tu_1", "respond_to_user", `{"text":"ok"}`),
	}}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM, 1)
	assert.Equal(t, "ok", got.LLM[0].Reply[0].Text)
}

// TestFold_MultiResultTurnDescribesTheBlockTheDriverReads pins the agreement
// between the fold and its consumer on a turn carrying SEVERAL tool results.
//
// The driver's lastToolResultBlock / lastToolResultName walk req.Messages
// backward but each message's blocks FORWARD, returning on the first hit — so
// despite their names they read the FIRST tool_result of the newest message.
// A turn holds more than one only after a PARALLEL tool call, which no authored
// bundle has ever produced and which a capture meets routinely. Folding the
// last block instead would fail three checkExpect assertions on a replay that
// took the identical path.
func TestFold_MultiResultTurnDescribesTheBlockTheDriverReads(t *testing.T) {
	const refused = "permission denied: alice does not have archive on widget:w1"
	got, err := steelthread.Fold(deniedFor("tu_3", refused, []memory.Turn{
		userText(0, "go"),
		assistantCalls(1,
			memory.ToolUseBlock{ID: "tu_1", Name: "acme_list_widgets", Input: []byte(`{"args":{}}`)},
			memory.ToolUseBlock{ID: "tu_2", Name: "acme_get_gadget", Input: []byte(`{"args":{}}`)},
			memory.ToolUseBlock{ID: "tu_3", Name: "acme_archive_widget", Input: []byte(`{"args":{}}`)},
		),
		multiToolResult(2,
			resultBlock("tu_1", `{"results":[]}`, false),
			resultBlock("tu_2", `{"gadget":"g1"}`, false),
			resultBlock("tu_3", refused, true),
		),
		assistantCall(3, "tu_4", "respond_to_user", `{"text":"done"}`),
	}), foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM, 2)
	e := got.LLM[1].Expect
	assert.Equal(t, "acme_list_widgets", e.LastToolResult,
		"Expect must name the FIRST result block — the one the driver reads")
	assert.Equal(t, `{"results":[]}`, e.LastToolResultContains)
	require.NotNil(t, e.LastToolResultIsError)
	assert.False(t, *e.LastToolResultIsError,
		"the first block succeeded; pinning the refused sibling would claim the step answered an error")

	// Describing one block does not stop the others reaching the bundle.
	assert.Contains(t, got.ToolOutputs, "list_widgets")
	assert.Contains(t, got.ToolOutputs, "get_gadget",
		"a SERVED result still contributes its canned output wherever it sits in the turn, "+
			"only the Expect is singular")
	assert.NotContains(t, got.ToolOutputs, "archive_widget",
		"the refused sibling produced no output to can: the gate stopped it before dispatch")
	assert.Empty(t, got.ToolErrors,
		"and it is not an upstream failure either — the replay's own gate refuses it again")
	assert.Empty(t, got.ErrorResults)
}

// TestFold_UnplaceableTurnsAreReported pins that nothing is dropped in silence.
// A hole in the replay that nothing reports is the exact failure class this
// feature exists to remove, so a turn the walk has no rule for lands in
// UnmappedTurns instead of vanishing.
func TestFold_UnplaceableTurnsAreReported(t *testing.T) {
	cases := []struct {
		name  string
		turns []memory.Turn
		want  []int
	}{
		{
			name: "a user turn carrying neither text nor tool results is reported",
			turns: []memory.Turn{
				userText(0, "go"),
				{Index: 1, Role: "user", Content: []memory.ContentBlock{{Type: "attachment",
					Attachment: &memory.AttachmentBlock{Filename: "notes.txt", MIME: "text/plain", Ref: "art-1"}}}},
			},
			want: []int{1},
		},
		{
			// Several unplaceable blocks are ONE hole, not one per block: the
			// unit a reader goes and looks at is the turn.
			name: "a turn whose results all answer unseen calls is reported once, not per block",
			turns: []memory.Turn{
				userText(0, "go"),
				multiToolResult(1,
					resultBlock("tu_unknown_a", `{"a":1}`, false),
					resultBlock("tu_unknown_b", `{"b":2}`, false),
				),
			},
			want: []int{1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: tc.turns}, foldOpts())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.UnmappedTurns)
		})
	}
}

// TestFold_UnnameableFirstResultSetsNoExpect pins that a hole stays a hole.
//
// checkExpect reaches for lastToolResultBlock whenever LastToolResultContains
// or LastToolResultIsError is set, WITHOUT requiring LastToolResult — so an
// Expect filled from a block the walk could not name would assert the NEXT step
// against data that never entered collected/ToolOutputs and has no route back
// into a replay. That is the Critical's false-regression mode in miniature, and
// it is invisible unless an assistant turn follows the unplaceable one.
func TestFold_UnnameableFirstResultSetsNoExpect(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		multiToolResult(1,
			resultBlock("tu_never_seen", `{"stray":true}`, true),
			resultBlock("tu_1", `{"results":[]}`, false),
		),
		assistantCall(2, "tu_2", "respond_to_user", `{"text":"ok"}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []int{1}, got.UnmappedTurns, "the turn is still reported as a hole")
	require.Len(t, got.LLM, 1)

	e := got.LLM[0].Expect
	assert.Empty(t, e.LastToolResult)
	assert.Empty(t, e.LastToolResultContains,
		"an unknowable block must contribute no assertion, not a nameless one")
	assert.Nil(t, e.LastToolResultIsError,
		"nil is the only honest value here: nothing replay produces can be compared against it")
	assert.Equal(t, "go", e.UserTextContains,
		"the pending Expect is left untouched, not cleared")
}

// TestFold_ServerSideKeyingMergesTwoServers pins the key toolOutputs is written
// under. Two MCP servers may export the same tool name; the replay registers
// ONE handler per server-side name, so both calls must merge into a single
// call-ordered sequence rather than one silently overwriting the other.
func TestFold_ServerSideKeyingMergesTwoServers(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", `{"results":["from acme"]}`, false),
		assistantCall(3, "tu_2", "other_list_widgets", `{"args":{}}`),
		toolResult(4, "tu_2", `{"results":["from other"]}`, false),
	}}, steelthread.FoldOptions{MCPPrefixes: []string{"acme", "other"}})
	require.NoError(t, err)

	assert.NotContains(t, got.ToolOutputs, "list_widgets")
	assert.NotContains(t, got.ToolOutputSequence, "acme_list_widgets",
		"the LLM-facing name is never a key; the replay dispatches server-side")
	require.Len(t, got.ToolOutputSequence["list_widgets"], 2,
		"both servers' results merge under the one name the replay registers")
	assert.JSONEq(t, `{"results":["from acme"]}`, string(got.ToolOutputSequence["list_widgets"][0]))
	assert.JSONEq(t, `{"results":["from other"]}`, string(got.ToolOutputSequence["list_widgets"][1]))
}

// TestFold_IsErrorPointersAreIndependent pins that each step owns its own bool.
// The turns are cloned SHALLOWLY, so pointing Expect at the record's field would
// alias the caller's memory.Turn — a later write through one step's pointer
// would silently rewrite a second step and the input records with it.
func TestFold_IsErrorPointersAreIndependent(t *testing.T) {
	turns := []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "acme_get_gadget", `{"args":{}}`),
		toolResult(4, "tu_2", `{"message":"boom"}`, true),
		assistantCall(5, "tu_3", "respond_to_user", `{"text":"done"}`),
	}
	got, err := steelthread.Fold(steelthread.Records{Turns: turns}, foldOpts())
	require.NoError(t, err)
	require.Len(t, got.LLM, 3)

	first, second := got.LLM[1].Expect.LastToolResultIsError, got.LLM[2].Expect.LastToolResultIsError
	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.NotSame(t, first, second, "each step must own its own bool")
	assert.False(t, *first)
	assert.True(t, *second)

	// The decisive property: the fold's output does not alias its input.
	*first = true
	assert.False(t, turns[2].Content[0].ToolResult.IsError,
		"writing through a folded pointer must not reach back into the source record")
	assert.True(t, *second, "nor into another step")
}

// TestFold_DerivedExpectIsWhatTheReplayWillServe closes the round trip between
// the two halves of a bundle: the toolOutputs entry the replay serves, and the
// Expect the capture derived from the same result.
//
// A real MCP server answers with keys in ITS order and ids past 2^53, and the
// replay's canned handler passes the recorded bytes through — so the Expect has
// to be the served form, not the map-shaped rewrite the driver used to produce.
// Three of four failures on the first live capture were exactly this.
func TestFold_DerivedExpectIsWhatTheReplayWillServe(t *testing.T) {
	// Key order is the server's, not alphabetical; the count exceeds float64's
	// exact range; and the label carries HTML characters the encoder rewrites.
	const recorded = `{"resource":"widgets","namespaced":true,"count":9007199254740993,"label":"a<b&c"}`

	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
		toolResult(2, "tu_1", recorded, false),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"done"}`),
	}}, foldOpts())
	require.NoError(t, err)

	stored, ok := got.ToolOutputs["list_widgets"]
	require.True(t, ok, "the result is canned for the replay to serve")

	// What the replay's own handler will hand the model, byte for byte.
	willServe, err := json.Marshal(bt.CannedHandler(stored)(map[string]any{}))
	require.NoError(t, err)

	assert.Equal(t, string(willServe), got.LLM[1].Expect.LastToolResultContains,
		"the derived Expect must be the text the replay serves, or it asserts on bytes nothing produces")
	assert.Contains(t, string(willServe), `"count":9007199254740993`,
		"and the served bytes keep the recorded integer exactly")
	assert.Contains(t, string(willServe), `"resource":"widgets","namespaced":true`,
		"in the recorded key order")
}

// TestFold_ArgsAreEmittedVerbatimAtEveryDepth pins the half of the inversion
// that is easy to lose: nothing rewrites an argument any more.
//
// MCP args are nested (`{"operation_id":…, "_reason":…, "args":{…}}`), so the id
// a tool operates on routinely sits a level down, and lists of handles are as
// ordinary as single ones. Every one of those shapes now round-trips unchanged,
// including an id EMBEDDED in a longer string — which the sentinel scheme could
// not carry at all, because it matched whole strings and left the embedded copy
// as a dead literal.
func TestFold_ArgsAreEmittedVerbatimAtEveryDepth(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{
			name: "a top-level id",
			args: `{"operation_id":"op-7f3a1c0b45de9982"}`,
		},
		{
			name: "an id nested inside the MCP args object",
			args: `{"operation_id":"op-7f3a1c0b45de9982","_reason":"asked","args":{"artifact_id":"artifact-91b2000000000000"}}`,
		},
		{
			name: "ids inside an array",
			args: `{"args":{"attached":["artifact-91b2000000000000","artifact-91b2000000000000"]}}`,
		},
		{
			// Was a known gap under the sentinel scheme and is not one now: the
			// replay mints op-7f3a1c0b45de9982 again, so the sentence still
			// names the operation the run is actually working under.
			name: "an id embedded in a longer string",
			args: `{"args":{"query":"op-7f3a1c0b45de9982 is what I looked at","limit":5}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
				userText(0, "go"),
				assistantCall(1, "tu_1", "new_operation", `{"kind":"read"}`),
				toolResult(2, "tu_1", `{"operation_id":"op-7f3a1c0b45de9982","artifact_id":"artifact-91b2000000000000"}`, false),
				assistantCall(3, "tu_2", "acme_list_widgets", tc.args),
			}}, foldOpts())
			require.NoError(t, err)

			require.Len(t, got.LLM, 2)
			require.NotNil(t, got.LLM[1].Reply[0].ToolUse)
			assert.JSONEq(t, tc.args, string(got.LLM[1].Reply[0].ToolUse.Args))
		})
	}
}

// TestFold_MintedIDsAreRecordedAtEveryDepth is the mirror: the RECORDING walk
// has to reach as deep as any result puts an id, because a mint it does not see
// is one the replay's minter never hands back — and worse, one whose absence
// shifts every later id in that family by a position.
func TestFold_MintedIDsAreRecordedAtEveryDepth(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "a top-level field, where new_operation puts its id",
			payload: `{"operation_id":"op-1111111111111111"}`,
			want:    []string{"op-1111111111111111"},
		},
		{
			name:    "nested one object down",
			payload: `{"plan":{"operation_id":"op-1111111111111111"}}`,
			want:    []string{"op-1111111111111111"},
		},
		{
			name: "inside an array of objects, where update_plan puts them",
			payload: `{"items":[{"operation_id":"op-1111111111111111"},` +
				`{"operation_id":"op-2222222222222222"}]}`,
			want: []string{"op-1111111111111111", "op-2222222222222222"},
		},
		{
			name:    "inside a bare array of strings",
			payload: `{"opened":["op-1111111111111111","op-2222222222222222"]}`,
			want:    []string{"op-1111111111111111", "op-2222222222222222"},
		},
		{
			name:    "an id embedded in a sentence is NOT a mint",
			payload: `{"message":"op-1111111111111111 is not registered"}`,
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
				userText(0, "go"),
				assistantCall(1, "tu_1", "acme_list_widgets", `{"args":{}}`),
				toolResult(2, "tu_1", tc.payload, false),
			}}, foldOpts())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.MintedIDs[bt.FamilyOperation])
		})
	}
}

// deniedFor builds the Records a run leaves behind when the authz gate refused
// one tool call: the decision log entry, indexed by the tool_use id it
// answered, carrying the SAME message the gate handed back as the tool result.
func deniedFor(useID, message string, turns []memory.Turn) steelthread.Records {
	return steelthread.Records{
		Turns: turns,
		DecisionsByToolCall: map[string][]authzdecision.Decision{
			useID: {{
				Outcome: authzdecision.OutcomeDenied, Subject: "alice",
				ResourceType: "widget", ResourceID: "w1", Permission: "read",
				Message: message,
			}},
		},
	}
}

const deniedBody = "permission denied: alice does not have read on widget:w1"

// gateRefusalTurns is the transcript of a single refused MCP call.
func gateRefusalTurns(body string) []memory.Turn {
	return []memory.Turn{
		userText(0, "show me the widget"),
		assistantCall(1, "tu_1", "acme_read_widget", `{"operation_id":"op-1","_reason":"asked","args":{"id":"w1"}}`),
		toolResult(2, "tu_1", body, true),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"I could not read that"}`),
	}
}

// TestFold_AGateGeneratedRefusalNeedsNothingCanned is the reframing this
// feature turned on.
//
// A denied tool never reaches the sandbox or MCP server
// (pkg/agent/runner/loop_dispatch.go), so the error the model saw was written
// by the gate, not returned by the upstream. The replay boots the same fixture
// with the same derived seed and the same gate configuration, so its own gate
// produces that refusal again — there is nothing to can, and canning something
// would make the stub answer a call the gate already stopped.
//
// The fold therefore emits NOTHING for such a call: no toolOutputs entry, no
// toolErrors entry, and no finding. Expect.LastToolResultIsError still pins
// that the call errored, which is the claim that matters.
func TestFold_AGateGeneratedRefusalNeedsNothingCanned(t *testing.T) {
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, gateRefusalTurns(deniedBody)), foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.ToolOutputs, "the tool never ran, so there is no output to reproduce")
	assert.Empty(t, got.ToolOutputSequence)
	assert.Empty(t, got.ToolErrors, "the upstream never answered; registering an error would be a fabrication")
	assert.Empty(t, got.ErrorResults, "a refusal the replay regenerates is not a gap in the format")

	require.Len(t, got.LLM, 2)
	require.NotNil(t, got.LLM[1].Expect.LastToolResultIsError)
	assert.True(t, *got.LLM[1].Expect.LastToolResultIsError,
		"the step still asserts the call came back an error")
}

// TestFold_AHumanRefusalIsAlsoGateGenerated covers the second provable
// pre-dispatch refusal: an approval a person answered "denied".
//
// The executor turns that into a PreToolCall Deny, so the call never reaches
// the server, exactly as an authz denial does — and unlike an authz denial the
// message it hands back is the approval host's own anti-confabulation framing,
// which no record holds. The approval outcome itself is the proof, so no
// content comparison is needed or possible.
func TestFold_AHumanRefusalIsAlsoGateGenerated(t *testing.T) {
	body := "SYSTEM_DECISION_DENIED: bob declined this call"
	recs := steelthread.Records{
		Turns: gateRefusalTurns(body),
		Approvals: map[string]approval.Pair{
			"tu_1": {Outcome: &approval.Outcome{Decision: "denied", Approver: "bob"}},
		},
	}
	got, err := steelthread.Fold(recs, foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.ToolOutputs)
	assert.Empty(t, got.ToolErrors, "a refused approval stopped the dispatch; the upstream never spoke")
	assert.Empty(t, got.ErrorResults)
}

// TestFold_AnErrorTheRecordsCannotExplainIsTreatedAsUpstream is the
// fail-closed half of the classification, and the direction matters.
//
// Reading an unexplained error as gate-generated would emit a bundle that
// serves the call to SUCCESS on replay — silently reversing what the capture is
// evidence of. Reading it as upstream at worst cans an error the gate would
// have produced anyway, which the gate then pre-empts. So anything the records
// cannot prove is upstream.
func TestFold_AnErrorTheRecordsCannotExplainIsTreatedAsUpstream(t *testing.T) {
	body := "mcp: mcp call \"read_widget\": upstream returned 503"
	got, err := steelthread.Fold(steelthread.Records{Turns: gateRefusalTurns(body)}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, map[string]bt.ToolError{"read_widget": {Message: body}}, got.ToolErrors,
		"the observed text becomes the JSON-RPC message, so the replayed result CONTAINS it verbatim")
	assert.Equal(t, []steelthread.ErrorResult{{
		TurnIndex: 2, UseID: "tu_1", Tool: "acme_read_widget", Server: "read_widget", Body: body,
	}}, got.ErrorResults, "the per-call record survives for the uniformity check and the report")
	assert.Empty(t, got.ToolOutputs,
		"an error body is not JSON, and storing it here is what once made the bundle unmarshallable")
	assert.Empty(t, got.ToolOutputSequence, "the sequence map is the same trap by another route")

	require.Len(t, got.LLM, 2)
	require.NotNil(t, got.LLM[1].Expect.LastToolResultIsError)
	assert.True(t, *got.LLM[1].Expect.LastToolResultIsError,
		"the step still asserts the call came back an error, whoever wrote it")
}

// TestFold_ADenialWhoseMessageIsNotWhatTheModelSawIsNotAGateRefusal is the
// anchor that makes the authz signal a proof rather than an inference.
//
// A denied check does not always stop the call: under a permissive tool-call
// mode a readonly/readwrite denial is logged and the dispatch PROCEEDS, so the
// error the model saw could be the upstream's. What separates the two is
// whether the bytes the model was handed are the bytes the gate recorded
// refusing with. When they differ, nothing here can prove the tool did not run.
//
// The consequence is the three-way outcome this fold now has, and the case is
// worth keeping for that alone: a PROVEN gate refusal is dropped whole (no
// record, no finding, nothing canned), an UNPROVEN one is recorded so the
// reader is warned but still cans nothing, and only a clean upstream failure
// reaches bt.Bundle.ToolErrors.
func TestFold_ADenialWhoseMessageIsNotWhatTheModelSawIsNotAGateRefusal(t *testing.T) {
	body := "mcp: mcp call \"read_widget\": upstream returned 503"
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, gateRefusalTurns(body)), foldOpts())
	require.NoError(t, err)

	require.Len(t, got.ErrorResults, 1,
		"a denial recorded against this call did not author THIS error, so the result is not dropped as one")
	assert.True(t, got.ErrorResults[0].DeniedInLog, "but the log DID deny the call, and the reader is told")
	assert.Empty(t, got.ToolErrors,
		"and nothing is canned: the capture cannot say whose error this was, so it says nothing")
}

// TestFold_ACallAllowedAfterItsDenialIsNotAGateRefusal pins that the LAST
// decision for a call is the one that decides.
//
// Denied-then-approved re-checks the same tool_use id and records both
// outcomes. The allow is what let the tool run, so an error carried by that
// call came from the upstream — reading the earlier denial as the author would
// drop a real upstream failure from the bundle.
func TestFold_ACallAllowedAfterItsDenialIsNotAGateRefusal(t *testing.T) {
	recs := steelthread.Records{
		Turns: gateRefusalTurns(deniedBody),
		DecisionsByToolCall: map[string][]authzdecision.Decision{
			"tu_1": {
				{Outcome: authzdecision.OutcomeDenied, Message: deniedBody},
				{Outcome: authzdecision.OutcomeAllowed},
			},
		},
	}
	got, err := steelthread.Fold(recs, foldOpts())
	require.NoError(t, err)
	assert.Equal(t, map[string]bt.ToolError{"read_widget": {Message: deniedBody}}, got.ToolErrors,
		"the allow is what dispatched the call, so its error is the upstream's")
}

// TestFold_ADenialWithNoMessageProvesNothing covers the writer's own gap:
// recordAuthzDecision copies res.Message, and a check that denied without one
// leaves it empty. An empty message matches an empty payload, which would make
// every unexplained empty error read as gate-generated.
func TestFold_ADenialWithNoMessageProvesNothing(t *testing.T) {
	got, err := steelthread.Fold(deniedFor("tu_1", "", gateRefusalTurns("")), foldOpts())
	require.NoError(t, err)
	require.Len(t, got.ErrorResults, 1,
		"a denial that recorded no reason cannot be shown to have authored anything, so this is not dropped")
	assert.True(t, got.ErrorResults[0].DeniedInLog)
	assert.Empty(t, got.ToolErrors, "nor is it canned, for the same reason: the origin is unproven")
}

// TestFold_NonUniformToolErrorsAreReported is what stays unrepresentable after
// toolErrors exists, and the only reason a hard finding survives here.
//
// MCPStub.OnToolError is registered per tool NAME and wins over OnTool for
// every call, so a tool that errored once and succeeded once — or errored twice
// with different messages — has no spelling. Both shapes are one rule, because
// they are one mechanism.
func TestFold_NonUniformToolErrorsAreReported(t *testing.T) {
	cases := []struct {
		name  string
		turns []memory.Turn
	}{
		{
			name: "an upstream error on one call and a success on another",
			turns: []memory.Turn{
				userText(0, "read both"),
				assistantCall(1, "tu_1", "acme_read_widget", `{"args":{"id":"w1"}}`),
				toolResult(2, "tu_1", `{"id":"w1"}`, false),
				assistantCall(3, "tu_2", "acme_read_widget", `{"args":{"id":"w2"}}`),
				toolResult(4, "tu_2", "mcp: upstream returned 503", true),
			},
		},
		{
			name: "two upstream errors that do not say the same thing",
			turns: []memory.Turn{
				userText(0, "read both"),
				assistantCall(1, "tu_1", "acme_read_widget", `{"args":{"id":"w1"}}`),
				toolResult(2, "tu_1", "mcp: upstream returned 503", true),
				assistantCall(3, "tu_2", "acme_read_widget", `{"args":{"id":"w2"}}`),
				toolResult(4, "tu_2", "mcp: upstream returned 429", true),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: tc.turns}, foldOpts())
			require.NoError(t, err)
			assert.Equal(t, []string{"read_widget"}, got.NonUniformErrorTools)
		})
	}
}

// TestFold_TwoIdenticalUpstreamErrorsAreUniform is the negative control for the
// case above: a tool that failed the same way every time IS expressible, and
// refusing it would put the check back to disqualifying sessions it can serve.
func TestFold_TwoIdenticalUpstreamErrorsAreUniform(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "read both"),
		assistantCall(1, "tu_1", "acme_read_widget", `{"args":{"id":"w1"}}`),
		toolResult(2, "tu_1", "mcp: upstream returned 503", true),
		assistantCall(3, "tu_2", "acme_read_widget", `{"args":{"id":"w2"}}`),
		toolResult(4, "tu_2", "mcp: upstream returned 503", true),
	}}, foldOpts())
	require.NoError(t, err)
	assert.Empty(t, got.NonUniformErrorTools)
	assert.Equal(t, map[string]bt.ToolError{"read_widget": {Message: "mcp: upstream returned 503"}}, got.ToolErrors)
}

// TestFold_AGateRefusalDoesNotConflictWithTheSameToolSucceeding is the shape
// the flagship capture has, and the one the old hard finding refused outright.
//
// Denied-then-approved is two calls of one tool: the first is stopped by the
// gate and needs nothing, the second reaches the server and needs an ordinary
// toolOutputs entry. Nothing has to interleave, so nothing is unrepresentable.
func TestFold_AGateRefusalDoesNotConflictWithTheSameToolSucceeding(t *testing.T) {
	turns := []memory.Turn{
		userText(0, "read it"),
		assistantCall(1, "tu_1", "acme_read_widget", `{"args":{"id":"w1"}}`),
		toolResult(2, "tu_1", deniedBody, true),
		assistantCall(3, "tu_2", "acme_read_widget", `{"args":{"id":"w1"}}`),
		toolResult(4, "tu_2", `{"id":"w1"}`, false),
	}
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, turns), foldOpts())
	require.NoError(t, err)

	assert.JSONEq(t, `{"id":"w1"}`, string(got.ToolOutputs["read_widget"]),
		"the call that DID reach the server contributes its payload as any other would")
	assert.Empty(t, got.ToolOutputSequence, "one collected value is a constant; the refusal must not pad it")
	assert.Empty(t, got.ToolErrors)
	assert.Empty(t, got.NonUniformErrorTools, "the refusal is not an upstream error, so there is nothing to conflict")
}

// TestFold_ARespondToUserCarryingMoreThanTextKeepsEveryArgument closes the
// second blocker, and it needed no new format.
//
// bt.ReplyPart.Text is shorthand for a text-only respond_to_user, so a reply
// that also delivered an artifact folded to its words alone and a bundle whose
// point was DELIVERY stopped proving delivery. bt.ReplyPart.ToolUse already
// round-trips any call with its arguments verbatim, so the fold picks the shape
// that fits: the shorthand when the args are exactly a non-empty text, and the
// full call otherwise.
func TestFold_ARespondToUserCarryingMoreThanTextKeepsEveryArgument(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "chart the widgets"),
		assistantCall(1, "tu_1", "respond_to_user",
			`{"text":"here is the chart","attached":["artifact-1"]}`),
	}}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM, 1)
	require.Len(t, got.LLM[0].Reply, 1)
	part := got.LLM[0].Reply[0]
	assert.Empty(t, part.Text, "the shorthand cannot carry the attachment, so it is not the shape used")
	require.NotNil(t, part.ToolUse)
	assert.Equal(t, "respond_to_user", part.ToolUse.Name)
	assert.JSONEq(t, `{"text":"here is the chart","attached":["artifact-1"]}`, string(part.ToolUse.Args),
		"every argument survives, which is what makes the delivery replayable at all")
}

// TestFold_ATextOnlyReplyStaysTheShorthand is the negative control: the common
// case must keep the readable spelling AND the assertion path that reads it.
func TestFold_ATextOnlyReplyStaysTheShorthand(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "hello"),
		assistantCall(1, "tu_1", "respond_to_user", `{"text":"hello back"}`),
	}}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM, 1)
	require.Len(t, got.LLM[0].Reply, 1)
	assert.Equal(t, "hello back", got.LLM[0].Reply[0].Text)
	assert.Nil(t, got.LLM[0].Reply[0].ToolUse)
}

// TestFold_ARespondToUserWithNoTextIsKeptAsTheCallItWas covers the two shapes
// the shorthand silently DROPS: the driver's replyParts switch matches on
// `p.Text != ""`, so a ReplyPart carrying an empty Text contributes no reply
// block at all and the replay diverges from the run for no stated reason.
//
// Emitting the call verbatim instead is both faithful and self-correcting: the
// replay makes the same malformed call and gets the same argument error back.
func TestFold_ARespondToUserWithNoTextIsKeptAsTheCallItWas(t *testing.T) {
	cases := []struct{ name, args string }{
		{name: "no arguments at all", args: ``},
		{name: "an empty object", args: `{}`},
		{name: "an explicitly empty text", args: `{"text":""}`},
		{name: "a text that is not a string", args: `{"text":7}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
				userText(0, "hi"),
				assistantCall(1, "tu_1", "respond_to_user", tc.args),
			}}, foldOpts())
			require.NoError(t, err)

			require.Len(t, got.LLM, 1)
			require.Len(t, got.LLM[0].Reply, 1)
			require.NotNil(t, got.LLM[0].Reply[0].ToolUse,
				"a reply the shorthand would drop is emitted as the call it actually was")
			assert.Equal(t, "respond_to_user", got.LLM[0].Reply[0].ToolUse.Name)
		})
	}
}

// TestFold_ARespondToUserWhoseArgsDoNotDecodeIsRefused is where "I could not
// read the args" is answered, and it has to be an outright refusal.
//
// The self-check used to carry a fail-closed branch for this, on the ground
// that unreadable bytes are not evidence that no attachment was in them. It is
// the fold's answer now, and a stronger one: bytes the capture cannot decode
// cannot be re-encoded into a bundle either, so the transcript is refused with
// the tool_use id named rather than emitted with something guessed in its
// place.
func TestFold_ARespondToUserWhoseArgsDoNotDecodeIsRefused(t *testing.T) {
	_, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "hi"),
		assistantCall(1, "tu_1", "respond_to_user", `{not json`),
	}}, foldOpts())
	require.Error(t, err, "a reply the capture cannot read must not be emitted with a guess in its place")
	assert.Contains(t, err.Error(), "tu_1", "the refusal must name the offending tool_use id")
	assert.Contains(t, err.Error(), "respond_to_user args")
}

// TestFold_AnUpstreamErrorOnADeniedCallIsMarkedUnproven is what the
// capturability survey's own replay probe turned up, and it is the honest cost
// of failing closed.
//
// Not every pre-dispatch refusal is provable. The approval flow denies with
// whatever BuildApprovalAsk's error said — "no one has standing to approve …" —
// and records nothing at all when the ask could not even be built, so the only
// per-call record is an authz denial whose message says something else. The
// call is classified upstream, which replays correctly (the replay's gate
// refuses it before the stub is reached), but the emitted toolErrors entry then
// says the SERVER returned a message the server never sent.
//
// So the fold marks it: the log denied this call, and the bytes the model was
// handed are not the denial the log recorded. The self-check turns that into a
// warning rather than a refusal — the bundle is still worth emitting, and a
// reader must not take that entry as evidence about the upstream.
func TestFold_AnUpstreamErrorOnADeniedCallIsMarkedUnproven(t *testing.T) {
	const body = `approval flow: no one has standing to approve tool "acme_read_widget"`
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, gateRefusalTurns(body)), foldOpts())
	require.NoError(t, err)

	require.Len(t, got.ErrorResults, 1)
	assert.True(t, got.ErrorResults[0].DeniedInLog,
		"the log denied this call, so the capture cannot claim the upstream authored the error")

	// The negative control, on the same field: an error with no denial recorded
	// against its call is an ordinary upstream failure and must not be marked.
	plain, err := steelthread.Fold(steelthread.Records{Turns: gateRefusalTurns("mcp: upstream 503")}, foldOpts())
	require.NoError(t, err)
	require.Len(t, plain.ErrorResults, 1)
	assert.False(t, plain.ErrorResults[0].DeniedInLog,
		"nothing denied this call; marking it would make the warning noise on every real upstream failure")
}

// TestFold_AnUnprovenErrorOriginCansNothing is the safety property, and it is a
// stronger reason than the one it replaces.
//
// An unproven-origin error was emitted as a toolErrors entry on the argument
// that the replay's own gate refuses the call before the stub is reached, so
// the entry is never served. That holds only while the gate WORKS. Trace a gate
// regression: the call is no longer refused, it reaches the stub, the stub
// serves the canned entry — whose message is the captured GATE REFUSAL text,
// byte for byte — and Expect.LastToolResultContains, which was derived from
// that same text, MATCHES. The bundle passes with the permission boundary
// broken, which is the exact defect class this package exists to eliminate.
//
// With nothing canned, both directions are honest: a working gate produces the
// real refusal and the Expect matches, while a broken one reaches a tool the
// stub has no error registered for and the served value cannot contain the
// refusal text, so the step fails and names itself.
func TestFold_AnUnprovenErrorOriginCansNothing(t *testing.T) {
	const body = `approval flow: no one has standing to approve tool "acme_read_widget"`
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, gateRefusalTurns(body)), foldOpts())
	require.NoError(t, err)

	assert.NotContains(t, got.ToolErrors, "read_widget",
		"canning the gate's own refusal text would let a regressed gate satisfy the Expect derived from it")
	assert.Empty(t, got.ToolErrors, "and there is nothing else to can either")
	assert.Empty(t, got.ToolOutputs, "least of all a success where the run was refused")

	// Still RECORDED, so the reader is told an error result went unrepresented.
	require.Len(t, got.ErrorResults, 1)
	assert.True(t, got.ErrorResults[0].DeniedInLog)
}

// TestFold_AnUnprovenErrorOriginCannotConflictWithASuccess is the shape the
// residual risk was about, and closing it is a consequence of canning nothing
// rather than a second rule.
//
// Denied-then-approved calls one tool twice: refused, then served. When the
// refusal is approval-flow-shaped its origin cannot be proven, so it used to
// contribute a toolErrors entry — which collided with the success and refused
// the whole capture as tool-error-not-uniform, on a session that is the
// feature's flagship. With nothing emitted for it there is nothing to collide
// with.
func TestFold_AnUnprovenErrorOriginCannotConflictWithASuccess(t *testing.T) {
	const body = `approval flow: no one has standing to approve tool "acme_read_widget"`
	turns := []memory.Turn{
		userText(0, "read it"),
		assistantCall(1, "tu_1", "acme_read_widget", `{"args":{"id":"w1"}}`),
		toolResult(2, "tu_1", body, true),
		assistantCall(3, "tu_2", "acme_read_widget", `{"args":{"id":"w1"}}`),
		toolResult(4, "tu_2", `{"id":"w1"}`, false),
	}
	got, err := steelthread.Fold(deniedFor("tu_1", deniedBody, turns), foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.NonUniformErrorTools,
		"an error nothing cans cannot disagree with a success about a registration that was never made")
	assert.JSONEq(t, `{"id":"w1"}`, string(got.ToolOutputs["read_widget"]),
		"and the call that DID reach the server still contributes its payload")
	assert.Empty(t, got.ToolErrors)
}
