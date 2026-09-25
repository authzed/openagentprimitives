package spiceboxsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// doorMemory answers Put the way the operator's facade does: an append-only
// write first SEEDS the publisher's hash chain, which reads the scope, and the
// capability door denies a caller that minted no approval. A stub that ignores
// the context cannot tell a minted caller from an unminted one — which is why
// the missing mint here survived a green suite.
type doorMemory struct{ puts []memory.Entry }

func (d *doorMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if err := memory.EnsureApproval(ctx, memory.ReadMemory, e.Scope.ID); err != nil {
		return memory.Entry{}, err
	}
	d.puts = append(d.puts, e)
	return e, nil
}

func (d *doorMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (d *doorMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (d *doorMemory) SendSignal(context.Context, memory.Signal) error { return nil }

// The `resolved` toolchain attestation is part of the tamper-evident log, and
// its write is deliberately non-fatal — so a denial at the capability door
// costs nothing visible and simply leaves the entry missing from every
// session's chain. It has to carry the operator's own approval, like every
// other in-process write to the facade.
func TestRecordResolved_writesUnderTheOperatorsMemoryApproval(t *testing.T) {
	mem := &doorMemory{}
	r := &Reconciler{AuditMemory: mem}
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bundle", Namespace: "ns",
			Labels: map[string]string{"agentprimitives.authzed.com/agentsession": "parent"},
		},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{
			ResolvedToolchains: []spiceboxv1alpha1.ToolchainMount{{Name: "tc", Image: "img@sha256:aa"}},
			ToolchainSetDigest: "sha256:bb",
		},
	}

	r.recordResolved(context.Background(), sess)

	require.Len(t, mem.puts, 1, "the attestation must reach the store, not be denied at the door")
	assert.Equal(t, "ns/parent", mem.puts[0].Scope.ID, "attributed to the parent AgentSession's scope")
}
