package passwordkind_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
)

// demoPassword is long enough to clear the length floor. It is also the value
// every leak assertion below looks for, so its last four characters are
// deliberately not ordinary English: the tail assertion below would otherwise
// be satisfied by a summary line that merely contains the word "password",
// proving nothing about whether the value leaked.
const demoPassword = "demo-admin-pw-7k3q"

// runPassword drives the real screens over a scripted input, through the same
// plain driver an off-TTY run gets, and returns what the wizard produced.
//
// One tui.Run over one reader: huh builds a fresh, greedy scanner per field, so
// a second run handed the same stdin would find it already drained — every
// answer after the first silently defaulted, with no error anywhere.
func runPassword(t *testing.T, in idp.WizardInput, script string) (idp.WizardOutput, *tui.State, string, error) {
	t.Helper()
	w := (&passwordkind.Kind{}).Wizard()
	screens, err := w.Screens(context.Background(), in)
	require.NoError(t, err, "the password wizard must describe its screens")

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

func TestPasswordWizard_HappyPath(t *testing.T) {
	res, st, _, err := runPassword(t, idp.WizardInput{}, script(demoPassword, demoPassword))
	require.NoError(t, err)

	assert.Equal(t, "password", res.Spec.Kind)
	assert.Equal(t, "admin@ap.local", res.Spec.ClientID)
	assert.True(t, res.Spec.AllowAnyEmail, "password sign-in has no domain concept")
	assert.Empty(t, res.Spec.AllowedEmailDomains)

	assert.Equal(t, "idp-password", res.SecretName)
	assert.Equal(t, "idp-password", res.Spec.ClientSecretRef.Name)
	assert.Equal(t, "client_secret", res.Spec.ClientSecretRef.Key)
	assert.Empty(t, res.Spec.ClientSecretRef.Namespace, "the wizard must leave Namespace for the dispatcher to fill")

	require.Contains(t, res.SecretData, "client_secret")
	assert.NoError(t, bcrypt.CompareHashAndPassword(res.SecretData["client_secret"], []byte(demoPassword)),
		"the written secret must be a bcrypt hash of the entered password")
	assert.NotContains(t, string(res.SecretData["client_secret"]), demoPassword,
		"the password itself must never be stored")

	// The strongest of the leak assertions: a masked note would still carry the
	// last four characters of the password into scrollback.
	assert.NotContains(t, noteValues(st), demoPassword, "the password must never reach the summary")
	assert.NotContains(t, noteValues(st), demoPassword[len(demoPassword)-4:],
		"not even the tail of the password may reach the summary")
}

func TestPasswordWizard_NoInputAtAllOnAFirstSetupRefuses(t *testing.T) {
	// The output is deliberately not asserted on: a WizardOutput is the zero
	// value on every error path, so "no Secret material was written" is true
	// here by construction rather than by anything this wizard did. The
	// property worth asserting — that a refused run writes nothing to the
	// cluster — belongs where the writing happens, and is asserted there
	// (cmd/oap, TestIdpSetup_ARefusedWizardWritesNothing).
	_, _, _, err := runPassword(t, idp.WizardInput{}, "")
	require.Error(t, err)
	assert.Contains(t, tui.UserFacing(err).Error(), "New password",
		"the refusal must name the question in the user's words")
}

// TestPasswordWizard_RefusesAnAnswerThatArrivedWithoutBeingAsked drives the
// fail-closed choke point rather than the prompt.
//
// The prompt cannot be used to assert these messages: huh's accessible renderer
// re-runs a field whose validator refused, so a script of bad answers is
// consumed by the retry loop and the run ends on "nothing was supplied" — the
// message for input that ran out, not for the rule that was broken. That retry
// IS the recovery an interactive user gets (TestPasswordWizard_MistypedConfirmationCanBeRetyped
// covers it). What has to be checked separately is the path where no validator
// ran at all: an answer already in State is never presented, so the screen's
// own re-check is the only thing between a bad password and a stored hash.
func TestPasswordWizard_RefusesAnAnswerThatArrivedWithoutBeingAsked(t *testing.T) {
	cases := []struct {
		name     string
		password string
		confirm  string
		wantErr  string
	}{
		{
			name:     "shorter than the floor: refuses, stating the rule",
			password: "short",
			confirm:  "short",
			wantErr:  "at least 8 characters",
		},
		{
			name:     "confirmation does not match: refuses",
			password: demoPassword,
			confirm:  "something-else-entirely",
			wantErr:  "do not match",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := (&passwordkind.Kind{}).Wizard()
			screens, err := w.Screens(context.Background(), idp.WizardInput{})
			require.NoError(t, err)

			st := tui.NewState()
			st.Set(passwordkind.KeyPassword, tc.password)
			st.Set(passwordkind.KeyConfirm, tc.confirm)

			var out bytes.Buffer
			answered, runErr := tui.RunWith(context.Background(), screens, tui.Options{
				Theme: tui.NewTheme(tui.Caps{}),
				In:    strings.NewReader(""),
				Out:   &out,
			}, st)
			require.Empty(t, out.String(), "a fully seeded run must ask nothing")

			// The refusal may come from the screen's re-check or, if a screen
			// ever stops re-checking, from Result. Both are fail-closed; what
			// must never happen is a stored hash.
			//
			// The output is reported in the failure message rather than
			// asserted on: a WizardOutput is the zero value on every error
			// path, so a nil-SecretData assertion here would hold whether or
			// not the wizard refused. Naming what it produced is what makes a
			// failure diagnosable.
			if runErr == nil {
				res, resErr := w.Result(answered)
				require.Error(t, resErr,
					"the wizard accepted this answer and produced %+v with %d secret key(s)",
					res.Spec, len(res.SecretData))
				runErr = resErr
			}
			require.Error(t, runErr)

			msg := tui.UserFacing(runErr).Error()
			assert.Contains(t, msg, tc.wantErr)
			assert.NotContains(t, msg, tc.password, "a refusal must not quote the password back")
		})
	}
}

// TestPasswordWizard_MistypedConfirmationCanBeRetyped is the recovery path a
// mismatch actually takes at a terminal: the field refuses in place and asks
// again, so a slip costs a retype rather than the whole run.
func TestPasswordWizard_MistypedConfirmationCanBeRetyped(t *testing.T) {
	res, _, _, err := runPassword(t, idp.WizardInput{},
		script(demoPassword, "mistyped-confirmation", demoPassword))
	require.NoError(t, err, "a retyped confirmation must complete the run")
	require.NotNil(t, res.SecretData)
	assert.NoError(t, bcrypt.CompareHashAndPassword(res.SecretData["client_secret"], []byte(demoPassword)))
}

// TestPasswordWizard_NoInternalFramingReachesTheUser guards the one thing a
// refusal must not carry: this project's own sequencer vocabulary. A screen ID
// names a step in tui and nothing anyone operating `oap idp setup` can act on.
func TestPasswordWizard_NoInternalFramingReachesTheUser(t *testing.T) {
	_, _, _, err := runPassword(t, idp.WizardInput{}, "")
	require.Error(t, err)

	msg := tui.UserFacing(err).Error()
	assert.NotContains(t, msg, "tui:", "the sequencer's own framing reached the user")
	assert.NotContains(t, msg, "screen ", "a screen ID reached the user")
	assert.NotContains(t, msg, "apply screen")
	assert.NotContains(t, msg, "prepare screen")
}

func TestPasswordWizard_ReSetup_BlankKeepsTheStoredPassword(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:     "password",
		ClientID: "admin@ap.local",
	}
	// One blank line: the confirmation is skipped entirely, because there is
	// nothing to confirm.
	res, st, _, err := runPassword(t, idp.WizardInput{
		Existing:     existing,
		SecretExists: true,
	}, script(""))
	require.NoError(t, err)

	assert.Nil(t, res.SecretData, "a blank answer with a password already stored must keep it")
	assert.Equal(t, "admin@ap.local", res.Spec.ClientID)
	assert.Contains(t, noteValues(st), "kept the stored one")
}

func TestPasswordWizard_ReSetup_NewPasswordReplacesTheHash(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:     "password",
		ClientID: "admin@ap.local",
	}
	res, _, _, err := runPassword(t, idp.WizardInput{
		Existing:     existing,
		SecretExists: true,
	}, script("replacement-password", "replacement-password"))
	require.NoError(t, err)
	require.NotNil(t, res.SecretData)
	assert.NoError(t, bcrypt.CompareHashAndPassword(res.SecretData["client_secret"], []byte("replacement-password")))
}

// TestPasswordWizard_ReSetup_HonorsExistingSecretRef: when the existing
// ClusterIdentityProvider declares a NON-default ClientSecretRef, the run must
// reuse that ref rather than reverting to the convention name — the same rule
// oidckind and googlekind follow.
func TestPasswordWizard_ReSetup_HonorsExistingSecretRef(t *testing.T) {
	existing := &spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:            "password",
		ClientID:        "admin@ap.local",
		ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{Name: "custom-password-secret", Key: "pw"},
	}
	res, _, _, err := runPassword(t, idp.WizardInput{
		Existing:     existing,
		SecretExists: true,
	}, script("replacement-password", "replacement-password"))
	require.NoError(t, err)

	assert.Equal(t, "custom-password-secret", res.SecretName)
	assert.Equal(t, "custom-password-secret", res.Spec.ClientSecretRef.Name)
	assert.Equal(t, "pw", res.Spec.ClientSecretRef.Key)
	require.Contains(t, res.SecretData, "pw", "the written data key must match the ref's declared key")
}

func TestHashPassword_ProducesVerifiableBcryptHash(t *testing.T) {
	hash, err := passwordkind.HashPassword(demoPassword)
	require.NoError(t, err)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(demoPassword)))
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong")))
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
