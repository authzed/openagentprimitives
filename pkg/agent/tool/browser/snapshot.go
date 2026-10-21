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
// the display role, the accessible name, the element's current value (form
// controls only; "" when it has none), whether that value is blanked (spec
// §3.6 — a credential placeholder was typed into it and the page has not
// navigated since), the indentation depth (by rendered ancestors), and the DOM
// backend node the ref resolves to.
type snapNode struct {
	ref           string
	role          string
	name          string
	value         string
	blanked       bool
	depth         int
	backendNodeID int64
}

// snapshot fetches a fresh full accessibility tree and builds the ordered
// interesting-node list plus the per-snapshot ref map. startBackendID scopes the
// walk to that node's subtree (0 = whole document). blanked is the session's
// current blanked-backend-node-ID set (spec §3.6); a node in it (and its whole
// subtree) has its value redacted rather than rendered, by backendNodeID, so
// the redaction survives any re-snapshot until the page genuinely navigates
// (navigate/back/follow clear the set; see Session.clearBlanked). ctx must be
// bound to the chromedp browser context (i.e. called from within Session.do).
//
// The blanked set is NOT pruned by AX-tree presence: Chromium OMITS a
// display:none subtree from the accessibility tree entirely, so a filled field
// hidden by a tab/accordion/"show password" toggle — still in the DOM, still
// carrying its typed value — would look "gone" to the AX tree and, if pruned,
// re-expose its value verbatim when re-shown without retyping. Blanking
// therefore persists until a real navigation, which is strictly fail-closed
// (at worst a field removed mid-document stays over-blanked until the next nav,
// a usability wart, never a leak).
func snapshot(ctx context.Context, startBackendID int64, blanked map[int64]bool) ([]snapNode, map[string]refEntry, error) {
	var nodes []*accessibility.Node
	err := runAction(ctx, func(c context.Context) error {
		var e error
		nodes, e = accessibility.GetFullAXTree().Do(c)
		return e
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading accessibility tree: %w", err)
	}
	lines, refs := buildSnapshot(nodes, startBackendID, blanked)
	return lines, refs, nil
}

// buildSnapshot is the pure half of snapshot: it turns a flat AX node list into
// the rendered-node list and ref map. Refs are assigned in document pre-order
// over interesting, non-ignored nodes. It never panics on a nil node, a missing
// child id, or a detached subtree. A node whose backend ID is in blanked never
// has its value populated, and its whole subtree is suppressed — the redaction
// happens here, once, rather than at render time, so every consumer of the
// returned []snapNode (read's outline, find's matched lines) is blind to it by
// construction.
func buildSnapshot(nodes []*accessibility.Node, startBackendID int64, blanked map[int64]bool) ([]snapNode, map[string]refEntry) {
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

	// ancestorBlanked carries down the recursion: Chrome represents a text
	// input's live rendered content as a CHILD static-text AX node (its own
	// backendNodeID, distinct from the input's), the same way it represents any
	// other visible text on the page — it is not a quirk of form controls
	// specifically, it is how the generic "visible text becomes a text/
	// statictext AX node" rendering behaves for an input's displayed value.
	// That child is the actual vector a typed credential surfaces through: the
	// input's OWN node rarely carries the text in its "value" property, but its
	// child does, in its NAME. So blanking by backendNodeID must suppress the
	// WHOLE subtree under a blanked node, not just that one node's value, or
	// the exact bytes redacted on the parent line reappear one line below it.
	var walk func(n *accessibility.Node, depth int, ancestorBlanked bool)
	walk = func(n *accessibility.Node, depth int, ancestorBlanked bool) {
		if n == nil || seen[n.NodeID] {
			return // nil child id or a cycle: never panic, never loop forever
		}
		seen[n.NodeID] = true

		backendID := int64(n.BackendDOMNodeID)
		selfBlanked := blanked[backendID]
		effectiveBlanked := ancestorBlanked || selfBlanked

		childDepth := depth
		if !n.Ignored {
			if disp, ok := interestingRole(axRole(n)); ok {
				if !ancestorBlanked {
					// Either an ordinary node, or the exact element a placeholder was
					// typed into (selfBlanked) — render it; render it here, but with
					// its value redacted rather than shown when selfBlanked.
					counter++
					ref := "e" + strconv.Itoa(counter)
					name := axName(n)
					var value string
					if !selfBlanked {
						value = axValueString(n.Value)
					}
					lines = append(lines, snapNode{
						ref:           ref,
						role:          disp,
						name:          name,
						value:         value,
						blanked:       selfBlanked,
						depth:         depth,
						backendNodeID: backendID,
					})
					refs[ref] = refEntry{backendNodeID: backendID, role: disp}
				}
				// else: a descendant of a blanked element — e.g. the rendered-value
				// text child described above — never gets a ref or a line. The
				// typed value is redacted on the blanked ancestor's own line; it must
				// never reappear via a descendant's name.
				childDepth = depth + 1
			}
		}
		for _, cid := range n.ChildIDs {
			walk(byID[cid], childDepth, effectiveBlanked)
		}
	}
	for _, r := range roots {
		walk(r, 0, false)
	}
	return lines, refs
}

// renderOutline renders the snapshot as an indented outline, one node per line:
//
//	[e3] link "Book a flight"
//	[e4] textbox "Username" value="alice"
//	[e5] textbox "Password" value=[redacted: filled via a credential placeholder]
func renderOutline(lines []snapNode) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(strings.Repeat("  ", ln.depth))
		b.WriteString(renderLine(ln))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderLine renders one node's "[ref] role "name"" line, appending its value
// when it has one. A blanked node (spec §3.6 — a credential placeholder was
// typed into it and the page has not navigated since) never has its value
// shown: the element itself still appears (ref, role, name), but the value
// is replaced with a fixed redaction marker, used by BOTH read's outline and
// find's matched lines, so neither can reveal what was typed.
func renderLine(ln snapNode) string {
	line := fmt.Sprintf("[%s] %s %q", ln.ref, ln.role, ln.name)
	switch {
	case ln.blanked:
		line += " value=[redacted: filled via a credential placeholder]"
	case ln.value != "":
		line += fmt.Sprintf(" value=%q", ln.value)
	}
	return line
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
