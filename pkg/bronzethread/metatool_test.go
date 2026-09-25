package bronzethread_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// cannableName is a meta tool bronzethread's own table declares cannable. Read
// through the table rather than spelled as a literal, so a row leaving
// metaOverrides fails these tests loudly instead of leaving them exercising a
// tool that now runs for real.
func cannableName(t *testing.T) string {
	t.Helper()
	names := bt.CannableMetaTools()
	require.NotEmpty(t, names, "no meta tool is cannable; these tests have nothing to exercise")
	return names[0]
}

// fakeMetaTool is a minimal tool.Tool the canner can wrap. It implements NONE
// of the optional interfaces on purpose — the case that does gets its own type
// below, so a wrapper that started dropping one silently would fail here rather
// than in whatever consumed the interface.
type fakeMetaTool struct {
	name string
	kind tool.Kind
	ran  *bool
}

func (f fakeMetaTool) Name() string        { return f.name }
func (f fakeMetaTool) Kind() tool.Kind     { return f.kind }
func (f fakeMetaTool) Description() string { return "describes " + f.name }
func (f fakeMetaTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
}
func (f fakeMetaTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (f fakeMetaTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f fakeMetaTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	if f.ran != nil {
		*f.ran = true
	}
	return tool.Result{Content: "the real tool ran"}, nil
}

// introspectableMetaTool is a meta tool carrying one of the optional interfaces
// a wrapper cannot forward.
type introspectableMetaTool struct{ fakeMetaTool }

func (introspectableMetaTool) Introspect() (string, error) { return "the contract", nil }

// TestMetaToolCannable_ReadsTheDispositionTable pins that the cannable set is
// the disposition table's ReplayOutput rows and nothing else.
//
// One table, two readers, so the set of tools whose OUTPUT is replayed and the
// set a bundle may CAN cannot drift into disagreement.
func TestMetaToolCannable_ReadsTheDispositionTable(t *testing.T) {
	for _, name := range bt.CannableMetaTools() {
		assert.True(t, bt.MetaToolCannable(name), "%q is listed as cannable and must report so", name)
	}
	assert.False(t, bt.MetaToolCannable("update_plan"),
		"update_plan is the code under test; canning it would serve a regression in it the recorded answer")
	assert.False(t, bt.MetaToolCannable("respond_to_user"),
		"the reply path runs for real; the bundle's own transcript is what drives it")
	assert.False(t, bt.MetaToolCannable(""), "an empty name matches nothing")
	assert.IsIncreasing(t, bt.CannableMetaTools(), "the list is sorted, so an error message is stable")
}

// TestBundle_ValidateMetaToolReplies is the enforcement this feature turned on.
//
// The rule that a canned reply needs a matching assert.toolsCalled entry was
// written into Assertions.ToolsCalled long before anything could break it. This
// is where it stops being documentation: Bundle.Validate — the one method the
// driver refuses on and the capture's self-check raises a hard finding on —
// refuses the bundle outright.
func TestBundle_ValidateMetaToolReplies(t *testing.T) {
	canned := cannableName(t)
	body := bt.MetaToolReply{Content: `{"entries":[]}`}

	cases := []struct {
		name    string
		doctor  func(b bt.Bundle) bt.Bundle
		wantErr string // empty means the bundle must validate
	}{
		{
			name: "a canned reply named in toolsCalled validates",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MetaToolReplies = map[string]bt.MetaToolReply{canned: body}
				b.Assert.ToolsCalled = []string{canned}
				return b
			},
		},
		{
			name: "a canned reply with NO toolsCalled entry is refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MetaToolReplies = map[string]bt.MetaToolReply{canned: body}
				return b
			},
			wantErr: "assert.toolsCalled does not name it",
		},
		{
			name: "toolsCalled naming a DIFFERENT tool does not excuse it",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MetaToolReplies = map[string]bt.MetaToolReply{canned: body}
				b.Assert.ToolsCalled = []string{"acme_list_widgets"}
				return b
			},
			wantErr: "assert.toolsCalled does not name it",
		},
		{
			name: "a tool that runs for real cannot be canned",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MetaToolReplies = map[string]bt.MetaToolReply{"update_plan": body}
				b.Assert.ToolsCalled = []string{"update_plan"}
				return b
			},
			wantErr: "is not cannable",
		},
		{
			name: "an empty canned body is refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MetaToolReplies = map[string]bt.MetaToolReply{canned: {}}
				b.Assert.ToolsCalled = []string{canned}
				return b
			},
			wantErr: "has no content",
		},
		{
			name:   "a bundle canning nothing is unaffected",
			doctor: func(b bt.Bundle) bt.Bundle { return b },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doctor(runnableBundle()).Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "metaToolReplies",
				"the message names the field, so a reader knows which half of the bundle to fix")
		})
	}
}

// TestMetaToolCanner_ServesTheRecordedReplyAndNothingElse is the seam itself.
//
// Only Execute is replaced. Name, kind, description, input schema and declared
// permission are the assembled tool's own, which is what keeps the catalog, the
// system prompt and the gate looking at the session that was recorded.
func TestMetaToolCanner_ServesTheRecordedReplyAndNothingElse(t *testing.T) {
	canned := cannableName(t)
	ran := false
	real := fakeMetaTool{name: canned, kind: tool.KindMeta, ran: &ran}

	c := bt.NewMetaToolCanner(map[string]bt.MetaToolReply{canned: {Content: "the recorded answer"}})
	require.NotNil(t, c)
	got, err := c.Replacer()(real)
	require.NoError(t, err)

	res, err := got.Execute(context.Background(), json.RawMessage(`{"text":"anything"}`), nil)
	require.NoError(t, err)
	assert.Equal(t, "the recorded answer", res.Content)
	assert.False(t, res.IsError, "an error is never canned, so a canned reply is never an error")
	assert.False(t, res.Trusted,
		"untrusted, matching the tool's own success path, so the runner wraps it in the same envelope")
	assert.False(t, ran, "the real tool's body must not run; that is the whole point and the whole cost")

	assert.Equal(t, real.Name(), got.Name())
	assert.Equal(t, real.Kind(), got.Kind())
	assert.Equal(t, real.Description(), got.Description())
	assert.JSONEq(t, string(real.InputSchema()), string(got.InputSchema()))
	assert.Equal(t, real.Permission(), got.Permission(),
		"the declared permission is what the authz check and the plan gate run against, and a canned "+
			"reply must not move that gate")
}

// TestMetaToolCanner_LeavesEverythingElseAlone: a tool the bundle does not can
// is handed back as the identical value, not a wrapper.
func TestMetaToolCanner_LeavesEverythingElseAlone(t *testing.T) {
	c := bt.NewMetaToolCanner(map[string]bt.MetaToolReply{cannableName(t): {Content: "x"}})
	other := fakeMetaTool{name: "update_plan", kind: tool.KindMeta}

	got, err := c.Replacer()(other)
	require.NoError(t, err)
	assert.Equal(t, tool.Tool(other), got, "an uncanned tool is returned unchanged, not re-wrapped")
}

// TestMetaToolCanner_NilWhenNothingIsCanned pins the compatibility path: a
// bundle canning nothing wires no seam at all, so every existing scenario
// assembles exactly as before.
func TestMetaToolCanner_NilWhenNothingIsCanned(t *testing.T) {
	assert.Nil(t, bt.NewMetaToolCanner(nil))
	assert.Nil(t, bt.NewMetaToolCanner(map[string]bt.MetaToolReply{}))
	assert.Nil(t, bt.NewMetaToolCanner(nil).Replacer(), "a nil canner wires nothing")
	assert.Empty(t, bt.NewMetaToolCanner(nil).Unapplied(), "and reports nothing outstanding")
}

// TestMetaToolCanner_RefusesWhatItCannotWrapFaithfully covers the two ways
// wrapping would change more than the reply.
//
// Both fail CLOSED — an error the driver surfaces — rather than wrapping
// anyway. A wrapper cannot forward an optional interface it does not name
// (embedding tool.Tool promotes only tool.Tool's own methods), so a tool
// carrying one would quietly lose behaviour the reply was never meant to touch.
func TestMetaToolCanner_RefusesWhatItCannotWrapFaithfully(t *testing.T) {
	canned := cannableName(t)

	cases := []struct {
		name    string
		tool    tool.Tool
		wantErr string
	}{
		{
			name:    "a tool carrying an optional interface the wrapper would drop",
			tool:    introspectableMetaTool{fakeMetaTool{name: canned, kind: tool.KindMeta}},
			wantErr: "tool.Introspectable",
		},
		{
			name:    "a name the run assembled as a transport rather than a meta tool",
			tool:    fakeMetaTool{name: canned, kind: tool.KindMCP},
			wantErr: "rather than a meta tool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := bt.NewMetaToolCanner(map[string]bt.MetaToolReply{canned: {Content: "x"}})
			got, err := c.Replacer()(tc.tool)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, got, "a refusal hands back nothing, so a caller cannot use the unwrapped tool "+
				"and believe the reply was canned")
			assert.Equal(t, []string{canned}, c.Unapplied(),
				"a refused replacement was never applied and must still be reported outstanding")
		})
	}
}

// TestMetaToolCanner_UnappliedNamesWhatNeverReachedTheAssembly is the check
// that keeps a canned reply from vanishing silently.
//
// A canning that never happened is invisible in every other assertion: the real
// tool runs, answers out of an empty fixture, and the step fails downstream
// naming the tool rather than the wiring. Asking afterwards is what makes the
// wiring itself testable — the same reason ToolCatalogCheck.Consulted exists.
func TestMetaToolCanner_UnappliedNamesWhatNeverReachedTheAssembly(t *testing.T) {
	canned := cannableName(t)
	c := bt.NewMetaToolCanner(map[string]bt.MetaToolReply{canned: {Content: "x"}})

	assert.Equal(t, []string{canned}, c.Unapplied(), "before assembly, nothing has been applied")

	_, err := c.Replacer()(fakeMetaTool{name: "update_plan", kind: tool.KindMeta})
	require.NoError(t, err)
	assert.Equal(t, []string{canned}, c.Unapplied(), "assembling some OTHER tool applies nothing")

	_, err = c.Replacer()(fakeMetaTool{name: canned, kind: tool.KindMeta})
	require.NoError(t, err)
	assert.Empty(t, c.Unapplied(), "once the canned tool is assembled, nothing is outstanding")
}

// TestBundle_MetaToolRepliesRoundTripThroughJSON pins the wire name, because
// the bundle format is a file a human reads and edits.
func TestBundle_MetaToolRepliesRoundTripThroughJSON(t *testing.T) {
	canned := cannableName(t)
	b := runnableBundle()
	b.MetaToolReplies = map[string]bt.MetaToolReply{canned: {Content: `{"entries":[]}`}}
	b.Assert.ToolsCalled = []string{canned}

	raw, err := json.Marshal(b)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"metaToolReplies"`)
	assert.Contains(t, string(raw), `"content"`)

	var back bt.Bundle
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, b.MetaToolReplies, back.MetaToolReplies)
	require.NoError(t, back.Validate())

	// Omitted entirely when empty, so no existing bundle gains a field.
	plain, err := json.Marshal(runnableBundle())
	require.NoError(t, err)
	assert.NotContains(t, string(plain), "metaToolReplies")
}
