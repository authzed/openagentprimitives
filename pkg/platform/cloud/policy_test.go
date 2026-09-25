// pkg/platform/cloud/policy_test.go
//
// The tunnel policy asserted over the WHOLE registry, plus the refusal every
// PublicEndpoint creation path must pass through. Both are registry-wide on
// purpose: a kind added without deciding its policy inherits the zero value,
// and the zero value is the one that refuses.
package cloud

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicEndpointPolicy_IsAnsweredByEveryRegisteredKind(t *testing.T) {
	want := map[string]PublicEndpointPolicy{
		KeyLocal:   PublicEndpointAlways,
		KeyDesktop: PublicEndpointOnDemand,
		KeyDefault: PublicEndpointNever,
		KeyGKE:     PublicEndpointNever,
		KeyEKS:     PublicEndpointNever,
		KeyAKS:     PublicEndpointNever,
	}
	for _, key := range RegisteredKeys() {
		got := MustFor(key).InstallProfile().PublicEndpointPolicy()
		assert.Equal(t, want[key], got, "kind %q", key)
	}
	assert.Len(t, want, len(RegisteredKeys()),
		"a kind was added without deciding its tunnel policy")
}

func TestPublicEndpointPolicy_DesktopDiffersFromLocal(t *testing.T) {
	assert.NotEqual(t,
		MustFor(KeyLocal).InstallProfile().PublicEndpointPolicy(),
		MustFor(KeyDesktop).InstallProfile().PublicEndpointPolicy(),
		"this is the answer that makes desktop a distinct kind")
}

// TestPublicEndpointPolicy_SaysWhoCreatesTheCR pins the OTHER derived
// predicate. AllowsTunnel answers "may one exist"; CreatedAtInstall answers
// "does `oap install` make one" — and they differ for exactly one kind, which
// is why neither can be spelled as a comparison at a call site.
func TestPublicEndpointPolicy_SaysWhoCreatesTheCR(t *testing.T) {
	assert.True(t, PublicEndpointAlways.CreatedAtInstall())
	assert.False(t, PublicEndpointOnDemand.CreatedAtInstall(),
		"OnDemand permits a tunnel but install must not create one; something that needs a webhook does")
	assert.False(t, PublicEndpointNever.CreatedAtInstall())

	var unset PublicEndpointPolicy
	assert.False(t, unset.CreatedAtInstall(), "the zero value must not create anything")
	assert.False(t, PublicEndpointPolicy(99).CreatedAtInstall(),
		"an unrecognized policy must not create anything either")
}

func TestPublicEndpoint_IsRefusedOnAKindWithRealIngress(t *testing.T) {
	err := CheckPublicEndpointAllowed(MustFor(KeyGKE))
	require.Error(t, err)
	assert.Contains(t, err.Error(), KeyGKE,
		"the refusal must name the kind: an operator who set AP_CLUSTER_KIND wrongly needs to see which one answered")
	assert.Contains(t, err.Error(), "ingress",
		"and say what to use instead, since the alternative is not obvious from the refusal alone")

	require.NoError(t, CheckPublicEndpointAllowed(MustFor(KeyLocal)))
	require.NoError(t, CheckPublicEndpointAllowed(MustFor(KeyDesktop)))
}

// TestCheckPublicEndpointAllowed_RefusesEveryNeverKind covers the other three
// durable kinds too, so a kind whose profile is later given its own override
// cannot slip past the single gke case above.
func TestCheckPublicEndpointAllowed_RefusesEveryNeverKind(t *testing.T) {
	for _, key := range RegisteredKeys() {
		s := MustFor(key)
		err := CheckPublicEndpointAllowed(s)
		if s.InstallProfile().PublicEndpointPolicy().AllowsTunnel() {
			assert.NoError(t, err, "kind %q permits a tunnel", key)
			continue
		}
		assert.Error(t, err, "kind %q refuses a tunnel and the check must say so", key)
	}
}

// TestCheckPublicEndpointAllowed_RefusesANilStrategy is the typed-nil door.
// Every caller resolves its Strategy from the registry, so nil means the
// resolution was skipped — which must refuse, not dereference.
func TestCheckPublicEndpointAllowed_RefusesANilStrategy(t *testing.T) {
	var s Strategy
	err := CheckPublicEndpointAllowed(s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster kind",
		"the refusal must say what was missing")
}
