// pkg/controllers/relationshipsource/controller.go
package relationshipsource

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credhost"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// defaultSyncInterval is the re-poll cadence when neither the controller nor
// the RelationshipSource spec sets one — matches spec.sync.interval's own
// doc comment ("Zero → controller default (15m)").
const defaultSyncInterval = 15 * time.Minute

// failureRetryInterval is the re-poll cadence after a failed pass — mirrors
// pkg/controllers/skillsource's constant of the same name and rationale: a
// failure is usually something an operator is actively repairing (a bad
// credential, an unregistered kind, a claim conflict), so the wait for the
// repair to be noticed is bounded to minutes rather than the full sync
// interval.
const failureRetryInterval = 2 * time.Minute

// verifyEveryNCycles is how often a pass ignores the stored hash sentinel
// and does a full read+diff even when a scope's sentinel matches — the
// policy task-4-brief.md's "Decided here" section settled on in place of a
// second configurable interval: a hash that drifted from reality (an
// out-of-band SpiceDB write, a bug) is caught within N cycles instead of
// trusted forever, with no new config surface. Default N = 10; at the
// default 15m interval that's a full verify roughly every 2.5 hours.
const verifyEveryNCycles = 10

// SpiceDBClient is the reconciler's narrow view of SpiceDB: relsync.Reader
// for Pass's reads (unguarded — reading a managed relation is normal), plus
// Writer to bind a guarded write surface to a KIND'S OWN claimed
// relsource.Source, never a source the reconciler picks itself. Satisfied by
// *spicedb.Client.
//
// Declared as the INTERFACE, never as *spicedb.Client: a typed-nil pointer
// assigned into an interface field yields a non-nil interface that panics on
// first call (AGENTS.md, "Nil interfaces"). Writer's return type is
// spicedb.RelWriter (not the narrower relsync.Writer Pass actually needs)
// because Go interface satisfaction does not do covariant returns —
// declaring it as relsync.Writer here would stop *spicedb.Client's own
// Writer method from satisfying this interface at all.
type SpiceDBClient interface {
	relsync.Reader
	Writer(src relsource.Source) spicedb.RelWriter
}

// RetryAfter is implemented by a kind's error to signal a rate limit with a
// server-specified backoff (Slack's 429, in particular — slack-go's own
// *slackapi.RateLimitedError carries its backoff as a FIELD, not a method,
// so pkg/channels/channelkinds/slack adapts it to this shape at every Slack
// API call site before returning; see that package's withRetryAfter).
// retryAfterFrom scans PassResult.ScopeErrors for it via errors.As, so a
// kind error wrapped by relsync's own "fetch: %w" / "relsync: enumerate
// scopes: %w" is still found — errors.As walks Unwrap() through both.
type RetryAfter interface {
	RetryAfter() time.Duration
}

// passPacer is process-local, in-memory bookkeeping for the two pacing
// decisions this controller makes: how often a pass fully re-verifies
// (verifyEveryNCycles), and the earliest moment the next pass may call
// UPSTREAM at all (notBefore). Neither decides what is safe to write.
//
// Deliberately not status. A deadline stored on the CR would be rewritten
// every time a new Retry-After arrived, and a status field that changes every
// pass is precisely the churn this whole area exists to stop — the fix would
// have been its own bug. Deliberately not pkg/controllers/internal/backoff
// either: that tracker is an EXPONENTIAL per-key failure backoff, and this is
// an absolute deadline armed by a SUCCESSFUL pass as readily as a throttled
// one, so reusing it would mean bending its semantics rather than sharing
// them.
//
// Losing it on an operator restart costs, at most: the next full verify is
// delayed by up to N cycles more, and each source is allowed ONE extra pass
// before its hold is re-armed. Both are cadence, never data safety
// (AGENTS.md's durability lens), and one extra upstream call per source per
// operator restart is not a rate-limit event.
//
// TODO(relationshipsource): the hold currently applies to every reason a
// reconcile can arrive, and that is right only while every reconcile is either
// a timer or a watch echo. invalidate.go describes an EVENT-DRIVEN scoped wake
// — an upstream webhook saying one named scope just changed, dispatched as a
// Pass with OnlyScopes set — and nothing wires OnWake to Reconcile yet. The day
// it does, the wake arrives as an ordinary enqueue and this gate will swallow
// it for up to a full interval, which is the exact opposite of what that file
// exists for: the whole point of a scoped wake is that it is NOT paced by the
// background cycle, and it costs one upstream call for one scope rather than
// the sweep the hold is protecting against.
//
// Deliberately NOT built ahead of the wiring: the escape hatch has to be keyed
// on something that distinguishes a real wake from a watch echo, and that
// discriminator is whatever OnWake ends up passing through — inventing one now
// would mean guessing at an interface that does not exist, and a bypass keyed
// on the wrong thing reopens the loop this gate closed. Whoever wires OnWake
// owns this: exempt the scoped path explicitly, and keep the unscoped one held.
type passPacer struct {
	mu    sync.Mutex
	state map[types.NamespacedName]*sourcePacing
}

// sourcePacing is one source's pacing record. A zero notBefore means no hold.
type sourcePacing struct {
	cycles    int
	notBefore time.Time
}

func (c *passPacer) entry(key types.NamespacedName) *sourcePacing {
	if c.state == nil {
		c.state = map[types.NamespacedName]*sourcePacing{}
	}
	s, ok := c.state[key]
	if !ok {
		s = &sourcePacing{}
		c.state[key] = s
	}
	return s
}

func (c *passPacer) shouldVerify(key types.NamespacedName) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entry(key).cycles%verifyEveryNCycles == 0
}

func (c *passPacer) recordCycleComplete(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry(key).cycles++
}

// holdUntil arms (or clears, with the zero time) the earliest moment the next
// pass for key may run. Always REPLACES: the deadline is recomputed from each
// pass's own outcome, so a source that stops being throttled stops being held.
func (c *passPacer) holdUntil(key types.NamespacedName, notBefore time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry(key).notBefore = notBefore
}

// remainingHold reports how long key must still wait before a pass may run,
// or 0 when it may run now.
func (c *passPacer) remainingHold(key types.NamespacedName, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.state[key]
	if !ok || s.notBefore.IsZero() {
		return 0
	}
	if d := s.notBefore.Sub(now); d > 0 {
		return d
	}
	return 0
}

// nextPassNotBefore is the deadline a just-finished pass arms for the next
// one. The zero time means no hold.
//
// Two independent reasons to wait, and the later of the two wins:
//
//   - A COMPLETED CYCLE waits out the source's own interval. There is nothing
//     left to make progress on until the next cycle is due, so a pass before
//     then can only re-ask upstream the same questions.
//   - A RETRY-AFTER waits however long upstream asked, completed cycle or not.
//     An upstream saying "not for 10 seconds" outranks our own cadence in the
//     direction of waiting longer.
//
// A cycle IN PROGRESS with no Retry-After arms nothing, and that is the
// important negative: with spec.sync.maxScopesPerPass set, a cycle is
// deliberately several passes in quick succession, and holding those would
// stall incremental sync outright rather than merely pace it.
// failedPassNotBefore is nextPassNotBefore's counterpart for a pass that
// returned an ERROR rather than a result. res cannot be trusted there (Pass's
// own doc reserves the error return for a failure it could not even attempt to
// recover from), so the floor is the cadence requeue is about to schedule —
// keeping the gate and the wake-up in agreement — and anything res DID manage
// to report can only push it later, never earlier.
func failedPassNotBefore(now time.Time, interval time.Duration, res relsync.PassResult) time.Time {
	notBefore := now.Add(min(interval, failureRetryInterval))
	if t := nextPassNotBefore(now, interval, res); t.After(notBefore) {
		notBefore = t
	}
	return notBefore
}

func nextPassNotBefore(now time.Time, interval time.Duration, res relsync.PassResult) time.Time {
	var notBefore time.Time
	if d, ok := retryAfterFrom(res.ScopeErrors); ok {
		notBefore = now.Add(d)
	}
	if res.CycleComplete {
		if t := now.Add(interval); t.After(notBefore) {
			notBefore = t
		}
	}
	return notBefore
}

// Reconciler resolves a RelationshipSource's credential, polls its upstream
// directory through the registered relsync.Kind, and writes/prunes the
// membership relationships it reports in SpiceDB, one pass at a time.
type Reconciler struct {
	Client client.Client
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
	// SpiceDB is required: without it no pass can read or write anything.
	// Declared as the interface per AGENTS.md's nil-interface rule.
	SpiceDB SpiceDBClient
	// MonitoringPublish reports a cross-source kind-claim conflict (two
	// RelationshipSources naming the same spec.kind) onto the fixed
	// monitoring bus. Optional: nil leaves the conflict logged only, same as
	// every other MonitoringPublish-carrying reconciler in this package
	// group (useridentity, guardian, agentsession, sandboxkinds).
	MonitoringPublish channelevents.PublishFunc
	// SyncInterval overrides the default re-poll cadence (15m).
	// spec.sync.interval, when set, takes precedence over both.
	SyncInterval time.Duration
	// Now is the clock. Test seam only — nil means time.Now. It exists
	// because the holds passPacer hands out are minutes to hours, and a test
	// that wanted to observe a pass resume after one elapsed would otherwise
	// have to sleep for it.
	Now func() time.Time

	pacing     passPacer
	monitoring monitoringTracker
}

// now reads the reconciler's clock, defaulting to time.Now.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// NewReconciler builds a Reconciler over its required dependencies.
// MonitoringPublish and SyncInterval are optional and may be set on the
// returned value afterward (mirrors pkg/controllers/guardian's
// NewReconciler + post-construction MonitoringPublish assignment).
func NewReconciler(c client.Client, secretReader *adoptguard.SecretReader, spiceDB SpiceDBClient) *Reconciler {
	return &Reconciler{
		Client:       c,
		SecretReader: secretReader,
		SpiceDB:      spiceDB,
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src v1.RelationshipSource
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &src); !cont {
		return ctrl.Result{}, err
	}
	if src.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Snapshot Ready's PRIOR state before anything below can mutate it —
	// publishRecovered needs this at the bottom of a successful pass, by
	// which point conditions.SetTrue has already overwritten it. Copying the
	// two fields out (rather than keeping the *metav1.Condition Find
	// returns) sidesteps any aliasing question: Find's pointer aims into
	// src.Status.Conditions' own backing array, which Set mutates in place.
	var priorReadyFailing bool
	var priorReadyReason string
	if c := conditions.Find(src.Status.Conditions, v1.RelationshipSourceConditionReady); c != nil {
		priorReadyFailing = c.Status == metav1.ConditionFalse
		priorReadyReason = c.Reason
	}

	interval := r.interval(&src)

	// Decision: refuse a second RelationshipSource of an already-claimed
	// kind. Two CRs sharing spec.kind share one Kind.Source() name, so the
	// SpiceDB write guard cannot tell them apart — see the package doc's
	// reap-vs-reap annihilation case.
	incumbent, err := r.findIncumbent(ctx, src.Spec.Kind, req.NamespacedName)
	if err != nil {
		logger.Info("RelationshipSource: list RelationshipSources for kind-claim check failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
		return ctrl.Result{}, err
	}
	if incumbent != nil {
		// Checked (and, if new, published) BEFORE the condition is mutated
		// inside requeue, so publishKindClaimed can tell a fresh conflict
		// from one already reported on a prior reconcile.
		r.publishKindClaimed(ctx, &src, incumbent)
		msg := fmt.Sprintf(
			"spec.kind %q is already claimed by RelationshipSource %s/%s (older by creationTimestamp); this source is parked and will not sync",
			scrubScopeErrorMessage(src.Spec.Kind), incumbent.Namespace, incumbent.Name)
		return r.requeue(ctx, &src, interval, v1.ReasonRelationshipSourceKindClaimed, msg)
	}

	kind, ok := relsync.Get(src.Spec.Kind)
	if !ok {
		logger.Info("RelationshipSource: unregistered kind",
			"relationshipsource", src.Name, "namespace", src.Namespace, "kind", src.Spec.Kind)
		return r.requeue(ctx, &src, interval, v1.ReasonRelationshipSourceKindUnregistered,
			// Scrubbed like every other tenant-writable value quoted into a
			// sink. spec.kind carries only MinLength=1 — no pattern, no
			// enum — so it is free text an operator typed, and it reaches
			// both the Ready condition and (via publishKindClaimed) a chat
			// channel. It is a registry key, so a credential has no business
			// being in one; that is an argument about what SHOULD be there,
			// and the scrub is about what CAN be.
			fmt.Sprintf("spec.kind %q is not a registered relsync kind",
				scrubScopeErrorMessage(src.Spec.Kind)))
	}

	creds, err := r.resolveCreds(ctx, &src)
	if err != nil {
		logger.Info("RelationshipSource auth resolution failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
		// Checked (and, if new, published) BEFORE requeue mutates the
		// condition — same ordering publishKindClaimed uses, for the same
		// reason: it is what lets publishCredResolveFailed tell a fresh
		// failure from one already reported on a prior reconcile.
		r.publishCredResolveFailed(ctx, &src, err)
		return r.requeue(ctx, &src, interval, v1.ReasonRelationshipSourceAuthResolveFailed,
			scrubScopeErrorMessage(err.Error()))
	}

	// The writer is bound to the KIND's own claimed Source, never a source
	// the reconciler picks itself — this is what makes the hash sentinel
	// (and every other write) trustworthy: relsource.CheckWrite refuses a
	// write naming a relation this Source didn't claim.
	writer := r.SpiceDB.Writer(kind.Source())

	// A cycle "starts" whenever the stored cursor is empty — either the very
	// first reconcile, or the reconcile right after the previous cycle
	// finished (relsync.Pass resets ResumeAfter to "" on CycleComplete).
	// Read BEFORE Pass overwrites it.
	startingNewCycle := src.Status.Sync.ResumeAfter == ""

	// ADMISSION CONTROL for the upstream call, and the reason it cannot be
	// expressed as a RequeueAfter.
	//
	// Observed live, against a real Slack workspace: a healthy first pass
	// (484 scopes, ~12k tuples written), then upstream began rate-limiting
	// conversations.info with Retry-After: 10s. Rate-limited scopes fail
	// DIFFERENTLY each pass, so the persisted counts differed every pass
	// (written 11943 → 68 → 50 → 67 → 51); different counts mean
	// applySyncResult reports moved; a status write fires the self-watch,
	// which has no predicate; and the resulting reconcile ran Pass again
	// immediately. Five reconciles in three minutes against a fifteen-minute
	// interval, and 999 rate-limit errors in five.
	//
	// The damning detail is that the backoff was already being honoured:
	// retryAfterFrom read the 10s and set requeueAfter to it. RequeueAfter is
	// only a request for a wake-up, and a watch event is a wake-up nobody
	// requested, arriving sooner. The controller read an explicit back-off
	// request, scheduled it, and then overrode its own schedule.
	//
	// So the gate is here rather than in the returned Result: a watch event
	// may requeue this reconcile — it must not be able to make an upstream
	// call. This is also why the check sits HERE and not at the top of
	// Reconcile. Everything above is local (the kind-claim List, the
	// credential resolve), and an operator editing a broken spec must see it
	// re-evaluated now, not at the next interval; only the upstream call is
	// paced.
	//
	// A spec edit bypasses the hold outright, for the same reason: someone who
	// just changed this source is asking for the change to be applied, not to
	// wait out a cadence armed under the old spec. That cannot re-open the
	// loop, because the loop is driven by STATUS writes and those never bump
	// the generation.
	if src.Status.ObservedGeneration == src.Generation {
		if wait := r.pacing.remainingHold(req.NamespacedName, r.now()); wait > 0 {
			logger.V(1).Info("RelationshipSource: upstream pass held, not calling upstream",
				"relationshipsource", src.Name, "namespace", src.Namespace,
				"wait", wait.String(), "resumeAfter", src.Status.Sync.ResumeAfter)
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	res, err := relsync.Pass(ctx, relsync.PassInput{
		Kind:         kind,
		Creds:        creds,
		Writer:       writer,
		Reader:       r.SpiceDB,
		ResumeAfter:  relsync.ScopeID(src.Status.Sync.ResumeAfter),
		MaxScopes:    int(src.Spec.Sync.MaxScopesPerPass),
		IgnoreHashes: r.pacing.shouldVerify(req.NamespacedName),
	})
	if err != nil {
		// relsync.Pass reserves its error return for a failure it cannot even
		// attempt to recover from; every anticipated failure (enumeration, a
		// scope's fetch/write, the reap scan) is folded into
		// res.ScopeErrors instead — see Pass's own doc.
		logger.Info("RelationshipSource pass failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
		// Armed even though this branch is unreachable today (Pass has exactly
		// one return, and it is nil). It is the ONE path that reached upstream
		// and would otherwise owe nothing, so leaving it unarmed is a trap
		// rather than a saving: the day Pass grows an error return that carries
		// a RetryAfter, this branch alone would let a watch event re-enter the
		// upstream call immediately and the loop reopens from the end nobody is
		// watching. The hold matches what requeue is about to schedule, and
		// still defers to anything res managed to report.
		r.pacing.holdUntil(req.NamespacedName,
			failedPassNotBefore(r.now(), interval, res))
		return r.requeue(ctx, &src, interval, v1.ReasonRelationshipSourcePassFailed,
			scrubScopeErrorMessage(err.Error()))
	}

	// Arm the next hold from THIS pass's outcome, before any of the branches
	// below can return. Every one of them is reached only by having ALREADY
	// called upstream, so every one of them has to leave the gate armed — the
	// enumeration-failure return in particular, which is exactly where a
	// rate-limited enumeration lands.
	r.pacing.holdUntil(req.NamespacedName, nextPassNotBefore(r.now(), interval, res))

	// One scope failing is not a pass failure (relsync.Pass's own doc) —
	// logged per AGENTS.md's no-silent-errors rule, never fatal to Ready.
	for _, se := range res.ScopeErrors {
		logger.Info("RelationshipSource: scope error (non-fatal)",
			"relationshipsource", src.Name, "namespace", src.Namespace,
			"scope", se.Scope, "err", se.Err.Error())
	}

	// "One scope failing is not a pass failure" does not extend to a TOTAL
	// enumeration failure: EnumComplete=false with zero scopes even visited
	// means nothing was synced this pass at all, not "one scope, non-fatal".
	// Without this check that state folds into ScopeErrors and the success
	// path below sets Ready=True/Synced regardless — a revoked token,
	// missing_scope, or network outage would then show as healthy on a
	// source that has never written a tuple.
	if !res.EnumComplete && res.Processed == 0 {
		return r.requeue(ctx, &src, interval, v1.ReasonRelationshipSourceEnumerationFailed,
			enumerationFailedMessage(res.ScopeErrors))
	}

	if res.CycleComplete {
		r.pacing.recordCycleComplete(req.NamespacedName)
	}

	if moved := applySyncResult(&src, &src.Status, startingNewCycle, res, passWrote(res), false); moved {
		if err := r.Client.Status().Update(ctx, &src); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reported regardless of whether status actually changed: recovery and
	// the per-pass issue set are facts about THIS pass, not about whether
	// the write above was a no-op.
	r.publishRecovered(ctx, &src, priorReadyFailing, priorReadyReason)
	r.publishPassIssues(ctx, &src, res)

	// Honour a 429: a kind's error carrying a Retry-After requeues after
	// that delay instead of the normal interval. The progress this pass
	// already made was persisted above regardless.
	requeueAfter := interval
	if d, ok := retryAfterFrom(res.ScopeErrors); ok && d > 0 {
		requeueAfter = d
	}
	// The `d > 0` is load-bearing, and it is the price of retryAfterFrom no
	// longer discarding a zero. A literal `Retry-After: 0` now reports
	// (0, true) — correctly, since upstream DID say it was throttling — and
	// assigning that here would produce ctrl.Result{RequeueAfter: 0}, which
	// controller-runtime reads as "do not requeue" rather than "requeue
	// immediately". Combined with the hold below blocking watch-driven
	// wake-ups, that would strand the source with no scheduled work at all.
	// Upstream asking for a zero wait means the interval governs, which is
	// exactly what this line leaves in place.
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// passWrote reports whether a Pass actually changed SpiceDB — the signal
// that forces a status write even when the stored ResumeAfter/EnumComplete
// happen to compute to the same values as before (recreating something
// deleted out from under the source, say).
func passWrote(res relsync.PassResult) bool {
	return res.Written > 0 || res.Pruned > 0 || res.ReapedScopes > 0
}

// applySyncResult folds one Pass's outcome onto st and reports whether
// anything actually needs to be persisted. A reconcile that changes nothing
// must not rewrite the status subresource (AGENTS.md, SSA idempotency) — the
// self-watch below has no predicate, so an unconditional write would
// re-enqueue this reconcile, and its Pass, forever.
//
// CycleStartedAt/LastSyncTime are deliberately excluded from the moved
// comparison itself: every steady-state reconcile against a small/unbounded
// source completes its own one-pass "cycle" (CycleComplete=true,
// ResumeAfter="" both before and after), so unconditionally re-stamping
// either timestamp would churn status on literally every reconcile even
// though nothing observable changed. They are stamped only once moved is
// already true for another reason — mirrors skillsource's RecordSync, whose
// own comment notes lastSyncTime is compared before being touched.
//
// Recording LastPass's counts means a pass that wrote is now always followed
// by exactly one extra status write and re-enqueue: the pass after a write
// reports Written/Pruned/ReapedScopes back at 0, which is a genuine
// difference from the previous non-zero LastPass, so moved is true one more
// time before the source settles. That is bounded — it converges on the very
// next pass, unlike the FinishedAt hazard above, which never converges — but
// it is a real, expected extra reconcile, not a bug; name it here so the next
// reader chasing "why did this reconcile twice" doesn't have to re-derive it.
//
// The scope-error half (count, sample, PartialFailure condition) rides this
// same write and is subject to this same hazard, from a worse direction:
// error text is authored upstream and one of the kinds already renders a live
// duration into it. scopeerrors.go carries that reasoning and the guard.
func applySyncResult(obj conditions.Generationer, st *v1.RelationshipSourceStatus, startingNewCycle bool, res relsync.PassResult, wrote bool, scoped bool) bool {
	prev := st.DeepCopy()

	st.ObservedGeneration = obj.GetGeneration()
	st.Sync.ResumeAfter = string(res.ResumeAfter)
	st.Sync.EnumComplete = res.EnumComplete
	conditions.SetTrue(obj, &st.Conditions, v1.RelationshipSourceConditionReady, v1.ReasonRelationshipSourceSynced)

	// Carry the prior stamp rather than minting a new one: FinishedAt is
	// excluded from the moved comparison for exactly the reason
	// CycleStartedAt/LastSyncTime are (see this function's doc). Build the
	// candidate with the OLD timestamp so two identical passes compare equal,
	// then stamp below only once moved is already true.
	var carriedFinishedAt *metav1.Time
	priorPass := st.Sync.LastPass
	if priorPass != nil {
		carriedFinishedAt = priorPass.FinishedAt
	}

	// The partial-failure half. res.ScopeErrors is non-fatal to Ready by
	// design and was, until now, logged and then discarded — so an entire arm
	// of a sync could fail with the CR still reading Ready/Synced. The count
	// and the bounded sample land here, and the PartialFailure condition below
	// is set from the SAME PassResult on the SAME write, so the two can never
	// disagree. See scopeerrors.go for why the sample is capped, scrubbed, and
	// gated on a stable key rather than persisted as rendered.
	scopeErrorCount := int32(len(res.ScopeErrors))
	samples := preserveStableSamples(priorPass, scopeErrorCount, buildScopeErrorSamples(res.ScopeErrors))

	st.Sync.LastPass = &v1.RelationshipSourcePassStats{
		ScopesProcessed:   int32(res.Processed),
		Written:           int32(res.Written),
		Pruned:            int32(res.Pruned),
		ReapedScopes:      int32(res.ReapedScopes),
		JoinMisses:        int32(res.JoinMisses),
		ScopeErrors:       scopeErrorCount,
		ScopeErrorSamples: samples,
		Scoped:            scoped,
		FinishedAt:        carriedFinishedAt,
	}

	// Set through conditions.Set rather than SetTrue/SetFalse because this
	// condition is True-is-bad (AgentSessionConditionFailed's polarity, not
	// Ready's) and the True arm carries a message.
	partial := metav1.Condition{
		Type:    v1.RelationshipSourceConditionPartialFailure,
		Status:  metav1.ConditionFalse,
		Reason:  v1.ReasonRelationshipSourceAllScopesSynced,
		Message: "",
	}
	if scopeErrorCount > 0 {
		partial.Status = metav1.ConditionTrue
		partial.Reason = v1.ReasonRelationshipSourceScopeErrors
		partial.Message = partialFailureMessage(scopeErrorCount, samples)
	}
	conditions.Set(obj, &st.Conditions, partial)

	moved := wrote || !equality.Semantic.DeepEqual(*prev, *st)
	if !moved {
		return false
	}
	if startingNewCycle {
		now := metav1.Now()
		st.Sync.CycleStartedAt = &now
	}
	if res.CycleComplete {
		now := metav1.Now()
		st.Sync.LastSyncTime = &now
	}
	stamp := metav1.Now()
	st.Sync.LastPass.FinishedAt = &stamp
	return true
}

// retryAfterFrom returns the LARGEST Retry-After any scope error in errs
// reports, so a pass honours the most conservative backoff a kind asked
// for, rather than the first one encountered.
//
// The reported flag tracks whether a kind said "throttled" AT ALL, separately
// from how long it asked for. Folding the two together — setting found only
// when a duration beat the running maximum — silently discarded a ZERO. Any
// kind may report zero: RetryAfter is a public, kind-agnostic interface, and
// nothing constrains what an implementation returns. Both kinds registered
// today happen to floor their backoff at one second, so neither produces a
// zero in production right now; pacing_test.go exercises the zero case
// directly rather than relying on a kind to keep producing one. The difference
// used to be cosmetic, because a zero-length requeue and the ordinary interval
// land in much the same place. It stopped
// being cosmetic when passPacer began reading this to decide whether a
// MID-CYCLE pass may call upstream at all: a dropped zero is not "wait no
// time", it is "upstream never said it was throttling", and that is the answer
// that takes the mid-cycle exemption.
func retryAfterFrom(errs []relsync.ScopeError) (time.Duration, bool) {
	var longest time.Duration
	var reported bool
	for _, se := range errs {
		var ra RetryAfter
		if errors.As(se.Err, &ra) {
			reported = true
			if d := ra.RetryAfter(); d > longest {
				longest = d
			}
		}
	}
	return longest, reported
}

// findIncumbent returns the RelationshipSource that legitimately owns kind —
// the oldest by creationTimestamp, tie-broken by namespace then name, so two
// reconciles (of either CR) can never disagree about which one wins — or nil
// when self is that CR (no conflict).
//
// Cluster-wide, not per-namespace: a Kind's Source().Name (and therefore its
// Claims) is the same for every RelationshipSource of that kind regardless
// of namespace, so uniqueness has to be enforced across the whole cluster —
// see the package doc's reap-vs-reap case.
func (r *Reconciler) findIncumbent(ctx context.Context, kind string, self types.NamespacedName) (*v1.RelationshipSource, error) {
	var list v1.RelationshipSourceList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list RelationshipSources: %w", err)
	}

	var claimants []*v1.RelationshipSource
	for i := range list.Items {
		if list.Items[i].Spec.Kind == kind {
			claimants = append(claimants, &list.Items[i])
		}
	}
	if len(claimants) == 0 {
		return nil, nil
	}
	sort.Slice(claimants, func(i, j int) bool {
		a, b := claimants[i], claimants[j]
		ta, tb := a.CreationTimestamp, b.CreationTimestamp
		if !ta.Equal(&tb) {
			return ta.Before(&tb)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	incumbent := claimants[0]
	if incumbent.Namespace == self.Namespace && incumbent.Name == self.Name {
		return nil, nil
	}
	return incumbent, nil
}

// publishKindClaimed reports a newly-detected cross-source kind conflict.
// Checked against src's PRIOR condition (before the caller mutates it), so a
// conflict already reported on an earlier reconcile is not re-announced on
// every interval for as long as an operator takes to resolve it.
func (r *Reconciler) publishKindClaimed(ctx context.Context, src *v1.RelationshipSource, incumbent *v1.RelationshipSource) {
	logger := log.FromContext(ctx)
	if already := conditions.Find(src.Status.Conditions, v1.RelationshipSourceConditionReady); already != nil &&
		already.Status == metav1.ConditionFalse && already.Reason == v1.ReasonRelationshipSourceKindClaimed {
		return
	}
	if r.MonitoringPublish == nil {
		logger.V(1).Info("RelationshipSource: kind-claim conflict not reported (no monitoring publisher configured)",
			"relationshipsource", src.Name, "namespace", src.Namespace, "kind", src.Spec.Kind,
			"incumbent", incumbent.Namespace+"/"+incumbent.Name)
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "reconcile",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "RelationshipSource", Namespace: src.Namespace, Name: src.Name,
		},
		Condition: v1.RelationshipSourceConditionReady,
		Reason:    v1.ReasonRelationshipSourceKindClaimed,
		Summary: fmt.Sprintf(
			"spec.kind %q is claimed by both %s/%s and %s/%s; only the older CR syncs, the other is parked",
			scrubScopeErrorMessage(src.Spec.Kind), incumbent.Namespace, incumbent.Name, src.Namespace, src.Name),
		Hint:      "delete or repoint one of the two RelationshipSources so only one claims this kind",
		Timestamp: time.Now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		logger.Info("publish RelationshipSource kind-claim conflict event failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
	}
}

// resolveCreds resolves spec.auth to relsync.SourceParams, carrying
// spec.baseURL through to SourceParams.Endpoint for a kind whose upstream is
// customer-hosted, and spec.config through to SourceParams.Config verbatim
// — but only once the credential is allowed to go there.
//
// WHERE the credential may go, before resolving WHAT it is.
//
// spec.baseURL and spec.auth are two independent fields on the same
// tenant-writable object. With nothing tying them together, a CR naming
// `baseURL: https://attacker.example` plus ANY credential on ANY AgentIdentity
// in the namespace made the operator send that credential's raw value as
// `Authorization: Bearer …` to the attacker's host — and the operator reads
// the Secret with its OWN cluster-wide credentials, so the actor never needed
// `get secrets`. That is the SkillSource incident (see credhost's package doc)
// in a second CR of the same shape; the check below is the same fix, in the
// same place in the sequence.
//
// It cannot be the kind's job, however natural that split looks: a kind
// receives only relsync.SourceParams{Token, Endpoint}. It never sees the
// AgentCredential, so it cannot compare the destination against that
// credential's allowedHosts. No kind can do this check.
//
// An EMPTY spec.baseURL is checked against nothing, deliberately. There is no
// tenant-writable destination then — the kind dials its own constant host
// (Slack talks to slack.com, full stop) — so demanding a scope there would
// break every Slack source on upgrade for no gain.
func (r *Reconciler) resolveCreds(ctx context.Context, src *v1.RelationshipSource) (relsync.SourceParams, error) {
	auth := src.Spec.Auth

	var id v1.AgentIdentity
	key := client.ObjectKey{Namespace: src.Namespace, Name: auth.AgentIdentity}
	if err := r.Client.Get(ctx, key, &id); err != nil {
		return relsync.SourceParams{}, fmt.Errorf("get AgentIdentity %s/%s: %w", src.Namespace, auth.AgentIdentity, err)
	}

	var cred *v1.AgentCredential
	for i := range id.Spec.Credentials {
		if id.Spec.Credentials[i].Name == auth.Credential {
			cred = &id.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return relsync.SourceParams{}, fmt.Errorf("AgentIdentity %s/%s has no credential %q",
			src.Namespace, auth.AgentIdentity, auth.Credential)
	}

	// Checked before the Secret is adopted or read, so a refusal never touches
	// the value: AdoptSecret below stamps the adoption label (a write), and
	// ResolveSecretValue reads the token itself.
	//
	// An UNSCOPED credential is refused here rather than allowed. credhost.Check
	// has to treat empty allowedHosts as permissive — anything else breaks every
	// existing credential on upgrade — but there is no safe default destination
	// for a SCIM bearer token, so this path demands the scope instead of
	// inheriting that default.
	if src.Spec.BaseURL != "" {
		if len(cred.AllowedHosts) == 0 {
			return relsync.SourceParams{}, fmt.Errorf(
				"relationshipsource %s/%s: credential %q on AgentIdentity %q declares no allowedHosts, "+
					"so it may not be sent to spec.baseURL. Set spec.credentials[].allowedHosts on that credential "+
					"to the upstream host(s) it belongs to (e.g. [\"scim.example.com\"])",
				src.Namespace, src.Name, cred.Name, auth.AgentIdentity)
		}
		if err := credhost.Check(*cred, src.Spec.BaseURL); err != nil {
			return relsync.SourceParams{}, fmt.Errorf("relationshipsource %s/%s: %w", src.Namespace, src.Name, err)
		}
	}

	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return relsync.SourceParams{}, fmt.Errorf("relationshipsource %s/%s credential %q: %w",
			src.Namespace, src.Name, cred.Name, err)
	}
	ref := k.SecretRef(*cred)
	if ref == nil {
		return relsync.SourceParams{}, fmt.Errorf(
			"relationshipsource %s/%s credential %q: type=%s has no backing Secret to read a token from",
			src.Namespace, src.Name, cred.Name, cred.Type)
	}

	// Adopt the referenced Secret before reading it — metadata-only SSA
	// stamping AdoptedLabel so subsequent reads are permitted. Uses the live
	// reader (r.SecretReader.Reader) so a not-yet-adopted Secret (absent from
	// the label-filtered cache) is still seen.
	ownerRef := types.NamespacedName{Namespace: src.Namespace, Name: src.Name}
	secretRef := types.NamespacedName{Namespace: src.Namespace, Name: ref.Name}
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "RelationshipSource"); err != nil {
		return relsync.SourceParams{}, fmt.Errorf("adopt Secret %s/%s: %w", src.Namespace, ref.Name, err)
	}

	val, err := credresolve.ResolveSecretValue(ctx, r.SecretReader.Reader, src.Namespace, *cred)
	if err != nil {
		return relsync.SourceParams{}, fmt.Errorf("resolve credential %q: %w", cred.Name, err)
	}

	params := relsync.SourceParams{Token: val, Endpoint: src.Spec.BaseURL}
	if src.Spec.Config != nil {
		params.Config = src.Spec.Config.Raw
	}
	return params, nil
}

// requeue sets Ready=False (reason/message) and returns a requeue-after
// result with no error (failures are condition-surfaced, not returned — see
// package doc).
//
// The status write is skipped when this reconcile changed nothing — the
// SAME before/after comparison applySyncResult uses for the success path,
// applied here to the failure paths. Every one of requeue's four callers
// (kind-claimed, kind-unregistered, auth-resolve-failed, pass-failed) used
// to call conditions.SetFalse itself and then write unconditionally; a
// REPEATED failure (an unregistered kind that stays unregistered, a park
// that stays parked) rewrote byte-identical status on every single
// reconcile. That is not cosmetic: SetupWithManager watches this object
// with no predicate, so a status-only write re-enqueues this same
// reconcile — a park or an unregistered kind free-ran a tight loop instead
// of polling calmly at failureRetryInterval. Centralized here rather than
// re-guarded at each of the four call sites, so a future failure path can't
// forget it (see AGENTS.md's "max DRY: force onto the abstraction").
//
// Every caller is a failure path, so the cadence is clamped to
// failureRetryInterval here rather than at each call site. The clamp is a
// floor on frequency, never a ceiling: a source asking to be polled faster
// than the failure cadence keeps its own interval.
func (r *Reconciler) requeue(ctx context.Context, src *v1.RelationshipSource, interval time.Duration, reason, message string) (ctrl.Result, error) {
	prev := src.Status.DeepCopy()

	src.Status.ObservedGeneration = src.GetGeneration()
	conditions.SetFalse(src, &src.Status.Conditions, v1.RelationshipSourceConditionReady, reason, message)

	if !equality.Semantic.DeepEqual(*prev, src.Status) {
		if err := r.Client.Status().Update(ctx, src); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: min(interval, failureRetryInterval)}, nil
}

// interval picks the effective re-poll cadence: spec.sync.interval, else the
// reconciler's SyncInterval, else the 15m default.
func (r *Reconciler) interval(src *v1.RelationshipSource) time.Duration {
	if d := src.Spec.Sync.Interval.Duration; d > 0 {
		return d
	}
	if r.SyncInterval > 0 {
		return r.SyncInterval
	}
	return defaultSyncInterval
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.RelationshipSource{}).
		Complete(r)
}
