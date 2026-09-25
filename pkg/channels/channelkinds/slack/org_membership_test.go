package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestOrgMembershipOf verifies the authorization-grade membership predicate:
// only a full, non-deleted, human member of the bot's own installed workspace
// is OrgMembershipMember. Every other shape — guests, strangers, foreign
// workspaces, bots, deleted accounts, a nil user, an unknown installed team —
// is OrgMembershipGuest, never empty, so the session-start gate fails closed.
//
// This is deliberately a separate predicate from accountTypeOf (descriptive
// only, never for authorization) and from emailTrusted (email-as-identity
// trust): this one exists to be branched on by the start gate.
func TestOrgMembershipOf(t *testing.T) {
	const installedTeam = "T_HOME"

	baseUser := func() *slackapi.User {
		return &slackapi.User{TeamID: installedTeam}
	}

	cases := []struct {
		name            string
		user            *slackapi.User
		installedTeamID string
		want            channelkinds.OrgMembership
	}{
		{
			name:            "same-workspace full member: member",
			user:            baseUser(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipMember,
		},
		{
			name: "foreign workspace (TeamID != installedTeamID): guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.TeamID = "T_FOREIGN"
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name: "IsRestricted (multi-channel guest): guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsRestricted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name: "IsUltraRestricted (single-channel guest): guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsUltraRestricted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name: "IsStranger (shared-channel foreign user): guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsStranger = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name: "IsBot: guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsBot = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name: "Deleted account: guest",
			user: func() *slackapi.User {
				u := baseUser()
				u.Deleted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name:            "nil user: guest",
			user:            nil,
			installedTeamID: installedTeam,
			want:            channelkinds.OrgMembershipGuest,
		},
		{
			name:            "empty installedTeamID: guest",
			user:            baseUser(),
			installedTeamID: "",
			want:            channelkinds.OrgMembershipGuest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orgMembershipOf(tc.user, tc.installedTeamID)
			assert.Equal(t, tc.want, got, "orgMembershipOf result")
			assert.NotEmpty(t, got, "orgMembershipOf must never return the empty (undeclared) value")
		})
	}
}

// TestKind_AttributesOrgMembership pins the capability declaration: the slack
// kind classifies every inbound user's org standing, so the pipeline's
// session-start gate applies to slack channels unconditionally — no
// per-channel opt-out, and no dependency on channel config being present.
// The assignment is the assertion that Kind implements the optional
// capability interface.
func TestKind_AttributesOrgMembership(t *testing.T) {
	var k channelkinds.OrgMembershipAttributor = &Kind{}
	assert.True(t, k.AttributesOrgMembership(nil),
		"slack must declare org-membership attribution or the start gate silently never applies")
	assert.True(t, k.AttributesOrgMembership(&spiceboxv1alpha1.Channel{}),
		"attribution must not depend on channel config")
}

// usersInfoServer serves a canned users.info response so resolveIdentity can
// be exercised through a real slack-go client.
func usersInfoServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveIdentity_OrgMembership verifies that every path out of
// resolveIdentity stamps a non-empty OrgMembership on the identity, and that
// only a successful users.info showing a full installed-workspace member
// yields OrgMembershipMember. All failure shapes yield OrgMembershipGuest —
// the start gate treats anything but member as gated, so an API blip must
// never widen access.
func TestResolveIdentity_OrgMembership(t *testing.T) {
	const installedTeam = "T0COMPANY"

	t.Run("full member from users.info: member, and membership is cached", func(t *testing.T) {
		srv := usersInfoServer(t, `{"ok":true,"user":{"id":"U0ALICE","team_id":"T0COMPANY","profile":{"email":"alice@example.com","display_name":"Alice"}}}`)
		l := &slackListener{
			api:             slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")),
			idents:          NewIdentityCache(4),
			installedTeamID: installedTeam,
		}

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, channelkinds.OrgMembershipMember, id.OrgMembership)

		cached, ok := l.idents.Get("U0ALICE")
		assert.True(t, ok, "successful lookup must be cached")
		assert.Equal(t, channelkinds.OrgMembershipMember, cached.Membership,
			"membership must ride the identity cache so a returning member is not re-gated by a cache hit")
	})

	t.Run("restricted guest from users.info: guest", func(t *testing.T) {
		srv := usersInfoServer(t, `{"ok":true,"user":{"id":"U0GUEST","team_id":"T0COMPANY","is_restricted":true,"profile":{"email":"guest@example.com","display_name":"Guest"}}}`)
		l := &slackListener{
			api:             slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")),
			idents:          NewIdentityCache(4),
			installedTeamID: installedTeam,
		}

		id := l.resolveIdentity(context.Background(), "U0GUEST")
		assert.Equal(t, channelkinds.OrgMembershipGuest, id.OrgMembership)
	})

	t.Run("users.info failure: guest, never member (fail-closed)", func(t *testing.T) {
		srv := usersInfoServer(t, `{"ok":false,"error":"ratelimited"}`)
		l := &slackListener{
			api:             slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")),
			idents:          NewIdentityCache(4),
			installedTeamID: installedTeam,
		}

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, channelkinds.OrgMembershipGuest, id.OrgMembership,
			"an API blip must gate the start, not wave it through")
	})

	t.Run("nil api (no client wired): guest", func(t *testing.T) {
		l := &slackListener{installedTeamID: installedTeam}

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, channelkinds.OrgMembershipGuest, id.OrgMembership)
	})

	t.Run("cache hit returns the cached membership without an API call", func(t *testing.T) {
		l := &slackListener{
			// api deliberately nil — a users.info call would panic the test's
			// intent: the hit must come from the cache alone.
			idents:          NewIdentityCache(4),
			installedTeamID: installedTeam,
		}
		l.idents.Put(userInfo{UserID: "U0ALICE", Email: "alice@example.com", TeamID: installedTeam, DisplayName: "Alice", Membership: channelkinds.OrgMembershipMember})

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, channelkinds.OrgMembershipMember, id.OrgMembership)
	})

	t.Run("cache hit with empty membership normalizes to guest (fail-closed)", func(t *testing.T) {
		l := &slackListener{
			idents:          NewIdentityCache(4),
			installedTeamID: installedTeam,
		}
		l.idents.Put(userInfo{UserID: "U0OLD", TeamID: installedTeam, DisplayName: "Old"})

		id := l.resolveIdentity(context.Background(), "U0OLD")
		assert.Equal(t, channelkinds.OrgMembershipGuest, id.OrgMembership,
			"an entry that never had membership stamped must gate, not pass")
	})
}
