package metaagent_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
)

// countingObserver records what the gate decided, standing in for the shadow
// counter the host wires.
type countingObserver struct{ invoked, filtered int }

func (o *countingObserver) PrefilterDecision(invoke bool, _ string) {
	if invoke {
		o.invoked++
		return
	}
	o.filtered++
}

// The affordability property: the extractor is an LLM call on every inbound
// turn, and most turns are ordinary conversation. The prefilter is what makes
// an ambient trigger affordable at all.
func TestShouldExtract_ordinaryConversationDoesNotSpendAnExtractorCall(t *testing.T) {
	var obs countingObserver

	for _, turn := range []string{
		"thanks, that looks right",
		"what did the second test say?",
		"can you summarise the diff",
		"",
	} {
		assert.False(t, metaagent.ShouldExtract(turn, false, &obs),
			"turn %q carries no session-change intent", turn)
	}
	assert.Equal(t, 4, obs.filtered)
	assert.Zero(t, obs.invoked)
}

// A turn that does look like intent spends the call.
func TestShouldExtract_anUtteranceThatLooksLikeIntentInvokesTheExtractor(t *testing.T) {
	var obs countingObserver

	for _, turn := range []string{
		"you can also read the deploy logs",
		"give yourself more tokens",
		"switch to the bigger model",
		"stop, cancel this",
		"don't touch the production repo",
	} {
		assert.True(t, metaagent.ShouldExtract(turn, false, &obs),
			"turn %q looks like a session change", turn)
	}
	assert.Equal(t, 5, obs.invoked)
}

// An EXPLICIT mention bypasses the filter entirely. The heuristic exists to save
// money on ambient turns; when a user addresses the metaagent directly there is
// nothing to guess about, and a filter that could swallow an explicit request
// would make the feature feel broken.
func TestShouldExtract_anExplicitMentionAlwaysInvokes(t *testing.T) {
	var obs countingObserver

	assert.True(t, metaagent.ShouldExtract("thanks, that looks right", true, &obs),
		"an explicit mention is never prefiltered, however ordinary the words")
	assert.Equal(t, 1, obs.invoked)
}

// The filter must not be fooled by ordinary casing or punctuation — a heuristic
// that only matches lowercase is a heuristic that misses most real messages.
func TestShouldExtract_matchesRegardlessOfCaseAndPunctuation(t *testing.T) {
	var obs countingObserver

	for _, turn := range []string{
		"You can ALSO read the deploy logs.",
		"Give yourself more tokens!",
		"  switch to the bigger model  ",
	} {
		assert.True(t, metaagent.ShouldExtract(turn, false, &obs), "turn %q", turn)
	}
}

// EVERY decision is observed, both ways. The whole point of the counter is to
// measure what the cheap filter MISSES, and a gate that only reported its hits
// could not answer that — the miss rate is the number that decides whether the
// heuristic needs replacing with a model.
func TestShouldExtract_recordsEveryDecisionSoMissesAreMeasurable(t *testing.T) {
	var obs countingObserver

	metaagent.ShouldExtract("you can also read the logs", false, &obs)
	metaagent.ShouldExtract("thanks", false, &obs)

	assert.Equal(t, 1, obs.invoked)
	assert.Equal(t, 1, obs.filtered)
}

// A nil observer must not panic: the counter is measurement, and losing it can
// never be allowed to break the path it measures.
func TestShouldExtract_toleratesNoObserver(t *testing.T) {
	require.NotPanics(t, func() {
		metaagent.ShouldExtract("you can also read the logs", false, nil)
	})
}

// Short lifecycle words must match as WORDS, not substrings. "stop" inside
// "stopped", "backstop" or "non-stop" is ordinary conversation, and a filter
// that fires on those spends an extractor call on a large share of normal
// turns — which defeats the affordability it exists to provide.
//
// This is a cost bug, not a safety one (a false positive grants nothing), but
// the whole reason the filter exists is cost.
func TestShouldExtract_shortLifecycleWordsMatchAsWordsNotSubstrings(t *testing.T) {
	var obs countingObserver

	for _, turn := range []string{
		"the build stopped working",
		"that's a good backstop for the retry",
		"it ran non-stop for an hour",
		"the cancellation policy is unclear",
		"the halting problem, basically",
	} {
		assert.False(t, metaagent.ShouldExtract(turn, false, &obs),
			"turn %q is ordinary conversation, not a lifecycle instruction", turn)
	}

	// The real instruction still fires.
	assert.True(t, metaagent.ShouldExtract("stop", false, &obs))
	assert.True(t, metaagent.ShouldExtract("please cancel this run", false, &obs))
}
