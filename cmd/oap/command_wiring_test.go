package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds the claims only the assembled tree can carry. Each command
// family's own package proves what its commands DO; nothing there can prove
// that anybody hung them off the root, or compare two commands that live in
// different packages.

// buildRootForTest builds the real assembled command tree — the same one
// NewRootCmd hands to Execute in production — so a wiring test proves a
// command is reachable from the actual root, not from a locally-built stub
// that would pass either way.
func buildRootForTest(t *testing.T) *cobra.Command {
	t.Helper()
	return NewRootCmd()
}

// TestDirectoryCommandIsRegisteredOnTheRoot covers what directorycmd's own
// tests cannot: that `oap directory` is actually hung off the root. A
// subpackage cannot import package main (see CLAUDE.md's "Where code lives"),
// so this assertion — the one that resolves the REAL root command tree,
// deliberately not a package-local stub — has to live here rather than in
// cmd/oap/internal/directorycmd/root_test.go. directorycmd's own list/wizard
// tests already prove the command family works; this is the wiring
// assertion this repo has been bitten by three times: a feature complete in
// every package and reachable from nothing.
func TestDirectoryCommandIsRegisteredOnTheRoot(t *testing.T) {
	root := buildRootForTest(t)
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "directory" {
			found = true
		}
	}
	assert.True(t, found, "oap directory must be reachable or the feature ships unusable")
}

// find walks the tree from root down the given path of command names and
// returns the command it lands on.
func find(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	cur := NewRootCmd()
	for i, name := range path {
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == name {
				next = c
				break
			}
		}
		require.NotNilf(t, next, "`oap %s` must be registered", strings.Join(path[:i+1], " "))
		cur = next
	}
	return cur
}

// TestAgentChatCmd_Registered covers the one thing chatcmd's own tests cannot:
// that the subtree is actually hung off `oap agent`. chatcmd builds the command
// and can prove everything about it except that anybody asked for it.
func TestAgentChatCmd_Registered(t *testing.T) {
	assert.Equal(t, "chat", find(t, "agent", "chat").Name())
}

// TestIdentitySetupFlagsAreRegisteredOnBothCommands: the two commands share
// identitycmd.AddSetupFlags precisely so they cannot diverge, but nothing would
// notice a dropped call — the flag would simply be absent, and every test that
// drives the run functions directly passes its options as a struct, bypassing
// cobra entirely.
func TestIdentitySetupFlagsAreRegisteredOnBothCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		path []string
	}{
		{name: "oap identity setup", path: []string{"identity", "setup"}},
		{name: "oap agent setup-identity", path: []string{"agent", "setup-identity"}},
	} {
		cmd := find(t, tc.path...)
		t.Run(tc.name+" registers --non-interactive", func(t *testing.T) {
			f := cmd.Flags().Lookup("non-interactive")
			require.NotNil(t, f, "the flag the presenter's whole fail-closed path keys off")
			assert.Equal(t, "false", f.DefValue, "prompting stays the default")
		})
		t.Run(tc.name+" offers no way to answer a question from a flag", func(t *testing.T) {
			assert.Nil(t, cmd.Flags().Lookup("answer"),
				"every answer a credential flow asks for either IS a credential or mints one, "+
					"and a flag value lands in shell history and in the process table")
		})
	}
}

// walk visits every command in the assembled tree, root included, calling fn
// with the command and the path a user would type to reach it.
func walk(cmd *cobra.Command, path string, fn func(cmd *cobra.Command, path string)) {
	fn(cmd, path)
	for _, c := range cmd.Commands() {
		walk(c, strings.TrimSpace(path+" "+c.Name()), fn)
	}
}

// TestFlagSpellingIsConsistentAcrossTheTree is the guard the per-family
// packages cannot carry: a family only ever sees its own flags, so nothing
// there can notice one command spelling a flag --out where its sibling spells
// it --output, or a file-reading command that omits the -f every other one
// carries. The user notices, on the second command, having learned the first.
//
// The rules are asserted over the whole assembled tree rather than a list of
// command paths so a newly-added command is covered the day it is hung off the
// root, without anybody remembering to add it here.
func TestFlagSpellingIsConsistentAcrossTheTree(t *testing.T) {
	// Every flag name that has one canonical spelling, mapped to the shorthand
	// it must carry. A misspelling is listed with an empty shorthand and its
	// canonical name in bannedNames below.
	shorthands := map[string]string{
		"output": "o",
		"file":   "f",
	}
	// A name nobody may register, and what to use instead. These are spellings
	// a second command reached for when the first had already settled the
	// question.
	bannedNames := map[string]string{
		"out":      "output",
		"out-dir":  "output-dir",
		"filename": "file",
	}

	walk(NewRootCmd(), "oap", func(cmd *cobra.Command, path string) {
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if want, banned := bannedNames[f.Name]; banned {
				assert.Failf(t, "banned flag spelling",
					"`%s` registers --%s; this tree spells it --%s", path, f.Name, want)
			}
			if want, ok := shorthands[f.Name]; ok {
				assert.Equalf(t, want, f.Shorthand,
					"`%s --%s` must carry -%s, the shorthand every other command gives it",
					path, f.Name, want)
			}
		})
	})
}

// TestModeValuedDryRunFlagsAgreeAcrossTheTree: --dry-run is a mode on the
// cluster-mutating commands and a bool on the settings ones, which is fine —
// they preview different things. What is not fine is two mode-valued
// --dry-runs disagreeing about which modes exist, because the user learns the
// vocabulary from whichever command they ran first and carries it to the next.
//
// Asserting over usage strings rather than over a hand-listed pair of commands
// means a third mode-valued --dry-run is covered the day it is added.
func TestModeValuedDryRunFlagsAgreeAcrossTheTree(t *testing.T) {
	seen := map[string][]string{} // usage -> command paths offering it
	walk(NewRootCmd(), "oap", func(cmd *cobra.Command, path string) {
		f := cmd.Flags().Lookup("dry-run")
		if f == nil || f.Value.Type() != "string" {
			return // absent, or the bool spelling, which is a different flag
		}
		seen[f.Usage] = append(seen[f.Usage], path)
	})
	require.NotEmpty(t, seen, "the tree must still have the mode-valued --dry-run this is about")
	assert.Lenf(t, seen, 1,
		"every mode-valued --dry-run must advertise the same modes; got %v", seen)
}

// TestNoSubcommandShadowsAPersistentFlag: cobra resolves a flag against the
// command's own set before the inherited one, so a subcommand that registers a
// name the root already owns persistently silently retargets it. -n meaning
// one namespace on `oap session list` and a different one on its neighbour is
// not a preference; it is the same key opening two doors.
func TestNoSubcommandShadowsAPersistentFlag(t *testing.T) {
	root := NewRootCmd()
	var persistent []string
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		persistent = append(persistent, f.Name)
	})
	require.NotEmpty(t, persistent, "the root must define the global flags this test is about")

	walk(root, "oap", func(cmd *cobra.Command, path string) {
		if cmd == root {
			return
		}
		for _, name := range persistent {
			assert.Nilf(t, cmd.Flags().Lookup(name), // Flags() is the command's own set, not the inherited one.
				"`%s` registers its own --%s, shadowing the root's; read the global instead", path, name)
		}
	})
}
