package bronzethread_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// KeyedMinter is the answer for an id that is DERIVED from something rather
// than drawn fresh. The property under test is that asking twice for one key is
// ONE mint — which is what keeps artifacts.FinalizeRevision idempotent across
// the artifact_prepare -> artifact_await handoff.

func TestKeyedMinter_SameKeyIsOneMint(t *testing.T) {
	seq, msgs := collectReports(t, map[string][]string{
		bt.FamilyArtifactRevision: {"artrev-1111111111111111", "artrev-2222222222222222"},
	})
	mint := seq.KeyedMinter(bt.FamilyArtifactRevision)
	require.NotNil(t, mint, "a family the bundle recorded gets a keyed minter")

	first := mint("uid-a")
	assert.Equal(t, "artrev-1111111111111111", first)
	assert.Equal(t, first, mint("uid-a"),
		"a second ask for one key is the SAME id: a fresh one would finalize a second revision of one render")
	assert.Equal(t, first, mint("uid-a"), "and a third, however many times the handoff repairs")

	assert.Equal(t, "artrev-2222222222222222", mint("uid-b"),
		"a different key draws the next recorded id")
	assert.Empty(t, *msgs, "two keys drew two ids, so nothing is exhausted")
	assert.Empty(t, seq.Unused(), "and nothing is left over")
}

func TestKeyedMinter_UnpinnedFamilyIsNil(t *testing.T) {
	seq, _ := collectReports(t, map[string][]string{bt.FamilyOperation: {"op-1111111111111111"}})
	assert.Nil(t, seq.KeyedMinter(bt.FamilyArtifactRevision),
		"a family the bundle pinned nothing for stays unpinned, and the component derives its own")
}

func TestKeyedMinter_KeysAreNamespacedByFamily(t *testing.T) {
	seq, _ := collectReports(t, map[string][]string{
		bt.FamilyArtifactRevision: {"artrev-1111111111111111"},
		bt.FamilyArtifact:         {"artifact-2222222222222222"},
	})
	assert.Equal(t, "artrev-1111111111111111", seq.KeyedMinter(bt.FamilyArtifactRevision)("shared"))
	assert.Equal(t, "artifact-2222222222222222", seq.KeyedMinter(bt.FamilyArtifact)("shared"),
		"two families keyed on one string must not alias each other")
}

func TestKeyedMinter_ExhaustionIsStillReported(t *testing.T) {
	seq, msgs := collectReports(t, map[string][]string{
		bt.FamilyArtifactRevision: {"artrev-1111111111111111"},
	})
	mint := seq.KeyedMinter(bt.FamilyArtifactRevision)
	require.Equal(t, "artrev-1111111111111111", mint("uid-a"))

	got := mint("uid-b")
	require.Len(t, *msgs, 1, "a second DISTINCT key past the end is a real over-mint and must be reported")
	assert.Contains(t, (*msgs)[0], bt.FamilyArtifactRevision)
	fam, ok := bt.FamilyOf(got)
	assert.False(t, ok, "the exhausted placeholder must not look minted (%s, family %q)", got, fam)
}

func TestKeyedMinter_ConcurrentAsksForOneKeyAgree(t *testing.T) {
	const n = 32
	ids := make([]string, 0, n)
	for i := range n {
		ids = append(ids, fmt.Sprintf("artrev-%016x", i))
	}
	seq, _ := collectReports(t, map[string][]string{bt.FamilyArtifactRevision: ids})
	mint := seq.KeyedMinter(bt.FamilyArtifactRevision)

	var wg sync.WaitGroup
	out := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = mint("one-uid") }()
	}
	wg.Wait()

	for i := range n {
		assert.Equal(t, out[0], out[i], "every concurrent ask for one key must agree")
	}
}
