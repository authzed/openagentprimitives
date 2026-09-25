// answered.go computes which composed hooks the viewer has already replied
// to, so a page reload does not re-offer an ap:question the viewer already
// answered (or moved on from). "Answered" is a DERIVED fact, not a stored
// one: nothing here writes on the viewer's behalf, and nothing an agent
// fragment writes can forge or clear it. See answeredHooks' own doc comment.
package agentui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// answeredHooks derives which composed hooks the viewer has already replied
// to: a hook whose standing fill was written BEFORE the viewer's latest
// visible message. It is a derivation over two facts that already exist —
// the fill's WrittenAt (uiview.Resolve fills View.ComposedAt) and the
// transcript — so nothing is stored, nothing an agent fragment could forge,
// and a repaint (a newer fill) makes the region fresh again on its own. A
// cleared hook has nothing to answer and is never listed. Computed for the
// GET bootstrap and for every pushed update alike — see answeredHooksFor,
// declarationWireFor's callers' shared path to this.
func answeredHooks(ctx context.Context, mem memory.Memory, scope memory.Scope, v uicomponents.View) ([]string, error) {
	turns, err := turn.ReadAll(ctx, mem, scope)
	if err != nil {
		return nil, fmt.Errorf("agentui: answered: read transcript: %w", err)
	}
	var lastUser time.Time
	for _, m := range turn.VisibleMessages(turns) {
		if m.Role == turn.VisibleRoleUser && m.CreatedAt.After(lastUser) {
			lastUser = m.CreatedAt
		}
	}
	if lastUser.IsZero() {
		return nil, nil
	}
	var out []string
	for _, h := range v.AgentComposed {
		at, ok := v.ComposedAt[h]
		if !ok || !at.Before(lastUser) {
			continue
		}
		// A hook found in a real declaration with zero children is a CLEAR
		// (loadFragments/ResolveView give a fill exactly one child, its fill
		// node, and a clear none) — nothing to answer, so it is excluded. A
		// nil node — no Declaration.View at all, or the hook not found in
		// it — is left INCLUDED rather than excluded: this function has no
		// way to tell "cleared" from "cannot tell", and defaulting to
		// exclude would be a silent false negative, not a safe fallback.
		if node := findHookNode(v.Declaration, h); node != nil && len(node.Children) == 0 {
			continue
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}

// findHookNode returns the hook node named name in d's tree, or nil when d
// has no View at all or declares no such hook. Named distinctly from this
// package's OWN test-only hookNode helper (viewmodel_test.go), which takes a
// *testing.T and fails the test rather than returning nil — a shape this
// production code cannot use. uicomponents.Hook carries only Path — the
// index route from the view root to the hook node — never the node itself,
// so this walks d.View along it; the walk mirrors uicomponents' own
// unexported replaceHookChildren, read-only.
func findHookNode(d uicomponents.Declaration, name string) *uicomponents.Node {
	if d.View == nil {
		return nil
	}
	for _, h := range uicomponents.Hooks(d) {
		if h.Name != name {
			continue
		}
		n := d.View
		for _, i := range h.Path {
			if i < 0 || i >= len(n.Children) {
				return nil
			}
			n = &n.Children[i]
		}
		return n
	}
	return nil
}

// answeredHooksFor is the shared tail declarationWireFor's two callers
// (ViewFor's GET bootstrap and buildLiveViewMessage's push) both go through,
// so the two producers can never disagree about which hooks the viewer has
// already answered.
//
// Best-effort by design, the same posture agentParamsFor takes just below it
// in viewmodel.go: a transcript read failure must not blank a page over a
// purely cosmetic derived cue, so the failure is LOGGED and the caller
// proceeds with no hook marked answered — every question simply shows,
// exactly as it did before this feature existed.
//
// `ui` is carried for the log alone — the derivation itself is per-SESSION and
// reads nothing keyed by UI name. It is there because ns/name locate the
// session but not which of its pages lost its answered cues, and the sibling
// agentParamsFor logs the same three.
func answeredHooksFor(ctx context.Context, d Deps, ns, name, ui string, v uicomponents.View) []string {
	mem := d.Memory()
	if mem == nil {
		return nil
	}
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	answered, err := answeredHooks(ctx, mem, scope, v)
	if err != nil {
		d.Logger().Info("agentui: could not compute answered hooks; showing every question as unanswered",
			"ns", ns, "name", name, "ui", ui, "err", err.Error())
		return nil
	}
	return answered
}
