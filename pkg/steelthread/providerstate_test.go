package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// demoReporter is a stand-in TriggerStatusReporter whose extractor is spelled
// out in the test, so a case can state exactly which text yields which value.
//
// A fake rather than the github kind, deliberately: this file is about whether
// the CAPTURE asks the kind and carries the answer, and driving a real kind here
// would make a github rewording fail a test about wiring. The github kind's own
// formatter/inverse pair is pinned by its round-trip tests, where it belongs.
type demoReporter struct {
	// in maps a text fragment to the state the "kind" reads out of it.
	in map[string]channelkinds.TriggerProviderState
	// calls counts what the capture actually handed over, so a case can assert
	// which texts were consulted rather than only what came back.
	calls []string
}

// Compile-time, and NOT decoration: consumers resolve a reporter by TYPE
// ASSERTION, so a fake that stops satisfying this interface still builds and
// silently stops being one — here it would make every case below assert about a
// capture that simply had no reporter.
var _ channelkinds.TriggerStatusReporter = (*demoReporter)(nil)

func (*demoReporter) TriggerSurfaceKind() string { return "a demo provider's status" }

func (*demoReporter) TriggerSurface(
	*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.ChannelBinding,
	channelkinds.WebhookSecrets, channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	return nil, nil
}

func (r *demoReporter) TriggerProviderStateIn(text string) channelkinds.TriggerProviderState {
	r.calls = append(r.calls, text)
	return r.in[text]
}

// triggeredCapture is captureInput plus the delivery record that makes the
// session a triggered one, and the fixture Channel the trigger signs with.
func triggeredCapture(t *testing.T) (steelthread.Records, steelthread.CaptureInput) {
	t.Helper()
	recs := syntheticRecords(t)
	recs.Trigger = &triggerdelivery.Content{
		Kind:       "demoforge",
		Event:      "pull_request",
		ChannelKey: "pr:demo-org/platform#42",
		Body:       []byte(`{"action":"opened"}`),
	}

	in := captureInput(t)
	in.Fixture.TriggerChannel = "demo-hooks"
	return recs, in
}

// TestCapture_CarriesTheProviderStateTheKindReadBack is the wire test for the
// whole tier-2 path, and it is the one M8 (dropping the HeadSHA assignment)
// walked straight through before it existed.
//
// Three separate things have to hold and each is one assignment away from being
// dropped: the reporter is CONSULTED, the revision reaches bt.Trigger.HeadSHA,
// and the minted ids reach bt.StandIn.TriggerStatusIDs. A bundle missing the
// first two replays into "response carried no head commit" — the surface reads
// the triggering resource before it does anything else — and one missing the
// third diverges on the id the recorded reply named.
func TestCapture_CarriesTheProviderStateTheKindReadBack(t *testing.T) {
	const revision = "ba03f5969a9e29334d669472f4346f29ea7247fe"

	recs, in := triggeredCapture(t)
	rep := &demoReporter{in: map[string]channelkinds.TriggerProviderState{
		"list the widgets": {SurfaceRevision: revision, MintedIDs: []string{"99044729080"}},
	}}
	in.TriggerProvider = rep

	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	require.NotEmpty(t, rep.calls, "the capture never asked the kind to read its own text back")

	require.NotNil(t, res.Bundle.Trigger)
	assert.Equal(t, revision, res.Bundle.Trigger.HeadSHA,
		"the revision the surface resolved is what the stand-in provider reports back; without "+
			"it every call to the surface refuses outright rather than answering differently")

	require.NotNil(t, res.Bundle.StandIn, "a run that observed provider ids has state to seed")
	assert.Equal(t, []string{"99044729080"}, res.Bundle.StandIn.TriggerStatusIDs,
		"the ids the provider minted, in mint order, for the stand-in to hand back")
}

// TestCapture_ReadsOnlyTheTextWorthReading pins the scoping, which is what keeps
// a phantom id out of the sequence.
//
// A spurious id is strictly worse than a missing one: the stand-in draws them IN
// ORDER, so one extra entry shifts every later id by a position and the run then
// addresses an object the recorded one never touched. Assistant prose is the
// obvious source of one — a model repeating an identifier it half-remembers — so
// it is not read at all.
func TestCapture_ReadsOnlyTheTextWorthReading(t *testing.T) {
	const modelProse = "I opened check run 12345 on demo-org/platform#42 earlier"

	recs, in := triggeredCapture(t)
	// The model narrating an identifier back. It looks exactly like something a
	// kind would have written, which is the point: it is the model's own words
	// and may be wrong, half-remembered, or about a different object entirely.
	recs.Turns = append(recs.Turns, memory.Turn{
		Index: 4, Role: "assistant",
		Content: []memory.ContentBlock{{Type: "text", Text: modelProse}},
	})

	rep := &demoReporter{}
	in.TriggerProvider = rep

	_, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	// syntheticRecords' only user text. Its tool result belongs to
	// demoforge_list_widgets, which is not a provider-surface tool.
	assert.Contains(t, rep.calls, "list the widgets",
		"an INBOUND turn's text is where the kind's own rendered prompt lands")
	assert.NotContains(t, rep.calls, `{"results":[]}`,
		"a tool result belonging to no provider-surface tool must not be read; an id found "+
			"there was minted by something else entirely")
	assert.NotContains(t, rep.calls, modelProse,
		"ASSISTANT prose is not a record of what a provider said. Reading it would let a model "+
			"repeating an identifier plant a mint that never happened — and a phantom entry shifts "+
			"every later id in the sequence by a position")
}

// TestCapture_NoReporterCarriesNothingAndSaysSo is the fail-closed direction.
//
// An empty seed has two readings — "the run observed nothing from the provider"
// and "nobody looked" — and only the second is a failure of the capture. With no
// reporter the capture must state neither a revision nor an id, and the gate
// finding is what tells the two apart for a session that called such a tool.
func TestCapture_NoReporterCarriesNothingAndSaysSo(t *testing.T) {
	recs, in := triggeredCapture(t)
	in.TriggerProvider = nil

	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	require.NotNil(t, res.Bundle.Trigger)
	assert.Empty(t, res.Bundle.Trigger.HeadSHA,
		"nothing was read, so nothing may be claimed; inventing a revision would produce a "+
			"bundle that replays cleanly and is evidence about nothing")
	assert.Nil(t, res.Bundle.StandIn,
		"and no stand-in block at all, which is the same value a bundle built by hand has")
}

// TestDeriveAssertions_ToolsCalledIsEveryDispatch pins the derived called-proof.
//
// A FACT, which is the whole reason a capture may state it: the transcript
// records the dispatch. Sorted and deduplicated, because ORDER is already
// asserted positionally and far more precisely by each step's own Expect, and a
// second ordered claim would report the same reordering twice.
func TestDeriveAssertions_ToolsCalledIsEveryDispatch(t *testing.T) {
	recs, in := triggeredCapture(t)
	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	assert.Equal(t, []string{"demoforge_list_widgets", "respond_to_user"}, res.Bundle.Assert.ToolsCalled,
		"every tool the transcript dispatched, sorted and deduplicated")
}
