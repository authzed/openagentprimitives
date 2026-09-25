package plangate

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// Table over the label derivers: a CLOSED set, exactly like
// authz.transformRegistry. An unrecognized name and a non-URL value are both
// covered — both must derive NOTHING rather than guess.
func TestDeriveLabel(t *testing.T) {
	cases := []struct {
		name    string
		deriver string
		raw     string
		want    string
	}{
		{
			name:    "url_path: a real URL shortens to its path",
			deriver: "url_path",
			raw:     "https://github.com/demo-org/demo-repo",
			want:    "demo-org/demo-repo",
		},
		{
			name:    "url_path: a trailing slash is trimmed",
			deriver: "url_path",
			raw:     "https://github.com/demo-org/demo-repo/",
			want:    "demo-org/demo-repo",
		},
		{
			name:    "url_path: no path at all derives nothing",
			deriver: "url_path",
			raw:     "https://github.com",
			want:    "",
		},
		{
			name:    "url_path: a non-URL value derives nothing",
			deriver: "url_path",
			raw:     "not a url",
			want:    "",
		},
		{
			name:    "last_segment: takes the final path segment",
			deriver: "last_segment",
			raw:     "https://tracker.example/org/proj/issues/42",
			want:    "42",
		},
		{
			name:    "last_segment: a non-URL value with no slash is itself the segment",
			deriver: "last_segment",
			raw:     "TICKET-42",
			want:    "TICKET-42",
		},
		{
			name:    "last_segment: a trailing slash is trimmed before splitting",
			deriver: "last_segment",
			raw:     "org/proj/issues/42/",
			want:    "42",
		},
		{
			name:    "last_segment: an empty value derives nothing",
			deriver: "last_segment",
			raw:     "",
			want:    "",
		},
		{
			name:    "none: an explicitly declared no-op derives nothing",
			deriver: "none",
			raw:     "https://github.com/demo-org/demo-repo",
			want:    "",
		},
		{
			name:    "unrecognized deriver name derives nothing, same as none",
			deriver: "regex_magic",
			raw:     "https://github.com/demo-org/demo-repo",
			want:    "",
		},
		{
			name:    "empty deriver name derives nothing",
			deriver: "",
			raw:     "https://github.com/demo-org/demo-repo",
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DeriveLabel(tc.deriver, tc.raw))
		})
	}
}

// gh.yaml's cards read a repository's path off its object id. An encoded id
// breaks url.Parse, which is how every card once fell back to printing the
// type name — decode first.
func TestDeriveB64URLPath_DecodesBeforeParsing(t *testing.T) {
	id := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/acme/widgets"))
	assert.Equal(t, "acme/widgets", DeriveLabel("b64url_path", id))
}

// A value that is not base64 must not panic or emit garbage.
func TestDeriveB64URLPath_UndecodableYieldsEmpty(t *testing.T) {
	assert.Equal(t, "", DeriveLabel("b64url_path", "!!!not-base64!!!"))
}

// Round-trip through the transform that actually mints these ids: an
// already-canonical GitHub URL must still come back through the deriver as
// its path, not "". A transform that leaves an already-canonical input
// unencoded produces an id this deriver cannot decode, which is exactly the
// regression gh.yaml records — the card falling back to the type name.
func TestDeriveB64URLPath_RoundTripsAnAlreadyCanonicalRepo(t *testing.T) {
	id, err := authz.ApplyTransforms("https://github.com/acme/widgets", []string{"github_repo_url_id"})
	require.NoError(t, err)
	assert.Equal(t, "acme/widgets", DeriveLabel("b64url_path", id))
}

// Table over the icon registry: a CLOSED, code-defined set. An unrecognized
// name must render no icon — never a fallback image, never a guess.
func TestKnownIcon(t *testing.T) {
	cases := []struct {
		name string
		icon string
		want string
	}{
		{name: "repository is a known generic mark", icon: "repository", want: "repository"},
		{name: "github is a known vendor mark", icon: "github", want: "github"},
		{name: "an unrecognized name renders nothing", icon: "not-a-real-icon", want: ""},
		{name: "empty renders nothing", icon: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, knownIcon(tc.icon))
		})
	}
}

// The link-eligibility table and the text-==-href invariant it proves now
// live with the function, in pkg/authz/resourcedisplay/objectid_test.go: the
// admin console's Directory panel links the same ids through the same guard,
// so the tests belong where both callers can be broken by changing it, not
// on one of the two sides.
