// pkg/channels/channelkinds/slack/app_home_pref_interaction_test.go
//
// Tests for handleHomePrefClick, the App Home preference-EDIT interaction
// handler. The crux is TestHandleHomePrefClick_SelfOnly_SubjectIsAlwaysClicker
// below: it proves the commit's Subject can only ever be the resolver's
// output for the clicker who fired the event, never anything the click's own
// payload carries.
package slack

import (
	"context"
	"fmt"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// recordedCommit captures one CommitPreferenceFirstParty call in full,
// including the (ns, className) the interface method takes as separate
// positional args, so a test can assert on every dimension of the write.
type recordedCommit struct {
	ns, className string
	req           preferences.CommitRequest
}

// recordingPreferencesClient is a stub channelkinds.PreferencesClient for
// these tests: CommitPreferenceFirstParty RECORDS every call (a test asserts
// exactly what got committed and to whom) and can be configured to fail
// (simulating a 409 lock or any other commit error); GetPreferencesFirstParty
// answers from a canned per-class snapshot, used only by the Save-button path
// to learn each key's declared type.
type recordingPreferencesClient struct {
	snapshots map[string]preferences.SnapshotResponse
	commitErr error

	commits []recordedCommit
}

func (f *recordingPreferencesClient) GetPreferencesFirstParty(_ context.Context, ns, className, _ string) (preferences.SnapshotResponse, error) {
	return f.snapshots[ns+"/"+className], nil
}

func (f *recordingPreferencesClient) CommitPreferenceFirstParty(_ context.Context, ns, className string, req preferences.CommitRequest) error {
	f.commits = append(f.commits, recordedCommit{ns: ns, className: className, req: req})
	return f.commitErr
}

// newPrefClickListener wires a listener with a populated identity cache (so
// resolveCanonicalForSlackUser hits the cache path, per its own doc comment:
// "l.idents only ever stores a trusted (or empty) email") plus a recording
// preferences client and a recording Home-tab publisher. slackUserID/email
// become the clicker's identity; expectedBareCanonical is computed the SAME
// way the production code computes it, by calling the resolver directly, so
// no test hand-derives a base64 encoding.
func newPrefClickListener(t *testing.T, slackUserID, email string) (l *slackListener, api *recordingHomeClient, prefs *recordingPreferencesClient, bareCanonical string) {
	t.Helper()
	api = &recordingHomeClient{Client: fakeslack.New()}
	prefs = &recordingPreferencesClient{snapshots: map[string]preferences.SnapshotResponse{}}
	l = &slackListener{
		api:             api,
		idents:          NewIdentityCache(8),
		installedTeamID: "T_HOME",
		preferences:     prefs,
	}
	l.idents.Put(userInfo{UserID: slackUserID, Email: email, TeamID: "T_HOME"})

	canonical, err := l.resolveCanonicalForSlackUser(context.Background(), slackUserID)
	require.NoError(t, err, "fixture setup: resolveCanonicalForSlackUser")
	bare, err := identity.Subject(canonical).CanonicalUserID()
	require.NoError(t, err, "fixture setup: CanonicalUserID")
	return l, api, prefs, bare.String()
}

// TestHandleHomePrefClick_SelfOnly_SubjectIsAlwaysClicker is THE security
// test: a home:pref: checkbox click from Slack user U (canonical C) must
// commit with Subject == C — the CLICKER's own canonical, computed by the
// resolver from cb.User.ID — and nothing else. The click's own BlockAction
// carries a forged "value" field shaped like it might name a different
// subject; the handler must never look at it, because there is no subject
// field anywhere in the decode path (action_id carries only classRef+key,
// per the DECODE CONTRACT; the value carries only the new setting). A
// crafted payload therefore has no field through which it could redirect the
// write to any other identity.
func TestHandleHomePrefClick_SelfOnly_SubjectIsAlwaysClicker(t *testing.T) {
	l, api, prefs, wantSubject := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID: prefActionID("ns/reviewbot", "notifications"),
				Type:     slackapi.ActionType(slackapi.METCheckboxGroups),
				// Forged: shaped like an attempt to redirect the write to a
				// different subject. There is no field named "subject" (or
				// anything else) that decodeControlValue or
				// handleHomePrefControlChange reads for identity purposes —
				// this MUST be completely ignored.
				Value:           `{"subject":"user:attacker-controlled","v":"interaction"}`,
				SelectedOptions: nil, // unchecked: notifications=false
			}},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.NoError(t, err, "handleHomePrefClick")
	assert.True(t, handled)

	require.Len(t, prefs.commits, 1, "exactly one commit")
	got := prefs.commits[0]
	assert.Equal(t, "ns", got.ns)
	assert.Equal(t, "reviewbot", got.className)
	assert.Equal(t, "notifications", got.req.Key)
	require.NotNil(t, got.req.Value)
	assert.JSONEq(t, `false`, string(got.req.Value.Raw))

	// THE assertion: Subject is the resolver's own output for the clicker,
	// not anything from the payload — and specifically NOT the forged value
	// above, which would fail this exact comparison if it had leaked through.
	assert.Equal(t, wantSubject, got.req.Subject, "commit Subject must be the clicker's own canonical, from the resolver")
	assert.NotContains(t, got.req.Subject, "attacker", "a crafted payload field must never reach the commit Subject")

	// Republished after a successful commit.
	assert.Len(t, api.published, 1, "home tab republished after commit")
}

// TestDecodePrefActionID_KeyContainingColon pins the DECODE CONTRACT
// documented on prefActionID (app_home_prefs.go): split on the FIRST colon
// only, so a key that itself contains ':' round-trips intact.
func TestDecodePrefActionID_KeyContainingColon(t *testing.T) {
	classRef, key, ok := decodePrefActionID("home:pref:ns/reviewbot:foo:bar")
	require.True(t, ok)
	assert.Equal(t, "ns/reviewbot", classRef)
	assert.Equal(t, "foo:bar", key, "splitting on the FIRST colon must leave the rest of the key, including its own colon, intact")
}

// TestHandleHomePrefClick_ColonInKey_CommitsFullKey exercises the same
// decode contract end to end: the commit must receive the key with its
// embedded colon, not a truncated prefix or suffix of it.
func TestHandleHomePrefClick_ColonInKey_CommitsFullKey(t *testing.T) {
	l, _, prefs, _ := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID:       "home:pref:ns/reviewbot:foo:bar",
				Type:           slackapi.ActionType(slackapi.OptTypeStatic),
				SelectedOption: *slackapi.NewOptionBlockObject("high", nil, nil),
			}},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.NoError(t, err)
	assert.True(t, handled)

	require.Len(t, prefs.commits, 1)
	assert.Equal(t, "foo:bar", prefs.commits[0].req.Key)
}

// TestHandleHomePrefClick_EnumChange_CommitsSelectedOptionValue verifies a
// static_select change commits the selected option's Value, JSON-encoded as
// a string.
func TestHandleHomePrefClick_EnumChange_CommitsSelectedOptionValue(t *testing.T) {
	l, _, prefs, wantSubject := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID:       prefActionID("ns/reviewbot", "priority"),
				Type:           slackapi.ActionType(slackapi.OptTypeStatic),
				SelectedOption: *slackapi.NewOptionBlockObject("high", nil, nil),
			}},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.NoError(t, err)
	assert.True(t, handled)

	require.Len(t, prefs.commits, 1)
	got := prefs.commits[0]
	assert.Equal(t, "priority", got.req.Key)
	require.NotNil(t, got.req.Value)
	assert.JSONEq(t, `"high"`, string(got.req.Value.Raw))
	assert.Equal(t, wantSubject, got.req.Subject)
}

// TestHandleHomePrefClick_SaveButton_CommitsFreeTextInput verifies a
// home:pref_save: click reads the free-text value out of the view's own
// state (cb.View.State.Values), not any button-value payload, and commits it
// using the type recorded in the resolved snapshot (here: "string").
func TestHandleHomePrefClick_SaveButton_CommitsFreeTextInput(t *testing.T) {
	l, _, prefs, wantSubject := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")
	prefs.snapshots["ns/reviewbot"] = preferences.SnapshotResponse{
		Snapshot: preferences.Snapshot{Keys: []preferences.Resolved{
			{Name: "notes", Type: "string"},
		}},
	}

	blockID := prefActionID("ns/reviewbot", "notes")
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID: "home:pref_save:ns/reviewbot",
				Type:     slackapi.ActionType(slackapi.METButton),
				Value:    "ns/reviewbot",
			}},
		},
		View: slackapi.View{
			State: &slackapi.ViewState{
				Values: map[string]map[string]slackapi.BlockAction{
					blockID: {blockID: {Value: "hello world"}},
				},
			},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.NoError(t, err)
	assert.True(t, handled)

	require.Len(t, prefs.commits, 1)
	got := prefs.commits[0]
	assert.Equal(t, "ns", got.ns)
	assert.Equal(t, "reviewbot", got.className)
	assert.Equal(t, "notes", got.req.Key)
	require.NotNil(t, got.req.Value)
	assert.JSONEq(t, `"hello world"`, string(got.req.Value.Raw))
	assert.Equal(t, wantSubject, got.req.Subject)
}

// TestHandleHomePrefClick_CommitError_SurfacesAndRepublishes_NoSuccessClaim
// pins the failure path (e.g. a 409 admin-lock conflict from the operator):
// the handler must return an error (never silently swallow it, and never
// return (true, nil) as though the write succeeded) and must still
// republish the Home tab so the user sees the ACTUAL stored state rather
// than their optimistic click.
func TestHandleHomePrefClick_CommitError_SurfacesAndRepublishes_NoSuccessClaim(t *testing.T) {
	l, api, prefs, _ := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")
	prefs.commitErr = fmt.Errorf("httpclient: status=409 body=key \"notifications\" is locked by admin policy")

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID:        prefActionID("ns/reviewbot", "notifications"),
				Type:            slackapi.ActionType(slackapi.METCheckboxGroups),
				SelectedOptions: []slackapi.OptionBlockObject{*slackapi.NewOptionBlockObject("true", nil, nil)},
			}},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.Error(t, err, "a commit failure must be reported, never claimed as success")
	assert.True(t, handled, "a recognized pref click must not fall through even when the commit fails")
	assert.Contains(t, err.Error(), "409")

	require.Len(t, prefs.commits, 1, "the commit was attempted")
	assert.Len(t, api.published, 1, "the Home tab is republished even after a failed commit, so the tab reflects the real (unlocked) state")
}

// TestHandleHomePrefClick_NonPrefAction_FallsThrough verifies a block_action
// unrelated to preferences returns (false, nil) so onInteraction's dispatch
// chain tries the next handler instead of erroring or silently swallowing
// the click.
func TestHandleHomePrefClick_NonPrefAction_FallsThrough(t *testing.T) {
	l, api, prefs, _ := newPrefClickListener(t, "U_CLICKER", "clicker@example.com")

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{
				ActionID: "home:manage_connections:demo-agent",
				Type:     slackapi.ActionType(slackapi.METButton),
			}},
		},
	}

	handled, err := l.handleHomePrefClick(context.Background(), cb)
	require.NoError(t, err)
	assert.False(t, handled, "a non-preference action_id must fall through to the next dispatch leg")
	assert.Empty(t, prefs.commits)
	assert.Empty(t, api.published)
}
