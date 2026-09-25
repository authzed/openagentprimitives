package slack

import (
	"encoding/base64"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalIDFromEmail(t *testing.T) {
	got := CanonicalID("Alice@Example.COM", "T01", "U001")
	want := base64.RawURLEncoding.EncodeToString([]byte("alice@example.com"))
	assert.Equal(t, want, got, "canonical ID for normalized email")

	// Round-trip: decode and verify lowercased email.
	dec, err := base64.RawURLEncoding.DecodeString(got)
	require.NoError(t, err, "base64 decode")
	assert.Equal(t, "alice@example.com", string(dec), "decoded value")

	// No padding character in URL-safe base64.
	assert.False(t, strings.ContainsAny(got, "=+/"),
		"base64url should not contain =, +, or /: %q", got)
}

func TestCanonicalIDFallback(t *testing.T) {
	got := CanonicalID("", "T01", "U001")
	want := base64.RawURLEncoding.EncodeToString([]byte("slack:T01:U001"))
	assert.Equal(t, want, got, "canonical ID fallback")
}

func TestIdentityCacheBasicLRU(t *testing.T) {
	c := NewIdentityCache(2)
	c.Put(userInfo{UserID: "U1", Email: "a@x", TeamID: "T1"})
	c.Put(userInfo{UserID: "U2", Email: "b@x", TeamID: "T1"})

	got, ok := c.Get("U1")
	require.True(t, ok, "U1 should be cached")
	assert.Equal(t, "a@x", got.Email, "U1 email")

	// Put U3 — should evict U2 (least recently used; U1 was just touched).
	c.Put(userInfo{UserID: "U3", Email: "c@x", TeamID: "T1"})
	_, ok = c.Get("U2")
	assert.False(t, ok, "U2 should have been evicted")
	_, ok = c.Get("U1")
	assert.True(t, ok, "U1 should still be cached")
	_, ok = c.Get("U3")
	assert.True(t, ok, "U3 should be cached")
}

func TestIdentityCacheZeroDisables(t *testing.T) {
	c := NewIdentityCache(0)
	c.Put(userInfo{UserID: "U1", Email: "a@x"})
	_, ok := c.Get("U1")
	assert.False(t, ok, "Get should miss with max=0")
}

func TestIdentityCacheUpdateExisting(t *testing.T) {
	c := NewIdentityCache(2)
	c.Put(userInfo{UserID: "U1", Email: "old"})
	c.Put(userInfo{UserID: "U1", Email: "new"})
	got, _ := c.Get("U1")
	assert.Equal(t, "new", got.Email, "expected updated email")
}

func TestIdentityCacheGetByEmail(t *testing.T) {
	c := NewIdentityCache(4)
	c.Put(userInfo{UserID: "U1", Email: "Joe@Example.com", TeamID: "T1"})

	// case-insensitive
	got, ok := c.GetByEmail("joe@example.com")
	require.True(t, ok, "GetByEmail: not found")
	assert.Equal(t, "U1", got.UserID, "UserID")
}

func TestIdentityCacheEvictionRemovesBothIndexes(t *testing.T) {
	c := NewIdentityCache(2) // capacity 2
	c.Put(userInfo{UserID: "U1", Email: "a@x", TeamID: "T1"})
	c.Put(userInfo{UserID: "U2", Email: "b@x", TeamID: "T1"})
	c.Put(userInfo{UserID: "U3", Email: "c@x", TeamID: "T1"}) // evicts U1

	_, ok := c.Get("U1")
	assert.False(t, ok, "U1 still in byID after eviction")
	_, ok = c.GetByEmail("a@x")
	assert.False(t, ok, "a@x still in byEmail after eviction")
}

func TestIdentityCacheReplacementUpdatesBothIndexes(t *testing.T) {
	c := NewIdentityCache(4)
	c.Put(userInfo{UserID: "U1", Email: "old@x", TeamID: "T1"})
	c.Put(userInfo{UserID: "U1", Email: "new@x", TeamID: "T1"})

	_, ok := c.GetByEmail("old@x")
	assert.False(t, ok, "old@x still in byEmail after replacement")
	got, ok := c.GetByEmail("new@x")
	require.True(t, ok, "new@x lookup failed")
	assert.Equal(t, "U1", got.UserID, "new@x lookup UserID")
}

// TestEmailTrusted verifies the gate that controls whether a Slack user's
// profile email is trusted as a SpiceDB authorization subject. Only a full
// member of the bot's own installed workspace qualifies; all other cases must
// return false so callers fall back to the unforgeable slack:<team>:<user>
// synthetic identity.
func TestEmailTrusted(t *testing.T) {
	const installedTeam = "T_HOME"

	baseUser := func() *slackapi.User {
		return &slackapi.User{
			TeamID:            installedTeam,
			IsRestricted:      false,
			IsUltraRestricted: false,
			IsStranger:        false,
			IsBot:             false,
			Deleted:           false,
		}
	}

	cases := []struct {
		name            string
		user            *slackapi.User
		installedTeamID string
		want            bool
	}{
		{
			name:            "same-workspace full member: trusted",
			user:            baseUser(),
			installedTeamID: installedTeam,
			want:            true,
		},
		{
			name: "foreign workspace (TeamID != installedTeamID): not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.TeamID = "T_FOREIGN"
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name: "IsRestricted (guest): not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsRestricted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name: "IsUltraRestricted (single-channel guest): not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsUltraRestricted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name: "IsStranger (shared-channel foreign user): not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsStranger = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name: "IsBot: not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.IsBot = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name: "Deleted account: not trusted",
			user: func() *slackapi.User {
				u := baseUser()
				u.Deleted = true
				return u
			}(),
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name:            "nil user: not trusted",
			user:            nil,
			installedTeamID: installedTeam,
			want:            false,
		},
		{
			name:            "empty installedTeamID: not trusted",
			user:            baseUser(),
			installedTeamID: "",
			want:            false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emailTrusted(tc.user, tc.installedTeamID)
			assert.Equal(t, tc.want, got, "emailTrusted result")
		})
	}
}
