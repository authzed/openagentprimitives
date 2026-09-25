package github

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// relsyncMethodScopes maps every GitHub REST endpoint relsync_kind.go's
// SyncKind calls (Task 9's inventory, driven for real below rather than
// transcribed) to the OAuth scope GitHub's own REST reference documents for
// it — modeled on pkg/channels/channelkinds/slack/relsync_scopes_test.go,
// which exists because this repo shipped a manifest requesting neither
// scope its own sync path needed, once.
//
// GET /orgs/{org}/teams, GET /orgs/{org}/members, GET /teams/{team_id},
// GET /orgs/{org}/teams/{team_slug}/members,
// GET /orgs/{org}/teams/{team_slug}/teams and
// GET /orgs/{org}/teams/{team_slug}/repos are all organization-teams reads:
// GitHub's reference requires the classic read:org scope for each (a
// fine-grained token needs "organization members: read" instead — named
// alongside read:org in FeatureRequirement.Setup below). The last of them is
// how this sync reads a repository's team-granted access: the repository-side
// GET /repos/{owner}/{repo}/teams would need the fine-grained
// "Administration: read" permission this sync deliberately does not request,
// so the same edges are read from the team's side instead — see
// repoTeamGrantsCache in relsync_kind.go.
//
// GET /orgs/{org}/repos and GET /repos/{owner}/{repo}/collaborators are
// repository reads gated on the classic repo scope once the repository is
// private or internal (a fine-grained token needs "repository metadata: read",
// also named in Setup) — a public repository needs no scope at all, but
// enumerating the private and internal repositories a public crawl could never
// see is this sync's whole purpose.
var relsyncMethodScopes = map[string][]string{
	"GET /orgs/{org}/teams":                     {"read:org"},
	"GET /orgs/{org}/repos":                     {"repo"},
	"GET /orgs/{org}/members":                   {"read:org"},
	"GET /teams/{team_id}":                      {"read:org"},
	"GET /orgs/{org}/teams/{team_slug}/members": {"read:org"},
	"GET /orgs/{org}/teams/{team_slug}/teams":   {"read:org"},
	"GET /orgs/{org}/teams/{team_slug}/repos":   {"read:org"},
	"GET /repos/{owner}/{repo}/collaborators":   {"repo"},
}

// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed drives ListScopes and
// every FetchScope it enumerates against a fake GitHub server that records
// which endpoint PATTERN each request hit, then asserts
// channelfeatures.DirectorySync's declared Scopes cover every one of them —
// so a relsync call added later without a matching scope fails this test,
// not a cluster (see relsyncMethodScopes' own doc; this repo shipped exactly
// that gap once, for Slack).
func TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/teams")
		writeGHJSON(t, w, []map[string]any{{"id": 10, "slug": "eng"}})
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/repos")
		writeGHJSON(t, w, []map[string]any{
			{"id": 20, "name": "widgets", "visibility": "private", "owner": map[string]any{"login": "acme"}},
		})
	})
	mux.HandleFunc("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/members")
		writeGHItems(w, []int64{1})
	})
	mux.HandleFunc("/teams/10", func(w http.ResponseWriter, r *http.Request) {
		record("GET /teams/{team_id}")
		writeGHJSON(t, w, map[string]any{
			"slug":         "eng",
			"organization": map[string]any{"login": "acme"},
			"parent":       nil,
		})
	})
	mux.HandleFunc("/orgs/acme/teams/eng/members", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/teams/{team_slug}/members")
		writeGHItems(w, []int64{2})
	})
	mux.HandleFunc("/orgs/acme/teams/eng/teams", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/teams/{team_slug}/teams")
		writeGHJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		record("GET /repos/{owner}/{repo}/collaborators")
		writeGHJSON(t, w, []map[string]any{{"id": 3, "role_name": "write"}})
	})
	mux.HandleFunc("/orgs/acme/teams/eng/repos", func(w http.ResponseWriter, r *http.Request) {
		record("GET /orgs/{org}/teams/{team_slug}/repos")
		writeGHJSON(t, w, []map[string]any{{"id": 20, "role_name": "write"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	page, err := k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	require.True(t, page.Complete)
	require.Len(t, page.Scopes, 3, "fixture must mint exactly one org, one team, and one repo scope")

	for _, sc := range page.Scopes {
		_, err := k.FetchScope(ctx, params, sc)
		require.NoError(t, err)
	}

	called := slices.Compact(slices.Sorted(slices.Values(calls)))
	require.NotEmpty(t, called, "the drive made no GitHub call; the fixture is not exercising the relsync code path")

	req, ok := (&Kind{}).FeatureSupport()[channelfeatures.DirectorySync]
	require.True(t, ok, "the github kind must declare a requirement for channelfeatures.DirectorySync")
	declared := req.Scopes

	for _, method := range called {
		accepted, known := relsyncMethodScopes[method]
		require.Truef(t, known,
			"the relsync sync path calls %s, which relsyncMethodScopes does not cover — look "+
				"the method up in GitHub's REST reference and add the scope(s) it lists", method)
		for _, s := range accepted {
			assert.Containsf(t, declared, s,
				"%s needs %s, which channelfeatures.DirectorySync does not declare", method, s)
		}
	}
}
