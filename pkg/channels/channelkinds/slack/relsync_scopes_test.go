package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// relsyncMethodBotScopes is history_scopes_test.go's slackMethodBotScopes,
// extended to every Slack Web API method relsync_kind.go's SyncKind calls:
// conversations.list, conversations.info, conversations.members, users.info
// and auth.test. Each is held to the SAME "must be a known method" gate
// slackMethodBotScopes' own test uses, so an unrecognized new method fails
// this test whether or not anyone thought to declare a scope for it.
//
// EVERY ENTRY IS CHECKED AGAINST THE DIRECTORY-SYNC DECLARATION, with no
// method carved out. There used to be one: users.info and auth.test were
// skipped on the grounds that users:read "arrives via" mention_lookup /
// thread_history / channel_history, all default-on. That was true and it was
// the coupling itself — an AgentClass that turns those three off and turns
// directory_sync on gets a bot token, and now a GENERATED APP MANIFEST
// (manifest.go's BotTokenAppManifestFor), carrying only what this feature
// declares. The carve-out is what let the declaration be short by the two
// scopes the member lookup cannot work without, while every scopes-valid
// check still reported healthy.
//
// conversations.list requests BOTH public_channel and private_channel types
// in the ONE call ListScopes makes (relsync_kind.go's
// GetConversationsParameters.Types), so both scopes are required TOGETHER,
// not as alternatives the way slackMethodBotScopes' conversations.replies/
// .history entries are (a single call there only ever touches one
// conversation type). conversations.info/.members are exercised below
// against one channel of each type, so the union across both calls is the
// same pair.
//
// users.info needs users:read to answer at all and users:read.email for the
// answer to carry profile.email. The second half is the load-bearing one:
// Slack omits the field rather than refusing the call, so a token without it
// returns a member whose email is empty, FetchScope drops that member as a
// join miss, and a sync that wrote NOTHING reports success. auth.test needs
// no scope.
var relsyncMethodBotScopes = map[string][]string{
	"conversations.list":    {"channels:read", "groups:read"},
	"conversations.info":    {"channels:read", "groups:read"},
	"conversations.members": {"channels:read", "groups:read"},
	"users.info":            {"users:read", "users:read.email"},
	"auth.test":             {},
}

// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed is
// history_scopes_test.go's guard extended to relsync_kind.go, per Critical 1
// of the whole-branch fix: the manifest requested NEITHER channels:read nor
// groups:read, so conversations.list/.info/.members all failed with
// missing_scope and no tuple ever reached SpiceDB. Driving the real
// ListScopes/FetchScope is what makes this a check on the code, not another
// copy of the scope list — add a new relsync Slack call and the recorder
// sees it whether or not features.go was updated to match.
//
// The drive walks a workspace with a MEMBER in it, so the per-member lookup
// runs and its scopes are checked like every other method's; see
// relsyncMethodBotScopes on why nothing is exempt any more.
func TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, r *http.Request) {
		record("conversations.list")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"channels": []map[string]any{
				{"id": "CPUB", "is_private": false},
				{"id": "CPRIV", "is_private": true},
			},
			"response_metadata": map[string]any{"next_cursor": ""},
		})
	})
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		record("conversations.info")
		_ = r.ParseForm()
		ch := r.FormValue("channel")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": map[string]any{"id": ch, "is_private": ch == "CPRIV"},
		})
	})
	// A NON-EMPTY membership, because an empty one never reaches users.info
	// and the recorder can only check the calls it sees. The carve-out this
	// test used to carry hid that: with no member to look up, the skipped
	// method was not merely exempt from the scope check, it was never called,
	// so removing the carve-out alone would have changed nothing.
	mux.HandleFunc("/conversations.members", func(w http.ResponseWriter, r *http.Request) {
		record("conversations.members")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "members": []string{"UMEMBER"}, "response_metadata": map[string]any{"next_cursor": ""},
		})
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		record("users.info")
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"id":      r.FormValue("user"),
				"profile": map[string]any{"email": "member@demo.test"},
			},
		})
	})
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		record("auth.test")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T1"})
	})
	startDirectoryTestServer(t, mux)

	k := &SyncKind{}
	ctx := context.Background()
	creds := testCreds()

	page, err := k.ListScopes(ctx, creds, relsync.Cursor{})
	require.NoError(t, err)
	require.Len(t, page.Scopes, 2, "fixture must offer one public and one private channel")

	for _, sc := range page.Scopes {
		_, err := k.FetchScope(ctx, creds, sc)
		require.NoError(t, err)
	}

	called := slices.Compact(slices.Sorted(slices.Values(calls)))
	require.NotEmpty(t, called, "the drive made no Slack call; the fixture is not exercising the relsync code path")
	require.Contains(t, called, "users.info",
		"the member lookup was never reached, so its scopes go unchecked below — give the "+
			"conversations.members fixture a member again rather than letting this test pass on a "+
			"workspace where nobody is in any channel")

	req, ok := (&Kind{}).FeatureSupport()[channelfeatures.DirectorySync]
	require.True(t, ok, "the Slack kind must declare a requirement for channelfeatures.DirectorySync")
	declared := req.Scopes

	for _, method := range called {
		accepted, known := relsyncMethodBotScopes[method]
		require.Truef(t, known,
			"the relsync sync path calls %s, which relsyncMethodBotScopes does not cover — look "+
				"the method up in Slack's reference and add the scopes it lists", method)
		for _, s := range accepted {
			assert.Containsf(t, declared, s,
				"%s needs %s, which channelfeatures.DirectorySync does not declare", method, s)
		}
	}
}
