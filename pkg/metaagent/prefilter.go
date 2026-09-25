package metaagent

import (
	"strings"
	"unicode"
)

// Observer records what the prefilter decided, per turn.
//
// This is the module's OWN shadow counter, deliberately not the plan gate's
// logging mode — that is a plan-gate setting, and this module is independent of
// it.
//
// It records both outcomes because the number that matters is what the cheap
// filter MISSES. A counter that only reported invocations could not answer
// that, and the miss rate is precisely what decides whether the heuristic ever
// needs replacing with a small model.
type Observer interface {
	// PrefilterDecision reports one turn's outcome. filter names which rule
	// decided, so a later change of heuristic stays comparable in the data.
	PrefilterDecision(invoke bool, filter string)
}

// intentMarkers are the cheap heuristic: phrases that plausibly ask for a
// session change.
//
// Deliberately a keyword list, per the spec's "start with the cheap filter and
// measure". It trades recall for cost — it will miss paraphrase — and that
// trade is only defensible because of the asymmetry documented on ShouldExtract.
// Replacing it with a model is a decision the shadow counter's data should
// drive, not an assumption made up front.
var intentMarkers = []string{
	// scope
	"you can also", "you may also", "also read", "also access", "don't touch",
	"do not touch", "stay out of", "only look at", "restrict yourself",
	// budget
	"more tokens", "more turns", "bigger budget", "raise the budget",
	"less budget", "fewer turns",
	// model
	"bigger model", "smaller model", "switch to the", "use the model",
	// plan
	"skip the", "move to the", "go back to the",
}

// intentWords are single words distinctive enough to signal intent on their
// own, matched at WORD BOUNDARIES rather than as substrings.
//
// Substring matching on a word this short is a cost bug: "stop" fires on
// "stopped", "backstop" and "non-stop", which is ordinary conversation, and a
// filter that spends an extractor call on those defeats the affordability it
// exists to provide. Safety is unaffected either way — a false positive grants
// nothing — but cost is the entire reason this filter is here.
var intentWords = []string{"stop", "cancel", "abort", "halt"}

// ShouldExtract reports whether this turn is worth spending an extractor call
// on, and records the decision.
//
// # Why a lossy filter is safe here
//
// The extractor is an LLM call, and the ambient trigger fires on EVERY inbound
// turn — most of which are ordinary conversation. Without a prefilter the
// trigger is unaffordable. What makes a lossy one acceptable is that neither
// failure direction can grant anything:
//
//   - A FALSE NEGATIVE means the metaagent stayed quiet on a turn that meant
//     something. Nothing widens, nothing narrows, and the user still has every
//     explicit control — buttons, the CLI, and addressing the metaagent
//     directly. Classification is a convenience layer, never the sole route to
//     any capability, and that is what makes the miss survivable.
//   - A FALSE POSITIVE spends one extractor call on a turn that meant nothing.
//     The extractor then classifies it as no action. It costs money, not
//     authority.
//
// So this function decides only whether to SPEND, never what is permitted.
// Everything that governs what a decision may do — direction from the registry,
// the admin ceiling, the speaker's standing — sits downstream and is untouched
// by whatever this returns.
//
// mentioned bypasses the filter: when a user addresses the metaagent directly
// there is nothing to guess about, and a heuristic that could swallow an
// explicit request would make the feature feel broken.
func ShouldExtract(turn string, mentioned bool, obs Observer) bool {
	invoke, filter := shouldExtract(turn, mentioned)
	if obs != nil {
		// Best-effort: the counter is measurement, and losing it must never
		// break the path it measures.
		obs.PrefilterDecision(invoke, filter)
	}
	return invoke
}

func shouldExtract(turn string, mentioned bool) (invoke bool, filter string) {
	if mentioned {
		return true, "explicit_mention"
	}
	lowered := strings.ToLower(turn)
	for _, m := range intentMarkers {
		if strings.Contains(lowered, m) {
			return true, "keyword"
		}
	}
	for _, f := range strings.FieldsFunc(lowered, func(r rune) bool {
		// A hyphen is a WORD character here: "non-stop" is one word, and
		// splitting it hands "stop" to the matcher below.
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	}) {
		for _, w := range intentWords {
			if f == w {
				return true, "keyword"
			}
		}
	}
	return false, "keyword"
}
