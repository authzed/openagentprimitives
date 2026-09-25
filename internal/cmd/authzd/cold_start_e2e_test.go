package main

// cold_start_e2e_test.go
//
// End-to-end integration test for the cold-start scope feature (spec §8).
//
// ColdStartHandler is the unit of integration: it wires the extractor LLM
// (untrusted input) → deterministic classification (ClassifySkipped +
// DetectCaveats) → composer LLM (text-free) → approver decision → scope apply
// per the chosen action → cold_start_task instruction for the runner. Because
// ColdStartHandler lives in package main (internal/cmd/authzd), this E2E is a package
// main test (Option B) reusing the existing internal/cmd/authzd fakes
// (fakeColdStartExtractor, fakeGranter, fakeMetaComposer, newColdStartHarness).
//
// The complementary library-level seams are covered elsewhere:
//   - cold_start_handler_test.go   — per-action unit coverage of Handle
//   - pkg/authz/scope/classify_test.go, caveats_test.go — pure classify/caveat
//   - pkg/agent/runner/loop_coldstart_test.go — runner turn-0 placement
//     (approved_cleaned → cleaned text, approved_original/ran_without_scope →
//     raw prompt, denied → no turn). That suite already drives runColdStart for
//     every status, so the runner-side scenario is NOT duplicated here.
//
// This file exercises Handle end-to-end across all five approver actions plus
// autoApply, extractor fail-open, and the two envelope-classification headline
// cases: an in-envelope hard-deny narrowing SURVIVES classification and reaches
// the granter ("do not read ENG"), while an out-of-envelope item in the same
// request is DROPPED (partial apply).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// e2eDecide returns a ColdStartDecide that always returns the given action,
// recording that it was invoked. A nil *bool means "must not be called" — the
// returned func t.Fatal's if reached.
func e2eDecide(t *testing.T, action string, called *bool) ColdStartDecide {
	t.Helper()
	return func(context.Context, scope.MetaagentOutput, string) (string, error) {
		if called == nil {
			t.Fatal("decider must not be called for this scenario")
		}
		*called = true
		return action, nil
	}
}

// hasDisallowTool reports whether the Layer-2 session_scope memory doc records
// tool in its tool-deny set.
func hasDisallowTool(t *testing.T, m memory.Memory, sc memory.Scope, tool string) bool {
	t.Helper()
	got, found, err := sessionscope.Get(approvedCtx(), m, sc)
	require.NoError(t, err)
	if !found {
		return false
	}
	for _, d := range got.Tools.Deny {
		if d == tool {
			return true
		}
	}
	return false
}

// hasDisallowResource reports whether the Layer-2 session_scope memory doc
// hard-denies rtype:id.
func hasDisallowResource(t *testing.T, m memory.Memory, sc memory.Scope, rtype, id string) bool {
	t.Helper()
	got, found, err := sessionscope.Get(approvedCtx(), m, sc)
	require.NoError(t, err)
	if !found {
		return false
	}
	return got.ResourceDisallowed(rtype, id)
}

// TestE2E_ColdStart_AllActions drives ColdStartHandler.Handle end-to-end for
// each of the five spec §8 outcomes plus autoApply and the extractor
// fail-open, asserting the resulting cold_start_task status, the cleaned-text
// instruction, the granter (SpiceDB) writes, the session_scope memory state,
// the metaagent_thread notice, and whether the decider ran.
func TestE2E_ColdStart_AllActions(t *testing.T) {
	// The proposed extraction: hard-deny the search tool (an in-envelope
	// narrowing) and clean "do not read ENG" out of the task.
	hardDenyTool := scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}

	cases := []struct {
		name        string
		ext         *fakeColdStartExtractor
		action      string // ignored when autoApply
		autoApply   bool
		deciderRuns bool // expect the decider to have been called

		wantStatus         string
		wantCleaned        string
		wantToolDisallowed bool // granter wrote disallowed_tool linear.search_issues
		wantScopeWritten   bool // session_scope doc present
		wantNotice         bool
		wantHandleErr      bool // Handle returns an error (extractor failure → fail closed)
	}{
		{
			name:               "approve_cleaned: status=approved_cleaned, cleaned text, scope applied, notice",
			ext:                &fakeColdStartExtractor{out: hardDenyTool},
			action:             ColdStartApproveCleaned,
			deciderRuns:        true,
			wantStatus:         coldstarttask.StatusApprovedCleaned,
			wantCleaned:        "summarize L-140",
			wantToolDisallowed: true,
			wantScopeWritten:   true,
			wantNotice:         true,
		},
		{
			name:               "approve_original: status=approved_original, no cleaned text, scope applied",
			ext:                &fakeColdStartExtractor{out: hardDenyTool},
			action:             ColdStartApproveOriginal,
			deciderRuns:        true,
			wantStatus:         coldstarttask.StatusApprovedOriginal,
			wantCleaned:        "",
			wantToolDisallowed: true,
			wantScopeWritten:   true,
			wantNotice:         true,
		},
		{
			name:        "run_without_scope: status=ran_without_scope, no granter writes, no session_scope",
			ext:         &fakeColdStartExtractor{out: hardDenyTool},
			action:      ColdStartRunWithoutScope,
			deciderRuns: true,
			wantStatus:  coldstarttask.StatusRanWithoutScope,
			wantNotice:  true,
		},
		{
			name:        "deny: status=denied, no scope, notice posted",
			ext:         &fakeColdStartExtractor{out: hardDenyTool}, // non-empty delta → decider runs (not the no-op path)
			action:      ColdStartDeny,
			deciderRuns: true,
			wantStatus:  coldstarttask.StatusDenied,
			wantNotice:  true,
		},
		{
			name:               "autoApply: decider NOT called, status=approved_cleaned, scope applied, confirmation notice",
			ext:                &fakeColdStartExtractor{out: hardDenyTool},
			autoApply:          true,
			deciderRuns:        false,
			wantStatus:         coldstarttask.StatusApprovedCleaned,
			wantCleaned:        "summarize L-140",
			wantToolDisallowed: true,
			wantScopeWritten:   true,
			wantNotice:         true,
		},
		{
			name:          "extractor error: FAIL CLOSED to scope_review_failed, notice posted, decider NOT called",
			ext:           &fakeColdStartExtractor{err: assertError("llm down")},
			deciderRuns:   false,
			wantStatus:    coldstarttask.StatusScopeReviewFailed,
			wantNotice:    true,
			wantHandleErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := approvedCtx()
			h, m, sc := newColdStartHarness(t, tc.ext)
			req := coldStartReq()
			req.AutoApply = tc.autoApply

			var called bool
			var decide ColdStartDecide
			if tc.deciderRuns {
				decide = e2eDecide(t, tc.action, &called)
			} else {
				decide = e2eDecide(t, "", nil) // t.Fatal if invoked
			}

			herr := h.Handle(ctx, sc, "ns/a", req, decide)
			if tc.wantHandleErr {
				require.Error(t, herr, "extractor failure must surface (fail closed)")
			} else {
				require.NoError(t, herr)
			}

			assert.Equal(t, tc.deciderRuns, called, "decider invocation")

			cst, found, err := coldstarttask.Get(ctx, m, sc)
			require.NoError(t, err)
			require.True(t, found, "cold_start_task must be written for every outcome")
			assert.Equal(t, tc.wantStatus, cst.Status)
			assert.Equal(t, tc.wantCleaned, cst.CleanedText, "cleaned-text instruction for the runner")

			assert.Equal(t, tc.wantToolDisallowed, hasDisallowTool(t, m, sc, "linear.search_issues"),
				"Layer-2 memory hard-deny tool")

			_, scopeFound, err := sessionscope.Get(ctx, m, sc)
			require.NoError(t, err)
			assert.Equal(t, tc.wantScopeWritten, scopeFound, "session_scope presence")

			assert.Equal(t, tc.wantNotice, coldStartHasNotice(t, m, sc), "metaagent_thread notice")
		})
	}
}

// TestE2E_ColdStart_InEnvelopeHardDenyKept is the "do not read ENG" headline
// case: a hard-deny narrowing whose resource type IS declared in the envelope
// survives ClassifySkipped and reaches the granter as a sticky disallow tuple
// — it is APPLIED, not dropped. A query-style tool in the envelope also
// produces a caveat (search tools can still surface the denied resource), which
// must be carried into the composed output passed to the approver.
func TestE2E_ColdStart_InEnvelopeHardDenyKept(t *testing.T) {
	ctx := approvedCtx()

	const (
		rtype  = "linear_issue"
		denyID = "ENG-42"
	)

	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{
				Resources: []scope.ResourceRef{{ResourceType: rtype, ID: denyID}},
			},
		},
		CleanedTask: "summarize L-140",
	}}

	h, m, sc := newColdStartHarness(t, ext)

	// Envelope declares linear_issue + a query-style search tool, so the
	// in-envelope hard-deny is classified in-envelope AND DetectCaveats flags
	// the search-tool-can-return gap.
	req := ColdStartRequest{
		Requester:   "user:alice",
		RequestText: "summarize L-140 and do not read ENG-42",
		Envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: rtype}},
			Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
		},
		RequesterPerms: scope.RequesterPerms{AllowedIDsByType: map[string]map[string]bool{rtype: {}}},
		ToolReads: map[string]scope.ToolReads{
			"linear.search_issues": {ResourceType: rtype, QueryStyle: true},
		},
	}

	// Capture the composed output handed to the approver to assert the caveat
	// reached the decision surface.
	var seen scope.MetaagentOutput
	require.NoError(t, h.Handle(ctx, sc, "ns/a", req,
		func(_ context.Context, out scope.MetaagentOutput, _ string) (string, error) {
			seen = out
			return ColdStartApproveCleaned, nil
		}))

	cst, found, err := coldstarttask.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)

	// The narrowing is APPLIED, not dropped: it lands in the Layer-2 Disallow set.
	assert.True(t, hasDisallowResource(t, m, sc, rtype, denyID),
		"in-envelope hard-deny narrowing must land in the Layer-2 memory Disallow set")

	// The applied delta passed to the approver kept the hard-deny resource.
	require.Len(t, seen.Delta.HardDeny.Resources, 1, "in-envelope hard-deny must survive classification")
	assert.Equal(t, denyID, seen.Delta.HardDeny.Resources[0].ID)
	assert.Empty(t, seen.Skipped, "nothing out-of-envelope → nothing skipped")

	// The query-style tool produces a completeness caveat for the approver.
	require.Len(t, seen.Caveats, 1, "query-style read tool must flag a search-can-return caveat")
	assert.Equal(t, scope.CaveatSearchToolCanReturn, seen.Caveats[0].Category)
}

// TestE2E_ColdStart_OutOfEnvelopeDropped_PartialApply proves the
// classification boundary in both directions within a single request: a
// hard-deny on an out-of-envelope resource type is DROPPED (appears in
// skipped, never reaches the granter), while an in-envelope hard-deny in the
// same request IS applied. This is the partial-apply guarantee — one bad item
// does not poison the rest, and an item the envelope does not cover never
// becomes an enforced tuple.
func TestE2E_ColdStart_OutOfEnvelopeDropped_PartialApply(t *testing.T) {
	ctx := approvedCtx()

	const (
		inType  = "linear_issue"
		inID    = "ENG-42"
		outType = "github_repo" // NOT declared in the envelope below
		outID   = "secret/repo"
	)

	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{
				Resources: []scope.ResourceRef{
					{ResourceType: inType, ID: inID},
					{ResourceType: outType, ID: outID},
				},
			},
		},
		CleanedTask: "summarize L-140",
	}}

	h, m, sc := newColdStartHarness(t, ext)

	// Envelope declares ONLY linear_issue; github_repo is out of envelope.
	req := ColdStartRequest{
		Requester:   "user:alice",
		RequestText: "summarize L-140; do not read ENG-42 or secret/repo",
		Envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: inType}},
		},
		RequesterPerms: scope.RequesterPerms{AllowedIDsByType: map[string]map[string]bool{inType: {}}},
	}

	var seen scope.MetaagentOutput
	require.NoError(t, h.Handle(ctx, sc, "ns/a", req,
		func(_ context.Context, out scope.MetaagentOutput, _ string) (string, error) {
			seen = out
			return ColdStartApproveCleaned, nil
		}))

	// In-envelope item applied.
	assert.True(t, hasDisallowResource(t, m, sc, inType, inID),
		"in-envelope hard-deny must be applied to the Layer-2 Disallow set")
	// Out-of-envelope item dropped — never reached the Disallow set.
	assert.False(t, hasDisallowResource(t, m, sc, outType, outID),
		"out-of-envelope hard-deny must NOT land in the Disallow set")

	// Applied delta kept exactly the in-envelope resource.
	require.Len(t, seen.Delta.HardDeny.Resources, 1, "exactly the in-envelope hard-deny survives")
	assert.Equal(t, inType, seen.Delta.HardDeny.Resources[0].ResourceType)

	// The dropped item appears in skipped with the out-of-envelope reason.
	require.Len(t, seen.Skipped, 1, "out-of-envelope item recorded as skipped")
	assert.Equal(t, scope.ReasonOutOfEnvelopeRtype, seen.Skipped[0].Reason)
	assert.Contains(t, seen.Skipped[0].RequestFragment, outType)

	// Session was still approved + a scope doc written (partial apply succeeded).
	cst, found, err := coldstarttask.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	_, scopeFound, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.True(t, scopeFound, "partial apply still writes session_scope")
}
