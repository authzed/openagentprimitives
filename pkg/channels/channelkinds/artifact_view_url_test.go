package channelkinds_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestComposeArtifactViewURL_NamesTheResourceAndCarriesNoCapability is the
// security claim stated at the composer, where it is cheapest to see: the URL
// is a pair of identifiers and nothing else. No signature, no expiry, no
// subject — so a reader who has the string holds no authority, and webd's
// arrival-time check is the only thing that can admit them.
func TestComposeArtifactViewURL_NamesTheResourceAndCarriesNoCapability(t *testing.T) {
	got, err := channelkinds.ComposeArtifactViewURL("https://ap.example", "default/review-42", "artifact-abc")
	require.NoError(t, err)

	u, perr := url.Parse(got)
	require.NoError(t, perr, "composed value must be a parseable URL: %q", got)
	assert.Equal(t, "/artifact-view", u.Path)

	q := u.Query()
	assert.Equal(t, "artifact-abc", q.Get("artifactId"))
	assert.Equal(t, "default/review-42", q.Get("sessionRef"))
	assert.ElementsMatch(t, []string{"artifactId", "sessionRef"}, keysOf(q),
		"a durable link carries identifiers ONLY: a signature, expiry or subject here would make knowing the URL an authority")
}

// keysOf lists a query's parameter names, so the assertion above can say
// "these and no others" rather than only checking the two it expects.
func keysOf(q url.Values) []string {
	out := make([]string, 0, len(q))
	for k := range q {
		out = append(out, k)
	}
	return out
}

// TestComposeArtifactViewURL_IsStableAcrossCalls: the link is written into a
// third party's record (a check run) and re-composed on every re-delivery of
// the same event. Two calls with the same inputs must produce the same string,
// or a redelivery would rewrite the check run with a different link to the same
// report.
func TestComposeArtifactViewURL_IsStableAcrossCalls(t *testing.T) {
	first, err := channelkinds.ComposeArtifactViewURL("https://ap.example", "default/review-42", "artifact-abc")
	require.NoError(t, err)
	second, err := channelkinds.ComposeArtifactViewURL("https://ap.example", "default/review-42", "artifact-abc")
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestComposeArtifactViewURL_EdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		sessionRef string
		artifactID string
		want       string
		wantErr    bool
	}{
		{
			name: "empty base (webd not configured yet): clean skip, no error",
			base: "", sessionRef: "default/s1", artifactID: "artifact-abc",
			want: "",
		},
		{
			name: "trailing slash on base: exactly one separator",
			base: "https://ap.example/", sessionRef: "default/s1", artifactID: "artifact-abc",
			want: "https://ap.example/artifact-view?artifactId=artifact-abc&sessionRef=default%2Fs1",
		},
		{
			name: "sessionRef with no separator: error, never a half-addressed link",
			base: "https://ap.example", sessionRef: "s1", artifactID: "artifact-abc",
			wantErr: true,
		},
		{
			name: "sessionRef with an empty namespace: error",
			base: "https://ap.example", sessionRef: "/s1", artifactID: "artifact-abc",
			wantErr: true,
		},
		{
			name: "empty artifactID: error, never a link to the whole session",
			base: "https://ap.example", sessionRef: "default/s1", artifactID: "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := channelkinds.ComposeArtifactViewURL(tc.base, tc.sessionRef, tc.artifactID)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, got, "a refusal must not also hand back a URL")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
