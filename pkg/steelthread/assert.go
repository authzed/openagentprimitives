package steelthread

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// DeriveAssertions derives the property assertions a capture can make about
// a finished run without guessing.
//
// Three claims are attempted; a fourth is deliberately declined:
//
//   - AgentReplyContains is exactly the text every respond_to_user call
//     carried, read off the folded reply rather than out of the transcript
//     again. It has to know BOTH shapes the fold emits: bt.ReplyPart.Text for
//     a call whose arguments were exactly a non-empty text, and
//     bt.ReplyPart.ToolUse for one carrying anything more — an artifact
//     delivery, say — which the shorthand cannot express whole. See
//     foldReply's "Which shape a respond_to_user takes".
//   - Authz.Decisions is the FINAL outcome per "<type>:<id>#<permission>"
//     key, matching AuthzAssertions' own doc: a call denied, then approved,
//     then re-dispatched, records both, and the resolved outcome is the one
//     that decided whether the tool ran.
//   - PlanGate states only what is read off a single field, never an
//     invariant that needs re-deriving the gate's own reconstruction logic
//     (rebuilding a plan from its log, checking a call was gated against the
//     frozen one, …) — those belong to a human who has looked at the run, not
//     to a mechanical capture.
//
// SystemPromptContains is left EMPTY, always. What a prompt carries in
// common with the class that composed it — which authored example matters,
// which piece of guidance is worth pinning — is a judgement call, not a fact
// sitting in a record. A capture that guessed would produce an assertion
// nobody meant and that nobody can defend when it later fails on an
// unrelated prompt edit. Leaving it empty is the honest answer: this claim
// is for a human to add, having read the prompt themselves.
func DeriveAssertions(recs Records, f Folded) bt.Assertions {
	return bt.Assertions{
		AgentReplyContains: deriveReplies(f),
		// See the doc comment above: never guessed, never derived.
		SystemPromptContains: nil,
		Authz:                deriveAuthzAssertions(recs.Decisions),
		PlanGate:             derivePlanGateAssertions(recs.Gate),
		// Every tool the run DISPATCHED. The fourth claim, and the one the
		// paragraph above draws the line for: WHICH tool mattered is a
		// judgement and stays a human's, but THAT a tool was called is a fact
		// sitting in the transcript, so stating it invents nothing.
		//
		// Worth stating separately from each step's positional Expect because a
		// narrowed Expect asserts nothing about its call — a result the capture
		// could not pin every byte of leaves LastToolResultContains empty — and
		// a call that stops happening then shifts the transcript rather than
		// failing by name.
		ToolsCalled: recordedToolCalls(recs),
	}
}

// deriveReplies collects every respond_to_user text, in the order the run
// sent them. A bare narration block (bt.ReplyPart.BareText) is not a reply to
// a person and is excluded — see foldReply's doc comment on the two text
// shapes.
func deriveReplies(f Folded) []string {
	var out []string
	for _, step := range f.LLM {
		for _, part := range step.Reply {
			if text := replyText(part); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// replyText returns the message one reply part carried to a person, in
// whichever of the two shapes the fold chose, or "" when it carried none.
//
// THE one reader of that fact. deriveReplies and the self-check's checkReplies
// both ask it, because a second spelling drifts the moment the fold gains a
// shape — which is exactly what happened when ToolUse-shaped replies were
// introduced and only one of the two readers was updated.
//
// The ToolUse branch is not a fallback: a respond_to_user that ALSO delivered
// an artifact is emitted as a full tool_use, and reading only ReplyPart.Text
// would silently drop the words of every such reply from AgentReplyContains —
// leaving the bundle asserting nothing about the one turn a person actually
// read.
//
// A `text` argument that is absent or is not a string yields "", and that is an
// answer rather than a swallowed error: the call carried no message a person
// could read, so there is no reply to assert. The arguments themselves are
// known to be valid JSON — foldReply refuses a transcript whose respond_to_user
// args do not decode, and re-marshals what it emits.
func replyText(part bt.ReplyPart) string {
	if part.Text != "" {
		return part.Text
	}
	if part.ToolUse == nil || part.ToolUse.Name != respondToUserToolName {
		return ""
	}
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(part.ToolUse.Args, &args); err != nil {
		return ""
	}
	return args.Text
}

// deriveAuthzAssertions folds the decision log to one outcome per key, kept
// in RECORD order so the last write for a key is the one that survives —
// "denied, then approved, then allowed" resolves to "allowed", the same
// semantics AuthzAssertions.Decisions documents. Returns nil when the run
// checked nothing: a bundle with no authorization claim to make should not
// carry an empty one, which the driver's checkAuthz would otherwise have to
// distinguish from "checked, and nothing decided."
func deriveAuthzAssertions(decisions []authzdecision.Decision) *bt.AuthzAssertions {
	if len(decisions) == 0 {
		return nil
	}
	final := make(map[string]string, len(decisions))
	for _, d := range decisions {
		final[fmt.Sprintf("%s:%s#%s", d.ResourceType, d.ResourceID, d.Permission)] = d.Outcome
	}
	return &bt.AuthzAssertions{Decisions: final}
}

// derivePlanGateAssertions states the two plan-gate facts readable off a
// single field, matching exactly what the driver's own checkPlanGate
// compares against (test/e2e/threadrun/driver.go):
//
//   - NoRecords, when the gate wrote nothing at all — the same emptiness
//     check checkPlanGate makes.
//   - FrozenPhases, a straight count of EventPlanApproved records — the same
//     count checkPlanGate derives from the log to check pg.FrozenPhases
//     against.
//
// Everything else PlanGateAssertions can express — OnePlanDigest,
// RebuildableFromLog, GatedAgainstFrozenPlan, Outcomes, ApprovalPrompts,
// CardWhatContains — requires re-deriving an invariant or picking which
// strings matter, the same judgement SystemPromptContains declines to guess.
// Returns nil when the gate ran but nothing ever froze: there is no
// mechanical fact left to state.
func derivePlanGateAssertions(gate []plangateaudit.Content) *bt.PlanGateAssertions {
	if len(gate) == 0 {
		return &bt.PlanGateAssertions{NoRecords: true}
	}
	var frozen int
	for _, g := range gate {
		if g.Event == plangateaudit.EventPlanApproved {
			frozen++
		}
	}
	if frozen == 0 {
		return nil
	}
	return &bt.PlanGateAssertions{FrozenPhases: frozen}
}

// GoldenTrace renders the run's authorization shape for freezing as
// trace.golden.
//
// AGENTS.md says PROMOTE a golden rather than starting with one, because a
// trace frozen before the property assertions are trusted pins whatever the
// code did that day, bug included. A capture is the case that rule does not
// cover: the trace is the record of a REAL run a maintainer already judged
// good, and that judgement is the promotion. Freezing it here is the same
// act, performed by the person who chose the session.
//
// This calls bt.TraceLines and joins it EXACTLY the way the driver's
// checkGoldenTrace does — see test/e2e/threadrun/driver.go. It must not
// re-render the trace by any other path: two renderings of the same logs is
// how this kind of harness rots, because the golden would then diff against
// its own replay on day one, get regenerated unread, and stop asserting
// anything.
func GoldenTrace(recs Records) []byte {
	return []byte(strings.Join(bt.TraceLinesWithFlow(recs.Gate, recs.Decisions,
		bt.TraceFlow{Audit: recs.Audit, Tags: recs.Tags}), "\n") + "\n")
}

// planGateAskKind values match pipeline.ApprovalAsk.Kind exactly — see the
// "plan_phase", "plan_amendment" case in PublishApproval
// (pkg/agent/runner/host_approval.go), which switches on these same two
// literals. They also equal channelinteractions/categories.PlanPhase and
// .PlanAmendment, but this package deliberately does not import that one:
// its init() registers all 37 production interaction categories
// process-wide, a side effect a pure log-derivation function has no business
// triggering.
const (
	planGateAskPhase     = "plan_phase"
	planGateAskAmendment = "plan_amendment"
)

// DeriveApprovalSettings reads the run's approval records and returns the
// AutoApprove / AutoDeny categories, the identity that decided, and any
// decided approval whose category could not be determined.
//
// Not an assertion — a REPLAY REQUIREMENT. Without it a captured phase that
// waited for a person blocks until the class timeout, and the bundle reads
// as a hang rather than as the pause it actually is.
//
// # Why a fourth return value
//
// approval.Request and approval.Outcome (Records.Approvals, keyed by
// toolUseID) carry no Category field — a tool-call approval's decision is
// durable, but which interaction category asked for it is not. Guessing
// "tool_approval" because that is the only category recorded there today
// would be exactly the kind of invented claim this package refuses to make
// elsewhere (see DeriveAssertions' SystemPromptContains): correct today,
// silently wrong the day a second category starts recording through the same
// path. Every decided toolUseID whose category cannot be read from a durable
// record is returned in `undetermined` instead, so the self-check can raise
// a finding and a human adds it to AutoApprove or AutoDeny by hand, having
// looked at what it actually was.
//
// # Where plan_phase / plan_amendment DO come from
//
// Unlike a tool-call approval, the plan gate's two categories ARE
// self-describing in Records.Gate: a per-call gate record's Handle is empty
// for a plain phase ask and carries the ONE permission an amendment asked to
// add otherwise (see pendingPlanGate.handle's doc comment in
// pkg/agent/runner/host_approval.go). The request record (EventCardBuilt /
// EventAmendmentRequested) is append-only and therefore never updated with
// the eventual decision — it keeps the pessimistic Outcome it was stamped
// with when raised — so resolving it means finding a LATER
// EventPhaseApproved record naming the same phase (or the same Handle, for
// an amendment). A request that never finds one stays at its recorded
// Outcome.
//
// Approved-vs-denied is decided per CATEGORY, not per phase, because that is
// the granularity the replay driver itself uses (test/e2e/threadrun/driver.go
// startAutoApprover matches on category name and checks AutoDeny before
// AutoApprove). A category with even one unresolved-and-denied request is
// reported as denied and left out of AutoApprove entirely: putting it there
// too would make the replay grant something the real run withheld on at
// least one occasion, and the driver's own deny-first check could not tell
// the difference between "always approved" and "approved except once."
//
// Both AutoApprove and AutoDeny are sorted and deduplicated, so a re-capture
// of the same session is byte-identical.
func DeriveApprovalSettings(recs Records) (autoApprove, autoDeny []string, approveAs string, undetermined []string) {
	approvedCats, deniedCats := planGateCategories(recs.Gate)
	approveAs, undetermined = approverAndUndetermined(recs.Approvals)
	return sortedKeys(approvedCats), sortedKeys(deniedCats), approveAs, undetermined
}

// approverAndUndetermined walks every DECIDED approval (Outcome != nil) and
// returns the identity that answered them plus the sorted, deduplicated list
// of toolUseIDs whose category cannot be recovered — see
// DeriveApprovalSettings' doc comment.
//
// An approval with no Outcome yet is skipped entirely, from both returns: it
// was never decided, so there is nothing for a replay to gate and nothing to
// raise a finding about.
func approverAndUndetermined(approvals map[string]approval.Pair) (approveAs string, undetermined []string) {
	counts := map[string]int{}
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(approvals)) {
		pair := approvals[id]
		if pair.Outcome == nil {
			continue
		}
		ids = append(ids, id)
		if pair.Outcome.Approver != "" {
			counts[pair.Outcome.Approver]++
		}
	}
	return pickApprover(counts), ids
}

// pickApprover resolves the single AutoApproveAs field from every decided
// approval's Outcome.Approver.
//
// Unambiguous in the overwhelmingly common case — one person answers every
// approval in a session, and the set collapses to that one subject. A
// multi-approver session cannot be expressed exactly (AutoApproveAs is one
// string), so ties are broken by FREQUENCY, then alphabetically: the
// identity who decided the most wins, and equal counts resolve the same way
// on every re-capture rather than depending on map iteration order.
func pickApprover(counts map[string]int) string {
	var best string
	var bestCount int
	for _, id := range slices.Sorted(maps.Keys(counts)) {
		if counts[id] > bestCount {
			best, bestCount = id, counts[id]
		}
	}
	return best
}

// planGateCategories walks Records.Gate and reports, for "plan_phase" and
// "plan_amendment", whether the category resolved to approved or denied.
// Absent from both means the category made no per-call ask this run — never
// requested, so nothing for a replay to gate.
func planGateCategories(gate []plangateaudit.Content) (approved, denied map[string]bool) {
	phaseReq := map[string]plangateaudit.Content{}
	phaseOK := map[string]bool{}
	amendReq := map[string]plangateaudit.Content{}
	amendOK := map[string]bool{}

	for _, g := range gate {
		switch g.Event {
		case plangateaudit.EventCardBuilt:
			phaseReq[planPhaseGateKey(g)] = g
		case plangateaudit.EventAmendmentRequested:
			if g.Handle != "" {
				amendReq[g.Handle] = g
			}
		case plangateaudit.EventPhaseApproved:
			if g.Handle == "" {
				phaseOK[planPhaseGateKey(g)] = true
			} else {
				amendOK[g.Handle] = true
			}
		}
	}

	approved = map[string]bool{}
	denied = map[string]bool{}
	resolvePlanGateCategory(planGateAskPhase, phaseReq, phaseOK, approved, denied)
	resolvePlanGateCategory(planGateAskAmendment, amendReq, amendOK, approved, denied)
	return approved, denied
}

// resolvePlanGateCategory decides one category's fate from its requests and
// their resolutions.
//
// A request that never resolved (logging mode never asks; a capture taken
// mid-pause) contributes NEITHER outcome — it is not a decision, so it must
// not read as one. Deny wins over approve whenever both occur within the
// category: see DeriveApprovalSettings' doc comment on why "approved except
// once" cannot be expressed by the replay driver's category-level gate.
func resolvePlanGateCategory(
	category string,
	requests map[string]plangateaudit.Content,
	resolved map[string]bool,
	approved, denied map[string]bool,
) {
	var sawApproved, sawDenied bool
	for key, req := range requests {
		switch {
		case resolved[key]:
			sawApproved = true
		case req.Outcome == plangateaudit.OutcomeDenied:
			sawDenied = true
		}
	}
	switch {
	case sawDenied:
		denied[category] = true
	case sawApproved:
		approved[category] = true
	}
}

// planPhaseGateKey identifies a per-call plan-gate request/approval by the
// plan and phase it names, which is the only handle a plain phase ask (as
// opposed to an amendment) carries.
func planPhaseGateKey(g plangateaudit.Content) string {
	idx := "-"
	if g.PhaseIndex != nil {
		idx = strconv.Itoa(int(*g.PhaseIndex))
	}
	return g.PlanDigest + "#" + idx
}

// sortedKeys returns a set's members sorted, or nil for an empty set — never
// an allocated empty slice, so a session that needed no auto-approval at all
// does not acquire an AutoApprove/AutoDeny list it never needed.
func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}
