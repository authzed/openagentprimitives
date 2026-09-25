package main

// approval_applied_test.go covers the in.metaagent_approval_applied route, and
// in particular what happens to a click whose in-process await died with the
// previous authzd process.

import (
	"context"
	"encoding/json"
	"sync"
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
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// denyFooBar is the classified delta a mid-session @metaagent narrow produces.
var denyFooBar = scope.ScopeDelta{
	HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
	},
}

// approvalAppliedPayload builds the wire payload channelsd publishes on a click.
func approvalAppliedPayload(t *testing.T, requestID string, approved bool, action string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"requestId":         requestID,
		"approved":          approved,
		"approverId":        "U_BOB",
		"approverCanonical": "user:Ym9i",
		"action":            action,
	})
	require.NoError(t, err, "marshal approval_applied payload")
	return data
}

func approvalAppliedSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentApprovalApplied)
}

// TestMetaagentApprovalApplied_PostRestartClick_ReDrivesFromDurableRecord is the
// restart test: the approval was published by a process that is now gone, so the
// orchestrator has no waiter and the goroutine that owned the 24h deadline died
// with it. The click still arrives. It must resolve off the durable
// metaagent_audit request record rather than vanish — the owner clicked Approve
// and channelsd has already told the thread the scope was approved.
func TestMetaagentApprovalApplied_PostRestartClick_ReDrivesFromDurableRecord(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "redrive"
	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	const reqID = "metaagent-scope-default/redrive-1"

	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: mem}
	// A FRESH orchestrator + worker: exactly the state authzd comes back in
	// after a restart. Nothing re-registers the in-flight await.
	w := NewMetaagentWorker(mg, approval.New(), nc, nil)
	route := newMetaagentApprovalRoute(w, &fakeManageScope{allowed: true})

	// The durable half the pre-restart process wrote at publish time
	// (recordScopeApprovalRequested → metaagentaudit.RecordRequested).
	require.NoError(t, metaagentaudit.RecordRequested(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
		Ts:             time.Now().UTC(),
		RequestID:      reqID,
		Requester:      "user:alice",
		RequestText:    "@metaagent do not read foo/bar",
		Classification: metaagentaudit.Classification{Applied: denyFooBar},
	}), "seed the durable request record")

	route.handle(approvedCtx(), approvalAppliedSubject(ns, name),
		approvalAppliedPayload(t, reqID, true, "approve"))

	got, found, err := sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.NoError(t, err)
	require.True(t, found, "a post-restart approve must still apply the approved scope change")
	assert.True(t, got.ResourceDisallowed("github_repo", "foo/bar"),
		"the delta the owner approved must land in the Layer-2 disallow set")

	// The outcome must also leave a durable trace linked to the request, so a
	// second click cannot apply the same delta twice.
	records, err := metaagentaudit.List(approvedCtx(), mem, scopeRef)
	require.NoError(t, err)
	var outcomes int
	for _, r := range records {
		if r.ApproverDecision != "" && r.RequestID == reqID {
			outcomes++
		}
	}
	assert.Equal(t, 1, outcomes, "the re-driven outcome must be recorded against its request id")
}

// collectMetaagentNotices subscribes to the session's out.metaagent_notice
// subject and returns a func that drains the notice bodies seen so far.
func collectMetaagentNotices(t *testing.T, nc *natsgo.Conn, ns, name string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	sub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentNotice),
		func(m *natsgo.Msg) {
			var pl struct {
				Requester string `json:"requester"`
				Body      string `json:"body"`
			}
			if err := json.Unmarshal(m.Data, &pl); err != nil {
				return
			}
			mu.Lock()
			bodies = append(bodies, pl.Requester+": "+pl.Body)
			mu.Unlock()
		})
	require.NoError(t, err, "subscribe to metaagent_notice")
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	// The drain takes no *testing.T: callers poll it from require.Eventually's
	// goroutine, where a FailNow would be illegal.
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// TestMetaagentApprovalApplied_UnresolvableClick_TellsTheClicker covers every
// way a post-restart click can fail to resolve. None of them may apply scope,
// and none of them may end in silence — the owner clicked a button and is owed
// an answer.
func TestMetaagentApprovalApplied_UnresolvableClick_TellsTheClicker(t *testing.T) {
	const reqID = "metaagent-scope-default/unresolvable-1"
	cases := []struct {
		name string
		// seed writes the durable state that exists before the click.
		seed     func(t *testing.T, mem memory.Memory, scopeRef memory.Scope)
		approved bool
		wantSaid string
	}{
		{
			name:     "no durable record: nothing applied, clicker told to ask again",
			seed:     func(*testing.T, memory.Memory, memory.Scope) {},
			approved: true,
			wantSaid: "no longer available",
		},
		{
			name: "decision already recorded: not applied a second time, clicker told so",
			seed: func(t *testing.T, mem memory.Memory, scopeRef memory.Scope) {
				require.NoError(t, metaagentaudit.RecordRequested(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
					Ts: time.Now().UTC(), RequestID: reqID, Requester: "user:alice",
					Classification: metaagentaudit.Classification{Applied: denyFooBar},
				}))
				require.NoError(t, metaagentaudit.Record(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
					Ts: time.Now().UTC(), RequestID: reqID, Requester: "user:alice",
					ApproverDecision: "approve",
				}))
			},
			approved: true,
			wantSaid: "already been recorded",
		},
		{
			name: "request older than the mid-session window: nothing applied, clicker told it expired",
			seed: func(t *testing.T, mem memory.Memory, scopeRef memory.Scope) {
				require.NoError(t, metaagentaudit.RecordRequested(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
					Ts: time.Now().UTC().Add(-25 * time.Hour), RequestID: reqID, Requester: "user:alice",
					Classification: metaagentaudit.Classification{Applied: denyFooBar},
				}))
			},
			approved: true,
			wantSaid: "expired",
		},
		{
			name: "record predates the persisted delta: approve is refused, clicker told so",
			seed: func(t *testing.T, mem memory.Memory, scopeRef memory.Scope) {
				require.NoError(t, metaagentaudit.RecordRequested(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
					Ts: time.Now().UTC(), RequestID: reqID, Requester: "user:alice",
				}))
			},
			approved: true,
			wantSaid: "could not be restored",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startEmbeddedNATS(t)
			const ns, name = "default", "unresolvable"
			scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
			mem := memory.NewLocal(inmem.NewBackend())
			w := NewMetaagentWorker(&Metaagent{Memory: mem}, approval.New(), nc, nil)
			route := newMetaagentApprovalRoute(w, &fakeManageScope{allowed: true})
			notices := collectMetaagentNotices(t, nc, ns, name)
			tc.seed(t, mem, scopeRef)

			route.handle(approvedCtx(), approvalAppliedSubject(ns, name),
				approvalAppliedPayload(t, reqID, tc.approved, "approve"))

			_, found, err := sessionscope.Get(approvedCtx(), mem, scopeRef)
			require.NoError(t, err)
			assert.False(t, found, "an unresolvable click must not apply scope")

			require.Eventually(t, func() bool { return len(notices()) >= 1 },
				5*time.Second, 10*time.Millisecond, "the clicker must be told")
			got := notices()
			require.Len(t, got, 1, "the clicker must be told exactly once")
			assert.Contains(t, got[0], "U_BOB", "the notice must be addressed to the clicker")
			assert.Contains(t, got[0], tc.wantSaid)
		})
	}
}

// TestMetaagentApprovalApplied_LiveWaiter_GoesToTheOrchestrator guards the
// normal path: while a waiter IS registered the click resolves it in process and
// the durable re-drive must stay out of the way (applying there too would apply
// the delta twice). It then sends a REPEAT click, which arrives after the waiter
// was forgotten but before its Apply stage could write an outcome record — the
// window the durable already-resolved check alone cannot cover.
func TestMetaagentApprovalApplied_LiveWaiter_GoesToTheOrchestrator(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "livewaiter"
	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	const reqID = "metaagent-scope-default/livewaiter-1"

	mem := memory.NewLocal(inmem.NewBackend())
	orch := approval.New()
	w := NewMetaagentWorker(&Metaagent{Memory: mem}, orch, nc, nil)
	route := newMetaagentApprovalRoute(w, &fakeManageScope{allowed: true})

	// The durable record exists (it always does), so a wrongly-taken re-drive
	// would visibly apply scope.
	require.NoError(t, metaagentaudit.RecordRequested(approvedCtx(), mem, scopeRef, metaagentaudit.Content{
		Ts: time.Now().UTC(), RequestID: reqID, Requester: "user:alice",
		Classification: metaagentaudit.Classification{Applied: denyFooBar},
	}))

	awaitCtx, cancel := context.WithTimeout(approvedCtx(), 10*time.Second)
	t.Cleanup(cancel)
	decCh := make(chan approval.Decision, 1)
	go func() {
		d, err := orch.Await(awaitCtx, approval.Request{RequestID: reqID, SessionRef: ns + "/" + name})
		if err == nil {
			decCh <- d
		}
	}()
	require.Eventually(t, func() bool {
		return len(orch.PendingRequestIDsForSession(ns+"/"+name)) == 1
	}, 5*time.Second, 10*time.Millisecond, "the await must register before the click")

	route.handle(approvedCtx(), approvalAppliedSubject(ns, name),
		approvalAppliedPayload(t, reqID, true, "approve"))

	select {
	case d := <-decCh:
		assert.True(t, d.Approved, "the waiter must receive the decision")
		assert.Equal(t, "U_BOB", d.ApproverID)
	case <-time.After(5 * time.Second):
		t.Fatal("the in-process waiter never received the decision")
	}
	_, found, err := sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.NoError(t, err)
	assert.False(t, found, "the re-drive must not also apply while a waiter owns the request")

	// The waiter is gone now and (in this test) never wrote an outcome record,
	// so only the in-process memory of having delivered it can stop a repeat.
	require.Empty(t, orch.PendingRequestIDsForSession(ns+"/"+name), "the await must be forgotten")
	route.handle(approvedCtx(), approvalAppliedSubject(ns, name),
		approvalAppliedPayload(t, reqID, true, "approve"))
	_, found, err = sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.NoError(t, err)
	assert.False(t, found, "a repeat click must not re-apply a decision this process already delivered")
}

// TestRunMetaagentLifecycle_AuditTrailIsReDrivable pins the two properties of
// the durable audit trail a post-restart re-drive depends on:
//
//   - the request-time record carries the CLASSIFIED DELTA the human is being
//     asked to approve (without it the record proves a request happened but not
//     what approving it would do), and
//   - the outcome record carries the REQUEST ID (without it a re-drive cannot
//     tell an undecided request from one already applied, and a second click
//     would apply the same delta twice).
func TestRunMetaagentLifecycle_AuditTrailIsReDrivable(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "reqrec"
	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}

	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory:    mem,
		Extractor: &fakeMetaExtractor{result: denyFooBar},
		Composer:  &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "Deny foo/bar."}},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nc, nil)
	w.SetManageScopeChecker(&fakeManageScope{allowed: true})

	// Stand in for channelsd: approve whatever request id the envelope carries.
	var publishedReqID string
	sub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval),
		func(m *natsgo.Msg) {
			var p struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal(m.Data, &p); err != nil || p.RequestID == "" {
				return
			}
			publishedReqID = p.RequestID
			orch.DeliverDecision(p.RequestID, approval.Decision{Approved: true, ApproverID: "U_BOB"})
		})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:alice",
		text:      "@metaagent do not read foo/bar",
		envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
		},
	}))
	require.NotEmpty(t, publishedReqID, "the approval envelope must have been published")

	rec, err := metaagentaudit.RequestByID(approvedCtx(), mem, scopeRef, publishedReqID)
	require.NoError(t, err)
	require.NotNil(t, rec, "the request-time record must exist")
	assert.Equal(t, denyFooBar, rec.Classification.Applied,
		"the request record must carry the classified delta a re-drive would apply")

	decided, err := decisionRecorded(approvedCtx(), mem, scopeRef, publishedReqID)
	require.NoError(t, err)
	assert.True(t, decided,
		"the outcome record must be stamped with the request id, so a later click cannot re-apply it")
}
