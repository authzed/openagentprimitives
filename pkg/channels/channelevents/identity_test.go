package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestExternalIdentityPrincipalRoundTrip(t *testing.T) {
	// raw slack identity → Principal → canonical is byte-identical to FromExternal
	e := channelevents.ExternalIdentity{Kind: "slack", TeamScope: "T1", ExternalID: "U1", Email: "u@example.com"}
	got, err := e.Principal().Canonical()
	require.NoError(t, err)
	want, err := identity.FromExternal("slack", "T1", "U1", "u@example.com").Canonical()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Subject passthrough → RawSubject → bare canonical
	e2 := channelevents.ExternalIdentity{Subject: "user:c2NvcGU"}
	got2, err := e2.Principal().Canonical()
	require.NoError(t, err)
	assert.Equal(t, identity.CanonicalFromTrusted("c2NvcGU", "test fixture"), got2)

	// no-email slack identity: canonical is the synthetic base64(kind:teamScope:externalID);
	// AllowSynthetic must be chained (Principal() intentionally does not — it is an opt-in),
	// matching the existing canonicalID() helper (pipeline.go) that resolves no-email users.
	eSyn := channelevents.ExternalIdentity{Kind: "slack", TeamScope: "T1", ExternalID: "U1"}
	gotSyn, err := eSyn.Principal().AllowSynthetic().Canonical()
	require.NoError(t, err)
	wantSyn, err := identity.FromExternal("slack", "T1", "U1", "").AllowSynthetic().Canonical()
	require.NoError(t, err)
	assert.Equal(t, wantSyn, gotSyn)

	// prove TeamScope/ExternalID actually forward (not tautological): a different externalID → different canonical
	eSyn2 := channelevents.ExternalIdentity{Kind: "slack", TeamScope: "T1", ExternalID: "U2"}
	gotSyn2, err := eSyn2.Principal().AllowSynthetic().Canonical()
	require.NoError(t, err)
	assert.NotEqual(t, gotSyn, gotSyn2, "externalID must influence the synthetic canonical")

	// no-email Principal WITHOUT AllowSynthetic errors — documents the fail-closed contract
	_, err = channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1"}.Principal().Canonical()
	assert.Error(t, err)

	// JSON round-trips the raw ExternalID (definitively raw)
	raw, _ := json.Marshal(e)
	var back channelevents.ExternalIdentity
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, identity.RawExternalID("U1"), back.ExternalID)
}
