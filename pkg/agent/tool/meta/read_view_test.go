package meta_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// boundDataSentinel is a value the fixture's granted, readonly tool
// "crm_list_leads" would return if anything ever resolved it. Nothing in
// this package's ReadViewConfig can reach a tool resolver (see
// meta.NewReadView's own doc comment), so nothing ever produces this string
// — its absence from read_view's payload is the assertion, not a resolver
// this test wires up and disables.
const boundDataSentinel = "LEADS_SENTINEL_VALUE_NEVER_RESOLVED"

// seedFragment writes a fragment through the real Runtime.Write — the same
// validated path update_view uses — so a test can set up an
// already-accepted slot without going through the tool's Execute.
func seedFragment(t *testing.T, ctx context.Context, rt *uiview.Runtime, slot, nodeJSON string) error {
	t.Helper()
	node, err := uicomponents.ParseNode([]byte(nodeJSON))
	if err != nil {
		return err
	}
	_, err = rt.Write(ctx, slot, node)
	return err
}

// seedFragmentRaw stores a fragment directly in the view-model store,
// bypassing Write's validate-then-write contract — the same end state a
// bundle redeploy produces when it narrows a grant or removes an action out
// from under an already-accepted record. Used to exercise read_view's
// "rejected" reporting, since Write itself refuses to store anything invalid.
func seedFragmentRaw(t *testing.T, ctx context.Context, rt *uiview.Runtime, slot, nodeJSON string) error {
	t.Helper()
	return uiviewmodel.Record(ctx, rt.Mem, memory.Scope{Kind: "session", ID: rt.Namespace + "/" + rt.Session}, uiviewmodel.Content{
		UI: rt.UIName, Slot: slot, Node: json.RawMessage(nodeJSON), WrittenAt: time.Now().UTC(),
	})
}

func TestReadViewReturnsTheMergedDeclarationAndNoValues(t *testing.T) {
	ctx, rt := newRuntimeFixture(t) // the SAME fixture Task 4 built; do not write a second one
	require.NoError(t, seedFragment(t, ctx, rt, "panel", `{"component":"ap:markdown","props":{"body":"agent copy"}}`))

	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	var out struct {
		Declaration   json.RawMessage                 `json:"declaration"`
		AgentComposed []string                        `json:"agentComposed"`
		Parameters    []struct{ Key, Value string }   `json:"parameters"`
		Rejected      []struct{ Hook, Reason string } `json:"rejected"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	assert.Equal(t, []string{"panel"}, out.AgentComposed)
	assert.Contains(t, string(out.Declaration), "ap:markdown", "the merged slot, not the Tier-0 one")
	assert.Empty(t, out.Rejected)

	// The Tier-0 binding the agent never wrote is DESCRIBED and never
	// RESOLVED: its ref is present, and no result value is anywhere in the
	// payload. The fixture's bound tool ("crm_list_leads") would return
	// boundDataSentinel if anything ever called it; its absence is the
	// assertion.
	assert.Contains(t, string(out.Declaration), "crm_list_leads")
	assert.NotContains(t, res.Content, boundDataSentinel)
}

func TestReadViewReportsCurrentParametersFromTheDeclaration(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	var out struct {
		Parameters []struct{ Key, Value string } `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	require.Len(t, out.Parameters, 1)
	assert.Equal(t, "span", out.Parameters[0].Key)
	assert.Equal(t, "30d", out.Parameters[0].Value)
}

func TestReadViewSurfacesRejectedFragments(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	// A fragment stored directly (bypassing Write, as a bundle redeploy
	// effectively does by moving the CR under an already-legal record).
	require.NoError(t, seedFragmentRaw(t, ctx, rt, "panel",
		`{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_delete_lead"}}}`))

	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	var out struct {
		Rejected []struct{ Hook, Reason string } `json:"rejected"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	require.Len(t, out.Rejected, 1, "the agent must be able to learn its own slot is not being shown")
	assert.Equal(t, "panel", out.Rejected[0].Hook)
	assert.NotEmpty(t, out.Rejected[0].Reason)
}

// TestReadViewResolvesNoBindingFromAnySource is the CONTENT half of the "no
// values, ever" pin: every one of the fixture's Tier-0 bindings has real,
// resolvable content behind it (plantBindableData), so a leak from any source
// — including one smuggled into an EXISTING field rather than added as a new
// one, which the structural pin in read_view_internal_test.go cannot see —
// puts a known sentinel in the payload.
//
// Only the `tool` source has no sentinel: its transport is absent by type, so
// no value exists for it to leak. boundDataSentinel stands in as the value
// that WOULD appear, and its absence is asserted alongside the rest.
func TestReadViewResolvesNoBindingFromAnySource(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	require.NoError(t, seedFragment(t, ctx, rt, "panel", `{"component":"ap:markdown","props":{"body":"agent copy"}}`))

	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	// Every source's binding is DESCRIBED — otherwise the absences below would
	// be absences of bindings, not of values.
	for _, ref := range []string{"crm_list_leads", "label", "advance", fixtureArtifactHandle} {
		assert.Contains(t, res.Content, ref, "the merged declaration must still describe every Tier-0 binding")
	}

	for name, value := range map[string]string{
		"tool":     boundDataSentinel,
		"memory":   memoryLeakSentinel,
		"action":   actionLeakSentinel(),
		"artifact": artifactLeakSentinel,
	} {
		assert.NotContains(t, res.Content, value,
			"read_view resolved a %s-source binding: the agent must get UI data by calling the binding's own tool, "+
				"under the ordinary per-turn budget, and through no other path", name)
	}
}

// TestReadViewPayloadCarriesNoOtherFields pins the payload's key set as the
// wire actually carried it. It is the RUNTIME half of the same guarantee
// read_view_internal_test.go's TestReadViewResultCarriesNoOtherFieldByType
// pins over the TYPE: this one catches a fifth key produced by something
// other than a struct field (a custom MarshalJSON, an inlined map), the other
// catches a fifth field that is `omitempty` and empty on this run.
//
// Neither alone is enough, and this one is the weaker of the two: its reach
// depends on the fixture having content behind every binding source for a
// leak to be non-empty, which plantBindableData is what supplies.
func TestReadViewPayloadCarriesNoOtherFields(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(res.Content), &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"declaration", "agentComposed", "parameters", "rejected", "jsx"}, keys,
		"read_view's payload must carry ONLY the declaration, the composed-slot marker, current parameters, "+
			"rejections, and the JSX printout — never a data field of any kind")
}

func TestReadViewWithNoRuntimeFailsLoudly(t *testing.T) {
	res, err := meta.NewReadView(meta.ReadViewConfig{}).Execute(context.Background(), json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestReadViewReturnsThePageAsJSXWithTheAgentsFillMarked(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	require.NoError(t, seedFragment(t, ctx, rt, "panel", `{"component":"ap:markdown","props":{"body":"agent copy"}}`))

	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)

	var out struct {
		JSX string `json:"jsx"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	assert.Contains(t, out.JSX, `<oap:generative name="panel"`, "the hook is in place")
	assert.Contains(t, out.JSX, "{/* your fill */}\n", "and marked as the agent's own")
	assert.Contains(t, out.JSX, `<ap:markdown body="agent copy" />`, "with the fill inside it")
	assert.Equal(t, out.JSX, uicomponents.JSX(mustView(t, ctx, rt).Declaration, mustView(t, ctx, rt).AgentComposed),
		"read_view's jsx is exactly the printer's output over the merged view")
}

// mustView reads the merged view straight from the Runtime, for comparing
// against what read_view printed.
func mustView(t *testing.T, ctx context.Context, rt *uiview.Runtime) uicomponents.View {
	t.Helper()
	v, err := rt.View(ctx)
	require.NoError(t, err)
	return v
}

// TestReadViewDoesNotHTMLEscapeItsPayload pins the SetEscapeHTML(false)
// choice: a model reading `<oap:generative` is reading noise, and this
// payload is a tool result, never a browser surface.
func TestReadViewDoesNotHTMLEscapeItsPayload(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)
	res, err := meta.NewReadView(meta.ReadViewConfig{View: rt}).Execute(ctx, json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	require.False(t, res.IsError, "Content: %s", res.Content)
	assert.Contains(t, res.Content, `<oap:generative`)
	assert.NotContains(t, res.Content, `\u003c`)
}

func TestReadViewDescriptionSaysCallItFirstAndWhatItReturns(t *testing.T) {
	desc := meta.NewReadView(meta.ReadViewConfig{}).Description()
	for _, want := range []string{"FIRST", "JSX", "NO bound data"} {
		assert.Contains(t, desc, want)
	}
}
