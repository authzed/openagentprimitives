package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// ctx is shared across this file's tests — none of them need cancellation or
// a per-test deadline.
var ctx = context.Background()

// tok is a fixture credential value. None of this file's fake servers
// inspect it beyond confirming Authorization is set (doGet always sets it).
var tok = sensitive.NewSensitiveValue([]byte("test-token"))

// paramsFor builds relsync.SourceParams pointed at srv (or, when srv is nil,
// with no endpoint at all — for cases that must never reach the network,
// such as a foreign cursor rejected before any request is built) and cfg
// marshaled as spec.config.
func paramsFor(t *testing.T, srv *httptest.Server, cfg map[string]any) relsync.SourceParams {
	t.Helper()
	var endpoint string
	if srv != nil {
		endpoint = srv.URL
	}
	var raw json.RawMessage
	if cfg != nil {
		b, err := json.Marshal(cfg)
		require.NoError(t, err)
		raw = b
	}
	return relsync.SourceParams{Token: tok, Endpoint: endpoint, Config: raw}
}

// requestLog records every request path a fake server observed, guarded by a
// mutex since httptest serves handlers on their own goroutines.
type requestLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, r.URL.Path)
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.paths...)
}

// writeGHItems writes a bare JSON array of {"id": <id>} objects — the only
// field this kind's ghTeam/ghRepo decode, matching GitHub's own team/repo
// list response shape (a top-level array, no envelope).
func writeGHItems(w http.ResponseWriter, ids []int64) {
	items := make([]map[string]int64, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]int64{"id": id})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// writeGHJSON writes v as the response body, matching GitHub's own
// envelope-free shapes (a bare array for a list endpoint, a bare object for
// a single-resource one) — the FetchScope-side fixtures need richer item
// shapes than writeGHItems's bare {"id": ...} (a role_name, a permission, a
// slug), so this takes whatever shape a given test needs directly.
func writeGHJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}

// hasTuple reports whether tuples contains exactly want, compared field by
// field — the precise shape FetchScope's tests assert against, since a
// tuple's Key() alone would hide a mismatched SubjectRelation (the
// difference between #user and #sole_user this plan's brief calls out as
// safety-critical).
func hasTuple(tuples []spicedb.Tuple, want spicedb.Tuple) bool {
	for _, got := range tuples {
		if got == want {
			return true
		}
	}
	return false
}

// seedRepoCache constructs a *SyncKind with repoID already resolved to
// owner/name/visibility under package tok's fingerprint — standing in for a
// prior ListScopes call within the same pass under the SAME credential,
// which is the only way fetchRepoScope ever resolves a github_repo scope's
// forge id in production (see repoByIDCache's own doc). A unit test
// exercising FetchScope in isolation seeds it directly instead of standing
// up a second fake server for GET /orgs/{org}/repos. Every caller of this
// helper must reach FetchScope through paramsFor (package tok), since the
// cache lookup is scoped to the credential's fingerprint.
func seedRepoCache(repoID int64, owner, name, visibility string) *SyncKind {
	k := &SyncKind{}
	k.repoCache.put(tokenFingerprint(tok.UnderlyingValue()), repoID, ghRepo{ID: repoID, Name: name, Visibility: visibility, Owner: ghRepoOwner{Login: owner}})
	return k
}

// repoScope builds the relsync.Scope FetchScope receives for a github_repo
// id.
func repoScope(id int64) relsync.Scope {
	return relsync.Scope{ID: relsync.ScopeID(fmt.Sprintf("%d", id)), ResourceType: githubRepoResourceType}
}

// TestKind_RepoRolesAreKeyedByAccountIDOnSoleUser proves a repository
// collaborator's role lands on github_user:<id>#sole_user — never #user,
// which agentsession membership alone may consume (see
// githubUserSoleUserRelation's own doc and pkg/authz/guardian/schema's
// invariant test).
func TestKind_RepoRolesAreKeyedByAccountIDOnSoleUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "direct", r.URL.Query().Get("affiliation"))
		writeGHJSON(t, w, []map[string]any{{"id": 12345, "role_name": "write"}})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(555, "acme", "widgets", "private")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(555))
	require.NoError(t, err)

	assert.True(t, hasTuple(content.Tuples, spicedb.Tuple{
		ResourceType:    githubRepoResourceType,
		ResourceID:      "555",
		Relation:        "write",
		SubjectType:     githubUserResourceType,
		SubjectID:       "12345",
		SubjectRelation: githubUserSoleUserRelation,
	}), "expected github_repo:555#write@github_user:12345#sole_user among %+v", content.Tuples)
}

// TestKind_RepoTeamAccessIsSourcedFromTeamRepos proves a repository's
// team-granted access is read from the ORG-side endpoints
// (GET /orgs/{org}/teams then GET /orgs/{org}/teams/{slug}/repos), NOT from
// the repository-side GET /repos/{owner}/{repo}/teams. The repository-side
// endpoint needs the fine-grained "Administration: read" repository
// permission, which this sync deliberately does not request — its declared
// permission set is Members: read + Metadata: read (see githubSetupScopes) —
// so a read-only token that followed the setup flow got 403 on every repo's
// teams call. The org-side endpoints need only "Members: read", which that
// token already has. The resulting tuple still lands on the github_repo
// object, so the per-scope prune is unchanged.
//
// The repository-side teams endpoint is deliberately left UNREGISTERED: a
// regression that reintroduces the call becomes a 404 (→ ErrScopeGone →
// require.NoError fails) rather than a silent pass.
func TestKind_RepoTeamAccessIsSourcedFromTeamRepos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 99, "slug": "eng"}})
	})
	mux.HandleFunc("/orgs/acme/teams/eng/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 555, "role_name": "write"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(555, "acme", "widgets", "private")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(555))
	require.NoError(t, err)

	assert.True(t, hasTuple(content.Tuples, spicedb.Tuple{
		ResourceType:    githubRepoResourceType,
		ResourceID:      "555",
		Relation:        "write",
		SubjectType:     githubTeamResourceType,
		SubjectID:       "99",
		SubjectRelation: "member",
	}), "expected github_repo:555#write@github_team:99#member sourced from /orgs/acme/teams/eng/repos among %+v", content.Tuples)
}

// TestKind_UnknownTeamRoleIsSkippedNotFatal mirrors
// TestKind_UnknownCollaboratorRoleIsSkippedNotFatal for the team-granted arm:
// GET /orgs/{org}/teams/{slug}/repos reports a team's role in the SAME modern
// role_name vocabulary a collaborator does, so an org that defines a CUSTOM
// repository role gets that role's own name back here too. Failing the whole
// scope over it would cost far more than the one grant it cannot express (it
// disables the cross-resource reap for the whole source — see the collaborator
// test's own doc), so an unrecognized team role is skipped with a log, and
// every other grant on the repository still lands.
func TestKind_UnknownTeamRoleIsSkippedNotFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 7, "slug": "custom-team"}, {"id": 8, "slug": "eng"}})
	})
	mux.HandleFunc("/orgs/acme/teams/custom-team/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 777, "role_name": "security-reviewer"}}) // an org-defined custom role
	})
	mux.HandleFunc("/orgs/acme/teams/eng/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 777, "role_name": "admin"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(777, "acme", "widgets", "private")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(777))
	require.NoError(t, err,
		"a custom team repository role must not fail the scope: that silently disables the cross-resource reap for the whole source")

	assert.True(t, hasTuple(content.Tuples, spicedb.Tuple{
		ResourceType:    githubRepoResourceType,
		ResourceID:      "777",
		Relation:        "admin",
		SubjectType:     githubTeamResourceType,
		SubjectID:       "8",
		SubjectRelation: "member",
	}), "the known team grant alongside the custom one must still land: %+v", content.Tuples)

	for _, tup := range content.Tuples {
		assert.NotEqual(t, "7", tup.SubjectID,
			"a team role this schema cannot express must contribute NO tuple, never a guessed one: %+v", tup)
	}
}

// TestKind_NeverReportsAJoinMiss proves this kind's identity join is free —
// GitHub never reports an email, and this sync never needs one, because
// role subjects are written as github_user:<id>#sole_user and resolved
// later by the useridentity reconciler's attested edges, never here. Proved
// across all three scope kinds, since the claim is structural to FetchScope
// as a whole, not particular to one resource type.
func TestKind_NeverReportsAJoinMiss(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{1, 2, 3})
	})
	mux.HandleFunc("/teams/20", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, map[string]any{
			"slug":         "child-team",
			"organization": map[string]any{"login": "acme"},
			"parent":       nil,
		})
	})
	mux.HandleFunc("/orgs/acme/teams/child-team/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{4, 5})
	})
	mux.HandleFunc("/orgs/acme/teams/child-team/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 6, "role_name": "write"}})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	cases := []struct {
		name  string
		k     *SyncKind
		scope relsync.Scope
	}{
		{name: "org scope", k: &SyncKind{}, scope: relsync.Scope{ID: "acme", ResourceType: githubOrgResourceType}},
		{name: "team scope", k: &SyncKind{}, scope: relsync.Scope{ID: "20", ResourceType: githubTeamResourceType}},
		{name: "repo scope", k: seedRepoCache(555, "acme", "widgets", "private"), scope: repoScope(555)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, err := tc.k.FetchScope(ctx, params, tc.scope)
			require.NoError(t, err)
			assert.Zero(t, content.JoinMisses)
			assert.NotEmpty(t, content.Tuples, "precondition: the fetch must actually have produced content")
		})
	}
}

// TestKind_WritesTheURLToIDBridge proves FetchScope ties a repo scope to
// its github_repo_url bridge object — base64url(unpadded) of the canonical
// "https://github.com/<owner>/<name>" form, the same encoding
// pkg/authz/transforms.go's gitHubRepoURLID produces for every other
// spelling of this repository.
func TestKind_WritesTheURLToIDBridge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(777, "acme", "widgets", "private")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(777))
	require.NoError(t, err)

	wantURLID := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/acme/widgets"))
	assert.True(t, hasTuple(content.Tuples, spicedb.Tuple{
		ResourceType: githubRepoURLResourceType,
		ResourceID:   wantURLID,
		Relation:     "repo",
		SubjectType:  githubRepoResourceType,
		SubjectID:    "777",
	}), "expected github_repo_url:%s#repo@github_repo:777 among %+v", wantURLID, content.Tuples)
}

// TestKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites closes the loop
// between what this kind DECLARES a reader should join against and what its
// fetch actually writes. The two are separately authored — a declaration in
// ScopeLabelBridges, a tuple in fetchRepoScope — and a mismatch between them
// is completely silent: the console reads a relation nobody writes, finds
// nothing, and renders raw ids, which is indistinguishable from a kind that
// declares no bridge at all. So the test derives the expectation from a real
// fetch rather than restating the constants.
func TestKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 1, "role_name": "admin"}})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(777, "acme", "widgets", "private")
	content, err := k.FetchScope(ctx, paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}}), repoScope(777))
	require.NoError(t, err)

	var written spicedb.Tuple
	for _, tup := range content.Tuples {
		if tup.ResourceType == githubRepoURLResourceType {
			written = tup
			break
		}
	}
	require.NotEmpty(t, written.ResourceType, "the fetch must write a bridge tuple for this test to mean anything")

	bridges := k.ScopeLabelBridges()
	require.Len(t, bridges, 1)
	b := bridges[0]
	assert.Equal(t, written.ResourceType, b.BridgeDefinition, "the declared bridge must name the definition the sync writes")
	assert.Equal(t, written.Relation, b.BridgeRelation, "…and the relation it writes")
	assert.Equal(t, written.SubjectType, b.ScopeDefinition, "…and the scope definition that tuple's SUBJECT is")

	// The decoder has to actually name the id the sync minted. A bridge wired
	// to the right relation but the wrong decoder resolves nothing, and fails
	// in exactly the same invisible way.
	title, href := b.Decoder.Decode(written.ResourceID)
	assert.Equal(t, "acme/widgets", title, "the declared decoder must name the id repoURLObjectID actually minted")
	assert.Equal(t, "https://github.com/acme/widgets", href)
}

// The other two scope definitions declare no bridge, and that is a positive
// claim rather than an omission: github_org is keyed by the login, which IS
// the name, and this kind stores no team slug anywhere for github_team. A
// bridge invented for either would read a relation nothing writes.
func TestKind_ScopeLabelBridgesCoverOnlyTheDefinitionWithAStoredName(t *testing.T) {
	k := &SyncKind{}
	scoped := map[string]bool{}
	for _, b := range k.ScopeLabelBridges() {
		scoped[b.ScopeDefinition] = true
	}
	assert.True(t, scoped[githubRepoResourceType], "the numeric repo id is the one that needs naming")
	assert.False(t, scoped[githubOrgResourceType], "an org login is already its own name")
	assert.False(t, scoped[githubTeamResourceType], "nothing stores a team's slug, so there is no bridge to read")
}

// TestKind_URLBridgeIsSingular proves the KIND's half of the singular-bridge
// guarantee: FetchScope returns EXACTLY ONE github_repo_url#repo tuple for a
// repo scope, never more.
//
// The engine's half — that a STALE bridge edge on the same URL object,
// pointing at a DIFFERENT (old) repo id, is reaped once a pass achieves
// full fetch coverage — is relsync.Pass's cross-resource reap
// (reapAbsentCrossResourceTuples), already covered by that package's own
// tests, and is not re-tested here: a repo scope's ResourceType is
// github_repo, so the per-scope diff this kind's own fetch feeds is bounded
// to github_repo objects and can never see, let alone delete, a
// github_repo_url tuple itself. See this plan's task-8 brief correction.
func TestKind_URLBridgeIsSingular(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 1, "role_name": "admin"}})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(888, "acme", "widgets", "internal")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(888))
	require.NoError(t, err)

	var bridgeCount int
	for _, tup := range content.Tuples {
		if tup.ResourceType == githubRepoURLResourceType {
			bridgeCount++
		}
	}
	assert.Equal(t, 1, bridgeCount, "expected exactly one github_repo_url#repo tuple, got %+v", content.Tuples)
}

// TestKind_PublicRepoWritesNoWildcardReader proves a public repository never
// gets a `user:*` reader arm: pttagmint intersects raw subject ids as opaque
// strings, so a wildcard would arrive as the literal id "*" and match
// nobody — see toolkits/gh.yaml's own note on `reader` carrying no `user:*`
// arm.
func TestKind_PublicRepoWritesNoWildcardReader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{{"id": 1, "role_name": "read"}})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(999, "acme", "widgets", "public")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(999))
	require.NoError(t, err)
	require.NotEmpty(t, content.Tuples, "precondition: the fetch must actually have produced content, or the loop below passes vacuously")

	for _, tup := range content.Tuples {
		assert.NotEqual(t, "*", tup.SubjectID, "no tuple may carry a wildcard subject id: %+v", tup)
	}
}

// TestKind_SubteamEdgeIsWrittenFromTheParentsOwnFetch proves nested teams
// write only a direct edge on each object — the child's #direct_member
// tuples name only its own scope, never the child's members flattened onto
// the parent, which the schema's own recursive permission
// (`member = direct_member + subteam->member`) already expresses — and that
// the #subteam edge is emitted by the PARENT'S fetch.
//
// MAJOR (final whole-branch review): it used to be emitted by the CHILD's
// fetch, from the parent field on the child's team detail, which silently
// collapsed the whole nesting. The parent is a scope of the same type, and
// readScopeTuples reads every non-relhash relation on the parent object, so
// the parent's own diff saw a tuple its own fetch never reported and DELETED
// it. Nothing protected it: recordExtraResources does fold it into the
// cross-resource union, and Pass then EXCLUDES that union entry precisely
// because github_team is itself a scope type. So the parent gaining a member
// (any change to its hash) dropped #subteam@20, and if the child's own
// content was unchanged its sentinel short-circuited and never re-asserted
// it — every member of team 20 losing what they inherited from team 10, with
// recovery depending on enumeration order.
//
// The regression assertion is the second one: the child's fetch must emit
// NOTHING on the parent's object.
func TestKind_SubteamEdgeIsWrittenFromTheParentsOwnFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/teams/10", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, map[string]any{
			"slug":         "parent-team",
			"organization": map[string]any{"login": "acme"},
			"parent":       nil,
		})
	})
	mux.HandleFunc("/orgs/acme/teams/parent-team/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{222})
	})
	mux.HandleFunc("/orgs/acme/teams/parent-team/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{20})
	})
	mux.HandleFunc("/teams/20", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, map[string]any{
			"slug":         "child-team",
			"organization": map[string]any{"login": "acme"},
			"parent":       map[string]any{"id": 10},
		})
	})
	mux.HandleFunc("/orgs/acme/teams/child-team/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{111})
	})
	mux.HandleFunc("/orgs/acme/teams/child-team/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	parent, err := k.FetchScope(ctx, params, relsync.Scope{ID: "10", ResourceType: githubTeamResourceType})
	require.NoError(t, err)
	child, err := k.FetchScope(ctx, params, relsync.Scope{ID: "20", ResourceType: githubTeamResourceType})
	require.NoError(t, err)

	assert.True(t, hasTuple(parent.Tuples, spicedb.Tuple{
		ResourceType: githubTeamResourceType,
		ResourceID:   "10",
		Relation:     "subteam",
		SubjectType:  githubTeamResourceType,
		SubjectID:    "20",
	}), "the parent's own fetch must report github_team:10#subteam@github_team:20, or its own diff deletes it: %+v", parent.Tuples)

	for _, tup := range child.Tuples {
		assert.NotEqual(t, "10", tup.ResourceID,
			"the child's fetch must assert nothing on the parent's object; the parent's diff would reap whatever it did not itself report: %+v", tup)
	}

	assert.True(t, hasTuple(child.Tuples, spicedb.Tuple{
		ResourceType:    githubTeamResourceType,
		ResourceID:      "20",
		Relation:        "direct_member",
		SubjectType:     githubUserResourceType,
		SubjectID:       "111",
		SubjectRelation: githubUserSoleUserRelation,
	}), "expected the child's own direct_member tuple among %+v", child.Tuples)

	for _, tup := range parent.Tuples {
		if tup.Relation == "direct_member" {
			assert.Equal(t, "222", tup.SubjectID,
				"the parent must carry only its OWN members, never the child's flattened onto it: %+v", tup)
		}
	}
}

// TestKind_OnlyInternalReposRelateToTheOrg proves visibility gates the
// #org edge: private gets none, and internal gets exactly one.
func TestKind_OnlyInternalReposRelateToTheOrg(t *testing.T) {
	cases := []struct {
		name       string
		visibility string
		wantOrgTup bool
	}{
		{name: "private repo: no #org tuple", visibility: "private", wantOrgTup: false},
		{name: "internal repo: exactly one #org tuple", visibility: "internal", wantOrgTup: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
				writeGHJSON(t, w, []map[string]any{})
			})
			mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
				writeGHJSON(t, w, []map[string]any{})
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			repoID := int64(1000 + i)
			k := seedRepoCache(repoID, "acme", "widgets", tc.visibility)
			params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

			content, err := k.FetchScope(ctx, params, repoScope(repoID))
			require.NoError(t, err)

			got := hasTuple(content.Tuples, spicedb.Tuple{
				ResourceType: githubRepoResourceType,
				ResourceID:   fmt.Sprintf("%d", repoID),
				Relation:     "org",
				SubjectType:  githubOrgResourceType,
				SubjectID:    "acme",
			})
			assert.Equal(t, tc.wantOrgTup, got, "content: %+v", content.Tuples)
		})
	}
}

// TestKind_FetchRepoScopeRequiresAPriorEnumeration proves a repo-cache miss
// fails loudly rather than guessing or silently reporting empty content —
// see repoByIDCache's own doc: relsync.Pass always enumerates a scope
// before fetching it, so this path is only reachable when FetchScope is
// called directly without that, which must never look like success.
func TestKind_FetchRepoScopeRequiresAPriorEnumeration(t *testing.T) {
	k := &SyncKind{}
	params := paramsFor(t, nil, map[string]any{"orgs": []string{"acme"}})

	_, err := k.FetchScope(ctx, params, repoScope(4242))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4242")
}

// TestKind_RepoCacheIsScopedPerCredentialNotJustRepoID proves repoCache
// cannot leak across two RelationshipSource CRs sharing this package's one
// registered *SyncKind singleton. Forge repo ids are unique only WITHIN one
// install: github.com has its own global id space, and each GitHub
// Enterprise Server host has its own independent one, so a CR pointed at
// github.com and a CR pointed at a GHES install can legitimately both use
// repo id 12345 for two completely UNRELATED repositories. Mirrors
// onepassword's tokenFingerprint-scoped userCache
// (pkg/channels/channelkinds/onepassword/relsync_kind.go) — resolving one
// credential's scope must never read back what a DIFFERENT credential
// cached for the same numeric id.
func TestKind_RepoCacheIsScopedPerCredentialNotJustRepoID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/repos/other-corp/internal-tool/collaborators", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/orgs/other-corp/teams", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	tokA := sensitive.NewSensitiveValue([]byte("token-a"))
	tokB := sensitive.NewSensitiveValue([]byte("token-b"))

	// CR-A's ListScopes decode: repo id 12345 is acme/widgets, public.
	k.repoCache.put(tokenFingerprint(tokA.UnderlyingValue()), 12345, ghRepo{ID: 12345, Name: "widgets", Visibility: "public", Owner: ghRepoOwner{Login: "acme"}})
	// CR-B's ListScopes decode, sharing this package's one registered
	// singleton: the SAME numeric id, naming a completely unrelated
	// repository on a different install/credential.
	k.repoCache.put(tokenFingerprint(tokB.UnderlyingValue()), 12345, ghRepo{ID: 12345, Name: "internal-tool", Visibility: "internal", Owner: ghRepoOwner{Login: "other-corp"}})

	paramsA := relsync.SourceParams{Token: tokA, Endpoint: srv.URL, Config: json.RawMessage(`{"orgs":["acme"]}`)}
	paramsB := relsync.SourceParams{Token: tokB, Endpoint: srv.URL, Config: json.RawMessage(`{"orgs":["other-corp"]}`)}

	contentA, err := k.FetchScope(ctx, paramsA, repoScope(12345))
	require.NoError(t, err)

	wantAURLID := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/acme/widgets"))
	assert.True(t, hasTuple(contentA.Tuples, spicedb.Tuple{
		ResourceType: githubRepoURLResourceType,
		ResourceID:   wantAURLID,
		Relation:     "repo",
		SubjectType:  githubRepoResourceType,
		SubjectID:    "12345",
	}), "credential A's fetch must resolve its OWN repository (acme/widgets), never a different credential's entry for the same numeric id: %+v", contentA.Tuples)
	for _, tup := range contentA.Tuples {
		assert.NotEqual(t, "other-corp", tup.SubjectID, "credential A must never see credential B's cached repository: %+v", tup)
	}

	// Symmetric: credential B must resolve ITS OWN repository too, not A's
	// — proving isolation runs both directions, not just that A happens to
	// win a race.
	contentB, err := k.FetchScope(ctx, paramsB, repoScope(12345))
	require.NoError(t, err)

	wantBURLID := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/other-corp/internal-tool"))
	assert.True(t, hasTuple(contentB.Tuples, spicedb.Tuple{
		ResourceType: githubRepoURLResourceType,
		ResourceID:   wantBURLID,
		Relation:     "repo",
		SubjectType:  githubRepoResourceType,
		SubjectID:    "12345",
	}), "credential B's fetch must resolve its OWN repository (other-corp/internal-tool): %+v", contentB.Tuples)
	for _, tup := range contentB.Tuples {
		assert.NotEqual(t, "acme", tup.SubjectID, "credential B must never see credential A's cached repository: %+v", tup)
	}
}

// TestRepoByIDCache_ClearForCredentialOnlyDropsThatCredentialsEntries tests
// repoByIDCache directly (not through FetchScope): clearForCredential must
// remove exactly the calling credential's entries and leave every OTHER
// credential's entries in this shared singleton untouched. A prior version
// of this fix reintroduced a size-cap reset that clears EVERYTHING
// regardless of fingerprint; TestKind_RepoCacheIsScopedPerCredentialNotJustRepoID
// alone could not catch that regression because it only calls
// seedRepoCache/FetchScope directly and never reaches clearForCredential —
// this test exercises the method itself.
func TestRepoByIDCache_ClearForCredentialOnlyDropsThatCredentialsEntries(t *testing.T) {
	var c repoByIDCache
	c.put("fpA", 1, ghRepo{Name: "a"})
	c.put("fpB", 1, ghRepo{Name: "b"})

	c.clearForCredential("fpA")

	_, okA := c.get("fpA", 1)
	assert.False(t, okA, "fpA's own entry must be cleared")
	rB, okB := c.get("fpB", 1)
	require.True(t, okB, "fpB's entry must survive clearing fpA")
	assert.Equal(t, "b", rB.Name)
}

// TestKind_ListScopesClearsOnlyThisCredentialsStaleRepoCacheEachCycle pins
// the CALL SITE, not just the method: two fresh (token == "") ListScopes
// cycles for ONE credential, where cycle 2's upstream repo set drops a
// repository cycle 1 had. A FetchScope for that dropped id must fail after
// cycle 2 — if ListScopes' fresh-cycle branch stopped calling
// clearForCredential (or clearForCredential itself regressed to a no-op),
// the stale cycle-1 entry would still resolve and this would wrongly
// succeed.
func TestKind_ListScopesClearsOnlyThisCredentialsStaleRepoCacheEachCycle(t *testing.T) {
	var reposCall int
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, nil)
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		reposCall++
		if reposCall == 1 {
			// Cycle 1: repos 111 and 222 both visible.
			writeGHJSON(t, w, []map[string]any{
				{"id": 111, "name": "widgets", "visibility": "private", "owner": map[string]any{"login": "acme"}},
				{"id": 222, "name": "gadgets", "visibility": "private", "owner": map[string]any{"login": "acme"}},
			})
			return
		}
		// Cycle 2: repo 111 no longer appears upstream (gone, or the token
		// lost visibility) — only 222 remains.
		writeGHJSON(t, w, []map[string]any{
			{"id": 222, "name": "gadgets", "visibility": "private", "owner": map[string]any{"login": "acme"}},
		})
	})
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/repos/acme/widgets/teams", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/repos/acme/gadgets/collaborators", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	mux.HandleFunc("/repos/acme/gadgets/teams", func(w http.ResponseWriter, r *http.Request) { writeGHJSON(t, w, []map[string]any{}) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	_, err := k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	_, err = k.FetchScope(ctx, params, repoScope(111))
	require.NoError(t, err, "precondition: cycle 1 must have cached repo 111")

	_, err = k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)

	_, err = k.FetchScope(ctx, params, repoScope(111))
	assert.Error(t, err, "cycle 2 must have cleared this credential's stale repo-111 entry from cycle 1, since repo 111 no longer appears upstream this cycle")
}

// TestKind_FetchScopePagesUpstreamMembersToCompletion proves fetchAllGH
// actually follows a paginated FetchScope-side list to its END, not just
// its first page — the constraint the task brief called the most
// damaging: "a partial member... list must never be reported... the
// per-scope prune deletes whatever this call does not report." Every OTHER
// FetchScope fixture in this file returns one page with no Link header, so
// none of them can catch a regression here — deleting fetchAllGH's own
// `next = link` line must fail this test specifically.
func TestKind_FetchScopePagesUpstreamMembersToCompletion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "2":
			writeGHItems(w, []int64{2})
		default:
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/orgs/acme/members?page=2>; rel="next"`, r.Host))
			writeGHItems(w, []int64{1})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, relsync.Scope{ID: "acme", ResourceType: githubOrgResourceType})
	require.NoError(t, err)

	var sawPage1, sawPage2 bool
	for _, tup := range content.Tuples {
		switch tup.SubjectID {
		case "1":
			sawPage1 = true
		case "2":
			sawPage2 = true
		}
	}
	assert.True(t, sawPage1, "expected the member from page 1: %+v", content.Tuples)
	assert.True(t, sawPage2, "expected the member from page 2 — proves the second page was actually fetched, not just page 1 returned: %+v", content.Tuples)
}

// TestKind_FetchScopeIsAtomicAcrossPages proves a failure on any page after
// the first returns NO content and an error — never page one's partial
// result, which the per-scope prune would otherwise treat as this scope's
// complete membership and delete everyone page two would have reported.
// Mirrors onepassword's own TestKind_FetchScopeIsAtomicAcrossPages.
func TestKind_FetchScopeIsAtomicAcrossPages(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/orgs/acme/members?page=2>; rel="next"`, r.Host))
			writeGHItems(w, []int64{1})
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // page 2 fails
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, relsync.Scope{ID: "acme", ResourceType: githubOrgResourceType})

	require.Error(t, err, "a scope whose second page failed must error, never return page one")
	assert.Equal(t, relsync.ScopeContent{}, content, "the atomicity contract: a failed scope reports nothing, or the prune deletes what it never read")
	assert.Equal(t, 2, hits, "both pages must have actually been attempted")
}

// TestKind_OrgLoginCasingNeverSplitsTheOrgObject proves an org resolves to
// ONE github_org object regardless of which of the two independent sources
// (the operator's own spec.config.orgs, or GitHub's own reported org
// login) supplied the casing, and pins BOTH canonicalization call sites:
// the operator's config is fed in MIXED case ("ACME-Corp"), distinct from
// GitHub's own reported login ("Acme-Corp") below, so removing EITHER
// canonicalOrgLogin call — the mint site in ListScopes or the one in
// fetchTeamScope — fails this test. (A prior version of this test fed an
// already-lowercase config value, which made the mint-site call's removal
// invisible: lowercasing a lowercase string is a no-op.)
func TestKind_OrgLoginCasingNeverSplitsTheOrgObject(t *testing.T) {
	mux := http.NewServeMux()
	// fetchOrgScope canonicalizes defensively regardless of what ListScopes
	// minted, so its own HTTP call always lands on the canonical path.
	mux.HandleFunc("/orgs/acme-corp/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{1})
	})
	mux.HandleFunc("/teams/20", func(w http.ResponseWriter, r *http.Request) {
		// GitHub's own reported casing — different from BOTH the operator's
		// mixed-case config below AND from each other.
		writeGHJSON(t, w, map[string]any{
			"slug":         "eng",
			"organization": map[string]any{"login": "Acme-Corp"},
			"parent":       nil,
		})
	})
	mux.HandleFunc("/orgs/acme-corp/teams/eng/members", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{2})
	})
	// Same canonicalized org path as the members route above: the child
	// listing is built from the SAME `org` variable, so a regression that
	// dropped canonicalization for one would drop it for both.
	mux.HandleFunc("/orgs/acme-corp/teams/eng/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	// ListScopes' own (uncanonicalized) org variable builds these two paths
	// directly from spec.config's mixed-case spelling — see freshPathFor's
	// call site in ListScopes, which never applies canonicalOrgLogin (only
	// the SCOPE ID it mints does): so these routes must match the
	// operator's ORIGINAL casing, not the lowercased one.
	mux.HandleFunc("/orgs/ACME-Corp/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, nil)
	})
	mux.HandleFunc("/orgs/ACME-Corp/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, nil)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The operator's own spelling: MIXED case, and different from GitHub's
	// own reported login below — pins that both independent sources fold
	// to the identical canonical form.
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"ACME-Corp"}})

	orgPage, err := (&SyncKind{}).ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	var orgScopeID relsync.ScopeID
	for _, s := range orgPage.Scopes {
		if s.ResourceType == githubOrgResourceType {
			orgScopeID = s.ID
		}
	}
	require.Equal(t, relsync.ScopeID("acme-corp"), orgScopeID,
		"the org scope's own id must be canonicalized at the MINT site (ListScopes), not left as the operator's mixed-case spec.config spelling")

	orgContent, err := (&SyncKind{}).FetchScope(ctx, params, relsync.Scope{ID: orgScopeID, ResourceType: githubOrgResourceType})
	require.NoError(t, err)
	require.Len(t, orgContent.Tuples, 1)
	orgResourceID := orgContent.Tuples[0].ResourceID

	teamContent, err := (&SyncKind{}).FetchScope(ctx, params, relsync.Scope{ID: "20", ResourceType: githubTeamResourceType})
	require.NoError(t, err)

	var orgEdgeSubject string
	for _, tup := range teamContent.Tuples {
		if tup.ResourceType == githubTeamResourceType && tup.Relation == "org" {
			orgEdgeSubject = tup.SubjectID
		}
	}
	require.NotEmpty(t, orgEdgeSubject, "expected a github_team#org tuple")

	assert.Equal(t, orgResourceID, orgEdgeSubject,
		"the org scope's own resource id (%q) and the team's #org edge subject (%q) must name the SAME SpiceDB object despite GitHub and the operator each reporting different casing",
		orgResourceID, orgEdgeSubject)
}

// TestKind_ListScopesEnumeratesOnlyConfiguredOrgs proves the synced set is
// the manifest's, not the credential's reach: a server that would answer for
// an org outside spec.config is set up here too (implicitly, by there being
// no handler for anything else), and every observed request path is
// asserted to name only the configured org.
func TestKind_ListScopesEnumeratesOnlyConfiguredOrgs(t *testing.T) {
	var log requestLog
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeGHItems(w, []int64{1})
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeGHItems(w, []int64{2})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	page, err := k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	assert.True(t, page.Complete)

	for _, p := range log.all() {
		assert.Contains(t, p, "acme", "every request must name a configured org, got %q", p)
	}
	assert.NotEmpty(t, log.all(), "precondition: the fake server must have been hit at all")

	var gotOrg, gotTeam, gotRepo bool
	for _, s := range page.Scopes {
		switch s.ResourceType {
		case githubOrgResourceType:
			assert.Equal(t, relsync.ScopeID("acme"), s.ID)
			gotOrg = true
		case githubTeamResourceType:
			gotTeam = true
		case githubRepoResourceType:
			gotRepo = true
		}
	}
	assert.True(t, gotOrg, "expected a github_org scope for acme")
	assert.True(t, gotTeam, "expected a github_team scope")
	assert.True(t, gotRepo, "expected a github_repo scope")
}

// TestKind_RefusesAnEmptyOrgList proves a kind with no orgs configured
// refuses rather than syncing nothing silently or enumerating everything the
// token can see.
func TestKind_RefusesAnEmptyOrgList(t *testing.T) {
	_, err := (&SyncKind{}).ListScopes(ctx, relsync.SourceParams{Token: tok}, relsync.Cursor{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orgs")
}

// TestKind_ListScopesFollowsTheLinkHeader proves GitHub's opaque Link header
// drives resumption: page one returns Complete=false with a cursor tagged
// "github"; feeding that cursor back reaches page two of the SAME teams
// list, then (having nothing left to resume) walks the org's repos to
// completion within that same call.
func TestKind_ListScopesFollowsTheLinkHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "2":
			writeGHItems(w, []int64{20})
		default:
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/orgs/acme/teams?page=2>; rel="next"`, r.Host))
			writeGHItems(w, []int64{10})
		}
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{30})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	page1, err := k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	assert.False(t, page1.Complete)
	require.Equal(t, KindName, page1.Next.Kind)
	assert.NotEmpty(t, page1.Next.Token)

	page2, err := k.ListScopes(ctx, params, page1.Next)
	require.NoError(t, err)
	assert.True(t, page2.Complete)

	var sawPage2Team bool
	for _, s := range page2.Scopes {
		if s.ResourceType == githubTeamResourceType && s.ID == "20" {
			sawPage2Team = true
		}
	}
	assert.True(t, sawPage2Team, "expected page 2's team (id 20) among the resumed scopes")
}

// TestKind_RefusesAForeignCursor proves a cursor minted by another kind is
// refused, not interpreted — checked before any request is built, so no
// server is needed at all.
func TestKind_RefusesAForeignCursor(t *testing.T) {
	params := paramsFor(t, nil, map[string]any{"orgs": []string{"acme"}})
	_, err := (&SyncKind{}).ListScopes(ctx, params, relsync.Cursor{Kind: "slack", Token: "x"})
	require.Error(t, err)
}

// TestKind_ListScopesCoversOrgTeamAndRepo proves three resource types live
// under one kind: a single org, one team and one repo, walked to completion.
func TestKind_ListScopesCoversOrgTeamAndRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{1})
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		writeGHItems(w, []int64{2})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	page, err := k.ListScopes(ctx, params, relsync.Cursor{})
	require.NoError(t, err)
	assert.True(t, page.Complete)

	types := map[string]bool{}
	for _, s := range page.Scopes {
		types[s.ResourceType] = true
	}
	assert.True(t, types[githubOrgResourceType], "expected a github_org scope")
	assert.True(t, types[githubTeamResourceType], "expected a github_team scope")
	assert.True(t, types[githubRepoResourceType], "expected a github_repo scope")
}

// A cursor token is not trusted input: it round-trips through the CR's own
// status, where a hand-edit (or an older writer) can put anything JSON
// accepts. A negative orgIndex passed the `< len(cfg.Orgs)` loop condition
// and panicked on the index that followed, taking the operator down rather
// than failing this one source.
func TestKind_NegativeOrgIndexCursorIsRefusedNotPanicked(t *testing.T) {
	k := &SyncKind{}
	params := paramsFor(t, nil, map[string]any{"orgs": []string{"acme"}})

	_, err := k.ListScopes(ctx, params, relsync.Cursor{
		Kind:  KindName,
		Token: `{"orgIndex":-1,"phase":"teams"}`,
	})

	require.Error(t, err, "a negative orgIndex must be refused")
	assert.Contains(t, err.Error(), "orgIndex", "the refusal must name the field, or an operator cannot tell which part of the cursor is bad")
}

// MAJOR (final whole-branch review): an org using CUSTOM repository roles
// gets that custom role's name back in `role_name`, and failing the whole
// scope over it cost far more than the one grant it could not express. The
// repository got no tuples at all (fail-closed, and fine on its own), Ready
// stayed Synced because a scope error is non-fatal — and fetchCoverage could
// never again reach len(scopes), which is the gate the cross-resource bridge
// reap runs behind. One custom role therefore disabled reaping for the whole
// source, permanently.
//
// Skipped with a log instead: the rest of that repository's grants still
// land, and coverage stays intact.
func TestKind_UnknownCollaboratorRoleIsSkippedNotFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/collaborators", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{
			{"id": 1, "role_name": "security-reviewer"}, // an org-defined custom role
			{"id": 2, "role_name": "admin"},
		})
	})
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		writeGHJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := seedRepoCache(777, "acme", "widgets", "private")
	params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

	content, err := k.FetchScope(ctx, params, repoScope(777))
	require.NoError(t, err,
		"a custom repository role must not fail the scope: that silently disables the cross-resource reap for the whole source")

	assert.True(t, hasTuple(content.Tuples, spicedb.Tuple{
		ResourceType:    githubRepoResourceType,
		ResourceID:      "777",
		Relation:        "admin",
		SubjectType:     githubUserResourceType,
		SubjectID:       "2",
		SubjectRelation: githubUserSoleUserRelation,
	}), "the known role alongside the custom one must still land: %+v", content.Tuples)

	for _, tup := range content.Tuples {
		assert.NotEqual(t, "1", tup.SubjectID,
			"a role this schema cannot express must contribute NO tuple, never a guessed one: %+v", tup)
	}
}
