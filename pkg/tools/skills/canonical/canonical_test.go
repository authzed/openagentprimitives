package canonical

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantErr   bool
		authority string
		subpath   string
		ref       string
		isLocal   bool
	}{
		{name: "git, unpinned", in: "github.com/someorg/somerepo//skills/skillone",
			authority: "github.com/someorg/somerepo", subpath: "skills/skillone"},
		{name: "git, tag ref", in: "github.com/someorg/somerepo//skills/skillone@v1.2.0",
			authority: "github.com/someorg/somerepo", subpath: "skills/skillone", ref: "v1.2.0"},
		{name: "git, sha ref", in: "github.com/someorg/somerepo//skills/skillone@a1b2c3d",
			authority: "github.com/someorg/somerepo", subpath: "skills/skillone", ref: "a1b2c3d"},
		{name: "gitlab subgroup", in: "gitlab.com/org/sub/repo//x",
			authority: "gitlab.com/org/sub/repo", subpath: "x"},
		{name: "local cluster", in: "local//house-style",
			authority: "local", subpath: "house-style", isLocal: true},
		{name: "local namespaced", in: "local/team-a//house-style",
			authority: "local/team-a", subpath: "house-style", isLocal: true},
		{name: "empty", in: "", wantErr: true},
		{name: "no separator", in: "github.com/org/repo/skills/x", wantErr: true},
		{name: "has scheme", in: "https://github.com/org/repo//x", wantErr: true},
		{name: "authority ends .git", in: "github.com/org/repo.git//x", wantErr: true},
		{name: "empty ref", in: "github.com/org/repo//x@", wantErr: true},
		{name: "empty subpath", in: "github.com/org/repo//", wantErr: true},
		{name: "empty authority", in: "//x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.authority, got.Authority)
			assert.Equal(t, tc.subpath, got.Subpath)
			assert.Equal(t, tc.ref, got.Ref)
			assert.Equal(t, tc.isLocal, got.IsLocal)
		})
	}
}

func TestRoundTripString(t *testing.T) {
	for _, in := range []string{
		"github.com/someorg/somerepo//skills/skillone",
		"github.com/someorg/somerepo//skills/skillone@v1.2.0",
		"local/team-a//house-style",
	} {
		n, err := Parse(in)
		require.NoError(t, err)
		assert.Equal(t, in, n.String())
	}
}

func TestPinStrength(t *testing.T) {
	cases := []struct {
		in   string
		want PinStrength
	}{
		{"github.com/o/r//x", PinUnpinned},
		{"github.com/o/r//x@main", PinNamed},
		{"github.com/o/r//x@v1.2.0", PinNamed},
		{"github.com/o/r//x@a1b2c3d", PinFrozen},
		{"github.com/o/r//x@0123456789abcdef0123456789abcdef01234567", PinFrozen},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			n, err := Parse(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, n.PinStrength())
		})
	}
}

func TestLastSegment(t *testing.T) {
	n, err := Parse("github.com/o/r//a/b/skillone@v1")
	require.NoError(t, err)
	assert.Equal(t, "skillone", n.LastSegment())
}

func TestSafeSlugDeterministicAndValid(t *testing.T) {
	n, err := Parse("github.com/someorg/somerepo//skills/SkillOne@v1.2.0")
	require.NoError(t, err)
	slug := n.SafeSlug()
	assert.Equal(t, slug, n.SafeSlug(), "slug must be deterministic")
	assert.Regexp(t, `^[a-z0-9][a-z0-9.-]*[a-z0-9]$`, slug)
	assert.LessOrEqual(t, len(slug), 253)
	other, _ := Parse("github.com/someorg/somerepo//skills/SkillOne@v2.0.0")
	assert.NotEqual(t, slug, other.SafeSlug())
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/org/repo.git", "github.com/org/repo"},
		{"https://github.com/org/repo/", "github.com/org/repo"},
		{"http://gitlab.com/org/sub/repo", "gitlab.com/org/sub/repo"},
		{"git@github.com:org/repo.git", "github.com/org/repo"},
		{"github.com/org/repo", "github.com/org/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, Normalize(tc.in))
		})
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"github.com/o/r//skills/x@v1", "github.com/o/r//skills/x@v1", true},            // exact
		{"github.com/o/r//skills/x@v1", "github.com/o/r//skills/x@v2", false},           // exact mismatch
		{"github.com/trusted-org/**", "github.com/trusted-org/repo//skills/x@v1", true}, // org prefix
		{"github.com/trusted-org/**", "github.com/other-org/repo//skills/x", false},
		{"github.com/o/r//skills/x@*", "github.com/o/r//skills/x@v9.9.9", true}, // any version
		{"github.com/o/r//skills/x@*", "github.com/o/r//skills/y@v1", false},
		{"*", "anything//at/all@v1", true},                                 // match-all
		{"github.com/o/r//skills/x", "github.com/o/r//skills/x@v1", false}, // no wildcard, not a prefix
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"~"+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MatchPattern(tc.pattern, tc.name))
		})
	}
}
