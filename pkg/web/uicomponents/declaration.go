package uicomponents

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ParseDeclaration is THE entry point for turning declaration JSON into a
// Declaration. Use it instead of json.Unmarshal — a plain Unmarshal ignores
// keys it does not recognize, so {"component":"ap:text","propz":{…},
// "childs":[…]} parses into a Node with no props and no children, and then
// validates clean. The agent (or a bundle author) is told SUCCESS and gets an
// empty node: the same silently-dropped-field failure validateProps refuses at
// the prop level, arriving one layer up at the wire level.
//
// Strictness applies to the four wire structs only — Declaration, Slot, Node,
// Binding. Node.Props and Binding.Args are json.RawMessage and are deliberately
// NOT parsed here: props are checked against the component's own struct by
// Validate, and a binding's args are the source's business, not this layer's.
//
// The tradeoff: this also rejects a Tier-0 bundle authored against a NEWER
// platform carrying a wire field this build never heard of. Fail-closed is
// deliberate — the alternative is a bundle quietly losing a field it depends
// on, in a document that is an authorization-shaped input (slots carry
// agentWritable, bindings carry tool refs). A forward-compatible parse would
// have to say WHICH unknown fields are safe to ignore, and nothing here knows.
//
// After the strict decode, Normalize turns whichever wire shape was sent (a
// View, or the legacy Slots) into the one runtime shape every consumer walks.
func ParseDeclaration(data []byte) (Declaration, error) {
	var d Declaration
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Declaration{}, fmt.Errorf("uicomponents: parse declaration: %w", err)
	}
	return Normalize(d)
}

// Declaration is a complete agent-UI document.
//
// After ParseDeclaration/Normalize it is ONE shape: View is the page tree and
// Slots is nil. Slots is accepted on the wire for pages authored before the
// tree existed — CompileSlots turns each one into a hook in a root ap:stack —
// and is never carried alongside a View: every consumer walks View, and a
// document that said both would have two answers to "what is on the page".
type Declaration struct {
	// Actions is the top-level action table — see Action for why it lives here
	// rather than on a node. Absent when the declaration invokes nothing.
	Actions []Action `json:"actions,omitempty"`

	// View is the page tree. Every oap:generative node in it is a region the
	// agent may fill (see Hooks). Always set after Normalize.
	View *Node `json:"view,omitempty"`

	// Slots is the LEGACY wire shape: a flat list of named regions. Accepted
	// on parse and compiled into View by Normalize; nil thereafter.
	Slots []Slot `json:"slots,omitempty"`
}

// Normalize returns d in the one runtime shape: View set, Slots nil.
//
//   - View set, no Slots: returned as-is.
//   - Slots set, no View: View = CompileSlots(Slots).
//   - neither: an empty root stack — a page with nothing on it, distinct
//     from a malformed one, which the strict decode already refused.
//   - both: an error. Two descriptions of one page cannot be reconciled
//     by picking one, so the author is told to pick.
func Normalize(d Declaration) (Declaration, error) {
	switch {
	case d.View != nil && len(d.Slots) > 0:
		return Declaration{}, fmt.Errorf("uicomponents: declare either view or slots, not both")
	case d.View == nil:
		d.View = CompileSlots(d.Slots)
	}
	d.Slots = nil
	return d, nil
}

// CompileSlots is the spec.slots shim: a root ap:stack whose children are,
// in slot order, a hook per agentWritable slot (name = the slot's, intent
// "", allowedComponents ["*"], children = the slot's default) and, for a
// slot that is not agentWritable, its default node placed directly (a
// region the agent could never write is not a hook, so it needs no name).
// A non-writable slot with no default contributes nothing.
func CompileSlots(slots []Slot) *Node {
	root := &Node{Component: "ap:stack"}
	for _, s := range slots {
		if !s.AgentWritable {
			if s.Default != nil {
				root.Children = append(root.Children, *s.Default)
			}
			continue
		}
		hook := Node{
			Component: GenerativeType,
			Props: map[string]json.RawMessage{
				"name":              mustJSON(s.Name),
				"allowedComponents": mustJSON([]string{AllowAll}),
			},
		}
		if s.Default != nil {
			hook.Children = []Node{*s.Default}
		}
		root.Children = append(root.Children, hook)
	}
	return root
}

// mustJSON marshals a value json.Marshal cannot fail on (a string, a
// []string). A panic here is a programming error in this file, not input.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("uicomponents: mustJSON: " + err.Error())
	}
	return raw
}

// Action is one declared, invocable mutation: a tool plus an args template.
// It lives at the TOP of a declaration, not on a node, and a control only
// ever NAMES one (ButtonProps.Action, FormProps.Action). Three consequences,
// all deliberate:
//
//   - The browser sends a NAME. Source, tool and args template are read out of
//     the server-side declaration, exactly as a data binding's are.
//   - An action is never in Node.Bindings, so "writes are never a data
//     binding" is enforced by the SHAPE of the document rather than by a check
//     that could be forgotten.
//   - A Tier-1 fragment rewrites a HOOK, so an agent can NAME an existing
//     action and can never mint one. Not the security boundary — the
//     per-click re-authorization in runner.handleAppToolCallReq is — but a
//     real, free narrowing that must not be widened silently.
type Action struct {
	// Name is what a control's action prop names. Unique within a declaration.
	Name string `json:"name"`

	// Tool is the app-tool this action calls, in the same <ref>_<tool>
	// vocabulary AgentUISpec.Tools and AgentClass.spec.agentUI.grantedTools
	// use. Compared against a grant ONLY after Options.NormalizeToolName —
	// see validateActions.
	//
	// Exactly one of Tool and Prompt is set. See Prompt.
	Tool string `json:"tool,omitempty"`

	// Prompt makes this action ASK THE AGENT instead of calling a tool: the
	// control sends this text to the session as a message from the viewer, and
	// the agent answers it in the ordinary way. It exists because a page needs
	// to say "summarize this" or "tell me about this row", and there is no
	// tool for "think about what is on screen".
	//
	// It grants NOTHING new: the message is one the viewer could have typed
	// into the transcript, over the same route, under the same authorization,
	// attributed to them — which is also why the browser filling this template
	// in is not escalation.
	//
	// Placeholders are {key} naming a binding parameter or a declared Input,
	// so a row's values can reach the sentence ("Tell me about {name}").
	// Deliberately NOT the {"$param": …} object form: this is prose with holes
	// in it, not a JSON document.
	//
	// Exactly one of Tool and Prompt is set: an action that did both would
	// have two outcomes, two failure modes, and no answer for what its pending
	// state means.
	Prompt string `json:"prompt,omitempty"`

	// Args is the source-specific argument template, with the SAME
	// {"$param":"<name>"} placeholder vocabulary a data binding's args use
	// (pkg/web/uibindings.SubstituteParams). Reusing it is the point: there is
	// one substitution rule in the codebase, not one per surface.
	Args json.RawMessage `json:"args,omitempty"`

	// Inputs names the values a CONTROL may supply at invoke time — an
	// ap:form's field names, typically. It is a server-side allowlist, which
	// is what lets the browser contribute values at all without contributing
	// keys: a name not listed here is rejected by the actions route, never
	// silently dropped into the template. validateActions also requires the
	// reverse containment — every name a control declares it will supply must
	// appear here — so a form that would 400 on every submit is rejected at
	// the author's document rather than on a viewer's page.
	//
	// Inputs must be DISJOINT from the declaration's runtime binding-parameter
	// KEYS (ParamKeys, not ParamNames), because keys are the domain the two
	// share: agentui's actionsHandler merges filtered params and filtered
	// inputs into ONE values map. Enforced statically in validateActions
	// rather than resolved at merge time — "which map wins" is a question with
	// no good answer.
	Inputs []string `json:"inputs,omitempty"`
}

// Action returns the declared action with this name. The bool is the whole
// authorization-shaped answer for the invoke path: an unknown name is a
// rejected request, never a fallthrough to some default action.
func (d Declaration) Action(name string) (Action, bool) {
	if name == "" {
		return Action{}, false
	}
	for _, a := range d.Actions {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

// Slot is one region of the LEGACY flat page shape; see CompileSlots.
type Slot struct {
	// Name identifies the slot; unique within a declaration.
	Name string `json:"name"`

	// AgentWritable permits the agent to overwrite this slot at runtime.
	// Default false: a slot is author-owned unless it opts in.
	AgentWritable bool `json:"agentWritable,omitempty"`

	// Default is the author-declared content. It is FUNCTIONAL, not
	// decorative — a slot with a default renders complete, working UI with no
	// agent turn involved, which is what lets a brand-new session paint on
	// first load.
	Default *Node `json:"default,omitempty"`
}

// Node is one component instance in the tree.
type Node struct {
	// Component is the vocabulary key this node renders as, e.g. "ap:table".
	Component string `json:"component"`

	// Props are literal prop values, validated against the component's props
	// struct. RawMessage so validation unmarshals into that struct rather than
	// round-tripping through interface{}.
	Props map[string]json.RawMessage `json:"props,omitempty"`

	// Bindings attach a prop to a platform-brokered source instead of a
	// literal; only names in the component's Bindable list are allowed.
	//
	// A prop MAY carry both a literal (in Props) and a binding at once — the
	// literal is the pre-resolution placeholder ("show 0 until the count
	// loads") and the resolved value replaces it. Rejecting the overlap would
	// cost a genuinely useful pattern.
	Bindings map[string]Binding `json:"bindings,omitempty"`

	// Children is the subtree, legal only for a component whose registration
	// sets AcceptsChildren.
	Children []Node `json:"children,omitempty"`
}

// Binding connects one prop to a source the platform resolves server-side under
// the viewing subject. The browser never holds a capability; it never resolves
// a binding itself.
type Binding struct {
	// Source is the broker. The legal set is bindingSources (validate.go),
	// which tracks the registered pkg/web/uibindings resolvers.
	Source string `json:"source"`

	// Ref names the thing within that source — a tool name, a memory kind, an
	// artifact handle, a declared action name.
	Ref string `json:"ref"`

	// Args is the source-specific argument payload (tool args, query filters).
	// Anywhere inside it, an object whose ONLY key is ParamRefKey and whose
	// value is a string is a placeholder for the viewer's current value of
	// that binding parameter; pkg/web/uibindings.SubstituteParams fills it in
	// server-side, and validateBindings checks at author time that some
	// control actually drives the name.
	Args json.RawMessage `json:"args,omitempty"`

	// Select names WHICH part of the resolved value fills the prop, in
	// pkg/web/uiselect's dotted-path-plus-"[]" language. Empty means the whole
	// resolved value, byte for byte.
	//
	// Applied SERVER-SIDE, after the source resolves and before the response
	// is written, so the browser receives a finished value and never an
	// envelope to dig through. It belongs to the DECLARATION, like Source, Ref
	// and Args: no field on the bindings request carries it.
	//
	// Legal on EVERY source — unlike Args, which validateBindings rejects on
	// an action binding, since args are an invocation input and an action
	// binding invokes nothing. A selector merely projects a value, and every
	// source produces one.
	//
	// SYNTAX is checked at author time; whether the path is present in a given
	// response is knowable only where the data is, and is checked there.
	Select string `json:"select,omitempty"`
}

// ParamRefKey is the sole key of an args-template placeholder object,
// {"$param":"<name>"}. It lives here, with the Binding type that owns the
// template, so the validator (which rejects a name no control drives) and the
// substituter (pkg/web/uibindings, which fills it in) cannot disagree about the
// spelling — a divergence would mean a placeholder that validates as ordinary
// data and then never gets substituted, or vice versa.
const ParamRefKey = "$param"
