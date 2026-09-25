package fake

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestSessionOwner closes a gap between what this kind DOES and what it
// DECLARES.
//
// fake is the stand-in for a user-attributed transport: every inbound it
// injects carries an ExternalIdentity, the pipeline canonicalizes it, and
// sessions get a real started_by — which is what the owner-approval tests
// exercise. But it did not implement SessionOwnerProvider, so the channel
// controller judged every fake Channel as "ownerless input" and marked it
// Valid=False/SpecInvalid.
//
// Nothing noticed, because the e2e harness starts listeners without consulting
// Channel validity. The result was a suite whose fixtures were all in a state
// production refuses to serve — so a gate that reads Channel validity passes in
// e2e and blocks in production, or vice versa, which is the one thing an
// end-to-end suite must not allow.
func TestSessionOwner(t *testing.T) {
	cases := []struct {
		name     string
		ids      channelkinds.ExternalIdentity
		provides bool
	}{
		{
			name:     "user with a verified email: owns the session",
			ids:      channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "u1@example.com"},
			provides: true,
		},
		{
			name:     "user with no email: still owns it, via the synthetic subject",
			ids:      channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", TeamScope: "T1"},
			provides: true,
		},
		{
			name:     "no external identity at all: no owner to derive",
			ids:      channelkinds.ExternalIdentity{Kind: "fake"},
			provides: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subj, ok := Kind{}.SessionOwner(channelkinds.InboundEvent{ExternalIDs: tc.ids})
			assert.Equal(t, tc.provides, ok, "provides")
			if !tc.provides {
				assert.Empty(t, subj, "no subject when none is provided")
				return
			}
			require.NotEmpty(t, subj, "subject")
			assert.Equal(t, "user", identity.Subject(subj).ObjectType(),
				"the owner is a user subject, matching the schema's `relation started_by: user`")
		})
	}
}

// TestSessionOwnerProviderDeclared pins the capability itself. The controller
// probes for the INTERFACE, so implementing the method without the compile-time
// assertion would leave the same silent divergence one refactor away.
func TestSessionOwnerProviderDeclared(t *testing.T) {
	var k any = Kind{}
	_, ok := k.(channelkinds.SessionOwnerProvider)
	assert.True(t, ok, "fake must declare SessionOwnerProvider: it provides a starting user on every inbound")
}
