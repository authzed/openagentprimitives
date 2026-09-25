package steelthread

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/contentguardaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// forToolCallRelation is the link relation every per-call record carries: the
// tool_use block id the record answers. Not a memory entry id, which is why the
// writers record it with an empty Link.Kind.
const forToolCallRelation = "for_tool_call"

// RecordsFromMemory reads one session's durable records into the bag a capture
// folds.
//
// Every failure is returned, never absorbed into an empty field. A Records that
// came back empty because a read failed is indistinguishable from a session
// that did nothing, and the capture built from it would look clean: no turns to
// fold, no decisions to seed, nothing for the self-check to object to. That is
// the exact false green this package exists to refuse.
func RecordsFromMemory(ctx context.Context, m memory.Memory, scope memory.Scope) (Records, error) {
	out := Records{Session: scope.ID}

	var err error
	if out.Turns, err = turn.ReadAll(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read transcript: %w", err)
	}
	if out.SystemPrompts, err = systemprompt.List(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read system prompts: %w", err)
	}
	if out.ToolCatalogs, err = toolcatalog.List(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read tool catalogs: %w", err)
	}
	if out.Gate, err = plangateaudit.List(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read plan-gate audit: %w", err)
	}
	if out.Decisions, out.DecisionsByToolCall, err = readDecisions(ctx, m, scope); err != nil {
		return Records{}, err
	}
	// The flow-control logs the golden also renders. They are read HERE, beside
	// the other two, because GoldenTrace must fold exactly what the replay's
	// checkGoldenTrace folds — a capture that rendered fewer logs would diff
	// against its own replay on day one.
	if out.Audit, err = infoleakageaudit.List(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read info-leakage audit: %w", err)
	}
	if out.Tags, err = pttag.List(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read pt-tags: %w", err)
	}
	if out.RelWrites, err = readRelWrites(ctx, m, scope); err != nil {
		return Records{}, err
	}
	if out.Trigger, err = triggerdelivery.Get(ctx, m, scope); err != nil {
		return Records{}, fmt.Errorf("steelthread: read trigger delivery: %w", err)
	}
	if out.Approvals, err = readApprovals(ctx, m, scope); err != nil {
		return Records{}, err
	}
	if out.TransformedToolUseIDs, err = readTransformedOutputs(ctx, m, scope); err != nil {
		return Records{}, err
	}
	return out, nil
}

// readDecisions reads the authz_decision log OLDEST-FIRST, and re-sorts it here
// rather than trusting the store to have done it.
//
// The order is load-bearing: DeriveAssertions resolves a key's final outcome as
// its LAST write, so a call denied, then approved, then allowed is asserted
// from whichever record came back last. OrderBy is asked for — a store that can
// sort should — but it is a predicate a backend may decline
// (QueryResult.DroppedPredicates says so out loud), and the inmem backend
// ranges a Go map when it does not sort. Sorting again costs nothing and turns
// "usually chronological" into a guarantee, which is what a re-capture of one
// session needs to produce the same assertion twice.
//
// Stable, so two decisions stamped at the same instant keep the store's
// relative order instead of swapping between runs.
//
// It returns the log twice over: flat, and indexed by the tool_use id each
// decision answered. The index is built in the same pass rather than folded
// out of the flat slice afterwards, because the id lives on the ENTRY's
// `for_tool_call` link and not on the Decision — a second pass would have
// nothing left to read it from. See Records.DecisionsByToolCall.
func readDecisions(
	ctx context.Context, m memory.Memory, scope memory.Scope,
) ([]authzdecision.Decision, map[string][]authzdecision.Decision, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{authzdecision.KindName},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("steelthread: read authz decisions: %w", err)
	}
	entries := slices.Clone(res.Entries)
	slices.SortStableFunc(entries, func(a, b memory.Entry) int { return a.CreatedAt.Compare(b.CreatedAt) })

	out := make([]authzdecision.Decision, 0, len(entries))
	byCall := map[string][]authzdecision.Decision{}
	for _, e := range entries {
		var d authzdecision.Decision
		if err := json.Unmarshal(e.Content, &d); err != nil {
			// Refused, not skipped. A decision that vanishes from this list is
			// a decision the golden trace and the authz assertions will not
			// mention, so the bundle would claim less than the run did and
			// nothing would say so.
			//
			// The Kind accessors (toolcatalog.List and its siblings) SKIP-LOG
			// the same condition, and both rules are right for their reader:
			// there, an unreadable row must not wedge a live session that is
			// still serving; here, the whole point of the read is to produce an
			// evidentiary artifact, and a partial one is worse than none.
			return nil, nil, fmt.Errorf("steelthread: decode authz decision %q: %w", e.ID, err)
		}
		out = append(out, d)
		for _, l := range e.Links {
			if l.Relation == forToolCallRelation && l.ID != "" {
				byCall[l.ID] = append(byCall[l.ID], d)
			}
		}
	}
	if len(byCall) == 0 {
		// nil rather than an empty map, so a Records built by hand in a test
		// and one read from a session that linked nothing are the same value.
		byCall = nil
	}
	return out, byCall, nil
}

// readRelWrites reads the session's own relationship writes — the first and
// largest of DeriveSeed's three subtraction classes.
//
// Sorted by entry id so a re-capture emits the same Records; the subtraction
// itself is a set membership test and does not care, but a Records that
// reordered between reads would make its own diff unreadable.
func readRelWrites(ctx context.Context, m memory.Memory, scope memory.Scope) ([]relwritesaudit.Audit, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{relwritesaudit.KindName},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	if err != nil {
		return nil, fmt.Errorf("steelthread: read relationship-write audit: %w", err)
	}
	entries := slices.Clone(res.Entries)
	slices.SortStableFunc(entries, func(a, b memory.Entry) int { return strings.Compare(a.ID, b.ID) })

	out := make([]relwritesaudit.Audit, 0, len(entries))
	for _, e := range entries {
		var a relwritesaudit.Audit
		if err := json.Unmarshal(e.Content, &a); err != nil {
			// Refused for the same reason a decision is: a relationship the
			// session wrote and this read dropped is a tuple that gets SEEDED,
			// which lets the bundle pass with the writer broken.
			return nil, fmt.Errorf("steelthread: decode relationship-write audit %q: %w", e.ID, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// readApprovals groups the scope's approval rows into the request/outcome pairs
// DeriveApprovalSettings reads, keyed by the toolUseID each pair answers.
//
// One query to learn WHICH tool calls were asked about, then approval.ByToolCall
// per id. The second hop is deliberate rather than a fold over the first
// query's rows: ByToolCall owns how a request row is told from an outcome row
// and what to do with one that will not decode, and a second copy of that here
// would be free to drift from it. The id set is a session's approvals, so the
// hop count is small.
func readApprovals(ctx context.Context, m memory.Memory, scope memory.Scope) (map[string]approval.Pair, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{approval.KindName}})
	if err != nil {
		return nil, fmt.Errorf("steelthread: read approvals: %w", err)
	}
	var ids []string
	seen := map[string]bool{}
	for _, e := range res.Entries {
		for _, l := range e.Links {
			if l.Relation != forToolCallRelation || l.ID == "" || seen[l.ID] {
				continue
			}
			seen[l.ID] = true
			ids = append(ids, l.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)

	out := make(map[string]approval.Pair, len(ids))
	for _, id := range ids {
		pair, err := approval.ByToolCall(ctx, m, scope, id)
		if err != nil {
			return nil, fmt.Errorf("steelthread: read approval for tool call %q: %w", id, err)
		}
		out[id] = pair
	}
	return out, nil
}

// readTransformedOutputs gathers the tool calls whose RECORDED RESULT is a
// guard's message rather than the tool's own answer.
//
// This is the field with no single Kind behind it, and the one the self-check
// refuses an empty answer for whenever a guard is configured — because "no
// guard rewrote anything" and "nobody gathered" look identical, and the second
// ships a bundle whose output a replay transforms a SECOND time.
//
// # What counts, and what deliberately does not
//
// Only a record whose verdict landed on the RESULT. A guard that inspected and
// passed changed nothing; so did one that warned; so did one that refused
// BEFORE dispatch, because the tool never produced an output and a replay
// re-evaluates the same arguments to the same refusal.
//
//   - contentguard: action "block" at post_tool_call, AND action "approve" at
//     post_tool_call whose approval was refused or timed out — see
//     approveRewrites, which is the shipped inspector's default path, not an
//     edge case. The pre_tool_call twins of both are excluded.
//   - toolguard: a byte budget on the INBOUND dimension whose action was not
//     warn — the dispatch loop withholds the payload and the transcript keeps
//     the "result was withheld" sentence. The outbound dimension, the rate
//     limits and the breaker events all refuse before the call.
//   - infoleakage: "leakage_denied", the reply refused after the fact. Its
//     siblings are excluded on the evidence: "read_denied" is written ONLY in
//     logging mode (the enforcing-mode denial returns a verdict and writes no
//     audit row at all), and "would_block_leakage" is notice-only by name.
//
// # Paths that rewrite output and are NOT gathered here
//
// Recorded so the next reader does not re-derive the list from the same three
// writers, and so a decision to close one of these is a decision rather than a
// discovery:
//
//   - toolguard's credential halt (pkg/authz/toolguard/credential_halt.go) is a
//     Halt at PostToolCall, so the recorded result IS the halt message — but it
//     carries no Limit, so resultSideByteLimit excludes it. It is left out
//     because a halt terminates the session: a capture of a halted session has
//     larger problems than this one, and flagging it would fire on the very
//     scenario a halt bundle exists to prove.
//   - "leakage_denied" is emitted from BOTH PostToolCall and PreResponse
//     (pkg/authz/hooks/infoleakaudience.go). Only the first rewrote a tool
//     result; at PreResponse the gate refused a reply and no recorded tool
//     output changed. The row carries no point to tell them apart, so a
//     PreResponse denial is gathered too and refuses a capture that was
//     actually faithful. Fail-closed and known; closing it means the writer
//     recording its point.
//
// # Two of the three audits record no tool_use id
//
// contentguard.Event and infoleakageaudit.AuditRecord carry no UseID field.
// contentguardaudit.Content HAS one and it is used when populated — the runner
// does not populate it today, because the Event it copies from has no such
// field — so a content-guard row falls back to the TOOL NAME, which is still a
// handle a reader can act on.
//
// An info-leakage row has neither. Its writers never set Tool: the audience
// gate's AuditRecord.Fields carries {leakedTo, resources, note} and no "tool"
// (infoleakaudience.go), and the runner host fills rec.Tool only from
// Fields["tool"] (host.go), so in production every leakage_denied row keys as
// "(unnamed tool call)" and several in one session collapse into one finding.
// That is a real loss and the fix belongs UPSTREAM in the writer, not in a
// fallback here: this package cannot invent a name the record does not carry.
func readTransformedOutputs(ctx context.Context, m memory.Memory, scope memory.Scope) (map[string]string, error) {
	out := map[string]string{}

	cg, err := contentguardaudit.List(ctx, m, scope)
	if err != nil {
		return nil, fmt.Errorf("steelthread: read content-guard audit: %w", err)
	}
	il, err := infoleakageaudit.List(ctx, m, scope)
	if err != nil {
		return nil, fmt.Errorf("steelthread: read info-leakage audit: %w", err)
	}
	// Both are re-sorted for the same reason readDecisions is: the pairing
	// below is positional in TIME, and infoleakageaudit.List asks for no
	// ordering at all while a backend may decline the ordering contentguard's
	// accessor does ask for.
	cg = slices.Clone(cg)
	slices.SortStableFunc(cg, func(a, b contentguardaudit.Content) int { return a.At.Compare(b.At) })
	il = slices.Clone(il)
	slices.SortStableFunc(il, func(a, b infoleakageaudit.AuditRecord) int { return a.At.Compare(b.At) })

	for _, c := range cg {
		if c.Action != contentGuardBlockAction || c.Point != string(pipeline.PostToolCall) {
			continue
		}
		out[transformKey(c.UseID, c.Tool)] = "content_guard:" + c.Inspector
	}
	maps.Copy(out, approveRewrites(cg, contentInspectionOutcomes(il)))

	tg, err := toolguardaudit.List(ctx, m, scope)
	if err != nil {
		return nil, fmt.Errorf("steelthread: read tool-guard audit: %w", err)
	}
	for _, c := range tg {
		if !resultSideByteLimit(c.Limit) || c.Action == toolguard.ActionWarn.String() {
			continue
		}
		out[transformKey(c.UseID, c.Tool)] = "tool_guard:" + c.Event
	}

	for _, r := range il {
		if r.Kind != infoLeakageDeniedKind {
			continue
		}
		out[transformKey("", r.Tool)] = "info_leakage:" + r.Kind
	}

	if len(out) == 0 {
		// nil rather than an allocated empty map, so a Records built by hand
		// and one read from a store compare equal.
		return nil, nil
	}
	return out, nil
}

// approveRewrites reports the content-guard "approve" rows whose approval did
// NOT release the tool's own output.
//
// This is the shipped default, not a corner. The prompt-injection inspector
// defaults to action=approve at BOTH points, so a flagged result raises an
// ApprovalAsk; when that approval is refused or times out the pipeline executor
// turns the decision into a Deny, and the dispatch loop replaces the tool's
// result with the gate's reason (IsError). The recorded output is then the
// refusal message, a replay would gate it a SECOND time, and the only audit row
// the guard wrote says "approve" — so the block rule above never sees it. The
// META inspection path writes a second "block" row on refusal, which is why the
// hole is specific to the gated pipeline path.
//
// # How a row is paired with its outcome
//
// The two records share no key: the guard's row names the tool and the point,
// and the approval's outcome lands in the info-leakage audit as an
// "approval_resolved" row carrying only {approvalKind, approver, approved}.
// What they share is ORDER, and only within one dispatch. Sequentially the
// executor raises one approval per approve decision and resolves it before the
// next, so the nth approve row is answered by the nth content_inspection
// resolution. Rows from BOTH points consume a resolution, because a pre-call
// approve raises one too and skipping it would shift every later pairing by one.
//
// An approve row with no resolution left to pair — a capture taken mid-pause, or
// a publish failure that halted before the outcome was written — counts as a
// rewrite, because "the output was released" is a claim and nothing here can
// make it.
//
// # Where the positional pairing is WRONG, and in the dangerous direction
//
// A turn's tool_use blocks are dispatched CONCURRENTLY — one goroutine each,
// with the PostToolCall executor inside every one (see the runner's dispatch
// loop, and host_approval's own notes on multiple tool goroutines sharing the
// Loop and on concurrent approval-gated calls in one turn). So two gated tools
// in a single turn produce approve(A), approve(B) in guard order and their
// resolutions in the order a HUMAN answered them. Answer B first and the rows
// read approve(A), approve(B), resolved(true), resolved(false): A pairs with
// true and is NOT flagged, though its recorded result is the guard's refusal
// text. That is an UNDER-flag — a bundle shipped with a refusal baked in as the
// tool's answer — not the harmless over-flag this comment once claimed.
//
// It is left standing because closing it is an upstream change: the
// "approval_resolved" record would have to carry the request id (or the tool
// name plus point) that the guard's row already implies, which turns this from
// an index into a key lookup and removes the ordering assumption entirely.
// Until then, do not extend this pairing — to a third audit, or to any
// additional inference — on the belief that a misalignment is safe. It is safe
// only for a turn that dispatched one tool at a time.
func approveRewrites(cg []contentguardaudit.Content, outcomes []bool) map[string]string {
	out := map[string]string{}
	next := 0
	for _, c := range cg {
		if c.Action != contentGuardApproveAction {
			continue
		}
		approved := false
		if next < len(outcomes) {
			approved = outcomes[next]
		}
		next++
		if c.Point != string(pipeline.PostToolCall) || approved {
			continue
		}
		out[transformKey(c.UseID, c.Tool)] = "content_guard:" + c.Inspector + " (approval refused)"
	}
	return out
}

// contentInspectionOutcomes returns, oldest first, whether each
// content_inspection approval the session raised was approved.
//
// Read from the "approval_resolved" rows the pipeline executor emits for every
// approval kind, filtered to this one by Details["approvalKind"] — the same
// string contentguard's adapter stamps on the ApprovalAsk it builds. A row
// whose "approved" detail is absent or unparseable reads as NOT approved: the
// only reason to consult it is to establish that the output was released, and
// an unreadable row establishes nothing.
func contentInspectionOutcomes(il []infoleakageaudit.AuditRecord) []bool {
	var out []bool
	for _, r := range il {
		if r.Kind != approvalResolvedKind || r.Details[approvalKindDetail] != contentInspectionApprovalKind {
			continue
		}
		out = append(out, r.Details[approvedDetail] == "true")
	}
	return out
}

const (
	// contentGuardBlockAction and contentGuardApproveAction are the strings
	// contentguard's adapter emits for a Block and an Approve finding (its
	// emit calls pass them as literals; the audit stores the action as text).
	// Comparisons against a recorded value, not branches on a kind.
	contentGuardBlockAction   = "block"
	contentGuardApproveAction = "approve"

	// infoLeakageDeniedKind is the info-leakage audit subtype meaning a reply
	// was refused after the tool had already produced its result.
	infoLeakageDeniedKind = "leakage_denied"

	// approvalResolvedKind, approvalKindDetail, approvedDetail and
	// contentInspectionApprovalKind name the one durable record of an approval
	// OUTCOME that a content-inspection gate leaves behind. The runner host
	// routes pipeline's "approval_resolved" AuditRecord into the info-leakage
	// kind with these three string-keyed details; the ask's own Kind is the
	// literal contentguard's adapter builds it with.
	//
	// A content_inspection approval writes NO approval memory row: only the
	// "tool_call" kind reaches recordApprovalOutcome, so Records.Approvals —
	// which is keyed by tool_use id — cannot answer this question at all.
	approvalResolvedKind          = "approval_resolved"
	approvalKindDetail            = "approvalKind"
	approvedDetail                = "approved"
	contentInspectionApprovalKind = "content_inspection"
)

// resultSideByteLimit reports whether a toolguard byte-budget dimension applies
// to what came BACK. The outbound dimension bounds the arguments, and a call
// refused for its arguments never produced an output to rewrite.
func resultSideByteLimit(dim string) bool {
	return dim == "ingress" || dim == "ui_ingress"
}

// transformKey is the map key one gathered record gets: its tool_use id when
// the audit recorded one, the tool's name when it recorded that, and a fixed
// placeholder when it recorded neither. See readTransformedOutputs on which
// writers reach which rung, and why the missing name is an upstream gap rather
// than something this package can fill.
func transformKey(useID, tool string) string {
	if useID != "" {
		return useID
	}
	if tool != "" {
		return tool
	}
	return "(unnamed tool call)"
}
