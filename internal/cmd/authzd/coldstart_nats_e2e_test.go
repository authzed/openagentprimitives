package main

// coldstart_nats_e2e_test.go
//
// Real-NATS round-trip for the cold-start metaagent_request flow. This is the
// integration seam the unit-with-fakes tests missed: it proves the runner's
// concrete publish subject
//
//	ap.session.<ns>.<name>.in.metaagent_request
//
// actually matches authzd's subscription pattern
//
//	ap.session.*.*.in.metaagent_request
//
// over a REAL embedded nats-server (not a fake conn), and that the full
// MetaagentWorker → ColdStartHandler → cold_start_task path runs end-to-end.
// The live break we hit (authzd pointed at the wrong NATS endpoint) manifested
// as "authzd never received it"; a subject-format regression would manifest the
// same way, and this test catches the in-code half of that class.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
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

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// approvedCtx returns a context carrying a system-source memory approval, the
// sanctioned way to authorize a trusted in-process caller (authzd) against the
// capability-gated memory data plane in these component tests.
func approvedCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

// approvedMem wraps a memory.Memory so every operation carries a system-source
// approval. Components under test that mint their own context internally — the
// Worker's per-session goroutine (context.Background) and the /debug HTTP
// handler (r.Context) — cannot inherit the test's approvedCtx, so the approval
// must ride on the Memory they hold. This mirrors test/e2e's sysApprovedMem and
// stands in for the operator httpsrv's per-request capability mint.
type approvedMem struct{ inner memory.Memory }

func (m approvedMem) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	return m.inner.Put(memory.WithSystemApproval(ctx, "test"), e)
}
func (m approvedMem) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return m.inner.Query(memory.WithSystemApproval(ctx, "test"), q)
}
func (m approvedMem) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	return m.inner.Search(memory.WithSystemApproval(ctx, "test"), req)
}
func (m approvedMem) SendSignal(ctx context.Context, sig memory.Signal) error {
	return m.inner.SendSignal(memory.WithSystemApproval(ctx, "test"), sig)
}

// startEmbeddedNATS boots an in-process nats-server with no auth (sufficient for
// a same-process subject-routing round-trip) and returns a connected client.
func startEmbeddedNATS(t *testing.T) *natsgo.Conn {
	t.Helper()
	srv := natstest.RunServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	})
	t.Cleanup(srv.Shutdown)

	nc, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err, "connect to embedded NATS")
	t.Cleanup(nc.Close)
	return nc
}

// TestColdStartMetaagentRequest_RealNATSRoundTrip wires a real MetaagentWorker
// onto the same subscription pattern as internal/cmd/authzd/main.go, publishes a
// cold-start metaagent_request on the EXACT subject the runner uses, and asserts
// the worker writes a cold_start_task to memory for that session.
func TestColdStartMetaagentRequest_RealNATSRoundTrip(t *testing.T) {
	nc := startEmbeddedNATS(t)

	const (
		ns       = "default"
		name     = "rt-sess-1"
		rawText  = "summarize L-140 and do not read ENG"
		cleaned  = "summarize L-140"
		denyTool = "linear.search_issues"
	)

	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory:    mem,
		Extractor: &fakeMetaExtractor{},
		Composer:  &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}},
	}
	// Cold-start extractor returns a known delta + cleaned task.
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{denyTool}}},
		CleanedTask: cleaned,
	}}
	coldStartHandler := &ColdStartHandler{Metaagent: mg, Extractor: ext}

	orch := approval.New()
	maWorker := NewMetaagentWorker(mg, orch, nc, coldStartHandler)

	// The worker publishes a metaagent_scope_approval envelope to channelsd's
	// out-subject and awaits a decision. We stand in for channelsd: subscribe to
	// that subject, pull the requestId, and deliver an approve_cleaned decision
	// back through the orchestrator, driving the real path rather than bypassing it.
	approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
	approvalSub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(m.Data, &p); err != nil || p.RequestID == "" {
			return
		}
		orch.DeliverDecision(p.RequestID, approval.Decision{Approved: true, Action: ColdStartApproveCleaned, ApproverID: "user:alice"})
	})
	require.NoError(t, err, "subscribe to scope_approval out-subject")
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = approvalSub.Unsubscribe() })

	// Wire the metaagent_request subscription through the SAME callback
	// internal/cmd/authzd/main.go installs, so the payload decoding under test is
	// production's and cannot drift from a copy kept here.
	maSubj := channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindMetaagentRequest)
	maSub, err := nc.Subscribe(maSubj, metaagentRequestHandler(approvedCtx(), maWorker))
	require.NoError(t, err, "subscribe to metaagent_request pattern")
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = maSub.Unsubscribe() })

	// The class's cold-start mode comes from the session's authz_session_config
	// snapshot (cold_start_policy.go), not from the request — this class asks a
	// human, which is the round-trip the test drives.
	require.NoError(t, asc.Snapshot(approvedCtx(), mem,
		memory.Scope{Kind: "session", ID: ns + "/" + name}, asc.Content{
			Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndApprove",
		}))

	// Publish on the runner's CONCRETE subject — the exact format from
	// internal/cmd/runner/main.go: ap.session.<ns>.<name>.in.metaagent_request.
	runnerSubj := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentRequest)
	reqPayload, err := json.Marshal(map[string]any{
		"requester": "user:alice",
		"text":      rawText,
		"coldStart": true,
		"envelope": scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
			Tools:         []scope.EnvelopeTool{{Name: denyTool}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(runnerSubj, reqPayload))
	require.NoError(t, nc.Flush())

	// Poll for the cold_start_task: its presence proves the publish subject
	// matched the subscription pattern over real NATS AND the full worker path
	// ran to completion.
	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	var (
		cst   coldstarttask.Content
		found bool
	)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, ok, gerr := coldstarttask.Get(approvedCtx(), mem, scopeRef)
		require.NoError(t, gerr)
		if ok {
			cst, found = c, true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.True(t, found, "cold_start_task must appear in memory: runner subject matched authzd subscription over real NATS")
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status, "approve_cleaned decision applied")
	assert.Equal(t, cleaned, cst.CleanedText, "cleaned task carried through the round-trip")
}

// natsSubjectMatches reports whether subject matches the NATS subscription
// pattern. Token-split on "."; "*" matches exactly one token; ">" matches one
// or more trailing tokens; otherwise tokens must be equal and counts must match.
// This is a focused reimplementation of NATS subject matching sufficient for the
// contract assertion below.
func natsSubjectMatches(pattern, subject string) bool {
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			// ">" must be the last token and matches one-or-more remaining.
			return i == len(pt)-1 && i < len(st)
		}
		if i >= len(st) {
			return false
		}
		if p == "*" {
			continue
		}
		if p != st[i] {
			return false
		}
	}
	return len(pt) == len(st)
}

// TestColdStartRequestSubjectMatchesAuthzdSubscription is a pure subject-contract
// guard: the runner's concrete publish subject must match authzd's subscription
// pattern. This catches token-count drift (the 5-vs-6-token class) without any
// NATS server.
func TestColdStartRequestSubjectMatchesAuthzdSubscription(t *testing.T) {
	const authzdPattern = "ap.session.*.*.in.metaagent_request" // internal/cmd/authzd/main.go

	cases := []struct {
		name    string
		subject string
		want    bool
	}{
		{
			name:    "runner concrete subject matches the authzd pattern",
			subject: channelevents.SubjectIn(channelevents.SubjectPrefix("ns", "sess-1"), channelevents.KindMetaagentRequest),
			want:    true,
		},
		{
			name:    "missing the name token (5 tokens) does NOT match",
			subject: "ap.session.ns.in.metaagent_request",
			want:    false,
		},
		{
			name:    "wrong leaf does NOT match",
			subject: "ap.session.ns.sess-1.in.user_message",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, natsSubjectMatches(authzdPattern, tc.subject))
		})
	}

	// Sanity-check the matcher itself against known NATS semantics.
	assert.True(t, natsSubjectMatches("ap.>", "ap.session.ns.name.in.metaagent_request"), "'>' matches trailing tokens")
	assert.False(t, natsSubjectMatches("ap.session.*", "ap.session.ns.name"), "'*' matches exactly one token")
}
