package cel

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/google/cel-go/cel"
)

// Compile returns a compiled CEL program for expr. Callers should compile once (at spec load)
// and re-use the Program for each invocation.
//
// The program carries celbudget.ProgramOptions, so every evaluation is bounded
// no matter which call site reaches it -- see that package for why CEL here is
// effectively untrusted input.
func Compile(expr string) (cel.Program, error) {
	env, err := Env()
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", expr, iss.Err())
	}
	prog, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	return prog, nil
}

// EvalBool evaluates prog against a call-shaped map and a config map, coercing
// the result to bool. config is the installer-bound AgentClass.spec.config
// exposed to toolspec constraints as the `config` root; pass an empty (non-nil)
// map when a caller has no config so a `config`-referencing expression errors
// (fail-closed) rather than silently seeing a null root.
func EvalBool(prog cel.Program, call, config map[string]any) (bool, error) {
	if config == nil {
		config = map[string]any{}
	}
	out, _, err := prog.Eval(map[string]any{"call": call, "config": config})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", out.Value())
	}
	return b, nil
}
