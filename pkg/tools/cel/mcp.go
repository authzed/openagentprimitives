package cel

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"google.golang.org/protobuf/types/known/structpb"
)

// MCPEnv returns the CEL environment used for MCPServer arg constraints.
// Unlike Env, which scopes a `call`-shaped map for SpiceboxToolspec, MCP
// constraints scope `args` as a google.protobuf.Struct so they match the JSON
// arguments handed to tools/call. Every helper registered under ScopeMCP is
// added on top.
//
// One source of truth for both compile-time type checking
// (pkg/tools/mcp/spec.Compile) and runtime evaluation
// (pkg/tools/mcp/validator.Check); the authoring prompt reads the same registry
// via HelperDocs(ScopeMCP).
func MCPEnv() (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("args", cel.ObjectType("google.protobuf.Struct")),
		cel.Types(&structpb.Struct{}),
	}
	opts = append(opts, envOptionsFor(ScopeMCP)...)
	return cel.NewEnv(opts...)
}

// MCPCompileExpr type-checks a single MCP constraint expression against
// MCPEnv. Returns nil on success; the returned error wraps the CEL
// issue with no extra framing (callers add tool/constraint indices).
func MCPCompileExpr(expr string) error {
	env, err := MCPEnv()
	if err != nil {
		return fmt.Errorf("cel env: %w", err)
	}
	if _, iss := env.Compile(expr); iss != nil && iss.Err() != nil {
		return iss.Err()
	}
	return nil
}
