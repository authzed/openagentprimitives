package spec

// RefusesEveryArgument reports whether an argument gate configured with these
// two values denies EVERY argument a caller passes.
//
// The gate is fail-closed by design (validator.checkAllowedFields): an empty
// allowedFields is "deny every argument", never "allow anything", and the only
// opt-out is an explicit unconstrainedArgs. So a tool entry that sets neither
// is not under-specified — it is fully specified, and it refuses every call
// that carries an argument.
//
// A true answer is not by itself a fault: a tool that takes no arguments never
// presents one and works exactly as authored. It is a fault only where
// arguments ARE going to be sent, which is what each caller judges — the
// enforcement point (the argument gate), the CR lint that knows the UI's own
// args templates, and the workshop's spec check, all of which read the rule
// from here rather than restating it.
func RefusesEveryArgument(allowedFields []string, unconstrainedArgs bool) bool {
	return !unconstrainedArgs && len(allowedFields) == 0
}
