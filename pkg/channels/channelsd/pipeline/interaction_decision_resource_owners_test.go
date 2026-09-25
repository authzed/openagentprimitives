package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeResourceEngine replaces the whole engine.Engine (a field on Pipeline) so
// the resource-owner path's ONLY engine call, CheckApproverAuthorized, can be
// observed directly: which resource slice it received AND whether it was
// consulted at all (the fail-closed path must NEVER reach it). The embedded
// nil engine.Engine panics on any other method — none is on this path.
type fakeResourceEngine struct {
	engine.Engine
	ok           bool
	consulted    bool
	gotResources []authz.ApproverResourceRef
}

func (f *fakeResourceEngine) CheckApproverAuthorized(_ context.Context, _ authz.SessionRef, resources []authz.ApproverResourceRef, _ identity.CanonicalUserID) (bool, error) {
	f.consulted = true
	f.gotResources = resources
	return f.ok, nil
}

// newTestMemory builds an in-process memory.Memory for the durable
// memapproval record the resource-owner gate reads on a cache miss. The
// facade's read/write capability doors are satisfied per-call with a system
// approval on the context (see WithSystemApproval below), mirroring the way
// production channelsd authenticates to the operator's memory over its token.
func newTestMemory(t *testing.T) memory.Memory {
	t.Helper()
	// System-approved: the pipeline calls the memory facade with whatever
	// context it was handed, and the capability doors on Local would otherwise
	// refuse a bare test context. Production carries its own token instead.
	return sysApproved{inner: memory.NewLocal(inmem.NewBackend())}
}

type sysApproved struct{ inner memory.Memory }

func (m sysApproved) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	return m.inner.Put(memory.WithSystemApproval(ctx, "test"), e)
}
func (m sysApproved) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return m.inner.Query(memory.WithSystemApproval(ctx, "test"), q)
}
func (m sysApproved) Search(ctx context.Context, r memory.SearchRequest) (memory.MergedSearchResult, error) {
	return m.inner.Search(memory.WithSystemApproval(ctx, "test"), r)
}
func (m sysApproved) SendSignal(ctx context.Context, s memory.Signal) error {
	return m.inner.SendSignal(memory.WithSystemApproval(ctx, "test"), s)
}

// sawPublishedKind reports whether natsRec recorded any envelope whose subject
// ends in the given kind (either .in or .out).
func sawPublishedKind(natsRec *fakeNATS, k channelevents.Kind) bool {
	for _, s := range natsRec.subjects {
		if strings.HasSuffix(s, "."+string(k)) {
			return true
		}
	}
	return false
}

// findInteractionDecisionRejected returns the first OUT
// KindInteractionDecisionRejected payload natsRec recorded, or fails the test.
func findInteractionDecisionRejected(t *testing.T, natsRec *fakeNATS) channelevents.InteractionDecisionRejectedPayload {
	t.Helper()
	for i, s := range natsRec.subjects {
		if !strings.HasSuffix(s, ".out."+string(channelevents.KindInteractionDecisionRejected)) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(natsRec.payloads[i], &env), "unmarshal envelope")
		var pl channelevents.InteractionDecisionRejectedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal rejected payload")
		return pl
	}
	t.Fatalf("no interaction_decision_rejected envelope found; subjects=%v", natsRec.subjects)
	return channelevents.InteractionDecisionRejectedPayload{}
}

// TestHandleInteractionDecision_DecideResourceOwners exercises the
// resource-owner decision gate across the four resolution paths — warm cache,
// cross-restart durable recovery, the unrecoverable fail-closed reject, and the
// warm-but-empty gate — plus a not-an-owner reject. The load-bearing invariant:
// when the resource set is unrecoverable the pipe fails CLOSED (deny + a
// published rejection) and NEVER consults the approver gate with an empty set,
// which would fold into the session approve-set (the 2026-07-02 regression).
func TestHandleInteractionDecision_DecideResourceOwners(t *testing.T) {
	cases := []struct {
		name                string
		cacheRequest        bool
		durableRecord       bool
		resources           []channelevents.InteractionResourceRef
		ownerOK             bool
		wantApplied         bool
		wantResources       []authz.ApproverResourceRef
		wantEngineConsulted bool
		wantRejected        bool
	}{
		{
			name:                "warm cache: resolves Resources from the cached request, owner approves",
			cacheRequest:        true,
			resources:           []channelevents.InteractionResourceRef{{Type: "data", ID: "d1"}},
			ownerOK:             true,
			wantApplied:         true,
			wantResources:       []authz.ApproverResourceRef{{Type: "data", ID: "d1"}},
			wantEngineConsulted: true,
		},
		{
			name:                "cold cache: recovers Resources from the durable memapproval record, owner approves",
			durableRecord:       true,
			resources:           []channelevents.InteractionResourceRef{{Type: "data", ID: "d1"}},
			ownerOK:             true,
			wantApplied:         true,
			wantResources:       []authz.ApproverResourceRef{{Type: "data", ID: "d1"}},
			wantEngineConsulted: true,
		},
		{
			name:                "unrecoverable (no cache, no durable): fail-closed reject, engine untouched, no applied",
			ownerOK:             true, // irrelevant — the engine must never be consulted
			wantApplied:         false,
			wantResources:       nil,
			wantEngineConsulted: false,
			wantRejected:        true,
		},
		{
			name:                "warm cache, empty Resources: folds to the session gate (empty-but-non-nil slice handed)",
			cacheRequest:        true,
			resources:           nil,
			ownerOK:             true,
			wantApplied:         true,
			wantResources:       []authz.ApproverResourceRef{},
			wantEngineConsulted: true,
		},
		{
			name:                "warm cache, decider is not a resource owner: fail-closed reject, prompt survives",
			cacheRequest:        true,
			resources:           []channelevents.InteractionResourceRef{{Type: "data", ID: "d1"}},
			ownerOK:             false,
			wantApplied:         false,
			wantResources:       []authz.ApproverResourceRef{{Type: "data", ID: "d1"}},
			wantEngineConsulted: true,
			wantRejected:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerInteractionCategory(t, channelinteractions.DecideResourceOwners)
			calls := 0
			channelinteractions.Bind(fixtureInteractionCategory,
				trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))

			cli := newFakeK8sClientWithExistingSession(t, "U_OWNER")
			sessKey := sessKeyForFixture()
			natsRec := &fakeNATS{}
			p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
			fe := &fakeResourceEngine{ok: tc.ownerOK}
			p.Engine = fe
			// ONE facade: the memapproval record and the parked prompt are both
			// read back through p.Mem, so two instances here would silently make
			// the cross-restart recovery case test nothing.
			mem := newTestMemory(t)
			p.Mem = mem

			// The in-process memory facade enforces the read/write capability
			// doors; a system approval on ctx stands in for channelsd's
			// operator-memory credential in production (an HTTP-backed facade).
			ctx := memory.WithSystemApproval(context.Background(), "test")

			if tc.cacheRequest {
				notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1",
					channelevents.InteractionRequestPayload{Resources: tc.resources})
			}
			if tc.durableRecord {
				require.NoError(t, memapproval.RecordRequest(ctx, p.Mem,
					memory.Scope{Kind: "session", ID: sessKey.Namespace + "/" + sessKey.Name}, "use-1",
					memapproval.Request{RequestID: "req-1", Resources: tc.resources}),
					"seed durable memapproval record")
			}

			decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"}
			env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", decider)
			require.NoError(t, p.HandleInteractionDecision(ctx, env), "HandleInteractionDecision")

			assert.Equal(t, tc.wantApplied, sawPublishedKind(natsRec, channelevents.KindInteractionApplied),
				"interaction_applied published?")
			assert.Equal(t, tc.wantResources, fe.gotResources,
				"resource slice handed to CheckApproverAuthorized (nil when the engine was never consulted)")
			assert.Equal(t, tc.wantEngineConsulted, fe.consulted,
				"engine consulted? (the unrecoverable path must NEVER reach the approver gate)")
			assert.Equal(t, tc.wantRejected, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected),
				"interaction_decision_rejected published?")

			if tc.wantRejected {
				rej := findInteractionDecisionRejected(t, natsRec)
				assert.Equal(t, "not_authorized", rej.Class, "rejection Class")
				assert.Equal(t, fixtureInteractionCategory, rej.Category, "rejection Category")
				assert.Equal(t, "req-1", rej.RequestRef, "rejection RequestRef")
				assert.Equal(t, 0, calls, "the bound handler must not run on a fail-closed reject")
			}

			// Prompt-survival invariant: an applied decision clears its cached
			// prompt; a rejected click leaves it parked so the real approver can
			// still resolve it. (Only meaningful when a prompt was cached.)
			if tc.cacheRequest {
				got := outstandingPrompts(t, p, sessKey)
				if tc.wantApplied {
					assert.Empty(t, got, "an applied decision must clear its cached prompt")
				} else {
					assert.Len(t, got, 1, "a rejected click must leave the prompt parked for the real approver")
				}
			}
		})
	}
}
