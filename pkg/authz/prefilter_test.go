package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestPrefilter(t *testing.T) {
	cases := []struct {
		name    string
		message string
		types   []authz.BoundEntitySpec
		want    bool
	}{
		{
			name:    "github_repo: owner/name pattern matches",
			message: "merge PR 17 in foo/bar",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
			want:    true,
		},
		{
			name:    "github_repo: no candidate text returns false",
			message: "hi, just checking in!",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
			want:    false,
		},
		{
			// The URL-keyed type is the one the gh toolkit's checks name, so it
			// is the one a class actually declares. Its OBJECT ids are
			// base64url, but the sieve reads the user's prose, where a
			// repository is still owner/name.
			name:    "github_repo_url: owner/name pattern matches in prose",
			message: "merge PR 17 in foo/bar",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo_url"}},
			want:    true,
		},
		{
			// And it must still be able to say NO. An unknown type falls
			// through to "allow the LLM call", so a github_repo_url case that
			// was never added would pass the row above while sieving nothing —
			// this row is what tells the two apart.
			name:    "github_repo_url: no candidate text returns false",
			message: "hi, just checking in!",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo_url"}},
			want:    false,
		},
		{
			name:    "linear_team: UUID matches",
			message: "team a8f9d2c1-1b2c-4d3e-9f0a-1c2d3e4f5a6b is broken",
			types:   []authz.BoundEntitySpec{{ResourceType: "linear_team"}},
			want:    true,
		},
		{
			name:    "unknown resource type falls through to allow (better burn LLM call than silently skip)",
			message: "something opaque about wonky_thing-foo",
			types:   []authz.BoundEntitySpec{{ResourceType: "wonky_thing"}},
			want:    true,
		},
		{
			name:    "empty message returns false",
			message: "",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.Prefilter(tc.message, tc.types))
		})
	}
}

// TestPrefilter_SkipsSlotsTheExtractorCannotFill: the sieve exists to decide
// whether an extractor call is worth making, so a slot nothing can bind from a
// user message must not hold it open. The unknown-type case is the sharp one —
// an unknown type otherwise returns true unconditionally, so without the gate a
// single fillFrom:[ask] slot would force an extractor call on EVERY turn.
func TestPrefilter_SkipsSlotsTheExtractorCannotFill(t *testing.T) {
	cases := []struct {
		name    string
		message string
		types   []authz.BoundEntitySpec
		want    bool
	}{
		{
			name:    "only slot admits no extractor binding: no extractor call",
			message: "please look at demo-org/demo-repo",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo", FillFrom: []string{"default"}}},
			want:    false,
		},
		{
			name:    "unknown type excluding query no longer forces a call",
			message: "something opaque about wonky_thing-foo",
			types:   []authz.BoundEntitySpec{{ResourceType: "wonky_thing", FillFrom: []string{"channel_thread"}}},
			want:    false,
		},
		{
			name:    "a sibling that DOES allow query still opens the sieve",
			message: "please look at demo-org/demo-repo",
			types: []authz.BoundEntitySpec{
				{ResourceType: "http_target", FillFrom: []string{"default"}},
				{ResourceType: "github_repo", FillFrom: []string{"query"}},
			},
			want: true,
		},
		{
			name:    "extract spelling opens the sieve like query",
			message: "please look at demo-org/demo-repo",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo", FillFrom: []string{"extract"}}},
			want:    true,
		},
		{
			name:    "unset fillFrom behaves as before the field existed",
			message: "please look at demo-org/demo-repo",
			types:   []authz.BoundEntitySpec{{ResourceType: "github_repo"}},
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.Prefilter(tc.message, tc.types))
		})
	}
}
