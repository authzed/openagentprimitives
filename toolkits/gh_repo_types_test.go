package toolkits_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// A running agent names a repository by URL — resolved locally, out of a call's
// own arguments, with no network round-trip. The forge names the same
// repository by a numeric id that survives a rename. Neither key can adopt the
// other's, so gh.yaml declares TWO objects and the directory sync writes an
// edge between them.
//
// github_repo_url is the URL-keyed alias every Check resolves against.
// github_repo is the forge-keyed type the sync writes, carrying GitHub's five
// collaborator roles; github_org and github_team are the containers those roles
// traverse.
func TestGhToolkit_DeclaresBothRepoTypes(t *testing.T) {
	gh := findToolkit(t, "gh")
	require.NotNil(t, gh.SpiceDBSchema, "the gh toolkit must ship the definitions its checks name")

	// The URL-keyed alias is STRUCTURED: its permissions are arrows, which
	// EmitSpicedbSchema writes verbatim from Expr, so it needs no RawZed — and
	// staying structured is what keeps it visible to
	// schema_coverage_test.go's both-directions cover check.
	assert.Contains(t, structuredNames(gh), "github_repo_url",
		"the type the checks resolve against must stay structured, or the coverage test stops seeing it")

	// The forge-keyed types are RawZed: subject-relations
	// (github_user#sole_user) and unions are shapes the structured form cannot
	// express.
	raw := rawZedDefinitionNames(gh)
	for _, want := range []string{"github_repo", "github_org", "github_team"} {
		assert.Contains(t, raw, want, "the directory sync writes %s; nothing else declares it", want)
	}
}

// Additive, and this is the property the whole traversal rests on: every
// github_repo_url permission unions the LOCAL owner grant with the forge
// traversal. A sync that is still earning trust — stale, half-written, or not
// running at all — must not be able to take away work that succeeds today.
func TestGhToolkit_RepoURLPermissionsUnionOwnerWithTheForge(t *testing.T) {
	gh := findToolkit(t, "gh")

	assert.Equal(t, "owner + repo->can_read", permExpr(t, gh, "github_repo_url", "read"))
	assert.Equal(t, "owner + repo->can_write", permExpr(t, gh, "github_repo_url", "write"))
	assert.Equal(t, "owner + repo->can_admin", permExpr(t, gh, "github_repo_url", "admin"))

	// create_repo is the deliberate exception: the repository does not exist on
	// the forge yet, so there is no forge permission to consult and an arm
	// arrowing into one could only ever contribute nothing.
	assert.Equal(t, "owner", permExpr(t, gh, "github_repo_url", "create_repo"),
		"a repository that does not exist yet has no forge permission to union in")
}

// The bridge is a written tuple, not an adopted key: github_repo_url#repo is
// the single relation that ties the URL the agent typed to the repository the
// forge enumerated. Without it every arrow above contributes nothing and the
// type collapses back to owner-only.
func TestGhToolkit_RepoURLBridgesToTheForgeKeyedType(t *testing.T) {
	gh := findToolkit(t, "gh")

	rels := map[string]string{}
	for _, r := range gh.SpiceDBSchema.Resources {
		if r.Name != "github_repo_url" {
			continue
		}
		for _, rel := range r.Relations {
			rels[rel.Name] = rel.SubjectType
		}
	}
	assert.Equal(t, "user", rels["owner"], "the local grant arm is written on a platform user")
	assert.Equal(t, "github_repo", rels["repo"],
		"the bridge must point at the forge-keyed type; pointing it anywhere else makes every arrow inert")
}

// Every check must name the URL-keyed type and carry the new transform. A check
// left on github_repo would resolve an id in the URL space and ask it against
// the forge space — a lookup that can only ever miss, silently, as a denial
// that looks like a missing grant.
//
// Stated over EVERY check the toolkit carries, variants included: `gh api`'s
// safe readings live in permissionVariants, and a variant left behind is the
// same defect on the widest call in the toolkit.
func TestGhToolkit_ChecksTargetTheURLKeyedType(t *testing.T) {
	gh := findToolkit(t, "gh")
	checks := checksOf(gh)
	require.NotEmpty(t, checks, "the walker must find the toolkit's checks, or this test asserts nothing")

	for _, c := range checks {
		if c.check.ResourceType == "github_repo" {
			t.Errorf("%s still names the forge-keyed type; its resolved id is a URL, "+
				"which no forge-keyed object will ever carry", c.where)
		}
		if c.check.ResourceType == "github_repo_url" {
			assert.Equal(t, []string{"github_repo_url_id"}, c.check.ResourceIDTransforms,
				"%s must key by base64url and must NOT also spicedb_escape: base64url is "+
					"already a legal object id, and escaping it again mangles it", c.where)
		}
	}
}

// The transform chain is only half the claim; the id it actually produces is
// the other half. These are the exact object ids a grant must be written on,
// spelled out rather than left to follow from the registry — the sentinels
// included, because `search` and `self` name no repository and must keep
// passing through unchanged rather than being folded into some encoded form.
func TestGhToolkit_ResolvesTheBase64URLIDAndLeavesSentinelsAlone(t *testing.T) {
	gh := findToolkit(t, "gh")

	for _, tc := range []struct {
		name string
		path []string
		args map[string]any
		want string
	}{
		{
			name: "a named repository keys by base64url of its canonical https URL",
			path: []string{"pr", "view"},
			args: map[string]any{"repo": "demo-org/demo-repo", "idOrUrl": "123"},
			want: "aHR0cHM6Ly9naXRodWIuY29tL2RlbW8tb3JnL2RlbW8tcmVwbw",
		},
		{
			name: "every spelling of one repository folds onto that same id",
			path: []string{"pr", "view"},
			args: map[string]any{"repo": "git@github.com:Demo-Org/Demo-Repo.git", "idOrUrl": "123"},
			want: "aHR0cHM6Ly9naXRodWIuY29tL2RlbW8tb3JnL2RlbW8tcmVwbw",
		},
		{
			name: "the search sentinel names no repository and passes through",
			path: []string{"search", "repos"},
			args: map[string]any{"query": "anything"},
			want: "search",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := findSubcommand(t, gh, tc.path)
			require.NotNil(t, sc.Permission)
			require.NotNil(t, sc.Permission.Check)

			got, err := authz.ResolveResourceID(*sc.Permission.Check, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The card label has to change with the key or the instance stops being
// identifiable: url_path on an encoded id finds no host and derives nothing, so
// every approval card falls back to printing the type name — the exact
// regression gh.yaml's own comment records from when ids were bare OWNER/NAME
// pairs.
func TestGhToolkit_RepoURLDisplayDecodesTheEncodedID(t *testing.T) {
	gh := findToolkit(t, "gh")

	for _, r := range gh.SpiceDBSchema.Resources {
		if r.Name != "github_repo_url" {
			continue
		}
		require.NotNil(t, r.Display, "without a display every card prints the wire type name")
		assert.Equal(t, "b64url_path", r.Display.Label,
			"the ids are base64url now; url_path would parse no host and derive nothing")
		assert.Equal(t, "github", r.Display.Icon)
		return
	}
	t.Fatal("github_repo_url is not declared")
}

// The roles the directory sync writes take github_user#sole_user, never #user.
// #user is minted for EVERY attested binding — two platform subjects claiming
// one GitHub account is a normal case the reconciler reports rather than
// vetoes — and it is scoped to agentsession membership by an invariant test in
// pkg/authz/guardian/schema. Repository access is durable, not session-scoped,
// so it hangs off the binding that exists only while exactly one subject claims
// the account. A role widened to #user would hand a second claimant the first's
// repository authority.
func TestGhToolkit_RepoRolesTakeSoleUserOnly(t *testing.T) {
	gh := findToolkit(t, "gh")
	raw := gh.SpiceDBSchema.RawZed
	require.NotEmpty(t, raw)

	assert.NotContains(t, raw, "github_user#user",
		"a repo role typed github_user#user confers the first claimant's repository "+
			"authority on a colliding second claimant")
	assert.Contains(t, raw, "github_user#sole_user")

	// The wildcard arm the connectors prototype carries is deliberately absent.
	// pttagmint intersects raw subject ids as opaque strings, so `user:*`
	// arrives as the literal two-character id `*` and matches nobody: a public
	// repository's memory would resolve to an audience of no one while every
	// permission check on it still said yes.
	assert.NotContains(t, raw, "user:*",
		"a wildcard reader arm reads as a literal id downstream and resolves to an audience of nobody")
}

// namedCheck is one PermissionCheck the toolkit carries, with where it came
// from — a toolkit's checks live in three places and a failure naming only the
// type is not actionable.
type namedCheck struct {
	check *authz.PermissionCheck
	where string
}

// checksOf walks every check the toolkit declares: the toolkit-wide default,
// each subcommand's override, and each subcommand's permissionVariants. The
// variants matter most — `gh api` is the widest call in the toolkit and all
// three of its safe readings are variants, so a walker that stopped at
// sc.Permission would leave them behind and report green.
func checksOf(tk *toolkit.Toolkit) []namedCheck {
	var out []namedCheck
	add := func(p *authz.Permission, where string) {
		if p == nil || p.Check == nil {
			return
		}
		out = append(out, namedCheck{check: p.Check, where: where})
	}
	add(tk.Permission, "the "+tk.Name+" toolkit default")
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		label := "`" + tk.Name + " " + strings.Join(sc.Path, " ") + "`"
		add(sc.Permission, label)
		for j := range sc.PermissionVariants {
			v := &sc.PermissionVariants[j]
			add(&v.Check, label+" variant "+v.When)
		}
	}
	return out
}

// structuredNames lists the definitions the toolkit declares in the STRUCTURED
// half of its fragment — the half schema_coverage_test.go indexes.
func structuredNames(tk *toolkit.Toolkit) []string {
	var out []string
	if tk.SpiceDBSchema == nil {
		return out
	}
	for _, r := range tk.SpiceDBSchema.Resources {
		out = append(out, r.Name)
	}
	return out
}

// rawZedDefinitionNames lists the definitions the toolkit declares in RawZed.
// A lexical scan, which is all that is needed to assert a definition is
// present; ValidateFragment compiles the same text for real.
func rawZedDefinitionNames(tk *toolkit.Toolkit) []string {
	var out []string
	if tk.SpiceDBSchema == nil {
		return out
	}
	for _, line := range strings.Split(tk.SpiceDBSchema.RawZed, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "definition" {
			out = append(out, strings.TrimSuffix(fields[1], "{"))
		}
	}
	return out
}

// permExpr returns the expression of one permission on one structured
// definition, failing the test when either is missing rather than returning an
// empty string a caller's assert.Equal would silently compare against.
func permExpr(t *testing.T, tk *toolkit.Toolkit, resource, permission string) string {
	t.Helper()
	require.NotNil(t, tk.SpiceDBSchema)
	for _, r := range tk.SpiceDBSchema.Resources {
		if r.Name != resource {
			continue
		}
		for _, p := range r.Permissions {
			if p.Name == permission {
				return p.Expr
			}
		}
		t.Fatalf("%s declares no %q permission", resource, permission)
	}
	t.Fatalf("the %s toolkit declares no %q resource", tk.Name, resource)
	return ""
}
