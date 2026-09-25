// This file is package parser_test (the external test package) rather than
// package parser: it drives the whole validator pipeline, and
// pkg/tools/toolspec/validator imports pkg/tools/toolspec/parser. An internal test file
// could not import it without an import cycle.
package parser_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/validator"
)

// catLikeToolkit mirrors the shape a real single-command toolkit takes: no
// subcommand token, one trailing variadic slot, and — crucially — NO
// afterDashDash on it, because `--` is a plain option terminator for such a
// binary. That is the commonest CLI shape there is, and it is the shape the
// LLM authoring prompt steers spec authors to constrain via
// call.positional[...].
func catLikeToolkit() *toolkit.Toolkit {
	return &toolkit.Toolkit{
		Name: "reader", Version: "1", ToolkitRevision: "r",
		Target: toolkit.Target{Binary: "reader"},
		Parser: toolkit.ParserConfig{Kind: "declarative"},
		Subcommands: []toolkit.Subcommand{{
			Path:       []string{},
			Positional: []toolkit.Positional{{Name: "files", Type: "stringList"}},
			Effects: toolkit.Effects{
				Reads: []string{"filesystem"}, Writes: []string{},
				Network:    toolkit.NetworkEffect{Destinations: []string{}},
				Filesystem: toolkit.FilesystemEffect{Paths: []string{}},
				Creds:      toolkit.CredsEffect{Required: []string{}, Writes: []string{}},
			},
		}},
	}
}

// workspaceOnlySpec constrains every value bound to the variadic slot to live
// under /work — the canonical "the agent may only read its own workspace"
// policy an operator writes over a file-reading tool.
func workspaceOnlySpec() *spec.Spec {
	return &spec.Spec{
		Name: "reader-workspace-only", Version: "1",
		Toolkit:          spec.ToolkitRef{Name: "reader", Revision: "r"},
		AllowSubcommands: []string{""},
		Constraints: []spec.Constraint{{
			CEL:     `call.positional["files"].all(f, path.isUnder(f, "/work"))`,
			Message: "files must live under /work",
		}},
	}
}

// TestCheck_PostDashDashArgIsConstrained is the regression test for the
// option-terminator constraint bypass: an argument smuggled after `--` reached
// the binary while never appearing in call.positional, so the operator's CEL
// constraint was evaluated against a DIFFERENT argument list than the one
// executed. toolkit.Positional.AfterDashDash's doc declares that impossible
// ("Post-`--` arguments are ALWAYS bound to some slot"); before the fix, an
// unmarked trailing variadic made it false.
//
// The two assertions are deliberately paired: the violating argument really is
// in the argv the runner executes, AND the decision must therefore deny. A
// test that only checked the deny could be satisfied by a parser that dropped
// the argument from argv as well — which would be a different (also wrong)
// behavior.
func TestCheck_PostDashDashArgIsConstrained(t *testing.T) {
	argv := []string{"/work/a.txt", "--", "/etc/shadow"}

	dec, err := validator.Check(catLikeToolkit(), workspaceOnlySpec(), validator.Invocation{
		Command: "reader",
		Argv:    argv,
	})
	require.NoError(t, err, "Check must not report an internal error")
	require.NotNil(t, dec.Parsed, "Parsed must be populated once parse succeeds")

	// Precondition: the argv handed to the binary is the argv we passed in —
	// `--` is an option terminator, so the binary reads /etc/shadow.
	require.Equal(t, argv, dec.Parsed.Argv, "Parsed.Argv is executed verbatim")

	// Fact 1: the post-`--` argument must be visible to the constraint surface.
	assert.Contains(t, dec.Parsed.Positional["files"], "/etc/shadow",
		"post-`--` argument must bind to the variadic slot, not vanish from call.positional")

	// Fact 2: with it bound, the workspace constraint must deny.
	assert.False(t, dec.Allow, "constraint must deny an argument the binary receives")
	if assert.NotNil(t, dec.FailedOn, "a denied decision names the failing check") {
		assert.Equal(t, "constraints[0]", dec.FailedOn.Path, "FailedOn.Path")
	}
}

// TestCheck_PostDashDashArgStillAllowedWhenCompliant is the other half of the
// pair: binding the tail must not turn a legitimate `--`-terminated call into
// a deny. Without this, "bind everything" and "reject every tail" would be
// indistinguishable.
func TestCheck_PostDashDashArgStillAllowedWhenCompliant(t *testing.T) {
	dec, err := validator.Check(catLikeToolkit(), workspaceOnlySpec(), validator.Invocation{
		Command: "reader",
		Argv:    []string{"/work/a.txt", "--", "/work/b.txt"},
	})
	require.NoError(t, err, "Check must not report an internal error")
	require.NotNil(t, dec.Parsed, "Parsed must be populated once parse succeeds")

	assert.Equal(t, []string{"/work/a.txt", "/work/b.txt"}, dec.Parsed.Positional["files"],
		"both sides of `--` bind to the variadic slot, in argv order")
	assert.True(t, dec.Allow, "a compliant `--`-terminated call must still be allowed; reason=%q", dec.Reason)
}
