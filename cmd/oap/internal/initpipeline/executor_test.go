package initpipeline

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
)

// fakeExecDeps returns an ExecDeps suitable for unit tests: streaming renderer
// writing to a real bytes.Buffer (not a TTY → selects streaming), assumeYes=true
// so the wait loop never prompts, a no-op Apply, and a live Recheck context.
// The streaming renderer's mutex serializes concurrent writes to the buffer, so
// this is safe under -race even when the WAIT phase runs multiple goroutines.
func fakeExecDeps(t *testing.T) ExecDeps {
	t.Helper()
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true /*assumeYes*/)
	t.Cleanup(func() { _ = rep.Close() })
	return ExecDeps{
		Rep:       rep,
		Apply:     func(context.Context, [][]byte) error { return nil },
		Recheck:   context.Background(),
		OnApplied: func(string) {},
	}
}

// firstIndexPrefix returns the lowest index in order whose element starts with
// prefix, or -1 if none.
func firstIndexPrefix(order []string, prefix string) int {
	for i, s := range order {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// lastIndexPrefix returns the highest index in order whose element starts with
// prefix, or -1 if none.
func lastIndexPrefix(order []string, prefix string) int {
	for i := len(order) - 1; i >= 0; i-- {
		if strings.HasPrefix(order[i], prefix) {
			return i
		}
	}
	return -1
}

// TestComponent_RowLabel_PrefersDisplayName verifies that rowLabel returns
// DisplayName when set, and falls back to Name when DisplayName is empty.
func TestComponent_RowLabel_PrefersDisplayName(t *testing.T) {
	cases := []struct {
		name      string
		comp      Component
		wantLabel string
	}{
		{
			name:      "DisplayName set: use DisplayName",
			comp:      Component{Name: "postgres", DisplayName: "PostgreSQL (agent memory)"},
			wantLabel: "PostgreSQL (agent memory)",
		},
		{
			name:      "DisplayName empty: fall back to Name",
			comp:      Component{Name: "NATS"},
			wantLabel: "NATS",
		},
		{
			name:      "both set: DisplayName wins",
			comp:      Component{Name: "graphiti", DisplayName: "Graphiti (KG extraction)"},
			wantLabel: "Graphiti (KG extraction)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantLabel, tc.comp.rowLabel())
		})
	}
}

// TestRun_SecretsBeforeApplyBeforeWait_NoDeadlock verifies the two ordering
// invariants the DAG executor preserves:
//
//  1. Anti-deadlock: EVERY Secrets call completes before ANY Manifests call.
//     SECRETS stays a global phase before the DAG (the operator/webhook-TLS
//     deadlock fix). This holds unconditionally.
//  2. Dependency ordering: a dependent's Manifests (apply) runs only AFTER its
//     dependency's Wait has completed. (The old "all applies before all waits"
//     rigidity no longer holds and is no longer asserted — the DAG removes it.)
//
// The mutex guards order because Manifests and Wait.Poll are called from
// concurrent component goroutines.
func TestRun_SecretsBeforeApplyBeforeWait_NoDeadlock(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	mk := func(name string, deps ...string) Component {
		return Component{
			Name: name, DependsOn: deps,
			Secrets:   func(context.Context) error { record("secret:" + name); return nil },
			Manifests: func() ([][]byte, error) { record("apply:" + name); return nil, nil },
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { record("wait:" + name); return true, nil },
				ETA:  time.Second,
			},
		}
	}
	// postgres depends on operator, so postgres's apply must follow operator's wait.
	comps := []Component{mk("operator"), mk("postgres", "operator")}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.NoError(t, err)

	mu.Lock()
	snap := append([]string(nil), order...)
	mu.Unlock()

	// EVERY secret must precede EVERY apply (anti-deadlock invariant).
	assert.Less(t, lastIndexPrefix(snap, "secret:"), firstIndexPrefix(snap, "apply:"),
		"all secrets must complete before any apply; order: %v", snap)
	// A dependent's apply must follow its dependency's wait (DAG ordering).
	assert.Less(t, firstIndexPrefix(snap, "wait:operator"), firstIndexPrefix(snap, "apply:postgres"),
		"postgres's apply must run after operator's wait completes; order: %v", snap)
}

// TestRun_DAG_ApplyAfterDependencyReady is the core DAG invariant: a component
// B that DependsOn A must not APPLY its manifests until A has reached READY
// (A's Wait poll returned ready). This is the whole point of the DAG — a
// dependent's apply happens only after its dependency is up, not crash-looping
// until the dependency materializes.
func TestRun_DAG_ApplyAfterDependencyReady(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	a := Component{
		Name:      "A",
		Manifests: func() ([][]byte, error) { record("apply:A"); return nil, nil },
		Wait: &WaitSpec{
			Poll: func(context.Context) (bool, error) { record("ready:A"); return true, nil },
			ETA:  time.Second,
		},
	}
	b := Component{
		Name:      "B",
		DependsOn: []string{"A"},
		Manifests: func() ([][]byte, error) { record("apply:B"); return nil, nil },
		Wait: &WaitSpec{
			Poll: func(context.Context) (bool, error) { record("ready:B"); return true, nil },
			ETA:  time.Second,
		},
	}
	err := Run(context.Background(), []Component{a, b}, fakeExecDeps(t))
	require.NoError(t, err)

	mu.Lock()
	snap := append([]string(nil), order...)
	mu.Unlock()

	readyA := lastIndexPrefix(snap, "ready:A")
	applyB := firstIndexPrefix(snap, "apply:B")
	require.NotEqual(t, -1, readyA, "A's readiness poll must have run; order: %v", snap)
	require.NotEqual(t, -1, applyB, "B's apply must have run; order: %v", snap)
	assert.Less(t, readyA, applyB,
		"B's apply must run only after A's wait (readiness) completes; order: %v", snap)
}

// TestRun_OptionalNeverReady_DoesNotFail verifies that an Optional component
// whose readiness poll never returns true does not cause Run to return an error.
// ETA=1ms keeps the test fast: the awaitLoop times out almost immediately.
func TestRun_OptionalNeverReady_DoesNotFail(t *testing.T) {
	comps := []Component{{
		Name: "graphiti", Optional: true,
		Wait: &WaitSpec{
			Poll: func(context.Context) (bool, error) { return false, nil },
			ETA:  time.Millisecond,
		},
	}}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.NoError(t, err, "optional component that never readies must not fail the run")
}

// TestRun_ConcurrentWaits_RaceFree verifies that three independent components
// in the same WAIT wave can write concurrently to the shared renderer backed by
// a real bytes.Buffer without triggering the race detector. This is the key
// regression test for the renderer mutex (Fixes 1 and 2).
func TestRun_ConcurrentWaits_RaceFree(t *testing.T) {
	comps := make([]Component, 3)
	for i := range comps {
		name := fmt.Sprintf("svc%d", i)
		comps[i] = Component{
			Name: name,
			Wait: &WaitSpec{
				// Immediately ready: concurrent goroutines all call ph.Done()
				// and the preceding Phase() calls in parallel — the renderer
				// mutex must serialize these writes to the shared bytes.Buffer.
				Poll: func(context.Context) (bool, error) { return true, nil },
				ETA:  time.Second,
			},
		}
	}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.NoError(t, err, "all immediately-ready components must succeed")
}

// TestRun_DependsOnCycle_Errors verifies that a cyclic DependsOn graph returns
// an error instead of hanging the pipeline.
func TestRun_DependsOnCycle_Errors(t *testing.T) {
	// A → B → A is a cycle.
	comps := []Component{
		{Name: "A", DependsOn: []string{"B"}},
		{Name: "B", DependsOn: []string{"A"}},
	}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.Error(t, err, "cyclic DependsOn must return an error")
	assert.Contains(t, err.Error(), "cycle")
}

// TestRun_UnknownDependsOn_FailsBeforeSecrets verifies that an unknown DependsOn
// name is caught before any Secrets function is called (Fix 5: validate before
// side effects).
func TestRun_UnknownDependsOn_FailsBeforeSecrets(t *testing.T) {
	secretsCalled := false
	comps := []Component{
		{
			Name:      "app",
			DependsOn: []string{"nonexistent"},
			Secrets: func(context.Context) error {
				secretsCalled = true
				return nil
			},
		},
	}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.Error(t, err, "unknown DependsOn must return an error")
	assert.False(t, secretsCalled, "Secrets must not be called before DependsOn validation")
}

// TestRun_OptionalDoesNotStarveDependents is the regression test for the
// critical per-dependency gating fix. An Optional component that never becomes
// Ready (e.g. graphiti with no OPENAI_API_KEY) must NOT delay another
// component's wait start if that other component does not depend on it.
//
// Topology: graphiti (optional, ETA=optionalETA, never ready) and nats (wave 0,
// immediately ready) share wave 0; channelsd (wave 1) depends only on nats.
// Under the old global wave-barrier, channelsd could not start until graphiti's
// AwaitOptional window elapsed. Under per-dependency gating, channelsd gates
// only on nats, which finishes instantly, so its poll starts well before half
// the optional ETA elapses.
func TestRun_OptionalDoesNotStarveDependents(t *testing.T) {
	const optionalETA = 60 * time.Millisecond

	var mu sync.Mutex
	var channelsdPollAt time.Time
	startedAt := time.Now()

	comps := []Component{
		{
			// Optional, never-ready; simulates graphiti with no OPENAI_API_KEY.
			Name: "graphiti", Optional: true,
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { return false, nil },
				ETA:  optionalETA,
			},
		},
		{
			// nats: wave 0, no DependsOn, immediately ready.
			Name: "nats",
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { return true, nil },
				ETA:  time.Second,
			},
		},
		{
			// channelsd: depends on nats (NOT graphiti). With per-dependency
			// gating, its wait starts as soon as nats is done, not after
			// graphiti's optional window.
			Name:      "channelsd",
			DependsOn: []string{"nats"},
			Wait: &WaitSpec{
				Poll: func(ctx context.Context) (bool, error) {
					mu.Lock()
					if channelsdPollAt.IsZero() {
						channelsdPollAt = time.Now()
					}
					mu.Unlock()
					return true, nil
				},
				ETA: time.Second,
			},
		},
	}

	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.NoError(t, err)

	mu.Lock()
	pollAt := channelsdPollAt
	mu.Unlock()

	require.False(t, pollAt.IsZero(), "channelsd poll must have been invoked")
	elapsed := pollAt.Sub(startedAt)
	// With per-dep gating channelsd starts promptly after nats; with a global
	// wave barrier it would only start after optionalETA has elapsed.
	assert.Less(t, elapsed, optionalETA/2,
		"channelsd wait must start before half the optional ETA elapses (per-dep gating fix); elapsed=%v optionalETA=%v",
		elapsed, optionalETA)
}

// TestRun_OptionalTailShortCircuit_SkippedPromptly verifies that a never-ready
// Optional component is Skipped promptly once all required (non-optional)
// components have finished their waits, rather than keeping Run alive for the
// full optional ETA. This is the regression test for the optional-tail
// short-circuit: without the fix, Run would block until graphiti's AwaitOptional
// window elapsed even though all required components were already ready.
func TestRun_OptionalTailShortCircuit_SkippedPromptly(t *testing.T) {
	const optionalETA = 120 * time.Millisecond

	comps := []Component{
		{
			// Required component that is immediately ready.
			Name: "required",
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { return true, nil },
				ETA:  time.Second,
			},
		},
		{
			// Optional component that NEVER becomes ready; simulates graphiti
			// when OPENAI_API_KEY is unset.
			Name: "optional", Optional: true,
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { return false, nil },
				ETA:  optionalETA,
			},
		},
	}

	start := time.Now()
	err := Run(context.Background(), comps, fakeExecDeps(t))
	elapsed := time.Since(start)

	require.NoError(t, err, "optional never-ready component must not fail the run")
	assert.Less(t, elapsed, optionalETA/2,
		"Run must return promptly after required is ready, not wait for the full optional ETA; elapsed=%v optionalETA=%v",
		elapsed, optionalETA)
}

// TestRun_ApplyCountBar_PerGroupBehaviorPreserving verifies the apply count-bar
// wiring: each manifest group is applied exactly once and in order (the apply is
// behavior-preserving — splitting Apply per group changes only the rendering),
// and the renderer prints the advancing n/m count, reaching completion.
func TestRun_ApplyCountBar_PerGroupBehaviorPreserving(t *testing.T) {
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true /*assumeYes*/)
	t.Cleanup(func() { _ = rep.Close() })

	var mu sync.Mutex
	var applied []string
	deps := ExecDeps{
		Rep: rep,
		Apply: func(_ context.Context, groups [][]byte) error {
			mu.Lock()
			defer mu.Unlock()
			for _, g := range groups {
				applied = append(applied, string(g))
			}
			return nil
		},
		Recheck:   context.Background(),
		OnApplied: func(string) {},
	}

	groups := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	comp := Component{
		Name:      "svc",
		Manifests: func() ([][]byte, error) { return groups, nil },
		Wait: &WaitSpec{
			Poll: func(context.Context) (bool, error) { return true, nil },
			ETA:  time.Second,
		},
	}
	require.NoError(t, Run(context.Background(), []Component{comp}, deps))

	mu.Lock()
	got := append([]string(nil), applied...)
	mu.Unlock()

	require.Len(t, got, len(groups), "every manifest group must be applied exactly once")
	for i := range groups {
		assert.Equal(t, string(groups[i]), got[i], "group %d must be applied in declared order", i)
	}
	assert.Contains(t, buf.String(), "svc: 4/4", "the apply count-bar must reach completion")
}

// TestRun_ApplyOnlyComponent_MarksRowDone covers the apply-only branch: a
// component with Manifests but no Wait gets a row, renders its apply bar, and is
// marked Done once its apply completes (no readiness spinner follows).
func TestRun_ApplyOnlyComponent_MarksRowDone(t *testing.T) {
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true /*assumeYes*/)
	t.Cleanup(func() { _ = rep.Close() })

	deps := ExecDeps{
		Rep:       rep,
		Apply:     func(context.Context, [][]byte) error { return nil },
		Recheck:   context.Background(),
		OnApplied: func(string) {},
	}
	comp := Component{
		Name:      "config",
		Manifests: func() ([][]byte, error) { return [][]byte{[]byte("x")}, nil },
		// No Wait: the apply bar completes and the row closes via Done().
	}
	require.NoError(t, Run(context.Background(), []Component{comp}, deps))

	out := buf.String()
	assert.Contains(t, out, "config: 1/1", "apply bar must reach completion for an apply-only component")
	assert.Contains(t, out, "config ready", "apply-only component's row must be marked done")
}

// TestRun_DependsOn_WaitOrder verifies that a component's wait begins only
// after every component it lists in DependsOn has completed its wait.
// "app" depends on "dep"; dep must reach Ready before app's wave starts.
func TestRun_DependsOn_WaitOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	mkWait := func(name string, deps ...string) Component {
		return Component{
			Name:      name,
			DependsOn: deps,
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { record("wait:" + name); return true, nil },
				ETA:  time.Second,
			},
		}
	}
	// app→dep: dep is wave 0, app is wave 1.
	comps := []Component{mkWait("app", "dep"), mkWait("dep")}
	err := Run(context.Background(), comps, fakeExecDeps(t))
	require.NoError(t, err)

	mu.Lock()
	snap := append([]string(nil), order...)
	mu.Unlock()

	depIdx := firstIndexPrefix(snap, "wait:dep")
	appIdx := firstIndexPrefix(snap, "wait:app")
	assert.NotEqual(t, -1, depIdx, "dep wait must have run; order: %v", snap)
	assert.NotEqual(t, -1, appIdx, "app wait must have run; order: %v", snap)
	assert.Less(t, depIdx, appIdx, "dep's wait must complete before app's wait starts; order: %v", snap)
}

// TestRun_FirstPollReady_RendersAlreadyRunning verifies the "safe re-run" light
// touch: a component ready on its FIRST poll (already up, e.g. on a re-run) is
// rendered with the "already running" indicator, while one that needs a second
// poll is not.
func TestRun_FirstPollReady_RendersAlreadyRunning(t *testing.T) {
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true /*assumeYes → streaming, no prompts*/)
	t.Cleanup(func() { _ = rep.Close() })
	deps := ExecDeps{
		Rep:       rep,
		Apply:     func(context.Context, [][]byte) error { return nil },
		Recheck:   context.Background(),
		OnApplied: func(string) {},
	}

	var notUpPolls int
	comps := []Component{
		{
			Name: "already-up",
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { return true, nil }, // ready on first poll
				ETA:  time.Second,
			},
		},
		{
			Name: "comes-up-late",
			Wait: &WaitSpec{
				// Not ready on the first poll; ready on the second. The deadline must
				// exceed the first backoff (~2s) so the second poll lands in-window.
				Poll: func(context.Context) (bool, error) { notUpPolls++; return notUpPolls > 1, nil },
				ETA:  3 * time.Second,
			},
		},
	}
	require.NoError(t, Run(context.Background(), comps, deps))

	out := buf.String()
	assert.Contains(t, out, "already-up ready (already running)",
		"a first-poll-ready component must show the already-running indicator; out: %s", out)
	assert.Contains(t, out, "comes-up-late ready", "the late component must still complete")
	assert.NotContains(t, out, "comes-up-late ready (already running)",
		"a component that needed a second poll must NOT be marked already running; out: %s", out)
}

// TestRun_WaitDeadlineOutlivesETA is the regression test for the conflated
// soft-warn / hard-deadline bug that failed an `oap desktop` bring-up: the
// executor used WaitSpec.ETA as the hard wait deadline, so the "taking longer
// than expected" warning fired at the exact instant the install gave up, and a
// component that was merely slower than its warm-node estimate (neo4j: a 353 MB
// cold image pull, first-time store creation, JVM boot, plus a readinessProbe
// with initialDelaySeconds: 30) failed the whole install. It went Ready one
// second after the 2 m deadline expired.
//
// ETA now drives the soft-warn only; the hard deadline is deadlineGrace × ETA,
// or WaitSpec.Deadline when set explicitly. The final case pins that the
// deadline still EXISTS — a never-ready component must still fail rather than
// hang forever.
//
// The two success cases each cost ~2 s of wall clock: awaitLoop's first backoff
// is 2 s, so a component that is not ready on its first poll cannot become ready
// any sooner, and the deadline has to outlive that backoff for the second poll
// to land in-window.
func TestRun_WaitDeadlineOutlivesETA(t *testing.T) {
	// readyOnSecondPoll is not ready on the first poll and ready on every poll
	// after it — i.e. ready at ~2 s, one backoff in. Each case gets its own
	// counter; a component's polls all run on that component's goroutine.
	readyOnSecondPoll := func() progress.Poll {
		var polls int
		return func(context.Context) (bool, error) { polls++; return polls > 1, nil }
	}
	neverReady := func() progress.Poll {
		return func(context.Context) (bool, error) { return false, nil }
	}

	cases := []struct {
		name     string
		eta      time.Duration
		deadline time.Duration
		poll     progress.Poll
		wantErr  bool
	}{
		{
			name:    "ETA elapses but component readies inside the derived grace: Run succeeds",
			eta:     time.Second, // soft-warn at 1s; derived deadline 3s outlives the 2s backoff
			poll:    readyOnSecondPoll(),
			wantErr: false,
		},
		{
			name:     "explicit Deadline overrides the ETA-derived grace: Run succeeds",
			eta:      time.Millisecond, // derived grace would be 3ms — far too short
			deadline: 5 * time.Second,
			poll:     readyOnSecondPoll(),
			wantErr:  false,
		},
		{
			name:    "never ready: Run still fails at the deadline",
			eta:     time.Millisecond, // derived deadline 3ms — fails fast, as it must
			poll:    neverReady(),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comps := []Component{{
				Name: "neo4j",
				Wait: &WaitSpec{Poll: tc.poll, ETA: tc.eta, Deadline: tc.deadline},
			}}
			err := Run(context.Background(), comps, fakeExecDeps(t))
			if tc.wantErr {
				require.Error(t, err, "a never-ready component must still fail at the hard deadline")
				assert.Contains(t, err.Error(), "neo4j", "the failure must name the component that stalled")
				return
			}
			require.NoError(t, err, "a component that readies after its ETA but inside its deadline must not fail the install")
		})
	}
}

// TestWaitDeadline covers the ETA→deadline derivation directly, including the
// zero-ETA fallback that no Component in the install pipeline exercises today.
func TestWaitDeadline(t *testing.T) {
	cases := []struct {
		name string
		spec WaitSpec
		want time.Duration
	}{
		{
			name: "explicit Deadline wins over ETA",
			spec: WaitSpec{ETA: time.Minute, Deadline: 90 * time.Second},
			want: 90 * time.Second,
		},
		{
			name: "no Deadline: derived as deadlineGrace × ETA",
			spec: WaitSpec{ETA: 2 * time.Minute},
			want: 6 * time.Minute,
		},
		{
			name: "neither set: defaultWaitDeadline",
			spec: WaitSpec{},
			want: defaultWaitDeadline,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, waitDeadline(&tc.spec))
		})
	}
}

// TestRun_GatedOptional_NotShortCircuitedByRequiredTail is the regression test
// for the interaction between the optional-tail short-circuit and a DependsOn
// edge on an Optional component (graphiti → neo4j).
//
// The short-circuit cancels still-pending optional waits once every required
// component has finished, so the install is not held up by an optional that is
// not coming. An optional GATED behind a required component is a different
// case: its gate opens at the instant its dependency reports Ready, which for
// the slowest dependency IS the instant the required set finishes. Cutting it
// off there would skip it every single time, without ever polling it once —
// turning the DependsOn edge that exists to make it start cleanly into a
// guarantee that it never starts at all.
//
// Topology mirrors the real one: "neo4j" is required and the LAST required
// component to become ready (second poll, ~2 s in); "graphiti" is Optional and
// DependsOn it. graphiti must reach ready, not be skipped.
func TestRun_GatedOptional_NotShortCircuitedByRequiredTail(t *testing.T) {
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true /*assumeYes → streaming, no prompts*/)
	t.Cleanup(func() { _ = rep.Close() })
	deps := ExecDeps{
		Rep:       rep,
		Apply:     func(context.Context, [][]byte) error { return nil },
		Recheck:   context.Background(),
		OnApplied: func(string) {},
	}

	var neo4jPolls, graphitiPolls int
	comps := []Component{
		{
			// Required, and the last to finish: ready on its second poll (~2 s,
			// one awaitLoop backoff in).
			Name: "neo4j",
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { neo4jPolls++; return neo4jPolls > 1, nil },
				ETA:  3 * time.Second,
			},
		},
		{
			Name:      "graphiti",
			Optional:  true,
			DependsOn: []string{"neo4j"},
			Wait: &WaitSpec{
				Poll: func(context.Context) (bool, error) { graphitiPolls++; return graphitiPolls > 1, nil },
				ETA:  3 * time.Second,
			},
		},
	}
	require.NoError(t, Run(context.Background(), comps, deps))

	out := buf.String()
	assert.Contains(t, out, "graphiti ready",
		"a gated optional must get its own wait window once its dependency readies, not be skipped by the required-tail short-circuit; out: %s", out)
	assert.NotContains(t, out, "continuing without graphiti",
		"graphiti must not be skipped; out: %s", out)
}
