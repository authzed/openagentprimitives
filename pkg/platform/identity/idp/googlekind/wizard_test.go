package googlekind_test

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
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// demoCallback is a redirect URI short enough for a note to carry whole.
const demoCallback = "https://ap.demo-cluster.example/oidc/callback/idp"

// demoClientID is the shape Google issues.
const demoClientID = "demo-client.apps.googleusercontent.com"

// accessDomains and accessAny are what the access screen's select reads under
// huh's accessible renderer: the 1-based index of the offered option. Named so
// a script reads as the choice it makes rather than as a bare number.
const (
	accessDomains = "1"
	accessAny     = "2"
)

// runGoogle drives the real screens over a scripted input, through the same
// plain driver an off-TTY run gets, and returns what the wizard produced.
//
// One tui.Run over one reader: huh builds a fresh, greedy scanner per field, so
// a second run handed the same stdin would find it already drained — every
// answer after the first silently defaulted, with no error anywhere.
func runGoogle(t *testing.T, in idp.WizardInput, script string) (idp.WizardOutput, *tui.State, string, error) {
	t.Helper()
	w := (&googlekind.Kind{}).Wizard()
	screens, err := w.Screens(context.Background(), in)
	require.NoError(t, err, "the google wizard must describe its screens")

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

func TestGoogleWizard_HappyPath_RestrictedToDomains(t *testing.T) {
	res, st, printed, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback},
		script(demoClientID, "demo-client-secret", accessDomains, "demo-corp.example,demo-corp.test"))
	require.NoError(t, err)

	assert.Equal(t, "google", res.Spec.Kind)
	assert.Equal(t, demoClientID, res.Spec.ClientID)
	assert.Equal(t, []string{"demo-corp.example", "demo-corp.test"}, res.Spec.AllowedEmailDomains)
	assert.False(t, res.Spec.AllowAnyEmail)

	assert.Equal(t, "idp-google", res.SecretName)
	assert.Equal(t, []byte("demo-client-secret"), res.SecretData["client_secret"])
	assert.Equal(t, "idp-google", res.Spec.ClientSecretRef.Name)
	assert.Equal(t, "client_secret", res.Spec.ClientSecretRef.Key)
	assert.Empty(t, res.Spec.ClientSecretRef.Namespace, "the wizard must leave Namespace for the dispatcher to fill")

	// The redirect URI is what the user has to carry to the Google console, so
	// it must be in front of them while they answer.
	assert.Contains(t, printed, demoCallback)

	assert.NotContains(t, noteValues(st), "demo-client-secret",
		"the client secret must never reach the summary")
}

func TestGoogleWizard_HappyPath_AllowAnyAccount(t *testing.T) {
	res, _, _, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback},
		script(demoClientID, "demo-client-secret", accessAny))
	require.NoError(t, err)
	assert.True(t, res.Spec.AllowAnyEmail)
	assert.Empty(t, res.Spec.AllowedEmailDomains)
}

// TestGoogleWizard_AllowAnyIsNeverReachedBySilence pins the one answer that
// must not be available by pressing Enter: huh's accessible renderer returns
// the bound value when its input runs out and reports no error doing so, so the
// restrictive option is the one the access screen binds.
func TestGoogleWizard_AllowAnyIsNeverReachedBySilence(t *testing.T) {
	_, st, _, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback},
		script(demoClientID, "demo-client-secret"))
	require.Error(t, err, "a run that never chose a policy must not produce a spec")
	// res is the zero WizardOutput on every error path, so asserting on it
	// here would assert nothing. What the run actually decided is in the
	// State, which is what the refusal above was derived from.
	assert.Equal(t, idpscreens.AccessDomains, st.Get(idpscreens.KeyAccess),
		"silence must land on the restrictive policy")
}

// TestGoogleWizard_NonStandardClientIDIsFlaggedNotRefused: a client ID without
// Google's suffix is almost always the wrong console field copied, but Google
// has changed the shape before — so it is recorded in the summary the user
// re-reads when sign-in does not work, and never refused.
func TestGoogleWizard_NonStandardClientIDIsFlaggedNotRefused(t *testing.T) {
	res, st, _, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback},
		script("not-a-standard-client-id", "demo-client-secret", accessDomains, "demo-corp.example"))
	require.NoError(t, err, "a non-standard client ID is a flag, not a refusal")
	assert.Equal(t, "not-a-standard-client-id", res.Spec.ClientID, "the typed value is still used")
	assert.Contains(t, noteValues(st), "check you copied the client ID")
}

func TestGoogleWizard_StandardClientIDIsNotFlagged(t *testing.T) {
	_, st, _, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback},
		script(demoClientID, "demo-client-secret", accessDomains, "demo-corp.example"))
	require.NoError(t, err)
	assert.NotContains(t, noteValues(st), "check you copied")
}

func TestGoogleWizard_RefusesAnUnanswered(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		wantErr string
	}{
		{
			name:    "no input at all: refuses, naming the client ID",
			script:  "",
			wantErr: "Client ID",
		},
		{
			name:    "no client secret on a first setup: refuses",
			script:  script(demoClientID),
			wantErr: "Client secret",
		},
		{
			name:    "restricted access with no domains: refuses",
			script:  script(demoClientID, "demo-client-secret", accessDomains),
			wantErr: "email domain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := runGoogle(t, idp.WizardInput{CallbackURL: demoCallback}, tc.script)
			require.Error(t, err)
			assert.Contains(t, tui.UserFacing(err).Error(), tc.wantErr)
		})
	}
}

func TestGoogleWizard_ReSetup_BlankKeepsEverything(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:                "google",
		ClientID:            demoClientID,
		AllowedEmailDomains: []string{"demo-corp.example"},
	}
	res, st, _, err := runGoogle(t, idp.WizardInput{
		CallbackURL:  demoCallback,
		Existing:     existing,
		SecretExists: true,
	}, script("", "", accessDomains, ""))
	require.NoError(t, err)

	assert.Equal(t, demoClientID, res.Spec.ClientID)
	assert.Equal(t, []string{"demo-corp.example"}, res.Spec.AllowedEmailDomains)
	assert.Nil(t, res.SecretData, "a blank secret with one already stored must keep it")
	assert.Contains(t, noteValues(st), "kept the stored one")
}

// TestGoogleWizard_ReSetup_HonorsExistingSecretRef: when the existing
// ClusterIdentityProvider declares a NON-default ClientSecretRef, both keeping
// and replacing the secret must reuse that ref rather than reverting to the
// convention name — otherwise the CR is re-applied pointing at a Secret that
// holds nothing, or the written data key and the declared key diverge.
func TestGoogleWizard_ReSetup_HonorsExistingSecretRef(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:                "google",
		ClientID:            demoClientID,
		ClientSecretRef:     spiceboxv1alpha1.ClusterSecretKeyRef{Name: "custom-google-secret", Key: "cs"},
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
			res, _, _, err := runGoogle(t, idp.WizardInput{
				CallbackURL:  demoCallback,
				Existing:     existing,
				SecretExists: true,
			}, script("", tc.secret, accessDomains, ""))
			require.NoError(t, err)

			assert.Equal(t, "custom-google-secret", res.SecretName)
			assert.Equal(t, "custom-google-secret", res.Spec.ClientSecretRef.Name)
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
