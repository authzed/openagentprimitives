package uicomponents

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// BoundProp is one resolved location of a binding inside a declaration.
type BoundProp struct {
	// Path is BindingPath's output — the key the browser looks a result up by.
	Path string
	// Region is the nearest enclosing hook's name this binding was found
	// under, or "" for a binding outside any hook.
	Region string
	// Prop is the bound prop name on the node.
	Prop string
	// Binding is the declaration's own binding, verbatim.
	Binding Binding
}

// BindingPath is the stable key identifying one bound prop.
// Format: "<region>/<dotted node index path>#<prop>" — the region is the
// nearest enclosing hook's name, or "" outside any hook, and the index path is
// measured from THAT region's root (the hook node, or the view root when the
// region ""). Inside a hook the numbering starts at the hook's own children, so
// a slot default compiled by the shim is "root/0#rows" and a node two levels
// under it is "root/0.2#rows". An EMPTY index path names the REGION'S ROOT, and
// only the "" region ever produces one ("/#rows", a bound view root): a hook
// node itself declares no bindable prop.
// web/packages/agentui/src/bindings.ts mirrors it exactly.
//
// The mirror is pinned by ONE SHARED ARTIFACT: a golden file both suites read
// — the Go test asserts real WalkBindings output against it, and the frontend
// suite re-derives the same list through bindingPath. A literal in each
// language's own test would NOT be a pin: a coordinated change sweeping both
// leaves both suites green while every response key misses the browser's
// lookup, every bound prop falls back to its placeholder, and the page stays
// that way forever on a clean 200.
func BindingPath(region string, nodePath []int, prop string) string {
	var b strings.Builder
	b.WriteString(region)
	b.WriteByte('/')
	for i, n := range nodePath {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	b.WriteByte('#')
	b.WriteString(prop)
	return b.String()
}

// regionCursor is the one place the binding-path REGION rule lives: a node's
// region is the nearest enclosing hook's name, or "" outside any hook, and
// its index path is measured from that region's root — the hook node, or the
// view root. WalkBindings and ActionRefs both step through it, so a path the
// bindings route answers for is a path the actions route names.
type regionCursor struct {
	region string
	path   []int
}

// enter returns the cursor for child i of the node c currently describes:
// if that child is a hook, a NEW region rooted at it (empty path); otherwise
// the same region one level deeper.
func (c regionCursor) enter(child Node, i int) regionCursor {
	if h, ok := hookOf(child); ok {
		return regionCursor{region: h.Name}
	}
	return regionCursor{region: c.region, path: append(append([]int(nil), c.path...), i)}
}

// rootCursor seeds the walk from the view's own root, applying the SAME
// classification enter applies to every other node: if the root itself is a
// hook — a legal authored page, {"view":{"component":"oap:generative",…}} —
// everything under it, including a binding or action ref on the root node
// itself, is IN that hook's region. Without this, the root would be a silent
// exception to the region rule: a node reached as some ancestor's CHILD gets
// classified by enter, but the walk's very first node never goes through
// enter at all, since it has no parent to enter it from.
func rootCursor(n Node) regionCursor {
	if h, ok := hookOf(n); ok {
		return regionCursor{region: h.Name}
	}
	return regionCursor{}
}

// WalkBindings returns every binding in the declaration in a DETERMINISTIC
// order: nodes depth-first in document order, and — because Node.Bindings is
// a Go map whose iteration order is randomized — props sorted by name within
// a node.
//
// Determinism is load-bearing rather than tidy: the browser looks a result up
// by BindingPath, and a walk whose order depended on Go map iteration would
// still produce correct keys but would make every failure non-reproducible.
func WalkBindings(d Declaration) []BoundProp {
	if d.View == nil {
		return nil
	}
	var out []BoundProp
	walkNodeBindings(*d.View, rootCursor(*d.View), &out)
	return out
}

func walkNodeBindings(n Node, c regionCursor, out *[]BoundProp) {
	for _, prop := range slices.Sorted(maps.Keys(n.Bindings)) {
		*out = append(*out, BoundProp{
			Path:    BindingPath(c.region, c.path, prop),
			Region:  c.region,
			Prop:    prop,
			Binding: n.Bindings[prop],
		})
	}
	for i, ch := range n.Children {
		walkNodeBindings(ch, c.enter(ch, i), out)
	}
}

// ParamNames returns the sorted, deduplicated set of binding-parameter names
// the declaration's controls declare, resolved through each component's
// registered ParamProp. A node whose component declares no ParamProp, or
// whose param prop is absent/empty/not a string, contributes nothing.
//
// Resolving through the registry rather than switching on Node.Component
// means a future control that drives a parameter is a registration change,
// not an edit here.
func ParamNames(d Declaration) []string {
	seen := map[string]struct{}{}
	if d.View != nil {
		collectParams(*d.View, seen)
	}
	return slices.Sorted(maps.Keys(seen))
}

// ParamKeys returns the sorted, deduplicated set of runtime parameter KEYS
// the declaration's controls drive — what a browser's parameter map may
// legally contain, and the only names an args template's {"$param":"…"} can
// ever be satisfied by.
//
// This is NOT ParamNames. A declared name is not always a key: an
// ap:daterange declaring "window" drives "window.from" and "window.to" and
// never "window" itself. The expansion comes from each component's registered
// ParamValues, so the compound-name convention lives with the control that
// invented it instead of being re-derived as a prefix rule at every consumer.
func ParamKeys(d Declaration) []string {
	seen := collectDeclarationParamStates(d)
	return slices.Sorted(maps.Keys(seen))
}

// ParamState is one control's current binding-parameter key and the value the
// declaration currently gives it.
type ParamState struct {
	// Key is the RUNTIME key — "span" for an ap:select declaring "span",
	// "window.from"/"window.to" for an ap:daterange declaring "window". Never
	// the declared NAME where the two differ; a name that is not a key can
	// never be satisfied by any viewer action, which is why ParamKeys exists.
	Key string `json:"key"`

	// Value is the declared default for that key, read from the component's
	// registered ParamValues[i].ValueProp. Empty when the declaration sets
	// none.
	Value string `json:"value"`
}

// ParamStates returns every runtime parameter key the declaration's controls
// drive, sorted by Key, with the value the declaration currently gives it.
//
// The Go half of a mirror: the browser's collectDefaultParams seeds its
// parameter map from the same declaration by the same rule. UNPINNED, unlike
// WalkBindings/BindingPath — no shared golden covers it, because nothing
// consumes ParamStates from both languages against one fixture yet. Whoever
// wires that consumer MUST add one rather than trust two independently-written
// literals.
//
// SERVER-SIDE declaration state, not viewer state: once a viewer moves a
// selector, their choice lives in the browser's parameter map and is sent per
// request, never persisted.
func ParamStates(d Declaration) []ParamState {
	seen := collectDeclarationParamStates(d)
	out := make([]ParamState, 0, len(seen))
	for k, v := range seen {
		out = append(out, ParamState{Key: k, Value: v})
	}
	slices.SortFunc(out, func(a, b ParamState) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// collectDeclarationParamStates is the ONE walk behind both ParamKeys and
// ParamStates — key set and key+value are the same traversal answering two
// questions about its result, not two separate tree walks.
func collectDeclarationParamStates(d Declaration) map[string]string {
	seen := map[string]string{}
	if d.View != nil {
		collectParamKeys(*d.View, seen)
	}
	return seen
}

// collectParamKeys walks one subtree, recording every runtime parameter key a
// control drives together with its declared default value.
func collectParamKeys(n Node, seen map[string]string) {
	if name, c, ok := declaredParam(n); ok {
		for _, v := range c.ParamValues {
			seen[v.ParamKey(name)] = paramValueString(n, v)
		}
	}
	for _, ch := range n.Children {
		collectParamKeys(ch, seen)
	}
}

// paramValueString reads one ParamValue's declared default from the node's
// own props, tolerantly: a ValueProp that is absent, or present but not a
// JSON string, contributes an empty value rather than an error. A real type
// mismatch on that prop is validateProps' job, not this walk's.
func paramValueString(n Node, v ParamValue) string {
	if v.ValueProp == "" {
		return ""
	}
	raw, present := n.Props[v.ValueProp]
	if !present {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func collectParams(n Node, seen map[string]struct{}) {
	if name, _, ok := declaredParam(n); ok {
		seen[name] = struct{}{}
	}
	for _, ch := range n.Children {
		collectParams(ch, seen)
	}
}

// declaredParam resolves the parameter name a node declares through the
// registry — never by switching on Node.Component. ok is false for a node
// whose component drives no parameter, or whose param prop is
// absent/empty/not a string.
func declaredParam(n Node) (string, Component, bool) {
	c, ok := registry.Get(n.Component)
	if !ok || c.ParamProp == "" {
		return "", Component{}, false
	}
	raw, present := n.Props[c.ParamProp]
	if !present {
		return "", Component{}, false
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return "", Component{}, false
	}
	return name, c, true
}

// ActionRef is one node's reference to a declared action.
type ActionRef struct {
	// Region is the nearest enclosing hook's name this reference was found
	// under, or "" for a reference outside any hook.
	Region string
	// Path is BindingPath's region/index prefix, for error messages.
	Path string
	// Action is the declared action name the control names.
	Action string

	// Inputs are the value names THIS control supplies when it fires — an
	// ap:form's field names. Empty for a control that supplies none. Resolved
	// through the component's registered InputNamesProp, so a new control that
	// supplies values is a registration change rather than an edit here.
	Inputs []string
}

// ActionRefs returns every action a declaration's controls name, in the same
// DETERMINISTIC order WalkBindings uses, resolved through each component's
// registered ActionProp — so a future control that fires an action is a
// registration change, not an edit here. A node whose component declares no
// ActionProp, or whose action prop is absent/empty/not a string, contributes
// nothing.
func ActionRefs(d Declaration) []ActionRef {
	if d.View == nil {
		return nil
	}
	var out []ActionRef
	collectActionRefs(*d.View, rootCursor(*d.View), &out)
	return out
}

func collectActionRefs(n Node, c regionCursor, out *[]ActionRef) {
	if name, comp, ok := declaredAction(n); ok {
		*out = append(*out, ActionRef{
			Region: c.region,
			Path:   actionPath(c.region, c.path),
			Action: name,
			Inputs: declaredInputNames(n, comp),
		})
	}
	// A list of actions on one node (ap:notice's buttons) contributes one ref
	// per entry, all at the node's own path: there is one node to point at
	// when an entry names an undeclared action, and the entry's index is in
	// the reason the value check (checkNotice) already produces.
	for _, name := range declaredActionList(n) {
		*out = append(*out, ActionRef{Region: c.region, Path: actionPath(c.region, c.path), Action: name})
	}
	for i, ch := range n.Children {
		collectActionRefs(ch, c.enter(ch, i), out)
	}
}

// declaredActionList returns the action names a node's ActionListProp
// carries, in order, skipping entries whose "action" is absent, empty, or not
// a string. See Component.ActionListProp for the array-of-objects contract.
//
// A malformed prop yields no names rather than an error, for the same reason
// declaredInputNames does: the props are separately validated against the
// component's own struct and Check, which is where the wrong shape is
// reported, once.
func declaredActionList(n Node) []string {
	c, ok := registry.Get(n.Component)
	if !ok || c.ActionListProp == "" {
		return nil
	}
	raw, present := n.Props[c.ActionListProp]
	if !present {
		return nil
	}
	var entries []struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Action != "" {
			out = append(out, e.Action)
		}
	}
	return out
}

// declaredAction resolves the action name a node names through the registry
// — never by switching on Node.Component. ok is false for a node whose
// component fires no action, or whose action prop is absent/empty/not a
// string. The Component is returned alongside so the caller can read the
// same registration's other action-related props without a second lookup.
func declaredAction(n Node) (string, Component, bool) {
	c, ok := registry.Get(n.Component)
	if !ok || c.ActionProp == "" {
		return "", Component{}, false
	}
	raw, present := n.Props[c.ActionProp]
	if !present {
		return "", Component{}, false
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return "", Component{}, false
	}
	return name, c, true
}

// declaredInputNames returns the value names a control supplies when it
// fires, read from the prop its registration named in InputNamesProp. See
// Component.InputNamesProp for the array-of-objects-with-"name" contract.
//
// A malformed or absent prop yields no names rather than an error: the props
// are separately validated against the component's own struct, which is where
// a wrongly-shaped `fields` is rejected with a message about its shape.
// Reporting it again here would produce two errors for one mistake, and
// Validate returns only the first.
func declaredInputNames(n Node, c Component) []string {
	if c.InputNamesProp == "" {
		return nil
	}
	raw, present := n.Props[c.InputNamesProp]
	if !present {
		return nil
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// actionPath renders the same "<region>/<dotted node index path>" prefix
// BindingPath uses before its "#<prop>" suffix. An ActionRef has no prop of
// its own to append — the action name IS the value, not a bound prop — so
// this stops short of BindingPath's trailing "#<prop>".
func actionPath(region string, nodePath []int) string {
	var b strings.Builder
	b.WriteString(region)
	b.WriteByte('/')
	for i, n := range nodePath {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}
