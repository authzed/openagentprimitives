package github_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// Diff-and-prune is only correct when nothing else writes what this sync
// deletes, so the claims are exclusive and exhaustive: every relation
// FetchScope writes appears here.
func TestDirectorySyncSource_ClaimsEveryRelationItWrites(t *testing.T) {
	got := github.DirectorySyncSource.Claims
	assert.ElementsMatch(t, []string{
		"github_org#member", "github_org#relhash",
		"github_team#org", "github_team#direct_member", "github_team#subteam", "github_team#relhash",
		"github_repo#org", "github_repo#admin", "github_repo#maintain",
		"github_repo#write", "github_repo#triage", "github_repo#reader", "github_repo#relhash",
		"github_repo_url#repo",
	}, got)
}

// github_user#sole_user is the useridentity reconciler's, not this sync's:
// an attestation proves who you are upstream, never what you may do there.
func TestDirectorySyncSource_DoesNotClaimTheIdentityEdge(t *testing.T) {
	assert.NotContains(t, github.DirectorySyncSource.Claims, "github_user#sole_user")
	assert.NotContains(t, github.DirectorySyncSource.Claims, "github_user#user")
}

// github_repo_url#owner stays unclaimed: session grants write it, and this
// sync must not prune them.
func TestDirectorySyncSource_DoesNotClaimTheOwnerRelation(t *testing.T) {
	assert.NotContains(t, github.DirectorySyncSource.Claims, "github_repo_url#owner")
}

// The bundle is a hand-maintained transcription; a claim owner missing from
// it yields a partial table in every binary with the sentinel latched
// complete. The tree-walking guard in relsource/imports catches it
// generically; this asserts it for THIS package, so the failure names the
// package rather than the bundle.
func TestSourceIsRegistered(t *testing.T) {
	var found bool
	for _, s := range relsource.All() {
		if s.Name == "githubdirectorysync" {
			found = true
			assert.ElementsMatch(t, github.DirectorySyncSource.Claims, s.Claims,
				"the registered source's claims must match the declared var")
		}
	}
	assert.True(t, found, "init() must register this source; an unregistered claim owner is inert")
}
