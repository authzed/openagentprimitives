package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestResurfacePendingMatrix exercises resurfacePending's non-approval
// baseline: Running is a no-op, because the enqueue-ack path owns that phase.
// Every re-surfaceable prompt flows through the category-generic leg
// (content_inspection, tool_approval, info_leakage at AwaitingDecision;
// identity_choice at AwaitingIdentityChoice; credential_link at
// AwaitingCredentials) — there is no per-category tail here, which is what
// makes a new category a registry row rather than a branch. The generic-leg
// republish (durable stale-guard + interrupt id) is covered by
// TestResurfacePending_GenericInteractionLeg and
// TestResurfacePending_RegenerateLeg.
func TestResurfacePendingMatrix(t *testing.T) {
	newPipe := func(mem memory.Memory) (*Pipeline, *fakeNATS) {
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats, Now: func() time.Time { return time.Now().UTC() }}
		return p, nats
	}
	sess := func(phase string) *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
		}
	}

	t.Run("Running: no-op (handled by enqueue-ack path)", func(t *testing.T) {
		mem := newTestMemory(t)
		p, nats := newPipe(mem)
		p.resurfacePending(context.Background(), sess(spiceboxv1alpha1.AgentSessionPhaseRunning))
		assert.Empty(t, nats.subjects)
	})
}

// registerRegenerateCategory resets the registry + regenerator bindings then
// registers fixtureRegenerateCategory, parked at AwaitingCredentials with
// Resurface: ResurfaceRegenerate — the credential_link shape. Mirrors
// interaction_decision_test.go's registerInteractionCategory but for the
// regenerate policy; kept in this file since it's resurfacePending-specific
// and no other test in the package needs a regenerate-policy fixture.
const fixtureRegenerateCategory = "fixture_regen_cat"

func registerRegenerateCategory(t *testing.T) channelinteractions.Category {
	t.Helper()
	// The category registry (unlike bindings/regenerators, which the binary
	// wires at startup rather than registering via init()) holds
	// init()-registered production categories, so a bare Reset() would
	// clobber them for the rest of the test binary — snapshot before
	// clearing and restore in Cleanup.
	saved := channelinteractions.All()
	channelinteractions.Reset()
	channelinteractions.ResetBindings()
	channelinteractions.ResetRegenerators()
	t.Cleanup(func() {
		channelinteractions.Reset()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
		channelinteractions.ResetBindings()
		channelinteractions.ResetRegenerators()
	})
	c := channelinteractions.Category{
		Name:      fixtureRegenerateCategory,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester,
		Resurface: channelinteractions.ResurfaceRegenerate,
	}
	channelinteractions.Register(c)
	return c
}

// TestResurfacePending_RegenerateLeg verifies the ResurfaceRegenerate branch
// of the category-generic leg (the credential_link shape): a cached
// interaction_request parked at AwaitingCredentials calls the bound
// Regenerator with the session and the decoded cached payload, and does NOT
// also republish the cached envelope verbatim (that's ResurfaceCached's
// job — regenerate categories always rebuild). A category with no bound
// regenerator is skipped without panicking or publishing.
func TestResurfacePending_RegenerateLeg(t *testing.T) {
	sessKey := client.ObjectKey{Namespace: "ns", Name: "s"}
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials},
	}

	t.Run("NOTHING stored: the regenerator still runs, driven by the phase alone", func(t *testing.T) {
		// The durability property in one test. Storage is deliberately empty —
		// as it is for every credential_link prompt, and as it is for ANY
		// category after a channelsd restart — and the prompt must still be
		// rebuilt, because the regenerate leg is driven by the session's phase
		// via the registry, not by iterating previously-cached prompts.
		c := registerRegenerateCategory(t)
		mem := newTestMemory(t)

		var calls int
		var gotSess *spiceboxv1alpha1.AgentSession
		var gotReq *channelevents.InteractionRequestPayload
		channelinteractions.BindRegenerator(c.Name, func(_ context.Context, sess *spiceboxv1alpha1.AgentSession, req *channelevents.InteractionRequestPayload) error {
			calls++
			gotSess = sess
			gotReq = req
			return nil
		})

		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		p.resurfacePending(context.Background(), s)

		assert.Equal(t, 1, calls, "the bound regenerator must be invoked exactly once")
		assert.Same(t, s, gotSess)
		require.NotNil(t, gotReq)
		assert.Equal(t, c.Name, gotReq.Category, "the regenerator is told which category to rebuild")
		assert.Equal(t, sessKey.Name, gotReq.AgentSessionRef.Name)
		assert.Empty(t, nats.subjects, "regenerate categories rebuild via the regenerator, never republish a stored envelope")
	})

	t.Run("wrong phase: the regenerator is not run", func(t *testing.T) {
		c := registerRegenerateCategory(t) // parks at AwaitingCredentials
		mem := newTestMemory(t)
		var calls int
		channelinteractions.BindRegenerator(c.Name, func(context.Context, *spiceboxv1alpha1.AgentSession, *channelevents.InteractionRequestPayload) error {
			calls++
			return nil
		})
		running := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
		}

		p := &Pipeline{Mem: mem, NATS: &fakeNATS{}}
		p.resurfacePending(context.Background(), running)

		assert.Zero(t, calls, "a session that is not parked on this category must not be re-prompted")
	})

	t.Run("regenerator error is logged, not fatal", func(t *testing.T) {
		c := registerRegenerateCategory(t)
		mem := newTestMemory(t)
		channelinteractions.BindRegenerator(c.Name, func(context.Context, *spiceboxv1alpha1.AgentSession, *channelevents.InteractionRequestPayload) error {
			return errors.New("mint failed")
		})

		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		assert.NotPanics(t, func() { p.resurfacePending(context.Background(), s) })
		assert.Empty(t, nats.subjects)
	})

	t.Run("no bound regenerator: skipped without panic or publish", func(t *testing.T) {
		registerRegenerateCategory(t)
		mem := newTestMemory(t)

		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		assert.NotPanics(t, func() { p.resurfacePending(context.Background(), s) })
		assert.Empty(t, nats.subjects)
	})

	t.Run("no memory facade: the regenerate leg still runs", func(t *testing.T) {
		// The best property of the split: credential_link — the category that
		// produced the original bug — recovers even when the storage this design
		// added is entirely unavailable, because it reads none of it.
		c := registerRegenerateCategory(t)
		var calls int
		channelinteractions.BindRegenerator(c.Name, func(context.Context, *spiceboxv1alpha1.AgentSession, *channelevents.InteractionRequestPayload) error {
			calls++
			return nil
		})

		p := &Pipeline{NATS: &fakeNATS{}} // Mem deliberately nil
		assert.NotPanics(t, func() { p.resurfacePending(context.Background(), s) })
		assert.Equal(t, 1, calls, "a storage-free category must not depend on storage being wired")
	})
}

// A ResurfaceRegenerate category stores nothing at all — the regenerate leg is
// driven by the session's phase via channelinteractions.RegenerateAtPark, never
// by iterating stored prompts — so no stale entry can accumulate and the
// re-mint doubling (1 -> 2 -> 4 -> 8) has no mechanism.
// TestResurfacePending_RegenerateLeg's bounded call count is what pins it.

// TestResurfacePending_GenericInteractionLeg verifies the category-generic
// leg: a parked KindInteractionRequest whose category is registered, Parks at
// the session's current phase, and is still listed in PendingInteractions is
// republished — with a fresh interrupt id when the payload says Interruptible.
// One whose category isn't registered is left alone; that is defensive, since
// a process may see a category it does not know about and must neither panic
// nor republish blindly. The stale-guard itself (a parked prompt whose
// decision already landed) is covered by
// TestResurface_InteractionLeg_DurableCrossCheck.
func TestResurfacePending_GenericInteractionLeg(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers) // Park: AwaitingDecision, Resurface: ResurfaceCached

	mem := newTestMemory(t)
	sessKey := client.ObjectKey{Namespace: "ns", Name: "s"}
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_X", Email: "x@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "live-interaction", channelevents.InteractionRequestPayload{
		Interruptible: true,
		Audience:      channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
	})
	notePendingInteractionRequest(t, mem, sessKey, "not_registered", "unknown-cat", channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
	})

	nats := &fakeNATS{}
	p := &Pipeline{Mem: mem, NATS: nats}
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision},
	}
	// The AwaitingDecision park cross-checks Status.PendingInteractions, so
	// "live-interaction" must be listed there to be resurfaced at all.
	s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
		{RequestID: "live-interaction", RequestRef: "live-interaction", Category: fixtureInteractionCategory, RequestedAt: metav1.Now()},
	}
	p.resurfacePending(context.Background(), s)

	require.Len(t, nats.subjects, 1, "only the registered, correctly-parked category republishes")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(nats.payloads[0], &env), "unmarshal republished envelope")
	assert.NotEmpty(t, env.ResurfaceInterruptRequestID, "interruptible interaction prompt gets a fresh interrupt id")
}

// TestResurfacePending_IgnoresProviderRetryAndQueued pins the two categories
// that must never be resurfaced. A parked
// KindInteractionRequest(category=provider_error_retry) sitting at
// AwaitingRetry must NOT be republished: the category is registered with
// Park:"" precisely because AwaitingRetry is a runner/operator-owned phase,
// not an interaction park state, and resurfacePending's `cat.Park == ""`
// short-circuit skips it regardless of the session's current phase.
// queued_messages' enqueue-ack fires mid-turn while the session is still
// Running and is never stored under KindInteractionRequest at all.
func TestResurfacePending_IgnoresProviderRetryAndQueued(t *testing.T) {
	sessKey := client.ObjectKey{Namespace: "ns", Name: "s"}

	t.Run("provider_error_retry cached while AwaitingRetry: no republish (Park is deliberately empty)", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, categories.ProviderErrorRetry, "retry-1",
			channelevents.InteractionRequestPayload{
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			})
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		s := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry},
		}
		p.resurfacePending(context.Background(), s)
		assert.Empty(t, nats.subjects, "provider_error_retry must not be resurfaced: its category registers Park:\"\", which the generic leg always skips")
	})

	t.Run("Running: nothing is resurfaced, whatever is stored", func(t *testing.T) {
		// queued_messages is the enqueue-ack's category and declares Park:"", so
		// it is never recorded in the first place — but the assertion that
		// matters is that Running resurfaces NOTHING regardless: that phase is
		// Deliver's enqueue-ack path, not resurfacePending's.
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, categories.QueuedMessages, "enqueue-1",
			channelevents.InteractionRequestPayload{
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			})
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		s := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
		}
		p.resurfacePending(context.Background(), s)
		assert.Empty(t, nats.subjects, "Running is the enqueue-ack path's own phase, owned by Deliver, not resurfacePending")
	})
}

// TestResurfacePending_AsleepWhileDecisionPending pins the case observed live
// on oap-desktop: the builder asked for a plan-phase approval, the runner then
// went to sleep (session phase Idle) with the request still listed in
// Status.PendingInteractions, the person reloaded the page, and the resurface
// skipped the prompt because its category parks at AwaitingDecision and the
// session's phase was Idle. The ask still stood — nothing could answer it
// but a reload, and the reload showed nothing. The durable fact is the
// PendingInteractions entry: while it lists the request, the prompt
// resurfaces whatever phase the sleeping runner left behind; once it is
// gone, the existing already-landed guard still holds.
func TestResurfacePending_AsleepWhileDecisionPending(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers) // Park: AwaitingDecision, Resurface: ResurfaceCached
	approver := channelevents.ExternalIdentity{Kind: "browser", ExternalID: "U_X", Email: "x@example.com"}
	mkSession := func(pending bool) *spiceboxv1alpha1.AgentSession {
		s := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
		}
		if pending {
			s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
				{RequestID: "asleep-ask", RequestRef: "asleep-ask", Category: fixtureInteractionCategory, RequestedAt: metav1.Now()},
			}
		}
		return s
	}

	t.Run("idle with the ask still pending: republished", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, client.ObjectKey{Namespace: "ns", Name: "s"}, fixtureInteractionCategory, "asleep-ask", channelevents.InteractionRequestPayload{
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
		})
		nats := &fakeNATS{}
		(&Pipeline{Mem: mem, NATS: nats}).resurfacePending(context.Background(), mkSession(true))
		require.Len(t, nats.subjects, 1, "a decision the session is asleep on must resurface to a newly attached surface")
	})
	t.Run("idle with the ask already answered: nothing republished", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, client.ObjectKey{Namespace: "ns", Name: "s"}, fixtureInteractionCategory, "asleep-ask", channelevents.InteractionRequestPayload{
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
		})
		nats := &fakeNATS{}
		(&Pipeline{Mem: mem, NATS: nats}).resurfacePending(context.Background(), mkSession(false))
		assert.Empty(t, nats.subjects, "the already-landed guard still holds while asleep")
	})
}
