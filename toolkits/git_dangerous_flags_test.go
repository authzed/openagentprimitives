package toolkits_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// git's `-c` sets one-shot config, which reaches core.fsmonitor,
// core.hooksPath, core.sshCommand and url.<base>.insteadOf — arbitrary command
// execution — and it OVERRIDES the GIT_CONFIG_COUNT/KEY/VALUE pinning the
// sandbox image applies. A spec that admits any subcommand and does not
// constrain it therefore grants a shell, however short its allowSubcommands
// list is.
//
// The toolkit must SAY so, or spec.ValidateRequiredFlagConstraints has nothing
// to enforce and the rule guards nothing.
func TestGitToolkitDeclaresDashCAsRequiringConstraint(t *testing.T) {
	tk := findToolkit(t, "git")
	require.Contains(t, tk.FlagsRequiringConstraint(), "c",
		"git -c must carry requiresConstraint, or an unconstrained spec is admitted silently")
}

// The end-to-end shape the rule exists for, exercised against the REAL git
// toolkit with package-local spec fixtures (never examples/, per AGENTS.md).
func TestGitToolkitRefusesASpecAdmittingDashCUnconstrained(t *testing.T) {
	required := findToolkit(t, "git").FlagsRequiringConstraint()

	unconstrained := &spec.Spec{
		Name: "demo-git-readonly",
		// The read-only shape: nothing here suggests a shell, which is
		// precisely why the omission survived review in a shipped example.
		AllowSubcommands: []string{"log", "diff", "show", "status", "blame"},
	}
	require.Error(t, spec.ValidateRequiredFlagConstraints(required, unconstrained),
		"a read-only-looking spec still reaches -c, so it must be refused")

	constrained := &spec.Spec{
		Name:             "demo-git-rw",
		AllowSubcommands: []string{"status", "commit", "push"},
		Constraints: []spec.Constraint{{
			CEL:     `!call.hasFlag('c') || call.flags['c'].all(v, v.startsWith('user.name=') || v.startsWith('user.email='))`,
			Message: "-c may only set the committer identity",
		}},
	}
	assert.NoError(t, spec.ValidateRequiredFlagConstraints(required, constrained),
		"bounding the value is what the rule asks for; it must then admit the spec")
}
