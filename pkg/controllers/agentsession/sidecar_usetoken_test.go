// pkg/controllers/agentsession/sidecar_usetoken_test.go
//
// Unit tests for materializeSidecarSecret's one-time pre-handout use_token
// check: a sidecar credential whose authorized_token grant is absent/revoked
// (a definitive SpiceDB deny) or whose check is indeterminate (the RPC
// itself errored) must NOT have its Secret written; a present/allowed grant
// must, and a nil TokenChecker (no SpiceDB wired, e.g. test fixtures) must
// skip the check entirely rather than panic or fail closed. Uses a fake
// controller-runtime client (no envtest), mirroring sidecar_federated_test.go's
// style.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// fakeUseTokenChecker records every CheckUseToken call and returns the
// configured (allowed, err) pair for all of them.
type fakeUseTokenChecker struct {
	allowed bool
	err     error
	calls   []fakeUseTokenCall
}

type fakeUseTokenCall struct {
	ns, name, credID, presented string
	fullyConsistent             bool
}

func (f *fakeUseTokenChecker) CheckUseToken(_ context.Context, ns, name, credID, presentedValueHash string, fullyConsistent bool) (bool, error) {
	f.calls = append(f.calls, fakeUseTokenCall{ns, name, credID, presentedValueHash, fullyConsistent})
	return f.allowed, f.err
}

// TestMaterializeSidecarSecret_UseTokenCheck exercises the pre-handout gate
// against one shared fixture (a session/class/identity with a single
// type=static sidecar credential), varying only the TokenChecker's behavior.
func TestMaterializeSidecarSecret_UseTokenCheck(t *testing.T) {
	const (
		ns          = "default"
		sidecarRef  = "linear"
		envVar      = "UPSTREAM_TOKEN"
		credSecret  = "linear-upstream-secret"
		secretValue = "super-secret-upstream-token"
		secretName  = "sc-secret"
		hashKey     = "session-args-hash-key"
	)

	newFixture := func(t *testing.T) (*Reconciler, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.AgentClass, spiceboxv1alpha1.ResolvedSidecarToolbox, *fakeUseTokenChecker) {
		t.Helper()
		upstreamSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credSecret, Namespace: ns},
			Data:       map[string][]byte{"token": []byte(secretValue)},
		}
		ai := &spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
			Spec: spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: sidecarRef + "-creds",
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: credSecret, Key: "token"},
					},
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(upstreamSecret, ai).Build()

		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns},
			Spec:       spiceboxv1alpha1.AgentSessionSpec{AgentIdentity: "ai-linear"},
		}
		ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns}}
		rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
			Name: sidecarRef,
			Ref:  sidecarRef,
			Spec: spiceboxv1alpha1.SidecarToolboxSpec{
				UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-token", EnvVar: envVar},
			},
		}
		checker := &fakeUseTokenChecker{}
		return &Reconciler{Client: c, TokenChecker: checker}, sess, ac, rt, checker
	}

	cases := []struct {
		name          string
		nilChecker    bool
		allowed       bool
		checkErr      error
		wantErr       bool
		wantErrSubstr string
		wantReason    string
		wantSecret    bool
	}{
		{
			name:          "definitive deny: error mentions revoked, Secret NOT written, generic SidecarBootFailed reason",
			allowed:       false,
			wantErr:       true,
			wantErrSubstr: "revoked",
			wantReason:    spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
			wantSecret:    false,
		},
		{
			name:       "indeterminate (RPC errored): fails closed, TokenAuthzUnavailable reason, Secret NOT written",
			checkErr:   assert.AnError,
			wantErr:    true,
			wantReason: spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable,
			wantSecret: false,
		},
		{
			name:       "allowed: Secret written with the resolved value",
			allowed:    true,
			wantErr:    false,
			wantSecret: true,
		},
		{
			name:       "nil TokenChecker: check skipped, Secret written",
			nilChecker: true,
			wantErr:    false,
			wantSecret: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, sess, ac, rt, checker := newFixture(t)
			checker.allowed = tc.allowed
			checker.err = tc.checkErr
			if tc.nilChecker {
				r.TokenChecker = nil
			}

			err := r.materializeSidecarSecret(t.Context(), sess, ac, rt, secretName, []byte(hashKey))
			if tc.wantErr {
				require.Error(t, err)
				if tc.wantErrSubstr != "" {
					assert.Contains(t, err.Error(), tc.wantErrSubstr)
				}
				assert.Equal(t, tc.wantReason, sidecarMaterializeFailReason(err))
			} else {
				require.NoError(t, err)
			}

			var sec corev1.Secret
			getErr := r.Client.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: secretName}, &sec)
			if tc.wantSecret {
				require.NoError(t, getErr, "Secret must be written")
				got := sec.StringData[envVar]
				if got == "" {
					got = string(sec.Data[envVar])
				}
				assert.Equal(t, secretValue, got)
			} else {
				assert.True(t, apierrors.IsNotFound(getErr), "Secret must NOT be written")
			}

			if !tc.nilChecker {
				require.Len(t, checker.calls, 1)
				call := checker.calls[0]
				assert.Equal(t, ns, call.ns)
				assert.Equal(t, "s-linear", call.name)
				assert.Equal(t, externaltoken.ValueHash([]byte(hashKey), secretValue), call.presented)
				assert.True(t, call.fullyConsistent, "the pre-handout check must be fully-consistent (never stale-read a just-written grant as absent)")
			}
		})
	}
}

// TestMaterializeSidecarSecret_SecretReaderWired_AdoptsBySecretRefName proves
// the credkind-registry migration's SecretRef-based adoption locates the
// upstream credential Secret by ref.Name — never ref.Key. The task that added
// SecretRef() to credkind.Kind flagged this exact confusion (an earlier
// migration nearly indexed a Secret by ref.Key, which is only meaningful for
// static's single-key shape and is empty for oauth). This fixture deliberately
// gives the credential's SecretRef.Name and SecretRef.Key DIFFERENT strings —
// "linear-upstream-secret" vs "token" — so adopting by the wrong field would
// either target a Secret that doesn't exist (a wrong-name Get failure) or
// silently skip adoption. Only adopting by ref.Name succeeds and stamps the
// adopted label on the Secret actually named by SecretRef.Name.
func TestMaterializeSidecarSecret_SecretReaderWired_AdoptsBySecretRefName(t *testing.T) {
	const (
		ns          = "default"
		envVar      = "UPSTREAM_TOKEN"
		sidecarRef  = "linear"
		credName    = sidecarRef + "-creds"
		secretName  = "linear-upstream-secret" // SecretRef.Name
		secretKey   = "token"                  // SecretRef.Key — deliberately != secretName
		secretValue = "super-secret-upstream-token"
	)

	upstreamSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{secretKey: []byte(secretValue)},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName,
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: secretKey},
				},
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(upstreamSecret, ai).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()

	// Warn (not Panic): this test exercises the adoption WRITE path, not the
	// guarded-read enforcement itself.
	secretReader := adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &Reconciler{Client: c, SecretReader: secretReader}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{AgentIdentity: "ai-linear"},
	}
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns}}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: sidecarRef,
		Ref:  sidecarRef,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-token", EnvVar: envVar},
		},
	}

	require.NoError(t, r.materializeSidecarSecret(t.Context(), sess, ac, rt, "sc-secret", nil))

	var adopted corev1.Secret
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: secretName}, &adopted),
		"the Secret named by SecretRef.Name must exist and be reachable")
	assert.Equal(t, "true", adopted.Labels[adoptguard.AdoptedLabel],
		"the Secret named by SecretRef.Name — not SecretRef.Key — must carry the adopted label")
}
