package authz

import (
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
)

// triggerInstanceEnv builds the CEL env for AuthzSlot.TriggerInstance
// expressions.
//
// It declares exactly two variables: `event` (the webhook event name, a
// string) and `payload` (the delivery body, dyn). Nothing else is true yet at
// the point this expression runs. TriggerInstance evaluates at session
// mint, before a session exists — there is no `args` (no tool call has
// happened), no `result` (nothing has been called), and no `session` (the
// session this would bind into is what's being minted). Widening this env to
// match relwrites' or expr.go's args/result/item/session/call would let an
// operator write an expression that compiles today and panics or silently
// evaluates against a nil binding the day mint-time ordering changes — so the
// env states, in its shape, exactly what a trigger delivery is: an event name
// and a body, nothing more.
//
// The expression itself is operator-authored (it ships in the AgentClass
// CRD, not in the inbound request), evaluated over a payload that already
// passed the channel kind's HMAC verification before this code ever sees it.
// CEL's own guarantees (total, no I/O, no loops, cost-bounded via
// celbudget) are what make running an operator-authored expression over a
// verified-but-still-untrusted-content body acceptable at all.
func triggerInstanceEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("event", cel.StringType),
		cel.Variable("payload", cel.DynType),
	)
}

// CompileTriggerInstanceExpr compiles a slot's TriggerInstance expression.
// The AgentClass controller calls this at admission — a slot whose
// expression fails to compile here refuses the class as not-ready, the same
// way any other malformed slot declaration does. Output type is left
// unconstrained (dyn) at compile time: the expression legitimately branches
// on `event` and different branches may return different literal types
// (e.g. `"" `), so the string requirement is enforced at eval time instead,
// by EvalTriggerInstance.
func CompileTriggerInstanceExpr(expr string) (cel.Program, error) {
	env, err := triggerInstanceEnv()
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

// EvalTriggerInstance evaluates prg (compiled by CompileTriggerInstanceExpr)
// against one verified delivery's event name and raw payload bytes, and
// returns the resource id it yields.
//
// Fail-closed on every path other than "compiled cleanly to a non-empty
// string": an unparseable payload, an eval error (including a reference to a
// key the payload doesn't have), a non-string result, and an empty string all
// return an error rather than a zero value a caller might mistake for "no
// binding intended." A slot's TriggerInstance is authoritative for that slot
// (see AuthzSlot.TriggerInstance's doc comment) — silently binding nothing on
// a malformed result would be indistinguishable from a class that correctly
// declared no trigger binding at all, so the caller must be able to tell the
// two apart from the returned error.
func EvalTriggerInstance(prg cel.Program, event string, payload []byte) (string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("trigger instance: payload: %w", err)
	}
	v, _, err := prg.Eval(map[string]any{"event": event, "payload": decoded})
	if err != nil {
		return "", fmt.Errorf("trigger instance: eval: %w", err)
	}
	s, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("trigger instance: eval: expected string, got %T", v.Value())
	}
	if s == "" {
		return "", fmt.Errorf("trigger instance: eval: empty string result")
	}
	return s, nil
}
