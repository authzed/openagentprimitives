package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// ghLike mirrors the shape of the real gh toolkit: a passthrough default with
// per-subcommand checks that differ in SEVERITY. Reading a repo and opening a
// pull request are not the same authorization question, and collapsing them is
// the defect these tests pin.
func ghLike() *toolkit.Toolkit {
	ro := authz.Permission{StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "read"}}
	ext := authz.Permission{StateImpact: authz.External,
		Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"}}
	pass := authz.Permission{StateImpact: authz.Passthrough}
	return &toolkit.Toolkit{
		Name:       "gh",
		Permission: &pass,
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"pr", "view"}, Permission: &ro},
			{Path: []string{"pr", "create"}, Permission: &ext},
			{Path: []string{"repo", "clone"}}, // no permission of its own
		},
	}
}

// A toolspec allowing several subcommands synthesizes ONE tool for the whole
// CLI, and that tool took the toolkit-level default — so every per-subcommand
// check declared in the toolkit was discarded before it reached the runtime.
// Observed live: an agent whose gh tools carry real github_repo checks
// enumerated a permission surface of ZERO.
func TestSandboxTool_exposesEachSubcommandsPermissionAsAVariant(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{
		BundleName: "gitlike", Suffix: "gh", Toolkit: ghLike(),
	})

	vs := tl.PermissionVariants()
	require.Len(t, vs, 2, "one variant per subcommand that declares a permission; repo clone declares none")

	byImpact := map[authz.StateImpact]*authz.PermissionCheck{}
	for _, v := range vs {
		byImpact[v.Check.StateImpact] = v.Check.Check
	}
	require.Contains(t, byImpact, authz.Readonly)
	require.Contains(t, byImpact, authz.External,
		"pr create is EXTERNAL — the most severe tier, and the one that must never auto-approve")
	assert.Equal(t, "github_repo", byImpact[authz.External].ResourceType)
	assert.Equal(t, "write", byImpact[authz.External].Permission)
}

// Dispatch must pick the permission for THIS argv, using the toolkit's own
// parser rather than re-deriving it. The parser already handles leading global
// flags; a hand-rolled match in an authorization path would be a second,
// divergent implementation of the same thing.
func TestSandboxTool_resolvesThePermissionForTheActualCall(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{
		BundleName: "gitlike", Suffix: "gh", Toolkit: ghLike(),
	})

	cases := []struct {
		name       string
		argv       []string
		wantImpact authz.StateImpact
	}{
		{"pr view is readonly", []string{"pr", "view", "123"}, authz.Readonly},
		{"pr create is external", []string{"pr", "create", "--title", "x"}, authz.External},
		{"a subcommand with no permission falls back to the toolkit default",
			[]string{"repo", "clone", "o/r"}, authz.Passthrough},
		{"unrecognized argv falls back to the toolkit default",
			[]string{"nonsense"}, authz.Passthrough},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tl.PermissionForCall(map[string]any{
				"args": toAny(tc.argv),
			})
			require.True(t, ok, "a sandbox tool can always answer for its own argv")
			assert.Equal(t, tc.wantImpact, got.StateImpact)
		})
	}
}

// The severity ordering that makes this worth doing: `gh pr create` must not be
// authorized as though it were `gh pr view`.
func TestSandboxTool_doesNotLetAReadDecideAWrite(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{BundleName: "gitlike", Suffix: "gh", Toolkit: ghLike()})

	read, _ := tl.PermissionForCall(map[string]any{"args": toAny([]string{"pr", "view"})})
	write, _ := tl.PermissionForCall(map[string]any{"args": toAny([]string{"pr", "create"})})

	assert.NotEqual(t, read.StateImpact, write.StateImpact)
	assert.Equal(t, "write", write.Check.Permission)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// The resource-id half. A check declaring resourceIDTemplate "{repo}" reads a
// NAMED arg, but a sandbox tool's arguments are an argv ARRAY — so the template
// could never resolve and every per-resource check on a sandbox tool failed
// with `template references arg "repo" which is not present`. Observed live.
//
// The parser already produces the named view (Call.Flags keyed by long name,
// Call.Positional by name); it was simply never handed to the check.
func TestSandboxTool_exposesParsedFlagsAsNamedArgs(t *testing.T) {
	tk := ghLike()
	// Give `pr view` the flag the real gh toolkit has, so the parse produces it.
	tk.Subcommands[0].Flags = []toolkit.Flag{{Long: "repo", Short: "R", Type: "string"}}

	tl := NewSandboxTool(SandboxOpts{BundleName: "gitlike", Suffix: "gh", Toolkit: tk})

	named, ok := tl.NamedArgs(map[string]any{
		"args": toAny([]string{"pr", "view", "--repo", "demo-org/demo-repo"}),
	})
	require.True(t, ok, "a sandbox tool can always render its own argv")
	assert.Equal(t, "demo-org/demo-repo", named["repo"],
		"this is the value resourceIDTemplate \"{repo}\" resolves against")
}

// Unparseable argv must not fabricate a named view: a check that then resolved
// against partial arguments would authorize against a resource the call never
// named.
func TestSandboxTool_refusesToGuessWhenArgvDoesNotParse(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{BundleName: "gitlike", Suffix: "gh", Toolkit: ghLike()})

	_, ok := tl.NamedArgs(map[string]any{"args": toAny([]string{"--nonsense-flag"})})
	assert.False(t, ok, "no named view is better than a partial one in an authz path")
}

// apiLike mirrors the real problem: ONE subcommand whose authority depends on an
// ARGUMENT, not on which subcommand it is. `gh api -X GET repos/o/n` reads;
// `gh api -X POST repos/o/n/pulls` opens a pull request. Same subcommand.
//
// The same shape covers `claude --dangerously-skip-permissions` (one root
// subcommand, authority set by a flag) and any CLI with a --dry-run.
func apiLike() *toolkit.Toolkit {
	pass := authz.Permission{StateImpact: authz.Passthrough}
	write := authz.Permission{StateImpact: authz.External,
		Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write",
			ResourceIDTemplate: "{repo}"}}
	read := authz.Permission{StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "read",
			ResourceIDTemplate: "{repo}"}}
	return &toolkit.Toolkit{
		Name:       "gh",
		Permission: &pass,
		Subcommands: []toolkit.Subcommand{{
			Path: []string{"api"},
			// Declared so the toolkit's parser can produce named args at all —
			// PermissionForCall resolves variants against PARSED args, so an
			// undeclared flag is invisible to the predicate.
			Positional: []toolkit.Positional{{Name: "endpoint", Type: "string", Required: true}},
			Flags:      []toolkit.Flag{{Long: "method", Short: "X", Type: "string"}},
			Permission: &write, // the fallback: an unknown method is the dangerous one
			PermissionVariants: []authz.PermissionVariant{
				{When: `!has(args.method) || args.method == "" || args.method == "GET"`, Check: read},
			},
		}},
	}
}

// PermissionForCall must resolve the ARGUMENT-level variant. Without this a
// subcommand has exactly one authority, so `gh api` had to be charged its
// most dangerous reading on every call — every GET routed through a human.
func TestPermissionForCall_resolvesAnArgumentLevelVariant(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{BundleName: "gitlike", Suffix: "gh", Toolkit: apiLike()})

	cases := []struct {
		name string
		args map[string]any
		want authz.StateImpact
	}{
		{name: "no method: a GET, so readonly", args: map[string]any{"args": []any{"api", "repos/o/n"}}, want: authz.Readonly},
		{name: "explicit GET: readonly", args: map[string]any{"args": []any{"api", "-X", "GET", "repos/o/n"}}, want: authz.Readonly},
		{name: "POST: falls through to the external fallback", args: map[string]any{"args": []any{"api", "-X", "POST", "repos/o/n/pulls"}}, want: authz.External},
		{name: "DELETE: fallback too — the list names what is SAFE, not what is dangerous", args: map[string]any{"args": []any{"api", "-X", "DELETE", "repos/o/n"}}, want: authz.External},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tl.PermissionForCall(tc.args)
			require.True(t, ok)
			assert.Equal(t, tc.want, got.StateImpact)
		})
	}
}

// Both readings must reach the permission SURFACE, or a phase cannot declare the
// cheap one and every plan has to ask for write to make a GET.
func TestSandboxTool_argumentVariantsAppearOnTheSurface(t *testing.T) {
	tl := NewSandboxTool(SandboxOpts{BundleName: "gitlike", Suffix: "gh", Toolkit: apiLike()})

	var impacts []authz.StateImpact
	for _, v := range tl.PermissionVariants() {
		impacts = append(impacts, v.Check.StateImpact)
	}
	assert.Contains(t, impacts, authz.Readonly, "the GET reading must be declarable in a plan")
	assert.Contains(t, impacts, authz.External, "so must the write reading")
}
