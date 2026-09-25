// Tests of the runtime validator's surface as the MCP dispatch path
// consumes it. Each test synthesizes a one-tool spec, calls
// validator.Check, and asserts on Decision fields rather than an
// error return — denials are not errors, they are Decision.Allow=false
// with a populated FailedOn.
package mcp_test

import (
	"testing"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkOne is a small helper that wraps the boilerplate of building
// a one-tool spec + invoking validator.Check on it. All the tests in
// this file exercise per-tool Args behavior, so the synthetic tool
// name is hard-coded.
func checkOne(t *testing.T, args mcpspec.Args, payload map[string]any) (*validator.Decision, error) {
	t.Helper()
	sp := &mcpspec.Spec{
		Tools: []mcpspec.Tool{{Name: "test", Args: args}},
	}
	return validator.Check(sp, validator.Invocation{
		ToolName: "test",
		Args:     payload,
	})
}

// TestValidator_AllowedFieldsAndConstraints covers the four pass/deny
// shapes Check produces when fed an Args block: allowed-fields pass,
// allowed-fields deny on extra keys, CEL constraint pass, CEL constraint
// deny (with the configured Message surfaced in Reason).
func TestValidator_AllowedFieldsAndConstraints(t *testing.T) {
	cases := []struct {
		name    string
		args    mcpspec.Args
		payload map[string]any
		check   func(t *testing.T, d *validator.Decision)
	}{
		{
			name:    "allowedFields: payload subset of allow-list passes",
			args:    mcpspec.Args{AllowedFields: []string{"a", "b"}},
			payload: map[string]any{"a": 1, "b": 2},
			check: func(t *testing.T, d *validator.Decision) {
				assert.True(t, d.Allow, "Allow=false; reason=%q failedOn=%+v", d.Reason, d.FailedOn)
			},
		},
		{
			name:    "allowedFields: extra key denies on path=allowedFields with key in reason",
			args:    mcpspec.Args{AllowedFields: []string{"a"}},
			payload: map[string]any{"a": 1, "extra": 2},
			check: func(t *testing.T, d *validator.Decision) {
				assert.False(t, d.Allow, "expected Allow=false")
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, "allowedFields", d.FailedOn.Path)
				assert.Contains(t, d.Reason, "extra", "Reason should mention the extra key")
			},
		},
		{
			name: "constraint true: passes",
			// UnconstrainedArgs isolates the constraints phase from the now
			// fail-closed allowedFields phase (this case passes a free-form `q`).
			args: mcpspec.Args{UnconstrainedArgs: true, Constraints: []toolspec.Constraint{
				{CEL: "args.q.size() <= 10", Message: "too long"},
			}},
			payload: map[string]any{"q": "hi"},
			check: func(t *testing.T, d *validator.Decision) {
				assert.True(t, d.Allow, "Allow=false; reason=%q failedOn=%+v", d.Reason, d.FailedOn)
			},
		},
		{
			name: "constraint false: denies under constraints[] with Message in reason",
			args: mcpspec.Args{UnconstrainedArgs: true, Constraints: []toolspec.Constraint{
				{CEL: `args.q.size() <= 5`, Message: "too long"},
			}},
			payload: map[string]any{"q": "hello world"},
			check: func(t *testing.T, d *validator.Decision) {
				assert.False(t, d.Allow, "expected Allow=false")
				assert.Contains(t, d.Reason, "too long", "Reason should surface the constraint Message")
				require.NotNil(t, d.FailedOn)
				assert.Truef(t,
					len(d.FailedOn.Path) >= len("constraints[") &&
						d.FailedOn.Path[:len("constraints[")] == "constraints[",
					"FailedOn.Path = %q; want under constraints[]", d.FailedOn.Path)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := checkOne(t, tc.args, tc.payload)
			require.NoError(t, err, "Check")
			tc.check(t, d)
		})
	}
}

// TestValidator_HasGuardWorks asserts the `has(args.X) || ...` idiom
// short-circuits when X is absent (allow) and fires the right half when
// X is present with the offending value (deny). Kept as a single test so
// both invocations share the same Args block.
func TestValidator_HasGuardWorks(t *testing.T) {
	args := mcpspec.Args{UnconstrainedArgs: true, Constraints: []toolspec.Constraint{
		{CEL: `!has(args.q) || args.q != "bad"`},
	}}

	// Empty args: the has() guard short-circuits → allow.
	d, err := checkOne(t, args, map[string]any{})
	require.NoError(t, err, "Check empty")
	assert.True(t, d.Allow, "empty args: Allow=false; reason=%q", d.Reason)

	// args.q == "bad": the right half of the constraint fires → deny.
	d, err = checkOne(t, args, map[string]any{"q": "bad"})
	require.NoError(t, err, "Check bad")
	assert.False(t, d.Allow, "expected Allow=false for q=bad")
}
