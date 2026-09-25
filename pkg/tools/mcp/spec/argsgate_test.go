package spec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// TestRefusesEveryArgument states the one rule three places depend on: the
// argument gate is fail-closed, so an empty allowedFields is "deny every
// argument", not "allow anything". Only unconstrainedArgs opts out.
func TestRefusesEveryArgument(t *testing.T) {
	cases := []struct {
		name          string
		allowedFields []string
		unconstrained bool
		want          bool
	}{
		{name: "neither set: every argument is refused", want: true},
		{name: "allowedFields listed: those arguments are accepted", allowedFields: []string{"query"}},
		{name: "unconstrainedArgs set: free-form arguments are accepted", unconstrained: true},
		{name: "both set: unconstrainedArgs still accepts", allowedFields: []string{"query"}, unconstrained: true},
		{name: "empty (not nil) allowedFields is still a refusal", allowedFields: []string{}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mcpspec.RefusesEveryArgument(tc.allowedFields, tc.unconstrained))
		})
	}
}
