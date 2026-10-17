package browser

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

// axVal builds an AXValue carrying the JSON-encoded string axValueString decodes
// — the shape GetFullAXTree returns a role/name/value in.
func axVal(s string) *accessibility.Value {
	q, _ := stdjson.Marshal(s)
	return &accessibility.Value{Type: accessibility.ValueTypeString, Value: q}
}

// blankingTree is the minimal shape the §3.6 vector lives in: a form-control
// node (the input) whose live rendered content Chrome exposes as a CHILD
// static-text node carrying the actual typed value in its NAME. The input's own
// node also carries a value, so a correct suppression hides BOTH. Backend IDs:
// root=100, input=200, value-child=300.
func blankingTree() []*accessibility.Node {
	return []*accessibility.Node{
		{
			NodeID:           "1",
			Role:             axVal("rootwebarea"),
			BackendDOMNodeID: cdp.BackendNodeID(100),
			ChildIDs:         []accessibility.NodeID{"2"},
		},
		{
			NodeID:           "2",
			Role:             axVal("textbox"),
			Name:             axVal("Password"),
			Value:            axVal("ownsecret"),
			ParentID:         "1",
			BackendDOMNodeID: cdp.BackendNodeID(200),
			ChildIDs:         []accessibility.NodeID{"3"},
		},
		{
			NodeID:           "3",
			Role:             axVal("statictext"),
			Name:             axVal("childsecret"),
			ParentID:         "2",
			BackendDOMNodeID: cdp.BackendNodeID(300),
		},
	}
}

// TestBuildSnapshotSuppressesBlankedSubtree locks the suppression MECHANISM
// (not just the plaintext-absent outcome): when the input's backend ID is
// blanked, buildSnapshot must drop the entire subtree under it — so the
// value-carrying child node produces NO line at all. This still guards if a
// future Chrome moves the rendered value onto the input's own node, and the
// converse: it must not over-suppress when nothing is blanked.
func TestBuildSnapshotSuppressesBlankedSubtree(t *testing.T) {
	nodes := blankingTree()

	// Blanked: the child's value-bearing line is gone; the input line stays but
	// redacted; neither secret appears. gen=1 so refs are "s1e1", "s1e2", ...
	lines, refs := buildSnapshot(nodes, 0, 1, map[int64]bool{200: true})
	out := renderOutline(lines)
	assert.NotContains(t, out, "childsecret", "a blanked node's value-child must be suppressed entirely")
	assert.NotContains(t, out, "ownsecret", "a blanked node's own value must be redacted")
	assert.Contains(t, out, "redacted", "the blanked node must still render, carrying the redaction marker")
	assert.Contains(t, out, "Password", "the element itself (role/name) still appears")

	require.Len(t, lines, 2, "root + the (redacted) input only — the value-child line is gone")
	assert.True(t, lines[1].blanked)
	assert.Empty(t, lines[1].value, "a blanked node carries no value")
	_, hasChildRef := refs["s1e3"]
	assert.False(t, hasChildRef, "a suppressed descendant gets no ref")

	// Not blanked: everything renders, both values present.
	lines2, _ := buildSnapshot(nodes, 0, 1, map[int64]bool{})
	out2 := renderOutline(lines2)
	assert.Contains(t, out2, "ownsecret")
	assert.Contains(t, out2, "childsecret")
	assert.Len(t, lines2, 3, "root + input + value-child all render when nothing is blanked")
}

// TestBuildSnapshotGenerationScopedRefs proves refs carry the generation nonce
// in their names and that two snapshots of the SAME tree under DIFFERENT
// generations produce DISJOINT ref names. That disjointness is the structural
// property behind the concurrent-dispatch fix: a parallel read re-minting refs
// between a tool call's classify (NamedArgs) and its execute advances the
// generation, so the model's older ref is simply absent from the current map
// and resolves stale — never silently rebound to a different element at the
// same index.
func TestBuildSnapshotGenerationScopedRefs(t *testing.T) {
	nodes := blankingTree()
	_, refs7 := buildSnapshot(nodes, 0, 7, map[int64]bool{})
	_, refs8 := buildSnapshot(nodes, 0, 8, map[int64]bool{})

	require.NotEmpty(t, refs7)
	require.Equal(t, len(refs7), len(refs8), "same tree ⇒ same ref count, only the generation prefix differs")

	for name := range refs7 {
		assert.True(t, strings.HasPrefix(name, "s7e"), "gen-7 ref %q must be s7e-prefixed", name)
		_, collides := refs8[name]
		assert.False(t, collides, "gen-7 ref %q must not resolve against the gen-8 map (an older ref can never rebind)", name)
	}
	for name := range refs8 {
		assert.True(t, strings.HasPrefix(name, "s8e"), "gen-8 ref %q must be s8e-prefixed", name)
	}
}

// TestInterestingRoleMintsSelectionAndTabRoles proves the role-vocabulary fix:
// the selection-control roles and tab are ref-minted AND carry their REAL AX
// role as the display role, so the consequence judge's benign recognizers can
// fire on them. Before this, interestingRole folded every control into a generic
// bucket (textbox/button/text), judge.Action.Role could never be radio/checkbox/
// option/tab, and isSelectionToggle/isTabSwitch were dead code — forcing a model
// to click a control's LABEL instead of the control and over-gating the toggle.
func TestInterestingRoleMintsSelectionAndTabRoles(t *testing.T) {
	// selection roles: display role must equal the AX role AND be exactly what
	// judge.IsSelectionRole (the descriptor + isSelectionToggle's set) recognizes.
	for _, role := range []string{"radio", "checkbox", "option", "menuitemradio", "menuitemcheckbox"} {
		disp, ok := interestingRole(role)
		require.True(t, ok, "%s must be an interesting (ref-minted) role", role)
		assert.Equal(t, role, disp, "%s must carry its real AX role, not a folded bucket", role)
		assert.True(t, judge.IsSelectionRole(disp),
			"the display role %q must be one the judge treats as a selection toggle", disp)
	}
	// tab is minted as "tab" so judge.isTabSwitch (which keys on Role=="tab") fires.
	disp, ok := interestingRole("tab")
	require.True(t, ok, "tab must be an interesting (ref-minted) role")
	assert.Equal(t, "tab", disp)
}

// TestRenderLineValueFormatting documents the three render forms on one line.
func TestRenderLineValueFormatting(t *testing.T) {
	assert.Equal(t, `[e1] link "Home"`, renderLine(snapNode{ref: "e1", role: "link", name: "Home"}))
	assert.Equal(t, `[e2] textbox "User" value="alice"`,
		renderLine(snapNode{ref: "e2", role: "textbox", name: "User", value: "alice"}))
	blanked := renderLine(snapNode{ref: "e3", role: "textbox", name: "Pass", value: "ignored", blanked: true})
	assert.Contains(t, blanked, "redacted")
	assert.NotContains(t, blanked, "ignored", "a blanked line never shows the value, even if one is set")
	assert.True(t, strings.HasPrefix(blanked, `[e3] textbox "Pass"`))
}
