package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

func TestDirectoryProjector_ProjectsKindCredentialAndLastPass(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec: spiceboxv1alpha1.RelationshipSourceSpec{
			Kind: "github",
			Auth: spiceboxv1alpha1.RelationshipSourceAuth{
				AgentIdentity: "forge-identity", Credential: "forge-pat",
			},
		},
		Status: spiceboxv1alpha1.RelationshipSourceStatus{
			Sync: spiceboxv1alpha1.RelationshipSourceSyncStatus{
				LastPass: &spiceboxv1alpha1.RelationshipSourcePassStats{
					ScopesProcessed: 3, Written: 40, Pruned: 2, JoinMisses: 7,
				},
			},
			Conditions: []metav1.Condition{
				cond(spiceboxv1alpha1.RelationshipSourceConditionReady, metav1.ConditionTrue, "Synced"),
			},
		},
	}
	c := newClient(t, src)

	p, ok := config.Get("directory")
	require.True(t, ok, "the directory slug must be registered")

	rows, err := p.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := rows[0]
	assert.Equal(t, "acme-github", row.Name)
	assert.Equal(t, "default", row.Namespace)
	assert.Equal(t, "namespaced", row.Scope)
	assert.Equal(t, "Ready", row.Status)
	assert.Equal(t, "oap directory configure", row.ManageCmd)

	// badgeVal/countVal return the zero value for a missing key, so these
	// assertions fail on an absent badge as well as a wrong one — which is what
	// matters, since a missing badge silently blanks a column (columns.ts).
	assert.Equal(t, "github", badgeVal(row, "kind"))
	assert.Equal(t, "forge-identity/forge-pat", badgeVal(row, "credential"))

	assert.Equal(t, 3, countVal(row, "scopes"))
	assert.Equal(t, 40, countVal(row, "written"))
	assert.Equal(t, 2, countVal(row, "pruned"))
	assert.Equal(t, 7, countVal(row, "joinMisses"))
}

// A source that has never completed a pass has no counts to show, and must not
// render zeros — "ran and found nothing" is a different and far less alarming
// claim than "has never run".
func TestDirectoryProjector_OmitsCountsBeforeTheFirstPass(t *testing.T) {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "slack"},
	}
	c := newClient(t, src)

	p, _ := config.Get("directory")
	rows, err := p.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Counts, "no pass has run, so there is nothing to count")
	assert.Equal(t, "Unknown", rows[0].Status, "no Ready condition yet")
}
