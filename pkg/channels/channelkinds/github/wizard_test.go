package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// demoAnswers is every key Result requires, fully answered — the one fixture
// every Result test builds from, so no two of them can drift apart.
func demoAnswers() map[string]string {
	return map[string]string{
		keyAgentClass:              "demo-reviewbot",
		keyOwnerType:               ownerTypeOrg,
		keyOrg:                     "demo-org",
		wizardkeys.KeyChannelName:  "demo-reviewbot-gh",
		wizardkeys.KeyAuthzSubject: "service:demo-reviewbot-github",
		keyAppID:                   "12345",
		keySlug:                    "demo-reviewbot",
		keyPrivateKeyPEM:           "-----BEGIN RSA PRIVATE KEY-----\nZmFrZWZha2VmYWtl\n-----END RSA PRIVATE KEY-----\n",
		keyWebhookSecret:           "whsec_faketestwebhooksecretvalue",
		keyInstallationID:          "67890",
	}
}

func newFakeK8s(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func newAgentClass(name, namespace string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// ---------------------------------------------------------------------------
// Result — the brief's required tests (Secret.Data, not StringData: see the
// task brief's correction note)
// ---------------------------------------------------------------------------

func TestResult_ProducesAChannelAndSecretWithNoVolatileFields(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default"}
	w := &wizard{}
	out1, err := w.Result(in, demoAnswers())
	require.NoError(t, err)

	out2, err := w.Result(in, demoAnswers())
	require.NoError(t, err)
	assert.Equal(t, out1, out2,
		"Result must be a pure function of its inputs: a volatile field would break SSA idempotency")
}

func TestResult_SecretsNeverLandInSpec(t *testing.T) {
	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, demoAnswers())
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)

	raw, err := json.Marshal(out.ChannelManifest.Spec)
	require.NoError(t, err)
	for _, forbidden := range []string{"BEGIN RSA PRIVATE KEY", "whsec_", "67890"} {
		assert.NotContains(t, string(raw), forbidden,
			"App credentials belong in the Secret, never in an SSA-applied spec field")
	}
}

func TestResult_SecretCarriesAllFourRequiredKeys(t *testing.T) {
	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, demoAnswers())
	require.NoError(t, err)
	require.NotNil(t, out.SecretManifest)
	for _, k := range (Kind{}).RequiredSecretKeys(nil) {
		assert.Contains(t, out.SecretManifest.Data, k,
			"the Channel controller fails Valid=False/SecretKeyMissing without every key")
	}
}

func TestResult_SecretHasNoOwnerReferenceToTheChannel(t *testing.T) {
	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "default"}, demoAnswers())
	require.NoError(t, err)
	require.NotNil(t, out.SecretManifest)
	assert.Empty(t, out.SecretManifest.OwnerReferences,
		"deleting the Channel must not pull a credential out from under a running review")
}

// ---------------------------------------------------------------------------
// Result — additional coverage: the concrete shape of what gets built
//
// The fail-closed-on-a-missing-answer sweep lives in wizard_data_test.go
// (TestGitHubWizard_ResultFailsClosedOnAMissingAnswer), which drives the same
// loop over the same fixture.
// ---------------------------------------------------------------------------

func TestResult_ChannelShape(t *testing.T) {
	out, err := (&wizard{}).Result(channelkinds.WizardInput{Namespace: "demo-ns"}, demoAnswers())
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	ch := out.ChannelManifest

	assert.Equal(t, "demo-reviewbot-gh", ch.Name)
	assert.Equal(t, "demo-ns", ch.Namespace)
	assert.Equal(t, "github", ch.Spec.Kind)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleInput, ch.Spec.Role)
	assert.Equal(t, "demo-reviewbot", ch.Spec.AgentClass)
	assert.Equal(t, "service:demo-reviewbot-github", ch.Spec.AuthzSubject,
		"without this the bound AgentClass goes Valid=False and the inbound pipeline has no service subject to attribute to")
	assert.Equal(t, "auto", ch.Spec.SessionScope)
	assert.Equal(t, "demo-reviewbot-gh-creds", ch.Spec.CredentialsRef.SecretName)
	require.NotNil(t, ch.Spec.GitHub)
	assert.Equal(t, "demo-reviewbot", ch.Spec.GitHub.AppSlug)

	require.NotNil(t, out.SecretManifest)
	sec := out.SecretManifest
	assert.Equal(t, "demo-reviewbot-gh-creds", sec.Name)
	assert.Equal(t, "demo-ns", sec.Namespace)
	assert.Equal(t, corev1.SecretTypeOpaque, sec.Type)
	assert.Equal(t, []byte("12345"), sec.Data["app-id"])
	assert.Equal(t, []byte("67890"), sec.Data["installation-id"])
	assert.Contains(t, string(sec.Data["private-key"]), "BEGIN RSA PRIVATE KEY")
	assert.Equal(t, []byte("whsec_faketestwebhooksecretvalue"), sec.Data["webhook-secret"])
}

// ---------------------------------------------------------------------------
// validation helpers
// ---------------------------------------------------------------------------

// TestValidateOwnerLogin covers ONE rule for both owner kinds: GitHub draws
// organization and personal-account logins from the same namespace under the
// same format rule, which is exactly why a login cannot say which of the two it
// is and why the owner type has to be asked separately.
func TestValidateOwnerLogin(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "ok: simple", in: "demo-org", wantErr: false},
		{name: "ok: a personal account's login is the same shape", in: "demo-owner", wantErr: false},
		{name: "ok: single char", in: "a", wantErr: false},
		{name: "ok: digits and hyphens", in: "demo-org-2", wantErr: false},
		{name: "ok: max length (39 chars)", in: strings.Repeat("a", 39), wantErr: false},
		{name: "rejects: empty", in: "", wantErr: true},
		{name: "rejects: leading hyphen", in: "-demo", wantErr: true},
		{name: "rejects: trailing hyphen", in: "demo-", wantErr: true},
		{name: "rejects: double hyphen", in: "demo--org", wantErr: true},
		{name: "rejects: underscore", in: "demo_org", wantErr: true},
		{name: "rejects: too long (40 chars)", in: strings.Repeat("a", 40), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOwnerLogin(tc.in)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidateOwnerType_AcceptsOnlyTheTwoDeclaredValues.
//
// The rejections matter more than the acceptances here. ownerIsUser reads
// anything that is not "user" as an organization, so every value this refuses
// is one that would otherwise be silently treated as an org — sending a
// personal account to an address GitHub answers 404 for, which is the whole
// bug the owner type was added to remove.
func TestValidateOwnerType_AcceptsOnlyTheTwoDeclaredValues(t *testing.T) {
	for _, ok := range []string{"organization", "user", "  user  "} {
		t.Run("accepts "+ok, func(t *testing.T) { assert.NoError(t, validateOwnerType(ok)) })
	}
	for _, bad := range []string{"", "org", "usr", "User", "personal", "ORGANIZATION"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			err := validateOwnerType(bad)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "organization", "the refusal must name what IS accepted")
			assert.Contains(t, err.Error(), "user")
		})
	}
}

// TestAppCreationAndInstallationURLsAreTotalOverBothOwnerKinds is the
// unit-level pin under the two behavioural tests in wizard_data_test.go: four
// addresses, each spelled out in full, so a change to any path prefix reddens
// here rather than only in whatever end-to-end test happened to cover it.
func TestAppCreationAndInstallationURLsAreTotalOverBothOwnerKinds(t *testing.T) {
	assert.Equal(t, "https://github.com/organizations/demo-org/settings/apps/new",
		appCreateURL(ownerTypeOrg, "demo-org"))
	assert.Equal(t, "https://github.com/settings/apps/new",
		appCreateURL(ownerTypeUser, "demo-owner"),
		"a personal account has no /organizations/<login>/ tree at all")

	assert.Equal(t, "https://github.com/organizations/demo-org/settings/installations",
		installationsURL(ownerTypeOrg, "demo-org"))
	assert.Equal(t, "https://github.com/settings/installations",
		installationsURL(ownerTypeUser, "demo-owner"))
}

// TestValidateExternalBaseURL covers both questions this validator answers,
// and they are different questions: is the answer a URL at all, and can GitHub
// deliver a webhook to it.
//
// `http://127.0.0.1:8080` USED TO BE A PASSING CASE HERE, named "ok: http with
// port". It is not ok, and the row was the bug written down as intent: the
// value is a perfectly well-formed URL, so a shape check has nothing to say
// about it, and GitHub refuses the entire App with "Hook url is not supported
// because it isn't reachable over the public Internet (127.0.0.1)" — after the
// operator has been to the browser and submitted the form.
func TestValidateExternalBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantErrContains is empty for a value that must be accepted, and
		// otherwise the substring the refusal has to carry. Asserting the
		// substring rather than merely `Error` is what keeps a reachability
		// refusal from being satisfied by a shape refusal that happens to fire
		// for an unrelated reason.
		wantErrContains string
	}{
		{name: "ok: a public https address", in: "https://ap.example.com"},
		{name: "ok: a public host with a port", in: "https://ap.example.com:8443"},
		{name: "ok: a public IPv4 literal", in: "http://198.51.100.7:8080"},
		{name: "ok: an unresolvable name is left alone (no DNS here)", in: "https://not-a-real-host.invalid"},

		{name: "rejects: empty", in: "", wantErrContains: "required"},
		{name: "rejects: no scheme", in: "ap.example.com", wantErrContains: "absolute URL"},
		{name: "rejects: relative path", in: "/webhooks/github", wantErrContains: "absolute URL"},
		{name: "rejects: scheme with no host", in: "https://", wantErrContains: "absolute URL"},

		// The reachability half. Each names WHY, because the operator's value
		// is not malformed and telling them it is would send them looking for
		// a typo that is not there.
		{name: "rejects: loopback v4 names the reachability constraint", in: "http://127.0.0.1:8080",
			wantErrContains: "loopback address"},
		{name: "rejects: any of 127/8, not just the canonical spelling", in: "http://127.9.9.9:8080",
			wantErrContains: "loopback address"},
		{name: "rejects: loopback v6", in: "http://[::1]:8080", wantErrContains: "loopback address"},
		{name: "rejects: localhost, whatever it resolves to", in: "http://localhost:8080",
			wantErrContains: "localhost"},
		{name: "rejects: a reserved .localhost subdomain", in: "http://ap.localhost:8080",
			wantErrContains: "localhost"},
		{name: "rejects: RFC1918 10/8", in: "http://10.1.2.3", wantErrContains: "private address"},
		{name: "rejects: RFC1918 192.168/16", in: "https://192.168.1.10:8443", wantErrContains: "private address"},
		{name: "rejects: RFC1918 172.16/12", in: "http://172.16.0.5", wantErrContains: "private address"},
		{name: "rejects: an IPv6 unique-local address", in: "http://[fd00::1]:8080", wantErrContains: "private address"},
		{name: "rejects: link-local v4", in: "http://169.254.1.1", wantErrContains: "link-local"},
		{name: "rejects: link-local v6", in: "http://[fe80::1]:8080", wantErrContains: "link-local"},
		{name: "rejects: the v4 wildcard", in: "http://0.0.0.0:8080", wantErrContains: "wildcard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExternalBaseURL(tc.in)
			if tc.wantErrContains == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContains)
		})
	}
}

// TestValidateExternalBaseURL_SaysWhyAndWhereToGetAWorkingAddress: the whole
// point of refusing here rather than letting GitHub refuse later is that this
// side can say something useful. A bare "invalid URL" would leave the operator
// hunting a typo in a value that is spelled perfectly.
func TestValidateExternalBaseURL_SaysWhyAndWhereToGetAWorkingAddress(t *testing.T) {
	err := validateExternalBaseURL("http://127.0.0.1:8080")
	require.Error(t, err)
	msg := err.Error()

	assert.Contains(t, msg, "http://127.0.0.1:8080", "the value the operator gave must be quoted back")
	assert.Contains(t, msg, "public Internet", "the constraint must be named — this is GitHub's rule, not ours")
	assert.Contains(t, msg, "tunnel", "and the way out for a local cluster must be named")
	assert.Contains(t, msg, "oap agent install", "by a command that actually opens one")
	assert.Contains(t, msg, spiceboxv1alpha1.WebdExternalURLConfigMap,
		"and the place an install reads the answer back from")
	assert.Contains(t, msg, "NGROK_AUTHTOKEN",
		"and the one thing the operator has to supply for either command to produce a URL")
}

func TestValidateAppID_AndInstallationID_RequireNumbers(t *testing.T) {
	for _, validate := range []func(string) error{validateAppID, validateInstallationID} {
		assert.NoError(t, validate("12345"))
		assert.Error(t, validate(""))
		assert.Error(t, validate("not-a-number"))
		assert.Error(t, validate("123abc"))
	}
}

// ---------------------------------------------------------------------------
// readPrivateKeyPEM — the file-path indirection, and why it exists
// ---------------------------------------------------------------------------

func TestReadPrivateKeyPEM(t *testing.T) {
	dir := t.TempDir()

	pemPath := filepath.Join(dir, "demo-app.pem")
	require.NoError(t, os.WriteFile(pemPath, []byte("-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n"), 0o600))

	notPEMPath := filepath.Join(dir, "not-a-key.txt")
	require.NoError(t, os.WriteFile(notPEMPath, []byte("hello world"), 0o600))

	t.Run("reads a real PEM file", func(t *testing.T) {
		got, err := readPrivateKeyPEM(pemPath)
		require.NoError(t, err)
		assert.Contains(t, got, "BEGIN RSA PRIVATE KEY")
	})
	t.Run("rejects an empty path", func(t *testing.T) {
		_, err := readPrivateKeyPEM("")
		assert.Error(t, err)
	})
	t.Run("rejects a missing file", func(t *testing.T) {
		_, err := readPrivateKeyPEM(filepath.Join(dir, "does-not-exist.pem"))
		assert.Error(t, err)
	})
	t.Run("rejects a file with no PRIVATE KEY block", func(t *testing.T) {
		_, err := readPrivateKeyPEM(notPEMPath)
		assert.ErrorContains(t, err, "PEM private key")
	})
}
