// pkg/controllers/agentsession/credential_grants_projectable_test.go
//
// The projected-credential grant path is a JOIN matched by hash, never by
// reference: the operator writes an externaltoken grant under
// CredID(writer's source), and the runner later checks use_token under
// CredID(resolver's source). CredID digests the credential TYPE along with the
// coordinates, so the two sides agree only while they spell Type the same way.
//
// The writer used to hardcode "static" while the resolver used cred.Type. With
// one Projectable kind registered those are the same string, so nothing showed;
// with a second, every sandbox tool call would be fail-closed denied with
// nothing naming the cause. This file registers that second kind — which is
// what makes the divergence observable at all.
package agentsession

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// demoProjectableKind is a SECOND Projectable kind: stored (not minted), with
// its own single-key backing Secret, exactly like static but under a different
// type name. It is the discriminator — with only "static" registered, a
// hardcoded Type literal and cred.Type are indistinguishable.
type demoProjectableKind struct{}

var _ credkind.Kind = demoProjectableKind{}

func (demoProjectableKind) Type() string        { return "demo-projectable" }
func (demoProjectableKind) Minted() bool        { return false }
func (demoProjectableKind) NeedsRefresh() bool  { return false }
func (demoProjectableKind) Projectable() bool   { return true }
func (demoProjectableKind) DisplayName() string { return "Demo projectable test kind" }

func (demoProjectableKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeUserIdentity, credkind.ScopeSessionUserIdentity}
}

func (demoProjectableKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

// HasBlock / SecretRefPath: this kind owns no AgentCredential union block —
// exactly like a real kind whose block this test binary cannot add to the CRD.
//
// Its store is named by CONVENTION from the credential name instead, and
// crucially NOT through cred.Static. That is what makes the fixture
// discriminating: a consumer that reaches for cred.Static gets nil for this
// credential, rather than quietly reading the same value by another route. An
// earlier version of this fake did borrow the static block, and the negative
// control for the projection writer passed with the bug reinstated.
func (demoProjectableKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }
func (demoProjectableKind) SecretRefPath() []string                        { return nil }

// demoProjectableSecretName is the conventional store for a demo credential —
// the analogue of a kind whose Secret name is derived rather than referenced.
func demoProjectableSecretName(credName string) string { return credName + "-demo-master" }

// demoProjectableSecretKey is the fixed key inside that store, the same way a
// GitHub App's Secret has fixed keys rather than one configurable one.
const demoProjectableSecretKey = "api-key"

func (demoProjectableKind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: demoProjectableSecretName(c.Name)}
}

func (demoProjectableKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "demo-projectable"}, nil
}

func (demoProjectableKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoProjectableKind: Resolve not exercised by this test")
}

// ReadStoredValue is a REAL read, not a stub: the projection test below drives
// materializePassthroughCredentials all the way through to a written Secret,
// and a stub that errored would make that test pass for the wrong reason.
func (demoProjectableKind) ReadStoredValue(ctx context.Context, c client.Reader, ns string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	name := demoProjectableSecretName(cred.Name)
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
		return sensitive.SensitiveValue{}, err
	}
	val, ok := sec.Data[demoProjectableSecretKey]
	if !ok || len(val) == 0 {
		return sensitive.SensitiveValue{}, fmt.Errorf("demoProjectableKind: %s/%s has no %q", ns, name, demoProjectableSecretKey)
	}
	return sensitive.NewSensitiveValue(val), nil
}

func (demoProjectableKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string   { return nil }
func (demoProjectableKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// projectableCredentials is the two-credential fixture both tests share: one
// real type=static credential (which DOES carry a static block) and one
// demo-projectable credential (which carries no block at all). The asymmetry
// is the point — see demoProjectableKind.HasBlock.
func projectableCredentials() []spiceboxv1alpha1.AgentCredential {
	return []spiceboxv1alpha1.AgentCredential{
		{
			Name: "static-cred", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "static-master", Key: "token"},
			},
		},
		{Name: "demo-cred", Type: "demo-projectable"},
	}
}

// passthroughSUID wraps those credentials in a SessionUserIdentity.
func passthroughSUID(creds ...spiceboxv1alpha1.AgentCredential) *spiceboxv1alpha1.SessionUserIdentity {
	return &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-session"},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{Credentials: creds},
	}
}

// TestProjectedGrantSources_CredIDMatchesTheResolver is the spanning
// assertion: for every projected credential, the CredID the grant WRITER
// derives must equal the CredID the RESOLVER derives at use time.
//
// The resolver side is computed here independently, through the same public
// entry point the runner's descriptor path uses (credresolve.SourceFor), so
// the two are compared rather than assumed. A writer that spells Type any
// other way — the "static" literal it used to carry — produces a different
// hash for the demo-projectable row and fails here.
func TestProjectedGrantSources_CredIDMatchesTheResolver(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoProjectableKind{})

	ctx := context.Background()
	suid := passthroughSUID(projectableCredentials()...)
	rid := credresolve.RuntimeIdentityFromSessionUserIdentity(suid)

	got, err := projectedGrantSources(ctx, suid, rid)
	require.NoError(t, err)
	require.Len(t, got, 2, "both Projectable credentials must produce a grant source")

	byName := map[string]spiceboxv1alpha1.CredentialSource{}
	for _, src := range got {
		byName[src.Key] = src // SourceFor keys a projected source by credential name
	}

	for i := range suid.Spec.Credentials {
		cred := &suid.Spec.Credentials[i]
		t.Run(cred.Type, func(t *testing.T) {
			writer, ok := byName[cred.Name]
			require.Truef(t, ok, "no grant source was produced for %q", cred.Name)

			reader, rErr := credresolve.SourceFor(cred, rid)
			require.NoError(t, rErr)

			assert.Equal(t, externaltoken.CredID(reader), externaltoken.CredID(writer),
				"the grant is written under the writer's CredID and checked under the resolver's; "+
					"a mismatch fail-closed denies every sandbox tool call with nothing naming the cause")
			assert.Equal(t, cred.Type, writer.Type,
				"the source must carry the credential's OWN type, never a hardcoded one")
		})
	}
}

// TestProjectedGrantSources_SecondProjectableKindIsIncluded is the
// non-vacuity control for the test above: if the collector silently dropped
// the non-static kind, every remaining row would agree trivially and the
// comparison would prove nothing.
func TestProjectedGrantSources_SecondProjectableKindIsIncluded(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoProjectableKind{})

	ctx := context.Background()
	// oauth is registered and NOT Projectable — the negative half, proving the
	// collector selects on the predicate rather than taking everything.
	creds := append(projectableCredentials(), spiceboxv1alpha1.AgentCredential{
		Name: "oauth-cred", Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "oauth-master"},
		},
	})
	suid := passthroughSUID(creds...)

	got, err := projectedGrantSources(ctx, suid, credresolve.RuntimeIdentityFromSessionUserIdentity(suid))
	require.NoError(t, err)

	var types []string
	for _, src := range got {
		types = append(types, src.Type)
	}
	assert.ElementsMatch(t, []string{"static", "demo-projectable"}, types,
		"every Projectable kind must be granted, and only those")
}

// TestMaterializePassthroughCredentials_ProjectsANonStaticProjectableKind is
// the writer half of the same defect: the projection loop read
// cred.Static.SecretRef directly, behind a collector that selects on
// Projectable(). The first non-static Projectable kind therefore failed every
// session with "has type=static but static block is nil" — about a credential
// that is not static at all — or, without that guard, nil-panicked the
// reconcile.
//
// The fixture deliberately gives the demo kind a DIFFERENT master Secret and
// key from the static one, so a body that still reached for the static block
// would project the wrong value rather than merely erroring.
func TestMaterializePassthroughCredentials_ProjectsANonStaticProjectableKind(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoProjectableKind{})

	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo-ns", UID: "uid-1"},
	}
	masters := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "static-master", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
			Data:       map[string][]byte{"token": []byte("static-value")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: demoProjectableSecretName("demo-cred"), Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			},
			Data: map[string][]byte{demoProjectableSecretKey: []byte("demo-value")},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(masters...).Build()
	r := &Reconciler{Client: c}

	_, err := r.materializePassthroughCredentials(ctx, sess, projectableCredentials())
	require.NoError(t, err,
		"a Projectable kind that is not type=static must project like any other")

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: "demo-ns", Name: spiceboxv1alpha1.PassthroughCredentialSecretName("s1"),
	}, &got))

	value := func(key string) string {
		if v := got.StringData[key]; v != "" {
			return v
		}
		return string(got.Data[key])
	}
	assert.Equal(t, "static-value", value("static-cred"))
	assert.Equal(t, "demo-value", value("demo-cred"),
		"the non-static Projectable credential's value must be projected under its own name")
}
