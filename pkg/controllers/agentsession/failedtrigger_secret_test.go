// pkg/controllers/agentsession/failedtrigger_secret_test.go
//
// The operator's Secret cache holds only adopted Secrets, so a credentials
// Secret that exists but has not been adopted yet (one recreated during a key
// rotation, before the Channel controller reconciles again) reads back from it
// as NotFound. An answer must not take that for the Secret being gone: it
// would settle the session as unanswerable for good and say something untrue.
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// filteredCacheReconciler builds a Reconciler wired the way the operator is:
// its Client reads Secrets through a cache that holds only adopted ones, and
// its SecretReader reads the API server directly, refusing (by panicking, the
// production mode) a Secret nothing adopted. live is the API server's own
// view, for arranging and asserting.
func filteredCacheReconciler(
	t *testing.T, sess *spiceboxv1alpha1.AgentSession, secret *corev1.Secret,
) (r *agentsession.Reconciler, live client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))

	objs := []client.Object{
		sess,
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: tsChannelName, Namespace: tsNS},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           tsKindName,
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: tsSecretName},
			},
		},
	}
	if secret != nil {
		objs = append(objs, secret)
	}
	base := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()

	cached := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, isSecret := obj.(*corev1.Secret); !isSecret {
				return nil
			}
			if _, adopted := obj.GetLabels()[adoptguard.AdoptedLabel]; !adopted {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
			}
			return nil
		},
	})
	return &agentsession.Reconciler{
		Client: cached,
		SecretReader: adoptguard.NewSecretReader(cached, base, adoptguard.Panic,
			func(types.NamespacedName) bool { return false }),
	}, base
}

func credentialsSecret(labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tsSecretName, Namespace: tsNS, Labels: labels},
		Data:       map[string][]byte{"fixture-key": []byte("present")},
	}
}

func TestFailedTriggerBackstop_CredentialsSecretIsReadLive(t *testing.T) {
	channelOwnerKey := adoptkit.OwnerAnnotationKey("Channel", types.NamespacedName{Namespace: tsNS, Name: tsChannelName})
	cases := []struct {
		name   string
		secret *corev1.Secret
		// wantAnswered: one could_not_finish reached the provider.
		wantAnswered bool
		wantMarker   string
		wantMessage  string
		// wantAdoptedForChannel: the Secret ends up carrying the adoption
		// label and the Channel's owner annotation.
		wantAdoptedForChannel bool
	}{
		{
			name:                  "Secret exists but nothing adopted it (absent from the cache): adopted for its Channel, answered",
			secret:                credentialsSecret(nil),
			wantAnswered:          true,
			wantMarker:            spiceboxv1alpha1.ReasonTriggerAnswered,
			wantAdoptedForChannel: true,
		},
		{
			name:         "Secret already adopted: answered, and no owner is added to it",
			secret:       credentialsSecret(map[string]string{adoptguard.AdoptedLabel: "true"}),
			wantAnswered: true,
			wantMarker:   spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name:        "Secret really does not exist: settled as Unanswerable naming it, nothing created",
			wantMarker:  spiceboxv1alpha1.ReasonTriggerUnanswerable,
			wantMessage: "not answered: read the credentials Secret " + tsSecretName,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := resetTriggerKind(t)
			sess := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			r, live := filteredCacheReconciler(t, sess, tc.secret)

			retry := r.ReconcileFailedTriggerForTest(context.Background(), sess)
			assert.Zero(t, retry, "each of these settles in one pass")

			_, published := k.snapshot()
			if tc.wantAnswered {
				require.Len(t, published, 1, "the pull request must be told")
				assert.Equal(t, channelkinds.TriggerOutcomeCouldNotFinish, published[0].Outcome)
			} else {
				assert.Empty(t, published)
			}
			assert.Equal(t, tc.wantMarker, settledReason(t, r, sess))
			if tc.wantMessage != "" {
				c := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionTriggerAnswered)
				require.NotNil(t, c)
				assert.Contains(t, c.Message, tc.wantMessage)
			}

			var got corev1.Secret
			err := live.Get(context.Background(), types.NamespacedName{Namespace: tsNS, Name: tsSecretName}, &got)
			if tc.secret == nil {
				assert.True(t, apierrors.IsNotFound(err), "adoption must not create a Secret that was not there; got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, got.Labels, adoptguard.AdoptedLabel)
			if tc.wantAdoptedForChannel {
				assert.Equal(t, tsNS+"/"+tsChannelName, got.Annotations[channelOwnerKey],
					"adopted as the Channel controller would adopt it, not under the session")
			} else {
				assert.Empty(t, got.Annotations, "an adopted Secret is read as it stands")
			}
		})
	}
}
