package spiceboxsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func ttlScheme(t *testing.T) *apiruntime.Scheme {
	t.Helper()
	s := apiruntime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// expiredSession builds a SpiceboxSession whose idle TTL elapsed long ago.
func expiredSession(name string, owners []metav1.OwnerReference) *spiceboxv1alpha1.SpiceboxSession {
	past := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", OwnerReferences: owners,
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class:   "toolbelt",
			IdleTTL: &metav1.Duration{Duration: 30 * time.Minute},
		},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{LastActivityAt: &past},
	}
}

// TestSweepOnce_SkipsAgentSessionOwnedSessions is the regression test for the
// zap2 mid-session bundle kill: the sweeper idle-TTL-deleted a bundle
// SpiceboxSession owned by a live AgentSession (the bundle's LastActivityAt
// only moves on ToolCall completion, so an unused bundle "idles out" while
// the user is actively chatting), which the parent then misread as a pod
// crash and terminally failed. Bundle sessions owned by an AgentSession are
// that controller's to reap — the sweeper must never touch them.
func TestSweepOnce_SkipsAgentSessionOwnedSessions(t *testing.T) {
	owned := expiredSession("sess-bundle", []metav1.OwnerReference{{
		APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:       "AgentSession",
		Name:       "parent",
		UID:        types.UID("parent-uid"),
		Controller: ptr.To(true),
	}})
	standalone := expiredSession("sess-standalone", nil)
	fresh := expiredSession("sess-fresh", nil)
	now := metav1.Now()
	fresh.Status.LastActivityAt = &now

	c := fake.NewClientBuilder().
		WithScheme(ttlScheme(t)).
		WithObjects(owned, standalone, fresh).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxSession{}).
		Build()
	r := &Reconciler{Client: c}

	r.sweepOnce(context.Background())

	var got spiceboxv1alpha1.SpiceboxSession
	assert.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess-bundle"}, &got),
		"AgentSession-owned session must survive the sweep even when expired")

	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess-standalone"}, &got)
	assert.Error(t, err, "expired standalone session must be swept")

	assert.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess-fresh"}, &got),
		"non-expired session must be untouched")
}
