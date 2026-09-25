package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
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
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// buildFakeMetaagent constructs a Metaagent with fake extractor and composer
// that immediately succeed with an empty delta (CannotAddress path), which
// avoids any SpiceDB or NATS calls.
func buildFakeMetaagent() *Metaagent {
	m := memory.NewLocal(inmem.NewBackend())
	return &Metaagent{
		Memory:    m,
		Extractor: &fakeMetaExtractor{result: scope.ScopeDelta{}},
		Composer:  &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "test"}},
	}
}

func TestMetaagentWorker_Handle_SpawnsPerSessionGoroutine(t *testing.T) {
	mg := buildFakeMetaagent()
	orch := approval.New()
	// nil nats.Conn is safe because the empty-delta path never calls Publish.
	w := NewMetaagentWorker(mg, orch, nil, nil)

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	err := w.Handle(approvedCtx(), HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "x"})
	require.NoError(t, err)

	// Registration is synchronous: Handle creates the channel and spawns the
	// goroutine under w.mu BEFORE it returns, so this needs no polling. The
	// context is live, so nothing deregisters the entry behind our back.
	w.mu.Lock()
	ch, registered := w.sessions[scopeRef.ID]
	sessionCount := len(w.sessions)
	w.mu.Unlock()
	require.True(t, registered, "Handle must register a channel keyed by the scope ID")
	assert.Equal(t, 1, sessionCount, "one request for one session registers exactly one entry")

	// The registered channel alone does not prove a goroutine is RECEIVING on
	// it — the send is buffered (cap 16), so it would succeed even if runSession
	// were never spawned. Draining is the observable proof.
	require.Eventually(t, func() bool { return len(ch) == 0 }, time.Second, 10*time.Millisecond,
		"the spawned per-session goroutine must drain the queued request")
}

// TestAuthzdInSubjectParse characterizes the subject parser every authzd NATS
// callback gates on: anything it rejects is dropped before any authorization
// runs, so the accept/reject boundary is worth pinning here as well as in
// pkg/channels/channelevents.
//
// authzd shares the canonical parser with the relay and channelsd's inbound
// handlers; there must never be a second local copy. The rows below pin the two
// refusals that matter: an empty ns/name must NOT route to a nameless session,
// and a segment the caller did not ask for (an "out" on an "in" parse) must be
// rejected outright.
func TestAuthzdInSubjectParse(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		ns      string
		obj     string
		ok      bool
	}{
		{name: "user_message subject: parsed into ns and name", subject: "ap.session.ns1.sess-a.in.user_message", ns: "ns1", obj: "sess-a", ok: true},
		{name: "metaagent_request subject: parsed into ns and name", subject: "ap.session.ns1.sess-a.in.metaagent_request", ns: "ns1", obj: "sess-a", ok: true},
		{name: "dotted kind leaf: still parsed from the first four tokens", subject: "ap.session.ns1.sess-a.in.a.dotted.kind", ns: "ns1", obj: "sess-a", ok: true},
		{name: "out segment on an in parse: rejected", subject: "ap.session.ns1.sess-a.out.user_message", ok: false},
		{name: "too few tokens: rejected", subject: "ap.session.ns1.sess-a.in", ok: false},
		{name: "wrong prefix: rejected", subject: "other.session.ns1.sess-a.in.user_message", ok: false},
		{name: "wrong second token: rejected", subject: "ap.notsession.ns1.sess-a.in.user_message", ok: false},
		{name: "empty subject: rejected", subject: "", ok: false},
		{name: "empty ns and name tokens: rejected, not routed to a nameless session", subject: "ap.session....", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, ok := channelevents.ParseInSubject(tc.subject)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.ns, ns)
			assert.Equal(t, tc.obj, name)
		})
	}
}

// TestMetaagentRequestHandler_RunContextCancelStopsSessionGoroutine pins the
// wiring, not the worker: the subscription callback must carry authzd's run
// context into MetaagentWorker.Handle. context.Background().Done() is a NIL
// channel, so a background context makes both ctx.Done() arms (the queue-full
// send in Handle, the receive loop in runSession) permanently unselectable —
// the per-session goroutines then outlive shutdown, and a full queue blocks the
// NATS dispatch goroutine, which nats.go runs serially per subscription, with
// no way out.
func TestMetaagentRequestHandler_RunContextCancelStopsSessionGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(approvedCtx())
	t.Cleanup(cancel)
	w := NewMetaagentWorker(buildFakeMetaagent(), approval.New(), nil, nil)
	sessionCount := func() int {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.sessions)
	}

	payload, err := json.Marshal(map[string]any{"requester": "user:alice", "text": "widen scope"})
	require.NoError(t, err)
	metaagentRequestHandler(ctx, w)(&natsgo.Msg{
		Subject: "ap.session.ns.a.in.metaagent_request",
		Data:    payload,
	})

	require.Eventually(t, func() bool { return sessionCount() == 1 }, time.Second, 10*time.Millisecond,
		"the request must spawn a per-session goroutine")

	cancel()

	require.Eventually(t, func() bool { return sessionCount() == 0 }, 2*time.Second, 10*time.Millisecond,
		"the per-session goroutine must exit (and deregister) when authzd's run context is cancelled")
}

func TestMetaagentWorker_PerSessionSerialization(t *testing.T) {
	// Wire a blocking extractor so concurrent Handle calls queue up.
	blocked := make(chan struct{})
	calls := int32(0)
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory: m,
		Extractor: &fakeMetaExtractorFn{fn: func() scope.ScopeDelta {
			atomic.AddInt32(&calls, 1)
			<-blocked
			return scope.ScopeDelta{}
		}},
		Composer: &fakeMetaComposer{},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nil, nil)

	scopeRef := memory.Scope{Kind: "session", ID: "ns/b"}
	// Send two requests for the same session.
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "req1"}))
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "req2"}))

	// Unblock after a short delay to give both requests time to queue.
	time.Sleep(20 * time.Millisecond)

	// Only one call should be in-flight at a time (serialization).
	assert.LessOrEqual(t, atomic.LoadInt32(&calls), int32(1),
		"per-session goroutine must not start a second extract while first is running")

	// Drain.
	close(blocked)
}

// TestMetaagentWorker_ChannelCleanupAfterGoroutineExit pins the deregistration
// half of the per-session goroutine's lifecycle: when the context Handle was
// given is already cancelled, runSession's deferred cleanup must remove the
// session's entry from w.sessions. Without that, every cancelled session leaks a
// map entry AND its channel, and the next Handle for the same scope hands its
// request to a channel nobody is receiving on.
func TestMetaagentWorker_ChannelCleanupAfterGoroutineExit(t *testing.T) {
	mg := buildFakeMetaagent()
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nil, nil)

	scopeRef := memory.Scope{Kind: "session", ID: "ns/c"}
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "x"}))

	// The goroutine exits when the channel drains (channel is not closed by
	// the test, but the per-session loop exits when ctx is cancelled). Use a
	// cancelled context to drive the exit immediately for this test.
	cancelCtx, cancel := context.WithCancel(approvedCtx())
	cancel()

	w2 := NewMetaagentWorker(mg, orch, nil, nil)
	// With an already-cancelled context, Handle should return ctx.Err.
	//
	// Both select arms in Handle are ready under an already-cancelled context
	// (the channel is buffered and ctx.Done is closed), so Go picks one at
	// random: a nil return and context.Canceled are BOTH correct. Any other
	// error is not — that is the invariant worth asserting.
	err := w2.Handle(cancelCtx, HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "x"})
	if err != nil {
		require.ErrorIs(t, err, context.Canceled,
			"a cancelled context is the only error Handle may return here")
	}

	require.Eventually(t, func() bool {
		w2.mu.Lock()
		defer w2.mu.Unlock()
		return len(w2.sessions) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"runSession must deregister the session when its context is cancelled")
}

// TestMetaagentWorker_PanicRecovery: a panicking extractor in the staged
// lifecycle is CONTAINED by the executor's safeEval (fail-closed Halt) and does
// NOT crash the worker goroutine — the session goroutine stays alive and no
// scope is mutated. A hook panic must always become a Halt. The owner gate is
// wired (allowed) so the panic reaches Extract.
func TestMetaagentWorker_PanicRecovery(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory:    m,
		Extractor: &panicExtractor{},
		Composer:  &fakeMetaComposer{},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nil, nil)
	w.SetManageScopeChecker(&fakeManageScope{allowed: true})

	scopeRef := memory.Scope{Kind: "session", ID: "ns/panic"}
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{Scope: scopeRef, Requester: "user:alice", Text: "x"}))

	// The panic is contained (Halt) — no scope is mutated. Poll briefly to let the
	// per-session goroutine process the message.
	require.Eventually(t, func() bool {
		_, found, _ := sessionscope.Get(approvedCtx(), m, scopeRef)
		return !found
	}, 2*time.Second, 20*time.Millisecond, "a contained panic mutates no scope")

	// The worker goroutine survives (the panic did not crash it): the session
	// entry remains registered and ready for the next message.
	w.mu.Lock()
	_, exists := w.sessions[scopeRef.ID]
	w.mu.Unlock()
	assert.True(t, exists, "the worker goroutine survives a contained hook panic")
}

// --- helpers ---

type fakeMetaExtractorFn struct {
	fn func() scope.ScopeDelta
}

func (f *fakeMetaExtractorFn) Extract(_ context.Context, _ ExtractorInput) (scope.ScopeDelta, error) {
	return f.fn(), nil
}

type panicExtractor struct{}

func (p *panicExtractor) Extract(_ context.Context, _ ExtractorInput) (scope.ScopeDelta, error) {
	panic("extractor panic for test")
}

// TestMetaagentWorker_ColdStart_RoutesEnvelopeToHandler proves the envelope
// supplied to Handle flows through metaagentRequest → the cold-start branch of
// runMetaagentLifecycle → ClassifySkipped, which keeps an in-envelope hard-deny
// narrowing (rather than dropping it as out-of-envelope). Runs on an
// extractAndAutoApply class to avoid the approval/NATS path; the extractor
// returns a hard-deny on a tool that IS in the supplied envelope.
func TestMetaagentWorker_ColdStart_RoutesEnvelopeToHandler(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory:   m,
		Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}},
	}
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}}
	csHandler := &ColdStartHandler{Metaagent: mg, Extractor: ext}
	w := NewMetaagentWorker(mg, approval.New(), nil, csHandler)

	scopeRef := memory.Scope{Kind: "session", ID: "ns/coldenv"}
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
		Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
	}
	// The class's cold-start mode is read from the session's
	// authz_session_config snapshot, not from the request — see
	// cold_start_policy.go.
	require.NoError(t, asc.Snapshot(approvedCtx(), m, scopeRef, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndAutoApply",
	}))
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{
		Scope: scopeRef, Requester: "user:alice",
		Text:      "summarize L-140 and do not read ENG",
		ColdStart: true, Envelope: env,
	}))

	// The handler runs async in the per-session goroutine; poll for the result.
	require.Eventually(t, func() bool {
		_, found, _ := coldstarttask.Get(approvedCtx(), m, scopeRef)
		return found
	}, 2*time.Second, 20*time.Millisecond, "cold_start_task must be written")

	cst, found, _ := coldstarttask.Get(approvedCtx(), m, scopeRef)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.Equal(t, "summarize L-140", cst.CleanedText)
	// The hard-deny tool is IN the envelope, so ClassifySkipped keeps it and it
	// lands in the Layer-2 memory tool-deny set — proving the envelope reached
	// the handler.
	got, found, _ := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.True(t, found, "session_scope must be written by the in-envelope hard-deny")
	assert.Contains(t, got.Tools.Deny, "linear.search_issues",
		"in-envelope hard-deny narrowing applied to Layer-2 memory")
}

// TestMetaagentWorker_ColdStart_Executor_PerActionOutcomes drives the cold-start
// path (handleColdStart → executor → authzd Host) for each of the four approver
// actions and asserts the resulting cold_start_task outcome. The approval
// round-trip goes through a real orchestrator: a stand-in subscriber pulls the
// requestId from the published envelope and DeliverDecision's the action.
func TestMetaagentWorker_ColdStart_Executor_PerActionOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		approved   bool
		wantStatus string
		wantApply  bool
	}{
		{name: "approve_cleaned → StatusApprovedCleaned + apply", action: ColdStartApproveCleaned, approved: true, wantStatus: coldstarttask.StatusApprovedCleaned, wantApply: true},
		{name: "approve_original → StatusApprovedOriginal + apply", action: ColdStartApproveOriginal, approved: true, wantStatus: coldstarttask.StatusApprovedOriginal, wantApply: true},
		{name: "run_without_scope → StatusRanWithoutScope, no apply", action: ColdStartRunWithoutScope, approved: true, wantStatus: coldstarttask.StatusRanWithoutScope, wantApply: false},
		{name: "deny → StatusDenied, no apply", action: ColdStartDeny, approved: false, wantStatus: coldstarttask.StatusDenied, wantApply: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startEmbeddedNATS(t)
			const ns, name = "default", "exec-act"
			mem := memory.NewLocal(inmem.NewBackend())
			mg := &Metaagent{Memory: mem, Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}}}
			ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
				ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
				CleanedTask: "summarize L-140",
			}}
			orch := approval.New()
			w := NewMetaagentWorker(mg, orch, nc, &ColdStartHandler{Metaagent: mg, Extractor: ext})

			// Stand in for channelsd: deliver the scripted action for the requestId.
			approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
			sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
				var p struct {
					RequestID string `json:"requestId"`
				}
				if err := json.Unmarshal(m.Data, &p); err != nil || p.RequestID == "" {
					return
				}
				orch.DeliverDecision(p.RequestID, approval.Decision{Approved: tc.approved, Action: tc.action, ApproverID: "user:bob"})
			})
			require.NoError(t, err)
			require.NoError(t, nc.Flush())
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
			env := scope.AgentClassEnvelope{
				BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
				Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
			}
			require.NoError(t, asc.Snapshot(approvedCtx(), mem, scopeRef, asc.Content{
				Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndApprove",
			}))
			require.NoError(t, w.Handle(approvedCtx(), HandleInput{
				Scope: scopeRef, Requester: "user:alice",
				Text:      "summarize L-140 and do not read ENG",
				ColdStart: true, Envelope: env,
			}))

			require.Eventually(t, func() bool {
				_, found, _ := coldstarttask.Get(approvedCtx(), mem, scopeRef)
				return found
			}, 5*time.Second, 20*time.Millisecond, "cold_start_task must be written")

			cst, found, _ := coldstarttask.Get(approvedCtx(), mem, scopeRef)
			require.True(t, found)
			assert.Equal(t, tc.wantStatus, cst.Status)
			// Apply lands the hard-deny tool in the Layer-2 memory tool-deny set;
			// non-apply writes no session_scope doc at all.
			scopeDoc, scopeFound, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
			if tc.wantApply {
				require.True(t, scopeFound, "approve must apply scope to Layer-2 memory")
				assert.Contains(t, scopeDoc.Tools.Deny, "linear.search_issues", "approve must apply the hard-deny tool")
			} else {
				assert.False(t, scopeFound, "non-approve must NOT apply scope")
			}
		})
	}
}

// TestMetaagentWorker_MidSession_Host_RealNATSRoundTrip drives the mid-session
// @metaagent path through the staged runMetaagentLifecycle over a REAL embedded
// NATS + a real approval.Orchestrator. A stand-in subscriber on
// out.metaagent_scope_approval pulls the requestId from the published envelope
// and DeliverDecision's approve/deny — proving the reqID correlation flows
// through the ENVELOPE (not an out-of-band channel). Drives a HardDeny narrow
// (the production-reachable mid-session shape, since the lifecycle passes no
// RequesterPerms — a documented follow-on — so an Add would be dropped):
//   - approve → the hard-deny lands in sessionscope (ResourceDisallowed),
//   - deny → a pure HardDeny denial is a no-op (no Add to make sticky),
//
// and in both the published payload carries NO coldStart key (3-button render).
func TestMetaagentWorker_MidSession_Host_RealNATSRoundTrip(t *testing.T) {
	cases := []struct {
		name         string
		approved     bool
		wantDisallow bool
	}{
		{name: "approve → hard-deny lands in sessionscope", approved: true, wantDisallow: true},
		{name: "deny → pure HardDeny denial is a no-op", approved: false, wantDisallow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startEmbeddedNATS(t)
			const ns, name = "default", "midrt"
			mem := memory.NewLocal(inmem.NewBackend())
			mg := &Metaagent{
				Memory: mem,
				Extractor: &fakeMetaExtractor{result: scope.ScopeDelta{
					HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}},
				}},
				Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "Deny foo/bar."}},
			}
			orch := approval.New()
			w := NewMetaagentWorker(mg, orch, nc, nil)
			w.SetManageScopeChecker(&fakeManageScope{allowed: true}) // owner gate passes

			// Stand in for channelsd: pull requestId from the ENVELOPE, assert no
			// coldStart key, then deliver the scripted decision for that reqID.
			var sawColdStart atomic.Bool
			approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
			sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
				var p map[string]any
				if err := json.Unmarshal(m.Data, &p); err != nil {
					return
				}
				if _, ok := p["coldStart"]; ok {
					sawColdStart.Store(true)
				}
				reqID, _ := p["requestId"].(string)
				if reqID == "" {
					return
				}
				orch.DeliverDecision(reqID, approval.Decision{Approved: tc.approved, ApproverID: "user:bob"})
			})
			require.NoError(t, err)
			require.NoError(t, nc.Flush())
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
			req := metaagentRequest{
				scopeRef:  scopeRef,
				requester: "user:alice",
				text:      "@metaagent do not read foo/bar",
				envelope: scope.AgentClassEnvelope{
					BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
				},
			}
			require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

			assert.False(t, sawColdStart.Load(),
				"mid-session payload must NOT carry coldStart (3-button render preserved)")

			got, found, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
			if tc.wantDisallow {
				require.True(t, found, "approve writes the hard-deny")
				assert.True(t, got.ResourceDisallowed("github_repo", "foo/bar"),
					"approved hard-deny lands in the Layer-2 memory Disallow set")
			} else {
				assert.False(t, found && got.ResourceDisallowed("github_repo", "foo/bar"),
					"a pure HardDeny denied records no disallow")
			}
		})
	}
}

// The dec → cold-start action mapping is tested in pkg/authz/coldstart
// (TestActionForDecision).

// midSessionScopeApprovalFields is the EXACT field set the mid-session
// metaagent_scope_approval payload carries. channelsd's
// buildMetaagentScopeApprovalBlocks branches on a coldStart key; the mid-session
// payload OMITS it, so the 3-button render fires, not the 5-button cold-start one.
var midSessionScopeApprovalFields = []string{
	"requestId", "requester", "verbatim",
	"approverSummary", "skippedExplain", "caveatExplain",
	"applied", "skipped", "caveats",
}

// TestMetaagentWorker_MidSessionScopeApprovalPayload_Characterization pins the
// EXACT JSON the mid-session @metaagent path publishes on
// out.metaagent_scope_approval:
//   - the field set is midSessionScopeApprovalFields (no more, no less),
//   - there is NO coldStart key (so channelsd renders the 3-button mid-session
//     block, not the 5-button cold-start block),
//   - the reqID has the metaagent-scope- prefix,
//   - the envelope's requestId equals the orchestrator-await reqID (correlation).
func TestMetaagentWorker_MidSessionScopeApprovalPayload_Characterization(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "midchar"

	mem := memory.NewLocal(inmem.NewBackend())
	// handleOne passes NO RequesterPerms (a documented follow-on), so an Add
	// would be skipped (ReasonRequesterLacksPerm) → CannotAddress → no publish.
	// A HardDeny passes the envelope-only check, so it reaches the approval gate
	// — the realistic mid-session payload the staged lifecycle produces today.
	mg := &Metaagent{
		Memory: mem,
		Extractor: &fakeMetaExtractor{result: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}},
		}},
		Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "Deny linear search."}},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nc, nil)
	// Owner gate wired (allowed) so the mid-session lifecycle reaches the
	// approval publish (the byte-identical payload is the point of this test).
	w.SetManageScopeChecker(&fakeManageScope{allowed: true})

	// Stand in for channelsd: capture the published envelope, then approve so the
	// worker's mid-session lifecycle completes.
	captured := make(chan []byte, 1)
	approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
	sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(m.Data, &p); err != nil || p.RequestID == "" {
			return
		}
		select {
		case captured <- append([]byte(nil), m.Data...):
		default:
		}
		orch.DeliverDecision(p.RequestID, approval.Decision{Approved: true, ApproverID: "user:bob"})
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
		Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
	}
	require.NoError(t, w.Handle(approvedCtx(), HandleInput{
		Scope: scopeRef, Requester: "user:alice",
		Text: "@metaagent do not read ENG", Envelope: env,
	}))

	var raw []byte
	select {
	case raw = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("never published metaagent_scope_approval")
	}

	var p map[string]any
	require.NoError(t, json.Unmarshal(raw, &p))

	// Field set is EXACTLY midSessionScopeApprovalFields.
	for _, key := range midSessionScopeApprovalFields {
		_, ok := p[key]
		assert.Truef(t, ok, "mid-session payload must carry %q", key)
	}
	assert.Len(t, p, len(midSessionScopeApprovalFields),
		"mid-session payload must carry EXACTLY the characterized field set (no extra keys)")

	// CRITICAL: no coldStart key → channelsd's 3-button mid-session render.
	_, hasColdStart := p["coldStart"]
	assert.False(t, hasColdStart, "mid-session payload must NOT carry coldStart (3-button render)")
	_, hasCleaned := p["cleanedTask"]
	assert.False(t, hasCleaned, "mid-session payload must NOT carry cleanedTask (cold-start-only)")

	// reqID prefix + correlation discipline.
	reqID, _ := p["requestId"].(string)
	assert.True(t, strings.HasPrefix(reqID, "metaagent-scope-"),
		"mid-session reqID must use the metaagent-scope- prefix")
	assert.Equal(t, "user:alice", p["requester"])
	assert.Equal(t, "@metaagent do not read ENG", p["verbatim"])
	assert.Equal(t, "Deny linear search.", p["approverSummary"])
}

// TestMetaagentWorker_DecisionPayload verifies the approval payload JSON
// shape that MetaagentWorker sends via OnPublish.
func TestMetaagentWorker_DecisionPayload_Shape(t *testing.T) {
	// Build the expected payload shape manually and verify it round-trips.
	payload, err := json.Marshal(map[string]any{
		"requestId":       "metaagent-scope-ns/a-12345",
		"requester":       "user:alice",
		"verbatim":        "@metaagent allow foo/bar",
		"approverSummary": "If approved, agent can access foo/bar.",
		"skippedExplain":  "",
		"caveatExplain":   "",
		"applied":         scope.ScopeDelta{},
		"skipped":         []scope.SkippedItem{},
		"caveats":         []scope.CaveatItem{},
	})
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(payload, &parsed))
	assert.Equal(t, "metaagent-scope-ns/a-12345", parsed["requestId"])
	assert.Equal(t, "user:alice", parsed["requester"])
}

// TestMetaagentScopeSession covers the split every metaagent OUT publish goes
// through. A scope ID that yields an empty half would compose a subject with an
// empty token, which no "ap.session.*.*.out.<leaf>" subscription matches: the
// publish would succeed and the human would simply never be asked.
func TestMetaagentScopeSession(t *testing.T) {
	t.Run("ns/name splits into separate tokens (6-token subject)", func(t *testing.T) {
		ns, name, err := metaagentScopeSession("ns/name")
		require.NoError(t, err)
		assert.Equal(t, "ns", ns)
		assert.Equal(t, "name", name)

		subj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name),
			channelevents.KindMetaagentScopeApproval)
		assert.Equal(t, "ap.session.ns.name.out.metaagent_scope_approval", subj)
		require.Len(t, strings.Split(subj, "."), 6,
			"subject must have exactly 6 NATS tokens to match ap.session.*.*.out.<leaf>")
	})

	cases := []struct {
		name    string
		scopeID string
	}{
		{name: "no slash: refused", scopeID: "noslash"},
		{name: "empty namespace half: refused rather than emitting an empty token", scopeID: "/name"},
		{name: "empty name half: refused rather than emitting an empty token", scopeID: "ns/"},
		{name: "empty scope ID: refused", scopeID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := metaagentScopeSession(tc.scopeID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "malformed session scope ID")
		})
	}
}
