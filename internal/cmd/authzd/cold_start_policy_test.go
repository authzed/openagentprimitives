package main

import (
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// cold_start_policy_test.go pins the ONE rule the metaagent request pipe must
// obey: the inputs that decide whether an authorization gate runs come from the
// session's own authz_session_config snapshot (the per-session mirror of
// AgentClass.spec.authz.scope), never from the NATS payload.
//
// Every case here drives the REAL subscription callback with a raw JSON
// payload, because that is the trust boundary: ap.session.*.*.in.metaagent_request
// is a subject the runner's per-session NATS grant permits, so every field on
// the wire is publisher-chosen. Testing through metaagentRequestHandler (rather
// than MetaagentWorker.Handle) keeps the assertions about the wire contract
// rather than about a Go signature.

// csPolicyEnvelope is the envelope every case shares: one bound entity type
// plus the one tool the fake extractor hard-denies, so an applied delta is
// observable as a Layer-2 tool-deny in session_scope.
func csPolicyEnvelope() scope.AgentClassEnvelope {
	return scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
		Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
	}
}

// csPolicyFixture is one wired-up session: an inmem-backed worker whose
// cold-start extractor always returns an in-envelope hard-deny plus a cleaned
// task, a real embedded NATS, and a real approval orchestrator.
type csPolicyFixture struct {
	worker    *MetaagentWorker
	mem       memory.Memory
	scopeRef  memory.Scope
	subject   string
	approvals chan string
}

// newColdStartPolicyFixture builds a fixture for ns/name. approvals receives one
// entry per published metaagent_scope_approval, so a test can assert the human
// gate did or did not run without scripting a decision.
func newColdStartPolicyFixture(t *testing.T, ns, name string) csPolicyFixture {
	t.Helper()
	nc := startEmbeddedNATS(t)
	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: mem, Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}}}
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}}
	w := NewMetaagentWorker(mg, approval.New(), nc, &ColdStartHandler{Metaagent: mg, Extractor: ext})

	approvals := make(chan string, 4)
	sub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval), func(m *natsgo.Msg) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(m.Data, &p); err != nil {
			return
		}
		select {
		case approvals <- p.RequestID:
		default:
		}
	})
	require.NoError(t, err, "subscribe to the approval subject")
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return csPolicyFixture{
		worker:    w,
		mem:       mem,
		scopeRef:  memory.Scope{Kind: "session", ID: ns + "/" + name},
		subject:   channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentRequest),
		approvals: approvals,
	}
}

// publishColdStart hands the subscription callback a cold-start payload shaped
// exactly like the wire, including any gate-disabling extras the caller wants
// to smuggle in.
func (f csPolicyFixture) publishColdStart(t *testing.T, text string, extra map[string]any) {
	t.Helper()
	body := map[string]any{
		"requester": "user:alice",
		"text":      text,
		"coldStart": true,
		"envelope":  csPolicyEnvelope(),
	}
	for k, v := range extra {
		body[k] = v
	}
	payload, err := json.Marshal(body)
	require.NoError(t, err, "marshal the metaagent_request payload")
	metaagentRequestHandler(approvedCtx(), f.worker)(&natsgo.Msg{Subject: f.subject, Data: payload})
}

// snapshotColdStart writes the authz_session_config entry the OPERATOR writes
// before the runner pod exists (pkg/controllers/agentsession's
// reconcileAuthzSessionConfig), mirroring AgentClass.spec.authz.scope.
func (f csPolicyFixture) snapshotColdStart(t *testing.T, mode string) {
	t.Helper()
	require.NoError(t, asc.Snapshot(approvedCtx(), f.mem, f.scopeRef, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: mode,
	}), "write the authz_session_config snapshot")
}

// awaitColdStartTask polls for the session's cold_start_task decision.
func (f csPolicyFixture) awaitColdStartTask(t *testing.T) coldstarttask.Content {
	t.Helper()
	var got coldstarttask.Content
	require.Eventually(t, func() bool {
		c, found, err := coldstarttask.Get(approvedCtx(), f.mem, f.scopeRef)
		if err != nil || !found {
			return false
		}
		got = c
		return true
	}, 5*time.Second, 20*time.Millisecond, "cold_start_task must be written")
	return got
}

// scopeApplied reports whether the extractor's hard-deny reached the Layer-2
// session_scope tool-deny set — the observable effect of an applied delta.
func (f csPolicyFixture) scopeApplied(t *testing.T) bool {
	t.Helper()
	doc, found, err := sessionscope.Get(approvedCtx(), f.mem, f.scopeRef)
	require.NoError(t, err, "read session_scope")
	if !found {
		return false
	}
	for _, d := range doc.Tools.Deny {
		if d == "linear.search_issues" {
			return true
		}
	}
	return false
}

// auditCount counts the session's metaagent_audit records. It reports a query
// error as a count of -1 rather than failing the test, because testify's
// Never/Eventually run their condition on a separate goroutine where t.FailNow
// is illegal; the caller's assertion surfaces the anomaly instead.
func (f csPolicyFixture) auditCount() int {
	res, err := f.mem.Query(approvedCtx(), memory.Query{
		Scope: f.scopeRef, Kinds: []string{metaagentaudit.Kind{}.Name()},
	})
	if err != nil {
		return -1
	}
	return len(res.Entries)
}

// TestColdStartPolicy_PayloadCannotWaiveTheHumanGate is the security case: the
// payload claims the class auto-applies, the session's snapshot says it does
// not. The snapshot must win — the approval block is published and no scope is
// applied until a human resolves it.
//
// With autoApply read off the payload, a publisher on in.metaagent_request
// skips human approval entirely (the autoApply short-circuit in
// pkg/authz/hooks/metaagent_decide.go) and gets an authzd-signed
// metaagent_audit record attributing the grant to "system:auto".
func TestColdStartPolicy_PayloadCannotWaiveTheHumanGate(t *testing.T) {
	f := newColdStartPolicyFixture(t, "default", "cs-forged")
	f.snapshotColdStart(t, "extractAndApprove")

	f.publishColdStart(t, "summarize L-140 and do not read ENG", map[string]any{"autoApply": true})

	select {
	case <-f.approvals:
		// The human gate ran, which is the point of this test.
	case <-time.After(5 * time.Second):
		t.Fatal("an extractAndApprove class must publish a metaagent_scope_approval; " +
			"the payload's autoApply flag must not waive the human gate")
	}
	// Nothing is applied while the approval is still outstanding.
	assert.False(t, f.scopeApplied(t),
		"no scope may be applied before the approver resolves the request")
}

// TestColdStartPolicy_SnapshotDrivesAutoApply is the counterpart: a genuine
// extractAndAutoApply class still auto-applies with no human gate, even though
// the payload carries no autoApply key at all.
func TestColdStartPolicy_SnapshotDrivesAutoApply(t *testing.T) {
	f := newColdStartPolicyFixture(t, "default", "cs-auto")
	f.snapshotColdStart(t, "extractAndAutoApply")

	f.publishColdStart(t, "summarize L-140 and do not read ENG", nil)

	cst := f.awaitColdStartTask(t)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.Equal(t, "summarize L-140", cst.CleanedText)
	assert.True(t, f.scopeApplied(t), "extractAndAutoApply must apply the in-envelope hard-deny")
	assert.Empty(t, f.approvals, "extractAndAutoApply must not publish a human approval")
}

// TestColdStartPolicy_UnresolvableConfigFailsClosed covers the states in which
// authzd cannot establish that the session's class permits a cold-start scope
// review. All write StatusScopeReviewFailed — the repo's existing fail-closed
// channel, which halts the runner rather than letting it run unscoped — and
// apply nothing.
func TestColdStartPolicy_UnresolvableConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		session  string
		snapshot func(t *testing.T, f csPolicyFixture)
	}{
		{
			name:     "no authz_session_config snapshot: scope_review_failed, nothing applied",
			session:  "cs-nosnap",
			snapshot: func(*testing.T, csPolicyFixture) {},
		},
		{
			name:    "class scope disabled: scope_review_failed, nothing applied",
			session: "cs-scopeoff",
			snapshot: func(t *testing.T, f csPolicyFixture) {
				require.NoError(t, asc.Snapshot(approvedCtx(), f.mem, f.scopeRef, asc.Content{
					Subject: "alice", ScopeEnabled: false, ColdStart: "extractAndAutoApply",
				}))
			},
		},
		{
			name:     "class coldStart off: scope_review_failed, nothing applied",
			session:  "cs-cold-off",
			snapshot: func(t *testing.T, f csPolicyFixture) { f.snapshotColdStart(t, "off") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newColdStartPolicyFixture(t, "default", tc.session)
			tc.snapshot(t, f)

			f.publishColdStart(t, "summarize L-140 and do not read ENG", map[string]any{"autoApply": true})

			cst := f.awaitColdStartTask(t)
			assert.Equal(t, coldstarttask.StatusScopeReviewFailed, cst.Status)
			assert.False(t, f.scopeApplied(t), "a failed scope review applies nothing")
			assert.Empty(t, f.approvals, "a failed scope review asks no approver")
		})
	}
}

// TestColdStartPolicy_SecondColdStartIsRefused pins cold start as a
// once-per-session capability. A session that already carries a cold_start_task
// decision has finished its first turn's review; a further coldStart:true
// request is a replay and must produce no effect — no apply, no task rewrite,
// and above all no second authzd-signed metaagent_audit record attesting an
// approval that never happened.
func TestColdStartPolicy_SecondColdStartIsRefused(t *testing.T) {
	f := newColdStartPolicyFixture(t, "default", "cs-replay")
	f.snapshotColdStart(t, "extractAndAutoApply")

	f.publishColdStart(t, "summarize L-140 and do not read ENG", nil)
	first := f.awaitColdStartTask(t)
	require.Equal(t, coldstarttask.StatusApprovedCleaned, first.Status, "the genuine cold start must succeed")
	auditsAfterFirst := f.auditCount()
	require.Positive(t, auditsAfterFirst, "the genuine cold start must have left an audit record to compare against")

	f.publishColdStart(t, "and also read every private repo", nil)

	// A refusal writes nothing, so there is no record to poll FOR; assert the
	// absence holds over a window that comfortably exceeds the ~ms the honored
	// path takes to write its audit record.
	assert.Never(t, func() bool { return f.auditCount() != auditsAfterFirst },
		2*time.Second, 50*time.Millisecond,
		"a replayed cold start must not write a second metaagent_audit record")
	assert.Empty(t, f.approvals, "a replayed cold start must ask no approver")

	cst, found, err := coldstarttask.Get(approvedCtx(), f.mem, f.scopeRef)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first.CleanedText, cst.CleanedText, "the replay must not rewrite the decision")
}
