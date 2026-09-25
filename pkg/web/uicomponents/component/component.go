// Package component holds the Component type in a package that does NOT depend
// on pkg/web/uicomponents. That is the invariant and the whole reason it
// exists: registry needs the concrete type in its Register/Get signatures, and
// uicomponents needs to call registry.Get from validate.go, so defining
// Component in uicomponents would be an import cycle. Both sides depend on
// this leaf instead, and uicomponents re-exports the type as an alias.
//
// Keep it that way: Component must never grow a method needing Node or
// Declaration — pulling either in here re-creates the cycle.
package component

// Component describes one renderable type in the vocabulary.
type Component struct {
	// Type is the namespaced vocabulary key, e.g. "ap:table". Namespacing is
	// load-bearing: it reserves "ext:<bundle>/<name>" for the deferred
	// bundle-supplied-component path without a future collision.
	Type string

	// Props is the zero value of this component's props struct. Validation
	// unmarshals into a fresh copy of its type; schema emission reflects over it.
	Props any

	// Bindable lists the prop names that may carry a binding instead of a
	// literal value. A binding on any other prop is rejected — this is what
	// keeps "which props can reach data" a platform decision rather than an
	// author or agent one.
	Bindable []string

	// AcceptsChildren reports whether Node.Children is legal for this type.
	// A leaf component with children is a malformed declaration, not a
	// silently-ignored field.
	AcceptsChildren bool

	// ParamProp names the prop carrying this control's binding-parameter NAME
	// (not its value): "param" on ap:select and ap:daterange. Empty for a
	// component that drives no parameter. It lets a caller ask the REGISTRY
	// which parameter a node declares, instead of switching on Type.
	ParamProp string

	// ParamValues describes the runtime parameter KEYS this control drives for
	// the name in ParamProp, and where each key's declared default lives.
	// Empty when there is no ParamProp.
	//
	// A declared name is not always a usable key: ap:daterange declaring
	// "window" drives "window.from" and "window.to" and NEVER "window", so
	// {"$param":"window"} can never be satisfied by any viewer action.
	// Enumerating the keys here lets the validator reject that at author time
	// and the browser seed the same set, instead of each consumer re-deriving
	// the compound-name convention from a prefix rule or a type switch.
	ParamValues []ParamValue

	// ActionProp names the prop carrying this control's ACTION NAME: "action"
	// on ap:button and ap:form. Empty for a component that fires nothing. Same
	// registry-not-type-switch reason as ParamProp, so a future control that
	// fires an action is a registration change, not a validator edit.
	ActionProp string

	// InputNamesProp names the prop carrying the INPUT NAMES this control
	// supplies at invoke time: "fields" on ap:form. Empty for a component that
	// supplies none.
	//
	// CONTRACT: the prop's value is a JSON array of objects each carrying a
	// "name" string. Only that key is read, so a control's own per-field shape
	// may grow freely without moving this contract.
	//
	// It lets the validator check a control's inputs against the named
	// action's Inputs allowlist through the registry. Without it the two
	// halves are never compared, and a mismatched form validates clean,
	// renders live, then rejects every submit.
	InputNamesProp string

	// ActionListProp names a prop carrying SEVERAL action names: "buttons" on
	// ap:notice. Empty for a component that fires nothing this way. Same
	// registry-not-type-switch reason as ActionProp, and the two are
	// independent — a component may declare either, both, or neither.
	//
	// CONTRACT: the prop's value is a JSON array of objects each carrying an
	// "action" string. Only that key is read (ActionRefs), so a control's own
	// per-entry shape may grow freely without moving this contract.
	ActionListProp string

	// Structural marks a type that shapes the page rather than rendering
	// content: "oap:generative", the agent's writable region. A structural
	// type is NEVER published to the agent (VocabularySchema omits it) and
	// is never legal inside an agent-written fill (ResolveView refuses it),
	// so the only author of one is the page's own author. The oap: namespace
	// is the visible half of the same fact: nothing an agent is shown ever
	// starts with it.
	Structural bool

	// Check validates prop VALUES after the shape check (validateProps'
	// own decode is a TYPE check only — see its doc comment on why values
	// are otherwise left unenforced). nil means no value check, which is
	// every component today except the few whose values are
	// security-relevant. It receives the decoded props struct as a
	// pointer to Props's type (what reflect.New(t).Interface() produces),
	// so an implementation type-asserts to *XProps and returns an error
	// naming the offending field.
	Check func(props any) error
}

// ParamValue is one runtime parameter key a control drives.
type ParamValue struct {
	// Suffix is appended to the declared parameter name, after a ".", to form
	// the runtime key. Empty means the declared name IS the key.
	Suffix string

	// ValueProp names the prop carrying this key's declared default value —
	// "value" on ap:select, "from"/"to" on ap:daterange. A control whose
	// declaration sets it starts with that value in the parameter map, which
	// is what makes a Tier-0 UI resolve on first paint with no interaction.
	ValueProp string
}

// ParamKey returns the runtime key v contributes for a control that declared
// the parameter name `name`.
func (v ParamValue) ParamKey(name string) string {
	if v.Suffix == "" {
		return name
	}
	return name + "." + v.Suffix
}

// Key returns the registration key. It exists so the registry can be
// constructed with Component.Key as its keyOf function.
func (c Component) Key() string { return c.Type }
