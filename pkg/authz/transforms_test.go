package authz_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestApplyTransforms(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		transforms []string
		want       string
	}{
		{
			name:       "lowercase produces all-lower",
			input:      "Authzed/SpiceDB",
			transforms: []string{"lowercase"},
			want:       "authzed/spicedb",
		},
		{
			name:       "remove_spaces strips whitespace including tabs",
			input:      "hello world\tfoo",
			transforms: []string{"remove_spaces"},
			want:       "helloworldfoo",
		},
		{
			name:       "basename keeps only the trailing path segment",
			input:      "/some/long/path/tail",
			transforms: []string{"basename"},
			want:       "tail",
		},
		{
			name:       "lowercase->spicedb_object_id pipeline order",
			input:      "Hello WORLD",
			transforms: []string{"lowercase", "spicedb_object_id"},
			want:       "hello-world",
		},
		{
			name:       "nil transform list returns input unchanged",
			input:      "untouched",
			transforms: nil,
			want:       "untouched",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authz.ApplyTransforms(tc.input, tc.transforms)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestApplyTransforms_SpicedbObjectID_StripsBadChars is property-based —
// it asserts a character-set invariant rather than an exact string, so it
// stays separate from the value-equality table above.
func TestApplyTransforms_SpicedbObjectID_StripsBadChars(t *testing.T) {
	// Colons and hashes are reserved in SpiceDB syntax; spaces aren't allowed.
	// Output must be in [a-zA-Z0-9_/\-=|+], no leading/trailing dashes,
	// no consecutive dashes.
	got, err := authz.ApplyTransforms("Hello World!:#", []string{"spicedb_object_id"})
	require.NoError(t, err)
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '/', r == '-', r == '=', r == '|', r == '+':
		default:
			t.Errorf("spicedb_object_id left %q (rune %q) in output", got, r)
		}
	}
	assert.False(t, strings.HasPrefix(got, "-"), "spicedb_object_id should trim leading dashes")
	assert.False(t, strings.HasSuffix(got, "-"), "spicedb_object_id should trim trailing dashes")
	assert.NotContains(t, got, "--", "spicedb_object_id should dedupe consecutive dashes")
}

// TestApplyTransforms_Sha256_StableLength asserts a property (output
// length) rather than an exact value, so it stays separate from the
// value-equality table above.
func TestApplyTransforms_Sha256_StableLength(t *testing.T) {
	got, err := authz.ApplyTransforms("anything", []string{"sha256"})
	require.NoError(t, err)
	assert.Len(t, got, 64, "sha256 should produce 64 hex chars")
}

func TestApplyTransforms_UnknownTransformIsError(t *testing.T) {
	_, err := authz.ApplyTransforms("x", []string{"no_such_transform"})
	require.Error(t, err, "expected error for unknown transform")
	assert.Contains(t, err.Error(), "no_such_transform", "error should name the unknown transform")
}

// spicedb_escape exists because the two properties an approval card needs were
// in conflict. spicedb_object_id is READABLE but not injective; sha256 is
// INJECTIVE but opaque. A card shows the value a human approved and the grant
// lands on the transformed id, so the pair has to be both: readable, or the
// human cannot check it; injective, or the value shown is not the only value
// that reaches the grant.
//
// The agent authors the slot value. That is what makes injectivity load-bearing
// rather than tidy — the transform set being closed and agent-uncontrolled does
// not help when the agent chooses the INPUT and a second input reaches the same
// id.
func TestSpicedbEscape_TheCollisionThatBreaksSpicedbObjectID(t *testing.T) {
	// spicedb_object_id maps ',' and '.' both to '-', so these two land on one
	// id: approve the first, and a call naming the second is authorized.
	const approved = "https://github.com/acme/app"
	const other = "https://github,com/acme/app"

	lossyA, err := authz.ApplyTransforms(approved, []string{"spicedb_object_id"})
	require.NoError(t, err)
	lossyB, err := authz.ApplyTransforms(other, []string{"spicedb_object_id"})
	require.NoError(t, err)
	require.Equal(t, lossyA, lossyB,
		"precondition: this pair is exactly why spicedb_object_id is not injective")

	gotA, err := authz.ApplyTransforms(approved, []string{"spicedb_escape"})
	require.NoError(t, err)
	gotB, err := authz.ApplyTransforms(other, []string{"spicedb_escape"})
	require.NoError(t, err)
	assert.NotEqual(t, gotA, gotB,
		"two distinct repositories must be two distinct object ids")
}

func TestSpicedbEscape_IsRegisteredInjective(t *testing.T) {
	assert.True(t, authz.IsRegisteredTransform("spicedb_escape"))
	assert.True(t, authz.IsInjectiveTransform("spicedb_escape"),
		"an expr-keyed check may only use injective transforms; a non-injective one is refused at validation")
	assert.Empty(t, authz.NonInjectiveTransforms([]string{"normalize_url", "spicedb_escape"}),
		"the chain git/gh will actually use must pass the terminal-injectivity rule")
}

// The output has to be a legal SpiceDB object id or the grant cannot be written
// at all — which is the constraint that rules out percent-encoding and makes
// '=' the escape character.
func TestSpicedbEscape_OutputIsAlwaysALegalObjectID(t *testing.T) {
	legal := func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '/' || r == '-' || r == '=' || r == '|' || r == '+'
	}
	inputs := []string{
		"https://github.com/acme/app",
		"git@github.com:acme/app.git",
		"https://user:pw@host.example/a b?q=1&r=2#frag",
		"plain",
		"",
		"=already=escaped=",
		"ünïcödé/path",
	}
	for _, in := range inputs {
		got, err := authz.ApplyTransforms(in, []string{"spicedb_escape"})
		require.NoError(t, err, "input %q", in)
		for _, r := range got {
			assert.True(t, legal(r), "input %q produced illegal rune %q in %q", in, r, got)
		}
	}
}

// Injectivity, asserted as the property rather than on one lucky pair: no two
// distinct inputs may share an output.
func TestSpicedbEscape_NoTwoDistinctInputsShareAnID(t *testing.T) {
	inputs := []string{
		"https://github.com/acme/app",
		"https://github,com/acme/app",
		"https://github.com/acme/app.git",
		"https://github.com/acme/app,git",
		"https://github.com:443/acme/app",
		"git@github.com:acme/app",
		"a?b", "a/b", "a-b", "a=b", "a b",
		"=3A", ":", "==", "=",
	}
	seen := map[string]string{}
	for _, in := range inputs {
		got, err := authz.ApplyTransforms(in, []string{"spicedb_escape"})
		require.NoError(t, err)
		if prev, dup := seen[got]; dup {
			t.Fatalf("collision: %q and %q both map to %q — an approval for one would authorize the other", prev, in, got)
		}
		seen[got] = in
	}
}

// Readability is the whole reason this exists rather than reusing sha256: a
// human reading a card or an audit record has to recognize the repository.
func TestSpicedbEscape_StaysRecognizable(t *testing.T) {
	got, err := authz.ApplyTransforms("https://github.com/acme/app", []string{"spicedb_escape"})
	require.NoError(t, err)
	assert.Contains(t, got, "acme/app", "the path a human recognizes must survive verbatim")
	assert.Contains(t, got, "github", "so must the host")
}

// A slot needs the value on the card to be the only thing reaching the grant.
// Injectivity is a PROXY for that, and it asks the wrong question for a
// case-insensitive namespace: folding Demo-Org/Demo-Repo onto demo-org/demo-repo
// merges two spellings of ONE repository, never two repositories.
func TestUnsafeSlotTransforms(t *testing.T) {
	cases := []struct {
		name  string
		chain []string
		want  []string
	}{
		{
			name:  "an injective chain is slot-safe",
			chain: []string{"normalize_url", "spicedb_escape"},
		},
		{
			name:  "an identity-preserving fold is slot-safe too",
			chain: []string{"casefold_identity", "spicedb_escape"},
		},
		{
			name:  "plain lowercase is NOT — it is a general string op with no domain claim",
			chain: []string{"lowercase"},
			want:  []string{"lowercase"},
		},
		{
			name:  "basename collapses genuinely different files",
			chain: []string{"basename"},
			want:  []string{"basename"},
		},
		{
			name:  "an unregistered name is unsafe rather than assumed benign",
			chain: []string{"not_a_transform"},
			want:  []string{"not_a_transform"},
		},
		{
			name:  "one unsafe step taints the chain, and is named",
			chain: []string{"casefold_identity", "basename", "spicedb_escape"},
			want:  []string{"basename"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.UnsafeSlotTransforms(tc.chain))
		})
	}
}

// casefold_identity must produce EXACTLY what lowercase did, so re-keying a
// type onto it changes no object id and needs no migration of existing tuples.
func TestCasefoldIdentity_MatchesLowercaseOutput(t *testing.T) {
	for _, in := range []string{"Demo-Org/Demo-Repo", "demo-org/demo-repo", "SELF", "a-b_c.d"} {
		lower, err := authz.ApplyTransforms(in, []string{"lowercase"})
		require.NoError(t, err)
		fold, err := authz.ApplyTransforms(in, []string{"casefold_identity"})
		require.NoError(t, err)
		assert.Equal(t, lower, fold, "re-keying must not move any existing object id (input %q)", in)
	}
}

// github_repo_id is what makes one repository one id no matter which tool
// names it. The git toolkit sees a remote URL, the gh toolkit sees
// OWNER/NAME, and a human approving sees one repository — so the two must
// derive the same object id or the card shows one repo as two targets and the
// approval writes two grants the user never asked for.
func TestGitHubRepoID_FoldsEverySpellingOfOneRepository(t *testing.T) {
	const want = "https://github.com/demo-org/demo-repo"
	for _, spelling := range []string{
		"demo-org/demo-repo",                        // gh --repo
		"https://github.com/demo-org/demo-repo",     // git remote, https
		"https://github.com/demo-org/demo-repo.git", // git remote, https + suffix
		"git@github.com:demo-org/demo-repo",         // git remote, scp
		"git@github.com:demo-org/demo-repo.git",     // git remote, scp + suffix
		"https://GitHub.com/Demo-Org/Demo-Repo",     // case: GitHub is case-insensitive here
		"https://github.com/demo-org/demo-repo/",    // trailing slash
	} {
		assert.Equal(t, want, applyOne(t, "github_repo_id", spelling),
			"spelling %q must fold onto the canonical id", spelling)
	}
}

// Anything that is not a GitHub repository passes through untouched. The git
// toolkit is host-agnostic and runs this transform on every remote it sees, so
// a GitLab or self-hosted URL must come out exactly as it went in — otherwise
// adding this to git.yaml would silently re-key every non-GitHub repository.
func TestGitHubRepoID_PassesThroughEverythingElse(t *testing.T) {
	for _, other := range []string{
		"https://gitlab.com/demo-org/demo-repo",
		"https://git.example.internal/demo-org/demo-repo.git",
		"git@gitlab.com:demo-org/demo-repo.git",
		"workspace", // git.yaml's constant for the checked-out copy
		"",
		"not a url at all",
		"demo-org/demo-repo/extra/segments", // not an OWNER/NAME pair
	} {
		assert.Equal(t, other, applyOne(t, "github_repo_id", other),
			"non-GitHub value %q must pass through unchanged", other)
	}
}

// It folds spellings, so it is NOT injective — and must not claim to be.
// Declaring it injective would let it key a free-form value, and the registry's
// injective flag exists to stop two DIFFERENT resources colliding into one
// grant. Folding `.git` is the opposite case: two spellings of the SAME
// repository, which is exactly what identityPreserving means.
func TestGitHubRepoID_IsIdentityPreservingNotInjective(t *testing.T) {
	assert.False(t, authz.IsInjectiveTransform("github_repo_id"),
		"it collapses .git and scp spellings, so it cannot be injective")
	assert.True(t, authz.IsRegisteredTransform("github_repo_id"))
	assert.Empty(t, authz.UnsafeSlotTransforms([]string{"github_repo_id"}),
		"identity-preserving folding is safe for slot keying")
}

// applyOne runs a single named transform and fails the test if the registry
// rejects it, so each case above reads as one input and one expected id.
func applyOne(t *testing.T, name, in string) string {
	t.Helper()
	got, err := authz.ApplyTransforms(in, []string{name})
	require.NoError(t, err, "transform %q must be registered", name)
	return got
}

// base64url's alphabet is inside spicedb_escape's safe set and contains no
// '=', so an encoded id can never be confused with an escaped one.
func TestGitHubRepoURLID_EncodesCanonicalURL(t *testing.T) {
	got := applyOne(t, "github_repo_url_id", "git@github.com:acme/widgets.git")
	want := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/acme/widgets"))
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "=")
}

// A non-github value passes through, exactly as github_repo_id does.
func TestGitHubRepoURLID_PassesThroughNonGitHub(t *testing.T) {
	assert.Equal(t, "not-a-url", applyOne(t, "github_repo_url_id", "not-a-url"))
}

// An input that is ALREADY in its canonical https form must still be
// encoded. A string-equality proxy for "not a GitHub repo" (canonical ==
// raw) is wrong here: this input reaches that equality for a second, benign
// reason — it already IS its own canonical form — and a proxy that cannot
// tell the two reasons apart leaves the most natural way to write a GitHub
// URL unencoded.
func TestGitHubRepoURLID_EncodesAlreadyCanonicalInput(t *testing.T) {
	const canonicalRaw = "https://github.com/acme/widgets"
	got := applyOne(t, "github_repo_url_id", canonicalRaw)

	decoded, err := base64.RawURLEncoding.DecodeString(got)
	require.NoError(t, err, "an already-canonical input must still be base64url-encoded, not passed through raw")
	assert.Equal(t, canonicalRaw, string(decoded))
}

// The property the transform actually promises: two aliases of ONE
// repository must fold onto the SAME id, exactly as github_repo_id does for
// the plain-text alias.
func TestGitHubRepoURLID_FoldsAliasesOntoOneID(t *testing.T) {
	https := applyOne(t, "github_repo_url_id", "https://github.com/acme/widgets")
	scp := applyOne(t, "github_repo_url_id", "git@github.com:acme/widgets.git")
	assert.Equal(t, https, scp, "both spellings name the same repository and must produce the same id")
}
