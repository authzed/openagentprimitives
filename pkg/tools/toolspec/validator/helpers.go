package validator

import (
	celgo "github.com/google/cel-go/cel"

	ctcel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// CEL compile/eval thin wrappers — keep here so callers in this package
// don't depend directly on pkg/tools/cel.
func compileExpression(expr string) (celgo.Program, error) { return ctcel.Compile(expr) }
func evalBool(prog celgo.Program, call, config map[string]any) (bool, error) {
	return ctcel.EvalBool(prog, call, config)
}

// registerSensitiveValues registers env-derived sensitive values from the
// toolkit and spec into the redactor up-front (before parse). Flag values
// live on the parsed Call and are registered later by
// registerSensitiveFlagValues.
func registerSensitiveValues(r *redact.Redactor, tk *toolkit.Toolkit, sp *spec.Spec, inv Invocation) {
	for _, e := range tk.Env.Allowed {
		if !e.Sensitive {
			continue
		}
		if v, ok := inv.Env[e.Name]; ok && v != "" {
			r.RegisterSensitive(v, redact.Descriptor{
				Description: firstNonEmpty(e.Description, e.Name),
				Kind:        "env",
				Name:        e.Name,
			})
		}
	}
	for _, name := range sp.Sensitive.Env {
		if v, ok := inv.Env[name]; ok && v != "" {
			r.RegisterSensitive(v, redact.Descriptor{Description: name, Kind: "env", Name: name})
		}
	}
}

// registerSensitiveFlagValues walks the parsed call's flags and registers
// every value whose flag descriptor (toolkit-side) or spec.Sensitive.Flags
// entry marks it sensitive.
func registerSensitiveFlagValues(r *redact.Redactor, tk *toolkit.Toolkit, sp *spec.Spec, call *parser.Call) {
	mark := func(flags []toolkit.Flag) {
		for _, f := range flags {
			if !f.Sensitive {
				continue
			}
			// Short-only flags (empty Long) key call.Flags by short name.
			key := f.Long
			if key == "" {
				key = f.Short
			}
			v, ok := call.Flags[key]
			if !ok {
				continue
			}
			registerBoundStrings(r, v, redact.Descriptor{
				Description: firstNonEmpty(f.Description, key),
				Kind:        "flag",
				Name:        key,
			})
		}
	}
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if sameSlice(sc.Path, call.SubcommandPath) {
			mark(sc.Flags)
			break
		}
	}
	mark(tk.GlobalFlags)
	for _, name := range sp.Sensitive.Flags {
		if v, ok := call.Flags[name]; ok {
			registerBoundStrings(r, v, redact.Descriptor{Description: name, Kind: "flag", Name: name})
		}
	}
}

// registerSensitivePositionals registers the values bound to positional slots
// the spec lists under sensitive.positional.
//
// The field is declared on spec.Sensitive and on its CRD mirror
// (v1alpha1.ToolspecSensitive.Positional). Leaving it unread would give an
// operator who declared a positional sensitive no redaction anywhere — not in
// the Decision's parsed view, not in a denial reason, not in the trace — and no
// signal that the declaration was inert.
func registerSensitivePositionals(r *redact.Redactor, sp *spec.Spec, call *parser.Call) {
	for _, name := range sp.Sensitive.Positional {
		v, ok := call.Positional[name]
		if !ok {
			continue
		}
		registerBoundStrings(r, v, redact.Descriptor{Description: name, Kind: "positional", Name: name})
	}
}

// registerBoundStrings records every string a parsed slot carries, whatever
// shape the parser bound it in. A `stringList` flag and a variadic positional
// both bind []string, and the redaction walk cannot mask a value it was never
// told about, however thoroughly it walks the field holding it.
//
// String-shaped values ONLY, deliberately: a bool flag's parsed value is
// presence (`true`), and registering "true" would have the redactor replace
// that substring throughout the trace; an int flag's value is a number the
// wrapped CLI acts on. Marking either sensitive is a spec-authoring mistake.
// The empty string is guarded in redact.RegisterSensitive.
func registerBoundStrings(r *redact.Redactor, v any, d redact.Descriptor) {
	switch x := v.(type) {
	case string:
		r.RegisterSensitive(x, d)
	case []string:
		for _, s := range x {
			r.RegisterSensitive(s, d)
		}
	case []any:
		// A builtin parser may bind a heterogeneous list; register the strings
		// in it and leave the rest to the same reasoning as above.
		for _, e := range x {
			if s, ok := e.(string); ok {
				r.RegisterSensitive(s, d)
			}
		}
	}
}

// findSubcommand looks up a toolkit subcommand by exact path.
func findSubcommand(tk *toolkit.Toolkit, path []string) *toolkit.Subcommand {
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if len(sc.Path) != len(path) {
			continue
		}
		ok := true
		for j, p := range sc.Path {
			if path[j] != p {
				ok = false
				break
			}
		}
		if ok {
			return sc
		}
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sameSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
