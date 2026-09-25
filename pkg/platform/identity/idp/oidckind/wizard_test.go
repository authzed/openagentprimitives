package oidckind_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
)

// demoCallback is a redirect URI short enough for a note to carry whole, which
// is the case every test here except TestCallbackTooWideForANoteIsNotShownInOne.
const demoCallback = "https://ap.demo-cluster.example/oidc/callback/idp"

// accessDomains and accessAny are what the access screen's select reads under
// huh's accessible renderer: the 1-based index of the offered option. Named so
// a script reads as the choice it makes rather than as a bare number.
const (
	accessDomains = "1"
	accessAny     = "2"
)

// runOIDC drives the real screens over a scripted input, through the same plain
// driver an off-TTY run gets, and returns what the wizard produced.
//
// One tui.Run over one reader: huh builds a fresh, greedy scanner per field, so
// a second run handed the same stdin would find it already drained — every
// answer after the first silently defaulted, with no error anywhere.
func runOIDC(t *testing.T, in idp.WizardInput, script string) (idp.WizardOutput, *tui.State, string, error) {
	t.Helper()
	w := (&oidckind.Kind{}).Wizard()
	screens, err := w.Screens(context.Background(), in)
	require.NoError(t, err, "the oidc wizard must describe its screens")

	var out bytes.Buffer
	st, err := tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(script),
		Out:   &out,
	}, tui.NewState())
	if err != nil {
		return idp.WizardOutput{}, st, out.String(), err
	}
	res, err := w.Result(st)
	return res, st, out.String(), err
}

// script joins one answer per line, which is what the plain driver reads.
func script(answers ...string) string { return strings.Join(answers, "\n") + "\n" }

func TestOIDCWizard_HappyPath_RestrictedToDomains(t *testing.T) {
	res, st, printed, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback},
		script("https://login.demo-idp.example", "demo-client-id", "demo-client-secret", accessDomains, "demo-corp.example"))
	require.NoError(t, err)

	assert.Equal(t, "oidc", res.Spec.Kind)
	assert.Equal(t, "https://login.demo-idp.example", res.Spec.Issuer)
	assert.Equal(t, "demo-client-id", res.Spec.ClientID)
	assert.Equal(t, []string{"demo-corp.example"}, res.Spec.AllowedEmailDomains)
	assert.False(t, res.Spec.AllowAnyEmail)

	assert.Equal(t, "idp-oidc", res.SecretName)
	assert.Equal(t, []byte("demo-client-secret"), res.SecretData["client_secret"])
	assert.Equal(t, "idp-oidc", res.Spec.ClientSecretRef.Name)
	assert.Equal(t, "client_secret", res.Spec.ClientSecretRef.Key)
	assert.Empty(t, res.Spec.ClientSecretRef.Namespace, "the wizard must leave Namespace for the dispatcher to fill")

	// The redirect URI is what the user has to carry to their provider's
	// console, so it must be in front of them while they answer.
	assert.Contains(t, printed, demoCallback)

	assert.NotContains(t, noteValues(st), "demo-client-secret",
		"the client secret must never reach the summary")
}

func TestOIDCWizard_HappyPath_AllowAnyAccount(t *testing.T) {
	res, _, _, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback},
		script("https://login.demo-idp.example", "demo-client-id", "demo-client-secret", accessAny))
	require.NoError(t, err)
	assert.True(t, res.Spec.AllowAnyEmail)
	assert.Empty(t, res.Spec.AllowedEmailDomains)
}

// TestOIDCWizard_AllowAnyIsNeverReachedBySilence pins the one answer that must
// not be available by pressing Enter: huh's accessible renderer returns the
// bound value when its input runs out and reports no error doing so, so the
// restrictive option is the one the access screen binds.
func TestOIDCWizard_AllowAnyIsNeverReachedBySilence(t *testing.T) {
	// Everything answered except the access policy and what follows it.
	_, st, _, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback},
		script("https://login.demo-idp.example", "demo-client-id", "demo-client-secret"))
	require.Error(t, err, "a run that never chose a policy must not produce a spec")
	// res is the zero WizardOutput on every error path, so asserting on it
	// here would assert nothing. What the run actually decided is in the
	// State, which is what the refusal above was derived from.
	assert.Equal(t, idpscreens.AccessDomains, st.Get(idpscreens.KeyAccess),
		"silence must land on the restrictive policy")
}

func TestOIDCWizard_IssuerValidation(t *testing.T) {
	cases := []struct {
		name       string
		issuers    []string
		wantIssuer string
	}{
		{
			name:       "https issuer: accepted as typed",
			issuers:    []string{"https://login.demo-idp.example"},
			wantIssuer: "https://login.demo-idp.example",
		},
		{
			name:       "http on localhost: accepted for local development",
			issuers:    []string{"http://localhost:8080"},
			wantIssuer: "http://localhost:8080",
		},
		{
			name:       "http on 127.0.0.1: accepted for local development",
			issuers:    []string{"http://127.0.0.1:9090"},
			wantIssuer: "http://127.0.0.1:9090",
		},
		{
			name:       "http on a remote host: re-prompted, then the https answer is taken",
			issuers:    []string{"http://remote.demo-idp.example", "https://login.demo-idp.example"},
			wantIssuer: "https://login.demo-idp.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := append(append([]string{}, tc.issuers...),
				"demo-client-id", "demo-client-secret", accessDomains, "demo-corp.example")
			res, _, _, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback}, script(answers...))
			require.NoError(t, err)
			assert.Equal(t, tc.wantIssuer, res.Spec.Issuer)
		})
	}
}

func TestOIDCWizard_RefusesAnUnanswered(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		wantErr string
	}{
		{
			name:    "no input at all: refuses, naming the issuer",
			script:  "",
			wantErr: "Issuer URL",
		},
		{
			name:    "issuer only: refuses, naming the client ID",
			script:  script("https://login.demo-idp.example"),
			wantErr: "Client ID",
		},
		{
			name:    "no client secret on a first setup: refuses",
			script:  script("https://login.demo-idp.example", "demo-client-id"),
			wantErr: "Client secret",
		},
		{
			name:    "restricted access with no domains: refuses",
			script:  script("https://login.demo-idp.example", "demo-client-id", "demo-client-secret", accessDomains),
			wantErr: "email domain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback}, tc.script)
			require.Error(t, err)
			assert.Contains(t, tui.UserFacing(err).Error(), tc.wantErr)
		})
	}
}

// TestOIDCWizard_NoInternalFramingReachesTheUser guards the one thing a refusal
// must not carry: this project's own sequencer vocabulary. A screen ID names a
// step in tui and nothing anyone operating `oap idp setup` can act on.
func TestOIDCWizard_NoInternalFramingReachesTheUser(t *testing.T) {
	_, _, _, err := runOIDC(t, idp.WizardInput{CallbackURL: demoCallback}, "")
	require.Error(t, err)

	msg := tui.UserFacing(err).Error()
	assert.NotContains(t, msg, "tui:", "the sequencer's own framing reached the user")
	assert.NotContains(t, msg, "screen ", "a screen ID reached the user")
	assert.NotContains(t, msg, "apply screen")
	assert.NotContains(t, msg, "prepare screen")
}

func TestOIDCWizard_ReSetup_BlankKeepsEverything(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:                "oidc",
		Issuer:              "https://login.demo-idp.example",
		ClientID:            "demo-client-id",
		AllowedEmailDomains: []string{"demo-corp.example"},
	}
	// Blank issuer, blank client ID, blank secret, keep the restricted policy,
	// blank domains.
	res, st, _, err := runOIDC(t, idp.WizardInput{
		CallbackURL:  demoCallback,
		Existing:     existing,
		SecretExists: true,
	}, script("", "", "", accessDomains, ""))
	require.NoError(t, err)

	assert.Equal(t, "https://login.demo-idp.example", res.Spec.Issuer)
	assert.Equal(t, "demo-client-id", res.Spec.ClientID)
	assert.Equal(t, []string{"demo-corp.example"}, res.Spec.AllowedEmailDomains)
	assert.Nil(t, res.SecretData, "a blank secret with one already stored must keep it")
	assert.Contains(t, noteValues(st), "kept the stored one")
}

func TestOIDCWizard_ReSetup_ReplacesValues(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind: "oidc", Issuer: "https://old.demo-idp.example", ClientID: "old-client-id",
		AllowedEmailDomains: []string{"old.demo-corp.example"},
	}
	res, _, _, err := runOIDC(t, idp.WizardInput{
		CallbackURL:  demoCallback,
		Existing:     existing,
		SecretExists: true,
	}, script("https://new.demo-idp.example", "new-client-id", "new-client-secret", accessDomains, "new.demo-corp.example"))
	require.NoError(t, err)

	assert.Equal(t, "https://new.demo-idp.example", res.Spec.Issuer)
	assert.Equal(t, "new-client-id", res.Spec.ClientID)
	assert.Equal(t, []string{"new.demo-corp.example"}, res.Spec.AllowedEmailDomains)
	assert.Equal(t, []byte("new-client-secret"), res.SecretData["client_secret"])
}

// TestOIDCWizard_ReSetup_HonorsExistingSecretRef: when the existing
// ClusterIdentityProvider declares a NON-default ClientSecretRef (hand-authored
// or pre-declared, per the same bug class fixed in setup.Store), both keeping
// and replacing the secret must reuse that ref rather than reverting to the
// convention name — otherwise the CR is re-applied pointing at a Secret that
// holds nothing, or the written data key and the declared key diverge.
func TestOIDCWizard_ReSetup_HonorsExistingSecretRef(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:                "oidc",
		Issuer:              "https://login.demo-idp.example",
		ClientID:            "demo-client-id",
		ClientSecretRef:     spiceboxv1alpha1.ClusterSecretKeyRef{Name: "custom-idp-secret", Key: "cs"},
		AllowedEmailDomains: []string{"demo-corp.example"},
	}
	cases := []struct {
		name       string
		secret     string
		wantData   bool
		wantStored string
	}{
		{name: "blank secret: ref honored, nothing written", secret: "", wantData: false},
		{name: "new secret: ref honored, written under the ref's key", secret: "new-client-secret", wantData: true, wantStored: "new-client-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, _, err := runOIDC(t, idp.WizardInput{
				CallbackURL:  demoCallback,
				Existing:     existing,
				SecretExists: true,
			}, script("", "", tc.secret, accessDomains, ""))
			require.NoError(t, err)

			assert.Equal(t, "custom-idp-secret", res.SecretName)
			assert.Equal(t, "custom-idp-secret", res.Spec.ClientSecretRef.Name)
			assert.Equal(t, "cs", res.Spec.ClientSecretRef.Key)
			if !tc.wantData {
				assert.Nil(t, res.SecretData, "keeping the secret must not write new data")
				return
			}
			require.NotNil(t, res.SecretData)
			assert.Equal(t, []byte(tc.wantStored), res.SecretData["cs"],
				"the written data key must match the ref's declared key")
		})
	}
}

// noteValues flattens the summary into one string, for "did this value leak"
// assertions.
func noteValues(st *tui.State) string {
	var b strings.Builder
	for _, n := range st.Notes() {
		b.WriteString(n.Label)
		b.WriteString(": ")
		b.WriteString(n.Value)
		b.WriteString("\n")
	}
	return b.String()
}
