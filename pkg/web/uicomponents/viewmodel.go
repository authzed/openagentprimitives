package uicomponents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// ParseNode turns one Tier-1 fragment's JSON into a Node with the SAME
// strictness ParseDeclaration applies to a whole document: unknown wire fields
// are rejected rather than dropped. An agent that writes {"componant":"ap:text"}
// must be told it was wrong, not handed a success for an empty node — which is
// the exact silently-dropped-field failure ParseDeclaration exists to prevent,
// arriving one layer down.
//
// Node.Props stays json.RawMessage here and is checked against the component's
// own props struct by Validate, exactly as a Tier-0 node's are. There is no
// second props checker for Tier 1.
func ParseNode(data []byte) (Node, error) {
	var n Node
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return Node{}, fmt.Errorf("uicomponents: parse fragment node: %w", err)
	}
	return n, nil
}

// Fragment is one stored Tier-1 rewrite: the hook it targets and the content
// that replaces it. It is deliberately NOT a Declaration — a fragment can
// never carry a view of its own, and can never carry actions (see Action's
// doc comment: a control may NAME a declared action and can never mint one).
type Fragment struct {
	// Hook is the hook name this fragment targets (Hook.Name).
	Hook string
	// Node is the replacement content for that hook. nil is a CLEAR: an
	// intentional empty the agent chose, distinct from a hook nothing has
	// ever touched (see AgentComposed).
	Node *Node
}

// Rejection is one fragment that could not be applied, with the reason a log
// and a chrome notice are both built from.
type Rejection struct {
	// Hook is always populated (the fragment's target hook name), so a
	// rejection can always be attributed.
	Hook string `json:"hook"`
	// Reason is the validator's own text, never a generic "invalid".
	Reason string `json:"reason"`
}

// View is the result of merging stored fragments onto an author's Tier-0
// declaration.
type View struct {
	// Declaration is what the browser renders and what every server-side
	// lookup (a binding path, an action name) resolves against. It always
	// validates: a fragment that would have made it invalid is in Rejected
	// instead, and that hook kept its prior content.
	Declaration Declaration

	// AgentComposed lists, sorted, every hook a fragment successfully
	// changed — a fill OR a clear. It is the ONLY source of the per-hook
	// marker, and it is computed here rather than stored, so no
	// agent-written record can assert or clear its own marker.
	AgentComposed []string

	// Rejected lists every fragment that was dropped, in hook order. It is
	// never silently empty: a caller MUST log it (AGENTS.md's
	// no-silent-errors rule) — a dropped fragment renders as "the UI didn't
	// update", which reads as a slow agent rather than a bug.
	Rejected []Rejection

	// ComposedAt is when each hook in AgentComposed was last written — the
	// WrittenAt of the fill or clear that stands. Filled by the reader that
	// holds the stored records (pkg/web/uiview.Resolve); ResolveView itself
	// never sees a time. A hook absent here shows its author default.
	ComposedAt map[string]time.Time
}

// ResolveView merges fragments onto base and returns a declaration that is
// guaranteed valid under o.
//
// It is THE merge. The runner calls it to decide whether an update_view write
// is legal; webd calls it on every read (bootstrap, bindings lookup, action
// lookup, live push). One function, so the two can never disagree about what
// the viewer is looking at.
//
// Why every READ re-validates rather than trusting the write:
//
//   - Action.Inputs must stay DISJOINT from ParamKeys, enforced statically by
//     validateActions over the declaration AS AUTHORED. A hook fill can
//     introduce a parameter name, so that static result goes stale the moment
//     a fragment lands. Re-running the MERGED declaration through Validate is
//     what keeps it true — there is no second copy of the rule.
//   - The Tier-0 declaration and the tool grant both move underneath a stored
//     fragment: a redeploy can remove a hook, remove the action a fragment
//     names, add a colliding input, or narrow GrantedTools. Legal when written
//     is not legal now.
//
// Fallback is PER HOOK and deterministic: fragments apply in the view's
// document order (Hooks(base)'s order), each onto the result accepted so
// far, and one failing Validate is dropped — that hook keeps its prior
// content while siblings stand. Whole-page rejection would blank every good
// fragment for one bad one, and "apply all, validate once" cannot say WHICH
// was at fault.
//
// A fragment naming a hook the page does not declare is rejected WITHOUT
// being applied. There is no separate "not writable" outcome to check: every
// hook is writable by definition — a region the author did not mark up as
// oap:generative was never compiled into a hook at all, so it carries no
// name a fragment could target.
//
// A fragment's target hook is re-derived from the tree ACCEPTED so far
// (findHook against Hooks(accepted)) on every iteration, never from a path
// computed once before the loop starts: an earlier-accepted fill can replace
// a subtree a later hook sat inside, and that hook is then simply gone from
// the page — the same "unknown hook" outcome as a name the page never
// declared, never a stale index read into whatever now occupies that spot.
//
// A fill (f.Node != nil) is checked against its hook's allowlist at EVERY
// depth, not just its root — an allowed component wrapped in a disallowed
// one is still disallowed — and may never contain a hook of its own, even
// under an allowlist of AllowAll: a fill minting a fresh oap:generative node
// would be a writable region the author never declared and Validate never
// saw. f.Node == nil is a CLEAR: it replaces the hook's current children
// with nothing, an intentional empty distinct from a hook no fragment has
// touched.
//
// base.Actions passes through untouched; Validate populates o.DeclaredActions
// itself.
func ResolveView(base Declaration, fragments []Fragment, o Options) View {
	// hooks is base's hook table in document order; order indexes each
	// hook's position in it and is used ONLY to place fragments
	// deterministically — never to locate the node a fragment targets (see
	// findHook below for why the node itself is re-derived per fragment).
	hooks := Hooks(base)
	order := make(map[string]int, len(hooks))
	for i, h := range hooks {
		order[h.Name] = i
	}

	// Order fragments by their hook's document position; a fragment naming an
	// unknown hook sorts last, so it neither disturbs a known fragment's
	// position nor makes the rejection order depend on input order. Ties (a
	// caller-supplied duplicate hook name) keep their relative input order,
	// which slices.SortStableFunc guarantees.
	ordered := slices.Clone(fragments)
	slices.SortStableFunc(ordered, func(a, b Fragment) int {
		ia, aok := order[a.Hook]
		ib, bok := order[b.Hook]
		switch {
		case aok && bok:
			return ia - ib
		case aok && !bok:
			return -1
		case !aok && bok:
			return 1
		default:
			return 0
		}
	})

	// accepted is the declaration built up so far. ResolveView never mutates
	// base or a prior candidate's tree — see replaceHookChildren.
	accepted := base

	var composed []string
	var rejected []Rejection

	for _, f := range ordered {
		// Re-derive the hook from the ACCEPTED tree, never from base's
		// recorded path: see the doc comment above.
		h, ok := findHook(Hooks(accepted), f.Hook)
		if !ok {
			rejected = append(rejected, Rejection{Hook: f.Hook, Reason: fmt.Sprintf("unknown hook %q", f.Hook)})
			continue
		}

		// children is the hook's new content: the fill wrapped as its sole
		// child, or nil for an intentional clear.
		var children []Node
		if f.Node != nil {
			if err := checkAllowed(*f.Node, h); err != nil {
				rejected = append(rejected, Rejection{Hook: f.Hook, Reason: err.Error()})
				continue
			}
			children = []Node{*f.Node}
		}

		// candidate is accepted-so-far with ONLY this hook's children
		// swapped — a fresh view per attempt, so a rejected candidate never
		// leaks its swap into the next fragment's starting point.
		candidate := accepted
		newView, err := replaceHookChildren(*accepted.View, h.Path, children)
		if err != nil {
			// h.Path was just computed from Hooks(accepted) above, so this
			// is expected to be unreachable — kept as a fail-closed guard
			// rather than trusting the computed path blindly.
			rejected = append(rejected, Rejection{Hook: f.Hook, Reason: fmt.Sprintf("hook %q: %s", f.Hook, err.Error())})
			continue
		}
		candidate.View = &newView

		if err := Validate(candidate, o); err != nil {
			// *ValidationError.Error() already renders "<path>: <reason>", which
			// is exactly what an agent correcting a rejected fragment and an
			// operator reading a log both need — where, then why.
			rejected = append(rejected, Rejection{Hook: f.Hook, Reason: err.Error()})
			continue
		}

		accepted = candidate
		composed = append(composed, f.Hook)
	}

	// Sorted for a stable wire, then deduplicated: a caller that passed two
	// fragments naming the same hook has both accepted (the second simply
	// overwrites the first's children), and AgentComposed answers "did the
	// agent write this region", a question one name can only answer once. The
	// browser reads it as a set and the tool result reads it as a list, so a
	// repeat would show up as nothing at all in one and as noise in the other.
	// uiview.apply already collapses a repeat before it gets here; this is the
	// same guarantee for every other caller.
	slices.Sort(composed)
	composed = slices.Compact(composed)

	return View{
		Declaration:   accepted,
		AgentComposed: composed,
		Rejected:      rejected,
	}
}
