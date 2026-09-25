package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// livePassthrough adds the SessionUserIdentity a userPassthrough session leaves
// behind: the resolved projection of the starter's catalog, narrowed to exactly
// the credentials the run used, with Secret names derived from the live user's
// own canonical subject.
func livePassthrough(t *testing.T, mutate func(*steelthread.FixtureInput)) steelthread.FixtureInput {
	t.Helper()
	liveSubject, err := identity.EmailReference("real.person@acme.example").Subject()
	require.NoError(t, err)
	liveUI := useridentity.NameForSubject(liveSubject)

	return liveFixture(t, func(f *steelthread.FixtureInput) {
		f.Class.Spec.IdentityMode = "userPassthrough"
		f.SessionUserIdentity = &spiceboxv1alpha1.SessionUserIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "acme-prod"},
			Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
				AgentSession: "demo-session",
				UserIdentity: liveUI,
				Subject:      liveSubject.String(),
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: "github-token",
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{
							Name: useridentity.MasterSecretName(liveUI, "github-token"),
							Key:  "token",
						},
					},
				}},
			},
		}
		if mutate != nil {
			mutate(f)
		}
	})
}

// fixtureUserIdentityName is the name the emitted UserIdentity must carry,
// derived here the same way the operator derives it so the test cannot pass
// against a rewrite that made the name up.
func fixtureUserIdentityName(t *testing.T) string {
	t.Helper()
	subj, err := identity.EmailReference("user@example.com").Subject()
	require.NoError(t, err)
	return useridentity.NameForSubject(subj)
}

// docsIn splits an emitted fixture file into its YAML documents.
func docsIn(t *testing.T, f steelthread.FixtureFile) []string {
	t.Helper()
	return strings.Split(string(f.YAML), "\n---\n")
}

// TestRewriteFixture_UserPassthroughEmitsALinkedUserIdentity pins the fixture a
// userPassthrough session needs to replay at all.
//
// Without it the replayed session parks in AwaitingCredentials until its
// credential-link deadline: the class draws its tool credentials from the
// STARTER's own catalog, and the replay's default user has none. The bundle
// then fails on a timeout minutes away from anything that explains it.
func TestRewriteFixture_UserPassthroughEmitsALinkedUserIdentity(t *testing.T) {
	rewritten, err := steelthread.RewriteFixture(livePassthrough(t, nil))
	require.NoError(t, err)

	f := fileNamed(t, rewritten.Files, "00a-useridentity.yaml")
	docs := docsIn(t, f)
	require.Len(t, docs, 3, "a Namespace, one placeholder Secret, and the UserIdentity")

	var ns corev1.Namespace
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), &ns))
	assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, ns.Name,
		"envtest creates no namespaces, and the operator resolves a passthrough Secret from this one")
	assert.Equal(t, "Namespace", ns.Kind,
		"it leads the file: a file's documents are applied in order, so the Secrets below land in it")

	uiName := fixtureUserIdentityName(t)

	var sec corev1.Secret
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), &sec))
	assert.Equal(t, useridentity.MasterSecretName(uiName, "github-token"), sec.Name)
	assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, sec.Namespace)
	assert.NotEmpty(t, sec.StringData["token"], "an empty value replays as an unlinked credential")

	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, yaml.Unmarshal([]byte(docs[2]), &ui))
	assert.Equal(t, uiName, ui.Name,
		"the passthrough gate looks the catalog up by NameForSubject; any other name resolves nothing")
	assert.Empty(t, ui.Namespace, "UserIdentity is cluster-scoped")
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, "github-token", ui.Spec.Credentials[0].Name,
		"the NAME is what the gate compares against, so it rides through untouched")
	assert.Equal(t, "static", ui.Spec.Credentials[0].Type)
	require.NotNil(t, ui.Spec.Credentials[0].Static)
	assert.Equal(t, sec.Name, ui.Spec.Credentials[0].Static.SecretRef.Name,
		"the credential must point at the Secret this same file wrote")
	assert.Equal(t, "token", ui.Spec.Credentials[0].Static.SecretRef.Key,
		"the KEY is part of the credential's shape and is not the rewrite's to change")

	assert.Equal(t, "user@example.com", rewritten.DefaultUser,
		"the bundle must send as the user this catalog is addressed to, or it resolves nothing")
}

// The live person's identity must not ride into a repo. Their canonical subject
// and every Secret name derived from it name a real user; both are re-derived
// from the fixture user instead.
func TestRewriteFixture_UserPassthroughCarriesNoLiveSubject(t *testing.T) {
	in := livePassthrough(t, nil)
	liveSubject := in.SessionUserIdentity.Spec.Subject
	liveUI := in.SessionUserIdentity.Spec.UserIdentity

	rewritten, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)

	for _, f := range rewritten.Files {
		assert.NotContains(t, string(f.YAML), liveSubject,
			"file %s carries the live starter's canonical subject", f.Name)
		assert.NotContains(t, string(f.YAML), liveUI,
			"file %s carries a name derived from the live starter's subject", f.Name)
	}
}

// The negative control, and the one that keeps the file out of every other
// capture: a session that ran as the AGENT leaves no SessionUserIdentity, so
// nothing is emitted and the bundle keeps the harness's own default user.
func TestRewriteFixture_AnAgentIdentitySessionEmitsNoUserIdentity(t *testing.T) {
	rewritten, err := steelthread.RewriteFixture(liveFixture(t, nil))
	require.NoError(t, err)

	for _, f := range rewritten.Files {
		assert.NotEqual(t, "00a-useridentity.yaml", f.Name)
		assert.NotContains(t, string(f.YAML), "kind: UserIdentity")
	}
	assert.Empty(t, rewritten.DefaultUser,
		"an empty DefaultUser leaves the harness default in place, which is what every other capture wants")
}

// A federated credential is minted on demand from the user's IdP identity and
// has no backing Secret to placeholder. It is still CARRIED: the passthrough
// gate synthesizes one per federated target and never reports it missing, so
// dropping it here would change what the replayed session resolves.
func TestRewriteFixture_UserPassthroughCarriesACredentialWithNoSecret(t *testing.T) {
	rewritten, err := steelthread.RewriteFixture(livePassthrough(t, func(f *steelthread.FixtureInput) {
		f.SessionUserIdentity.Spec.Credentials = append(f.SessionUserIdentity.Spec.Credentials,
			spiceboxv1alpha1.AgentCredential{
				Name: "acme-mcp",
				Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{
					Resource:          "https://acme.example/mcp",
					ResourceServerURL: "https://acme.example",
					IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "idp-identity-live"},
				},
			})
	}))
	require.NoError(t, err)

	docs := docsIn(t, fileNamed(t, rewritten.Files, "00a-useridentity.yaml"))
	require.Len(t, docs, 3, "the federated credential adds no Secret document")

	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, yaml.Unmarshal([]byte(docs[2]), &ui))
	require.Len(t, ui.Spec.Credentials, 2, "but the credential itself is still carried")
	assert.Equal(t, "acme-mcp", ui.Spec.Credentials[1].Name)
}

// Byte-identical on a re-run, like every other rewrite: two captures of one
// session must diff to nothing, or nobody reads the diff.
func TestRewriteFixture_UserPassthroughIsDeterministic(t *testing.T) {
	first, err := steelthread.RewriteFixture(livePassthrough(t, nil))
	require.NoError(t, err)
	second, err := steelthread.RewriteFixture(livePassthrough(t, nil))
	require.NoError(t, err)

	assert.Equal(t,
		string(fileNamed(t, first.Files, "00a-useridentity.yaml").YAML),
		string(fileNamed(t, second.Files, "00a-useridentity.yaml").YAML))
}
