package spec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// A spec that admits subcommands reachable with a RequiresConstraint flag and
// says nothing about that flag is the defect this rule exists for: two of the
// four shipped git toolspecs were in exactly this state, one of them
// advertised as read-only while `git -c core.fsmonitor=<cmd> status` gave a
// shell.
func TestValidateRequiredFlagConstraints_RefusesAnUnconstrainedDangerousFlag(t *testing.T) {
	sp := &spec.Spec{
		Name:             "git-readonly",
		AllowSubcommands: []string{"status", "diff"},
		Constraints: []spec.Constraint{
			{CEL: `call.positional.size() <= 2`, Message: "at most two paths"},
		},
	}

	err := spec.ValidateRequiredFlagConstraints([]string{"c"}, sp)
	require.Error(t, err, "a spec admitting -c without constraining it must be refused")
	assert.Contains(t, err.Error(), "c",
		"the message must name the flag, or an author cannot act on it")
}

// The rule is satisfied by a constraint that actually addresses the flag —
// the shape codebot and reviewbot already use.
func TestValidateRequiredFlagConstraints_AcceptsAConstrainedFlag(t *testing.T) {
	sp := &spec.Spec{
		Name:             "git-rw",
		AllowSubcommands: []string{"status", "commit", "push"},
		Constraints: []spec.Constraint{
			{
				CEL:     `!call.hasFlag('c') || call.flags['c'].all(v, v.startsWith('user.name=') || v.startsWith('user.email='))`,
				Message: "-c may only set the committer identity",
			},
		},
	}
	require.NoError(t, spec.ValidateRequiredFlagConstraints([]string{"c"}, sp))
}

// A toolkit that declares nothing dangerous costs every spec against it
// nothing, which is what keeps the rule from becoming a migration.
func TestValidateRequiredFlagConstraints_IsAnAllowWhenNothingIsRequired(t *testing.T) {
	sp := &spec.Spec{Name: "gh-ro", AllowSubcommands: []string{"pr view"}}
	require.NoError(t, spec.ValidateRequiredFlagConstraints(nil, sp))
}

// Every required flag has to be covered, not just one of them — otherwise a
// spec constraining the first dangerous flag would be admitted while leaving
// the second wide open.
func TestValidateRequiredFlagConstraints_RequiresEveryFlagToBeCovered(t *testing.T) {
	sp := &spec.Spec{
		Name:             "partly-constrained",
		AllowSubcommands: []string{"status"},
		Constraints: []spec.Constraint{
			{CEL: `!call.hasFlag('c') || call.flags['c'].all(v, v.startsWith('user.name='))`},
		},
	}

	err := spec.ValidateRequiredFlagConstraints([]string{"c", "exec"}, sp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec")
	assert.NotContains(t, err.Error(), "'c'",
		"the covered flag must not be reported; only the uncovered one is actionable")
}

// A spec admitting no subcommands can reach no flag, so it has nothing to
// constrain. Refusing it would make the rule fire on specs that cannot
// exercise the risk.
func TestValidateRequiredFlagConstraints_IgnoresASpecThatAdmitsNothing(t *testing.T) {
	sp := &spec.Spec{Name: "empty"}
	require.NoError(t, spec.ValidateRequiredFlagConstraints([]string{"c"}, sp))
}
