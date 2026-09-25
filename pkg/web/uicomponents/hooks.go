package uicomponents

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// Hook is one oap:generative node found in a view: the agent's writable
// region, with the contract its author gave it.
type Hook struct {
	// Name is what update_view targets; unique per page (enforced by Validate).
	Name string
	// Intent is the author's instruction for this region. Empty is legal —
	// the shim produces it — and the runner logs the absence once per session.
	Intent string
	// AllowedComponents bounds what the AGENT may put here; AllowAll admits
	// the registry. It never bounds the author's own children.
	AllowedComponents []string
	// Step is the timeline step id this hook is bound to; "" when unbound.
	Step string
	// Title is the author's fold header for this hook; "" means use the
	// step's label.
	Title string
	// Path is the index path from the view root to the hook node itself.
	Path []int
}

// Hooks walks the view depth-first and returns every hook in document order.
// It VALIDATES NOTHING — a nameless or duplicate hook is reported as found —
// so Validate can name the fault; callers that need a legal page have one
// only after Validate accepted it.
func Hooks(d Declaration) []Hook {
	if d.View == nil {
		return nil
	}
	var out []Hook
	collectHooks(*d.View, nil, &out)
	return out
}

func collectHooks(n Node, path []int, out *[]Hook) {
	if h, ok := hookOf(n); ok {
		h.Path = append([]int(nil), path...)
		*out = append(*out, h)
	}
	for i, ch := range n.Children {
		collectHooks(ch, append(append([]int(nil), path...), i), out)
	}
}

// hookOf reads a node's GenerativeProps when it is a hook. Props that do not
// decode yield the zero fields — validateProps, not this reader, rejects a
// wrongly-typed prop, and reporting it twice would give Validate two errors
// for one mistake.
func hookOf(n Node) (Hook, bool) {
	if n.Component != GenerativeType {
		return Hook{}, false
	}
	var p GenerativeProps
	if raw, ok := n.Props["name"]; ok {
		_ = json.Unmarshal(raw, &p.Name)
	}
	if raw, ok := n.Props["intent"]; ok {
		_ = json.Unmarshal(raw, &p.Intent)
	}
	if raw, ok := n.Props["allowedComponents"]; ok {
		_ = json.Unmarshal(raw, &p.AllowedComponents)
	}
	if raw, ok := n.Props["step"]; ok {
		_ = json.Unmarshal(raw, &p.Step)
	}
	if raw, ok := n.Props["title"]; ok {
		_ = json.Unmarshal(raw, &p.Title)
	}
	return Hook{Name: p.Name, Intent: p.Intent, AllowedComponents: p.AllowedComponents, Step: p.Step, Title: p.Title}, true
}

// findHook returns the hook named name, if hooks has one. ResolveView calls
// this against Hooks(accepted) — the tree AS IT STANDS after every fragment
// applied so far in the same call — never against a path computed once at
// the start, so a hook an earlier fragment's fill just removed reads as
// absent: the same outcome as a name the page never declared.
func findHook(hooks []Hook, name string) (Hook, bool) {
	for _, h := range hooks {
		if h.Name == name {
			return h, true
		}
	}
	return Hook{}, false
}

// replaceHookChildren returns a copy of root in which the node at path has
// its Children replaced. Nodes ALONG path are cloned; every sibling subtree
// is shared with root — safe because ResolveView never mutates a candidate
// after building it: a rejected candidate is discarded outright, never
// patched in place (TestResolveViewIsPure guards this).
//
// ResolveView re-derives path from root's OWN current hooks before every
// call (findHook against Hooks(accepted)), so path is expected to describe
// root correctly by the time it gets here. The bounds check below is
// defense in depth rather than the primary guard: it turns a wrong index
// into an error instead of a panic on a caller's tree, should that
// expectation ever be violated.
func replaceHookChildren(root Node, path []int, children []Node) (Node, error) {
	if len(path) == 0 {
		root.Children = children
		return root, nil
	}
	i := path[0]
	if i < 0 || i >= len(root.Children) {
		return Node{}, fmt.Errorf("path index %d is out of range (%d children)", i, len(root.Children))
	}
	next := slices.Clone(root.Children)
	replaced, err := replaceHookChildren(next[i], path[1:], children)
	if err != nil {
		return Node{}, err
	}
	next[i] = replaced
	root.Children = next
	return root, nil
}

// checkAllowed walks a fill and returns the first component outside the
// hook's allowlist, at any depth. A structural type is refused even under
// "*": a hook inside a fill would be a region nobody declared.
//
// The refusal reads Component.Structural from the registry — the same flag
// VocabularySchema reads when it decides which types the agent is told about,
// and the same one validateHooks reads when it refuses a structural type in an
// author's allowlist. A structural type registered later is therefore refused
// here by virtue of its registration, with nobody having to remember this
// function; matching on the generative type alone would have left the next one
// admissible under "*".
func checkAllowed(fill Node, h Hook) error {
	if c, ok := registry.Get(fill.Component); ok && c.Structural {
		// oap:generative is structural AND a region, so its refusal names the
		// region the fill tried to open — more use to an agent rewriting the
		// fragment than the type name it already wrote.
		if inner, isHook := hookOf(fill); isHook {
			return fmt.Errorf("a fill may not contain a hook (%q)", inner.Name)
		}
		return fmt.Errorf("a fill may not contain the structural component %q", fill.Component)
	}
	if !slices.Contains(h.AllowedComponents, AllowAll) && !slices.Contains(h.AllowedComponents, fill.Component) {
		return fmt.Errorf("component %q is not allowed in hook %q; allowed: [%s]",
			fill.Component, h.Name, strings.Join(slices.Sorted(slices.Values(h.AllowedComponents)), ", "))
	}
	for _, ch := range fill.Children {
		if err := checkAllowed(ch, h); err != nil {
			return err
		}
	}
	return nil
}
