// DefinitionError is the vocabulary Decision.Definition speaks. It lives here,
// in the generic pipeline package, so that a hook can report a broken
// definition and a consumer can act on one without either importing the
// other's domain: the mcp_trust hook builds one out of a CEL failure, the
// runner turns one into operator-facing monitoring copy, and neither has to
// know the other exists.

package pipeline

// DefinitionError describes a piece of an agent's own definition that could
// not be evaluated, in terms an operator reading an alert can act on.
//
// The fields are deliberately plain strings rather than typed references to
// CRDs or specs. A hook near the failure knows exactly which rule broke and in
// which artifact; encoding that as domain types would drag those types into
// every consumer, and every consumer would then flatten them back to strings
// to render. The hook does the flattening once, at the point where it still
// knows what the pieces mean.
type DefinitionError struct {
	// Subject names what is broken, as an operator would refer to it — the
	// tool and server whose spec failed, not a Go symbol or a struct field.
	Subject string
	// Locus is the position within that definition: a rule path such as
	// "constraints[1]". Empty when the failure is not positional.
	Locus string
	// Detail is the authored text that failed — the CEL expression, the field
	// name — quoted verbatim so the operator can find it by searching. It is
	// configuration, never caller data, which is what makes it safe to put in
	// a monitoring event and unnecessary to redact.
	Detail string
	// Err is the underlying cause.
	Err error
}

func (e *DefinitionError) Error() string {
	msg := e.Subject
	if e.Locus != "" {
		msg += " " + e.Locus
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *DefinitionError) Unwrap() error { return e.Err }
