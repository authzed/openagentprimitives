package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// TestFakeGrantsEveryRequiredScope pins fakeslack's granted-scope header to
// the scopes the full feature set actually requires — channelkinds.ScopesFor
// over FeatureSupport (features.go), the single source of truth since the
// scopes.go/manifest.go/features.go triplicate collapsed onto it.
//
// The two were once held together by nothing but a comment, and the first
// scope added after that comment was written drifted immediately: every e2e
// scenario began tripping a spurious ScopesValid=false, which is noise in
// exactly the suite where a genuine scope regression should be loudest. A
// comment cannot fail a build; this can.
func TestFakeGrantsEveryRequiredScope(t *testing.T) {
	required := channelkinds.ScopesFor(&Kind{}, channelfeatures.All())

	granted := map[string]bool{}
	for _, s := range strings.Split(fakeslack.GrantedBotScopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			granted[s] = true
		}
	}
	for _, want := range required {
		assert.Truef(t, granted[want],
			"fakeslack.GrantedBotScopes is missing %q — add it there whenever FeatureSupport grows a new scope", want)
	}
}

// TestMissingScopesAgainstAnExplicitRequiredSet covers the missingScopes
// helper across its observable failure modes, driven entirely by the
// required argument rather than a package-level scope list: missingScopes has
// no static notion of "the required scopes" — the caller supplies it
// (listener.go derives it from channelkinds.ScopesFor).
func TestMissingScopesAgainstAnExplicitRequiredSet(t *testing.T) {
	cases := []struct {
		name     string
		granted  string
		required []string
		want     []string
	}{
		{
			name:     "all required granted: nothing missing",
			granted:  "chat:write,files:write",
			required: []string{"chat:write", "files:write"},
			want:     nil,
		},
		{
			name:     "one required absent: that one reported",
			granted:  "chat:write",
			required: []string{"chat:write", "files:write"},
			want:     []string{"files:write"},
		},
		{
			name:     "granted superset: nothing missing",
			granted:  "chat:write,files:write,users:read",
			required: []string{"chat:write"},
			want:     nil,
		},
		{
			name:     "empty header is suspicious: all required reported",
			granted:  "",
			required: []string{"chat:write", "files:write"},
			want:     []string{"chat:write", "files:write"},
		},
		{
			name:     "empty required set: nothing missing even with an empty header",
			granted:  "",
			required: nil,
			want:     nil,
		},
		{
			name:     "header whitespace is tolerated",
			granted:  " chat:write , files:write ",
			required: []string{"chat:write", "files:write"},
			want:     nil,
		},
		{
			name:     "several required absent: every gap reported, sorted",
			granted:  "chat:write",
			required: []string{"users:read", "files:write", "app_mentions:read", "chat:write"},
			want:     []string{"app_mentions:read", "files:write", "users:read"},
		},
		{
			name:     "whitespace-only header with unsorted required: all reported, sorted",
			granted:  "   ",
			required: []string{"files:write", "chat:write"},
			want:     []string{"chat:write", "files:write"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, missingScopes(tc.granted, tc.required))
		})
	}
}

// TestFilesReadRequiredForAttachmentFetch ties files:read to the code path
// that actually needs it, rather than just asserting the string is present
// somewhere in an arbitrary list.
//
// attachments.go asserts `var _ channelkinds.AttachmentFetcher = (*Kind)(nil)`
// at package scope, so Kind implementing FetchAttachment (whose first leg is
// files.info) is a build-time fact, not a maybe. This test is the other half
// of that fact: as long as this package compiles — i.e. as long as Kind still
// implements AttachmentFetcher — files:read dropping out of FeatureSupport's
// AttachmentsInbound entry must fail a test, not just silently ship a fresh
// install that 403s on its first inbound attachment.
func TestFilesReadRequiredForAttachmentFetch(t *testing.T) {
	got := channelkinds.ScopesFor(&Kind{}, channelfeatures.All())
	assert.Contains(t, got, "files:read",
		"FetchAttachment (attachments.go) resolves an inbound file's download "+
			"URL via files.info, which requires files:read; without it every "+
			"inbound attachment fails with missing_scope and reports the "+
			"transient \"could not be retrieved\" notice")
}
