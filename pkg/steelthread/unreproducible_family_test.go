package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// The check reads TWO shapes, and this is the second one.
//
// The generic rule is a prefix plus sixteen hex digits, which is what every
// memory-kind mint site emits. A render CR name is not that — the session
// sits between its prefix and a six-hex tail — so for a long time it was
// listed as something this check deliberately did not catch, and a bundle that
// failed to pin one would emit clean and then diverge at replay on a value
// nobody could see in the emitted file.
//
// It has a family now, so the check also walks a decoded result with the FOLD's
// own matcher. That is what keeps "recognizable" and "collectable" the same
// set: any id the capture could have pinned is one it is held to.
func TestSelfCheck_UnreproducibleIDs_ReadsAFamilysOwnShape(t *testing.T) {
	const handle = "ar-demo-agent-9999-cd752b"

	withPinning := func(t *testing.T, minted map[string][]string) []steelthread.Finding {
		t.Helper()
		in := cleanCapture(t)
		in.MetaTools = append(in.MetaTools, "artifact_prepare")
		in.Folded.MintedIDs = minted
		in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
			LastToolResult:         "artifact_prepare",
			LastToolResultContains: `{"handle":"` + handle + `","status":"ready"}`,
		}})
		return steelthread.SelfCheck(in)
	}

	t.Run("a render handle the bundle failed to pin: refused, by name", func(t *testing.T) {
		f := findByCode(t, withPinning(t, nil), steelthread.CodeUnreproducibleID)
		assert.Contains(t, f.Message, handle,
			"the finding must name the handle; the generic sixteen-hex rule cannot see it, "+
				"so a reader who is only told 'an id' has nothing to look for")
	})

	t.Run("the same handle pinned: exempt", func(t *testing.T) {
		for _, f := range withPinning(t, map[string][]string{bt.FamilyRenderHandle: {handle}}) {
			require.NotEqual(t, steelthread.CodeUnreproducibleID, f.Code,
				"a pinned handle is handed back by the render-name seam")
		}
	})
}

// TestSelfCheck_UnreproducibleIDs_FamilyShapeNeedsDecodableJSON is the boundary
// of the second rule. It walks a DECODED result, so prose that merely mentions
// a handle is not a mint — the same whole-string discipline the fold applies
// when it collects one.
func TestSelfCheck_UnreproducibleIDs_FamilyShapeNeedsDecodableJSON(t *testing.T) {
	in := cleanCapture(t)
	in.MetaTools = append(in.MetaTools, "artifact_await")
	in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
		LastToolResult:         "artifact_await",
		LastToolResultContains: `artifact_await: handle "ar-demo-agent-9999-cd752b" is not this session's.`,
	}})
	for _, f := range steelthread.SelfCheck(in) {
		require.NotEqual(t, steelthread.CodeUnreproducibleID, f.Code,
			"a refusal message naming a handle minted nothing; the replay re-emits that sentence")
	}
}
