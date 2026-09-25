//go:build e2e

// leads_console_view_test.go is spec E2E 15: the agent calls update_view ->
// the slot updates live, carrying the agent-composed marker. Drives the
// AGENT — a real turn, the real update_view/read_view meta tools, gated by
// the real spec.capabilities grant, against the real Loop.AppTools the
// runner materialized from the real CRM — then reads the result through the
// real browser socket. This is what Plan 6's Task 9 (liveHandler exercised
// directly, with a fragment written straight through uiview.Runtime.Write)
// could not catch: a capability, tool, or grant wired in internal/cmd/runner but not
// in the harness factory (or vice versa).
//
// Every turn below ends in respond_to_user, not agent_work_complete (unlike
// this package's read/action test files, which never send a second user
// message). agent_work_complete marks the SESSION complete — empirically,
// sending it here made the runner cold-start-replay the WHOLE stored
// conversation from scratch on every subsequent SendUserMessage, forever
// (each replay independently re-reaching "complete" and re-restarting).
// respond_to_user + the standard OnToolResult(...).Reply(EndTurn()).Repeating()
// idiom (the SAME one centerdot's multi-turn scenarios use successfully) has
// no such effect: a turn ends, the session goes Idle, and the NEXT
// SendUserMessage cold-starts cleanly exactly once. Synchronized via
// h.ExpectAgentReply, not WaitForSessionIdle — the latter polls "is
// currently Idle", which a session already-Idle from a PRIOR turn can
// satisfy on its very first poll, before the runner has even picked up the
// new message.
package agentui_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// narrowedGrantedToolsOverride re-applies the fixture's pipeline-desk
// AgentClass with "deals_list_leads" dropped from
// spec.agentUI.grantedTools — mutation/step 6's "shrink the grant so the
// merged declaration no longer validates" trigger. Applied mid-test via
// h.ApplyManifest (updates the existing object in place — see
// applyLeadsConsoleFixture's identical use of ApplyManifest), not
// Options.ExtraManifests (which only applies once at harness startup and
// this narrowing must happen AFTER the successful write below).
//
// Dropping deals_list_leads — used by the PIPELINE slot's own Tier-0 rows
// binding and the ADVANCE slot's own Tier-0 options binding, neither of
// which this file ever writes an agent fragment to — is deliberate, not
// incidental: uicomponents.Validate walks EVERY slot in a ResolveView
// candidate, not just the one a fragment targets (viewmodel.go's
// ResolveView builds each candidate as "accepted so far + this ONE slot's
// Default swapped in" and then validates the WHOLE candidate declaration).
// So narrowing a tool an UNRELATED Tier-0 slot's own default binds to is
// what makes the summary fragment's re-validation fail on read, even
// though the fragment itself carries no binding at all — see
// pkg/web/uicomponents/viewmodel.go's ResolveView doc comment.
const narrowedGrantedToolsOverride = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: pipeline-desk
  namespace: default
spec:
  displayName: PipelineDeskBot
  description: "E2E fixture agent for the agent-defined-UI read-half scenarios. Do not deploy to a real cluster."
  model:
    provider: test
    name: scripted
    apiKey:
      name: pipeline-desk-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      DO NOT deploy to a real cluster — your model provider is ` + "`test`" + `.
  mcpServers:
    - name: deals
      ref: dealbook-crm
  agentUI:
    ref: pipeline-desk-ui
    grantedTools: [deals_stage_breakdown, deals_advance_lead_stage]
  capabilities:
    update_view: {}
    read_view: {}
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
`

// readViewResultMirror mirrors pkg/agent/tool/meta's unexported
// readViewResult (read_view.go) — its own doc comment pins this shape to
// EXACTLY these five fields, never a sixth carrying resolved binding data;
// this test's "no bound data" assertion is the e2e half of that same pin.
// Declaration/ParamState/Rejection are all EXPORTED pkg/web/uicomponents types
// used directly (unlike the browser-facing wire mirror elsewhere in this
// package, read_view's result marshals uicomponents.Declaration itself, not
// the wire-only declarationWire shape webd's routes serve).
type readViewResultMirror struct {
	Declaration   uicomponents.Declaration  `json:"declaration"`
	AgentComposed []string                  `json:"agentComposed"`
	Parameters    []uicomponents.ParamState `json:"parameters"`
	Rejected      []uicomponents.Rejection  `json:"rejected"`
	// JSX is the declaration printed as JSX from the declaration alone — not
	// a sixth field carrying resolved data, only a second rendering of the
	// same four fields above.
	JSX string `json:"jsx"`
}

// hookIsComposed reports whether hook is named in decl's AgentComposed list —
// the wire's ONLY signal that a hook's current content is an agent's
// fragment rather than its Tier-0 default (declarationWire's own doc comment:
// the tree cannot say this about itself).
func hookIsComposed(decl wireDeclarationMirror, hook string) bool {
	return slices.Contains(decl.AgentComposed, hook)
}

// nodeAtHook returns hook's current content — the single fill or Tier-0
// default child a hook node carries, or nil for a cleared hook with no
// default — found by walking decl's page tree through uicomponents.Hooks'
// own recorded Path. The served wire carries no per-slot "default" field any
// more (that was the legacy shape); a hook's content is read off the tree
// itself, the same way the runtime does.
func nodeAtHook(t *testing.T, decl wireDeclarationMirror, hook string) *uicomponents.Node {
	t.Helper()
	full := uicomponents.Declaration{Actions: decl.Actions, View: decl.View}
	for _, h := range uicomponents.Hooks(full) {
		if h.Name != hook {
			continue
		}
		node := nodeAtPath(decl.View, h.Path)
		if node == nil || len(node.Children) == 0 {
			return nil
		}
		return &node.Children[0]
	}
	return nil
}

// nodeAtPath descends root by a Hook's own index path (uicomponents.Hooks'
// Path: index-from-root to the hook node itself, never a region-relative
// one), returning the node found there or nil if the path runs off the tree.
func nodeAtPath(root *uicomponents.Node, path []int) *uicomponents.Node {
	n := root
	for _, i := range path {
		if n == nil || i < 0 || i >= len(n.Children) {
			return nil
		}
		n = &n.Children[i]
	}
	return n
}

// awaitLiveViewFrame drains frames until one of Type=="view" arrives, or
// deadline elapses. Every other frame type (snapshot/event/error) on this
// socket is ignored — the action-lifecycle arm is Task 7's concern, not
// this file's.
func awaitLiveViewFrame(t *testing.T, ch <-chan e2e.LiveFrame, deadline time.Time) e2e.LiveFrame {
	t.Helper()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("awaitLiveViewFrame: timed out waiting for a view-type frame")
		}
		select {
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("awaitLiveViewFrame: live socket closed before a view-type frame arrived")
			}
			if frame.Type == "view" {
				return frame
			}
		case <-time.After(remaining):
			t.Fatalf("awaitLiveViewFrame: timed out waiting for a view-type frame")
		}
	}
}

// topLevelKeys returns m's keys, for an ElementsMatch assertion against a
// decoded top-level JSON object.
func topLevelKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// captureLatestResult returns an OnToolResult predicate that always matches
// while stashing the parsed tool_result content into *dst. Safe to read
// *dst any time AFTER the h.ExpectAgentReply call for the SAME turn — that
// call is this file's synchronization point, so there is no race between
// the scripted LLM's goroutine writing *dst and this test's goroutine
// reading it afterward (unlike a value that must be waited on WHILE the
// turn is still in flight, this is a plain happens-before via
// ExpectAgentReply's own blocking read).
func captureLatestResult(dst *any) func(any) bool {
	return func(v any) bool {
		*dst = v
		return true
	}
}

// TestLeadsConsole_UpdateViewLive drives update_view/read_view through real
// agent turns against an already-open browser socket, asserting the live
// push carries the merged declaration (never the triggering envelope's own
// contents — see channelevents.UIViewUpdatePayload's doc comment: the push
// is a trigger, webd re-resolves), that the agent-composed marker lands
// ONLY on the slot actually written, and that the platform's own
// re-validation on read — not the agent's own honesty — is what makes a
// stored-but-now-invalid fragment fall back to Tier 0.
func TestLeadsConsole_UpdateViewLive(t *testing.T) {
	_, crm := newLeadflowServer(t)
	h := e2e.Start(t, e2e.Options{})
	applyLeadsConsoleFixture(t, h, crm.URL)
	stampAgentUIValidity(t, h, "default", "pipeline-desk-ui", pipelineDeskGrantedTools)
	h.WaitForAgentClassValid("pipeline-desk", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const viewerEmail = "viewer@demo.invalid"
	const summaryBody = "3 leads in New, 3 in Qualified, 3 in Proposal, 3 in Won."

	var readViewResult, updateViewValidResult, updateViewBadResult, updateViewPipelineResult any

	// Turn 1 (cold start): a plain respond_to_user, so the baseline below
	// can be observed BEFORE any write — chaining read_view/update_view
	// into this same turn would race the socket open against the write
	// (the runner starts processing the instant SendUserMessage publishes;
	// nothing blocks until the FULL chain lands).
	h.LLM.OnUserMessage("open the pipeline desk").
		Reply(e2e.RespondToUser("opened the pipeline desk"))

	// Turn 2: read_view (spec integration row 12), then a VALID update_view
	// write to the one hook the page declares ("summary" — the console's one
	// agentWritable slot).
	h.LLM.OnUserMessage("summarize the pipeline into the summary slot").
		Reply(e2e.ToolUse("read_view", map[string]any{}))
	h.LLM.OnToolResult("read_view", captureLatestResult(&readViewResult)).
		Reply(e2e.ToolUse("update_view", map[string]any{
			"hook": "summary",
			"node": map[string]any{
				"component": "ap:markdown",
				"props":     map[string]any{"body": summaryBody},
			},
		}))
	h.LLM.OnToolResult("update_view", captureLatestResult(&updateViewValidResult)).
		Reply(e2e.RespondToUser("updated the summary slot"))

	// Turn 3: a SECOND write to "summary" carrying a wire field the node
	// vocabulary does not define (an explicit agentComposed attempt) — must
	// be REJECTED, and turn 2's write must survive untouched.
	h.LLM.OnUserMessage("try a different summary for the pipeline slot").
		Reply(e2e.ToolUse("update_view", map[string]any{
			"hook": "summary",
			"node": map[string]any{
				"component":     "ap:markdown",
				"props":         map[string]any{"body": "a fragment trying to self-mark as agent-composed"},
				"agentComposed": false,
			},
		}))
	h.LLM.OnToolResult("update_view", captureLatestResult(&updateViewBadResult)).
		Reply(e2e.RespondToUser("tried a second summary"))

	// Turn 4: a write to "pipeline" — a NON-agentWritable slot compiles to no
	// hook at all, so this must be refused as an unknown hook, not merely a
	// non-writable one.
	h.LLM.OnUserMessage("update the pipeline slot too").
		Reply(e2e.ToolUse("update_view", map[string]any{
			"hook": "pipeline",
			"node": map[string]any{
				"component": "ap:markdown",
				"props":     map[string]any{"body": "the agent should not be able to write this slot"},
			},
		}))
	h.LLM.OnToolResult("update_view", captureLatestResult(&updateViewPipelineResult)).
		Reply(e2e.RespondToUser("tried the pipeline slot"))

	// Shared terminator for every turn above — the proven centerdot idiom.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.SendUserMessage("open the pipeline desk", e2e.AsUser(viewerEmail))
	h.ExpectAgentReply(e2e.Contains("opened the pipeline desk"))

	sess := e2e.FindOnlyAgentSession(t, h, h.Namespace())
	ns := h.Namespace()

	viewer := e2e.CanonicalForFakeEmail(viewerEmail)
	b := h.AgentUIBrowserFor("user:" + viewer.String())
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)

	// --- 1. Baseline, on the socket's OWN view of the document. -----------
	//
	// Opened after turn 1 (cold start) settles but before turn 2 (the
	// write) is even sent — the only point in this test's timeline where
	// "before any write" can be observed without racing the write itself.
	frames, closeLive, err := b.Live(ctx, ns, sess.Name)
	require.NoError(t, err, "dial the live socket")
	t.Cleanup(closeLive)

	firstFrame, ok := <-frames
	require.True(t, ok, "the live socket must deliver at least one frame")
	require.Equal(t, "snapshot", firstFrame.Type, "the FIRST frame on open must be the action snapshot")

	openFrame, ok := <-frames
	require.True(t, ok, "the live socket must deliver a second, open-time frame")
	require.Equal(t, "view", openFrame.Type, "the SECOND frame on open must be the view push")
	var openDecl wireDeclarationMirror
	require.NoError(t, json.Unmarshal(openFrame.Declaration, &openDecl))
	require.NotNil(t, nodeAtHook(t, openDecl, "summary"), "declaration must carry a summary hook with its Tier-0 default")
	assert.False(t, hookIsComposed(openDecl, "summary"), "summary must be Tier-0 (agentComposed=false) before any update_view write")

	// --- 2. The agent writes it. -------------------------------------------
	h.SendUserMessage("summarize the pipeline into the summary slot", e2e.AsUser(viewerEmail))
	h.ExpectAgentReply(e2e.Contains("updated the summary slot"))

	// Assert success AT THE TOOL CALL, not at the socket (mutation 3's own
	// discriminating requirement: a capability regression must fail here
	// first, not be inferred from what the socket did or didn't deliver).
	validBody, ok := updateViewValidResult.(map[string]any)
	require.True(t, ok, "update_view result must decode as an object: %#v", updateViewValidResult)
	assert.Equal(t, "summary", validBody["hook"])
	assert.Equal(t, true, validBody["updated"], "a rejected write with a green socket assertion would be a test of nothing")

	// read_view's own assertion (spec integration row 12): the declaration
	// carries the pipeline slot's OWN rows binding, and NOTHING resolved.
	// Pipeline is not agentWritable and compiles to no hook, so its binding
	// carries no region ("") — "rows" is the only prop name this page binds,
	// so it alone is enough to find the right one.
	readRaw, err := json.Marshal(readViewResult)
	require.NoError(t, err)
	var readResult readViewResultMirror
	require.NoError(t, json.Unmarshal(readRaw, &readResult))
	bound := uicomponents.WalkBindings(readResult.Declaration)
	var sawPipelineRows bool
	for _, bp := range bound {
		if bp.Prop != "rows" {
			continue
		}
		sawPipelineRows = true
		assert.Equal(t, "", bp.Region, "pipeline is not agentWritable and compiles to no hook, so its binding carries no region")
		assert.Equal(t, "tool", bp.Binding.Source, "pipeline/rows binding source")
		assert.Equal(t, "deals_list_leads", bp.Binding.Ref, "pipeline/rows binding ref")
	}
	assert.True(t, sawPipelineRows, "read_view's declaration must carry the pipeline slot's rows binding")
	// The TYPE itself carries no sixth field for resolved values
	// (readViewResultMirror mirrors meta.readViewResult exactly); this
	// checks the WIRE bytes too, so a hand-added JSON key on the production
	// side (bypassing the struct) would also be caught. jsx is the fifth
	// field, printed from the declaration alone — it carries no resolved
	// data of its own.
	var readTopLevel map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(readRaw, &readTopLevel))
	assert.ElementsMatch(t, []string{"declaration", "agentComposed", "parameters", "rejected", "jsx"}, topLevelKeys(readTopLevel),
		"read_view's payload must carry exactly these five fields — jsx is printed from the declaration only and carries no resolved data — never a sixth with resolved data")

	// --- 3. A "view" frame arrives on the OPEN socket, no reload. ----------
	pushed := awaitLiveViewFrame(t, frames, deadline)
	assert.Equal(t, "summary", pushed.Hook, "the pushed envelope must name the written hook")

	var pushedDecl wireDeclarationMirror
	require.NoError(t, json.Unmarshal(pushed.Declaration, &pushedDecl))
	assert.True(t, hookIsComposed(pushedDecl, "summary"), "summary must now be agentComposed")
	summaryNode := nodeAtHook(t, pushedDecl, "summary")
	require.NotNil(t, summaryNode, "the written node must be the hook's new content")
	assert.Equal(t, "ap:markdown", summaryNode.Component)
	var bodyProp string
	require.NoError(t, json.Unmarshal(summaryNode.Props["body"], &bodyProp))
	assert.Equal(t, summaryBody, bodyProp, "the pushed declaration must carry the WRITTEN node, not the envelope's own contents")

	// The marker is not the fragment's to set: it must land ONLY on the hook
	// actually written, never on any Tier-0 author region (a marker on
	// everything carries no information). "summary" is the console's only
	// hook, so the composed list naming exactly it also proves nothing else
	// was marked.
	assert.Equal(t, []string{"summary"}, pushedDecl.AgentComposed, "only the written hook may be agentComposed")

	// --- 4. The marker is not the fragment's to set (second write, bad). ---
	h.SendUserMessage("try a different summary for the pipeline slot", e2e.AsUser(viewerEmail))
	h.ExpectAgentReply(e2e.Contains("tried a second summary"))

	// A ParseNode-level rejection returns a plain string tool result
	// (update_view.go's Execute: the errors.As(*ValidationError) branch is
	// for a DIFFERENT rejection class — this one never reaches
	// Runtime.Write at all, since Node carries no agentComposed field for
	// ParseNode's DisallowUnknownFields to accept in the first place).
	badStr, ok := updateViewBadResult.(string)
	require.True(t, ok, "a parse-level rejection must be a plain string result, got %#v", updateViewBadResult)
	assert.Contains(t, badStr, "update_view", "the rejection must name the failing tool")

	// The previously-accepted slot must be UNCHANGED — re-resolve via the
	// page, not the socket (a second push for a REJECTED write would itself
	// be a defect, but this assertion must hold even if one arrived).
	propsRaw, err := b.Page(ctx, ns, sess.Name)
	require.NoError(t, err)
	unchangedProps, _ := decodePageProps(t, propsRaw)
	assert.True(t, hookIsComposed(unchangedProps.Declaration, "summary"), "the rejected write must not have cleared the earlier accepted one")
	unchangedNode := nodeAtHook(t, unchangedProps.Declaration, "summary")
	require.NotNil(t, unchangedNode)
	var unchangedBody string
	require.NoError(t, json.Unmarshal(unchangedNode.Props["body"], &unchangedBody))
	assert.Equal(t, summaryBody, unchangedBody, "the slot must still carry turn 2's accepted body, not the rejected fragment's")

	// --- 5. A non-agentWritable slot compiles to no hook, so it is refused. -
	h.SendUserMessage("update the pipeline slot too", e2e.AsUser(viewerEmail))
	h.ExpectAgentReply(e2e.Contains("tried the pipeline slot"))

	pipelineRejection, ok := updateViewPipelineResult.(map[string]any)
	require.True(t, ok, "a Runtime.Write-level rejection must be a structured object, got %#v", updateViewPipelineResult)
	assert.Equal(t, "invalid_view", pipelineRejection["error"])
	assert.Equal(t, "pipeline", pipelineRejection["hook"])
	assert.Contains(t, pipelineRejection["reason"], `unknown hook "pipeline"`,
		"a non-writable legacy slot compiles to no hook at all — the reason must say the hook is unknown, not merely off-limits")

	// --- 6. Read-time fallback: narrow the grant, re-open, Tier-0. --------
	h.ApplyManifest(narrowedGrantedToolsOverride)

	propsRaw2, err := b.Page(ctx, ns, sess.Name)
	require.NoError(t, err, "the page must still load — this is webd's re-validation dropping a fragment, not a doors-ladder denial")
	fallbackProps, _ := decodePageProps(t, propsRaw2)
	assert.False(t, hookIsComposed(fallbackProps.Declaration, "summary"), "after narrowing the grant, summary must serve its Tier-0 default again")
	fallbackNode := nodeAtHook(t, fallbackProps.Declaration, "summary")
	require.NotNil(t, fallbackNode)
	var fallbackBody string
	require.NoError(t, json.Unmarshal(fallbackNode.Props["body"], &fallbackBody))
	assert.NotEqual(t, summaryBody, fallbackBody, "the served body must be the Tier-0 default, not the agent's write")

	// --- 6b. The SAME fallback, through a FRESH socket's own open-time
	// push. -----------------------------------------------------------------
	//
	// Step 6 above proves webd's GET-page path re-validates on read; this
	// proves live.go's connect-time push (buildLiveViewMessage, called for
	// EVERY new socket, not only on a write) does too. The two call the
	// same resolveView/ResolveView path in production, but a mutant that
	// makes the live arm push a locally re-derived declaration instead of
	// resolveView's OWN re-validated one (see this file's own package doc
	// comment on why assertion 3 above cannot tell that apart: the write's
	// OWN push happens before any narrowing, so its content matches either
	// way) would show the stale, now-invalid write here and nowhere else a
	// no-reload client could observe it. A pre-existing OPEN socket cannot
	// exercise this — nothing re-triggers its already-delivered "view"
	// frame just because the grant changed — so this must be a NEW
	// connection.
	freshFrames, closeFresh, err := b.Live(ctx, ns, sess.Name)
	require.NoError(t, err, "dial a second, fresh live socket after narrowing")
	t.Cleanup(closeFresh)

	freshSnapshot, ok := <-freshFrames
	require.True(t, ok, "the fresh socket must deliver at least one frame")
	require.Equal(t, "snapshot", freshSnapshot.Type)

	freshView, ok := <-freshFrames
	require.True(t, ok, "the fresh socket must deliver a second, open-time frame")
	require.Equal(t, "view", freshView.Type)
	var freshDecl wireDeclarationMirror
	require.NoError(t, json.Unmarshal(freshView.Declaration, &freshDecl))
	assert.False(t, hookIsComposed(freshDecl, "summary"), "a FRESH socket's own open-time push must also revert to Tier-0 after narrowing")
	freshNode := nodeAtHook(t, freshDecl, "summary")
	require.NotNil(t, freshNode)
	var freshBody string
	require.NoError(t, json.Unmarshal(freshNode.Props["body"], &freshBody))
	assert.NotEqual(t, summaryBody, freshBody, "a FRESH socket's push must serve the Tier-0 body, not the agent's write")

	h.LLM.AssertAllRulesConsumed()
}
