package steelthread

import (
	"slices"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Deriving the state a REPLAY has to seed into its stand-in provider.
//
// See bronzethread's standin.go for the rule this implements. The short of it:
// a meta tool's reply mixes what our code composed with what a provider minted
// or held, and only the second half has to be carried, because the first is
// reproduced by running the code. This file finds the second half.
//
// # Nothing here knows a provider's format
//
// Every value is extracted by the CHANNEL KIND, through
// channelkinds.TriggerStatusReporter.TriggerProviderStateIn, over text that
// same kind composed. This package supplies only the two things the kind cannot
// know: WHICH text is worth reading, and in what order. Spelling a provider's
// format here instead would be the `if kind == "github"` this repo keeps out of
// consumers, and it would drift the first time the kind reworded itself.

// deriveProviderState reads back, from the recorded transcript, the values the
// run got from its trigger's provider.
//
// # Which text is read, and why not all of it
//
// Two sources, both narrow on purpose. A spurious id is strictly worse than a
// missing one — the stand-in hands them back IN ORDER, so one phantom entry
// shifts every later id by a position and the run then addresses an object the
// recorded one never touched.
//
//   - INBOUND turns' text. The head commit reaches a record only inside the
//     prompt the kind's own receiver rendered for the delivery, and that lands
//     as a user turn. Assistant turns are excluded because the model's prose is
//     not a record of what a provider said; it is the model repeating something,
//     possibly wrongly.
//   - Tool results of the PROVIDER-SURFACE tools, and no others. Those are the
//     calls that reached the provider at all, identified by the caller through
//     the same assemble-and-difference the fixture-tool prediction uses. A
//     sandbox tool that printed something provider-shaped is not a provider
//     answer, and reading one would file a number the provider never minted.
//
// Order is transcript order, and Merge dedups: the same ref echoed by a later
// summary is not a second mint.
//
// Returns the zero value when no reporter is supplied, which is the ordinary
// answer for a session whose channel kind reports no trigger status at all.
func deriveProviderState(recs Records, providerTools map[string]bool, reporter channelkinds.TriggerStatusReporter) channelkinds.TriggerProviderState {
	if reporter == nil {
		return channelkinds.TriggerProviderState{}
	}

	var out channelkinds.TriggerProviderState
	useIDToName := map[string]string{}

	turns := slices.Clone(recs.Turns)
	slices.SortStableFunc(turns, func(a, b memory.Turn) int { return a.Index - b.Index })

	for _, t := range turns {
		for _, blk := range t.Content {
			switch {
			case blk.Type == "tool_use" && blk.ToolUse != nil:
				useIDToName[blk.ToolUse.ID] = blk.ToolUse.Name

			case blk.Type == "text" && blk.Text != "" && t.Role == "user":
				out = out.Merge(reporter.TriggerProviderStateIn(blk.Text))

			case blk.Type == "tool_result" && blk.ToolResult != nil:
				if !providerTools[useIDToName[blk.ToolResult.ToolUseID]] {
					continue
				}
				payload, _ := toolenvelope.Unwrap(blk.ToolResult.Content)
				out = out.Merge(reporter.TriggerProviderStateIn(payload))
			}
		}
	}
	return out
}

// recordedToolCalls is every tool the transcript DISPATCHED, deduplicated and
// sorted.
//
// A FACT, not a judgement, which is the whole reason a capture may state it.
// The capture declines to derive Expect.ToolOffered because WHICH tool mattered
// to a scenario is a claim nobody made; "this tool was called" is sitting in
// the transcript and needs no interpretation. See bt.Assertions.ToolsCalled.
//
// Sorted rather than kept in call order because order is already asserted,
// positionally and far more precisely, by each step's own Expect. A second
// ordered list would restate that and then fail on a reordering the positional
// check had already reported.
func recordedToolCalls(recs Records) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range recs.Turns {
		for _, blk := range t.Content {
			if blk.Type != "tool_use" || blk.ToolUse == nil || blk.ToolUse.Name == "" {
				continue
			}
			if seen[blk.ToolUse.Name] {
				continue
			}
			seen[blk.ToolUse.Name] = true
			out = append(out, blk.ToolUse.Name)
		}
	}
	slices.Sort(out)
	return out
}
