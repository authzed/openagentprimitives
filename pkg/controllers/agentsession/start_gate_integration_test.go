//go:build integration

package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// Against real SpiceDB, the property the fakes cannot prove: the explicit gate
// excludes a platform admin, the default gate admits one, and a starter written
// by the class reconciler an instant earlier is seen (FullyConsistent).
func TestStartGate_AgainstSpiceDB(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spdb.Close() })
	ctx := context.Background()

	const ns = "default"
	admin := identity.CanonicalFromTrusted(uniqLeak(t, "admin-"), "test fixture")
	listed := identity.CanonicalFromTrusted(uniqLeak(t, "listed-"), "test fixture")
	stranger := identity.CanonicalFromTrusted(uniqLeak(t, "stranger-"), "test fixture")
	require.NoError(t, spdb.TouchPlatformAdmin(ctx, admin))

	no := false
	cases := []struct {
		name        string
		class       string
		adminsStart *bool
		who         identity.CanonicalUserID
		wantPhase   string
	}{
		{"explicit gate refuses an unlisted platform admin", uniqLeak(t, "gate-x-"), &no, admin, spiceboxv1alpha1.AgentSessionPhaseFailed},
		{"explicit gate admits a listed starter", uniqLeak(t, "gate-y-"), &no, listed, ""},
		{"default gate admits a platform admin", uniqLeak(t, "gate-z-"), nil, admin, ""},
		{"default gate refuses a stranger", uniqLeak(t, "gate-w-"), nil, stranger, spiceboxv1alpha1.AgentSessionPhaseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, spdb.EnsureAgentClassPlatform(ctx, ns, tc.class))
			require.NoError(t, spdb.EnsureAgentClassStarters(ctx, ns, tc.class, []string{"user:" + listed.String()}))
			ac := &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: tc.class, Namespace: ns},
				Spec: spiceboxv1alpha1.AgentClassSpec{Authz: &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
					AllowedStarters: []string{"user:" + listed.String()}, PlatformAdminsMayStart: tc.adminsStart,
				}}},
				// The gate refuses to evaluate an unconfirmed starter set, and
				// the EnsureAgentClassStarters above is exactly the write this
				// condition attests to — so stamping it here matches the state a
				// reconciled class is in, rather than papering over a gap.
				Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
					Type:               spiceboxv1alpha1.AgentClassConditionStartersLinked,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonStartersLinked,
					LastTransitionTime: metav1.Now(),
				}}},
			}
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: tc.class + "-s", Namespace: ns,
					Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + tc.who.String()}},
				Spec: spiceboxv1alpha1.AgentSessionSpec{Class: tc.class},
			}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
				WithObjects(sess, ac).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
			r := &agentsession.Reconciler{Client: c, StartChecker: spdb}

			_, halted, err := r.EnforceStartGate(ctx, sess, ac)
			require.NoError(t, err)
			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: sess.Name}, &got))
			if tc.wantPhase == "" {
				assert.False(t, halted)
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
				return
			}
			assert.True(t, halted)
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, got.Status.FailureReason)
		})
	}
}
