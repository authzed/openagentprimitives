package credupdate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// baseInput is a static credential with a definitive rejection — the simplest
// case that opens a card. Each test mutates one field away from it so a
// failure names exactly which input moved.
func baseInput() credupdate.Input {
	return credupdate.Input{
		CredType:      "static",
		Probe:         builtins.VerifyRejected,
		ProbeDetail:   "GitHub rejected the token: 401 Unauthorized — Bad credentials",
		ProviderTitle: "GitHub",
	}
}

func TestDetermine_ProbeMatrix(t *testing.T) {
	cases := []struct {
		name              string
		probe             builtins.VerifyStatus
		corroborated      bool
		wantTier          credupdate.Tier
		wantDetermination string
	}{
		{
			name:              "valid probe refuses even without corroboration: a live token is an access problem",
			probe:             builtins.VerifyValid,
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive,
		},
		{
			name:              "valid probe refuses WITH corroboration too: the probe beats an observed failure",
			probe:             builtins.VerifyValid,
			corroborated:      true,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive,
		},
		{
			name:              "rejected probe opens verified without corroboration: the provider's own 401 is definitive",
			probe:             builtins.VerifyRejected,
			corroborated:      false,
			wantTier:          credupdate.TierVerified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		},
		{
			name:              "rejected probe opens verified with corroboration",
			probe:             builtins.VerifyRejected,
			corroborated:      true,
			wantTier:          credupdate.TierVerified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		},
		{
			// A 403 is never on its own the evidence that opens a card: the
			// provider took the credential and refused this one request, which
			// is what a live-but-SSO-restricted token looks like too.
			name:              "forbidden without corroboration refuses: a working-but-unauthorized token is never replaced",
			probe:             builtins.VerifyForbidden,
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "forbidden with corroboration opens unverified, exactly as indeterminate does",
			probe:             builtins.VerifyForbidden,
			corroborated:      true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			name:              "indeterminate without corroboration refuses: the agent's word alone never suffices",
			probe:             builtins.VerifyIndeterminate,
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "indeterminate with corroboration opens unverified",
			probe:             builtins.VerifyIndeterminate,
			corroborated:      true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			name:              "unsupported without corroboration refuses",
			probe:             builtins.VerifyUnsupported,
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "unsupported with corroboration opens unverified",
			probe:             builtins.VerifyUnsupported,
			corroborated:      true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
		{
			// The default arm. A verdict this build has no branch for is read as
			// "did not settle the question", which is the conservative reading
			// here: it withholds the card unless the platform independently
			// observed auth failures.
			name:              "a verdict this build does not know, without corroboration: refuses",
			probe:             "verdict-this-build-does-not-know",
			corroborated:      false,
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "a verdict this build does not know, with corroboration: opens unverified",
			probe:             "verdict-this-build-does-not-know",
			corroborated:      true,
			wantTier:          credupdate.TierUnverified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Probe = tc.probe
			in.Corroborated = tc.corroborated

			got := credupdate.Determine(in)

			assert.Equal(t, tc.wantTier, got.Tier)
			assert.Equal(t, tc.wantDetermination, got.Determination)
			assert.NotEmpty(t, got.Reason, "every outcome must carry a human-readable reason — no silent refusals")
		})
	}
}

func TestDetermine_PreSteps(t *testing.T) {
	cases := []struct {
		name              string
		mutate            func(*credupdate.Input)
		wantTier          credupdate.Tier
		wantDetermination string
		wantReasonSubstr  string
	}{
		{
			name:              "federated refuses NotUpdatable: there is no stored token to re-enter",
			mutate:            func(in *credupdate.Input) { in.CredType = "federated" },
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			wantReasonSubstr:  "minted on demand",
		},
		{
			name: "federated refuses even when the probe rejected: no human action would help",
			mutate: func(in *credupdate.Input) {
				in.CredType = "federated"
				in.Probe = builtins.VerifyRejected
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			wantReasonSubstr:  "minted on demand",
		},
		{
			name: "successful refresh refuses SelfHealed: never ask a human for what the machine did",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.RefreshRan = true
				in.RefreshOK = true
				in.Probe = builtins.VerifyValid
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationSelfHealed,
			wantReasonSubstr:  "refreshed",
		},
		{
			name: "invalid_grant opens verified without any probe: a dead refresh token is definitive",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.RefreshRan = true
				in.RefreshOK = false
				in.RefreshErr = `token endpoint returned 400: {"error":"invalid_grant"}`
				in.Probe = builtins.VerifyUnsupported
				in.Corroborated = false
			},
			wantTier:          credupdate.TierVerified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedRefreshDead,
			wantReasonSubstr:  "re-authoriz",
		},
		{
			name: "a non-invalid_grant refresh failure falls through to the probe, not to a card",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.RefreshRan = true
				in.RefreshOK = false
				in.RefreshErr = "dial tcp: connection refused"
				in.Probe = builtins.VerifyIndeterminate
				in.Corroborated = false
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
			wantReasonSubstr:  "could not confirm",
		},
		{
			// Without this the card publishes, an admin opens it, the write
			// path refuses with a 409 ("this credential can't be replaced
			// here"), and the request then EXPIRES announcing that nobody
			// updated the credential — a live honesty failure about a human who
			// did exactly what was asked.
			name: "an AGENT-OWNED oauth credential refuses NotUpdatable: nobody can paste a replacement",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.AgentOwned = true
				in.Probe = builtins.VerifyRejected
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			wantReasonSubstr:  "re-authorized by an operator",
		},
		{
			name: "an agent-owned oauth credential with a DEAD refresh token is refused too, not opened as verified",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.AgentOwned = true
				in.RefreshRan = true
				in.RefreshOK = false
				in.RefreshErr = `token endpoint returned 400: {"error":"invalid_grant"}`
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			wantReasonSubstr:  "re-authorized by an operator",
		},
		{
			// Ordering guard: the machine's own fix must still win. Refusing
			// before step 2 would turn every routine agent-token refresh into a
			// "report the failure instead" dead end.
			name: "an agent-owned oauth credential that SELF-HEALED still reports SelfHealed",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.AgentOwned = true
				in.RefreshRan = true
				in.RefreshOK = true
			},
			wantTier:          credupdate.TierNone,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationSelfHealed,
			wantReasonSubstr:  "refreshed",
		},
		{
			// The scope guard. A PERSON's oauth credential is genuinely
			// fixable from the card: the link offers them the OAuth ceremony
			// for their own account. Refusing it here would break the flow the
			// earlier slices shipped.
			name: "a USER-owned oauth credential is untouched: its card really can fix it",
			mutate: func(in *credupdate.Input) {
				in.CredType = "oauth"
				in.AgentOwned = false
				in.Probe = builtins.VerifyRejected
			},
			wantTier:          credupdate.TierVerified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			wantReasonSubstr:  "401 Unauthorized",
		},
		{
			// And the type guard: agent-owned STATIC credentials are the whole
			// point of this slice and must keep opening cards.
			name: "an agent-owned STATIC credential still opens a card: that is the flow this slice exists for",
			mutate: func(in *credupdate.Input) {
				in.AgentOwned = true
				in.Probe = builtins.VerifyRejected
			},
			wantTier:          credupdate.TierVerified,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			wantReasonSubstr:  "401 Unauthorized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mutate(&in)

			got := credupdate.Determine(in)

			assert.Equal(t, tc.wantTier, got.Tier)
			assert.Equal(t, tc.wantDetermination, got.Determination)
			assert.Contains(t, got.Reason, tc.wantReasonSubstr)
		})
	}
}

// TestDetermine_ReasonQuotesTheProviderVerdict pins the property that makes the
// card trustworthy: on a verified rejection the reason repeats the provider's
// OWN words, so the human reads GitHub's verdict rather than the agent's claim.
func TestDetermine_ReasonQuotesTheProviderVerdict(t *testing.T) {
	in := baseInput()
	in.ProbeDetail = "GitHub rejected the token: 401 Unauthorized — Bad credentials"

	got := credupdate.Determine(in)

	assert.Equal(t, credupdate.TierVerified, got.Tier)
	assert.Contains(t, got.Reason, "401 Unauthorized")
	assert.Contains(t, got.Reason, "Bad credentials")
}

// TestProbeWouldNotHelp_AgreesWithDetermine keeps the reconciler's
// probe-skipping predicate honest.
//
// The predicate is an optimization, but a wrong one is a CORRECTNESS bug in
// both directions: too loose and the reconciler skips a probe whose result the
// verdict actually needed (a credential silently refused as unverifiable); too
// tight and it pays for provider egress on every request for a credential the
// verdict refuses on shape alone. So assert the property that defines it —
// where it says "true", Determine's verdict is the SAME for every probe
// result — over the whole credential-shape space rather than a hand-picked row.
func TestProbeWouldNotHelp_AgreesWithDetermine(t *testing.T) {
	probes := []builtins.VerifyStatus{
		builtins.VerifyValid, builtins.VerifyRejected, builtins.VerifyForbidden,
		builtins.VerifyIndeterminate, builtins.VerifyUnsupported,
	}
	for _, credType := range []string{"static", "oauth", "federated"} {
		for _, agentOwned := range []bool{false, true} {
			for _, corroborated := range []bool{false, true} {
				in := credupdate.Input{
					CredType: credType, AgentOwned: agentOwned, Corroborated: corroborated,
					ProbeDetail: "the provider rejected it", ProviderTitle: "GitHub",
				}
				if !credupdate.ProbeWouldNotHelp(in) {
					continue
				}
				base := in
				base.Probe = probes[0]
				want := credupdate.Determine(base)
				for _, p := range probes[1:] {
					next := in
					next.Probe = p
					assert.Equal(t, want, credupdate.Determine(next),
						"ProbeWouldNotHelp says the probe is irrelevant for credType=%q agentOwned=%v, "+
							"but Determine's verdict moved with probe=%v -- the reconciler is skipping a probe the verdict needs",
						credType, agentOwned, p)
				}
			}
		}
	}
}

// TestProbeWouldNotHelp_StillProbesWhatTheVerdictNeeds is the other direction:
// the shapes the credential-update flow exists to serve must keep being probed.
func TestProbeWouldNotHelp_StillProbesWhatTheVerdictNeeds(t *testing.T) {
	assert.False(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "static", AgentOwned: true}),
		"an agent's own STATIC credential is exactly what this slice makes replaceable; its probe is the evidence")
	assert.False(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "static"}),
		"a person's static credential is the original flow")
	assert.False(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "oauth"}),
		"a person's oauth credential IS fixable from the card, via their own re-authorization")
}

// TestProbeWouldNotHelp_MintedTypesHaveNothingToProbe pins ProbeWouldNotHelp's
// registry-driven form against the literal-comparison behavior it replaces:
// federated (Minted) always short-circuits the probe, an agent-owned oauth
// (NeedsRefresh) bundle does too, and a plain static credential still probes.
func TestProbeWouldNotHelp_MintedTypesHaveNothingToProbe(t *testing.T) {
	assert.True(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "federated"}),
		"nothing is stored, so there is nothing to probe or replace")
	assert.True(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "oauth", AgentOwned: true}),
		"an agent-owned oauth bundle can only be minted by the ceremony")
	assert.False(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "static"}))
}

// TestProbeWouldNotHelp_UnregisteredType pins the fail-closed answer for a
// credential type this build's registry does not know: assume the probe would
// not help rather than open a card for a shape nothing understands.
func TestProbeWouldNotHelp_UnregisteredType(t *testing.T) {
	assert.True(t, credupdate.ProbeWouldNotHelp(credupdate.Input{CredType: "unknown-shape"}))
}

// TestDetermine_UnregisteredCredType pins Determine's own fail-closed answer
// for the same unrecognized-type input ProbeWouldNotHelp refuses to probe:
// TierNone, with a Reason that says so plainly rather than falling through to
// the probe-driven steps (which never ran, per ProbeWouldNotHelp above).
func TestDetermine_UnregisteredCredType(t *testing.T) {
	in := baseInput()
	in.CredType = "unknown-shape"

	got := credupdate.Determine(in)

	assert.Equal(t, credupdate.TierNone, got.Tier)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable, got.Determination)
	assert.Contains(t, got.Reason, "unknown-shape",
		"the refusal must name the type it did not recognize, so an operator can find the wiring gap")
}

// TestDetermine_FederatedMessageFallsBackToDisplayNameWithoutAProviderTitle
// pins the new title fallback: when the reconciler never populated
// ProviderTitle (no provider.ByID match), the federated refusal must name the
// credential kind itself rather than read "This the provider credential...".
func TestDetermine_FederatedMessageFallsBackToDisplayNameWithoutAProviderTitle(t *testing.T) {
	in := baseInput()
	in.CredType = "federated"
	in.ProviderTitle = ""

	got := credupdate.Determine(in)

	assert.Equal(t, credupdate.TierNone, got.Tier)
	assert.Contains(t, got.Reason, "Federated (ID-JAG)",
		"absent a provider title, the credential kind's own DisplayName must fill the sentence")
	assert.NotContains(t, got.Reason, "This the provider credential",
		"the old universal \"the provider\" fallback reads as broken grammar for this sentence shape")
}

// mintedNonFederatedKind stands in for a kind like githubApp: Minted() is
// true, but it is not federated — nothing about it is minted "from the
// user's enterprise identity" via an ID-JAG exchange. Registering a SECOND
// minted kind is what makes Determine's Step 1 prose testable as
// type-neutral at all: today federated is the only Minted kind, so a message
// that hardcodes federated's own minting mechanism passes every existing
// test while being wrong for any other minted kind.
type mintedNonFederatedKind struct{}

var _ credkind.Kind = mintedNonFederatedKind{}

func (mintedNonFederatedKind) Type() string        { return "demo-minted" }
func (mintedNonFederatedKind) Minted() bool        { return true }
func (mintedNonFederatedKind) NeedsRefresh() bool  { return false }
func (mintedNonFederatedKind) DisplayName() string { return "Demo Minted Kind" }
func (mintedNonFederatedKind) Projectable() bool   { return false }

func (mintedNonFederatedKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (mintedNonFederatedKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

func (mintedNonFederatedKind) SecretRefPath() []string { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (mintedNonFederatedKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (mintedNonFederatedKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return nil
}

func (mintedNonFederatedKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("credential %q: mintedNonFederatedKind cannot be built by the setup flow", name)
}

func (mintedNonFederatedKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("mintedNonFederatedKind: Resolve not exercised by this test")
}

func (mintedNonFederatedKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("mintedNonFederatedKind: minted, nothing stored to read")
}

func (mintedNonFederatedKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }
func (mintedNonFederatedKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string {
	return nil
}

// TestDetermine_MintedNonFederatedKindDoesNotUseFederatedSpecificProse pins
// the fix for Step 1's refusal prose: before the fix it unconditionally said
// "minted on demand from the user's enterprise identity ... re-authenticate
// with the identity provider" for EVERY Minted kind, which happened to read
// correctly only because federated was the only one registered. This
// registers a second Minted kind that is not federated and proves the
// message no longer names federated's own minting mechanism, while still
// falling back to the kind's own DisplayName absent a ProviderTitle.
func TestDetermine_MintedNonFederatedKindDoesNotUseFederatedSpecificProse(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(mintedNonFederatedKind{})

	in := baseInput()
	in.CredType = "demo-minted"
	in.ProviderTitle = ""

	got := credupdate.Determine(in)

	assert.Equal(t, credupdate.TierNone, got.Tier)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable, got.Determination)
	assert.Contains(t, got.Reason, "Demo Minted Kind",
		"absent a provider title, the message must name this kind's own DisplayName")
	assert.NotContains(t, got.Reason, "enterprise identity",
		"the message must not hardcode federated's own minting mechanism for a different minted kind")
	assert.NotContains(t, got.Reason, "re-authenticate with the identity provider",
		"the message must not hardcode federated's own remediation for a different minted kind")
	// The two checks above pin the specific phrases the OLD message used; this
	// one pins the actual stated rule ("no message anywhere may hardcode
	// 'federated' for a generic minted kind") directly, so a future rewrite
	// that avoids those exact phrases but still names "federated" some other
	// way is still caught. (Neither the old nor the new Step-1 template
	// literally contains the word "federated" — this kind's own DisplayName,
	// "Demo Minted Kind", doesn't either — so this assertion does not by
	// itself distinguish pre-fix from post-fix code here; it is defense
	// against a regression the phrase-level checks would not catch.)
	assert.NotContains(t, got.Reason, "federated",
		"no message here may hardcode the word \"federated\" for a generic minted kind")
}
