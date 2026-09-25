package credupdate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"

	// Registers the github-pat builtin flow. This test is about the SHIPPED
	// wiring — catalog entry → registered flow → VerifyCredential → Determine —
	// so it must exercise the real probe, not a stand-in status constant.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
)

// forbiddenProbeProvider returns the shipped github-pat catalog entry with its
// verify: probe redirected at a server that answers 403 the way a live-but-
// SSO-restricted personal access token is answered by the real API.
//
// The catalog entry is COPIED, not mutated: provider.ByID hands out the shared
// embedded entry, and rewriting its endpoint would leak a loopback URL into
// every other test in the process.
func forbiddenProbeProvider(t *testing.T) *provider.Provider {
	t.Helper()

	builtins.SetVerifyHTTPClient(func() *http.Client { return &http.Client{} })
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource protected by organization SAML enforcement. ` +
			`You must grant your token access to this organization."}`))
	}))
	t.Cleanup(srv.Close)

	shipped, ok := provider.ByID("github-pat")
	require.True(t, ok, "github-pat must be present in the embedded provider catalog")
	require.NotNil(t, shipped.Verify, "github-pat must declare a verify: probe, or this test proves nothing")
	require.NotNil(t, shipped.AuthFailure, "github-pat must declare authFailure:, which decides corroboration below")

	prov := *shipped
	verify := *shipped.Verify
	verify.Endpoint = srv.URL
	prov.Verify = &verify
	return &prov
}

// TestDetermine_ForbiddenProbeDoesNotOpenACardOnItsOwn pins the founding
// requirement of the whole credential-update flow: a human is asked to replace
// a credential only when it has genuinely stopped authenticating, NOT when it
// merely lacks access to something.
//
// A 403 is precisely "merely lacks access". A live token that is SSO-
// restricted, org-blocked, or missing a scope answers 403 at a probe endpoint
// exactly as a revoked one does, so a 403 cannot carry the definitive verdict
// that opens a card unaided — that would put a form in front of a human asking
// them to replace a credential that works.
//
// It is equally not proof of validity (some providers do answer 403 for a
// revoked token), so it must not suppress a card either. VerifyForbidden
// asserts neither and defers to independent evidence, which is what the
// corroboration cases below check.
//
// The same asymmetry is already declared on the catalog side — github-pat's
// authFailure: httpStatuses: [401] deliberately excludes 403 — so this test
// also pins the two halves against each other: the probe and the corroboration
// gate must treat a 403 the same way.
func TestDetermine_ForbiddenProbeDoesNotOpenACardOnItsOwn(t *testing.T) {
	prov := forbiddenProbeProvider(t)

	probe := builtins.VerifyCredential(context.Background(), prov,
		builtins.StoreValue{Bearer: "ghp_live-but-sso-restricted"})
	// The positive control for every "does not open a card" assertion below:
	// it proves the shipped wiring really ran and really reached the 403 arm,
	// so a no-card verdict cannot be passing because the probe never happened
	// (an unregistered flow answers Unsupported, which also opens no card).
	require.Equal(t, builtins.VerifyForbidden, probe.Status,
		"the shipped github-pat wiring must carry a 403 through to VerifyForbidden")
	// assert, not require: if the probe regresses to a definitive rejection we
	// want the per-case verdict assertions to run anyway, so the failure output
	// names the DOWNSTREAM damage (a card opened against a working credential)
	// rather than only its cause.
	assert.NotEqual(t, builtins.VerifyRejected, probe.Status,
		"a 403 says the request was refused, not that the credential died; it must not be a definitive rejection")
	assert.NotEqual(t, builtins.VerifyIndeterminate, probe.Status,
		"a 403 must stay distinguishable from a timeout: the CLI and the browser forms treat the two differently")
	assert.NotEmpty(t, probe.Detail, "no-silent-errors: the probe outcome always carries its cause")

	cases := []struct {
		name              string
		obs               credupdate.Observation
		wantCorroborated  bool
		wantTier          credupdate.Tier
		wantDetermination string
	}{
		{
			name:              "403 probe alone: no card — the founding requirement, a working-but-unauthorized token is never replaced",
			obs:               credupdate.Observation{},
			wantCorroborated:  false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "403 probe + an observed 401 the agent cannot forge: opens the unverified tier",
			obs:               credupdate.Observation{HTTPStatus: 401},
			wantCorroborated:  true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			name:              "403 probe + an observed 403 the agent can provoke: still no card, 403 is non-evidence on both sides",
			obs:               credupdate.Observation{HTTPStatus: 403},
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
				CredType:      "static",
				AgentOwned:    false,
				Probe:         probe.Status,
				ProbeDetail:   probe.Detail,
				Corroborated:  corroborated,
				ProviderTitle: prov.Title,
			})

			assert.Equal(t, tc.wantTier, out.Tier, "tier")
			assert.Equal(t, tc.wantDetermination, out.Determination, "determination")
			assert.NotEqual(t, credupdate.TierVerified, out.Tier,
				"a 403 is never the provider definitively rejecting the credential, so it can never reach the verified tier")
			assert.NotEmpty(t, out.Reason, "the reason is user-facing and is never empty")
		})
	}
}
