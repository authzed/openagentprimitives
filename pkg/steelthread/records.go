// Package steelthread captures a live agent session as a replayable bundle.
//
// A steel thread is a bronzethread bundle whose transcript was TAKEN from a
// real session rather than authored. Same format, same driver, same divergence
// contract — the difference is evidentiary: a green bronze bundle proves the
// system handles an interaction, a green steel bundle also shows a real model
// produced it, at least once.
//
// Nothing here invokes an LLM. Every emitted field is derived from a durable
// record or left empty, and where a human would have to decide what is worth
// asserting, the capture declines rather than guessing.
package steelthread

import (
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
)

// Records is everything one session durably recorded that a capture reads.
//
// A plain struct rather than a live reader, so the fold is a pure function of
// its input: every rule below is unit-testable against synthetic records with
// no cluster, which is what makes a mechanical capture reviewable at all.
type Records struct {
	// Session is the namespace/name these records came from.
	Session string

	// Turns is the transcript, ordered by Index.
	Turns []memory.Turn

	// SystemPrompts are the prompts the session ran under, oldest first. More
	// than one means the prompt changed mid-session.
	SystemPrompts []systemprompt.Content

	// ToolCatalogs are the recorded catalog changes, ordered by FromTurnIndex.
	// EMPTY means unknown — a session captured before tool_catalog shipped —
	// not "no tools were offered".
	//
	// CARRIED WHOLE into bt.Bundle.ToolCatalogs, where the replay offers exactly
	// the recorded set at each turn and fails on any difference. What the run
	// was offered is a FACT the capture can simply state, and pinning the whole
	// set invents nothing.
	//
	// Still deliberately NOT reduced to a bt.Expect.ToolOffered. That is a
	// CLAIM about which capability mattered to the scenario ("was select_phase
	// offered while the gate was on?"), and a capture cannot pick one without
	// inventing a claim nobody made — the same judgement DeriveAssertions
	// declines for SystemPromptContains, for the same stated reason: an
	// assertion nobody meant fails later on an unrelated edit and nobody can
	// defend it. A human hand-adding one consults this field, or the bundle's
	// own toolCatalogs, to find out what was actually on offer.
	ToolCatalogs []toolcatalog.Content

	// Decisions and Gate are two of the four logs bt.TraceLinesWithFlow folds
	// into a golden.
	Decisions []authzdecision.Decision
	Gate      []plangateaudit.Content

	// Audit and Tags are the other two: the info-leakage/trifecta verdicts and
	// the provenance a run minted. A scenario about per-datum provenance
	// touches neither of the logs above, so without these its golden renders
	// EMPTY — a file that can never fail.
	Audit []infoleakageaudit.AuditRecord
	Tags  []pttag.TagRecord

	// DecisionsByToolCall is Decisions indexed by the tool_use block id each
	// one answered, oldest first within a call — the `for_tool_call` link
	// authzdecision.Record writes and the Decision struct itself has no field
	// for.
	//
	// It exists so Fold can tell an error result the GATE authored from one the
	// upstream server returned. A denied tool never reaches the MCP server
	// (pkg/agent/runner/loop_dispatch.go), so that distinction decides whether
	// a bundle needs a canned error at all — and getting it wrong in either
	// direction produces a bundle that replays differently from the session it
	// claims to be evidence of.
	//
	// A decision the writer could not attribute (an empty link, which
	// recordAuthzDecision emits when the dispatch context carries no ids)
	// appears in Decisions and NOT here: one unattributable decision must not
	// be allowed to speak for a call it may have nothing to do with.
	//
	// Order within a call is load-bearing in the same way Decisions' order is.
	// A call denied, approved and re-checked records BOTH outcomes, and only
	// the last says whether the tool ran.
	DecisionsByToolCall map[string][]authzdecision.Decision

	// RelWrites are the relationships the session's own tool dispatches wrote.
	// Subtracted from the derived seed: seeding a tuple the session wrote would
	// let the bundle pass with the relationship-writing code broken.
	RelWrites []relwritesaudit.Audit

	// Trigger is the signed delivery that opened the session, or nil when a
	// person started it.
	Trigger *triggerdelivery.Content

	// Approvals are the approval request/outcome pairs the run produced,
	// keyed by the toolUseID each pair answers — the same key
	// approval.ByToolCall queries by.
	//
	// A session that legitimately paused for a human cannot replay without
	// them: the bundle needs AutoApprove / AutoDeny / AutoApproveAs, and with
	// none of those a phase awaiting approval simply blocks until the class
	// timeout — the run reads as a hang rather than as the pause it is.
	Approvals map[string]approval.Pair

	// TransformedToolUseIDs are the tool calls whose output a guard rewrote,
	// gathered from the contentguard / toolguard / infoleakage audits.
	//
	// Their recorded output is post-transform, and replay would transform it
	// AGAIN. The capture cannot invert that, so it refuses — this set is how it
	// knows to.
	//
	// REQUIRED whenever the captured AgentClass configures a tool guard. Unlike
	// every other field here, this one is not read from a single memory Kind:
	// the caller must gather it across the guard audits, so an empty map has
	// two readings — no guard rewrote anything, or nobody gathered. Leaving it
	// empty does NOT pass the check: with SelfCheckInput.GuardConfigured set it
	// raises CodeGuardScanSkipped, because a scan that cannot say it looked
	// proves nothing, and the failure it would have caught is an output replay
	// transforms a second time.
	TransformedToolUseIDs map[string]string // toolUseID -> which guard
}
