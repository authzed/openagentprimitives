// pkg/controllers/internal/identityrefresh/refreshable_kind_test.go
//
// This package gates on credkind.Kind.NeedsRefresh — a registry predicate any
// kind may answer true — and then, in three places, read cred.OAuth as though
// "refreshable" and "type=oauth" were the same thing. One of those three was
// SILENT: classify returned "nothing to do" for a NeedsRefresh credential with
// no oauth block, so a second refreshable kind would never be refreshed and
// would expire with no log, no condition and no event.
//
// Every test here registers a SECOND refreshable kind whose token lives
// somewhere other than the oauth block. That is the only way the difference is
// observable at all: while oauth is the sole NeedsRefresh kind, the predicate
// and the field agree on every input.
package identityrefresh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	// Registers the shipped kinds in THIS test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// demoRefreshableKind is a SECOND NeedsRefresh kind. It carries no oauth block
// — its store is named by convention from the credential name — so any site
// that reaches for cred.OAuth sees nil for it.
type demoRefreshableKind struct{}

var _ credkind.Kind = demoRefreshableKind{}

func demoRefreshableSecretName(credName string) string { return credName + "-refreshable-store" }

func (demoRefreshableKind) Type() string        { return "demo-refreshable" }
func (demoRefreshableKind) Minted() bool        { return false }
func (demoRefreshableKind) NeedsRefresh() bool  { return true }
func (demoRefreshableKind) Projectable() bool   { return false }
func (demoRefreshableKind) DisplayName() string { return "Demo refreshable test kind" }

func (demoRefreshableKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity, credkind.ScopeUserIdentity}
}

func (demoRefreshableKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }
func (demoRefreshableKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool      { return false }
func (demoRefreshableKind) SecretRefPath() []string                             { return nil }

func (demoRefreshableKind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: demoRefreshableSecretName(c.Name)}
}

func (demoRefreshableKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "demo-refreshable"}, nil
}

func (demoRefreshableKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoRefreshableKind: Resolve not exercised")
}

func (demoRefreshableKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("demoRefreshableKind: ReadStoredValue not exercised")
}

func (demoRefreshableKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string   { return nil }
func (demoRefreshableKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// ---------------------------------------------------------------------------
// ReferencesSecret — the watch mapping
// ---------------------------------------------------------------------------

// TestReferencesSecret_TracksTheRefreshablePredicate pins both directions: a
// second refreshable kind's Secret MUST wake the identity, and the
// non-refreshable kinds' Secrets must still not.
//
// The negative rows are what keep the fix honest — routing through the
// registry must not have broadened the watch to every credential's Secret,
// which is what the original comment (correctly) warned against.
func TestReferencesSecret_TracksTheRefreshablePredicate(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoRefreshableKind{})

	creds := []spiceboxv1alpha1.AgentCredential{
		{
			Name: "oauth-cred", Type: "oauth",
			OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretRef{Name: "oauth-store"},
			},
		},
		{Name: "demo-cred", Type: "demo-refreshable"},
		{
			Name: "static-cred", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "static-store", Key: "token"},
			},
		},
		{Name: "bogus-cred", Type: "not-registered"},
	}

	cases := []struct {
		name   string
		secret string
		want   bool
	}{
		{name: "oauth credential's Secret: wakes the identity", secret: "oauth-store", want: true},
		{
			name:   "second refreshable kind's Secret: wakes the identity",
			secret: demoRefreshableSecretName("demo-cred"),
			want:   true,
		},
		{name: "static credential's Secret: does NOT wake the identity", secret: "static-store"},
		{name: "a Secret no credential names: does NOT wake the identity", secret: "unrelated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ReferencesSecret(creds, tc.secret))
		})
	}
}

// ---------------------------------------------------------------------------
// classify — the reconcile path
// ---------------------------------------------------------------------------

// fakeIdentity is the minimal Identity the policy needs, backed by a real
// AgentIdentity so nothing about the CR is simulated.
type fakeIdentity struct {
	ai *spiceboxv1alpha1.AgentIdentity
}

func (f fakeIdentity) Object() client.Object             { return f.ai }
func (fakeIdentity) Kind() string                        { return "AgentIdentity" }
func (f fakeIdentity) SecretNamespace() string           { return f.ai.Namespace }
func (fakeIdentity) ConditionType() string               { return "Refresh" }
func (fakeIdentity) ThresholdOverride() *metav1.Duration { return nil }

func (f fakeIdentity) Credentials() []spiceboxv1alpha1.AgentCredential {
	return f.ai.Spec.Credentials
}
func (f fakeIdentity) StatusConditions() *[]metav1.Condition { return &f.ai.Status.Conditions }
func (f fakeIdentity) SetLastRefreshAt(metav1.Time)          {}
func (f fakeIdentity) StatusSnapshot() any                   { return f.ai.Status }
func (f fakeIdentity) DeepCopyIdentity() Identity            { return fakeIdentity{ai: f.ai.DeepCopy()} }

func newTestCore(t *testing.T, objs ...client.Object) (*Core, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	sr := adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	return New(c, sr, time.Hour), c
}

// TestClassify_DoesNotSilentlySkipASecondRefreshableKind is the silent-skip
// defect itself.
//
// classify gated on `!k.NeedsRefresh() || cred.OAuth == nil`. The second half
// classified this credential as credSkipNotOAuth — indistinguishable from "not
// refreshable at all" — with no log, no condition and no event, so the token
// would expire and every upstream call would start failing with nothing
// anywhere pointing at the cause.
//
// The assertion is that classify REACHES the credential's real store: given a
// Secret that is due for refresh, the plan must be credAttempt.
func TestClassify_DoesNotSilentlySkipASecondRefreshableKind(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoRefreshableKind{})

	cred := spiceboxv1alpha1.AgentCredential{Name: "demo-cred", Type: "demo-refreshable"}
	store := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: demoRefreshableSecretName("demo-cred")},
		Data: map[string][]byte{
			"refresh_token": []byte("rt"),
			// Inside the one-hour threshold, so this is due now.
			"expires_at": []byte(time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-identity"},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{cred}},
	}
	co, _ := newTestCore(t, store, ai)

	plan, err := co.classify(context.Background(), fakeIdentity{ai: ai}, cred, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, credAttempt, plan.class,
		"a second refreshable kind must be classified from ITS OWN store; "+
			"skipping it is invisible — no log, no condition, and the token simply expires")
}

// TestClassify_SecondRefreshableKindSchedulesWhenNotYetDue is the other half
// of the same reach: a plan that is not yet due must SCHEDULE, not skip. A
// body that still read cred.OAuth would return the same skip for both rows,
// so covering only the due case would leave that indistinguishable.
func TestClassify_SecondRefreshableKindSchedulesWhenNotYetDue(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoRefreshableKind{})

	cred := spiceboxv1alpha1.AgentCredential{Name: "demo-cred", Type: "demo-refreshable"}
	store := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: demoRefreshableSecretName("demo-cred")},
		Data: map[string][]byte{
			"refresh_token": []byte("rt"),
			"expires_at":    []byte(time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-identity"},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{cred}},
	}
	co, _ := newTestCore(t, store, ai)

	plan, err := co.classify(context.Background(), fakeIdentity{ai: ai}, cred, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, credSchedule, plan.class,
		"a not-yet-due refreshable credential must schedule the next check, not be skipped")
	assert.False(t, plan.expiresAt.IsZero(), "the schedule needs the expiry it read from the store")
}

// TestClassify_NonRefreshableKindIsStillSkipped is the non-vacuity control:
// the gate must still exclude the kinds it always did, or the two tests above
// would pass for a classify that simply stopped filtering.
func TestClassify_NonRefreshableKindIsStillSkipped(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoRefreshableKind{})

	cred := spiceboxv1alpha1.AgentCredential{
		Name: "static-cred", Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "static-store", Key: "token"},
		},
	}
	store := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "static-store"},
		Data: map[string][]byte{
			"refresh_token": []byte("rt"),
			"expires_at":    []byte(time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-identity"},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{cred}},
	}
	co, _ := newTestCore(t, store, ai)

	plan, err := co.classify(context.Background(), fakeIdentity{ai: ai}, cred, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, credSkipNotOAuth, plan.class,
		"a non-refreshable kind must still be skipped even though its Secret would otherwise be due")
}
