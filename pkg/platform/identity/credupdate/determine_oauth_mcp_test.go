package credupdate_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"

	// Registers the oauth-mcp builtin flow. This test is about the SHIPPED
	// wiring — catalog entry → registered flow → VerifyCredential → Determine —
	// so it must exercise the real flow, not a stand-in status constant.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/oauth_mcp"
)

// TestDetermine_OAuthMCPDeadCredentialIsNotRefusedAsLive is the behavioural
// half of the oauth-mcp fix: the flow-level change only matters because of what
// it does HERE.
//
// oauth-mcp is the provider every generated MCPServer defaults to, so this is
// the common path. While its Verify returned VerifyValid unconditionally,
// Determine's step 5 refused every request with CredentialLive — telling the
// agent the credential "still authenticates successfully" about a credential
// nobody had checked and that may well be dead. That is the platform asserting
// the opposite of the truth, and no card could ever be raised for an
// oauth-mcp origin.
//
// With an honest unsupported, the verdict falls through to the corroboration
// branch: the platform's OWN observed auth failures decide, and a dead
// credential reaches the unverified tier.
func TestDetermine_OAuthMCPDeadCredentialIsNotRefusedAsLive(t *testing.T) {
	prov, ok := provider.ByID("oauth-mcp")
	require.True(t, ok, "oauth-mcp must be present in the embedded provider catalog")

	// The real choke point, against a credential that is genuinely dead. No
	// stub: if the flow regresses to VerifyValid, this test is what fails.
	probe := builtins.VerifyCredential(context.Background(), prov,
		builtins.StoreValue{OAuth: &builtins.OAuthValue{
			AccessToken:  "at-revoked",
			RefreshToken: "rt-revoked",
		}})
	// assert, not require: if the flow regresses to VerifyValid we want the
	// per-case verdict assertions below to run anyway, so the failure output
	// names the DOWNSTREAM damage (tier none / credential_live) and not just
	// the probe status. The behaviour is the point; the status is the cause.
	assert.NotEqual(t, builtins.VerifyValid, probe.Status,
		"oauth-mcp performs no live check, so it must not report the credential valid")

	cases := []struct {
		name              string
		corroborated      bool
		wantTier          credupdate.Tier
		wantDetermination string
	}{
		{
			name:              "platform observed auth failures at this origin: opens the unverified tier, no longer refused as live",
			corroborated:      true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			name:              "no independent evidence: still no card, but refused for the honest reason (unconfirmed, not live)",
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A USER-owned oauth credential: an agent-owned one is refused on its
			// shape at step 3 before the probe is ever consulted, so it could not
			// show the probe's effect. The refresh attempt ran and failed for a
			// reason that says nothing about the credential (not invalid_grant),
			// which is exactly when the probe decides.
			out := credupdate.Determine(credupdate.Input{
				CredType:      "oauth",
				AgentOwned:    false,
				RefreshRan:    true,
				RefreshOK:     false,
				RefreshErr:    "Post \"https://auth.example.test/token\": dial tcp: i/o timeout",
				Probe:         probe.Status,
				ProbeDetail:   probe.Detail,
				Corroborated:  tc.corroborated,
				ProviderTitle: prov.Title,
			})

			assert.Equal(t, tc.wantTier, out.Tier, "tier")
			assert.Equal(t, tc.wantDetermination, out.Determination, "determination")
			assert.NotEqual(t, spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive, out.Determination,
				"a credential nobody checked must never be reported as live")
			assert.NotContains(t, out.Reason, "still authenticates successfully",
				"the reason must not assert successful authentication that never happened")
		})
	}
}

// TestDetermine_OAuthMCPObserved401OpensACard walks the whole chain a real
// MCP-origin card now depends on, with nothing hardcoded in the middle:
//
//	shipped providers/oauth-mcp.yaml authFailure: httpStatuses: [401]
//	  → IsAuthShaped(observed 401) → Input.Corroborated
//	  → Determine → TierUnverified / RejectedUnverified
//
// The test above supplies Corroborated as a boolean; this one DERIVES it, which
// is what proves the outcome is reachable rather than merely representable.
// Before this slice it was not: the flow answered VerifyValid so Determine
// refused at step 5 before consulting corroboration, and the catalog entry
// declared no authFailure: block, so IsAuthShaped could only ever return false.
// Both halves are required — either one missing and no card opens.
//
// Corroboration is also the ONLY path to a card here. A live probe is
// impossible for an audience-bound MCP token without an interface change
// (VerifyRequest carries no target URL), so unlike github-pat there is no
// VerifyRejected route to TierVerified standing behind this.
func TestDetermine_OAuthMCPObserved401OpensACard(t *testing.T) {
	prov, ok := provider.ByID("oauth-mcp")
	require.True(t, ok, "oauth-mcp must be present in the embedded provider catalog")
	require.NotNil(t, prov.AuthFailure,
		"oauth-mcp must declare authFailure: — it is the only evidence that can corroborate an MCP-origin credential")

	probe := builtins.VerifyCredential(context.Background(), prov,
		builtins.StoreValue{OAuth: &builtins.OAuthValue{AccessToken: "at-revoked"}})
	require.Equal(t, builtins.VerifyUnsupported, probe.Status,
		"the probe must be non-definitive, or the corroboration branch is never reached")

	cases := []struct {
		name              string
		obs               credupdate.Observation
		wantCorroborated  bool
		wantTier          credupdate.Tier
		wantDetermination string
	}{
		{
			name:              "the MCP server answered 401: corroborated, opens the unverified tier",
			obs:               credupdate.Observation{HTTPStatus: 401},
			wantCorroborated:  true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			// SECURITY: the agent chooses which resource it asks for, so it can
			// provoke a 403 on a perfectly live credential. Declaring [401] only
			// is what keeps that from becoming a card the agent manufactured.
			name:              "SECURITY: a 403 the agent can provoke does NOT corroborate; still refused",
			obs:               credupdate.Observation{HTTPStatus: 403},
			wantCorroborated:  false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "the server authenticated the call: positive evidence beats the claim; still refused",
			obs:               credupdate.Observation{HTTPStatus: 200, OriginAuthenticated: true},
			wantCorroborated:  false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			corroborated := credupdate.IsAuthShaped(tc.obs, prov.AuthFailure)
			require.Equal(t, tc.wantCorroborated, corroborated,
				"corroboration must be decided by the shipped catalog entry, not assumed")

			out := credupdate.Determine(credupdate.Input{
				CredType:      "oauth",
				AgentOwned:    false,
				Probe:         probe.Status,
				ProbeDetail:   probe.Detail,
				Corroborated:  corroborated,
				ProviderTitle: prov.Title,
			})

			assert.Equal(t, tc.wantTier, out.Tier, "tier")
			assert.Equal(t, tc.wantDetermination, out.Determination, "determination")
			assert.NotEmpty(t, out.Reason, "the reason is user-facing and is never empty")
		})
	}
}
