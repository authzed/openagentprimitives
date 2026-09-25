package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// startDirectoryTestServer points directoryClientFactory at an httptest
// server backed by mux for the life of t, restoring the real factory on
// cleanup. A real *slackapi.Client drives every request (via
// slackapi.OptionAPIURL), never the shared fakeslack double: fakeslack
// answers per Go method call with no notion of "page 2 of this call
// fails", which is exactly the atomicity property this file's tests exist
// to prove.
func startDirectoryTestServer(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	orig := directoryClientFactory
	directoryClientFactory = func(creds relsync.SourceParams) slackDirectoryClient {
		return slackapi.New(string(creds.Token.UnderlyingValue()), slackapi.OptionAPIURL(srv.URL+"/"))
	}
	t.Cleanup(func() { directoryClientFactory = orig })

	// Disable proactive pacing for the life of the test: pacing under Slack's
	// real per-minute tiers would add seconds of real sleeping to every
	// multi-call test. The one test that asserts WHICH method was paced
	// installs its own recordingPacer over this (installPacer), and the
	// tierLimiter's own gating is proven directly in relsync_ratelimit_test.go.
	origPacer := directoryPacer
	directoryPacer = noopPacer{}
	t.Cleanup(func() { directoryPacer = origPacer })
}

// testCreds is a fixture credential; the token value is never inspected by
// the fake server, only forwarded as a form field.
func testCreds() relsync.SourceParams {
	return relsync.SourceParams{Token: sensitive.NewSensitiveValue([]byte("xoxb-test-token"))}
}

func jsonHandler(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// referencesSlackUser reports whether uid appears anywhere in tuples —
// either as the slack_user resource itself (the identity link) or as a
// slack_user#user subject (a channel or workspace membership tuple). Used
// to assert a dropped member left NO trace, not merely that one relation
// omitted it.
func referencesSlackUser(tuples []spicedb.Tuple, uid string) bool {
	for _, tup := range tuples {
		if tup.ResourceType == slackUserResourceType && tup.ResourceID == uid {
			return true
		}
		if tup.SubjectType == slackUserResourceType && tup.SubjectID == uid {
			return true
		}
	}
	return false
}

// The identity join: an upstream email becomes a canonical subject, never a
// raw email as an object id.
func TestSlackKind_JoinsMembersByCanonicalizedEmail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok":      true,
		"channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok":                true,
		"members":           []string{"U1"},
		"response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok":   true,
		"user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "Alice@Example.com"}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	wantCanonical, canonErr := identity.EmailReference(identity.Email("alice@example.com")).Canonical()
	require.NoError(t, canonErr, "precondition: a non-empty email must canonicalize")

	assert.Contains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackUserResourceType, ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: wantCanonical.String(),
	}, "the identity link must carry the CANONICALIZED email as the subject id")

	assert.NotContains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackUserResourceType, ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "Alice@Example.com",
	}, "the raw email must never appear as a SpiceDB object id")
	assert.NotContains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackUserResourceType, ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice@example.com",
	}, "the lower-cased-but-not-encoded email must never appear as a SpiceDB object id either")

	assert.Contains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackChannelResourceType, ResourceID: "C1", Relation: "member",
		SubjectType: slackUserResourceType, SubjectID: "U1", SubjectRelation: "user",
	})
	assert.Equal(t, 0, content.JoinMisses)
}

// Credentials.Endpoint exists for a kind whose service is customer-hosted
// (the 1Password SCIM Bridge). Slack's API host is fixed by
// directoryClientFactory (slackapi.OptionAPIURL), never by this field, so
// this kind must behave identically whether Endpoint is empty or set to
// anything at all — including a value that is not even a valid URL.
func TestSlackKind_IgnoresTheEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok":      true,
		"channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok":                true,
		"members":           []string{"U1"},
		"response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok":   true,
		"user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "alice@example.com"}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	creds := testCreds()
	creds.Endpoint = "ignored" // not a URL Slack would ever accept — must have no effect

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), creds, relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err, "a non-empty Endpoint must not divert this kind from the fixed Slack API host directoryClientFactory already points at")

	wantCanonical, canonErr := identity.EmailReference(identity.Email("alice@example.com")).Canonical()
	require.NoError(t, canonErr, "precondition: a non-empty email must canonicalize")

	assert.Contains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackUserResourceType, ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: wantCanonical.String(),
	})
	assert.Equal(t, 0, content.JoinMisses)
}

// A member whose email matches no platform user gets NO edge — they
// resolve to nobody and the gate refuses, which is the correct direction.
// But the miss is counted, because writing 17 of 20 silently is the shape
// the no-silent-errors rule exists to prevent.
func TestSlackKind_UnresolvableMemberIsDroppedAndCounted(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok": true, "channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1", "U2"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var profile map[string]any
		switch r.FormValue("user") {
		case "U1":
			profile = map[string]any{"email": "bob@example.com"}
		case "U2":
			// No email visible for this member (no users:read.email scope
			// coverage for them, or a foreign-workspace guest) — resolves
			// to nobody.
			profile = map[string]any{"email": ""}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "user": map[string]any{"id": r.FormValue("user"), "is_bot": false, "profile": profile},
		})
	})
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	assert.Equal(t, 1, content.JoinMisses, "exactly the email-less member must be counted as a miss")
	assert.True(t, referencesSlackUser(content.Tuples, "U1"), "the resolvable member must still be written")
	assert.False(t, referencesSlackUser(content.Tuples, "U2"), "the unresolvable member must appear nowhere, not just be missing its own relation")
}

// slack_channel#workspace is written ONLY for public channels, so
// view = member + workspace->member makes a public channel's audience the
// whole workspace and a private channel's audience its members. That is the
// DM-and-private distinction, for free.
func TestSlackKind_WorkspaceEdgeOnlyForPublicChannels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		channelID := r.FormValue("channel")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": map[string]any{"id": channelID, "is_private": channelID == "CPRIV"},
		})
	})
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok": true, "user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "carol@example.com"}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	ctx := context.Background()
	pub, err := k.FetchScope(ctx, testCreds(), relsync.Scope{ID: "CPUB", ResourceType: slackChannelResourceType})
	require.NoError(t, err)
	priv, err := k.FetchScope(ctx, testCreds(), relsync.Scope{ID: "CPRIV", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	assert.Contains(t, pub.Tuples, spicedb.Tuple{
		ResourceType: slackChannelResourceType, ResourceID: "CPUB", Relation: "workspace",
		SubjectType: slackWorkspaceResourceType, SubjectID: "T1",
	}, "a public channel must assert its workspace link")

	for _, tup := range priv.Tuples {
		assert.Falsef(t, tup.ResourceType == slackChannelResourceType && tup.Relation == "workspace",
			"a private channel must never get a workspace edge, got %+v", tup)
	}

	// Workspace MEMBERSHIP is independent of channel privacy: both channels
	// contribute the same member to the same workspace.
	wantWorkspaceMember := spicedb.Tuple{
		ResourceType: slackWorkspaceResourceType, ResourceID: "T1", Relation: "member",
		SubjectType: slackUserResourceType, SubjectID: "U1", SubjectRelation: "user",
	}
	assert.Contains(t, pub.Tuples, wantWorkspaceMember)
	assert.Contains(t, priv.Tuples, wantWorkspaceMember)
}

// FetchScope is atomic: page 2 failing yields an error and no partial set.
func TestSlackKind_FetchScopeIsAtomicAcrossPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok": true, "channel": map[string]any{"id": "C1", "is_private": false},
	}))
	var membersCalls int32
	mux.HandleFunc("/conversations.members", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&membersCalls, 1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "members": []string{"U1"}, "response_metadata": map[string]any{"next_cursor": "PAGE2"},
			})
			return
		}
		// Page 2 fails outright (a transient upstream error, not
		// channel_not_found) after page 1 already succeeded and returned U1.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "internal_error"})
	})
	// Registered defensively so an implementation detail (call order)
	// cannot turn a real bug into a spurious 404 instead of the failure
	// this test is actually about; neither should be reached if
	// conversations.members atomicity holds, since page 2 fails before the
	// join step ever runs.
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok": true, "user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "dave@example.com"}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})

	require.Error(t, err, "a failed page must surface as an error, never be swallowed")
	assert.Empty(t, content.Tuples, "page 1's already-resolved member must never leak once page 2 fails")
	assert.Equal(t, 0, content.JoinMisses)
	assert.Equal(t, int32(2), atomic.LoadInt32(&membersCalls), "both pages must have actually been attempted")
}

// A truncated conversations.list reports Complete=false rather than looking
// like a whole enumeration.
func TestSlackKind_TruncatedListReportsIncomplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", jsonHandler(map[string]any{
		"ok":                true,
		"channels":          []map[string]any{{"id": "C1", "is_private": false}},
		"response_metadata": map[string]any{"next_cursor": "PAGE2"},
	}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	page, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.NoError(t, err)

	assert.False(t, page.Complete, "a page reporting more upstream must never claim Complete=true")
	require.Len(t, page.Scopes, 1, "the page must still be well-formed, not discarded for being partial")
	assert.Equal(t, relsync.ScopeID("C1"), page.Scopes[0].ID)
	assert.Equal(t, slackChannelResourceType, page.Scopes[0].ResourceType)
	assert.Equal(t, relsync.Cursor{Kind: KindName, Token: "PAGE2"}, page.Next)
}

// A whole conversations.list — no further cursor — reports Complete=true.
// Paired with the truncated case above so Complete is proven to track the
// upstream cursor in BOTH directions, not just default to one value.
func TestSlackKind_WholeListReportsComplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", jsonHandler(map[string]any{
		"ok":                true,
		"channels":          []map[string]any{{"id": "C1", "is_private": false}},
		"response_metadata": map[string]any{"next_cursor": ""},
	}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	page, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.NoError(t, err)

	assert.True(t, page.Complete)
	assert.Equal(t, relsync.Cursor{}, page.Next, "an exhausted listing must return the exact zero Cursor")
}

// slack_bot#agent is deliberately not one of DirectorySyncSource's claims,
// so this kind must never write a bot's membership. A bot member is
// excluded outright, not counted as a join miss (see SyncKind's own doc).
func TestSlackKind_BotMembersAreExcludedAndNotCountedAsAJoinMiss(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok": true, "channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"UBOT"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok": true, "user": map[string]any{"id": "UBOT", "is_bot": true, "profile": map[string]any{"email": ""}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	assert.Equal(t, 0, content.JoinMisses, "a bot exclusion is not an identity-join failure")
	assert.False(t, referencesSlackUser(content.Tuples, "UBOT"))
	// The channel is still public, so its own workspace link is the only
	// tuple this scope contributes.
	assert.Equal(t, []spicedb.Tuple{{
		ResourceType: slackChannelResourceType, ResourceID: "C1", Relation: "workspace",
		SubjectType: slackWorkspaceResourceType, SubjectID: "T1",
	}}, content.Tuples)
}

// ErrScopeGone is not the same as an empty result: a channel Slack reports
// gone must map to it explicitly, not to a merely-empty ScopeContent.
func TestSlackKind_ChannelGoneMapsToErrScopeGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{"ok": false, "error": "channel_not_found"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	content, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "CGONE", ResourceType: slackChannelResourceType})

	require.Error(t, err)
	assert.ErrorIs(t, err, relsync.ErrScopeGone)
	assert.Equal(t, relsync.ScopeContent{}, content)
}

// The kind must self-register under KindName, and its Source's Claims must
// cover every relation FetchScope/ListScopes above prove it writes — this
// is the executable half of the handoff list in
// .superpowers/sdd/2026-09-10-relationship-source-slack/task-7-brief.md.
func TestSlackKind_RegisteredWithClaimsCoveringEveryWrittenRelation(t *testing.T) {
	got, ok := relsync.Get(KindName)
	require.True(t, ok, "the slack kind must self-register via init()")
	_, isSyncKind := got.(*SyncKind)
	assert.True(t, isSyncKind)
	assert.Equal(t, KindName, got.Name())

	claims := got.Source().Claims
	for _, want := range []string{
		"slack_channel#member",
		"slack_channel#workspace",
		"slack_workspace#member",
		"slack_user#user",
	} {
		assert.Contains(t, claims, want, "this kind writes %s; it must be claimed", want)
	}
}

// A member present in many channels must be resolved via users.info once
// per pass, not once per channel: FetchScope is called once per channel
// scope, sharing the same credential across every call in one pass, and
// the whole point of resumability is surviving Slack's rate limits.
func TestSlackKind_UsersInfoIsMemoizedWithinAPass(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		channelID := r.FormValue("channel")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": map[string]any{"id": channelID, "is_private": false},
		})
	})
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	var usersInfoCalls int32
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&usersInfoCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "erin@example.com"}},
		})
	})
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{} // one Kind instance, standing in for the one Pass() shares across its scopes
	ctx := context.Background()
	creds := testCreds()

	wantCanonical, err := identity.FromExternal(identity.KindSlack, identity.TeamScope("T1"), identity.RawExternalID("U1"), identity.Email("erin@example.com")).Canonical()
	require.NoError(t, err)
	wantIdentityLink := spicedb.Tuple{
		ResourceType: slackUserResourceType, ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: wantCanonical.String(),
	}

	first, err := k.FetchScope(ctx, creds, relsync.Scope{ID: "CHAN1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)
	second, err := k.FetchScope(ctx, creds, relsync.Scope{ID: "CHAN2", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	assert.Equal(t, int32(1), atomic.LoadInt32(&usersInfoCalls), "the second channel's fetch must reuse the first's users.info result")
	// The memoization must not change WHICH tuples are produced: both
	// fetches resolve U1 to the exact same identity link, cache hit or not.
	assert.Contains(t, first.Tuples, wantIdentityLink)
	assert.Contains(t, second.Tuples, wantIdentityLink)
}

// The users.info cache is keyed by credential, not by Slack user id alone:
// two different workspaces (this package's one registered *SyncKind can in
// principle serve many RelationshipSource CRs, each with its own
// credential) must never see each other's cached profile, even for the
// same user id string.
func TestSlackKind_UsersInfoCacheDoesNotLeakAcrossCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok": true, "channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	var usersInfoCalls int32
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&usersInfoCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "frank@example.com"}},
		})
	})
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	ctx := context.Background()
	credsA := relsync.SourceParams{Token: sensitive.NewSensitiveValue([]byte("xoxb-workspace-a"))}
	credsB := relsync.SourceParams{Token: sensitive.NewSensitiveValue([]byte("xoxb-workspace-b"))}

	_, err := k.FetchScope(ctx, credsA, relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)
	_, err = k.FetchScope(ctx, credsB, relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	assert.Equal(t, int32(2), atomic.LoadInt32(&usersInfoCalls), "a different credential must never be served the other workspace's cached profile")
}

// MINOR 6 (whole-branch review): pkg/controllers/relationshipsource's
// RetryAfter interface (RetryAfter() time.Duration) is matched via
// errors.As — but slack-go's *slackapi.RateLimitedError carries its backoff
// as a FIELD, which cannot also be a method, so a raw Slack 429 never
// matched it. The only test of that path used a hand-built type production
// never produces. This drives a REAL Slack 429 (an httptest server, not
// fakeslack — a raw HTTP status code, no notion of "call N fails" that a
// per-method fake could encode) through the real ListScopes and asserts the
// resulting error satisfies the SAME method shape the reconciler checks —
// declared locally, structurally, so this test proves the real thing
// without importing the (kind-agnostic, and for good reason) controller
// package into this kind's own test.
func TestSlackKind_RateLimitOnListScopesCarriesRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	_, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.Error(t, err, "a 429 must surface as an error, never be swallowed")

	var ra interface{ RetryAfter() time.Duration }
	require.ErrorAsf(t, err, &ra,
		"a Slack 429 must surface a value implementing RetryAfter() time.Duration — the exact shape "+
			"pkg/controllers/relationshipsource's RetryAfter interface matches via errors.As — or the "+
			"reconciler's backoff honouring silently does nothing")
	assert.Equal(t, 37*time.Second, ra.RetryAfter())

	// withRetryAfter must not hide the original slack-go error: every other
	// call site in this file (isChannelNotFound, e.g.) matches on the raw
	// Slack error directly, and must keep doing so.
	var rl *slackapi.RateLimitedError
	assert.ErrorAsf(t, err, &rl, "the original *slackapi.RateLimitedError must still be reachable via errors.As through the wrapper")
}

// A 429 that names no backoff is the hole the header case left open, and it
// is the one a live run actually fell into: 999 rate-limit errors in five
// minutes.
//
// slack-go builds a *slackapi.RateLimitedError only when the 429 carries a
// Retry-After header (its misc.go guards on exactly that), so a bare 429
// arrives as a plain slackapi.StatusCodeError{Code: 429} and used to pass
// through withRetryAfter untouched. Nothing then satisfied the reconciler's
// RetryAfter interface, so retryAfterFrom reported NOTHING — and a pass that
// reported no throttling is exempt from the interval hold when it runs
// mid-cycle, so the next pass went straight back out to a Slack that had just
// said stop.
//
// The floor is what makes the signal expressible: a zero we invented would
// requeue immediately, which is the hammering this exists to stop.
func TestSlackKind_RateLimitWithoutRetryAfterHeaderStillCarriesBackoff(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, _ *http.Request) {
		// No Retry-After. This is the shape slack-go declines to recognize.
		w.WriteHeader(http.StatusTooManyRequests)
	})
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	_, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.Error(t, err, "a 429 must surface as an error, never be swallowed")

	var ra interface{ RetryAfter() time.Duration }
	require.ErrorAsf(t, err, &ra,
		"a 429 with no Retry-After must still satisfy RetryAfter() time.Duration — without it the "+
			"reconciler is told nothing was throttled, and a mid-cycle pass takes the interval exemption "+
			"straight back out to a Slack that just refused it")
	assert.GreaterOrEqual(t, ra.RetryAfter(), minRateLimitBackoff,
		"a backoff we invented must be at least the floor; zero would requeue immediately and re-hammer")

	// The original slack-go error stays reachable, exactly as it does on the
	// header path: the substring checks elsewhere in this file match on it.
	var sce slackapi.StatusCodeError
	assert.ErrorAsf(t, err, &sce, "the original slackapi.StatusCodeError must remain reachable through the wrapper")
	assert.Equal(t, http.StatusTooManyRequests, sce.Code)
}

// Slack also reports a rate limit as an ordinary 200 whose JSON body carries
// error:"ratelimited" — the shape users.info answers with, which FetchScope
// calls for every member of every scope. It reaches the reconciler through the
// same interface and had the same hole: no HTTP 429 to key on at all.
func TestSlackKind_RateLimitedJSONBodyStillCarriesBackoff(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", jsonHandler(map[string]any{"ok": false, "error": "ratelimited"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	_, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.Error(t, err)

	var ra interface{ RetryAfter() time.Duration }
	require.ErrorAsf(t, err, &ra,
		"Slack's ratelimited error code must arm the hold too; it is a rate limit that simply never "+
			"used an HTTP status to say so")
	assert.GreaterOrEqual(t, ra.RetryAfter(), minRateLimitBackoff)
}

// The non-rate-limited case: withRetryAfter must not manufacture a
// RetryAfter-satisfying value out of an ordinary error, or every scope
// error would look like a rate limit to the reconciler.
func TestSlackKind_OrdinaryErrorNeverClaimsRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", jsonHandler(map[string]any{"ok": false, "error": "internal_error"}))
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	_, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.Error(t, err)

	var ra interface{ RetryAfter() time.Duration }
	assert.False(t, errors.As(err, &ra), "an ordinary Slack error must never satisfy RetryAfter")
}
