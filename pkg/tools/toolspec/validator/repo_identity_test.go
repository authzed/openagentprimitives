package validator

import (
	"encoding/base64"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// git and gh must resolve one repository to the SAME canonical identity.
//
// The two toolkits name a repository in different vocabularies — git sees a
// remote URL, gh sees OWNER/NAME — and nothing but a shared canonical form
// keeps them agreeing on which repository is meant. When they diverge, a phase
// reaching one repository through both tools writes TWO grants and shows an
// approver TWO targets, one wearing a generic icon and the other GitHub's. That
// is what a reviewer saw on a live card.
//
// What they no longer share is the OBJECT ID, and that is deliberate rather
// than drift: gh keys `github_repo_url` by base64url of the canonical URL so
// that a forge-keyed `github_repo` can exist alongside it without the two
// colliding. base64url is a WIRE ENCODING over the canonical identity, exactly
// as git_repo's own `spicedb_escape` step is — so the property worth asserting
// is that stripping each toolkit's encoding leaves one string, which is what
// this does.
//
// Read from the real toolkits/ tree rather than restating the chains here: a
// hardcoded copy would keep passing while the YAML drifted out from under it,
// which is the exact failure this is meant to catch.
func TestGitAndGhResolveOneRepositoryToOneCanonicalIdentity(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")

	gitChain := repoChainFor(t, filepath.Join(repoRoot, "toolkits", "git.yaml"), "git_repo", "push")
	ghChain := repoChainFor(t, filepath.Join(repoRoot, "toolkits", "gh.yaml"), "github_repo_url", "read")

	// Every spelling either tool can produce for one repository.
	for _, tc := range []struct{ name, gitValue, ghValue string }{
		{"plain https", "https://github.com/demo-org/demo-repo", "demo-org/demo-repo"},
		{"git suffix", "https://github.com/demo-org/demo-repo.git", "demo-org/demo-repo"},
		{"scp form", "git@github.com:demo-org/demo-repo.git", "demo-org/demo-repo"},
		{"mixed case", "https://github.com/Demo-Org/Demo-Repo", "Demo-Org/Demo-Repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromGit, err := authz.ApplyTransforms(tc.gitValue, gitChain)
			require.NoError(t, err, "git chain %v", gitChain)
			fromGh, err := authz.ApplyTransforms(tc.ghValue, ghChain)
			require.NoError(t, err, "gh chain %v", ghChain)

			// gh's id is base64url; git's is spicedb_escape'd. Undo each
			// encoding and the canonical URL underneath must be identical.
			decoded, derr := base64.RawURLEncoding.DecodeString(fromGh)
			require.NoError(t, derr,
				"gh %q must key by base64url; %q does not decode, so the id is not the form "+
					"b64url_path and the directory-sync bridge both read", tc.ghValue, fromGh)

			assert.Equal(t, unescapeSpiceDBID(fromGit), string(decoded),
				"git %q and gh %q name one repository and must agree on its canonical identity",
				tc.gitValue, tc.ghValue)

			// And the two ids must NOT be equal as strings, because they key
			// two different definitions now. Asserting it keeps the split
			// visible: if a future edit puts them back on one id space, the
			// forge-keyed github_repo would start colliding with URL-keyed
			// tuples, which is the silent reinterpretation this whole split
			// exists to avoid.
			assert.NotEqual(t, fromGit, fromGh,
				"git_repo and github_repo_url are different definitions and must not share an id space")
		})
	}
}

// unescapeSpiceDBID reverses spicedb_escape's `=XX` hex form, so a git_repo
// object id can be compared against the URL underneath it. Deliberately local
// to this test: nothing in production needs to invert the escape, and offering
// an inverse from the transform package would invite one to.
func unescapeSpiceDBID(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '=' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// A non-GitHub remote must be untouched by the shared canonicalizer — git.yaml
// is host-agnostic, and re-keying every GitLab or self-hosted repository to
// make GitHub tidy would invalidate grants that already exist.
func TestGitChainLeavesNonGitHubRemotesAlone(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	gitChain := repoChainFor(t, filepath.Join(repoRoot, "toolkits", "git.yaml"), "git_repo", "push")

	got, err := authz.ApplyTransforms("https://gitlab.com/demo-org/demo-repo.git", gitChain)
	require.NoError(t, err)

	want, err := authz.ApplyTransforms("https://gitlab.com/demo-org/demo-repo.git",
		[]string{"normalize_url", "spicedb_escape"})
	require.NoError(t, err)

	assert.Equal(t, want, got,
		"a GitLab remote must key exactly as it did before github_repo_id joined the chain")
}

// EVERY check on a repository type keys it the same way.
//
// gh declares two dozen checks on github_repo_url and git several on git_repo.
// One of them drifting onto a different chain is worse than all of them
// drifting: most calls would agree with the card while a single subcommand
// quietly addressed a different object, which no scenario asserting one call
// would catch. Checked here as a set so a partial drift fails as loudly as a
// total one.
func TestEveryRepositoryCheckSharesOneChain(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")

	// wantTransform is the canonicalizer that type's chain must run. The two
	// differ only by the wire encoding on the end — github_repo_url_id is
	// github_repo_id plus base64url — so naming each explicitly is what keeps
	// this from passing on a substring.
	for _, tc := range []struct{ path, resourceType, wantTransform string }{
		{filepath.Join(repoRoot, "toolkits", "gh.yaml"), "github_repo_url", "github_repo_url_id"},
		{filepath.Join(repoRoot, "toolkits", "git.yaml"), "git_repo", "github_repo_id"},
	} {
		t.Run(tc.resourceType, func(t *testing.T) {
			tk, err := toolkit.Load(tc.path)
			require.NoError(t, err, "load %s", tc.path)

			seen := map[string][]string{} // chain -> the subcommands declaring it
			for _, sc := range tk.Subcommands {
				p := sc.Permission
				if p == nil || p.Check == nil || p.Check.ResourceType != tc.resourceType {
					continue
				}
				// The workspace sentinel is a different instance, not a
				// different spelling of the repository, so it is allowed its
				// own keying.
				if p.Check.ResourceIDTemplate == "workspace" {
					continue
				}
				key := strings.Join(p.Check.ResourceIDTransforms, ",")
				seen[key] = append(seen[key], strings.Join(sc.Path, " "))
			}

			require.NotEmpty(t, seen, "%s declares no checks on %s", tc.path, tc.resourceType)
			assert.Len(t, seen, 1,
				"every %s check must share one chain; found %d distinct: %v", tc.resourceType, len(seen), seen)
			for chain := range seen {
				assert.Contains(t, strings.Split(chain, ","), tc.wantTransform,
					"the shared chain must canonicalize repository identity with %s", tc.wantTransform)
			}
		})
	}
}

// repoChainFor finds the value chain a toolkit declares for the named
// (resourceType, permission) check, so the assertions above run against what
// actually ships. TestEveryRepositoryCheckSharesOneChain is what makes taking
// the first match safe — without it, this would read one check and say nothing
// about the rest.
func repoChainFor(t *testing.T, path, resourceType, permission string) []string {
	t.Helper()
	tk, err := toolkit.Load(path)
	require.NoError(t, err, "load %s", path)

	for _, sc := range tk.Subcommands {
		p := sc.Permission
		if p == nil || p.Check == nil {
			continue
		}
		if p.Check.ResourceType == resourceType && p.Check.Permission == permission {
			require.NotEmpty(t, p.Check.ResourceIDTransforms,
				"%s %s/%s declares no value chain", path, resourceType, permission)
			return p.Check.ResourceIDTransforms
		}
	}
	t.Fatalf("%s declares no check on %s#%s", path, resourceType, permission)
	return nil
}
