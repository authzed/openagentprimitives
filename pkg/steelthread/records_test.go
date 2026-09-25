package steelthread_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/contentguardaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// captureScope is the one scope every case in this file reads and writes.
var captureScope = memory.Scope{Kind: "session", ID: "default/demo-session"}

func writeCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "steelthread-test")
}

// reversingMemory wraps a Memory and hands back every Query result in REVERSE
// order.
//
// Not a contrivance: memory.Query.OrderBy is a predicate a backend may decline
// (QueryResult.DroppedPredicates exists for exactly that), and the inmem
// backend ranges a Go map when it does not sort. A read whose chronology
// depends on the backend honoring OrderBy is a read whose chronology is not
// guaranteed, so the ordering RecordsFromMemory promises has to hold against a
// backend that returns rows in any order at all.
type reversingMemory struct{ memory.Memory }

func (r reversingMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	res, err := r.Memory.Query(ctx, q)
	if err != nil {
		return res, err
	}
	slices.Reverse(res.Entries)
	return res, nil
}

// recordDecisionAt writes one authz_decision stamped at ts. authzdecision.Record
// stamps time.Now() itself, so the entry is written directly here — the point of
// the case is to control CreatedAt.
func recordDecisionAt(t *testing.T, m memory.Memory, ts time.Time, d authzdecision.Decision) {
	t.Helper()
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	_, err = m.Put(writeCtx(), memory.Entry{
		Scope:     captureScope,
		Kind:      authzdecision.KindName,
		ID:        memory.NewID(authzdecision.Kind{}),
		CreatedAt: ts,
		Tags:      []string{"outcome:" + d.Outcome},
		Content:   raw,
	})
	require.NoError(t, err)
}

// TestRecordsFromMemory_OrdersDecisionsChronologically is R2.
//
// DeriveAssertions resolves "final outcome = last write", so the ORDER of
// Records.Decisions decides what the bundle asserts about any key checked more
// than once — which is exactly the denied-then-approved-then-allowed sequence
// this feature exists to capture. Left to the backend's iteration order the
// answer is nondeterministic, and a re-capture of one session produces a
// different assertion each time.
func TestRecordsFromMemory_OrdersDecisionsChronologically(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

	// Written newest-first, then reversed again by the wrapper, so neither the
	// insertion order nor the backend's own order is chronological.
	recordDecisionAt(t, m, base.Add(2*time.Minute), authzdecision.Decision{
		ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list",
		Outcome: authzdecision.OutcomeAllowed, Message: "third",
	})
	recordDecisionAt(t, m, base, authzdecision.Decision{
		ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list",
		Outcome: authzdecision.OutcomeDenied, Message: "first",
	})
	recordDecisionAt(t, m, base.Add(time.Minute), authzdecision.Decision{
		ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list",
		Outcome: authzdecision.OutcomeDenied, Message: "second",
	})

	got, err := steelthread.RecordsFromMemory(writeCtx(), reversingMemory{m}, captureScope)
	require.NoError(t, err)

	require.Len(t, got.Decisions, 3)
	assert.Equal(t, []string{"first", "second", "third"},
		[]string{got.Decisions[0].Message, got.Decisions[1].Message, got.Decisions[2].Message},
		"decisions must come back oldest-first, whatever order the backend returned them in")

	// The consequence, stated where it bites: last write wins, so the run reads
	// as ALLOWED. Unsorted, this assertion is whatever the map iteration
	// happened to produce.
	folded, err := steelthread.Fold(got, steelthread.FoldOptions{})
	require.NoError(t, err)
	as := steelthread.DeriveAssertions(got, folded)
	require.NotNil(t, as.Authz)
	assert.Equal(t, authzdecision.OutcomeAllowed, as.Authz.Decisions["widget_catalog:wc1#list"])
}

// TestRecordsFromMemory_GathersTheTranscriptAndTheTrigger pins the plain read
// path: the turns come back in transcript order, and a session nobody typed
// into carries its opening delivery.
func TestRecordsFromMemory_GathersTheTranscriptAndTheTrigger(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := writeCtx()
	app := turn.NewAppender(m, captureScope)
	require.NoError(t, app.Append(ctx, userText(0, "list the widgets")))
	require.NoError(t, app.Append(ctx, assistantCall(1, "tu_1", "demoforge_list_widgets", `{}`)))

	got, err := steelthread.RecordsFromMemory(ctx, reversingMemory{m}, captureScope)
	require.NoError(t, err)
	assert.Equal(t, "default/demo-session", got.Session)
	require.Len(t, got.Turns, 2)
	assert.Equal(t, 0, got.Turns[0].Index)
	assert.Equal(t, 1, got.Turns[1].Index)
	assert.Nil(t, got.Trigger, "a session a person typed into has no trigger delivery, and that absence is meaningful")
}

// TestRecordsFromMemory_GathersOnlyGuardRecordsThatREWROTETheOutput is R5.
//
// Every entry in TransformedToolUseIDs becomes a HARD finding, so a gather that
// counted a guard which merely INSPECTED would refuse every capture of every
// guarded agent. A gather that counted nothing is worse in the other direction:
// with a guard configured, an empty map is indistinguishable from "nobody
// looked", which is why SelfCheck refuses that too.
func TestRecordsFromMemory_GathersOnlyGuardRecordsThatREWROTETheOutput(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := writeCtx()

	// The three content-guard rows carry NO UseID, because no writer sets one:
	// contentguard.Event has no such field, so every real row of this kind keys
	// by TOOL NAME. Distinct tools per row, since that key is all they have.
	//
	// Inspected and passed: the output the transcript holds is the tool's own.
	require.NoError(t, contentguardaudit.Record(ctx, m, captureScope, contentguardaudit.Content{
		Inspector: "url-allowlist", Action: "pass", Tool: "demoforge_list_widgets",
		Point: "post_tool_call",
	}))
	// Blocked BEFORE the call: the tool never ran, so no output of its was
	// rewritten — replay re-evaluates the same arguments and blocks again.
	require.NoError(t, contentguardaudit.Record(ctx, m, captureScope, contentguardaudit.Content{
		Inspector: "url-allowlist", Action: "block", Tool: "demoforge_fetch",
		Point: "pre_tool_call",
	}))
	// Blocked AFTER the call: the recorded result is the guard's message, not
	// the tool's answer. THIS is the one a replay would transform a second time.
	require.NoError(t, contentguardaudit.Record(ctx, m, captureScope, contentguardaudit.Content{
		Inspector: "secret-scan", Action: "block", Tool: "demoforge_read_secrets",
		Point: "post_tool_call",
	}))
	// A byte budget that only warned: the payload flowed through untouched.
	require.NoError(t, toolguardaudit.Record(ctx, m, captureScope, toolguardaudit.Content{
		Event: "guard_warn", Tool: "demoforge_dump", UseID: "tu_warn",
		Limit: "ingress", Action: "warn",
	}))
	// A byte budget that denied: "the result exceeded the limit and was
	// withheld" — the transcript holds that sentence, not the rows.
	require.NoError(t, toolguardaudit.Record(ctx, m, captureScope, toolguardaudit.Content{
		Event: "ingress_limit_hit", Tool: "demoforge_dump", UseID: "tu_ingress",
		Limit: "ingress", Action: "deny",
	}))
	// An OUTBOUND byte budget: the arguments were too big and the call never
	// dispatched, so there is no recorded output to have been rewritten.
	require.NoError(t, toolguardaudit.Record(ctx, m, captureScope, toolguardaudit.Content{
		Event: "egress_limit_hit", Tool: "demoforge_upload", UseID: "tu_egress",
		Limit: "egress", Action: "deny",
	}))
	// A read the leakage gate merely observed (logging mode records this; the
	// enforcing-mode denial writes no audit row at all).
	require.NoError(t, infoleakageaudit.Append(ctx, m, captureScope, infoleakageaudit.AuditRecord{
		Kind: "read_denied", Tool: "demoforge_read_notes",
	}))
	// A reply the leakage gate refused after the fact. Written exactly as
	// production writes it: NO Tool. The audience gate's AuditRecord carries
	// {leakedTo, resources, note} and the runner host fills rec.Tool only from
	// a "tool" field that is never present, so every leakage_denied row in a
	// real session reaches the gather with no handle at all. Supplying one here
	// would pin a property that never holds.
	require.NoError(t, infoleakageaudit.Append(ctx, m, captureScope, infoleakageaudit.AuditRecord{
		Kind: "leakage_denied",
	}))

	got, err := steelthread.RecordsFromMemory(ctx, m, captureScope)
	require.NoError(t, err)

	assert.Equal(t, "content_guard:secret-scan", got.TransformedToolUseIDs["demoforge_read_secrets"],
		"a content-guard row has no tool_use id in production, so the tool name is its key")
	assert.Equal(t, "tool_guard:ingress_limit_hit", got.TransformedToolUseIDs["tu_ingress"],
		"a toolguard row DOES carry the tool_use id (its Event has the field), so it keys by that")
	assert.Equal(t, "info_leakage:leakage_denied", got.TransformedToolUseIDs["(unnamed tool call)"],
		"a leakage_denied row names neither a tool_use id nor a tool, so it can only key as the "+
			"placeholder; naming it is an upstream fix in the writer, not a fallback here")

	for _, notRewritten := range []string{
		"demoforge_list_widgets", "demoforge_fetch", "tu_warn", "tu_egress",
	} {
		assert.NotContains(t, got.TransformedToolUseIDs, notRewritten,
			"a guard that inspected, warned, or blocked BEFORE the call rewrote no recorded output")
	}
	assert.NotContains(t, got.TransformedToolUseIDs, "demoforge_read_notes",
		"read_denied is written only in logging mode, where the result flows through unchanged")
}

// TestRecordsFromMemory_ReportsAReadFailure pins that a memory that cannot
// answer produces an error rather than an empty Records. An empty Records reads
// as a clean capture of a session that did nothing — the exact shape of a false
// green this whole package exists to refuse.
func TestRecordsFromMemory_ReportsAReadFailure(t *testing.T) {
	_, err := steelthread.RecordsFromMemory(writeCtx(), failingMemory{}, captureScope)
	require.Error(t, err)
}

type failingMemory struct{ memory.Memory }

func (failingMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, assertAnError{}
}

// approvalResolved is the durable record of an approval OUTCOME as the runner
// host writes it: the pipeline executor's "approval_resolved" AuditRecord,
// routed into the info-leakage kind with string-keyed details. A
// content_inspection approval writes nothing to the approval kind, so this is
// the only place its answer is recorded.
func approvalResolved(t *testing.T, m memory.Memory, at time.Time, kind string, approved bool) {
	t.Helper()
	decision := "false"
	if approved {
		decision = "true"
	}
	require.NoError(t, infoleakageaudit.Append(writeCtx(), m, captureScope, infoleakageaudit.AuditRecord{
		At:      at,
		Kind:    "approval_resolved",
		Details: map[string]string{"approvalKind": kind, "approved": decision, "approver": "user:alice"},
	}))
}

// contentGuardRow appends one content-guard audit entry at a fixed instant, so
// the chronological pairing below is driven by the fixture rather than by how
// fast the test runs.
//
// NO UseID, because no writer sets one: contentguard.Event has no such field,
// so the runner cannot populate contentguardaudit.Content.UseID and every real
// row keys by TOOL NAME. Supplying an id here would exercise a branch
// production never reaches and skip the one it always takes — and would carry
// the collision that comes with it, since two calls to the same tool in one
// session share a key. The cases below therefore give each row a distinct tool.
func contentGuardRow(t *testing.T, m memory.Memory, at time.Time, action, tool, point string) {
	t.Helper()
	require.NoError(t, contentguardaudit.Record(writeCtx(), m, captureScope, contentguardaudit.Content{
		At: at, Inspector: "prompt-injection", Action: action, Tool: tool, Point: point,
	}))
}

// TestRecordsFromMemory_AnApproveWhoseApprovalWasRefusedIsARewrite covers the
// contentguard path that ships ON by default.
//
// The prompt-injection inspector's default action is "approve" at both points,
// so a flagged result raises an approval rather than blocking. When that
// approval is REFUSED the executor turns it into a Deny and the dispatch loop
// replaces the tool's result with the gate's reason — but the only audit row
// the guard wrote still says "approve". A gather that keyed on "block" alone
// would ship a bundle whose recorded output a replay gates a second time.
//
// Both directions, because either one alone is satisfiable by a rule that is
// wrong: flag everything, or flag nothing.
func TestRecordsFromMemory_AnApproveWhoseApprovalWasRefusedIsARewrite(t *testing.T) {
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// approved is the answer the paired content_inspection approval got.
		// nil means no approval_resolved row was ever written.
		approved *bool
		point    string
		wantFlag bool
		wantWhy  string
	}{
		{
			name:     "refused at post_tool_call: the recorded result is the gate's message",
			approved: ptr(false), point: "post_tool_call", wantFlag: true,
			wantWhy: "content_guard:prompt-injection (approval refused)",
		},
		{
			name:     "approved at post_tool_call: the tool's own output was released",
			approved: ptr(true), point: "post_tool_call", wantFlag: false,
		},
		{
			// Fail-closed. A capture taken mid-pause, or a publish failure that
			// halted before the outcome was written, cannot establish that the
			// output was released — and "released" is the claim being made.
			name:     "no outcome recorded at all: cannot claim the output was released",
			approved: nil, point: "post_tool_call", wantFlag: true,
			wantWhy: "content_guard:prompt-injection (approval refused)",
		},
		{
			// The arguments were gated, not the result. The tool never ran, and
			// a replay re-evaluates the same arguments to the same refusal.
			name:     "refused at pre_tool_call: no output of the tool's was rewritten",
			approved: ptr(false), point: "pre_tool_call", wantFlag: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := memory.NewLocal(inmem.NewBackend())
			contentGuardRow(t, m, base, "approve", "demoforge_read_notes", tc.point)
			if tc.approved != nil {
				approvalResolved(t, m, base.Add(time.Second), "content_inspection", *tc.approved)
			}

			got, err := steelthread.RecordsFromMemory(writeCtx(), m, captureScope)
			require.NoError(t, err)

			// Keyed by tool name, which is the only key a content-guard row
			// ever has in production.
			if !tc.wantFlag {
				assert.NotContains(t, got.TransformedToolUseIDs, "demoforge_read_notes")
				return
			}
			assert.Equal(t, tc.wantWhy, got.TransformedToolUseIDs["demoforge_read_notes"])
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestRecordsFromMemory_ApproveRowsPairWithOutcomesInOrder pins the pairing
// itself, and specifically that a PRE-call approve row consumes a resolution
// even though it can never be flagged.
//
// The guard's row and the approval's outcome share no key — only order. A
// pre-call approve raises an approval of its own, so a walk that consumed
// outcomes for post-call rows ALONE would answer each post-call gate with some
// earlier gate's decision.
//
// The first case is the one that discriminates, and it discriminates in the
// direction that matters: a refused pre-call gate followed by an APPROVED
// post-call gate. Skip the pre-call row's outcome and the post-call gate
// inherits the refusal, so a capture whose output was genuinely released is
// refused — a false positive nobody can act on. The second case is the mirror,
// and it is the one a naive walk happens to get right; both are here so the
// pairing cannot be satisfied by a rule that only shifts one way.
func TestRecordsFromMemory_ApproveRowsPairWithOutcomesInOrder(t *testing.T) {
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	type row struct {
		action, tool, point string
		// resolvedApproved is the content_inspection outcome recorded right
		// after this row, when the row raised an approval.
		resolvedApproved *bool
	}
	cases := []struct {
		name     string
		rows     []row
		flagged  []string
		released []string
	}{
		{
			name: "a refused PRE-call gate must not be inherited by an approved post-call gate",
			rows: []row{
				{action: "approve", tool: "demoforge_args", point: "pre_tool_call", resolvedApproved: ptr(false)},
				{action: "approve", tool: "demoforge_read", point: "post_tool_call", resolvedApproved: ptr(true)},
			},
			released: []string{"demoforge_args", "demoforge_read"},
		},
		{
			name: "an approved PRE-call gate must not release a refused post-call gate",
			rows: []row{
				{action: "approve", tool: "demoforge_args", point: "pre_tool_call", resolvedApproved: ptr(true)},
				{action: "approve", tool: "demoforge_read", point: "post_tool_call", resolvedApproved: ptr(false)},
			},
			flagged:  []string{"demoforge_read"},
			released: []string{"demoforge_args"},
		},
		{
			name: "three gates in a row keep their own answers",
			rows: []row{
				{action: "approve", tool: "demoforge_one", point: "post_tool_call", resolvedApproved: ptr(true)},
				{action: "approve", tool: "demoforge_two", point: "pre_tool_call", resolvedApproved: ptr(false)},
				{action: "approve", tool: "demoforge_three", point: "post_tool_call", resolvedApproved: ptr(false)},
			},
			flagged:  []string{"demoforge_three"},
			released: []string{"demoforge_one", "demoforge_two"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := memory.NewLocal(inmem.NewBackend())
			at := base
			for _, r := range tc.rows {
				contentGuardRow(t, m, at, r.action, r.tool, r.point)
				at = at.Add(time.Second)
				if r.resolvedApproved != nil {
					approvalResolved(t, m, at, "content_inspection", *r.resolvedApproved)
					at = at.Add(time.Second)
				}
			}

			// Read through the reversing wrapper so the chronological pairing
			// is the code's doing and not the backend's.
			got, err := steelthread.RecordsFromMemory(writeCtx(), reversingMemory{m}, captureScope)
			require.NoError(t, err)

			for _, tool := range tc.flagged {
				assert.Contains(t, got.TransformedToolUseIDs, tool,
					"%s: this gate's own outcome refused it", tool)
			}
			for _, tool := range tc.released {
				assert.NotContains(t, got.TransformedToolUseIDs, tool,
					"%s: flagging this one refuses a capture whose output was released", tool)
			}
		})
	}
}

// TestRecordsFromMemory_ANonContentInspectionApprovalIsNotConsumed pins the
// filter on approvalKind. Every approval kind writes an approval_resolved row
// through the same executor, so a walk that consumed a tool_call approval's
// outcome would shift the content-inspection pairing by one and answer a gate
// with a decision about something else.
func TestRecordsFromMemory_ANonContentInspectionApprovalIsNotConsumed(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	// A tool-call approval that was DENIED, resolved before the gate below.
	approvalResolved(t, m, base, "tool_call", false)
	contentGuardRow(t, m, base.Add(time.Second), "approve", "demoforge_read_notes", "post_tool_call")
	approvalResolved(t, m, base.Add(2*time.Second), "content_inspection", true)

	got, err := steelthread.RecordsFromMemory(writeCtx(), m, captureScope)
	require.NoError(t, err)
	assert.NotContains(t, got.TransformedToolUseIDs, "demoforge_read_notes",
		"the content-inspection gate was approved; a tool_call denial is a decision about another thing")
}

// TestRecordsFromMemory_KeepsTheToolCallEachDecisionAnswered pins the link
// readDecisions used to discard.
//
// authzdecision.Record writes the tool_use block id as a `for_tool_call` link
// with an empty Kind, and the Decision struct itself has no field for it. Fold
// needs that correspondence to tell an error result the GATE authored — a call
// that never reached the MCP server — from one the server itself returned, so
// dropping it left the capture unable to classify the very sessions this
// feature exists for.
//
// Order per id is preserved for the same reason Decisions' order is: a call
// denied and then re-checked after an approval records both, and only the LAST
// one says whether the tool ran.
func TestRecordsFromMemory_KeepsTheToolCallEachDecisionAnswered(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

	recordDecisionForCallAt(t, m, base.Add(time.Minute), "tu_1", authzdecision.Decision{
		ResourceType: "widget", ResourceID: "w1", Permission: "read",
		Outcome: authzdecision.OutcomeAllowed, Message: "after approval",
	})
	recordDecisionForCallAt(t, m, base, "tu_1", authzdecision.Decision{
		ResourceType: "widget", ResourceID: "w1", Permission: "read",
		Outcome: authzdecision.OutcomeDenied, Message: "permission denied: alice does not have read on widget:w1",
	})
	recordDecisionForCallAt(t, m, base.Add(2*time.Minute), "tu_2", authzdecision.Decision{
		ResourceType: "widget", ResourceID: "w2", Permission: "read",
		Outcome: authzdecision.OutcomeDenied, Message: "permission denied: alice does not have read on widget:w2",
	})

	got, err := steelthread.RecordsFromMemory(writeCtx(), reversingMemory{m}, captureScope)
	require.NoError(t, err)

	require.Len(t, got.DecisionsByToolCall, 2, "one entry per tool_use id the log linked to")
	require.Len(t, got.DecisionsByToolCall["tu_1"], 2)
	assert.Equal(t,
		[]string{"permission denied: alice does not have read on widget:w1", "after approval"},
		[]string{got.DecisionsByToolCall["tu_1"][0].Message, got.DecisionsByToolCall["tu_1"][1].Message},
		"a call checked twice keeps both decisions, oldest first, whatever order the backend returned")
	require.Len(t, got.DecisionsByToolCall["tu_2"], 1)
	assert.Equal(t, authzdecision.OutcomeDenied, got.DecisionsByToolCall["tu_2"][0].Outcome)

	assert.Len(t, got.Decisions, 3, "the flat log is unchanged; the index is additive")
}

// TestRecordsFromMemory_DecisionWithNoToolCallLinkIsNotIndexed covers the
// writer's own honest gap: recordAuthzDecision reads the tool_use id off the
// dispatch context and records an EMPTY one when that lookup fails. An empty
// key correlates to nothing, so indexing it would let one unattributable
// decision speak for a call it may have nothing to do with.
func TestRecordsFromMemory_DecisionWithNoToolCallLinkIsNotIndexed(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ts := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

	recordDecisionForCallAt(t, m, ts, "", authzdecision.Decision{
		ResourceType: "widget", ResourceID: "w1", Permission: "read",
		Outcome: authzdecision.OutcomeDenied, Message: "denied",
	})

	got, err := steelthread.RecordsFromMemory(writeCtx(), m, captureScope)
	require.NoError(t, err)
	assert.Empty(t, got.DecisionsByToolCall, "an unattributable decision indexes under no call")
	assert.Len(t, got.Decisions, 1, "but it is still part of the log the assertions and the golden read")
}

// recordDecisionForCallAt writes one authz_decision at ts, linked to useID the
// way authzdecision.Record links it. Written directly rather than through the
// accessor because the accessor stamps time.Now() and the cases above turn on
// CreatedAt.
func recordDecisionForCallAt(
	t *testing.T, m memory.Memory, ts time.Time, useID string, d authzdecision.Decision,
) {
	t.Helper()
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	_, err = m.Put(writeCtx(), memory.Entry{
		Scope:     captureScope,
		Kind:      authzdecision.KindName,
		ID:        memory.NewID(authzdecision.Kind{}),
		CreatedAt: ts,
		Links:     []memory.Link{{Relation: "for_tool_call", ID: useID}},
		Tags:      []string{"outcome:" + d.Outcome},
		Content:   raw,
	})
	require.NoError(t, err)
}
