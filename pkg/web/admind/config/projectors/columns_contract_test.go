package projectors

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// directoryColumnKeys mirrors testdata/directory_column_keys.json — the
// committed handshake between this projector and columns.ts's own vitest
// test (pkg/web/adminui/ui/config/columns.directory.test.ts). Three earlier
// rounds had this Go test regex-parse columns.ts's TypeScript directly, and
// each round's pattern broke on a different reformatting (adjacency, an
// intervening property, a nested brace) — regex-parsing one language from
// the other was the wrong tool, not any one pattern. Neither side parses the
// other's language now: both read this same fixture natively.
type directoryColumnKeys struct {
	Badges []string `json:"badges"`
	Counts []string `json:"counts"`
}

// TestDirectoryProjectorKeysMatchFixture asserts the directory projector
// emits exactly the badge keys and count labels recorded in
// testdata/directory_column_keys.json. Renaming a badge/count key here
// (without updating the fixture) fails this test; asking columns.ts for a
// key the fixture doesn't list fails the paired TypeScript test instead —
// together the two catch a rename from either side.
func TestDirectoryProjectorKeysMatchFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/directory_column_keys.json")
	require.NoError(t, err, "read testdata/directory_column_keys.json")
	var fixture directoryColumnKeys
	require.NoError(t, json.Unmarshal(raw, &fixture), "parse testdata/directory_column_keys.json")

	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-github", Namespace: "default"},
		Spec: spiceboxv1alpha1.RelationshipSourceSpec{
			Kind:    "github",
			Auth:    spiceboxv1alpha1.RelationshipSourceAuth{AgentIdentity: "forge-identity", Credential: "forge-pat"},
			BaseURL: "https://ghe.example.internal",
		},
		Status: spiceboxv1alpha1.RelationshipSourceStatus{
			Sync: spiceboxv1alpha1.RelationshipSourceSyncStatus{
				LastPass: &spiceboxv1alpha1.RelationshipSourcePassStats{
					ScopesProcessed: 3, Written: 40, Pruned: 2, JoinMisses: 7,
				},
			},
		},
	}
	c := newClient(t, src)

	p, ok := config.Get("directory")
	require.True(t, ok)
	rows, err := p.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	var badges, counts []string
	for _, bd := range rows[0].Badges {
		badges = append(badges, bd.Key)
	}
	for _, ct := range rows[0].Counts {
		counts = append(counts, ct.Label)
	}

	assert.ElementsMatch(t, fixture.Badges, badges,
		"the directory projector's emitted badge keys must match testdata/directory_column_keys.json")
	assert.ElementsMatch(t, fixture.Counts, counts,
		"the directory projector's emitted count labels must match testdata/directory_column_keys.json")
}
