//go:build !integration && !e2e

// pkg/authz/spicedb/relsource/imports/subject_identity_claims_test.go
//
// The COMPLETE-table guard for what an admin console's "Directory identities"
// panel probes. relsource.Source.SubjectIdentityClaims is what
// spicedb.subjectProbes derives from, and its defining hazard is that every
// mistake is SILENT: a probe that cannot match returns nothing, exactly like a
// person with no links, and a probe that matches too much returns rows that
// look plausible. Neither shows up as a failure anywhere.
//
// This file asserts against the real registered Sources (imported directly, not
// reconstructed) because a look-alike fixture would drift from the declaration
// that actually ships. It lives HERE rather than in pkg/authz/spicedb because
// this is the one package that can link every claim owner at once — the channel
// kinds import pkg/authz/spicedb, so that package cannot import them back.
package imports_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory/pttagmint"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
)

// Every source in the linked table, however it got there, must declare an
// identity-claim SUBSET of its own claims and never the hash sentinel.
//
// relsource.Register enforces both at registration, so this is the table-level
// restatement: it covers a source registered by a package this test does not
// name, and it fails as an assertion rather than as a panic in whatever test
// happened to run first.
func TestSubjectIdentityClaims_AreASubsetAndNeverTheSentinel(t *testing.T) {
	sources := relsource.All()
	require.NotEmpty(t, sources, "the bundle's init() must have registered the claim table")

	for _, src := range sources {
		owned := map[string]bool{}
		for _, c := range src.Claims {
			owned[c] = true
		}
		for _, c := range src.SubjectIdentityClaims {
			assert.True(t, owned[c],
				"source %q declares SubjectIdentityClaim %q outside its own Claims", src.Name, c)
			_, relation, ok := strings.Cut(c, "#")
			require.True(t, ok, "source %q declares malformed SubjectIdentityClaim %q", src.Name, c)
			assert.False(t, relsource.IsSentinelRelation(relation),
				"source %q declares the hash sentinel %q as an identity link; its subject is a string digest", src.Name, c)
		}
	}
}

// The identity-link declarations, source by source, each pinned to the tuple
// shape its own writer produces. Getting one wrong is invisible at runtime,
// which is the entire reason to assert them by name.
func TestSubjectIdentityClaims_MatchEachSourcesActualWriteShape(t *testing.T) {
	cases := []struct {
		name string
		src  relsource.Source
		want []string
	}{
		{
			// slack_user:<uid>#user@user:<canonical> is the one bare-user write
			// this kind makes. Its memberships take a slack_user#user USERSET.
			name: "slack: the slack_user identity link only, never a channel/workspace membership",
			src:  slack.DirectorySyncSource,
			want: []string{"slack_user#user"},
		},
		{
			// onepassword_group:<gid>#member@user:<canonical> — a bare user
			// subject, unlike every other kind's membership.
			name: "1password: group membership IS a direct user link",
			src:  onepassword.DirectorySyncSource,
			want: []string{"onepassword_group#member"},
		},
		{
			// Every github membership names a github_user#sole_user or
			// github_team#member userset, so NONE of them is probeable. An
			// empty declaration here is a positive statement, not a gap.
			name: "github: nothing — every membership it writes has a userset subject",
			src:  github.DirectorySyncSource,
			want: nil,
		},
		{
			// The useridentity attestation: github_user:<id>#user and
			// #sole_user, both @user:<canonical>. The agentsession claims on
			// this same source are excluded — their subject is a session.
			name: "typed writes: both github_user attestation edges, and neither agentsession edge",
			src:  spicedb.TypedWritesSource,
			want: []string{"github_user#user", "github_user#sole_user"},
		},
		{
			// C2's actual defect. memory_entry#creator is TOUCHed on every
			// memory Put by every user, so a reader walking Claims rendered one
			// console row per memory entry a person had ever created.
			name: "memory authorizer: nothing — memory_entry#creator is not an identity",
			src:  spicedbauthorizer.Source,
			want: nil,
		},
		{
			name: "pt_tag minter: nothing — a provenance tag is not an identity",
			src:  pttagmint.Source,
			want: nil,
		},
		{
			name: "leakage grants: nothing — a disclosure grant is not an identity",
			src:  approval.LeakageSource,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.ElementsMatch(t, tc.want, tc.src.SubjectIdentityClaims)
		})
	}
}

// Display is what a console row is attributed to, and Name ("githubdirectory
// sync", "spicedbtypedwrites") is not a thing to show a human. Every source
// that contributes a probe must therefore carry a DisplayName; one that
// contributes none may keep its Name, since nothing renders it.
func TestSubjectIdentityClaims_EveryProbingSourceHasAHumanName(t *testing.T) {
	for _, src := range relsource.All() {
		if len(src.SubjectIdentityClaims) == 0 {
			continue
		}
		assert.NotEmpty(t, src.DisplayName,
			"source %q contributes console rows but has no DisplayName, so they would be attributed to %q",
			src.Name, src.Display())
	}
}
