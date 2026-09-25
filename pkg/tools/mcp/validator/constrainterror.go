package validator

// ConstraintError is the typed error Check returns when a tool's authored CEL
// constraints cannot be EVALUATED — the expression does not compile, its
// program cannot be built, evaluation errors, or it yields a non-bool.
//
// This is distinct from a constraint that evaluates cleanly to false: that is
// a Decision with Allow=false and a FailedOn path, the policy working as
// written. A ConstraintError means the policy could not be consulted at all,
// which is an authoring defect in the MCPServer spec and is nobody's fault at
// the call site — no argument value and no caller identity gets past it.
//
// It is typed rather than an fmt.Errorf so the fields survive the trip to
// whoever can fix it. The locus and the expression are what an operator needs
// in order to find the broken rule; flattened into prose they can only be
// recovered by parsing a message that is free to change.
type ConstraintError struct {
	// Path is the rule's position in the tool's spec, e.g. "constraints[1]".
	Path string
	// Stage is where evaluation broke: "compile", "program", "eval", or
	// "result" (it evaluated, but not to a bool).
	Stage string
	// CEL is the authored expression verbatim. Safe to show an operator — it
	// is configuration, not caller data — and deliberately NOT shown to a
	// viewer, who neither wrote it nor can act on it.
	CEL string
	// Err is the underlying failure from cel-go.
	Err error
}

func (e *ConstraintError) Error() string {
	return e.Path + " " + e.Stage + ": " + e.Err.Error()
}

func (e *ConstraintError) Unwrap() error { return e.Err }
