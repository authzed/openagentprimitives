package initpipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
)

const (
	// defaultWaitDeadline is used when a WaitSpec carries neither a Deadline nor
	// an ETA to derive one from.
	defaultWaitDeadline = 5 * time.Minute
	// deadlineGrace multiplies a WaitSpec's ETA to derive its hard wait deadline
	// when the spec does not set one explicitly.
	//
	// ETA is a warm-node expectation and drives the soft-warn; the deadline has
	// to cover the cold case, which is dominated by image pull — a network-bound
	// cost routinely several times the warm estimate, and not one the installer
	// can speed up by giving up on it. 3× is what separates "slower than usual,
	// keep waiting" from "this is never going to happen".
	//
	// The two must stay separate numbers. Collapsed into one, the soft-warn fires
	// at the same instant the install dies, and a component that is merely slow
	// fails the whole install — neo4j reaching Ready one second past a 2 m window
	// aborts an `oap desktop` bring-up. Widening a component's ETA to buy itself
	// deadline is the wrong lever: it buys the deadline by giving up the warning.
	// Set WaitSpec.Deadline instead.
	deadlineGrace = 3
	// maxConcurrent caps the number of component goroutines that may be inside
	// their APPLY+WAIT body (i.e. holding the semaphore) at once. Goroutines
	// blocked on a dependency gate do NOT hold the semaphore.
	maxConcurrent = 8
)

// ExecDeps are the external collaborators Run needs to execute the install
// pipeline. Dynamic and Typed are available for Apply closures and Wait.Poll
// implementations; they are not used directly by Run itself.
type ExecDeps struct {
	// Rep renders install progress to the terminal.
	Rep progress.Reporter
	// Dynamic is a dynamic Kubernetes client for Apply/Wait closures.
	Dynamic dynamic.Interface
	// Typed is a typed Kubernetes client for Apply/Wait closures.
	Typed kubernetes.Interface
	// Apply server-side applies a set of manifest documents.
	Apply func(ctx context.Context, groups [][]byte) error
	// Recheck is a context that is NOT subject to the install --timeout. It is
	// passed to Phase.Await as recheckBase so that an exhausted overall timeout
	// cannot make a "keep waiting?" prompt a no-op. Set this to a
	// SIGINT-cancelable background context in callers.
	Recheck context.Context
	// OnApplied is called after each component's manifests land, so that a
	// Ctrl-C cleanup handler can scope removal to what was actually installed.
	// May be nil.
	OnApplied func(name string)
}

// Run executes the install pipeline as a dependency DAG, in three phases:
//
//	SECRETS → DAG(apply+wait, per component) → VERIFY
//
// SECRETS is a global phase: every component's Secrets runs before ANY
// component's Manifests are applied. This preserves the anti-deadlock guarantee
// — a readiness wait can never block while a dependency secret (e.g. the
// operator's webhook-TLS bundle, or a database credential) is still being
// written.
//
// The DAG phase launches one goroutine per component. Each goroutine:
//
//  1. waits for every DependsOn dependency to reach READY (its done-channel
//     closes — i.e. that dependency's apply+wait completed), or for the run to
//     be cancelled;
//  2. acquires the concurrency semaphore (AFTER gating, so a goroutine blocked
//     on a dependency never holds a slot);
//  3. APPLY: applies its Manifests (if any);
//  4. WAIT: drives its readiness Wait (if any);
//  5. closes its OWN done-channel — on ready, skip, OR fail — so dependents
//     unblock regardless of outcome.
//
// Independent components (no DependsOn) run fully in parallel. A dependent's
// APPLY now happens only AFTER its dependencies are READY — e.g. a gateway
// applies only once cert-manager is Ready, instead of crash-looping until the
// dependency materializes.
//
// DependsOn semantics: "this component's apply+wait does not start until every
// listed dependency has reached READY." This intentionally relaxes the older
// "all applies before all waits" rigidity — that rigidity is exactly what the
// DAG removes. The data-plane is modestly more serial (dependency-ordered) but
// cleaner (no crashloop-until-deps).
//
// A failing non-Optional component (apply OR wait) aborts the pipeline with a
// wrapped error and cancels the run; Optional ones warn/Skip and continue in a
// degraded state, still closing their done-channel so any dependents proceed.
func Run(ctx context.Context, comps []Component, d ExecDeps) error {
	// Validate the DependsOn graph (unknown references + cycles) BEFORE any side
	// effect (Secrets writes K8s objects) so a misconfigured component set fails
	// fast without leaving half-applied state.
	if err := validateDAG(comps); err != nil {
		return err
	}

	// ── SECRETS: all before any apply (anti-deadlock; UNCHANGED) ──────────────
	for _, comp := range comps {
		if comp.Secrets == nil {
			continue
		}
		if err := comp.Secrets(ctx); err != nil {
			if comp.Optional {
				d.Rep.Warn("secrets for %s failed (optional, continuing): %v", comp.Name, err)
				continue
			}
			return fmt.Errorf("secrets for %s: %w", comp.Name, err)
		}
	}

	// ── DAG: one goroutine per component, apply+wait gated on dependencies ─────
	if err := runDAG(ctx, comps, d); err != nil {
		return err
	}

	// ── VERIFY: call each Verify in registration order (UNCHANGED) ────────────
	for _, comp := range comps {
		if comp.Verify == nil {
			continue
		}
		if err := comp.Verify(ctx); err != nil {
			if comp.Optional {
				d.Rep.Warn("verify %s failed (optional, continuing): %v", comp.Name, err)
				continue
			}
			return fmt.Errorf("verify %s: %w", comp.Name, err)
		}
	}

	return nil
}

// validateDAG checks that every DependsOn names a known component and that the
// dependency graph is acyclic. It performs no side effects and must run before
// SECRETS so a misconfigured component set fails fast.
//
// Cycle detection uses iterative longest-path propagation: for N components an
// acyclic graph settles in at most N iterations; exceeding that proves a cycle
// (the longest-path depths would otherwise grow without bound).
func validateDAG(comps []Component) error {
	nameToIdx := make(map[string]int, len(comps))
	for i, c := range comps {
		nameToIdx[c.Name] = i
	}
	// Validate every DependsOn reference up front.
	for _, c := range comps {
		for _, dep := range c.DependsOn {
			if _, ok := nameToIdx[dep]; !ok {
				return fmt.Errorf("component %q: unknown DependsOn %q", c.Name, dep)
			}
		}
	}
	depth := make([]int, len(comps))
	for iter, changed := 0, true; changed; iter++ {
		if iter > len(comps) {
			// More iterations than an acyclic graph of this size could need → cycle.
			names := make([]string, len(comps))
			for i, c := range comps {
				names[i] = c.Name
			}
			return fmt.Errorf("cycle detected in component DependsOn graph; components: %v", names)
		}
		changed = false
		for i, c := range comps {
			for _, dep := range c.DependsOn {
				j := nameToIdx[dep]
				if depth[j]+1 > depth[i] {
					depth[i] = depth[j] + 1
					changed = true
				}
			}
		}
	}
	return nil
}

// runDAG executes the APPLY+WAIT body of every component as the dependency DAG
// described on Run. One goroutine per component gates on its dependencies'
// done-channels, then (under a bounded semaphore) applies its manifests and
// awaits its readiness, finally closing its own done-channel.
//
// Concurrency contract:
//   - done[name] closes exactly once, when that component's body finishes
//     (ready, skipped, or failed) — every component has a channel so a
//     DependsOn edge can gate on an apply-only (Wait==nil) dependency too.
//   - The semaphore is acquired AFTER dependency gating, so a goroutine blocked
//     on a dependency never holds a slot; this is what keeps the DAG free of
//     semaphore-starvation deadlock (a dependency always makes progress and
//     releases its slot before its dependents need one).
//   - A non-Optional apply/wait failure records the first error, cancels the
//     run, and aborts; Optional failures warn/Skip and continue.
//   - Optional-tail short-circuit: once all non-optional components finish,
//     optCtx is cancelled so still-pending optional waits exit promptly as
//     Skipped rather than running their full window. This applies to UNGATED
//     optional components only; one with a DependsOn edge waits under ctx, since
//     its gate opens exactly when the required set finishes (see the call site).
func runDAG(ctx context.Context, comps []Component, d ExecDeps) error {
	// Pre-declare every Phase row (components that either apply Manifests or run a
	// Wait) BEFORE launching any goroutine, so the checklist shows all of them
	// pending up front. A component with Manifests gets a row so its APPLY step can
	// render a determinate progress bar; a component with a Wait gets a row for its
	// readiness spinner. Components with both reuse the one row (bar → spinner).
	phases := make(map[string]progress.Phase, len(comps))
	for _, comp := range comps {
		if comp.Wait != nil || comp.Manifests != nil {
			phases[comp.Name] = d.Rep.Phase(comp.rowLabel())
		}
	}

	// One done-channel per component, closed when its body finishes (ready,
	// skipped, or failed).
	done := make(map[string]chan struct{}, len(comps))
	for _, comp := range comps {
		done[comp.Name] = make(chan struct{})
	}

	sem := make(chan struct{}, maxConcurrent)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// optCtx is cancelled once all required (non-optional) components finish so
	// any still-pending optional waits are short-circuited promptly. When there
	// are no required components, optional waits run their full window
	// (unchanged behaviour). A required failure cancels the parent ctx, which
	// propagates to optCtx via inheritance, so the failure path is unchanged.
	optCtx, optCancel := context.WithCancel(ctx)
	defer optCancel()

	// requiredWg tracks every non-optional component goroutine. The defer inside
	// each goroutine ensures Done is called regardless of how it exits (ready,
	// error, or early return during dependency gating), so the optional-tail
	// watcher is never stalled by a goroutine that bailed on cancellation.
	var requiredWg sync.WaitGroup
	requiredCount := 0
	for _, comp := range comps {
		if !comp.Optional {
			requiredCount++
		}
	}
	if requiredCount > 0 {
		requiredWg.Add(requiredCount)
		go func() {
			requiredWg.Wait()
			optCancel()
		}()
	}

	var wg sync.WaitGroup
	for _, comp := range comps {
		c := comp
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[c.Name])
			// Signal requiredWg when this non-optional goroutine exits for ANY
			// reason, so the optional-tail-cancel watcher is not stalled.
			if !c.Optional {
				defer requiredWg.Done()
			}

			// 1. Gate on every dependency reaching READY (its done-channel
			//    closes), or abort if the run was cancelled. DependsOn references
			//    always resolve here because every component has a done-channel.
			for _, dep := range c.DependsOn {
				if ch, ok := done[dep]; ok {
					select {
					case <-ch:
					case <-ctx.Done():
						return
					}
				}
			}
			// A required dependency may have failed — closing its done-channel
			// while cancelling the run. Don't start applying into a cancelled run.
			select {
			case <-ctx.Done():
				return
			default:
			}

			// 2. Bound concurrency. Acquired AFTER gating so a goroutine blocked
			//    on a dependency does not hold a semaphore slot.
			sem <- struct{}{}
			defer func() { <-sem }()

			ph := phases[c.Name] // nil only for components with neither Manifests nor Wait.

			// 3. APPLY this component's manifests (now that its deps are READY).
			//    applyOne drives ph's progress bar as each manifest group lands and
			//    handles Optional apply failures internally (warns, returns nil), so
			//    a non-nil error here is always fatal.
			if err := applyOne(ctx, c, d, ph); err != nil {
				if ph != nil {
					ph.Fail()
				}
				select {
				case errCh <- err:
					cancel()
				default:
				}
				return
			}

			// 4. WAIT for readiness, if this component declares one.
			if c.Wait == nil {
				// Apply-only component: no readiness spinner follows, so close out
				// the row now that its apply bar has completed.
				if ph != nil {
					ph.Done()
				}
				return
			}
			// The hard give-up window, which deliberately outlives the ETA the
			// soft-warn fires on (see waitDeadline / deadlineGrace).
			deadline := waitDeadline(c.Wait)

			// Wrap the poll so we learn whether the component was ALREADY ready on
			// the first poll — true on an idempotent re-run where it never went
			// down. firstReady is read only after the wait returns, in this same
			// goroutine (happens-after), so the read is race-free.
			poll, firstReady := firstPollProbe(c.Wait.Poll)

			if c.Optional {
				// An UNGATED optional races the required set, so it waits under
				// optCtx: once everything required is up, its wait is short-circuited
				// rather than holding the install for a component that is not coming.
				//
				// A GATED optional (graphiti behind neo4j) waits under ctx instead.
				// Its gate opens the instant its dependency reports Ready — and for
				// the slowest dependency that IS the instant the required set
				// finishes, so optCtx would already be cancelled when its wait began.
				// It would be skipped every time without ever being polled once,
				// turning the DependsOn edge that exists to make it start cleanly
				// into a guarantee that it never starts at all. Its own deadline
				// still bounds it.
				waitCtx := optCtx
				if len(c.DependsOn) > 0 {
					waitCtx = ctx
				}
				// Pass the ETA for the soft-warn.
				if err := ph.AwaitOptional(waitCtx, d.Recheck, deadline, c.Wait.ETA, poll, c.Wait.Diagnose); err != nil {
					ph.Skip(err.Error())
				} else {
					markDone(ph, *firstReady)
				}
				return
			}

			if err := ph.Await(ctx, d.Recheck, deadline, c.Wait.ETA, poll, c.Wait.Diagnose); err != nil {
				ph.Fail()
				select {
				case errCh <- fmt.Errorf("waiting for %s: %w", c.Name, err):
					cancel()
				default:
				}
				return
			}
			markDone(ph, *firstReady)
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// applyOne applies a single component's manifests and calls OnApplied. When ph
// is non-nil it renders a determinate progress bar that advances as each
// manifest group lands. It handles Optional components internally (warns and
// returns nil) so that the caller only receives fatal (non-Optional) errors.
//
// The groups are applied one at a time. This is byte-for-byte equivalent to a
// single Apply(allGroups): the shared apply body carries no cross-group state,
// so splitting it only changes the rendering granularity, never which bytes are
// applied or in what order. The bar is the only added effect.
func applyOne(ctx context.Context, comp Component, d ExecDeps, ph progress.Phase) error {
	if comp.Manifests != nil {
		groups, err := comp.Manifests()
		if err != nil {
			if comp.Optional {
				d.Rep.Warn("manifests for %s failed (optional, continuing): %v", comp.Name, err)
				return nil
			}
			return fmt.Errorf("manifests for %s: %w", comp.Name, err)
		}
		total := len(groups)
		if ph != nil && total > 0 {
			ph.Progress(0, total, "") // show the empty bar before the first group lands
		}
		for i, g := range groups {
			if err := d.Apply(ctx, [][]byte{g}); err != nil {
				if comp.Optional {
					d.Rep.Warn("apply for %s failed (optional, continuing): %v", comp.Name, err)
					return nil
				}
				return fmt.Errorf("apply for %s: %w", comp.Name, err)
			}
			if ph != nil {
				ph.Progress(i+1, total, "")
			}
		}
	}
	if d.OnApplied != nil {
		d.OnApplied(comp.Name)
	}
	return nil
}

// firstPollProbe wraps a Poll so the caller can learn whether the very first
// poll already reported ready — i.e. the component was already up (a re-run of
// `oap init`/`oap install` that found it healthy). The returned *bool is valid to
// read only AFTER the wait completes, from the same goroutine that ran the wait.
// sync.Once makes the first-result capture safe regardless of how many times the
// wrapped poll is called.
func firstPollProbe(p progress.Poll) (progress.Poll, *bool) {
	var once sync.Once
	ready := new(bool)
	return func(ctx context.Context) (bool, error) {
		done, err := p(ctx)
		once.Do(func() { *ready = done && err == nil })
		return done, err
	}, ready
}

// markDone completes a phase row, adding the "already running" re-run indicator
// when the component was ready on the first poll (a no-op re-run).
func markDone(ph progress.Phase, alreadyRunning bool) {
	if alreadyRunning {
		ph.DoneWith("already running")
		return
	}
	ph.Done()
}

// waitDeadline returns the hard give-up window for a WaitSpec: the explicit
// Deadline when set, else deadlineGrace × ETA, else defaultWaitDeadline. See
// deadlineGrace for why the deadline is deliberately a multiple of the ETA
// rather than the ETA itself.
func waitDeadline(w *WaitSpec) time.Duration {
	switch {
	case w.Deadline > 0:
		return w.Deadline
	case w.ETA > 0:
		return deadlineGrace * w.ETA
	default:
		return defaultWaitDeadline
	}
}
