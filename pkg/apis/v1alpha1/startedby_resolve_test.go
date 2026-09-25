// pkg/apis/v1alpha1/startedby_resolve_test.go
//
// ResolveStartedByCanonical is the ONE definition of "who started this
// session" for every consumer that needs a canonical id rather than the raw
// annotation. It exists because three call sites — internal/cmd/runner's loop wiring,
// pkg/agent/runner's ResolveAuthSubjects, and test/e2e's in-process runner
// factory — each hand-rolled this precedence, and two of the three got it
// wrong in the same way: they skipped the stamped annotation and re-derived
// from the external id alone, silently substituting the SYNTHETIC
// kind:teamScope:externalID encoding for the email-derived canonical the
// channel pipeline had already resolved.
package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func resolveFixture(t *testing.T, kind string, annotations map[string]string) *AgentSession {
	t.Helper()
	sess := &AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-session", Namespace: "default", Annotations: annotations,
		},
	}
	if kind != "" {
		sess.Spec.InputChannel = &ChannelBinding{Kind: kind}
	}
	return sess
}

func TestResolveStartedByCanonical(t *testing.T) {
	const email = "dana@example.com"
	const externalID = "U0DEMO123"

	emailCanon, err := identity.VerifiedEmail(identity.Email(email), "").Canonical()
	require.NoError(t, err)
	syntheticCanon, err := identity.FromExternal(
		"slack", "", identity.RawExternalID(externalID), "",
	).AllowSynthetic().Canonical()
	require.NoError(t, err)

	cases := []struct {
		name        string
		kind        string
		annotations map[string]string
		want        identity.CanonicalUserID
	}{
		{
			name: "stamped canonical wins over re-derivation from the external id",
			kind: "slack",
			annotations: map[string]string{
				AnnotationStartedByCanonicalID: "user:" + emailCanon.String(),
				AnnotationStartedByExternalID:  externalID,
			},
			want: emailCanon,
		},
		{
			name:        "no stamped canonical: derives the synthetic from the external id",
			kind:        "slack",
			annotations: map[string]string{AnnotationStartedByExternalID: externalID},
			want:        syntheticCanon,
		},
		{
			name:        "stamped canonical, no external id: still returns the stamped canonical",
			kind:        "slack",
			annotations: map[string]string{AnnotationStartedByCanonicalID: "user:" + emailCanon.String()},
			want:        emailCanon,
		},
		{
			name:        "no starter annotations: empty, meaning no started_by",
			kind:        "slack",
			annotations: nil,
			want:        identity.CanonicalFromTrusted("", "test fixture"),
		},
		{
			name:        "kubectl-driven session (no input channel) with no annotations: empty",
			kind:        "",
			annotations: nil,
			want:        identity.CanonicalFromTrusted("", "test fixture"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ResolveStartedByCanonical(resolveFixture(t, tc.kind, tc.annotations)))
		})
	}
}

// TestResolveStartedByCanonicalDecodesToTheVerifiedEmail pins the property the
// user_profile capability depends on: the subject built from this canonical
// must decode back to the starter's email. pkg/agent/runner's speakerEmail()
// drops any subject whose decoded payload has no "@" — silently, since that is
// the ordinary state for a guest or a foreign-workspace user — so an encoding
// regression here would surface as "the agent doesn't know who I am" and
// nothing else.
func TestResolveStartedByCanonicalDecodesToTheVerifiedEmail(t *testing.T) {
	const email = "dana@example.com"
	emailCanon, err := identity.VerifiedEmail(identity.Email(email), "").Canonical()
	require.NoError(t, err)

	sess := resolveFixture(t, "slack", map[string]string{
		AnnotationStartedByCanonicalID: "user:" + emailCanon.String(),
		AnnotationStartedByExternalID:  "U0DEMO123",
	})

	canon := ResolveStartedByCanonical(sess)
	require.NotEmpty(t, canon)
	assert.Equal(t, email, identity.DecodeForDisplay("user:"+canon.String()))
}

// TestResolveStartedByCanonicalMatchesStartedByCanonicalWhenStamped keeps the
// resolving helper and the plain accessor from drifting: whenever the
// annotation is present they must agree, so a caller picking either one by
// name gets the same identity.
func TestResolveStartedByCanonicalMatchesStartedByCanonicalWhenStamped(t *testing.T) {
	emailCanon, err := identity.VerifiedEmail("dana@example.com", "").Canonical()
	require.NoError(t, err)
	sess := resolveFixture(t, "slack", map[string]string{
		AnnotationStartedByCanonicalID: "user:" + emailCanon.String(),
		AnnotationStartedByExternalID:  "U0DEMO123",
	})
	assert.Equal(t, StartedByCanonical(sess), ResolveStartedByCanonical(sess))
}
