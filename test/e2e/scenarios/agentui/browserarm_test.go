//go:build e2e

package agentui_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uidemo/leadflow"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// applyLeadsConsoleFixture applies every *.yaml file in
// test/e2e/testdata/leads-console-e2e (sorted by the 00-/01-/.../04- prefix
// order that encodes apply order), substituting the sentinel {{CRM_URL}}
// with crmURL. Deliberately NOT Options.AgentDir: that mechanism
// substitutes the DIFFERENT sentinel {{MCP_URL}} with the harness's own
// MCPStub, and this fixture needs a REAL pkg/web/uidemo/leadflow server instead
// — see 01-mcpserver.yaml's own doc comment for why.
func applyLeadsConsoleFixture(t *testing.T, h *e2e.Harness, crmURL string) {
	t.Helper()
	dir := e2e.TestdataDir("leads-console-e2e")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read leads-console-e2e fixture dir")

	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "read fixture file %s", name)
		substituted := strings.ReplaceAll(string(raw), "{{CRM_URL}}", crmURL)
		h.ApplyManifest(substituted)
	}
}

// newLeadflowServer starts a real pkg/web/uidemo/leadflow MCP server and
// registers its cleanup. Returned alongside the httptest.Server so a caller
// can also reach book/call-log accessors off the underlying *leadflow.Server
// when needed (leads_console_read_test.go's J4 spanning assertion).
func newLeadflowServer(t *testing.T) (*leadflow.Server, *httptest.Server) {
	t.Helper()
	srv := leadflow.New(leadflow.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// fixtureWireDeclaration mirrors pkg/controllers/agentui's own unexported
// wireDeclaration (controller.go) — the {"view": <node>} envelope
// parseWireNode wraps ONE raw node (spec.View, or a single slot's Default)
// in so it strict-decodes through uicomponents.ParseDeclaration exactly like
// an author-submitted page does. Duplicated rather than imported because
// that type is unexported in a package this test must not depend on for its
// production import graph.
type fixtureWireDeclaration struct {
	View json.RawMessage `json:"view,omitempty"`
}

// fixtureParseWireNode mirrors the controller's own unexported
// parseWireNode: wrap one raw node as a one-node wireDeclaration and
// strict-decode it through ParseDeclaration, returning just the parsed node.
func fixtureParseWireNode(t *testing.T, raw []byte) *uicomponents.Node {
	t.Helper()
	wire, err := json.Marshal(fixtureWireDeclaration{View: json.RawMessage(raw)})
	require.NoError(t, err, "marshal wire declaration")
	one, err := uicomponents.ParseDeclaration(wire)
	require.NoError(t, err, "parse declaration")
	return one.View
}

// stampAgentUIValidity computes AgentUI Valid the SAME way
// pkg/controllers/agentui's Reconciler would (its unexported
// validateDeclaration: parse spec.View whole, or compile spec.Slots into one
// page tree, into a single uicomponents.Declaration; add spec.Actions;
// validate against the eligibility ceiling uigrant.Ceiling(spec.Tools,
// grantedTools)) and stamps the resulting Condition directly.
//
// This harness does not register that reconciler — see
// view_capability_test.go's markAgentUIValid doc comment for why (growing
// the shared harness for one task's scenarios is out of scope). Unlike
// markAgentUIValid's unconditional True, this is a REAL validation call:
// dropping a tool from grantedTools (leads_console_read_test.go's mutation
// 3) changes what this function stamps, exactly as the live reconciler's
// own re-reconcile would — which is what lets that mutation observe the
// doors ladder's real 422, not a hardcoded fixture response.
func stampAgentUIValidity(t *testing.T, h *e2e.Harness, ns, name string, grantedTools []string) {
	t.Helper()
	ctx := context.Background()

	var aui spiceboxv1alpha1.AgentUI
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &aui), "get AgentUI %s/%s", ns, name)

	ceiling := uigrant.Ceiling(aui.Spec.Tools, grantedTools)
	set := make(map[string]bool, len(ceiling))
	for _, toolName := range ceiling {
		set[toolName] = true
	}
	opts := uicomponents.DefaultOptions()
	opts.GrantedTools = set
	opts.ReadonlyTools = set
	opts.NormalizeToolName = synthesize.NormalizeName

	var decl uicomponents.Declaration
	for _, a := range aui.Spec.Actions {
		var args json.RawMessage
		if a.Args != nil {
			args = json.RawMessage(a.Args.Raw)
		}
		decl.Actions = append(decl.Actions, uicomponents.Action{
			Name: a.Name, Tool: a.Tool, Args: args, Inputs: a.Inputs,
		})
	}

	// Mirrors validateDeclaration's own view/slots switch: spec.View parses
	// whole, spec.Slots parses each slot's Default on its own (for
	// per-slot attribution — see that function's doc comment) and then
	// Normalize compiles the whole slots list into ONE page tree, exactly as
	// the controller's admission does.
	switch {
	case aui.Spec.View != nil:
		decl.View = fixtureParseWireNode(t, aui.Spec.View.Raw)
	default:
		for _, slot := range aui.Spec.Slots {
			s := uicomponents.Slot{Name: slot.Name, AgentWritable: slot.AgentWritable}
			if slot.Default != nil {
				s.Default = fixtureParseWireNode(t, slot.Default.Raw)
			}
			decl.Slots = append(decl.Slots, s)
		}
		normalized, err := uicomponents.Normalize(decl)
		require.NoError(t, err, "normalize compiled slots declaration")
		decl = normalized
	}

	cond := metav1.Condition{
		Type:               spiceboxv1alpha1.AgentUIConditionValid,
		LastTransitionTime: metav1.Now(),
	}
	if verr := uicomponents.Validate(decl, opts); verr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "InvalidDeclaration"
		cond.Message = verr.Error()
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = spiceboxv1alpha1.ReasonAgentUISpecOK
	}
	aui.Status.Conditions = []metav1.Condition{cond}
	require.NoError(t, h.K8s.Status().Update(ctx, &aui), "stamp AgentUI %s/%s validity", ns, name)
}

// pipelineDeskGrantedTools is the fixture AgentClass's own
// spec.agentUI.grantedTools list (03-agentclass.yaml) — the SAME literal a
// scenario passes to stampAgentUIValidity so the two stay in sync without a
// second source of truth. leads_console_read_test.go's mutation 3 builds
// its own narrowed copy rather than mutating this slice.
var pipelineDeskGrantedTools = []string{"deals_list_leads", "deals_stage_breakdown", "deals_advance_lead_stage"}

// TestAgentUIBrowserArm_SelfTest proves three facts about the arm's OWN
// wiring — so leads_console_read_test.go (and any future scenario built on
// this arm) can trust it rather than a broken arm reading as a broken
// feature three tasks downstream:
//
//  1. A subject WITH agentsession#interact standing gets the page's props
//     back.
//  2. A subject WITHOUT that standing is denied (403), proving
//     webui.WithSubjectForTest is actually reaching SubjectFromContext and
//     CheckInteract is actually gating — the arm is not silently
//     authorizing itself.
//  3. A /bindings POST carrying an undeclared parameter key is rejected
//     with 400 (bindingsHandler's step 6 — see bindings.go), proving the
//     POST route is reachable and its request-shape gate is live.
//
// None of these three facts needs the CRM tools or the fixture's own
// dealbook_lead schema/bootstrap to have converged — but CheckInteract
// itself DOES need Guardian to have composed the BASE schema (the
// `agentsession` object definition every CheckInteract call resolves
// against) at least once, which only happens once Guardian has reconciled
// an MCPServer/Guardian-relevant CR. WaitForAgentClassValid is what
// triggers and observes that composition here, exactly as every other
// e2e scenario waits before its first SpiceDB write/check.
//
// The AgentSession is created through h.SendUserMessage (the same
// channel-attached path leads_console_read_test.go and
// leads_console_action_test.go use for this identical fixture message), not
// a bare h.K8s.Create with no InputChannel. Every AgentSession's spec.Prompt
// is required and the in-process runner's cold start always places it as
// turn 0 and answers it through the (scripted) LLM before the session
// reaches any phase this page will serve — there is no such thing as a live
// AgentSession that has made zero LLM calls (see
// TestLeadsConsole_TierZeroPaintsAndReQueries's doc comment). A session
// created with no InputChannel forces that turn to end TERMINALLY
// (agent_work_complete -> Succeeded, or a stalled/no-tool_use turn ->
// Failed — loop.go has no non-terminal outcome for a session that isn't
// ChannelAttached), and ResolveSession maps both Succeeded and Failed to
// the Started branch, which resolveAgentUIDoors turns into a 410 for every
// subtest below. So the cold-start turn must both be scripted (an
// unscripted call previously ran on a background goroutine that raced this
// test's own teardown, intermittently hitting ScriptedLLM's t.Fatalf on a
// goroutine other than the one running the test) and be awaited before any
// subtest touches the session, and it must resolve to a phase that stays on
// the Attached branch — which is exactly what a channel-attached
// agent_work_complete (Idle, not Succeeded) gives.
func TestAgentUIBrowserArm_SelfTest(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	_, crm := newLeadflowServer(t)
	applyLeadsConsoleFixture(t, h, crm.URL)
	stampAgentUIValidity(t, h, "default", "pipeline-desk-ui", pipelineDeskGrantedTools)
	h.WaitForAgentClassValid("pipeline-desk", 30*time.Second)

	const participantEmail = "viewer@demo.invalid"

	// Cold-start turn: same single-shot-terminal shape leads_console_read_test.go
	// and leads_console_action_test.go script for this identical fixture
	// message. Repeating() because a spec.Prompt replay can drive the seed
	// message through the LLM more than once (see those files' identical
	// rule and comment); agent_work_complete needs no respond_to_user
	// round-trip to reach Idle.
	h.LLM.OnUserMessage("open the pipeline desk").
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "opened the pipeline desk"})).
		Repeating()

	h.SendUserMessage("open the pipeline desk", e2e.AsUser(participantEmail))
	e2e.WaitForSessionIdle(t, h)

	sess := e2e.FindOnlyAgentSession(t, h, h.Namespace())

	// agentsession#owner is granted to participant's canonical subject by
	// the SendUserMessage pipeline itself (per-turn authorship) — no
	// explicit WriteRel needed, matching leads_console_read_test.go and
	// leads_console_action_test.go's identical SendUserMessage-then-Page
	// sequence. stranger never sent anything, so it is granted nothing.
	participant := e2e.CanonicalForFakeEmail(participantEmail)
	stranger := e2e.CanonicalForFakeEmail("stranger@demo.invalid")

	ctx := context.Background()

	t.Run("subject WITH interact standing gets the page's props", func(t *testing.T) {
		b := h.AgentUIBrowserFor("user:" + participant.String())
		props, err := b.Page(ctx, "default", sess.Name)
		require.NoError(t, err, "Page must succeed for a viewer with agentsession#interact")
		assert.Contains(t, string(props), `"declaration"`, "props must carry the bootstrap declaration")
		assert.Contains(t, string(props), `"ns":"default"`, "props must echo the URL's namespace")
	})

	t.Run("subject WITHOUT interact standing is denied", func(t *testing.T) {
		b := h.AgentUIBrowserFor("user:" + stranger.String())
		_, err := b.Page(ctx, "default", sess.Name)
		require.Error(t, err, "Page must be denied for a viewer with no agentsession#interact standing")
		var pe *webui.PageError
		require.True(t, errors.As(err, &pe), "error must be a *webui.PageError, got %T: %v", err, err)
		assert.Equal(t, http.StatusForbidden, pe.Status, "a clean interact denial is 403, not a 401/500")
	})

	t.Run("bindings POST with an undeclared parameter key is rejected 400", func(t *testing.T) {
		b := h.AgentUIBrowserFor("user:" + participant.String())
		_, status, err := b.Bindings(ctx, "default", sess.Name, map[string]string{"windo.from": "2026-01-01T00:00:00Z"})
		require.NoError(t, err, "Bindings transport must succeed even though the request is rejected")
		assert.Equal(t, http.StatusBadRequest, status, "an undeclared parameter key must reject the WHOLE request, not be silently dropped")
	})
}
