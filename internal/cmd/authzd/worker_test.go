package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	eex "github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	turnkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

type fakeExtractor struct {
	calls       int32
	inflight    int32
	maxInFlight int32
	delay       time.Duration
	respond     func(in extract.ExtractInput) ([]extract.ExtractedEntity, error)
}

func (f *fakeExtractor) Extract(ctx context.Context, in extract.ExtractInput) ([]extract.ExtractedEntity, error) {
	atomic.AddInt32(&f.calls, 1)
	now := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	for {
		max := atomic.LoadInt32(&f.maxInFlight)
		if now > max {
			if atomic.CompareAndSwapInt32(&f.maxInFlight, max, now) {
				break
			}
			continue
		}
		break
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.respond != nil {
		return f.respond(in)
	}
	return []extract.ExtractedEntity{
		{ResourceType: "github_repo", ResourceID: "authzed/spicedb"},
	}, nil
}

// seedSession writes an authz_session_config entry (with a github_repo
// BoundEntityType) and one inbox turn at the given index. Tests that need
// extraction to run must call this before Handle.
func seedSession(t *testing.T, mem memory.Memory, scope memory.Scope, turnIdx int, text string) {
	t.Helper()
	require.NoError(t, asc.Snapshot(approvedCtx(), mem, scope, asc.Content{
		Subject: "alice",
		BoundEntities: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"},
		},
	}))
	require.NoError(t, turnkind.RecordInbox(approvedCtx(), mem, scope, turnIdx, text, ""))
}

func TestWorker_WakesAndExtracts(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	// Seed config + an inbox turn containing a github_repo reference so
	// the prefilter passes and the extractor is actually called.
	seedSession(t, mem, scope, 1, "open authzed/spicedb please")
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	// Wait for the per-session goroutine to process; poll for the
	// extraction_state{complete}.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&ext.calls) >= 1
	}, 2*time.Second, 20*time.Millisecond)
}

func TestWorker_PerSessionSerialization(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	// Seed one inbox turn; repeated Handle calls wake the goroutine but
	// the already-processed turn produces no additional Extract calls.
	// Use a message with a repo slug so prefilter passes.
	seedSession(t, mem, scope, 1, "review authzed/spicedb pr 42")
	ext := &fakeExtractor{delay: 100 * time.Millisecond}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})
	for i := 0; i < 3; i++ {
		require.NoError(t, w.Handle(approvedCtx(), scope))
	}
	// Only 1 extract call expected: the turn has a stable ID so after the
	// first run the extraction_state is written and findUnprocessedInbox
	// returns nothing on subsequent wakes.
	require.Eventually(t, func() bool { return atomic.LoadInt32(&ext.calls) >= 1 }, 2*time.Second, 20*time.Millisecond)
	// Allow time for the remaining two wakes to drain (they should be no-ops).
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&ext.calls),
		"per-session goroutine must not re-extract already-processed turns")
	assert.Equal(t, int32(1), atomic.LoadInt32(&ext.maxInFlight),
		"per-session goroutine must serialize Extract calls")
}

func TestWorker_PerSessionIdleExit(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	// First call: seed + process. Second call: add a new turn so the
	// fresh goroutine has work to do.
	seedSession(t, mem, scope, 1, "open authzed/spicedb please")
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: 100 * time.Millisecond})
	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool { return atomic.LoadInt32(&ext.calls) == 1 }, 1*time.Second, 20*time.Millisecond)
	// After idle expiry the goroutine should exit. We verify by re-Handling
	// after seeding a second inbox turn and observing a fresh goroutine
	// takes over (calls increments to 2).
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, turnkind.RecordInbox(approvedCtx(), mem, scope, 2, "look at example/repo now", ""))
	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool { return atomic.LoadInt32(&ext.calls) == 2 }, 1*time.Second, 20*time.Millisecond)
}

// TestWorker_ReleaseSession covers the idle-exit decision in isolation. Handle
// hands a wake-up to a channel it read out of w.sessions; if the idle timer
// fires in that window the goroutine must not retire the registration, or the
// buffered wake-up is stranded in a channel no one will ever read and the
// session's extraction stalls until some later message respawns the goroutine.
func TestWorker_ReleaseSession(t *testing.T) {
	const key = "ns/idle"

	t.Run("wake-up buffered: keeps the registration and refuses to exit", func(t *testing.T) {
		w := NewWorker(WorkerDeps{IdleTimeout: time.Minute})
		ch := make(chan struct{}, 16)
		w.sessions[key] = ch
		ch <- struct{}{} // Handle delivered a signal as the idle timer fired

		assert.False(t, w.releaseSession(key, ch),
			"must not exit with a wake-up still buffered")

		w.mu.Lock()
		_, registered := w.sessions[key]
		w.mu.Unlock()
		assert.True(t, registered,
			"the session must stay registered so the next Handle reaches this live goroutine")
	})

	t.Run("channel drained: deletes the registration and exits", func(t *testing.T) {
		w := NewWorker(WorkerDeps{IdleTimeout: time.Minute})
		ch := make(chan struct{}, 16)
		w.sessions[key] = ch

		assert.True(t, w.releaseSession(key, ch), "an idle session must be allowed to exit")

		w.mu.Lock()
		_, registered := w.sessions[key]
		w.mu.Unlock()
		assert.False(t, registered,
			"the wake channel must leave the map so a later Handle spawns a fresh goroutine")
	})
}

func TestWorker_WritesExtractedEntitiesAndState(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedSession(t, mem, scope, 1, "open authzed/spicedb please")
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool {
		st, ok, _ := exs.ForTurn(approvedCtx(), mem, scope, 1)
		return ok && st.Status == exs.StatusComplete
	}, 2*time.Second, 20*time.Millisecond)

	got, err := eex.ForTurn(approvedCtx(), mem, scope, 1)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, "github_repo", got[0].ResourceType)
}

func TestWorker_ExtractFailWritesFailedState(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	seedSession(t, mem, scope, 1, "open authzed/spicedb please")
	ext := &fakeExtractor{respond: func(_ extract.ExtractInput) ([]extract.ExtractedEntity, error) {
		return nil, assert.AnError
	}}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool {
		st, ok, _ := exs.ForTurn(approvedCtx(), mem, scope, 1)
		return ok && st.Status == exs.StatusFailed
	}, 2*time.Second, 20*time.Millisecond)
}

func TestWorker_PrefilterSkip_NoExtractCall(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	// "hello there" contains no owner/name slug — github_repo prefilter says no.
	seedSession(t, mem, scope, 1, "hello there")
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool {
		st, ok, _ := exs.ForTurn(approvedCtx(), mem, scope, 1)
		return ok && st.Status == exs.StatusComplete && st.CandidateCount == 0
	}, 2*time.Second, 20*time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&ext.calls), "extractor must not be called when prefilter says no")
}

// seedSessionWithSlots is seedSession with caller-supplied slots, for the
// fillFrom cases where the declaration is the variable under test.
func seedSessionWithSlots(t *testing.T, mem memory.Memory, scope memory.Scope, turnIdx int, text string, slots []spiceboxv1alpha1.AuthzSlot) {
	t.Helper()
	require.NoError(t, asc.Snapshot(approvedCtx(), mem, scope, asc.Content{
		Subject:       "alice",
		BoundEntities: slots,
	}))
	require.NoError(t, turnkind.RecordInbox(approvedCtx(), mem, scope, turnIdx, text, ""))
}

// TestWorker_FillFromAdmittingNoExtractor_NoExtractCall is the wiring half of the
// fillFrom gate. authz.Prefilter has its own unit tests, but those pass whether
// or not anything calls it with the fillFrom populated — and this feature has
// already shipped five validators that were logic-correct and never reached.
// Here the real worker drives a real session config: the message names a repo,
// so the prefilter WOULD open, and the only thing that keeps the extractor LLM
// away from the user's text is the declaration.
func TestWorker_FillFromAdmittingNoExtractor_NoExtractCall(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/fillfrom-excluded"}
	seedSessionWithSlots(t, mem, scope, 1, "open demo-org/demo-repo please",
		[]spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "github_repo", Description: "GitHub repo", Permission: "read",
			FillFrom: []string{"default"},
		}})
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool {
		st, ok, _ := exs.ForTurn(approvedCtx(), mem, scope, 1)
		return ok && st.Status == exs.StatusComplete && st.CandidateCount == 0
	}, 2*time.Second, 20*time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&ext.calls),
		"a slot the extractor may not fill must not spend an extractor call on the user's text")
}

// TestWorker_FillFromAdmittingExtractor_NarrowsThePrompt: the surviving
// slot still runs, and the excluded one is absent from EntityTypes — so the
// extractor is never even told to look for it. Narrowing only the prefilter
// would leave the prompt naming a type nothing can bind.
func TestWorker_FillFromAdmittingExtractor_NarrowsThePrompt(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/fillfrom-mixed"}
	seedSessionWithSlots(t, mem, scope, 1, "open demo-org/demo-repo please",
		[]spiceboxv1alpha1.AuthzSlot{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read",
				FillFrom: []string{"query"}},
			{ResourceType: "http_target", Description: "a URL", Permission: "reachable",
				FillFrom: []string{"default"}},
		})

	// respond runs on the worker's per-session goroutine, so the capture needs
	// its own lock — and the wait below must poll the CAPTURE, not ext.calls:
	// Extract increments that counter before it reaches respond, so a wait on
	// the count can return before anything has been recorded.
	var mu sync.Mutex
	var sawTypes []string
	ext := &fakeExtractor{respond: func(in extract.ExtractInput) ([]extract.ExtractedEntity, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, et := range in.EntityTypes {
			sawTypes = append(sawTypes, et.ResourceType)
		}
		return nil, nil
	}}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	captured := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sawTypes...)
	}
	require.Eventually(t, func() bool { return len(captured()) > 0 }, 2*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"github_repo"}, captured(),
		"the extractor prompt must name only the slots it may fill")
}

// A slot declaring only `ask` must reach the extractor: the agent prompts with
// respond_to_user, the user answers in an ordinary turn, and that answer is
// what the extractor is there to find. Gating this path on query alone would
// have made `ask` a control that binds nothing.
func TestWorker_AskSlotStillReachesTheExtractor(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/ask-reaches"}
	seedSessionWithSlots(t, mem, scope, 1, "use demo-org/demo-repo please",
		[]spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "github_repo", Description: "GitHub repo", Permission: "read",
			FillFrom: []string{"ask"},
		}})
	ext := &fakeExtractor{}
	w := NewWorker(WorkerDeps{Memory: approvedMem{mem}, Extractor: ext, IdleTimeout: time.Minute})

	require.NoError(t, w.Handle(approvedCtx(), scope))
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&ext.calls) >= 1
	}, 2*time.Second, 20*time.Millisecond)
}
