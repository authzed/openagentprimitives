package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func ownerlessPolicy(permission string, fromOutput bool) *spiceboxv1alpha1.ChannelOwnerPolicy {
	return &spiceboxv1alpha1.ChannelOwnerPolicy{
		Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{
			Permission: permission, FromOutputChannel: fromOutput,
		},
	}
}

// The precedence is the whole design, and one rule in it is load-bearing for
// how broadly a session is owned: `starter` outranks BOTH ownerless sources.
//
// That is why channel ownership is narrower than it looks. On a kind that
// attributes messages to a user, the human who summoned the agent owns the
// session and fromOutputChannel never fires — it applies to kinds with no
// per-user attribution, which is the case it exists for.
func TestResolveOwnerSubject_precedence(t *testing.T) {
	cases := []struct {
		name         string
		identityMode string
		policy       *spiceboxv1alpha1.ChannelOwnerPolicy
		starter      string
		outputGroup  string
		wantSubject  string
		wantSource   spiceboxv1alpha1.OwnerSource
	}{
		{
			name:         "passthrough forces the starter, outranking an explicit override",
			identityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			policy:       &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:eng#member"},
			starter:      "user:alice",
			wantSubject:  "user:alice",
			wantSource:   spiceboxv1alpha1.OwnerSourcePassthrough,
		},
		{
			name:        "explicit override beats the starter",
			policy:      &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:eng#member"},
			starter:     "user:alice",
			wantSubject: "group:eng#member",
			wantSource:  spiceboxv1alpha1.OwnerSourceExplicit,
		},
		{
			// THE rule that keeps channel ownership narrow.
			name:        "a starter beats fromOutputChannel: an individual owns it",
			policy:      ownerlessPolicy("", true),
			starter:     "user:alice",
			outputGroup: "slack_channel:C1#member",
			wantSubject: "user:alice",
			wantSource:  spiceboxv1alpha1.OwnerSourceStarter,
		},
		{
			name:        "a starter beats an ownerless permission too",
			policy:      ownerlessPolicy("group:eng#member", false),
			starter:     "user:alice",
			wantSubject: "user:alice",
			wantSource:  spiceboxv1alpha1.OwnerSourceStarter,
		},
		{
			name:        "no starter: the ownerless permission owns it",
			policy:      ownerlessPolicy("group:eng#member", true),
			outputGroup: "slack_channel:C1#member",
			wantSubject: "group:eng#member",
			wantSource:  spiceboxv1alpha1.OwnerSourcePermission,
		},
		{
			name:        "no starter, no permission: the output channel owns it",
			policy:      ownerlessPolicy("", true),
			outputGroup: "slack_channel:C1#member",
			wantSubject: "slack_channel:C1#member",
			wantSource:  spiceboxv1alpha1.OwnerSourceOutputChannel,
		},
		{
			// The derivation. A Channel that declares no owner at all, on a kind
			// that names no starter, takes the output channel's membership —
			// the people the agent already posts to. Reaching this rule with an
			// undeclared policy is what stops a fresh webhook-driven install
			// from needing the field hand-patched before anything can run.
			name:        "nothing declared at all: the output channel owns it",
			policy:      nil,
			outputGroup: "slack_channel:C1#member",
			wantSubject: "slack_channel:C1#member",
			wantSource:  spiceboxv1alpha1.OwnerSourceOutputChannel,
		},
		{
			name:        "an ownerless block declaring nothing: the output channel owns it",
			policy:      ownerlessPolicy("", false),
			outputGroup: "slack_channel:C1#member",
			wantSubject: "slack_channel:C1#member",
			wantSource:  spiceboxv1alpha1.OwnerSourceOutputChannel,
		},
		{
			// The regression direction: a declared permission is an intent, and
			// the output channel never overrides it even though one is offered.
			name:        "a declared ownerless permission outranks the output channel",
			policy:      ownerlessPolicy("group:eng#member", false),
			outputGroup: "slack_channel:C1#member",
			wantSubject: "group:eng#member",
			wantSource:  spiceboxv1alpha1.OwnerSourcePermission,
		},
		{
			name:        "a declared explicit owner outranks the output channel",
			policy:      &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "user:carol"},
			outputGroup: "slack_channel:C1#member",
			wantSubject: "user:carol",
			wantSource:  spiceboxv1alpha1.OwnerSourceExplicit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(
				tc.identityMode, tc.policy, tc.starter, tc.outputGroup, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSubject, subject)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

// Fail-closed: an unresolvable owner is an error, never a silent empty subject.
// A session with no owner would have nobody able to approve for it, and writing
// an empty tuple would be far worse than refusing.
func TestResolveOwnerSubject_failsClosed(t *testing.T) {
	cases := []struct {
		name         string
		identityMode string
		policy       *spiceboxv1alpha1.ChannelOwnerPolicy
		starter      string
		outputGroup  string
	}{
		{name: "nothing resolvable at all"},
		{name: "passthrough with no starter", identityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
		{
			name:   "fromOutputChannel requested but the kind supplied no group",
			policy: ownerlessPolicy("", true),
		},
		{
			// The other half of the derivation: nothing to derive FROM is still
			// nothing. An output channel with no configured destination yields
			// no membership, and the answer must stay a refusal rather than
			// fall through to anything broader.
			name:         "passthrough with no starter, even when a group is offered",
			identityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			outputGroup:  "slack_channel:C1#member",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+": errors", func(t *testing.T) {
			_, _, err := spiceboxv1alpha1.ResolveOwnerSubject(
				tc.identityMode, tc.policy, tc.starter, tc.outputGroup, nil)
			require.Error(t, err)
		})
	}
}

// IsCollectiveOwnership is what a channel keys its disclosure on. It must be
// true for exactly the sources that mean "a population owns this" — the cases
// where the people affected never individually agreed and cannot see the tuple.
func TestIsCollectiveOwnership(t *testing.T) {
	collective := []spiceboxv1alpha1.OwnerSource{
		spiceboxv1alpha1.OwnerSourcePermission,
		spiceboxv1alpha1.OwnerSourceOutputChannel,
	}
	individual := []spiceboxv1alpha1.OwnerSource{
		spiceboxv1alpha1.OwnerSourcePassthrough,
		spiceboxv1alpha1.OwnerSourceStarter,
	}

	for _, s := range collective {
		assert.Truef(t, spiceboxv1alpha1.IsCollectiveOwnership(s),
			"%s means a population owns the session and must be disclosed", s)
	}
	for _, s := range individual {
		assert.Falsef(t, spiceboxv1alpha1.IsCollectiveOwnership(s),
			"%s is a single identified person", s)
	}

	// explicit is deliberately NOT collective: it may name a group, but an
	// operator wrote that subject by hand for this channel, so the string
	// itself is the disclosure. Keying on the source would mislabel an
	// explicit single-user override as a population.
	assert.False(t, spiceboxv1alpha1.IsCollectiveOwnership(spiceboxv1alpha1.OwnerSourceExplicit))
}

// OwnerMayComeFromOutputChannel is the admin veto the derivation must respect.
// ResolveOwnerSubject never sees the ceiling — the ceiling is enforced by
// refusing the Channel CONFIG — so a derived owner, which is never written into
// a Channel's spec, would sail straight past it without this gate.
func TestOwnerMayComeFromOutputChannel(t *testing.T) {
	cases := []struct {
		name    string
		ceiling *spiceboxv1alpha1.OwnerCeiling
		want    bool
	}{
		{name: "no ceiling: the output channel may own it", ceiling: nil, want: true},
		{name: "an empty ceiling declares no veto: permitted", ceiling: &spiceboxv1alpha1.OwnerCeiling{}, want: true},
		{
			name:    "starterOnly: refused, or the ceiling would acquire a whole-channel owner",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
			want:    false,
		},
		{
			name:    "fixed: refused, the admin already named the owner",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:sec#member"},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, spiceboxv1alpha1.OwnerMayComeFromOutputChannel(tc.ceiling))
		})
	}
}

// EffectiveSessionInteractPermission is the one place the spec-beats-derived
// precedence lives. A consumer reading only one half would disagree with the
// controller about who may interact with a session.
func TestEffectiveSessionInteractPermission(t *testing.T) {
	withDeclared := func(declared, derived string) *spiceboxv1alpha1.AgentClass {
		ac := &spiceboxv1alpha1.AgentClass{}
		if declared != "" {
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
				Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: declared},
			}
		}
		ac.Status.DerivedSessionInteractPermission = derived
		return ac
	}

	cases := []struct {
		name     string
		declared string
		derived  string
		want     string
	}{
		{name: "neither: no policy at all", want: ""},
		{name: "declared only: the authored value", declared: "group:eng#member", want: "group:eng#member"},
		{name: "derived only: the derived value", derived: "slack_channel:C0DEMO123#member", want: "slack_channel:C0DEMO123#member"},
		{
			name:     "both: the authored value wins, derivation never overrides an intent",
			declared: "group:eng#member",
			derived:  "slack_channel:C0DEMO123#member",
			want:     "group:eng#member",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, withDeclared(tc.declared, tc.derived).EffectiveSessionInteractPermission())
		})
	}

	assert.Empty(t, (*spiceboxv1alpha1.AgentClass)(nil).EffectiveSessionInteractPermission(),
		"nil-safe: a caller holding no class reads no policy, it does not panic")
}
