package identitycmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"

	// Blank imports register the idp kinds used in tests.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
)

// idpAccessDomains is what the sign-in-policy select reads under huh's
// accessible renderer: the 1-based index of the offered option, the first of
// which restricts sign-in to listed email domains.
const idpAccessDomains = "1"

// idpScript joins one wizard answer per line, which is what the plain driver
// reads. One reader for the whole run: huh builds a fresh, greedy scanner per
// field, so a second reader over the same stdin would find it already drained.
func idpScript(answers ...string) string { return strings.Join(answers, "\n") + "\n" }

// newIdpFakeClient returns a fake client with the scheme and status subresource
// registered for ClusterIdentityProvider.
func newIdpFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(aptest.Scheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.ClusterIdentityProvider{})
	if len(objs) > 0 {
		b = b.WithObjects(objs...)
	}
	return b.Build()
}

// webdURLConfigMapForIdp creates the webd external-URL ConfigMap.
func webdURLConfigMapForIdp(baseURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: externalurl.Namespace,
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
		},
		Data: map[string]string{
			spiceboxv1alpha1.WebdTrustedURLKey: baseURL,
		},
	}
}

// markIdpValid sets the ClusterIdentityProvider Valid condition to True.
// Used in tests to unblock the poll after creation.
func markIdpValid(t *testing.T, c client.Client) {
	t.Helper()
	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	err := c.Get(context.Background(), types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cidp)
	if err != nil {
		return // not created yet; poller will retry
	}
	cidp.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.ConditionIdPValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonIdPReady,
		LastTransitionTime: metav1.Now(),
	}}
	_ = c.Status().Update(context.Background(), &cidp)
}

// withIdpPollSeams overrides the poll interval and timeout for fast tests.
func withIdpPollSeams(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	origInterval := idpSetupPollInterval
	origTimeout := idpSetupPollTimeout
	idpSetupPollInterval = interval
	idpSetupPollTimeout = timeout
	t.Cleanup(func() {
		idpSetupPollInterval = origInterval
		idpSetupPollTimeout = origTimeout
	})
}

// TestIdpSetup_HappyGoogle verifies a google setup:
//   - Secret created with client_secret key.
//   - ClusterIdentityProvider created with correct spec fields.
//   - ClientSecretRef.Namespace filled with operator namespace.
func TestIdpSetup_HappyGoogle(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)

	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))

	// Background goroutine marks the CR valid shortly after creation.
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(20 * time.Millisecond)
			markIdpValid(t, c)
		}
	}()

	stdin := strings.NewReader(idpScript("my-client.apps.googleusercontent.com", "my-secret", idpAccessDomains, "example.com"))
	var out bytes.Buffer
	require.NoError(t, RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "google"))

	// Secret created.
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Name: "idp-google", Namespace: externalurl.Namespace,
	}, &secret), "secret must be created")
	assert.Equal(t, []byte("my-secret"), secret.Data["client_secret"])

	// ClusterIdentityProvider created with correct spec.
	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Name: spiceboxv1alpha1.ClusterIdentityProviderName,
	}, &cidp), "ClusterIdentityProvider must be created")
	assert.Equal(t, "google", cidp.Spec.Kind)
	assert.Equal(t, "my-client.apps.googleusercontent.com", cidp.Spec.ClientID)
	assert.Equal(t, []string{"example.com"}, cidp.Spec.AllowedEmailDomains)
	assert.Equal(t, externalurl.Namespace, cidp.Spec.ClientSecretRef.Namespace, "dispatcher must fill Namespace")
	assert.Equal(t, "idp-google", cidp.Spec.ClientSecretRef.Name)
	assert.Equal(t, "client_secret", cidp.Spec.ClientSecretRef.Key)

	assert.Contains(t, out.String(), "configured and valid")
}

// TestIdpSetup_HappyOIDC verifies an oidc setup creates the secret and CR
// with the issuer field populated.
func TestIdpSetup_HappyOIDC(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)

	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(20 * time.Millisecond)
			markIdpValid(t, c)
		}
	}()

	stdin := strings.NewReader(idpScript("https://login.example.com", "client-id", "client-secret", idpAccessDomains, "example.com"))
	var out bytes.Buffer
	require.NoError(t, RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "oidc"))

	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Name: spiceboxv1alpha1.ClusterIdentityProviderName,
	}, &cidp))
	assert.Equal(t, "oidc", cidp.Spec.Kind)
	assert.Equal(t, "https://login.example.com", cidp.Spec.Issuer)
	assert.Equal(t, externalurl.Namespace, cidp.Spec.ClientSecretRef.Namespace)
}

// TestIdpSetup_UnknownKind verifies that an unknown kind returns an error that
// lists the available kinds.
func TestIdpSetup_UnknownKind(t *testing.T) {
	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))
	stdin := strings.NewReader("")
	var out bytes.Buffer
	err := RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "nonexistent-kind")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent-kind")
	// Should list available kinds.
	assert.Contains(t, err.Error(), "google")
	assert.Contains(t, err.Error(), "oidc")
}

// TestIdpSetup_NoExternalURL verifies that a missing external URL prints
// guidance and returns an error.
func TestIdpSetup_NoExternalURL(t *testing.T) {
	c := newIdpFakeClient(t) // no ConfigMap
	stdin := strings.NewReader("")
	var out bytes.Buffer
	err := RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "google")
	require.Error(t, err)
	assert.Contains(t, out.String(), "oap init --local")
}

// TestIdpStatus_Absent verifies that status with no CR prints the setup hint.
func TestIdpStatus_Absent(t *testing.T) {
	c := newIdpFakeClient(t)
	var out bytes.Buffer
	require.NoError(t, runIdpStatus(context.Background(), &out, c))
	assert.Contains(t, out.String(), "oap idp setup")
}

// TestIdpStatus_Present verifies that status with a CR prints the spec fields.
func TestIdpStatus_Present(t *testing.T) {
	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: spiceboxv1alpha1.ClusterIdentityProviderName,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "google",
			ClientID: "my-client.apps.googleusercontent.com",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: externalurl.Namespace,
				Name:      "idp-google",
				Key:       "client_secret",
			},
			AllowedEmailDomains: []string{"example.com"},
		},
	}
	c := newIdpFakeClient(t, cidp)
	var out bytes.Buffer
	require.NoError(t, runIdpStatus(context.Background(), &out, c))
	s := out.String()
	assert.Contains(t, s, "google")
	assert.Contains(t, s, "my-client.apps.googleusercontent.com")
	assert.Contains(t, s, "example.com")
}

// TestIdpRemove_WithYes verifies --yes removes both the CR and the Secret.
func TestIdpRemove_WithYes(t *testing.T) {
	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: spiceboxv1alpha1.ClusterIdentityProviderName,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "google",
			ClientID: "my-client.apps.googleusercontent.com",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: externalurl.Namespace,
				Name:      "idp-google",
				Key:       "client_secret",
			},
			AllowAnyEmail: true,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idp-google",
			Namespace: externalurl.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"client_secret": []byte("s3cret")},
	}
	c := newIdpFakeClient(t, cidp, secret)

	var out bytes.Buffer
	require.NoError(t, runIdpRemove(context.Background(), strings.NewReader(""), &out, c, true))
	assert.Contains(t, out.String(), "removed")

	// CR deleted.
	var getCidp spiceboxv1alpha1.ClusterIdentityProvider
	err := c.Get(context.Background(), types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &getCidp)
	assert.True(t, err != nil, "CR should be deleted")

	// Secret deleted.
	var getSecret corev1.Secret
	err = c.Get(context.Background(), types.NamespacedName{Name: "idp-google", Namespace: externalurl.Namespace}, &getSecret)
	assert.True(t, err != nil, "Secret should be deleted")
}

// TestIdpRemove_Absent verifies remove with no CR exits cleanly.
func TestIdpRemove_Absent(t *testing.T) {
	c := newIdpFakeClient(t)
	var out bytes.Buffer
	require.NoError(t, runIdpRemove(context.Background(), strings.NewReader(""), &out, c, true))
	assert.Contains(t, out.String(), "nothing to remove")
}

// TestIdpRemove_ConfirmN verifies that declining the confirmation aborts.
func TestIdpRemove_ConfirmN(t *testing.T) {
	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: spiceboxv1alpha1.ClusterIdentityProviderName,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:          "google",
			ClientID:      "my-client.apps.googleusercontent.com",
			AllowAnyEmail: true,
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: externalurl.Namespace,
				Name:      "idp-google",
				Key:       "client_secret",
			},
		},
	}
	c := newIdpFakeClient(t, cidp)

	var out bytes.Buffer
	require.NoError(t, runIdpRemove(context.Background(), strings.NewReader("n\n"), &out, c, false))
	assert.Contains(t, out.String(), "Aborted")

	// CR must still exist.
	var getCidp spiceboxv1alpha1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &getCidp))
}

// TestIdpSetup_ReSetup_KeepsExistingSecret: re-running oidc setup with all
// blanks keeps the stored secret (skips ApplySecret) and re-applies the spec.
func TestIdpSetup_ReSetup_KeepsExistingSecret(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)

	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "oidc",
			Issuer:   "https://login.example.com",
			ClientID: "client-id",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: externalurl.Namespace, Name: "idp-oidc", Key: "client_secret",
			},
			AllowedEmailDomains: []string{"example.com"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "idp-oidc", Namespace: externalurl.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"client_secret": []byte("original-secret")},
	}
	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"), cidp, secret)
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(20 * time.Millisecond)
			markIdpValid(t, c)
		}
	}()

	// All blanks: keep issuer, client ID, secret and domains; keep the
	// restricted access policy the existing CR already expresses.
	require.NoError(t, RunIdpSetup(context.Background(),
		strings.NewReader(idpScript("", "", "", idpAccessDomains, "")), &bytes.Buffer{}, &apcmd.Globals{}, c, "oidc"))

	var gotSecret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "idp-oidc", Namespace: externalurl.Namespace}, &gotSecret))
	assert.Equal(t, []byte("original-secret"), gotSecret.Data["client_secret"], "kept secret must be unchanged")

	var gotCR spiceboxv1alpha1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &gotCR))
	assert.Equal(t, "https://login.example.com", gotCR.Spec.Issuer)
	assert.Equal(t, "client-id", gotCR.Spec.ClientID)
	assert.Equal(t, []string{"example.com"}, gotCR.Spec.AllowedEmailDomains, "kept domains must be unchanged")
}

// TestIdpSetup_SecretProbeError_Returns verifies that a non-NotFound error
// probing the existing client-secret Secret is surfaced (not swallowed as
// "secret absent").
func TestIdpSetup_SecretProbeError_Returns(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)

	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "oidc",
			Issuer:   "https://login.example.com",
			ClientID: "client-id",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: externalurl.Namespace, Name: "idp-oidc", Key: "client_secret",
			},
			AllowedEmailDomains: []string{"example.com"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(aptest.Scheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.ClusterIdentityProvider{}).
		WithObjects(webdURLConfigMapForIdp("https://ap.example.com"), cidp).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == "idp-oidc" {
					return apierrors.NewServiceUnavailable("boom")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	err := RunIdpSetup(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &apcmd.Globals{}, c, "oidc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe existing client-secret Secret")
}

// TestIdpSetup_ReSetup_KindMismatchStartsFresh: an existing google CR must not
// pre-fill an oidc re-run (different kind) — oidc must require its own inputs.
func TestIdpSetup_ReSetup_KindMismatchStartsFresh(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)
	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
		Spec:       spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "google", ClientID: "g.apps.googleusercontent.com"},
	}
	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"), cidp)
	// Blank issuer with no oidc default → must refuse, naming the question.
	err := RunIdpSetup(context.Background(), strings.NewReader("\n"), &bytes.Buffer{}, &apcmd.Globals{}, c, "oidc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Issuer URL")
}

// TestIdpSetup_WizardRefusalCarriesNoInternalFraming pins what a user reads
// when the wizard refuses, at the command that produced the framing rather
// than inside the kind package that wrote the sentence.
//
// The sequencer wraps every screen error as `tui: apply screen "<id>": …`,
// which is the right shape for a log and the wrong one for a terminal: `tui:`
// is this project's plumbing and a screen ID names a step in the sequencer,
// neither of which anyone running `oap idp setup` can act on. Asserted here
// because this is the only place the stripping is DONE — a kind's own tests
// call tui.UserFacing themselves, so they would keep passing with the call
// site's stripping removed.
func TestIdpSetup_WizardRefusalCarriesNoInternalFraming(t *testing.T) {
	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))

	// No input at all: the first screen refuses.
	err := RunIdpSetup(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &apcmd.Globals{}, c, "oidc")
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "Issuer URL", "the refusal must name the question in the user's words")
	assert.NotContains(t, msg, "tui:", "the sequencer's own framing reached the user")
	assert.NotContains(t, msg, "screen \"", "a screen ID reached the user")
	assert.NotContains(t, msg, "apply screen")
	assert.NotContains(t, msg, "prepare screen")
}

// TestIdpSetup_OverWideCallbackIsPrintedOutsideTheForm: a redirect URI a note
// cannot render whole is put on the stream instead, because huh wraps an
// over-long note line into two halves with no sign they belong together — and
// this address exists to be pasted into a provider's console.
func TestIdpSetup_OverWideCallbackIsPrintedOutsideTheForm(t *testing.T) {
	const longHost = "https://agents.platform.internal.demo-corporation.example"
	c := newIdpFakeClient(t, webdURLConfigMapForIdp(longHost))

	var out bytes.Buffer
	// The run refuses at the first question; the address is printed before it.
	err := RunIdpSetup(context.Background(), strings.NewReader(""), &out, &apcmd.Globals{}, c, "oidc")
	require.Error(t, err)
	assert.Contains(t, out.String(), longHost+"/oidc/callback/idp",
		"an address too wide for a note must be printed outside the form")
}

// TestIdpSetup_SummaryRecordsTheDecisionsWithoutTheSecret: the summary is what
// stays in scrollback once the form is gone, so it must carry what was decided
// — and must not carry what was typed to authenticate.
func TestIdpSetup_SummaryRecordsTheDecisionsWithoutTheSecret(t *testing.T) {
	withIdpPollSeams(t, 10*time.Millisecond, 2*time.Second)

	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(20 * time.Millisecond)
			markIdpValid(t, c)
		}
	}()

	// The domain fixture is deliberately NOT a substring of the issuer, and not
	// of the redirect URI either. Both of those carry "example.com", so a
	// domains assertion using it would be satisfied by the issuer line beside
	// it — scoping to the summary block closes the guidance-note leak and
	// leaves that one.
	const domains = "demo-corp.test"
	stdin := strings.NewReader(idpScript("https://login.example.com", "client-id", "s3cr3t-value", idpAccessDomains, domains))
	var out bytes.Buffer
	require.NoError(t, RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "oidc"))

	// Scoped to the summary block, not to the whole stream. The stream also
	// carries the guidance note, whose redirect URI is
	// https://ap.example.com/oidc/callback/idp — so an unscoped
	// Contains("example.com") is satisfied with every summary line deleted,
	// and would report the summary as present when it is not.
	summary := summaryBlock(t, out.String())
	assert.Contains(t, summary, "https://login.example.com", "the summary must record the issuer")
	assert.Contains(t, summary, domains, "the summary must record who may sign in")

	// The whole stream for the credential, which must appear nowhere in it.
	assert.NotContains(t, out.String(), "s3cr3t-value", "the client secret must never be printed")
}

// summaryBlock returns just the post-run summary lines of a rendered stream.
//
// Keyed off the mark tui.RenderSummary prefixes every line with, which is the
// only thing distinguishing that block from the form output above it. It
// requires a non-empty result, so a change to the mark fails here loudly rather
// than silently reducing every assertion over the block to a tautology.
func summaryBlock(t *testing.T, printed string) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(printed, "\n") {
		if strings.Contains(l, "✓") {
			lines = append(lines, l)
		}
	}
	got := strings.Join(lines, "\n")
	require.NotEmpty(t, got, "no summary was rendered, so there is nothing to assert about it")
	return got
}

// TestIdpSetup_ARefusedWizardWritesNothing is the property the per-kind tests
// cannot assert: a WizardOutput is the zero value on every error path, so
// "nothing was written" is true there by construction. It is only meaningful
// where the writing happens.
func TestIdpSetup_ARefusedWizardWritesNothing(t *testing.T) {
	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))

	// No input at all: the wizard refuses at its first question.
	err := RunIdpSetup(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &apcmd.Globals{}, c, "oidc")
	require.Error(t, err)

	var secret corev1.Secret
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{
		Name: "idp-oidc", Namespace: externalurl.Namespace,
	}, &secret)), "a refused run must not create the client-secret Secret")

	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{
		Name: spiceboxv1alpha1.ClusterIdentityProviderName,
	}, &cidp)), "a refused run must not create the ClusterIdentityProvider")
}

// TestIdpSetup_PollTimeout verifies that a timeout prints guidance.
func TestIdpSetup_PollTimeout(t *testing.T) {
	// Very short poll timeout so the test finishes fast.
	withIdpPollSeams(t, 5*time.Millisecond, 50*time.Millisecond)

	c := newIdpFakeClient(t, webdURLConfigMapForIdp("https://ap.example.com"))
	// No goroutine sets Valid — poll will time out.

	stdin := strings.NewReader(idpScript("my-client.apps.googleusercontent.com", "my-secret", idpAccessDomains, "example.com"))
	var out bytes.Buffer
	err := RunIdpSetup(context.Background(), stdin, &out, &apcmd.Globals{}, c, "google")
	require.Error(t, err)
	assert.Contains(t, out.String(), "oap idp status")
}
