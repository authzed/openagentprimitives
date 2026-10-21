package browser

import (
	"context"
	stdjson "encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/accessibility"
)

// snapNode is one rendered line of an accessibility outline: a per-snapshot ref,
// the display role, the accessible name, the indentation depth (by rendered
// ancestors), and the DOM backend node the ref resolves to.
type snapNode struct {
	ref           string
	role          string
	name          string
	depth         int
	backendNodeID int64
}

// snapshot fetches a fresh full accessibility tree and builds the ordered
// interesting-node list plus the per-snapshot ref map. startBackendID scopes the
// walk to that node's subtree (0 = whole document). ctx must be bound to the
// chromedp browser context (i.e. called from within Session.do).
func snapshot(ctx context.Context, startBackendID int64) ([]snapNode, map[string]refEntry, error) {
	var nodes []*accessibility.Node
	err := runAction(ctx, func(c context.Context) error {
		var e error
		nodes, e = accessibility.GetFullAXTree().Do(c)
		return e
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading accessibility tree: %w", err)
	}
	lines, refs := buildSnapshot(nodes, startBackendID)
	return lines, refs, nil
}

// buildSnapshot is the pure half of snapshot: it turns a flat AX node list into
// the rendered-node list and ref map. Refs are assigned in document pre-order
// over interesting, non-ignored nodes. It never panics on a nil node, a missing
// child id, or a detached subtree.
func buildSnapshot(nodes []*accessibility.Node, startBackendID int64) ([]snapNode, map[string]refEntry) {
	byID := make(map[accessibility.NodeID]*accessibility.Node, len(nodes))
	for _, n := range nodes {
		if n != nil {
			byID[n.NodeID] = n
		}
	}

	var roots []*accessibility.Node
	if startBackendID != 0 {
		for _, n := range nodes {
			if n != nil && int64(n.BackendDOMNodeID) == startBackendID {
				roots = []*accessibility.Node{n}
				break
			}
		}
	}
	if len(roots) == 0 {
		for _, n := range nodes {
			if n != nil && n.ParentID == "" {
				roots = append(roots, n)
			}
		}
	}

	refs := make(map[string]refEntry)
	lines := make([]snapNode, 0, len(nodes))
	seen := make(map[accessibility.NodeID]bool, len(nodes))
	counter := 0

	var walk func(n *accessibility.Node, depth int)
	walk = func(n *accessibility.Node, depth int) {
		if n == nil || seen[n.NodeID] {
			return // nil child id or a cycle: never panic, never loop forever
		}
		seen[n.NodeID] = true

		childDepth := depth
		if !n.Ignored {
			if disp, ok := interestingRole(axRole(n)); ok {
				counter++
				ref := "e" + strconv.Itoa(counter)
				name := axName(n)
				lines = append(lines, snapNode{
					ref:           ref,
					role:          disp,
					name:          name,
					depth:         depth,
					backendNodeID: int64(n.BackendDOMNodeID),
				})
				refs[ref] = refEntry{backendNodeID: int64(n.BackendDOMNodeID), role: disp}
				childDepth = depth + 1
			}
		}
		for _, cid := range n.ChildIDs {
			walk(byID[cid], childDepth)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return lines, refs
}

// renderOutline renders the snapshot as an indented outline, one node per line:
//
//	[e3] link "Book a flight"
func renderOutline(lines []snapNode) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(strings.Repeat("  ", ln.depth))
		fmt.Fprintf(&b, "[%s] %s %q\n", ln.ref, ln.role, ln.name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// interestingRole maps an AX role to its display form, reporting whether the
// role is one the outline renders. Only "interesting" roles surface; everything
// else is traversed for its children but not rendered.
func interestingRole(role string) (string, bool) {
	switch strings.ToLower(role) {
	case "rootwebarea", "webarea", "document":
		return "document", true
	case "heading":
		return "heading", true
	case "link":
		return "link", true
	case "button":
		return "button", true
	case "textbox", "textfield", "searchbox", "combobox":
		return "textbox", true
	case "list":
		return "list", true
	case "listitem":
		return "listitem", true
	case "image", "img":
		return "image", true
	case "row":
		return "row", true
	case "cell", "gridcell", "columnheader", "rowheader":
		return "cell", true
	case "statictext", "text", "paragraph":
		return "text", true
	default:
		return "", false
	}
}

// axRole returns a node's computed role as a plain string ("" if absent).
func axRole(n *accessibility.Node) string {
	if n == nil {
		return ""
	}
	return axValueString(n.Role)
}

// axName returns a node's accessible name as a plain string ("" if absent).
func axName(n *accessibility.Node) string {
	if n == nil {
		return ""
	}
	return axValueString(n.Name)
}

// axValueString decodes an AXValue's JSON-encoded value into a display string.
// The computed value is raw JSON (usually a quoted string); non-string values
// fall back to their trimmed raw form. Returns "" for a nil value.
func axValueString(v *accessibility.Value) string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	raw := []byte(v.Value)
	var s string
	if err := stdjson.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
