package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"

	celgo "github.com/google/cel-go/cel"
)

// TestRegistry_BuiltinsRegistered asserts the helpers existing CEL code
// depends on (path.isUnder, host.matches, glob.match, semver.satisfies,
// the call.* member overloads) are registered at package init so other
// callers can discover them via HelpersFor.
func TestRegistry_BuiltinsRegistered(t *testing.T) {
	wantToolspec := []string{
		"path.isUnder",
		"host.matches",
		"host.of",
		"glob.match",
		"semver.satisfies",
		"call.flag",
		"call.hasFlag",
		"call.hasEnv",
	}
	got := names(HelpersFor(ScopeToolspec))
	for _, want := range wantToolspec {
		assert.Contains(t, got, want, "ScopeToolspec missing helper %q", want)
	}
	// call.* member overloads are nonsense for the MCP env (no `call`
	// variable). The generic helpers (host/glob/semver) SHOULD still
	// surface in MCP — they operate on argument values, not on `call`.
	wantMCP := []string{"host.matches", "glob.match", "semver.satisfies"}
	dontWantMCP := []string{"call.flag", "call.hasFlag", "call.hasEnv"}
	gotMCP := names(HelpersFor(ScopeMCP))
	for _, want := range wantMCP {
		assert.Contains(t, gotMCP, want, "ScopeMCP missing helper %q", want)
	}
	for _, no := range dontWantMCP {
		assert.NotContains(t, gotMCP, no, "ScopeMCP should NOT include %q", no)
	}
}

// TestRegistry_HelperDocs_NonEmpty asserts the LLM-facing docs include the
// helper name and signature for each registered helper. The toolspec
// authoring prompt and the MCP authoring prompt both splice these in.
func TestRegistry_HelperDocs_NonEmpty(t *testing.T) {
	docs := HelperDocs(ScopeToolspec)
	assert.Contains(t, docs, "path.isUnder", "ScopeToolspec docs missing path.isUnder")
	assert.Contains(t, docs, "semver.satisfies", "ScopeToolspec docs missing semver.satisfies")
	mcpDocs := HelperDocs(ScopeMCP)
	assert.NotContains(t, mcpDocs, "call.hasFlag", "ScopeMCP docs should NOT include call.* helpers")
	assert.Contains(t, mcpDocs, "host.matches", "ScopeMCP docs missing host.matches")
}

// TestRegistry_Register_AppendsToScopedDocs asserts a runtime-registered
// helper appears in both HelpersFor and HelperDocs.
func TestRegistry_Register_AppendsToScopedDocs(t *testing.T) {
	// Register a unique helper for this test only; the registry is
	// process-wide, so use a name unlikely to collide.
	name := "__test_registry_register_appends"
	Register(Helper{
		Name:      name,
		Signature: name + "() -> bool",
		Doc:       "test helper",
		Scope:     ScopeToolspec,
		EnvOptions: []celgo.EnvOption{
			celgo.Function(name,
				celgo.Overload(name,
					[]*celgo.Type{},
					celgo.BoolType,
				),
			),
		},
	})
	assert.Contains(t, names(HelpersFor(ScopeToolspec)), name, "Register: helper not in HelpersFor(ScopeToolspec)")
	assert.Contains(t, HelperDocs(ScopeToolspec), name, "Register: helper not in HelperDocs(ScopeToolspec)")
	assert.NotContains(t, names(HelpersFor(ScopeMCP)), name, "Register: toolspec-scoped helper leaked into ScopeMCP")
}

func names(hs []Helper) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}
