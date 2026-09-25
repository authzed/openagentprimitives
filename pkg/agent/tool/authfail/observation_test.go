package authfail

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// recordCall is one captured RecordCredentialAuthFailure invocation.
type recordCall struct {
	origin string
	count  int32
}

// fakeWriter captures the status writes the recorder makes, so every test can
// assert on the NUMBER of writes as well as their content — the
// write-only-on-transition behavior is invisible to a fake that only records
// the final state.
type fakeWriter struct {
	mu      sync.Mutex
	records []recordCall
	clears  []string
	err     error
}

func (f *fakeWriter) RecordCredentialAuthFailure(_ context.Context, origin string, count int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, recordCall{origin: origin, count: count})
	return nil
}

func (f *fakeWriter) ClearCredentialAuthFailure(_ context.Context, origin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.clears = append(f.clears, origin)
	return nil
}

func (f *fakeWriter) snapshot() ([]recordCall, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordCall(nil), f.records...), append([]string(nil), f.clears...)
}

func (f *fakeWriter) setErr(t *testing.T, err error) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// defaultBlock is an authFailure: block that declares nothing, which
// credupdate.IsAuthShaped reads as the documented 401-ONLY default.
func defaultBlock() *provider.AuthFailure { return &provider.AuthFailure{} }

// lookupAll returns af for every origin.
func lookupAll(af *provider.AuthFailure) func(string) *provider.AuthFailure {
	return func(string) *provider.AuthFailure { return af }
}

// newRecorder builds a recorder over a fresh fake writer with no pre-existing
// status observations.
func newRecorder(t *testing.T, af *provider.AuthFailure) (*Recorder, *fakeWriter) {
	t.Helper()
	w := &fakeWriter{}
	return New(w, lookupAll(af), nil), w
}

func TestRecordWritesOnceOnTheTransitionToCorroborated(t *testing.T) {
	ctx := context.Background()

	t.Run("auth-shaped failure: one status write naming the origin", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		recs, clears := w.snapshot()
		require.Len(t, recs, 1, "an auth-shaped failure must record exactly one observation")
		assert.Equal(t, "mcpserver/demo", recs[0].origin)
		assert.Equal(t, int32(1), recs[0].count)
		assert.Empty(t, clears, "recording a failure must not clear anything")
	})

	// The NO-EVIDENCE case, and the near-miss it must not be confused with.
	//
	// A failure that never reached the provider's auth layer — a connection
	// refused, a DNS failure, a timeout, a 5xx — says nothing in EITHER
	// direction. It is not corroboration, so nothing is recorded; but it is
	// equally not proof the credential works, so it must not retract an
	// observation the way a genuine success does.
	//
	// That is deliberately NOT the rule for a call the origin AUTHENTICATED
	// and then answered with a tool-level error (an MCP 200 carrying
	// isError: true, a CLI that exits 0 without producing its declared secret
	// file). Those arrive as error results too, but they are positive evidence
	// the credential works and they CLEAR — see
	// TestAnAuthenticatedCallClearsEvenWhenItReturnedAnError. Collapsing the
	// two is how a stale observation outlives a credential fixed out of band,
	// and the agent picks the arguments, so it can manufacture the tool-level
	// error shape at will.
	t.Run("failure with NO evidence either way (transport-level): nothing recorded, nothing cleared", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/demo", false))

		recs, clears := w.snapshot()
		assert.Empty(t, recs, "a failure that is not auth-shaped is not evidence about the credential")
		assert.Empty(t, clears,
			"a failure that never reached the provider's auth layer is not proof the credential works, so it must not clear a real observation")
	})

	t.Run("repeated failures: still ONE write, counter stays in memory", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		for i := 0; i < 5; i++ {
			require.NoError(t, r.Record(ctx, "mcpserver/demo", true))
		}

		recs, _ := w.snapshot()
		require.Len(t, recs, 1,
			"status must be written only on the TRANSITION to corroborated; a write per failing call would be noisy and non-idempotent")
		assert.Equal(t, int32(1), recs[0].count, "the recorded count is the transition-time count")
	})

	t.Run("a non-auth-shaped failure between two auth-shaped ones does not re-write", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))
		require.NoError(t, r.Record(ctx, "mcpserver/demo", false))
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		recs, clears := w.snapshot()
		assert.Len(t, recs, 1, "still one transition — the origin never left the observed state")
		assert.Empty(t, clears)
	})

	t.Run("distinct origins each transition independently", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/one", true))
		require.NoError(t, r.Record(ctx, "mcpserver/two", true))
		require.NoError(t, r.Record(ctx, "mcpserver/one", true))

		recs, _ := w.snapshot()
		require.Len(t, recs, 2)
		assert.Equal(t, "mcpserver/one", recs[0].origin)
		assert.Equal(t, "mcpserver/two", recs[1].origin)
	})

	t.Run("empty origin is a no-op", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "", true))
		require.NoError(t, r.Clear(ctx, ""))

		recs, clears := w.snapshot()
		assert.Empty(t, recs)
		assert.Empty(t, clears)
	})

	t.Run("nil recorder is a safe no-op", func(t *testing.T) {
		var r *Recorder
		assert.NoError(t, r.Record(ctx, "mcpserver/demo", true))
		assert.NoError(t, r.Clear(ctx, "mcpserver/demo"))
		assert.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{HTTPStatus: 401}))
	})
}

// TestSuccessClearsTheObservation is THE invariant that keeps a stale
// observation from corroborating a healthy credential: a credential that just
// worked is not one that needs replacing.
func TestSuccessClearsTheObservation(t *testing.T) {
	ctx := context.Background()

	t.Run("success after a recorded failure clears it", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))
		require.NoError(t, r.Clear(ctx, "mcpserver/demo"))

		recs, clears := w.snapshot()
		require.Len(t, recs, 1)
		require.Len(t, clears, 1, "a subsequent success MUST clear the origin's observation")
		assert.Equal(t, "mcpserver/demo", clears[0])
	})

	t.Run("success with no observation does not touch the API server", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		for i := 0; i < 5; i++ {
			require.NoError(t, r.Clear(ctx, "mcpserver/demo"))
		}

		_, clears := w.snapshot()
		assert.Empty(t, clears,
			"every successful tool call would otherwise cost a status read+patch; clearing is only a write when there is something to clear")
	})

	t.Run("a clear only clears its own origin", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/one", true))
		require.NoError(t, r.Record(ctx, "mcpserver/two", true))
		require.NoError(t, r.Clear(ctx, "mcpserver/one"))

		_, clears := w.snapshot()
		require.Len(t, clears, 1)
		assert.Equal(t, "mcpserver/one", clears[0])
	})

	t.Run("failing again after a success records a NEW observation", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))
		require.NoError(t, r.Clear(ctx, "mcpserver/demo"))
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		recs, clears := w.snapshot()
		require.Len(t, clears, 1)
		require.Len(t, recs, 2, "the clear returned the origin to the un-observed state, so the next failure is a fresh transition")
		assert.Equal(t, int32(1), recs[1].count,
			"the in-memory counter restarts at the clear: the count must describe the CURRENT run of failures, not a lifetime total")
	})

	t.Run("an observation carried over from a previous runner process is clearable", func(t *testing.T) {
		w := &fakeWriter{}
		// A restarted runner has an empty in-memory map but the session status
		// still carries the entry the PREVIOUS process wrote. Without seeding,
		// the first success would find "nothing recorded" and skip the clear,
		// stranding the stale observation for the rest of the session.
		r := New(w, lookupAll(defaultBlock()), []spiceboxv1alpha1.CredentialAuthFailure{
			{Origin: "mcpserver/demo", Count: 1},
		})
		require.NoError(t, r.Clear(ctx, "mcpserver/demo"))

		_, clears := w.snapshot()
		require.Len(t, clears, 1, "a success must clear an observation this process did not itself record")
		assert.Equal(t, "mcpserver/demo", clears[0])
	})

	t.Run("a seeded origin does not re-write status on its next failure", func(t *testing.T) {
		w := &fakeWriter{}
		r := New(w, lookupAll(defaultBlock()), []spiceboxv1alpha1.CredentialAuthFailure{
			{Origin: "mcpserver/demo", Count: 1},
		})
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		recs, _ := w.snapshot()
		assert.Empty(t, recs, "the origin was already in the observed state; there is no transition to write")
	})
}

// TestAnAuthenticatedCallClearsEvenWhenItReturnedAnError is the other half of
// TestSuccessClearsTheObservation's invariant, and the half that is easy to
// get wrong because it does not look like a success.
//
// IsError is not the question. The question is whether the provider's own auth
// layer accepted the credential:
//
//   - a transport failure (connection refused, DNS, timeout, a 5xx) never got
//     that far, so it is evidence about NOTHING and retracts nothing;
//   - an MCP server answering HTTP 200 with a tool-level isError, or a CLI that
//     exits 0 but fails to produce a declared secret file, DID authenticate.
//     The error is about the arguments or the work, and the credential
//     demonstrably works.
//
// Without the second rule a stale observation survives every later call at
// that origin, because the agent CHOOSES the arguments and can produce
// tool-level errors on demand. Pair that with a provider that ships no verify:
// probe — corroboration is decisive there — and a prompt-injected agent can
// keep a dead 401 alive until a human is asked to re-enter a working
// credential.
func TestAnAuthenticatedCallClearsEvenWhenItReturnedAnError(t *testing.T) {
	ctx := context.Background()

	t.Run("authenticated error result retracts a recorded observation", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{HTTPStatus: 401}))
		require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{OriginAuthenticated: true}))

		recs, clears := w.snapshot()
		require.Len(t, recs, 1, "the 401 is the transition into the observed state")
		require.Len(t, clears, 1,
			"a call the origin authenticated is proof the credential works and MUST retract the stale observation, error result or not")
		assert.Equal(t, "mcpserver/demo", clears[0])
	})

	t.Run("authentication beats every declared shape the same result also matches", func(t *testing.T) {
		// The dangerous combination: a provider declaring the over-broad
		// exitCodes: [1] plus a loose stderr pattern, and a CLI call that ran
		// fine on arguments the agent chose. Every declared signal "matches",
		// and the call still authenticated — so it clears rather than records.
		af := &provider.AuthFailure{ExitCodes: []int{1}, StderrPatterns: []string{`(?i)unauthorized`}}
		r, w := newRecorder(t, af)
		require.NoError(t, r.Record(ctx, "toolkit/demo", true))

		exit := int32(1)
		require.NoError(t, r.ObserveFailure(ctx, "toolkit/demo", credupdate.Observation{
			ExitCode:            &exit,
			Stderr:              "error: unauthorized flag --unauthorized",
			OriginAuthenticated: true,
		}))

		recs, clears := w.snapshot()
		require.Len(t, recs, 1, "only the first, genuine failure is recorded")
		require.Len(t, clears, 1, "positive evidence must beat a pattern match, never the other way round")
	})

	t.Run("authenticated call at an unobserved origin costs no status write", func(t *testing.T) {
		r, w := newRecorder(t, defaultBlock())
		for range 5 {
			require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{OriginAuthenticated: true}))
		}

		recs, clears := w.snapshot()
		assert.Empty(t, recs)
		assert.Empty(t, clears, "clearing stays a write only when there is something to clear")
	})

	t.Run("authenticated call with no provider lookup still clears", func(t *testing.T) {
		// The retraction must not depend on resolving a provider: an origin
		// whose authFailure: block is unknown can still be carrying an entry
		// seeded from a previous runner process.
		w := &fakeWriter{}
		r := New(w, nil, []spiceboxv1alpha1.CredentialAuthFailure{{Origin: "mcpserver/demo", Count: 3}})
		require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{OriginAuthenticated: true}))

		_, clears := w.snapshot()
		require.Len(t, clears, 1, "positive evidence needs no declared shape to interpret it")
	})
}

func TestObserveFailureClassifiesBeforeRecording(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string
		af         *provider.AuthFailure
		obs        credupdate.Observation
		wantRecord bool
	}{
		{
			name:       "401 against the 401-only default: recorded",
			af:         defaultBlock(),
			obs:        credupdate.Observation{HTTPStatus: 401},
			wantRecord: true,
		},
		{
			name: "403 against the 401-only default: NOT recorded (an agent can provoke a 403 on demand)",
			af:   defaultBlock(),
			obs:  credupdate.Observation{HTTPStatus: 403},
		},
		{
			name:       "403 against a provider that opted in: recorded",
			af:         &provider.AuthFailure{HTTPStatuses: []int{403}},
			obs:        credupdate.Observation{HTTPStatus: 403},
			wantRecord: true,
		},
		{
			name: "500: not an auth failure",
			af:   defaultBlock(),
			obs:  credupdate.Observation{HTTPStatus: 500},
		},
		{
			name: "no status observed (transport failure): nothing to classify",
			af:   defaultBlock(),
			obs:  credupdate.Observation{},
		},
		{
			name: "provider declares no authFailure: block: no corroboration available",
			af:   nil,
			obs:  credupdate.Observation{HTTPStatus: 401},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, w := newRecorder(t, tc.af)
			require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", tc.obs))

			recs, _ := w.snapshot()
			if tc.wantRecord {
				assert.Len(t, recs, 1)
				return
			}
			assert.Empty(t, recs)
		})
	}

	t.Run("no lookup wired: nothing is ever corroborated", func(t *testing.T) {
		w := &fakeWriter{}
		r := New(w, nil, nil)
		require.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{HTTPStatus: 401}))

		recs, _ := w.snapshot()
		assert.Empty(t, recs, "with no origin→provider lookup there is no declared shape to match, so nothing may be recorded")
	})
}

func TestWriteFailuresAreReturnedAndDoNotStickTheState(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("apiserver said no")

	t.Run("a failed record write is returned and retried on the next failure", func(t *testing.T) {
		w := &fakeWriter{}
		r := New(w, lookupAll(defaultBlock()), nil)

		w.setErr(t, boom)
		require.ErrorIs(t, r.Record(ctx, "mcpserver/demo", true), boom,
			"a write failure must reach the caller, never be swallowed")

		w.setErr(t, nil)
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		recs, _ := w.snapshot()
		require.Len(t, recs, 1,
			"the failed write must NOT have marked the origin observed, or the observation would be lost for the whole session")
	})

	t.Run("a failed clear write is returned and retried on the next success", func(t *testing.T) {
		w := &fakeWriter{}
		r := New(w, lookupAll(defaultBlock()), nil)
		require.NoError(t, r.Record(ctx, "mcpserver/demo", true))

		w.setErr(t, boom)
		require.ErrorIs(t, r.Clear(ctx, "mcpserver/demo"), boom)

		w.setErr(t, nil)
		require.NoError(t, r.Clear(ctx, "mcpserver/demo"))

		_, clears := w.snapshot()
		require.Len(t, clears, 1,
			"the failed clear must NOT have marked the origin un-observed, or the stale observation would survive every later success")
	})
}

// statusWriter is a fake that models the REAL AgentSession status: writes are
// applied in the order they land, so the final `present` flag is what a reader
// (the CredentialUpdateRequest reconciler) would actually see. gate, when
// non-nil, blocks inside the record write so a test can hold one patch in
// flight while another operation is decided.
type statusWriter struct {
	mu      sync.Mutex
	present bool
	writes  int

	gate        chan struct{} // closed by the test to release a blocked record write
	recordEntry chan struct{} // closed once a record write has actually begun
	clearEntry  chan struct{} // closed once a clear write has actually begun
	recordOnce  sync.Once
	clearOnce   sync.Once
}

func (s *statusWriter) RecordCredentialAuthFailure(_ context.Context, _ string, _ int32) error {
	if s.gate != nil {
		s.recordOnce.Do(func() { close(s.recordEntry) })
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.present = true
	s.writes++
	return nil
}

func (s *statusWriter) ClearCredentialAuthFailure(_ context.Context, _ string) error {
	if s.clearEntry != nil {
		s.clearOnce.Do(func() { close(s.clearEntry) })
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.present = false
	s.writes++
	return nil
}

func (s *statusWriter) state() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.present, s.writes
}

// TestRecordClearInterleavingConvergesOnStatus is the regression test for the
// stranding bug: a Clear decided WHILE a Record patch is in flight must not
// leave the entry on status with the recorder believing it is gone. If it did,
// every later success would short-circuit and the observation would corroborate
// a healthy credential for the rest of the session.
func TestRecordClearInterleavingConvergesOnStatus(t *testing.T) {
	ctx := context.Background()
	const origin = "mcpserver/demo"

	w := &statusWriter{
		gate:        make(chan struct{}),
		recordEntry: make(chan struct{}),
		clearEntry:  make(chan struct{}),
	}
	r := New(w, lookupAll(defaultBlock()), nil)

	// A failing call starts its status write and parks inside it.
	recordDone := make(chan error, 1)
	go func() { recordDone <- r.Record(ctx, origin, true) }()
	<-w.recordEntry

	// While that patch is in flight, a SUCCESS on the same origin is observed.
	clearDone := make(chan error, 1)
	go func() { clearDone <- r.Clear(ctx, origin) }()

	// Give the clear every chance to reach the API server FIRST — this is the
	// interleaving being forced. Correct behavior is that it cannot: the
	// per-origin write lock is held by the parked record, so this wait must time
	// out. An implementation that decides and writes without holding that lock
	// lands the clear here, and the still-parked record then overwrites it —
	// leaving an entry on status that memory believes is gone.
	select {
	case <-w.clearEntry:
		t.Fatal("a clear write reached the API server while a record write for the SAME origin was in flight: " +
			"their patches can land in the opposite order to their decisions, stranding the observation")
	case <-time.After(250 * time.Millisecond):
	}

	// Release the parked record write; both operations now complete.
	close(w.gate)
	require.NoError(t, <-recordDone)
	require.NoError(t, <-clearDone)

	present, _ := w.state()
	assert.False(t, present,
		"the success was the LAST thing observed, so status must not carry an observation")

	// The recorder's memory must agree with status, not just happen to look
	// right: a follow-up failure has to be treated as a fresh transition and
	// actually write. If memory were stuck at "present", this would be skipped
	// and the origin could never be corroborated again.
	_, before := w.state()
	require.NoError(t, r.Record(ctx, origin, true))
	presentAfter, after := w.state()
	assert.True(t, presentAfter, "a fresh failure after the interleaving must record again")
	assert.Greater(t, after, before, "…and that must be a real write, not a short-circuit")
}

// TestFailedWriteLeavesOriginUnknownSoTheNextOpRewrites covers the other half
// of the same failure class: a write whose response was lost may have
// COMMITTED, so the recorder must not assume it did not.
func TestFailedWriteLeavesOriginUnknownSoTheNextOpRewrites(t *testing.T) {
	ctx := context.Background()
	const origin = "mcpserver/demo"
	boom := errors.New("apiserver said no")

	w := &fakeWriter{}
	r := New(w, lookupAll(defaultBlock()), nil)

	// The record write fails. Status may or may not now hold the entry.
	w.setErr(t, boom)
	require.ErrorIs(t, r.Record(ctx, origin, true), boom)
	w.setErr(t, nil)

	// A subsequent success MUST issue a clear anyway. Skipping it — on the
	// assumption the failed write never landed — is exactly how a committed
	// entry gets stranded.
	require.NoError(t, r.Clear(ctx, origin))
	_, clears := w.snapshot()
	assert.Len(t, clears, 1,
		"after an indeterminate write the next success must force a clear, not assume the entry is absent")
}

func TestConcurrentFailuresOnOneOriginWriteOnce(t *testing.T) {
	ctx := context.Background()
	w := &fakeWriter{}
	r := New(w, lookupAll(defaultBlock()), nil)

	// Tool dispatch runs each tool_use in its own goroutine, so sibling tools
	// of ONE MCP server can observe the same origin's 401 at the same instant.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, r.ObserveFailure(ctx, "mcpserver/demo", credupdate.Observation{HTTPStatus: 401}))
		}()
	}
	wg.Wait()

	recs, _ := w.snapshot()
	assert.Len(t, recs, 1, "concurrent observers of one origin must still produce exactly one transition write")
}
