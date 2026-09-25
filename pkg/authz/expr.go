// Package authz CEL evaluator.
//
// This file provides Compile/Eval helpers for the two CEL slots in the
// slice-4 permission model:
//
//   - `permissionVariants[].when` — a bool expression selecting a variant.
//   - `permission.check.resourceIDExpr` (and the variant-level analog) — a
//     string expression extracting a SpiceDB resource ID from the tool-call
//     args.
//
// The shared CEL env binds three top-level variables:
//
//   - `args`   — the tool-call argument map (used by both `when` and
//     `resourceIDExpr`).
//   - `result` — the tool-call response payload (used by post-effect
//     evaluators; bound to nil here so expressions that reference it stay
//     compilable, but `when` / `resourceIDExpr` are evaluated pre-call).
//   - `item`   — per-iteration binding for ForEach post-effects (bound to
//     nil here for the same reason).
//
// The `spicedb_user_id(email) -> string` function is registered via
// spicedbUserIDLib in cel_funcs.go and is a thin wrapper over
// identity.Principal.Canonical() so spec authors can reference principals by email
// without leaking the base64-lowercase encoding.
package authz

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// celEnv builds the shared CEL env used by CompileBool / CompileString.
// Variables not consumed by a given expression are simply ignored — CEL
// tolerates unused bindings at eval time.
func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("args", cel.DynType),
		cel.Variable("result", cel.DynType),
		cel.Variable("item", cel.DynType),
		spicedbUserIDFunction(),
		// String helpers (split, join, lowerAscii, substring, replace, …).
		//
		// A resourceIDExpr exists to derive an authorizable id from a call's own
		// arguments, and those arguments are strings that need taking apart: a gh
		// endpoint carries its repository in a path segment, and startsWith can
		// only TEST for it, never extract it. Without this the gh api guard was an
		// `undeclared reference to 'split'` — an expression that compiled to
		// nothing and could therefore never authorize a call, not even a
		// well-formed one.
		//
		// It grants no new capability class. CEL remains total — no loops, no I/O,
		// no way to reach past `args` — and an unknown identifier is still a
		// COMPILE error, which pkg/authz/expr_strings_test.go pins alongside the
		// helpers themselves.
		ext.Strings(),
	)
}

// CompileBool compiles a CEL expression expected to yield a bool. The
// type-check enforces the result type at compile time so a misspelled
// variant `when` clause fails fast at spec load rather than silently
// returning a falsey non-bool at eval time.
func CompileBool(expr string) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("CEL env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expr, iss.Err())
	}
	out := ast.OutputType()
	if out != cel.BoolType && out.TypeName() != "dyn" {
		return nil, fmt.Errorf("CEL: expression %q must return bool, got %s", expr, out)
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("CEL program: %w", err)
	}
	return prg, nil
}

// CompileString compiles a CEL expression expected to yield a string.
// The result type is left as dyn at compile time because resourceIDExpr
// patterns often pull a field out of a nested map and the type-checker
// resolves them as dyn; EvalString does the concrete bool/string coercion
// at eval time.
func CompileString(expr string) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("CEL env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL compile %q: %w", expr, iss.Err())
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("CEL program: %w", err)
	}
	return prg, nil
}

// EvalBool runs prg with the given args map. `result` and `item` are
// bound to nil since `when` / `resourceIDExpr` are evaluated pre-call.
func EvalBool(prg cel.Program, args map[string]any) (bool, error) {
	v, _, err := prg.Eval(map[string]any{"args": args, "result": nil, "item": nil})
	if err != nil {
		return false, fmt.Errorf("CEL eval: %w", err)
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL eval: expected bool, got %T", v.Value())
	}
	return b, nil
}

// EvalString runs prg with the given args map and returns the string
// result. An empty string is treated as an error because a missing /
// empty resource ID would produce an unresolvable SpiceDB check; failing
// loudly at eval time is the right call here (fail-closed).
func EvalString(prg cel.Program, args map[string]any) (string, error) {
	v, _, err := prg.Eval(map[string]any{"args": args, "result": nil, "item": nil})
	if err != nil {
		return "", fmt.Errorf("CEL eval: %w", err)
	}
	s, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("CEL eval: expected string, got %T", v.Value())
	}
	if s == "" {
		return "", fmt.Errorf("CEL eval: empty string result")
	}
	return s, nil
}

// spicedbUserIDFunction registers the `spicedb_user_id(email) -> string`
// CEL function. The library is defined in cel_funcs.go to keep the
// types/ref imports out of this file.
func spicedbUserIDFunction() cel.EnvOption { return cel.Lib(spicedbUserIDLib{}) }

// SpiceDBUserIDFunction is the exported form of spicedbUserIDFunction, for CEL
// environments outside this package that must accept the same expressions.
//
// Every env that evaluates user-authored CEL over an email needs it: it is the
// only supported way to turn an email into a SpiceDB user id, so an expression
// like `"user:" + spicedb_user_id(item.email)` is unrepresentable without it.
//
// It is exported because pkg/authz/relwrites builds its own env (it declares
// session/call bindings this package deliberately does not) and, in forking,
// silently lost this function — so every MCPServer relationship-write
// expression deriving a subject from an email failed to compile at runtime.
// Any future env that forks likewise must include this option rather than
// re-deriving the function.
func SpiceDBUserIDFunction() cel.EnvOption { return spicedbUserIDFunction() }
