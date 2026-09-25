package bronzethread_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// collectReports returns a sequence whose loud-failure reports land in the
// returned slice instead of a *testing.T, so the report text itself is
// assertable.
func collectReports(t *testing.T, ids map[string][]string) (*bt.MintedIDSequence, *[]string) {
	t.Helper()
	var (
		mu   sync.Mutex
		msgs []string
	)
	seq := bt.NewMintedIDSequence(ids, func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, msg)
	})
	return seq, &msgs
}

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "operation id: recognized as the operation family", in: "op-86eb4d1c9f0a2b37", want: bt.FamilyOperation, ok: true},
		{name: "artifact id: recognized as the artifact family", in: "artifact-9d4e0c1122ab33ff", want: bt.FamilyArtifact, ok: true},
		{name: "revision id: recognized as the artifactRevision family", in: "artrev-9d4e0c1122ab33ff", want: bt.FamilyArtifactRevision, ok: true},
		{name: "render handle: recognized as the renderHandle family", in: "ar-demo-agent-a1b2c3", want: bt.FamilyRenderHandle, ok: true},
		{name: "render handle, one-character session: still one", in: "ar-x-a1b2c3", want: bt.FamilyRenderHandle, ok: true},
		{name: "no session segment: not a render handle", in: "ar-a1b2c3", ok: false},
		{name: "render tail too short: not a render handle", in: "ar-demo-a1b2c", ok: false},
		{name: "render tail not hex: not a render handle", in: "ar-demo-a1b2cg", ok: false},
		{name: "render tail not preceded by a dash: not a render handle", in: "ar-demoa1b2c3", ok: false},
		{name: "a filename ending in a render-shaped tail: not a render handle", in: "ar-demo-a1b2c3.html", ok: false},
		{name: "an artifact id does not fall into the render family", in: "artifact-9d4e0c1122ab33ff", want: bt.FamilyArtifact, ok: true},
		{name: "hand-authored short id: not the minted shape", in: "op-1", ok: false},
		{name: "uppercase hex: not the shape crypto/rand+hex emits", in: "op-86EB4D1C9F0A2B37", ok: false},
		{name: "non-hex body: not an id", in: "op-zzzzzzzzzzzzzzzz", ok: false},
		{name: "too long: not an id", in: "op-86eb4d1c9f0a2b370", ok: false},
		{name: "prose mentioning an id: whole-string match only", in: "operation op-86eb4d1c9f0a2b37 is registered", ok: false},
		{name: "empty string: not an id", in: "", ok: false},
		{name: "bare prefix: not an id", in: "artifact-", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bt.FamilyOf(tc.in)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestMintedIDSequence_HandsOutRecordedIDsInOrder(t *testing.T) {
	seq, msgs := collectReports(t, map[string][]string{
		bt.FamilyOperation: {"op-1111111111111111", "op-2222222222222222"},
	})
	mint := seq.Minter(bt.FamilyOperation)
	require.NotNil(t, mint, "a family the bundle recorded gets a minter")

	assert.Equal(t, "op-1111111111111111", mint())
	assert.Equal(t, "op-2222222222222222", mint())
	assert.Empty(t, *msgs, "a run that mints exactly what was recorded reports nothing")
	assert.Empty(t, seq.Unused(), "and leaves nothing unused")
}

func TestMintedIDSequence_UnrecordedFamilyGetsNoMinter(t *testing.T) {
	seq, _ := collectReports(t, map[string][]string{
		bt.FamilyOperation: {"op-1111111111111111"},
	})
	assert.Nil(t, seq.Minter(bt.FamilyArtifact),
		"a family the bundle pinned nothing for must fall through to production minting")
	assert.Nil(t, bt.NewMintedIDSequence(nil, nil).Minter(bt.FamilyOperation),
		"a bundle with no mintedIDs at all pins nothing")
}

func TestMintedIDSequence_ExhaustionReportsFamilyAndIndex(t *testing.T) {
	seq, msgs := collectReports(t, map[string][]string{
		bt.FamilyOperation: {"op-1111111111111111"},
	})
	mint := seq.Minter(bt.FamilyOperation)
	require.Equal(t, "op-1111111111111111", mint())

	got := mint()
	require.Len(t, *msgs, 1, "the second mint is one more than was recorded and must be reported")
	assert.Contains(t, (*msgs)[0], bt.FamilyOperation, "the report names the family")
	assert.Contains(t, (*msgs)[0], "#2", "and the 1-based index that had no recorded id")
	assert.Contains(t, (*msgs)[0], "recorded 1", "and how many the capture did record")
	assert.NotEqual(t, "op-1111111111111111", got,
		"an exhausted minter must NOT repeat a recorded id: two operations would collide")
	fam, ok := bt.FamilyOf(got)
	assert.False(t, ok, "and must not look like a real minted id (%s, family %q)", got, fam)
}

func TestMintedIDSequence_LeftoversAreReported(t *testing.T) {
	seq, _ := collectReports(t, map[string][]string{
		bt.FamilyOperation: {"op-1111111111111111", "op-2222222222222222", "op-3333333333333333"},
		bt.FamilyArtifact:  {"artifact-4444444444444444"},
	})
	mint := seq.Minter(bt.FamilyOperation)
	mint()

	unused := seq.Unused()
	assert.Equal(t, []string{"op-2222222222222222", "op-3333333333333333"}, unused[bt.FamilyOperation],
		"the ids the replay never asked for, in recorded order")
	assert.Equal(t, []string{"artifact-4444444444444444"}, unused[bt.FamilyArtifact],
		"a family whose minter was never even called is entirely unused")
}

func TestMintedIDSequence_NilReporterPanicsRatherThanDroppingTheFailure(t *testing.T) {
	seq := bt.NewMintedIDSequence(map[string][]string{bt.FamilyOperation: {"op-1111111111111111"}}, nil)
	mint := seq.Minter(bt.FamilyOperation)
	require.Equal(t, "op-1111111111111111", mint())
	assert.Panics(t, func() { mint() },
		"a sequence with nowhere to report to must not swallow an exhaustion")
}

func TestMintedIDSequence_ConcurrentMintersDoNotRace(t *testing.T) {
	const n = 64
	ids := make([]string, 0, n)
	for i := range n {
		ids = append(ids, fmt.Sprintf("op-%016x", i))
	}
	seq, msgs := collectReports(t, map[string][]string{bt.FamilyOperation: ids})
	mint := seq.Minter(bt.FamilyOperation)

	var wg sync.WaitGroup
	out := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = mint() }()
	}
	wg.Wait()

	assert.Empty(t, *msgs)
	assert.ElementsMatch(t, ids, out, "every recorded id is handed out exactly once")
}
