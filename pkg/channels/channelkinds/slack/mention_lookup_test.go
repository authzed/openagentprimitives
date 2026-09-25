package slack

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// testScheme returns a scheme with the agentprimitives API group registered,
// so a fake.NewClientBuilder can host UserIdentity objects.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

func TestMentionForSubject_FindsSlackID(t *testing.T) {
	canonical := "user:" + base64.RawURLEncoding.EncodeToString([]byte("alice@example.com"))
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{
			{Kind: "slack", Domain: "T1", ExternalID: "U123", DisplayName: "Alice"},
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ui).Build()
	id, ok := mentionForSubject(context.Background(), cl, canonical, "T1")
	require.True(t, ok)
	assert.Equal(t, "U123", id)
}

func TestMentionForSubject_MissIsGraceful(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_, ok := mentionForSubject(context.Background(), cl, "user:unknown", "T1")
	assert.False(t, ok, "a user who never spoke in this workspace has no slack id — render fallback")
}

func TestMentionForSubject_WrongWorkspaceIsGraceful(t *testing.T) {
	canonical := "user:" + base64.RawURLEncoding.EncodeToString([]byte("bob@example.com"))
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{
			{Kind: "slack", Domain: "T-OTHER", ExternalID: "U999", DisplayName: "Bob"},
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ui).Build()
	_, ok := mentionForSubject(context.Background(), cl, canonical, "T1")
	assert.False(t, ok, "a slack identity recorded in a different workspace must not leak into this one's mention")
}
