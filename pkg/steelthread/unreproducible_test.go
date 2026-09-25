package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestSelfCheck_UnreproducibleIDs_NegativeControls is the half that decides
// whether the check is worth having.
//
// A rule that refused every id-shaped string would refuse every capture, and
// the one that matters most is the PINNED case: operation and artifact ids
// appear in meta-tool results constantly, they ARE reproduced by their own
// seams, and a check that could not tell them from an unseamed one would make
// the whole feature unusable.
func TestSelfCheck_UnreproducibleIDs_NegativeControls(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		payload string
		minted  map[string][]string
	}{
		{
			name:    "an id the bundle PINS is reproduced by its own seam",
			tool:    "artifact_prepare",
			payload: `{"artifact_id":"artifact-1d48c3e8f06bfab6"}`,
			minted:  map[string][]string{bt.FamilyArtifact: {"artifact-1d48c3e8f06bfab6"}},
		},
		{
			name:    "a pinned operation id, which every plan step's result carries",
			tool:    "update_plan",
			payload: `{"items":[{"operation_id":"op-72334cfbe590ed6c","state":"in_progress"}]}`,
			minted:  map[string][]string{bt.FamilyOperation: {"op-72334cfbe590ed6c"}},
		},
		{
			// The handle used to sit in the "not caught" list because its shape
			// is not the sixteen-hex body the generic rule reads. It has a seam
			// and a family now, so it is exempt the way every other id is —
			// by being PINNED, not by being unrecognizable.
			name:    "a pinned render handle, which artifact_prepare returns",
			tool:    "artifact_prepare",
			payload: `{"handle":"ar-demo-reviewbot-gh-47e5d583-cd752b"}`,
			minted:  map[string][]string{bt.FamilyRenderHandle: {"ar-demo-reviewbot-gh-47e5d583-cd752b"}},
		},
		{
			name:    "a pinned revision id, derived from the render CR's UID",
			tool:    "artifact_prepare",
			payload: `{"revision_id":"artrev-171fcb3ee98f70b3"}`,
			minted:  map[string][]string{bt.FamilyArtifactRevision: {"artrev-171fcb3ee98f70b3"}},
		},
		{
			// The session segment makes a handle look enough like a filename to
			// be worth a control: an artifact's own filename is in the same
			// result and must not be read as an id.
			name:    "a filename that ends in a render-shaped tail",
			tool:    "artifact_prepare",
			payload: `{"filename":"ar-demo-a1b2c3.html"}`,
		},
		{
			name:    "a commit sha, which is 40 hex behind no prefix",
			tool:    "claim_trigger_status",
			payload: `{"ref":"ba03f5969a9e29334d669472f4346f29ea7247fe"}`,
		},
		{
			name:    "a check run id, which is decimal and a provider's own",
			tool:    "claim_trigger_status",
			payload: `{"ref":"demo-reviewbot check run 99044729080 on demo-org/platform#42"}`,
		},
		{
			name:    "prose that merely contains hex",
			tool:    "respond_to_user",
			payload: `the build at commit deadbeefdeadbeef finished`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			in.MetaTools = append(in.MetaTools, tc.tool)
			in.Folded.MintedIDs = tc.minted
			in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
				LastToolResult:         tc.tool,
				LastToolResultContains: tc.payload,
			}})

			for _, f := range steelthread.SelfCheck(in) {
				assert.NotEqual(t, steelthread.CodeUnreproducibleID, f.Code,
					"nothing here is an id a replay would mint differently; refusing it would "+
						"refuse every capture: %s", f.Message)
			}
		})
	}
}

// TestSelfCheck_UnreproducibleIDs_OnlyReadsMetaToolResults pins the scope.
//
// An MCP or sandbox tool's result is CANNED — the replay serves back the
// recorded bytes verbatim — so an id inside one is reproduced by definition and
// is not this check's business. Only a meta tool composes its reply afresh.
func TestSelfCheck_UnreproducibleIDs_OnlyReadsMetaToolResults(t *testing.T) {
	in := cleanCapture(t)
	// Deliberately NOT added to MetaTools: this stands for an upstream tool
	// whose whole result the bundle cans.
	in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
		LastToolResult:         "demoforge_lookup",
		LastToolResultContains: `{"ticket_id":"ticket-171fcb3ee98f70b3"}`,
	}})

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeUnreproducibleID, f.Code,
			"a canned result is served back byte for byte, so an id inside one replays exactly")
	}
}

// TestSelfCheck_UnreproducibleIDs_ExemptsByPinningNotByName is the property that
// makes the rule survive a new family.
//
// The check asks whether the id is IN Bundle.MintedIDs, never whether its prefix
// is one it knows. So a family that gains a seam stops firing with nothing here
// edited, and one whose seam is removed starts firing again — which is exactly
// the direction a regression should move it.
func TestSelfCheck_UnreproducibleIDs_ExemptsByPinningNotByName(t *testing.T) {
	const id = "widget-0123456789abcdef"

	withPinning := func(t *testing.T, minted map[string][]string) []steelthread.Finding {
		t.Helper()
		in := cleanCapture(t)
		in.MetaTools = append(in.MetaTools, "demo_meta_tool")
		in.Folded.MintedIDs = minted
		in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
			LastToolResult:         "demo_meta_tool",
			LastToolResultContains: `{"widget":"` + id + `"}`,
		}})
		return steelthread.SelfCheck(in)
	}

	t.Run("unpinned, and its prefix is in no family this package knows", func(t *testing.T) {
		f := findByCode(t, withPinning(t, nil), steelthread.CodeUnreproducibleID)
		assert.Contains(t, f.Message, id,
			"the finding has to name the id, or a reader cannot find the mint site that needs a seam")
	})

	t.Run("pinned under any family name at all, and it is exempt", func(t *testing.T) {
		got := withPinning(t, map[string][]string{"widget": {id}})
		for _, f := range got {
			require.NotEqual(t, steelthread.CodeUnreproducibleID, f.Code,
				"an id the bundle pins is handed back by its seam; the check must read the "+
					"pinning, not a prefix it recognizes")
		}
	})
}
