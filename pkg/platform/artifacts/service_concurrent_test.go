package artifacts_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// headReadBarrier holds the first `want` head reads until all of them have
// arrived, or until a grace period elapses, forcing the interleaving a real
// parallel tool-use fan-out produces on a loaded cluster: both finalizes read the
// head before either writes it.
//
// The grace period is what keeps the test honest in BOTH directions. Once
// FinalizeRevision serializes per head, the second read cannot arrive while the
// first is held — so the barrier releases on the timer, the two calls run one
// after the other, and the assertions describe the fixed behaviour instead of
// deadlocking.
type headReadBarrier struct {
	memory.Memory

	mu      sync.Mutex
	arrived int
	want    int
	grace   time.Duration
	release chan struct{}
	once    sync.Once
}

func newHeadReadBarrier(inner memory.Memory, want int, grace time.Duration) *headReadBarrier {
	return &headReadBarrier{Memory: inner, want: want, grace: grace, release: make(chan struct{})}
}

func (b *headReadBarrier) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	if len(q.Kinds) == 1 && q.Kinds[0] == (memartifact.Kind{}).Name() {
		b.arrive()
	}
	return b.Memory.Query(ctx, q)
}

func (b *headReadBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	full := b.arrived >= b.want
	b.mu.Unlock()
	if full {
		b.once.Do(func() { close(b.release) })
		return
	}
	select {
	case <-b.release:
	case <-time.After(b.grace):
		b.once.Do(func() { close(b.release) })
	}
}

// TestFinalizeRevision_ConcurrentRevisesOfOneHead_NeitherLosesItsSeqOrTags is
// the defect. Loop.dispatchToolUses runs a turn's tool_uses on separate
// goroutines, so two artifact_prepare(revises:) calls in one assistant message
// really do finalize concurrently. FinalizeRevision is a read-modify-write of the
// head with nothing serializing it: both read RevisionCount=N, both mint
// seq=N+1, and the later head Put overwrites the earlier's applied tags — the
// artifact silently loses a revision from its count and a handle stops resolving.
func TestFinalizeRevision_ConcurrentRevisesOfOneHead_NeitherLosesItsSeqOrTags(t *testing.T) {
	bar := newHeadReadBarrier(memory.NewLocal(inmem.NewBackend()), 2, 2*time.Second)
	svc := artifacts.NewService(bar, nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess-concurrent"}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	crs := [2]*spiceboxv1alpha1.ArtifactRender{
		renderedCR("ar-par-a", headID, "", "branch a", "reviewed", types.UID("uid-par-a")),
		renderedCR("ar-par-b", headID, "", "branch b", "shipped", types.UID("uid-par-b")),
	}
	for _, cr := range crs {
		cr.Annotations[artifacts.AnnoArtifactName] = "report"
	}

	var (
		wg      sync.WaitGroup
		results [2]artifacts.RevisionResult
		errs    [2]error
	)
	for i := range crs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.FinalizeRevision(ctx, scope, crs[i])
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0], "finalize A")
	require.NoError(t, errs[1], "finalize B")

	head, ok, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err, "GetHead after two concurrent finalizes")
	require.True(t, ok, "head must exist")

	assert.Equal(t, 2, head.RevisionCount,
		"both revisions must be counted; a shared seq means one was lost from the head")
	assert.NotEqual(t, results[0].Seq, results[1].Seq,
		"two concurrent revisions of one head must not be minted at the same seq")
	assert.Equal(t, results[0].RevisionID, head.Tags["reviewed"],
		"the first finalize's applied tag must not be clobbered by the second's head write")
	assert.Equal(t, results[1].RevisionID, head.Tags["shipped"],
		"the second finalize's applied tag must be recorded")

	// Both revisions stay reachable — losing one from the head is the
	// user-visible symptom (a handle that no longer resolves).
	tree, err := svc.RevisionTree(ctx, scope, headID)
	require.NoError(t, err, "RevisionTree")
	assert.Len(t, tree, 2, "both revisions are durable and listed")
}

// TestFinalizeRevision_ConcurrentFinalizesOfDifferentHeadsDoNotSerialize keeps
// the lock's granularity honest: serializing per head is required, serializing
// the whole Service is not, and a coarse lock would hold one artifact's render
// behind an unrelated one's durable writes.
func TestFinalizeRevision_ConcurrentFinalizesOfDifferentHeadsDoNotSerialize(t *testing.T) {
	// want=2 with a grace far longer than the test's patience: if the two
	// finalizes did NOT overlap, the barrier would only release on the timer.
	bar := newHeadReadBarrier(memory.NewLocal(inmem.NewBackend()), 2, time.Minute)
	svc := artifacts.NewService(bar, nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess-disjoint"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	headA, headB := svc.NewArtifactID(), svc.NewArtifactID()
	crs := [2]*spiceboxv1alpha1.ArtifactRender{
		renderedCR("ar-dis-a", headA, "", "a", "", types.UID("uid-dis-a")),
		renderedCR("ar-dis-b", headB, "", "b", "", types.UID("uid-dis-b")),
	}

	done := make(chan error, 2)
	for i := range crs {
		go func(i int) {
			_, err := svc.FinalizeRevision(ctx, scope, crs[i])
			done <- err
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			require.NoError(t, err, "finalize of a distinct head")
		case <-time.After(15 * time.Second):
			t.Fatal("finalizes of two DIFFERENT heads must not serialize behind each other")
		}
	}
}
