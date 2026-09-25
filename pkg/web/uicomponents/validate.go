package uicomponents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
)

// ValidationError is a structured rejection: which node was wrong, and why.
// It is deliberately machine-shaped rather than a formatted string, because
// the agent receives it as a tool error and is expected to correct from it.
type ValidationError struct {
	// Path locates the offending node, e.g. "view.children[0].children[2]".
	Path string
	// Reason is a single, self-contained explanation.
	Reason string
	// Err is the underlying error this rejection wraps — a uiselect sentinel
	// from a malformed selector, today — so a caller can classify with
	// errors.Is instead of matching Reason's text. nil for every rejection
	// built here from a format string.
	Err error
}

func (e *ValidationError) Error() string { return e.Path + ": " + e.Reason }

// Unwrap exposes Err so errors.Is/As can reach the underlying sentinel
// through a *ValidationError. nil is the ordinary case.
func (e *ValidationError) Unwrap() error { return e.Err }

// Options bounds a validation pass.
type Options struct {
	// GrantedTools is the materialized set of tools this UI may bind to. A nil
	// map means NO tool binding is permitted — fail closed, so a caller that
	// forgets to populate it cannot accidentally authorize everything.
	GrantedTools map[string]bool

	// ReadonlyTools is the subset of GrantedTools a DATA binding may name:
	// tools that could satisfy the readonly auto-run predicate for SOME
	// arguments (a declared StateImpact of Readonly, on the static fallback or
	// some PermissionVariant, AND the origin's readOnlyHint). A CEILING, not a
	// guarantee any given call auto-runs — the real predicate resolves against
	// the arguments a viewer's page sends, which validation time has none of.
	// Every Node.Bindings entry is a data binding by construction (a mutation
	// is an ACTION binding, a different field), so anything outside this set
	// is rejected.
	//
	// Fail-closed on nil, like GrantedTools: indexing a nil map returns false,
	// so forgetting to populate it authorizes ZERO tool bindings. Do NOT add a
	// `!= nil` guard — that branch needs its own deny arm to stay closed, and
	// a forgotten one inverts the default.
	//
	// Defense in depth, NOT the gate. A caller with no session and therefore
	// no tool origins (the AgentUI reconciler) passes the same ceiling it
	// passes for GrantedTools, making this check vacuous there. The real,
	// unconditional gate is runner.handleAppToolCallReq, re-evaluated against
	// the live tool on every call.
	ReadonlyTools map[string]bool

	// NormalizeToolName maps a binding's Ref into the vocabulary GrantedTools
	// and ReadonlyTools are keyed by, before either lookup. A func rather than
	// a direct import because this package is CRD-adjacent (the AgentUI
	// reconciler validates through it) and must not pull the agent tool
	// packages in behind it; callers that can import synthesize set it to
	// synthesize.NormalizeName.
	//
	// nil is the identity — safe in the REJECT direction, since an
	// un-normalized ref simply misses a normalized grant, but not
	// forward-compatible, so every production caller sets it.
	NormalizeToolName func(string) string

	// DeclaredActions is the action-name set of the declaration UNDER
	// VALIDATION. Validate populates it from that declaration's own Actions
	// before validating any node, OVERWRITING whatever a caller supplied: the
	// only correct set is this declaration's own table, and any other source
	// could drift from the one validateActions just checked.
	//
	// It exists so validateBindings — which takes no Declaration — can resolve
	// an `action:` binding's Ref without widening its signature.
	DeclaredActions map[string]bool

	// MaxDepth and MaxNodes bound the TREE — how many nodes exist and how
	// deeply they nest — so a looping or adversarial agent cannot make the
	// recursive walk unbounded, nor drive the renderer into unbounded React
	// reconciliation depth.
	//
	// They bound NOTHING about the DATA inside a node: one ap:table with
	// 50,000 rows is a single node, validates clean, and renders every row.
	// There is deliberately no payload byte ceiling here — props are opaque
	// json.RawMessage to Validate, and a reasonable size is a per-component
	// question, not a per-tree one. A caller needing one imposes it before or
	// after Validate (a transport body limit, a per-tool output cap).
	MaxDepth int
	MaxNodes int
}

// DefaultOptions returns the standard bounds with NO tools granted. Callers
// that permit tool bindings must set GrantedTools explicitly.
func DefaultOptions() Options {
	return Options{MaxDepth: 32, MaxNodes: 512}
}

// Validate checks a declaration against the registered vocabulary and the
// supplied bounds, returning the FIRST failure as a *ValidationError. First-
// failure (rather than accumulate-all) is deliberate: the agent corrects one
// thing at a time, and a cascade of downstream errors from one bad node is
// noise, not signal.
func Validate(d Declaration, o Options) error {
	// The action table is validated FIRST, and unconditionally before any
	// node — so a malformed table (an ungranted tool, a duplicate name)
	// produces ONE error naming the table, not a cascade of "undeclared
	// action" errors from every control that referenced it.
	if err := validateActions(d, o); err != nil {
		return err
	}
	// DeclaredActions is derived from d.Actions ONLY here, after the table
	// above has already been validated — never from whatever the caller
	// passed in Options, so it cannot drift from the table validateActions
	// just checked.
	o.DeclaredActions = make(map[string]bool, len(d.Actions))
	for _, a := range d.Actions {
		o.DeclaredActions[a.Name] = true
	}

	// The set of parameter keys the declaration's own controls drive is a
	// WHOLE-DECLARATION fact — a control in one region legitimately drives a
	// binding in another — so it is computed once here and threaded down,
	// rather than re-derived per node.
	paramKeys := ParamKeys(d)
	declaredKeys := make(map[string]struct{}, len(paramKeys))
	for _, k := range paramKeys {
		declaredKeys[k] = struct{}{}
	}

	// A declaration reaching Validate must already be normalized: exactly one
	// of these is the caller's mistake (Slots survived a hand-built
	// Declaration instead of going through ParseDeclaration/Normalize), and
	// the other is a document with nothing to render at all — never silently
	// treated as an empty page here, since Normalize already makes that
	// distinction for every real entry point.
	if len(d.Slots) > 0 {
		return &ValidationError{Path: "slots", Reason: "declaration was not normalized: call ParseDeclaration or Normalize before Validate"}
	}
	if d.View == nil {
		return &ValidationError{Path: "view", Reason: "missing view"}
	}

	// Hooks are validated as a TABLE, same reasoning as validateActions above:
	// a bad hook contract (a duplicate name, a bogus allowlist entry) should
	// name that one hook, not cascade into every node the ordinary walk below
	// would otherwise visit.
	if err := validateHooks(d); err != nil {
		return err
	}
	// oap:page's one placement rule is checked here, before the ordinary node
	// walk: validateNode has no notion of "structural" and would happily
	// accept a nested oap:page as an ordinary child, so this is the only place
	// a misplaced page is named as such.
	if err := validatePageRoot(d); err != nil {
		return err
	}

	count := 0
	if err := validateNode(*d.View, "view", 1, &count, o, declaredKeys); err != nil {
		return err
	}
	// After the node walk: every ap:steps has passed checkSteps by now, so
	// the ids read here are labels and unique, and every hook's props have
	// decoded.
	return validateStepBindings(d)
}

// MaxHookIntentLen bounds a hook's intent, in runes.
//
// An intent is not a comment: meta.HookLine renders it into the "Your page"
// prompt section AND into update_view's `hook` property description, so every
// hook's intent lands in the system prompt TWICE on every request, for the
// life of every session on the class — and read_view prints them all again on
// each call. Nothing else bounds it: spec.view is opaque JSON, so no CRD
// marker can reach a hook's props, and the hook count itself is bounded only
// by MaxNodes.
//
// The limit is generous but real, and the precedent is AgentUIAction.Prompt's
// MaxLength=2000: a long instruction belongs in the class prompt, where
// standing instructions live, not repeated per turn beside a region.
const MaxHookIntentLen = 1000

// validateHooks enforces the author's contract for every hook on the page:
// a name (unique), an intent within MaxHookIntentLen, an allowlist (present,
// every entry registered or "*", never a structural type), and no hook inside
// another hook.
//
// It deliberately does NOT check a hook's own children against its
// allowlist. The allowlist bounds what the AGENT may write into the region
// (ResolveView applies it to every fill); the author's default is bounded by
// the registry alone, through the ordinary node walk. An author may give a
// region a default the agent cannot reproduce — the agent can still replace
// or clear it — and rejecting that would make the allowlist a constraint on
// the person it exists to protect.
func validateHooks(d Declaration) error {
	seen := map[string]struct{}{}
	return walkHooks(*d.View, "view", "", func(h Hook, path string) error {
		if h.Name == "" {
			return &ValidationError{Path: path, Reason: "missing hook name"}
		}
		if _, dup := seen[h.Name]; dup {
			return &ValidationError{Path: path, Reason: fmt.Sprintf("duplicate hook name %q", h.Name)}
		}
		seen[h.Name] = struct{}{}
		if n := utf8.RuneCountInString(h.Intent); n > MaxHookIntentLen {
			return &ValidationError{Path: path, Reason: fmt.Sprintf(
				"hook %q: intent is %d characters; the limit is %d — a long instruction belongs in the class prompt",
				h.Name, n, MaxHookIntentLen)}
		}
		if len(h.AllowedComponents) == 0 {
			return &ValidationError{Path: path, Reason: fmt.Sprintf("hook %q declares no allowedComponents; use [\"*\"] for the whole vocabulary", h.Name)}
		}
		for _, allowed := range h.AllowedComponents {
			if allowed == AllowAll {
				continue
			}
			c, ok := registry.Get(allowed)
			if !ok {
				return &ValidationError{Path: path, Reason: fmt.Sprintf("hook %q allows unknown component type %q", h.Name, allowed)}
			}
			if c.Structural {
				return &ValidationError{Path: path, Reason: fmt.Sprintf("hook %q allows a structural type %q; a fill can never contain one", h.Name, allowed)}
			}
		}
		return nil
	})
}

// walkHooks visits every hook depth-first with its JSON-ish path, refusing a
// hook whose ancestor is a hook. enclosing is the ancestor hook's name, or
// "" at the top; the nesting check happens here rather than in the visitor
// so no caller can forget it.
//
// This is a SECOND traversal, distinct from hooks.go's collectHooks, rather
// than collectHooks reused: collectHooks carries an []int index path (what
// update_view addresses a hook by) and does not stop on error, while this
// walk carries a "view.children[…]"-shaped string (what a *ValidationError
// reports) and an enclosing-hook name, and must short-circuit on the first
// failure. Bending collectHooks's Path type and adding an error return and
// an ancestor parameter to serve a validation-only concern would contort a
// reader used by Hooks() for an unrelated purpose (surfacing hooks to the
// runner, not rejecting a document).
func walkHooks(n Node, path, enclosing string, visit func(Hook, string) error) error {
	if h, ok := hookOf(n); ok {
		if enclosing != "" {
			return &ValidationError{Path: path, Reason: fmt.Sprintf("hook %q is nested inside hook %q; hooks cannot nest", h.Name, enclosing)}
		}
		if err := visit(h, path); err != nil {
			return err
		}
		enclosing = h.Name
	}
	for i, ch := range n.Children {
		if err := walkHooks(ch, fmt.Sprintf("%s.children[%d]", path, i), enclosing, visit); err != nil {
			return err
		}
	}
	return nil
}

// validateStepBindings checks every hook bound to a timeline step
// (GenerativeProps.Step) against the page's ap:steps: there must be exactly
// one timeline, its steps must be literal (a bound `steps` has no ids to check
// at author time), and the id must be declared. Runs after the node walk, so
// the timeline's own ids have already passed checkSteps.
//
// The reason names the hook, the id, and what the timeline declares: this
// message reaches the AGENT when a repaint of the timeline drops an id a hook
// depends on (ResolveView validates the composite), and it must say which id
// to put back.
func validateStepBindings(d Declaration) error {
	var bound []Hook
	for _, h := range Hooks(d) {
		if h.Step != "" {
			bound = append(bound, h)
		}
	}
	if len(bound) == 0 {
		return nil
	}
	first := bound[0]
	var timelines []Node
	collectByComponent(*d.View, "ap:steps", &timelines)
	if len(timelines) != 1 {
		return &ValidationError{Path: hookPathString(first.Path), Reason: fmt.Sprintf(
			"hook %q binds step %q, but the page has %d ap:steps timelines; step binding needs exactly one",
			first.Name, first.Step, len(timelines))}
	}
	tl := timelines[0]
	if _, isBound := tl.Bindings["steps"]; isBound {
		return &ValidationError{Path: hookPathString(first.Path), Reason: fmt.Sprintf(
			"hook %q binds step %q, but the timeline's steps are bound to a source; step ids must be literal",
			first.Name, first.Step)}
	}
	var steps []Step
	if raw, ok := tl.Props["steps"]; ok {
		// Shape already checked by validateNode; a decode failure here would
		// have failed the page there first.
		_ = json.Unmarshal(raw, &steps)
	}
	ids := make([]string, 0, len(steps))
	for _, st := range steps {
		if st.ID != "" {
			ids = append(ids, st.ID)
		}
	}
	for _, h := range bound {
		if !slices.Contains(ids, h.Step) {
			list := "none"
			if len(ids) > 0 {
				list = strings.Join(ids, ", ")
			}
			return &ValidationError{Path: hookPathString(h.Path), Reason: fmt.Sprintf(
				"hook %q binds step %q, which the timeline does not declare (ids: %s)", h.Name, h.Step, list)}
		}
	}
	return nil
}

// validatePageRoot enforces oap:page's one placement rule: it may be the root
// of the view and nothing else. A nested page would let a fill (already refused
// as structural) or an author put a second layout inside the first, and the
// renderer has no answer for what that means.
func validatePageRoot(d Declaration) error {
	var pages []Node
	collectByComponent(*d.View, PageType, &pages)
	switch {
	case len(pages) == 0:
		return nil
	case len(pages) == 1 && d.View.Component == PageType:
		return nil
	}
	return &ValidationError{Path: "view", Reason: fmt.Sprintf(
		"oap:page may only be the root of the view, once; found %d", len(pages))}
}

// collectByComponent appends every node of the given component type, in
// document order.
func collectByComponent(n Node, component string, out *[]Node) {
	if n.Component == component {
		*out = append(*out, n)
	}
	for _, ch := range n.Children {
		collectByComponent(ch, component, out)
	}
}

// hookPathString renders a Hook.Path the way the node walk renders paths
// ("view.children[1].children[0]"), so a step-binding error points where a
// props error would. Keep it in step with validateNode's "%s.children[%d]"
// path format below — the two must agree for a step-binding error to point
// where a props error does.
func hookPathString(path []int) string {
	var b strings.Builder
	b.WriteString("view")
	for _, i := range path {
		fmt.Fprintf(&b, ".children[%d]", i)
	}
	return b.String()
}

// validateActions checks the declaration's top-level action table, then that
// every control's action prop names an entry in it. Order matters: the table
// is validated first so a malformed table produces ONE error naming the
// table, not one error per control that referenced it.
//
// The grant check consults GrantedTools ONLY, never ReadonlyTools: an action
// exists to mutate, and requiring readonly here would make the write half
// unreachable. That asymmetry with validateBindings — which requires BOTH
// maps — is "writes are never a data binding" expressed in code, and the test
// fixture keeps ReadonlyTools narrower than GrantedTools so a copy-paste of
// the data-binding check turns red.
//
// Fail-closed on a nil GrantedTools map for the same mechanical reason
// validateBindings is: indexing a nil map returns false. Do NOT add a
// `!= nil` guard — that branch needs its own deny arm to stay closed.
//
// Every {"$param":"…"} placeholder in a.Args must name a declared ParamKey or
// one of that SAME action's Inputs — exactly the union agentui's
// actionsHandler builds at invoke time before substituting. Without this,
// {"leadId":{"$param":"leed"}} validates clean, renders live, and 400s on
// EVERY submit with ErrUnknownParam, aiming advice at a viewer who cannot fix
// an author's typo.
func validateActions(d Declaration, o Options) error {
	seenNames := make(map[string]struct{}, len(d.Actions))
	// ParamKeys, NOT ParamNames. The collision this prevents happens in ONE
	// map: agentui's actionsHandler merges filtered params and filtered inputs
	// into a single values map. So the domain that must be disjoint is the
	// runtime KEY set, and a declared name is not always a key — an
	// ap:daterange named "window" drives "window.from"/"window.to", never
	// "window". ParamNames would be wrong in BOTH directions: an input named
	// "window.from" would validate and then collide at merge time, while a
	// legitimate input named "window" would be rejected.
	paramKeys := ParamKeys(d)
	declaredParamKeys := make(map[string]struct{}, len(paramKeys))
	for _, k := range paramKeys {
		declaredParamKeys[k] = struct{}{}
	}

	for i, a := range d.Actions {
		path := fmt.Sprintf("actions[%d]", i)
		if a.Name == "" {
			return &ValidationError{Path: path, Reason: "missing action name"}
		}
		if _, dup := seenNames[a.Name]; dup {
			return &ValidationError{Path: path, Reason: fmt.Sprintf("duplicate action name %q", a.Name)}
		}
		seenNames[a.Name] = struct{}{}
		// An action either CALLS something or SAYS something. Both would give
		// it two outcomes and two failure modes with no answer for what its
		// pending state means; neither leaves a control that does nothing when
		// pressed, which is worse than a document that fails to load.
		switch {
		case a.Tool == "" && a.Prompt == "":
			return &ValidationError{Path: path, Reason: "action must declare either a tool to call or a prompt to send"}
		case a.Tool != "" && a.Prompt != "":
			return &ValidationError{Path: path, Reason: "action declares both a tool and a prompt; it must declare exactly one"}
		}

		// A prompt action reaches no tool, so there is no grant to check. What
		// bounds it instead is that its message is one the viewer could have
		// typed into the transcript themselves — same route, same
		// authorization, same attribution. Requiring a tool grant here would
		// be asking for permission to do something it does not do.
		if a.Tool != "" {
			ref := a.Tool
			if o.NormalizeToolName != nil {
				ref = o.NormalizeToolName(ref)
			}
			if !o.GrantedTools[ref] {
				return &ValidationError{Path: path, Reason: fmt.Sprintf("tool is not granted to this UI: %q", a.Tool)}
			}
		}
		seenInputs := make(map[string]struct{}, len(a.Inputs))
		for _, in := range a.Inputs {
			if in == "" {
				return &ValidationError{Path: path, Reason: "action input name must not be empty"}
			}
			if _, dup := seenInputs[in]; dup {
				return &ValidationError{Path: path, Reason: fmt.Sprintf("duplicate action input name %q", in)}
			}
			seenInputs[in] = struct{}{}
			if _, collide := declaredParamKeys[in]; collide {
				return &ValidationError{Path: path, Reason: fmt.Sprintf("action input %q collides with a binding parameter of the same name", in)}
			}
		}

		// Gate B: every placeholder in the action's OWN args template must
		// resolve. See this function's doc comment for the exact union
		// (ParamKeys ∪ this action's Inputs) and why it must be that union and
		// not either set alone.
		refs, err := resolveParamRefs(a.Args)
		if err != nil {
			return &ValidationError{Path: path, Reason: fmt.Sprintf("args are not valid JSON: %v", err)}
		}
		for _, name := range refs {
			if _, ok := declaredParamKeys[name]; ok {
				continue
			}
			if slices.Contains(a.Inputs, name) {
				continue
			}
			return &ValidationError{Path: path, Reason: fmt.Sprintf(
				"args reference a value nothing supplies: no control drives the binding parameter %q and this action does not declare it as an input (parameters: %s; inputs: %s)",
				name, declaredKeyList(declaredParamKeys), declaredInputList(a.Inputs))}
		}

		// The same gate for a prompt's {key} placeholders, over the same
		// union. Unfilled, a placeholder does not fail — it reaches the agent
		// as the literal text "{name}", which is a question about nothing that
		// the agent will answer as best it can. Catching it at the document is
		// the only place it is still obviously a mistake.
		for _, name := range PromptRefs(a.Prompt) {
			if _, ok := declaredParamKeys[name]; ok {
				continue
			}
			if slices.Contains(a.Inputs, name) {
				continue
			}
			return &ValidationError{Path: path, Reason: fmt.Sprintf(
				"prompt references a value nothing supplies: no control drives the binding parameter %q and this action does not declare it as an input (parameters: %s; inputs: %s)",
				name, declaredKeyList(declaredParamKeys), declaredInputList(a.Inputs))}
		}
	}

	for _, r := range ActionRefs(d) {
		a, ok := d.Action(r.Action)
		if !ok {
			return &ValidationError{Path: r.Path, Reason: fmt.Sprintf("undeclared action %q", r.Action)}
		}
		// Every value a control supplies at invoke time must be one the action
		// declared. This is the ONLY place the two halves of that contract —
		// a control's field names and the action's Inputs allowlist — are
		// compared: without it, an ap:form declaring [title, body] against an
		// action declaring [title] validates clean, renders live, and rejects
		// EVERY submit at agentui's filterActionInputs, aiming a reload at a
		// viewer who cannot fix an author's document. The author can, and the
		// AgentUI's Valid condition is where they are looking.
		for _, in := range r.Inputs {
			if in == "" {
				return &ValidationError{Path: r.Path, Reason: "control declares an input with no name"}
			}
			if !slices.Contains(a.Inputs, in) {
				return &ValidationError{Path: r.Path, Reason: fmt.Sprintf(
					"control supplies the input %q, which action %q does not declare (declared: %s)",
					in, r.Action, declaredInputList(a.Inputs))}
			}
		}
	}
	return nil
}

// declaredInputList renders an action's Inputs for an author-facing error.
// "none" rather than an empty list, because "this action accepts no inputs at
// all" is a different, more useful fact than "you picked the wrong one" — the
// same argument declaredKeyList makes for binding parameters.
func declaredInputList(inputs []string) string {
	if len(inputs) == 0 {
		return "none"
	}
	return strings.Join(inputs, ", ")
}

// validateNode walks one subtree. depth is 1-based at the view root; count is
// owned by the caller and threaded through the whole walk, so MaxNodes bounds
// the PAGE — a tree that spread itself across many hooks is bounded the same
// as one that put everything in a single region, not once per region.
func validateNode(n Node, path string, depth int, count *int, o Options, declaredKeys map[string]struct{}) error {
	if depth > o.MaxDepth {
		return &ValidationError{Path: path, Reason: fmt.Sprintf("maximum nesting depth exceeded (%d)", o.MaxDepth)}
	}
	*count++
	if *count > o.MaxNodes {
		return &ValidationError{Path: path, Reason: fmt.Sprintf("maximum node count exceeded (%d)", o.MaxNodes)}
	}
	if n.Component == "" {
		return &ValidationError{Path: path, Reason: "missing component type"}
	}
	c, ok := registry.Get(n.Component)
	if !ok {
		return &ValidationError{Path: path, Reason: fmt.Sprintf("unknown component type %q", n.Component)}
	}
	if len(n.Children) > 0 && !c.AcceptsChildren {
		return &ValidationError{Path: path, Reason: fmt.Sprintf("component %q does not accept children", n.Component)}
	}
	if err := validateProps(n.Props, c); err != nil {
		return &ValidationError{Path: path, Reason: "invalid props: " + err.Error()}
	}
	if err := validateBindings(n.Bindings, c, o, declaredKeys); err != nil {
		// Err preserves the chain (e.g. down to a uiselect sentinel) so a
		// caller can errors.Is/errors.As through the *ValidationError; Reason
		// still carries the flattened text for the plain-string callers.
		return &ValidationError{Path: path, Reason: err.Error(), Err: err}
	}
	for i, ch := range n.Children {
		if err := validateNode(ch, fmt.Sprintf("%s.children[%d]", path, i), depth+1, count, o, declaredKeys); err != nil { // same format hookPathString renders
			return err
		}
	}
	return nil
}

// validateProps checks a node's props in two passes: every top-level key must
// be an EXACT json field name of the component's props struct, and the whole
// map must then decode into a fresh instance of that struct with
// DisallowUnknownFields.
//
// The struct IS the schema, rather than a separate JSON Schema document, so
// the validator and the schema published to the agent (schema.go) cannot
// drift. Rejecting an invented prop rather than ignoring it is the point: the
// props are LLM output, and a silently-dropped prop reads to the agent as
// success while the renderer paints an empty block.
//
// The exact-name pass exists because encoding/json's field matching is
// CASE-INSENSITIVE and DisallowUnknownFields does not change that. Without it
// {"Body": …} and {"BODY": …} both decode into MarkdownProps.Body — two
// spellings the emitted schema rejects and the React renderer never reads,
// which is the same silently-dropped-prop failure arriving through the
// validator's front door. It also keeps Go's `json: unknown field "…"` wording
// out of what the agent is shown.
//
// Both passes are TOP-LEVEL only. Fields nested inside a props struct still
// reach the case-insensitive matcher: DisallowUnknownFields rejects an
// invented nested name but not a case-variant one. Closing that needs a
// recursive walk of the JSON against the struct type.
func validateProps(props map[string]json.RawMessage, c Component) error {
	if len(props) == 0 {
		// A component with a Check hook may still reject the empty case (an
		// omitted required prop, e.g. ap:agentlink's Label) — every OTHER
		// component's props are all-optional-with-defaults, which is why this
		// early return exists at all, but Check exists precisely for the
		// components where that isn't true. Run it against a zero-value
		// struct rather than skip it, so "no props" doesn't silently bypass
		// the same validation a props map full of blanks would hit.
		if c.Check != nil {
			if c.Props == nil {
				return fmt.Errorf("component %q was registered with no props schema (Props is nil)", c.Type)
			}
			return c.Check(reflect.New(reflect.TypeOf(c.Props)).Interface())
		}
		return nil
	}
	// c.Props == nil is a malformed registration (Register has no way to
	// reject it: nil is a legal `any`). reflect.TypeOf(nil) returns nil, and
	// reflect.New(nil) panics — guard here so a bad registration surfaces as
	// the same *ValidationError every other rejection in this file returns,
	// not a panic with no recover() on this call path.
	if c.Props == nil {
		return fmt.Errorf("component %q was registered with no props schema (Props is nil)", c.Type)
	}
	// Same class of malformed registration: jsonFieldNames calls NumField,
	// which panics on any non-struct kind. A pointer registration
	// (Props: &FooProps{}) looks right, Schema() accepts it happily, and it
	// would then panic on the first agent-supplied props map, on a path with
	// no recover(). Props is the zero VALUE of the struct, not a pointer.
	t := reflect.TypeOf(c.Props)
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("component %q was registered with a non-struct props schema (Props is %s, want a struct value)", c.Type, t.Kind())
	}
	legal := jsonFieldNames(t)
	for name := range props {
		if _, ok := legal[name]; !ok {
			return fmt.Errorf("unknown prop %q on component %q", name, c.Type)
		}
	}
	raw, err := json.Marshal(props)
	if err != nil {
		return err
	}
	// A fresh zero value per call: reusing c.Props would let one node's props
	// leak into the next node's validation.
	target := reflect.New(t).Interface()
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Still set, even though the loop above already rejected every unknown
	// TOP-LEVEL name: DisallowUnknownFields applies recursively, so this is
	// what rejects an invented field inside a nested struct element.
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if c.Check != nil {
		return c.Check(target)
	}
	return nil
}

// jsonFieldNames returns the exact set of top-level JSON keys encoding/json
// would write for a props struct type: exported fields only, the tag name
// before the first comma, "-" (opt-out) skipped, and the Go field name for an
// untagged field (which is what encoding/json itself uses).
//
// An embedded struct's promoted fields are NOT flattened, so a props struct
// that grew one would have its promoted names rejected. No vocabulary
// component embeds today, and that direction of wrongness is a rejection, not
// a silent accept — the safe way to be wrong at this boundary.
//
// PRECONDITION: t.Kind() == reflect.Struct. NumField panics on anything else,
// and this package has no recover() on the validation path, so the caller must
// establish that first — validateProps does.
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: invisible to encoding/json
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = struct{}{}
	}
	return out
}

// sourceTool is the binding source that names a tool. It is a named constant
// — not a literal repeated at each use — because it appears both in
// bindingSources (the closed set of known brokers) and in the grant check
// below; renaming one occurrence without the other would silently remove the
// grant check from whatever source ends up spelled "tool".
const sourceTool = "tool"

// sourceAction is the binding source that references a DECLARED ACTION — not
// an invocation path. A prop bound to it may only ask "does this action
// exist", with empty Args; the action's tool is never called by resolving a
// binding, only by a viewer clicking the control that NAMES the action via
// ActionProp, re-authorized per click in runner.handleAppToolCallReq. Named
// the same as it appears on the wire, for the same reason sourceTool is.
const sourceAction = "action"

// bindingSources is the closed set of brokers a binding may name. Adding a
// source here is a deliberate act: each one is a distinct data path the
// platform resolves server-side under the viewing subject.
var bindingSources = []string{sourceTool, "memory", "artifact", sourceAction}

// validateBindings enforces seven separate things, in order of how cheaply
// they fail:
//
//  1. the prop is one the COMPONENT declared bindable — which keeps "what can
//     reach data" a platform decision, not an author or agent one;
//  2. the source is a known broker and the ref is non-empty;
//  3. for a tool source, the tool is in the materialized grant;
//  4. for a tool source, the tool is READONLY — every entry in Node.Bindings
//     is a DATA binding by construction (a mutation is an action binding, on
//     a different field entirely), so a data binding naming a tool that
//     writes is always wrong, independent of whether it was granted;
//  5. for an action source, the ref names an entry in o.DeclaredActions and
//     carries no Args — this is a REFERENCE to a declared action, not an
//     invocation of it, so there is nothing to parameterize;
//  6. the selector, if any, is syntactically well formed (validateSelect).
//     SYNTAX only: a string scan over at most uiselect.MaxSelectorLen bytes,
//     cheaper than (7)'s decode, and a question about the binding's own shape.
//     Whether the path MATCHES a given response is decidable only against
//     that response, and is checked there;
//  7. every {"$param":"…"} placeholder in the args template names a parameter
//     key some control in THIS declaration actually drives — see
//     validateParamRefs for why an unsatisfiable reference must fail here
//     rather than on a viewer's page.
//
// (3) and (4) are fail-closed on a nil map, by MECHANISM not intent: indexing
// a nil Go map returns false rather than panicking, so "no grant" / "not
// readonly" IS the default with no nil check. Do NOT add a `!= nil` guard —
// that branch needs its own deny arm to stay closed, and a forgotten one flips
// the default to allow. Do NOT swap either map for a slice + contains() check
// either: a nil slice reads the same as deny today, but the map's whole point
// is that indexing is unconditionally safe on nil, and re-deriving that by
// hand is exactly the quiet inversion this comment exists to prevent.
//
// Both compare o.NormalizeToolName(ref), never the raw ref — a binding's Ref
// carries no CRD-enforced vocabulary of its own.
//
// The whole function is DEFENSE IN DEPTH, not the boundary: it rejects a bad
// binding only when the validating caller can see tool metadata at all (an
// AgentUI reconciler has no session and passes the same ceiling for both maps,
// making (4) vacuous there by design). The unconditional gate is
// runner.handleAppToolCallReq, re-evaluated against the live tool per call.
func validateBindings(bindings map[string]Binding, c Component, o Options, declaredKeys map[string]struct{}) error {
	for prop, b := range bindings {
		if !slices.Contains(c.Bindable, prop) {
			return fmt.Errorf("prop is not bindable: %q on component %q", prop, c.Type)
		}
		if !slices.Contains(bindingSources, b.Source) {
			return fmt.Errorf("unknown binding source %q on prop %q", b.Source, prop)
		}
		if b.Ref == "" {
			return fmt.Errorf("missing binding ref on prop %q", prop)
		}
		if b.Source == sourceTool {
			ref := b.Ref
			if o.NormalizeToolName != nil {
				ref = o.NormalizeToolName(ref)
			}
			if !o.GrantedTools[ref] {
				return fmt.Errorf("tool is not granted to this UI: %q", b.Ref)
			}
			// Every entry in Node.Bindings is a DATA binding; a mutation is an
			// action binding on a different field. So a data binding may only
			// name a tool that reads.
			if !o.ReadonlyTools[ref] {
				return fmt.Errorf("a data binding may only read: %q changes state; wire it to an action instead", b.Ref)
			}
		}
		if b.Source == sourceAction {
			if !o.DeclaredActions[b.Ref] {
				return fmt.Errorf("binding names an undeclared action: %q", b.Ref)
			}
			if len(b.Args) > 0 {
				return fmt.Errorf("an action binding must not carry args: prop %q names action %q", prop, b.Ref)
			}
		}
		if err := validateSelect(b.Select, prop); err != nil {
			return err
		}
		if err := validateParamRefs(b.Args, prop, declaredKeys); err != nil {
			return err
		}
	}
	return nil
}

// validateSelect rejects a binding whose selector does not parse. A
// selector's correctness is split across two layers on purpose: SYNTAX is
// decidable from the document alone and belongs on the author's Valid
// condition, while whether the path MATCHES is decidable only against a
// response. A malformed selector reaching a viewer renders a permanent error
// card no reload can clear, aimed at someone who cannot fix an author's
// document — the same argument validateParamRefs makes.
func validateSelect(sel string, prop string) error {
	if sel == "" {
		return nil
	}
	if _, err := uiselect.Parse(sel); err != nil {
		return fmt.Errorf("binding selector on prop %q is not valid: %w", prop, err)
	}
	return nil
}

// validateParamRefs rejects an args template that references a binding
// parameter no control in this declaration drives.
//
// Without it the failure lands on the VIEWER: SubstituteParams returns
// ErrUnknownParam at request time and the node renders a permanent error card
// no reload can clear, because the key it wants exists nowhere in the
// document. Two shapes reach that from an otherwise legal declaration — a
// typo, and naming an ap:daterange's DECLARED name ("window") where only its
// expanded keys ever exist. Both are author bugs, and an author bug belongs on
// the AgentUI's Valid condition, not on a stranger's page.
//
// declaredKeys comes from ParamKeys — each control's own registered
// ParamValues — so a new parameterizing component satisfies this by
// registration alone.
func validateParamRefs(args json.RawMessage, prop string, declaredKeys map[string]struct{}) error {
	refs, err := resolveParamRefs(args)
	if err != nil {
		return fmt.Errorf("binding args on prop %q are not valid JSON: %w", prop, err)
	}
	for _, name := range refs {
		if _, ok := declaredKeys[name]; ok {
			continue
		}
		return fmt.Errorf("binding args on prop %q reference a parameter no control drives: no control declares the binding parameter %q (declared: %s)",
			prop, name, declaredKeyList(declaredKeys))
	}
	return nil
}

// resolveParamRefs decodes an args template and returns every placeholder name
// within it; (nil, nil) for an empty template. Shared by validateParamRefs (a
// data binding's args) and validateActions' Gate B (an action's own args), so
// there is ONE answer to "what does this template reference" — the two callers
// differ only in what counts as SATISFIED.
func resolveParamRefs(args json.RawMessage) ([]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil, err
	}
	return collectParamRefs(v), nil
}

// collectParamRefs returns every parameter name referenced by a placeholder
// anywhere in a decoded args template. A placeholder is an object with EXACTLY
// one key, ParamRefKey, whose value is a string — the same rule
// pkg/web/uibindings.SubstituteParams applies when filling it in, so the two can
// never disagree about which objects are placeholders.
func collectParamRefs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if name, ok := t[ParamRefKey].(string); ok {
				return []string{name}
			}
		}
		// Sorted so the FIRST-failure message this feeds is deterministic
		// rather than an accident of Go's randomized map iteration.
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out = append(out, collectParamRefs(t[k])...)
		}
	case []any:
		for _, child := range t {
			out = append(out, collectParamRefs(child)...)
		}
	}
	return out
}

// declaredKeyList renders the legal parameter keys for an author-facing error.
// "(declared: none)" rather than an empty list, because "the document declares
// no parameters at all" is a different, more useful fact than "you picked the
// wrong one".
func declaredKeyList(declaredKeys map[string]struct{}) string {
	if len(declaredKeys) == 0 {
		return "none"
	}
	return strings.Join(slices.Sorted(maps.Keys(declaredKeys)), ", ")
}
