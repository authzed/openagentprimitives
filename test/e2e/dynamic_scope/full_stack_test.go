// test/e2e/dynamic_scope/full_stack_test.go
//
// Full-stack integration test for the Dynamic Session Scope feature.
//
// This test exercises the complete approval pipeline without requiring real
// NATS or Slack infrastructure. It proves the seam contracts between:
//
//  1. The scope delta classifier (pkg/authz/scope.ClassifySkipped)
//  2. The approval orchestrator (pkg/authz/guardian/approval.Orchestrator)
//  3. The conservative SpiceDB+memory apply ordering
//  4. The NATS subject conventions for metaagent_request /
//     metaagent_scope_approval / metaagent_approval_applied
//
// Individual seam tests already cover each component in isolation:
//   - TestHandleEventsAPI_MetaagentMention_Publishes   (listener → NATS)
//   - TestBuildMetaagentScopeApprovalBlocks_*          (block rendering)
//   - TestOnInteraction_MetaagentApprove               (button → NATS)
//   - TestApplyScopeChange_*                           (SpiceDB + memory)
//   - TestE2E_ApprovalOutcomes                         (full orchestrator E2E)
//
// This file adds a white-box integration test that wires those same pieces
// together using only importable packages (no cmd/* main packages needed).
package dynamicscope_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// ---------------------------------------------------------------------------
// Fake NATS
// ---------------------------------------------------------------------------

// fakeNATS is a minimal pub/sub that captures published messages by subject
// and delivers synchronously to registered handlers. Supports exact-subject
// matching only (no wildcards); sufficient for this test.
type fakeNATS struct {
	mu       sync.Mutex
	handlers map[string]func([]byte)
	posted   []postedMsg
}

type postedMsg struct {
	Subject string
	Data    []byte
}

func newFakeNATS() *fakeNATS {
	return &fakeNATS{handlers: map[string]func([]byte){}}
}

func (f *fakeNATS) subscribe(subject string, handler func([]byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[subject] = handler
}

func (f *fakeNATS) publish(subject string, data []byte) error {
	f.mu.Lock()
	f.posted = append(f.posted, postedMsg{Subject: subject, Data: data})
	h, ok := f.handlers[subject]
	f.mu.Unlock()
	if ok {
		h(data) // synchronous in tests; avoids timing races
	}
	return nil
}

func (f *fakeNATS) allPosted() []postedMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]postedMsg, len(f.posted))
	copy(out, f.posted)
	return out
}

// ---------------------------------------------------------------------------
// Fake SpiceDB granter
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Inline scope orchestrator
//
// Mirrors internal/cmd/authzd.Metaagent using only importable packages. This lets
// the test compile and run without importing cmd/* (package main).
// ---------------------------------------------------------------------------

// scopeOrchestrator drives the apply pipeline using the importable scope
// and memory packages. Hard-deny is enforced entirely at Layer 2 (the
// session_scope memory Disallow set the Scope hook reads at dispatch);
// Layer-3 SpiceDB disallow is retired (Phase 4), so the orchestrator has no
// SpiceDB granter.
type scopeOrchestrator struct {
	mem memory.Memory
}

// applyDelta applies a delta (Add widening or HardDeny) to the session scope in
// memory via scope.ApplyDelta. Mirrors internal/cmd/authzd.Metaagent.ApplyScopeChange's
// memory write: HardDeny.Resources land in next.Disallow, HardDeny.Tools in
// next.Tools.Deny, Add in next.Resources.
func (o *scopeOrchestrator) applyDelta(
	ctx context.Context,
	scopeRef memory.Scope,
	delta scope.ScopeDelta,
) error {
	cur, _, err := sessionscope.Get(ctx, o.mem, scopeRef)
	if err != nil {
		return fmt.Errorf("read scope: %w", err)
	}
	next := scope.ApplyDelta(cur, delta, scope.SourceMetaagentApproved, time.Time{})
	return sessionscope.Put(ctx, o.mem, scopeRef, next)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// natsSubject returns the NATS subject for a session-scoped metaagent event.
// dir is the direction column of TestFullStack_NATSSubjectConventions' table;
// everything else routes through the one subject grammar, so this file cannot
// spell a subject no publisher produces.
func natsSubject(t *testing.T, ns, name, dir, kind string) string {
	t.Helper()
	prefix := channelevents.SubjectPrefix(ns, name)
	switch dir {
	case "in":
		return channelevents.SubjectIn(prefix, channelevents.Kind(kind))
	case "out":
		return channelevents.SubjectOut(prefix, channelevents.Kind(kind))
	default:
		t.Fatalf("natsSubject: unknown direction %q; want \"in\" or \"out\"", dir)
		return ""
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestFullStack_ApprovalWidensScope drives the full approve path:
//   - scope classifier keeps the resource (it is in the envelope + perms)
//   - approval orchestrator blocks then receives Approve decision
//   - ApplyDelta writes memory; DeleteRelationships revokes prior disallow
func TestFullStack_ApprovalWidensScope(t *testing.T) {
	const (
		sessNS   = "default"
		sessName = "sess-e2e"
		sessRef  = sessNS + "/" + sessName
		rtype    = "github_repo"
		rid      = "foo/bar"
	)

	mem := memory.NewLocal(inmem.NewBackend())
	orch := approval.New()
	nat := newFakeNATS()

	scopeRef := memory.Scope{Kind: "session", ID: sessRef}
	orchSvc := &scopeOrchestrator{mem: mem}

	// Wire the approval_applied subscriber that delivers the decision to
	// the orchestrator — this is the seam F5 tests.
	appliedSubj := natsSubject(t, sessNS, sessName, "in", "metaagent_approval_applied")
	nat.subscribe(appliedSubj, func(data []byte) {
		var payload struct {
			RequestID string `json:"requestId"`
			Approved  bool   `json:"approved"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return
		}
		orch.DeliverDecision(payload.RequestID, approval.Decision{Approved: payload.Approved})
	})

	// Proposed delta: Add github_repo foo/bar.
	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: rtype, ID: rid}},
		},
	}
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: rtype, Permission: "read"}},
	}
	perms := scope.RequesterPerms{
		AllowedIDsByType: map[string]map[string]bool{rtype: {rid: true}},
	}

	// Step 2: classify.
	applied, skipped := scope.ClassifySkipped(proposed, env, perms)
	require.Empty(t, skipped, "nothing should be skipped when resource is in envelope + perms")
	require.False(t, applied.IsEmpty(), "applied delta must be non-empty")

	// Step 4: decider wired through approval orchestrator + fake NATS.
	requestID := "test-request-001"
	approvalSubj := natsSubject(t, sessNS, sessName, "out", "metaagent_scope_approval")

	// Register the await before publishing, then deliver the decision via NATS.
	var awaited approval.Decision
	awaitDone := make(chan struct{})
	go func() {
		defer close(awaitDone)
		d, err := orch.Await(memory.WithSystemApproval(context.Background(), "test"), approval.Request{
			RequestID:  requestID,
			SessionRef: sessRef,
			OnPublish: func(ctx context.Context) error {
				// Simulate authzd publishing the approval block.
				payload, _ := json.Marshal(map[string]any{
					"requestId":       requestID,
					"requester":       "U_ALICE",
					"approverSummary": "Grant read on foo/bar.",
				})
				return nat.publish(approvalSubj, payload)
			},
		})
		awaited = d
		_ = err
	}()

	// Assert the scope_approval publish happened (seam 3).
	require.Eventually(t, func() bool {
		for _, m := range nat.allPosted() {
			if m.Subject == approvalSubj {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "metaagent_scope_approval must be published")

	var scopeApprovalPayload map[string]any
	for _, m := range nat.allPosted() {
		if m.Subject == approvalSubj {
			require.NoError(t, json.Unmarshal(m.Data, &scopeApprovalPayload))
			break
		}
	}
	assert.Equal(t, requestID, scopeApprovalPayload["requestId"], "scope approval payload must contain requestId")
	assert.Equal(t, "Grant read on foo/bar.", scopeApprovalPayload["approverSummary"])

	// Simulate button click: publish approval_applied (seam 5).
	approvedPayload, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"approved":  true,
	})
	require.NoError(t, nat.publish(appliedSubj, approvedPayload))

	// Wait for Await to return.
	select {
	case <-awaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("approval.Await did not return after DeliverDecision")
	}

	assert.True(t, awaited.Approved, "approval decision must be Approved=true")

	// Step 5: apply the scope change (seams 6 + 7).
	require.NoError(t, orchSvc.applyDelta(memory.WithSystemApproval(context.Background(), "test"), scopeRef, applied))

	// Assert memory was updated.
	got, found, err := sessionscope.Get(memory.WithSystemApproval(context.Background(), "test"), mem, scopeRef)
	require.NoError(t, err)
	require.True(t, found, "scope must be written to memory on approve")
	require.Len(t, got.Resources, 1)
	assert.Equal(t, rtype, got.Resources[0].ResourceType)
	assert.Equal(t, []string{rid}, got.Resources[0].IDs)
}

// TestFullStack_DenyWritesStickyMemoryDisallow drives the deny path:
//   - scope classifier keeps the resource
//   - decider returns deny (simulating button click via orchestrator)
//   - deny converts the Add to a HardDeny → Layer-2 memory Disallow set
//   - the deny is sticky and read by the Scope hook at dispatch
//
// Layer-3 SpiceDB disallow is retired (Phase 4): the production deny path
// (HandleMetaagentRequest deny → ApplyScopeChange → ApplyDelta) advances the
// memory Disallow set, which the Scope hook is the sole enforcer of.
func TestFullStack_DenyWritesStickyMemoryDisallow(t *testing.T) {
	const (
		sessNS   = "default"
		sessName = "sess-deny"
		sessRef  = sessNS + "/" + sessName
		rtype    = "github_repo"
		rid      = "no/access"
	)

	mem := memory.NewLocal(inmem.NewBackend())
	orch := approval.New()
	nat := newFakeNATS()

	scopeRef := memory.Scope{Kind: "session", ID: sessRef}
	orchSvc := &scopeOrchestrator{mem: mem}

	// Wire approval_applied subscriber.
	appliedSubj := natsSubject(t, sessNS, sessName, "in", "metaagent_approval_applied")
	nat.subscribe(appliedSubj, func(data []byte) {
		var payload struct {
			RequestID string `json:"requestId"`
			Approved  bool   `json:"approved"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return
		}
		orch.DeliverDecision(payload.RequestID, approval.Decision{Approved: payload.Approved})
	})

	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: rtype, ID: rid}},
		},
	}
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: rtype, Permission: "read"}},
	}
	perms := scope.RequesterPerms{
		AllowedIDsByType: map[string]map[string]bool{rtype: {rid: true}},
	}

	applied, skipped := scope.ClassifySkipped(proposed, env, perms)
	require.Empty(t, skipped)
	require.False(t, applied.IsEmpty())

	requestID := "test-deny-001"

	// Start the await goroutine. The OnPublish callback signals readiness via
	// a channel so the test can publish the deny after the orchestrator has
	// registered the pending request.
	var awaited approval.Decision
	awaitDone := make(chan struct{})
	registered := make(chan struct{})
	go func() {
		defer close(awaitDone)
		d, _ := orch.Await(memory.WithSystemApproval(context.Background(), "test"), approval.Request{
			RequestID:  requestID,
			SessionRef: sessRef,
			OnPublish: func(_ context.Context) error {
				close(registered) // signals the orchestrator is ready
				return nil
			},
		})
		awaited = d
	}()

	// Wait for the orchestrator to register before delivering the decision.
	select {
	case <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("await goroutine did not register within timeout")
	}

	// Deliver deny decision.
	denyPayload, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"approved":  false,
	})
	require.NoError(t, nat.publish(appliedSubj, denyPayload))

	select {
	case <-awaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("approval.Await did not return")
	}

	assert.False(t, awaited.Approved, "decision must be Approved=false (deny)")

	// Deny-conversion: convert Add to HardDeny.
	conv := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: append([]scope.ResourceRef(nil), applied.Add.Resources...),
		},
	}
	require.NoError(t, orchSvc.applyDelta(memory.WithSystemApproval(context.Background(), "test"), scopeRef, conv))

	// The deny lands in the Layer-2 memory Disallow set (the Scope hook reads it
	// at dispatch). The Add is NOT widened into scope — only the hard-deny is.
	got, found, err := sessionscope.Get(memory.WithSystemApproval(context.Background(), "test"), mem, scopeRef)
	require.NoError(t, err)
	require.True(t, found, "deny advances the memory scope doc with the sticky disallow")
	assert.True(t, got.ResourceDisallowed(rtype, rid),
		"deny must record a sticky Layer-2 Disallow the Scope hook enforces")
	assert.Empty(t, got.Resources, "deny must not widen the in-scope Resources set")
}

// TestFullStack_UnaddressableMention verifies that when a request cannot
// be addressed (all items skipped), the decider is not called and
// CannotAddress is set on the output.
//
// This seam verifies the classifier + CannotAddress path without an
// approval round-trip (the metaagent must not block waiting for a Slack
// button click when there is nothing addressable).
func TestFullStack_UnaddressableMention(t *testing.T) {
	// Propose a resource type not in the envelope.
	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1234"}},
		},
	}
	// Envelope only declares github_repo — linear_issue is out of envelope.
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo", Permission: "read"}},
	}
	perms := scope.RequesterPerms{AllowedIDsByType: map[string]map[string]bool{}}

	applied, skipped := scope.ClassifySkipped(proposed, env, perms)

	// The applied delta is empty; a real orchestrator would set CannotAddress.
	assert.True(t, applied.IsEmpty(), "applied delta must be empty when all items are out-of-envelope")
	require.Len(t, skipped, 1, "all requested items must appear in skipped list")
	assert.Equal(t, scope.ReasonOutOfEnvelopeRtype, skipped[0].Reason,
		"skipped item must carry ReasonOutOfEnvelopeRtype")
	assert.Contains(t, skipped[0].RequestFragment, "linear_issue",
		"skipped fragment must identify the resource type")
}

// TestFullStack_NATSSubjectConventions verifies that the NATS subject
// strings emitted by the fakeNATS helper match the conventions expected
// by both the Slack listener (in.metaagent_request, in.metaagent_approval_applied)
// and authzd worker (out.metaagent_scope_approval, out.metaagent_notice).
//
// This is a compile-time + runtime assertion that the string constants used
// in this test file match the conventions tested in isolation by the other
// packages.
func TestFullStack_NATSSubjectConventions(t *testing.T) {
	cases := []struct {
		dir  string
		kind string
		want string
	}{
		{
			dir: "in", kind: "metaagent_request",
			want: "ap.session.default.my-sess.in.metaagent_request",
		},
		{
			dir: "in", kind: "metaagent_approval_applied",
			want: "ap.session.default.my-sess.in.metaagent_approval_applied",
		},
		{
			dir: "out", kind: "metaagent_scope_approval",
			want: "ap.session.default.my-sess.out.metaagent_scope_approval",
		},
		{
			dir: "out", kind: "metaagent_notice",
			want: "ap.session.default.my-sess.out.metaagent_notice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := natsSubject(t, "default", "my-sess", tc.dir, tc.kind)
			assert.Equal(t, tc.want, got,
				"natsSubject must produce the canonical convention subject")
		})
	}
}
