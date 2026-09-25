//go:build e2e

package threadrun

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// The two artifact id seams the driver wires, tested at the wiring rather than
// at either end of it.
//
// Both ends already have their own tests — bt.MintedIDSequence's minters in
// bronzethread, artifacts.Service's seams in that package, one of them wired to
// the real KeyedMinter. What neither can see is which minter this driver
// actually reaches for, and that is the choice with a silent failure mode: a
// revision seam drawn from Minter instead of KeyedMinter satisfies the first
// finalize and breaks prepare->await idempotence on the second, which no bundle
// that finalizes once would notice.

func TestRevisionIDMinter_IsKeyedSoTheHandoffStaysIdempotent(t *testing.T) {
	seq := bt.NewMintedIDSequence(
		map[string][]string{bt.FamilyArtifactRevision: {"artrev-1111111111111111", "artrev-2222222222222222"}},
		func(msg string) { t.Errorf("%s", msg) })

	mint := revisionIDMinter(seq)
	require.NotNil(t, mint, "a bundle that pinned revision ids gets a minter")

	first := mint("uid-a")
	assert.Equal(t, "artrev-1111111111111111", first)
	assert.Equal(t, first, mint("uid-a"),
		"artifact_await re-derives the id for the same CR and MUST get the same one back; "+
			"a plain Minter here would hand it artrev-2222222222222222 and finalize a second revision")
	assert.Equal(t, "artrev-2222222222222222", mint("uid-b"),
		"a different render still draws the next id")
}

func TestRevisionIDMinter_UnpinnedIsNil(t *testing.T) {
	seq := bt.NewMintedIDSequence(nil, func(msg string) { t.Errorf("%s", msg) })
	assert.Nil(t, revisionIDMinter(seq),
		"a bundle pinning no revision ids leaves the service deriving its own, as in production")
}

func TestRenderNameMinter_HandsBackTheRecordedHandleWhole(t *testing.T) {
	seq := bt.NewMintedIDSequence(
		map[string][]string{bt.FamilyRenderHandle: {"ar-recorded-session-cd752b"}},
		func(msg string) { t.Errorf("%s", msg) })

	mint := renderNameMinter(seq)
	require.NotNil(t, mint)
	assert.Equal(t, "ar-recorded-session-cd752b", mint("live-session-name"),
		"the recorded CR name is returned entire, session segment included — re-composing it "+
			"here would be a second spelling of artifacts.RenderName")
}

func TestRenderNameMinter_UnpinnedIsNil(t *testing.T) {
	seq := bt.NewMintedIDSequence(nil, func(msg string) { t.Errorf("%s", msg) })
	assert.Nil(t, renderNameMinter(seq),
		"an unpinned family leaves the service minting its own name")
}
