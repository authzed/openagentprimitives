package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// cannedCapture is cleanCapture with the transcript rewritten so the model also
// calls one CANNABLE meta tool, and every list that has to know about a meta
// tool told about it.
//
// Built through the real Fold and the real bundle assembly, like cleanCapture,
// so a case here is measured against the same base every other self-check case
// is — and so a finding that fires for an unrelated reason (an unroutable call,
// a tool the fixture will not offer) shows up as that finding rather than as a
// silent absence of the one under test.
func cannedCapture(t *testing.T, results ...*memory.ToolResultBlock) steelthread.SelfCheckInput {
	t.Helper()
	require.NotEmpty(t, results, "a canned capture needs at least one recorded result")

	turns := []memory.Turn{userText(0, "what do you remember about widgets?")}
	idx := 1
	for _, r := range results {
		turns = append(turns,
			assistantCall(idx, r.ToolUseID, cannableMeta, `{"text":"widgets"}`),
			multiToolResult(idx+1, r),
		)
		idx += 2
	}
	turns = append(turns, assistantCall(idx, "tu_last", "respond_to_user", `{"text":"here is what I found"}`))

	in := cleanCapture(t)
	recs := in.Records
	recs.Turns = turns
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{MCPPrefixes: []string{"acme"}})
	require.NoError(t, err)

	in.Records = recs
	in.Folded = folded
	in.MetaTools = append(in.MetaTools, cannableMeta)
	in.FixtureTools = append(in.FixtureTools, steelthread.FixtureTool{Name: cannableMeta})
	in.Bundle.UserTurns = bundleTurns(folded.UserTurns)
	in.Bundle.LLM = folded.LLM
	in.Bundle.MetaToolReplies = folded.MetaToolReplies
	// The claim the canning is refused without. Derived the way Capture derives
	// it, from the transcript, rather than transcribed — so a case that broke
	// the derivation would fail here too.
	in.Bundle.Assert = steelthread.DeriveAssertions(recs, folded)
	in.Emitted = emittedFrom(t, in.Bundle, in.Files)
	return in
}

// TestCapture_CarriesTheCannedReplyAndTheClaimBesideIt runs the whole capture
// — records in, assembled bundle out — rather than assembling a bundle by hand.
//
// It exists because everything else in this file builds Bundle.MetaToolReplies
// itself, so a capture that FOLDED the reply correctly and then dropped it on
// the way into the bundle would pass every one of them. Mutation showed exactly
// that: blanking the assignment in Capture left this file green.
//
// It also pins the other half of the enforcement from the only place that can:
// the claim assert.toolsCalled must carry is DERIVED from the transcript, so a
// capture never has to remember to add it and can never emit a bundle its own
// Validate refuses.
func TestCapture_CarriesTheCannedReplyAndTheClaimBesideIt(t *testing.T) {
	const body = `{"entries":[{"id":"m-1","content":"the widget rollout shipped"}]}`

	recs := syntheticRecords(t)
	recs.Turns = append(recs.Turns[:3:3],
		assistantCall(3, "tu_2", cannableMeta, `{"text":"widgets"}`),
		toolResult(4, "tu_2", body, false),
		assistantCall(5, "tu_3", "respond_to_user", `{"text":"no widgets found"}`),
	)

	in := captureInput(t)
	in.MetaTools = append(in.MetaTools, cannableMeta)
	in.FixtureTools = append(in.FixtureTools, steelthread.FixtureTool{Name: cannableMeta})

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	require.False(t, steelthread.HasHardFinding(findings), "findings: %+v", findings)

	require.Contains(t, got.Bundle.MetaToolReplies, cannableMeta,
		"the fold canned this reply and the assembled bundle must carry it, or the replay runs the "+
			"real tool against an empty fixture")
	assert.Equal(t, body, got.Bundle.MetaToolReplies[cannableMeta].Content)
	assert.Contains(t, got.Bundle.Assert.ToolsCalled, cannableMeta,
		"the claim the canning is refused without is derived, so a capture cannot forget it")
	require.NoError(t, got.Bundle.Validate(),
		"an emitted bundle must satisfy the very method the driver refuses on")

	f := findByCode(t, findings, "meta-reply-canned")
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
}

// TestSelfCheck_ACannedMetaReplyWarnsAndNamesWhatStoppedRunning is the warning
// this feature owes a reader.
//
// A capture that cans a meta tool gives up coverage of this repo's own code,
// and nothing in the emitted bundle makes that obvious to somebody scanning it.
// The finding says which tool, what stops running, and what still does — the
// same job CodeStreamResultServedVerbatim does for a streaming toolkit.
func TestSelfCheck_ACannedMetaReplyWarnsAndNamesWhatStoppedRunning(t *testing.T) {
	in := cannedCapture(t, resultBlock("tu_1", `{"entries":[{"id":"m-1"}]}`, false))
	findings := steelthread.SelfCheck(in)

	require.False(t, steelthread.HasHardFinding(findings),
		"canning is a warning, not a refusal: the bundle is still legitimate")

	f := findByCode(t, findings, "meta-reply-canned")
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, cannableMeta, "the finding names the tool, never just 'a meta tool'")
	for _, want := range []string{
		"never runs at replay",
		"authz check and plan gate",
		"assert.toolsCalled",
	} {
		assert.Contains(t, f.Message, want,
			"the message must say what stops running, what still does, and what the bundle is refused without")
	}
}

// TestSelfCheck_ACleanCaptureRaisesNoCanningWarning is the negative control for
// the test above.
//
// Without it the warning could fire on every capture and the test above would
// still pass, which is exactly the assertion-that-asserts-nothing shape this
// suite keeps finding.
func TestSelfCheck_ACleanCaptureRaisesNoCanningWarning(t *testing.T) {
	for _, f := range steelthread.SelfCheck(cleanCapture(t)) {
		assert.NotEqual(t, "meta-reply-canned", f.Code,
			"a capture that called no cannable meta tool cans nothing and must warn about nothing")
	}
}

// TestSelfCheck_ANonUniformMetaReplyIsRefused: one body per tool name cannot
// stand for two different answers, so the capture refuses rather than emitting
// a bundle whose later step fails downstream naming the tool.
func TestSelfCheck_ANonUniformMetaReplyIsRefused(t *testing.T) {
	in := cannedCapture(t,
		resultBlock("tu_1", `{"entries":[{"id":"m-1"}]}`, false),
		resultBlock("tu_2", `{"entries":[{"id":"m-2"}]}`, false),
	)
	findings := steelthread.SelfCheck(in)

	f := findByCode(t, findings, "meta-reply-not-uniform")
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, cannableMeta)
	assert.True(t, steelthread.HasHardFinding(findings), "a hard finding stops the capture")
}

// TestSelfCheck_AGateRefusedMetaCallCansNothing is the safety property, checked
// where a reader will look for it: end to end, through SelfCheck, on the same
// input shape the tests above use.
//
// A canned refusal would carry the gate's own text — the same bytes the step's
// expectation was derived from — so a gate that STOPPED refusing would be
// served that text back and satisfy the assertion meant to catch it. Nothing is
// canned and nothing is warned about, because there is nothing to tell a reader.
func TestSelfCheck_AGateRefusedMetaCallCansNothing(t *testing.T) {
	const refusal = "permission denied: alice does not have read on memory:s1"
	in := cannedCapture(t, resultBlock("tu_1", refusal, true))
	in.Records = deniedFor("tu_1", refusal, in.Records.Turns)

	folded, err := steelthread.Fold(in.Records, steelthread.FoldOptions{MCPPrefixes: []string{"acme"}})
	require.NoError(t, err)
	in.Folded = folded
	in.Bundle.MetaToolReplies = folded.MetaToolReplies
	in.Emitted = emittedFrom(t, in.Bundle, in.Files)

	assert.Empty(t, in.Bundle.MetaToolReplies, "the gate refused the call; there is nothing to can")
	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, "meta-reply-canned", f.Code,
			"nothing was canned, so nothing is warned about")
		assert.NotEqual(t, "meta-reply-not-uniform", f.Code,
			"a tool that only ever errored has nothing to be non-uniform about")
	}
}

// TestSelfCheck_ACannedBundleIsRefusedWithoutItsToolsCalledEntry is the
// enforcement seen from the CAPTURE side.
//
// Bundle.Validate is the one definition both consumers use, and checkBundle
// runs it over the assembled bundle — so a capture that ever emitted a canned
// reply without the claim beside it is refused HERE, before a file is written,
// rather than minutes later inside somebody else's suite.
func TestSelfCheck_ACannedBundleIsRefusedWithoutItsToolsCalledEntry(t *testing.T) {
	in := cannedCapture(t, resultBlock("tu_1", `{"entries":[{"id":"m-1"}]}`, false))
	require.Contains(t, in.Bundle.Assert.ToolsCalled, cannableMeta,
		"the derivation supplies the claim, so this case has something to remove")

	// Remove ONLY the claim, leaving the canned reply in place.
	var kept []string
	for _, n := range in.Bundle.Assert.ToolsCalled {
		if n != cannableMeta {
			kept = append(kept, n)
		}
	}
	in.Bundle.Assert.ToolsCalled = kept
	in.Emitted = emittedFrom(t, in.Bundle, in.Files)

	findings := steelthread.SelfCheck(in)
	f := findByCode(t, findings, "bundle-invalid")
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.True(t, strings.Contains(f.Message, "metaToolReplies"),
		"the refusal names the field a reader has to fix, got: %s", f.Message)
}
