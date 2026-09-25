package workshopthreadsrv

// This file is an INTERNAL test (package workshopthreadsrv, not
// workshopthreadsrv_test) specifically so it can reach bindingAnchor/
// bindsAnchor directly — the unexported pair this package deliberately
// duplicates from pkg/channels/channelsd/pipeline's own threadAnchor/
// bindsAnchor rather than importing (see bindingAnchor's doc comment,
// MINOR-6). thread_owner.go is live channelsd pipeline routing code on a
// hot path, so unifying the two was ruled out of scope for this round; this
// test is the substitute — it makes any future drift between the two LOUD
// instead of silent, by asserting they agree over a shared input table —
// with one deliberate, documented exception (the "divergence" field on
// sessCases below): bindsAnchor carries an explicit empty-anchor guard
// pipeline's own bindsAnchor does not have. That row asserts the actual,
// divergent values on each side rather than papering over the difference
// with equality.
//
// pipeline.ThreadAnchorForTest / pipeline.BindsAnchorForTest are tiny
// exported test-only accessors added to that package for exactly this
// purpose (see their own doc comment there), following the repo's
// established "…ForTest" convention (pkg/agent/tool/mcp's
// SetTimeoutForTest/UseTokenGateForTest) for an accessor that must cross a
// package boundary.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

func TestAnchorHelpers_MatchPipelinesOwnBehavior(t *testing.T) {
	extCases := []struct {
		name string
		ext  map[string]string
	}{
		{"both fields present", map[string]string{"channel_id": "C100", "thread_ts": "T200"}},
		{"missing channel_id", map[string]string{"thread_ts": "T200"}},
		{"missing thread_ts", map[string]string{"channel_id": "C100"}},
		{"empty map", map[string]string{}},
		{"nil map", nil},
	}
	for _, tc := range extCases {
		t.Run("bindingAnchor == pipeline.ThreadAnchorForTest: "+tc.name, func(t *testing.T) {
			assert.Equal(t, pipeline.ThreadAnchorForTest(tc.ext), bindingAnchor(&spiceboxv1alpha1.ChannelBinding{External: tc.ext}))
		})
	}
	t.Run("bindingAnchor == pipeline.ThreadAnchorForTest: nil binding", func(t *testing.T) {
		assert.Equal(t, pipeline.ThreadAnchorForTest(nil), bindingAnchor(nil))
	})

	const anchor = "C100:T200"
	matchingBinding := &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C100", "thread_ts": "T200"}}
	otherBinding := &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C100", "thread_ts": "T999"}}

	sessCases := []struct {
		name   string
		sess   *spiceboxv1alpha1.AgentSession
		anchor string
		// divergence documents this package's ONE known, deliberate difference
		// from pipeline's own bindsAnchor (see this package's own bindsAnchor
		// doc): pipeline's bindsAnchor has no empty-anchor guard, so an empty
		// anchor argument also matches a binding whose OWN computed anchor is
		// also empty ("" == ""); this package's bindsAnchor refuses on an
		// empty anchor unconditionally. Set only on the one row that exercises
		// it — asserting the plain equality below on that row would fail (see
		// the PARITY GAP finding this row exists to pin), which is exactly
		// what makes it worth a real assertion instead of silent agreement.
		divergence bool
	}{
		{
			name:   "input binding matches, no output binding",
			sess:   &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: matchingBinding}},
			anchor: anchor,
		},
		{
			name:   "output binding matches, no input binding",
			sess:   &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{OutputChannel: matchingBinding}},
			anchor: anchor,
		},
		{
			name:   "neither binding matches",
			sess:   &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: otherBinding, OutputChannel: otherBinding}},
			anchor: anchor,
		},
		{
			name:   "no bindings at all",
			sess:   &spiceboxv1alpha1.AgentSession{},
			anchor: anchor,
		},
		{
			name:   "empty anchor argument",
			sess:   &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: matchingBinding}},
			anchor: "",
		},
		{
			// PARITY GAP: the row above uses a binding that DOES carry an
			// anchor ("C100:T200" != ""), so both sides fall through to "not
			// equal" and never exercise the "" == "" case at all. This row's
			// binding carries channel_id but no thread_ts, so ITS OWN computed
			// anchor is also "" — the one shape that actually reaches the
			// divergence.
			name: "KNOWN DIVERGENCE -- empty anchor against a binding lacking thread_ts",
			sess: &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{
				InputChannel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C100"}},
			}},
			anchor:     "",
			divergence: true,
		},
	}
	for _, tc := range sessCases {
		t.Run("bindsAnchor == pipeline.BindsAnchorForTest: "+tc.name, func(t *testing.T) {
			pipelineResult := pipeline.BindsAnchorForTest(tc.sess, tc.anchor)
			localResult := bindsAnchor(tc.sess, tc.anchor)
			if tc.divergence {
				assert.True(t, pipelineResult, "pipeline.bindsAnchor has no empty-anchor guard: \"\" == \"\" matches a binding with no computed anchor of its own")
				assert.False(t, localResult, "this package's bindsAnchor refuses an empty anchor unconditionally (see its own doc)")
				return
			}
			assert.Equal(t, pipelineResult, localResult)
		})
	}
}
