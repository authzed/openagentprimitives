package browser

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// redacted; neither secret appears.
	lines, refs := buildSnapshot(nodes, 0, map[int64]bool{200: true})
	out := renderOutline(lines)
	assert.NotContains(t, out, "childsecret", "a blanked node's value-child must be suppressed entirely")
	assert.NotContains(t, out, "ownsecret", "a blanked node's own value must be redacted")
	assert.Contains(t, out, "redacted", "the blanked node must still render, carrying the redaction marker")
	assert.Contains(t, out, "Password", "the element itself (role/name) still appears")

	require.Len(t, lines, 2, "root + the (redacted) input only — the value-child line is gone")
	assert.True(t, lines[1].blanked)
	assert.Empty(t, lines[1].value, "a blanked node carries no value")
	_, hasChildRef := refs["e3"]
	assert.False(t, hasChildRef, "a suppressed descendant gets no ref")

	// Not blanked: everything renders, both values present.
	lines2, _ := buildSnapshot(nodes, 0, map[int64]bool{})
	out2 := renderOutline(lines2)
	assert.Contains(t, out2, "ownsecret")
	assert.Contains(t, out2, "childsecret")
	assert.Len(t, lines2, 3, "root + input + value-child all render when nothing is blanked")
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
