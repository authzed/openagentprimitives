package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

// Env returns a CEL environment for toolspec constraints — a single
// `call`-shaped map variable plus every helper registered with Scope
// containing ScopeToolspec. New helpers are added via Register() in an
// init() (see helpers.go, time.go); both this function and the LLM
// authoring prompt's HelperDocs(ScopeToolspec) read the same registry,
// so they never drift.
func Env() (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("call", cel.MapType(cel.StringType, types.DynType)),
		// config is the installer-bound AgentClass.spec.config, exposed to
		// toolspec constraints as a second root so a constraint can gate on
		// operator-supplied values (e.g. an allowlist) without the value being
		// baked into the expression. Populated from the session's stamped
		// config by the toolcall controller; empty (but non-nil) when a caller
		// supplies none, so a `config.X` reference fails closed rather than
		// seeing a null root.
		cel.Variable("config", cel.MapType(cel.StringType, types.DynType)),
	}
	opts = append(opts, envOptionsFor(ScopeToolspec)...)
	return cel.NewEnv(opts...)
}
