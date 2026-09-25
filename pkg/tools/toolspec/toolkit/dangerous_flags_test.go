package toolkit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// A flag that hands the caller arbitrary command execution — git's `-c`, which
// sets one-shot config and so reaches core.fsmonitor, core.sshCommand,
// core.hooksPath and url.<base>.insteadOf — cannot be described as dangerous in
// any machine-readable way today. The obligation to constrain it lives in a
// YAML comment, and two of the four shipped git toolspecs do not meet it.
//
// RequiresConstraint is that declaration: the toolkit says which flags a spec
// must constrain before admitting a subcommand they can reach.
func TestFlagsRequiringConstraint_ReportsTheKeyASpecMustConstrain(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name: "demotool",
		GlobalFlags: []toolkit.Flag{
			// Short-only, exactly git's -c: the addressing key is the short
			// name, and a rule keyed on the long name would silently never fire.
			{Short: "c", Type: "stringList", RequiresConstraint: true},
			{Long: "verbose", Short: "v", Type: "bool"},
		},
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"status"}, Flags: []toolkit.Flag{
				{Long: "porcelain", Type: "bool"},
				{Long: "exec", Type: "string", RequiresConstraint: true},
			}},
		},
	}

	got := tk.FlagsRequiringConstraint()
	assert.ElementsMatch(t, []string{"c", "exec"}, got,
		"both the global short-only flag and the per-subcommand one must be reported, by their addressing key")
}

// The common case must stay quiet: a toolkit declaring nothing dangerous
// reports nothing, so the rule costs existing toolkits nothing.
func TestFlagsRequiringConstraint_IsEmptyWhenNothingIsMarked(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name:        "demotool",
		GlobalFlags: []toolkit.Flag{{Long: "verbose", Short: "v", Type: "bool"}},
	}
	require.Empty(t, tk.FlagsRequiringConstraint())
}

// A flag is reported once even when several subcommands declare it, so a
// caller can use the result as a set without deduplicating.
func TestFlagsRequiringConstraint_DeduplicatesAcrossSubcommands(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name: "demotool",
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"a"}, Flags: []toolkit.Flag{{Long: "exec", RequiresConstraint: true}}},
			{Path: []string{"b"}, Flags: []toolkit.Flag{{Long: "exec", RequiresConstraint: true}}},
		},
	}
	assert.Equal(t, []string{"exec"}, tk.FlagsRequiringConstraint())
}
