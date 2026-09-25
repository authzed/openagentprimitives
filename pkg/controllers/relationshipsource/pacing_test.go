// pkg/controllers/relationshipsource/pacing_test.go
//
// The upstream-call throttle. Everything here is about ONE question: can a
// reconcile this controller did not schedule cause a call to the upstream
// directory? Before passPacer the answer was yes, and it was observed in
// production against a real workspace — a rate-limited sync whose per-pass
// counts differed every pass, so every pass wrote status, and every status
// write re-enqueued through the predicate-less self-watch: five reconciles in
// three minutes against a fifteen-minute interval, 999 rate-limit errors in
// five.
package relationshipsource

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// manualClock only moves when a test says so — the opposite of
// monitoring_test.go's intervalClock, and the right tool here, because these
// tests are about the hold itself rather than about what happens across
// intervals.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// upstreamCalls counts how many times this kind was asked to enumerate. Each
// relsync.Pass over a single complete page calls ListScopes exactly once, so
// against the one-page fixtures below this IS the pass count — and a pass is
// exactly the thing the throttle exists to stop.
func upstreamCalls(fk *fakeKind) int { return len(fk.receivedCreds()) }

// reconcileOnceFor runs one reconcile and fails the test on error.
func reconcileOnceFor(t *testing.T, r *Reconciler, key types.NamespacedName) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	return res
}

// newPacingFixtures builds a one-scope source whose every pass completes a
// cycle, on a manual clock.
func newPacingFixtures(t *testing.T, kindName string, interval time.Duration) (*Reconciler, types.NamespacedName, *fakeKind, *manualClock) {
	t.Helper()
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:    kindName,
		source:  relsource.Source{Name: kindName + "-sync"},
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	relsync.Register(fk)

	clock := newManualClock()
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb,
		SyncInterval: interval, Now: clock.now,
	}
	return r, types.NamespacedName{Namespace: "ns", Name: "src"}, fk, clock
}

// THE regression test. The production shape, reproduced at the seam that
// caused it: a pass whose counts differ from the last one, so status moves,
// so the self-watch (no predicate) re-enqueues immediately. The re-enqueue is
// modelled by calling Reconcile again straight away, which is exactly what the
// watch does.
//
// Status moving is CORRECT and is asserted, not suppressed — different counts
// are genuinely new information. What must not follow is another upstream
// call.
func TestReconcile_AStatusWriteDoesNotForceAnotherUpstreamCall(t *testing.T) {
	r, key, fk, clock := newPacingFixtures(t, "fakekind-pacing-watchloop", 15*time.Minute)

	reconcileOnceFor(t, r, key)
	require.Equal(t, 1, upstreamCalls(fk), "precondition: the first reconcile calls upstream")

	var first spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &first))

	// The next pass would report different counts, the way a rate-limited
	// directory does: scopes that failed last time succeed this time and vice
	// versa, so written/joinMisses differ every pass and status moves every
	// pass. Stage that change so this test cannot pass merely because the
	// second pass would have been a no-op.
	fk.members = map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice"), tup("C1", "bob")}}

	// The self-watch fires on the status write above.
	held := reconcileOnceFor(t, r, key)
	assert.Equal(t, 1, upstreamCalls(fk),
		"a watch event may requeue this reconcile; it must not make an upstream call")
	assert.Greater(t, held.RequeueAfter, time.Duration(0),
		"a held reconcile must ask to be woken when the hold expires, not drop the source")
	assert.LessOrEqual(t, held.RequeueAfter, 15*time.Minute)

	var second spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &second))
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a held reconcile observed nothing, so it must write no status — or it feeds the loop it just refused to join")

	// Five more watch events inside the interval, the density the live
	// incident produced. Still one call.
	for range 5 {
		reconcileOnceFor(t, r, key)
	}
	assert.Equal(t, 1, upstreamCalls(fk), "the hold must survive repeated wake-ups, not just the first")

	// Once the interval has genuinely elapsed, the source resumes.
	clock.advance(15 * time.Minute)
	reconcileOnceFor(t, r, key)
	assert.Equal(t, 2, upstreamCalls(fk), "the throttle must pace the sync, not stop it")

	var third spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &third))
	assert.NotEqual(t, second.ResourceVersion, third.ResourceVersion,
		"the pass that did run reported different counts, which is real news and must be persisted")
}

// A cycle IN PROGRESS must not be throttled. With spec.sync.maxScopesPerPass
// set, a cycle is deliberately several passes in quick succession; holding
// those would not pace incremental sync, it would stall it — the source would
// advance one budget per interval and could never finish a large directory.
func TestReconcile_MidCyclePassesAreNotThrottled(t *testing.T) {
	const kindName = "fakekind-pacing-midcycle"
	src := newSrc("ns", "src", kindName)
	src.Spec.Sync.MaxScopesPerPass = 1
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages: []relsync.ScopePage{
			{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Next: relsync.Cursor{Token: "1"}, Complete: false},
			{Scopes: []relsync.Scope{{ID: "C2", ResourceType: "fake_scope"}}, Next: relsync.Cursor{}, Complete: true},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "alice")},
			"C2": {tup("C2", "bob")},
		},
	}
	relsync.Register(fk)

	clock := newManualClock()
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb,
		SyncInterval: time.Hour, Now: clock.now,
	}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	reconcileOnceFor(t, r, key)

	var afterFirst spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &afterFirst))
	require.Equal(t, "C1", afterFirst.Status.Sync.ResumeAfter,
		"precondition: the budgeted pass stopped mid-cycle")

	// NOT advanced: the clock stays put, so only the mid-cycle rule can let
	// this through.
	reconcileOnceFor(t, r, key)
	assert.Equal(t, []relsync.ScopeID{"C1", "C2"}, fk.fetchedIDs(),
		"a mid-cycle pass must run immediately; throttling it stalls incremental sync outright")

	var afterSecond spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &afterSecond))
	require.Empty(t, afterSecond.Status.Sync.ResumeAfter, "precondition: the cycle finished")

	// And the moment the cycle DOES finish, the interval applies again.
	before := len(fk.fetchedIDs())
	reconcileOnceFor(t, r, key)
	assert.Len(t, fk.fetchedIDs(), before,
		"a completed cycle is held for the interval, mid-cycle exemption spent")
}

// Retry-After outranks both rules. An upstream that has asked for a backoff
// must not be called again until it elapses — mid-cycle or not, which is the
// case the mid-cycle exemption above would otherwise wave straight through.
//
// This is the half the live incident proved was missing: retryAfterFrom DID
// read Slack's Retry-After and DID set RequeueAfter to it. A requeue is only a
// request for a wake-up, and the status write delivered an earlier one.
func TestReconcile_RetryAfterHoldsEvenMidCycle(t *testing.T) {
	const kindName = "fakekind-pacing-retryafter"
	src := newSrc("ns", "src", kindName)
	src.Spec.Sync.MaxScopesPerPass = 1
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages: []relsync.ScopePage{
			{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Next: relsync.Cursor{Token: "1"}, Complete: false},
			{Scopes: []relsync.Scope{{ID: "C2", ResourceType: "fake_scope"}}, Next: relsync.Cursor{}, Complete: true},
		},
		fetchErrs: map[relsync.ScopeID]error{
			"C1": fmt.Errorf("upstream: %w", &fakeRateLimitErr{after: 10 * time.Second}),
		},
	}
	relsync.Register(fk)

	clock := newManualClock()
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb,
		SyncInterval: time.Hour, Now: clock.now,
	}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	res := reconcileOnceFor(t, r, key)
	require.Equal(t, 10*time.Second, res.RequeueAfter,
		"precondition: the backoff was read and scheduled, exactly as it was in production")

	var mid spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &mid))
	require.Equal(t, "C1", mid.Status.Sync.ResumeAfter,
		"precondition: this is a MID-CYCLE pass, the case the exemption would otherwise let through")

	before := upstreamCalls(fk)
	reconcileOnceFor(t, r, key)
	assert.Equal(t, before, upstreamCalls(fk),
		"an upstream that asked for 10s must not be called again inside them, mid-cycle or not")

	clock.advance(10 * time.Second)
	reconcileOnceFor(t, r, key)
	assert.Greater(t, upstreamCalls(fk), before,
		"once the backoff has elapsed the sync must resume, not stay parked")
}

// A rate-limited ENUMERATION must arm the hold too. That pass returns down the
// Ready=False branch, before the normal success path, and it is the shape a
// throttled directory hits first: the very first upstream call of the pass is
// the one being refused, so nothing is synced and there is nothing to report
// but the failure. Arming only on the success path would leave exactly the
// worst case unthrottled.
func TestReconcile_ARateLimitedEnumerationArmsTheHold(t *testing.T) {
	r, key, fk, clock := newPacingFixtures(t, "fakekind-pacing-enumratelimit", time.Hour)
	fk.listErr = fmt.Errorf("upstream: %w", &fakeRateLimitErr{after: 30 * time.Second})

	reconcileOnceFor(t, r, key)
	require.Equal(t, 1, upstreamCalls(fk), "precondition: enumeration was attempted and refused")

	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &got))
	ready := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status,
		"precondition: a total enumeration failure is the Ready=False branch, not the success path")

	reconcileOnceFor(t, r, key)
	assert.Equal(t, 1, upstreamCalls(fk),
		"the branch that returns early still called upstream, so it still owes the backoff")

	clock.advance(30 * time.Second)
	reconcileOnceFor(t, r, key)
	assert.Equal(t, 2, upstreamCalls(fk), "and it resumes once the backoff elapses")
}

// The two reasons to wait compose by taking the LATER, not the last one
// written. A completed cycle on a 1h interval whose pass also reported a 10s
// backoff waits the hour: the source's own cadence is the longer wait, and
// Retry-After is a floor on waiting, never a licence to poll sooner.
func TestReconcile_CompletedCycleAndRetryAfterTakeTheLongerWait(t *testing.T) {
	r, key, fk, clock := newPacingFixtures(t, "fakekind-pacing-longer", time.Hour)
	fk.fetchErrs = map[relsync.ScopeID]error{
		"C1": fmt.Errorf("upstream: %w", &fakeRateLimitErr{after: 10 * time.Second}),
	}

	reconcileOnceFor(t, r, key)
	require.Equal(t, 1, upstreamCalls(fk))

	clock.advance(11 * time.Second)
	reconcileOnceFor(t, r, key)
	assert.Equal(t, 1, upstreamCalls(fk),
		"the 10s backoff elapsing does not shorten the source's own hour-long cadence")

	clock.advance(time.Hour)
	reconcileOnceFor(t, r, key)
	assert.Equal(t, 2, upstreamCalls(fk))
}

// A spec edit bypasses the hold: an operator who just changed this source is
// asking for the change to be applied, not to wait out a cadence armed under
// the old spec. This cannot reopen the loop, because the loop is driven by
// STATUS writes and a status write never bumps the generation.
func TestReconcile_ASpecEditBypassesTheHold(t *testing.T) {
	r, key, fk, _ := newPacingFixtures(t, "fakekind-pacing-specedit", time.Hour)

	reconcileOnceFor(t, r, key)
	require.Equal(t, 1, upstreamCalls(fk))
	reconcileOnceFor(t, r, key)
	require.Equal(t, 1, upstreamCalls(fk), "precondition: held while the spec is unchanged")

	var cur spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &cur))
	cur.Spec.Sync.Interval.Duration = 30 * time.Minute
	// Stamped explicitly rather than left to the fake client: a real API
	// server bumps metadata.generation on a spec write, and whether this
	// fake does is a detail of the fake, not the behaviour under test.
	cur.Generation = cur.Generation + 1
	require.NoError(t, r.Client.Update(context.Background(), &cur))

	var edited spiceboxv1alpha1.RelationshipSource
	require.NoError(t, r.Client.Get(context.Background(), key, &edited))
	require.NotEqual(t, edited.Generation, edited.Status.ObservedGeneration,
		"precondition: the edit left status behind, which is what the bypass reads")

	reconcileOnceFor(t, r, key)
	assert.Equal(t, 2, upstreamCalls(fk),
		"an operator edit must take effect now, not at the next interval")
}

// retryAfterFrom answers two questions that used to be folded into one: HOW
// LONG upstream asked for, and WHETHER it said anything at all. Folding them
// discarded a zero — and slack-go builds RateLimitedError{0} from a literal
// `Retry-After: 0`, so zero is reachable, not theoretical.
//
// It was cosmetic while the answer only chose a requeue delay. It stopped being
// cosmetic when passPacer began reading it to decide whether a MID-CYCLE pass
// may call upstream at all: a dropped zero is not "wait no time", it is
// "upstream never said it was throttling", which is the answer that takes the
// mid-cycle exemption.
func TestRetryAfterFrom(t *testing.T) {
	cases := []struct {
		name         string
		errs         []relsync.ScopeError
		wantDuration time.Duration
		wantReported bool
	}{
		{
			name:         "nothing reported",
			errs:         []relsync.ScopeError{scopeErr("C1", "upstream: 500 internal server error")},
			wantDuration: 0,
			wantReported: false,
		},
		{
			name:         "no errors at all",
			errs:         nil,
			wantDuration: 0,
			wantReported: false,
		},
		{
			name: "a ZERO Retry-After is still a report, not an absence",
			errs: []relsync.ScopeError{{Scope: "C1",
				Err: fmt.Errorf("fetch: %w", &fakeRateLimitErr{after: 0})}},
			wantDuration: 0,
			wantReported: true,
		},
		{
			name: "the largest of several wins, and a zero alongside does not hide it",
			errs: []relsync.ScopeError{
				{Scope: "C1", Err: fmt.Errorf("fetch: %w", &fakeRateLimitErr{after: 0})},
				{Scope: "C2", Err: fmt.Errorf("fetch: %w", &fakeRateLimitErr{after: 90 * time.Second})},
				{Scope: "C3", Err: fmt.Errorf("fetch: %w", &fakeRateLimitErr{after: 30 * time.Second})},
			},
			wantDuration: 90 * time.Second,
			wantReported: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, reported := retryAfterFrom(tc.errs)
			assert.Equal(t, tc.wantDuration, d)
			assert.Equal(t, tc.wantReported, reported)
		})
	}
}

// The other half of no longer discarding a zero: (0, true) must not become
// ctrl.Result{RequeueAfter: 0}, which controller-runtime reads as "do not
// requeue" rather than "requeue immediately". With the hold refusing
// watch-driven wake-ups, that would leave the source with no scheduled work at
// all — a fix for one stall creating another.
func TestReconcile_AZeroRetryAfterDoesNotStrandTheSource(t *testing.T) {
	r, key, fk, _ := newPacingFixtures(t, "fakekind-pacing-zeroretry", 20*time.Minute)
	fk.fetchErrs = map[relsync.ScopeID]error{
		"C1": fmt.Errorf("upstream: %w", &fakeRateLimitErr{after: 0}),
	}

	res := reconcileOnceFor(t, r, key)
	assert.Equal(t, 20*time.Minute, res.RequeueAfter,
		"upstream asking for a zero wait means the source's own interval governs, not 'never wake up again'")
}

// failedPassNotBefore covers the one branch that reached upstream and returns
// before the ordinary arming. Unreachable today — relsync.Pass has a single
// return and it is nil — so this is tested at the function rather than through
// Reconcile, which is also the honest way to say it is a latent trap rather
// than a live bug.
func TestFailedPassNotBefore(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("floors at the cadence requeue is about to schedule", func(t *testing.T) {
		// interval > failureRetryInterval, so the failure cadence is the floor,
		// and the gate agrees with the wake-up instead of contradicting it.
		assert.Equal(t, base.Add(failureRetryInterval),
			failedPassNotBefore(base, time.Hour, relsync.PassResult{}))
	})

	t.Run("a shorter interval than the failure cadence wins", func(t *testing.T) {
		assert.Equal(t, base.Add(30*time.Second),
			failedPassNotBefore(base, 30*time.Second, relsync.PassResult{}))
	})

	t.Run("anything res reported can only push it later", func(t *testing.T) {
		// The future hazard this branch exists for: a Pass error return that
		// carries a RetryAfter longer than the failure cadence.
		got := failedPassNotBefore(base, time.Hour, relsync.PassResult{
			ScopeErrors: []relsync.ScopeError{{
				Err: fmt.Errorf("fatal: %w", &fakeRateLimitErr{after: 10 * time.Minute}),
			}},
		})
		assert.Equal(t, base.Add(10*time.Minute), got)
	})

	t.Run("never the zero time, or the branch would arm no hold at all", func(t *testing.T) {
		assert.False(t, failedPassNotBefore(base, time.Hour, relsync.PassResult{}).IsZero())
	})
}

// nextPassNotBefore is the whole policy in one function, so it gets a table of
// its own alongside the Reconcile-level tests above.
func TestNextPassNotBefore(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rateLimited := []relsync.ScopeError{{
		Scope: "C1",
		Err:   fmt.Errorf("fetch: %w", &fakeRateLimitErr{after: 90 * time.Second}),
	}}
	plainFailure := []relsync.ScopeError{scopeErr("C1", "upstream: 500 internal server error")}

	cases := []struct {
		name     string
		interval time.Duration
		res      relsync.PassResult
		want     time.Time
	}{
		{
			name:     "mid-cycle, nothing reported: no hold, or incremental sync stalls",
			interval: 15 * time.Minute,
			res:      relsync.PassResult{},
			want:     time.Time{},
		},
		{
			name:     "mid-cycle with ordinary scope failures: still no hold",
			interval: 15 * time.Minute,
			res:      relsync.PassResult{ScopeErrors: plainFailure},
			want:     time.Time{},
		},
		{
			name:     "completed cycle: hold for the interval",
			interval: 15 * time.Minute,
			res:      relsync.PassResult{CycleComplete: true},
			want:     base.Add(15 * time.Minute),
		},
		{
			name:     "mid-cycle with a Retry-After: hold for the backoff",
			interval: 15 * time.Minute,
			res:      relsync.PassResult{ScopeErrors: rateLimited},
			want:     base.Add(90 * time.Second),
		},
		{
			name:     "completed cycle with a SHORTER Retry-After: the interval wins",
			interval: 15 * time.Minute,
			res:      relsync.PassResult{CycleComplete: true, ScopeErrors: rateLimited},
			want:     base.Add(15 * time.Minute),
		},
		{
			name:     "completed cycle with a LONGER Retry-After: the backoff wins",
			interval: time.Second,
			res:      relsync.PassResult{CycleComplete: true, ScopeErrors: rateLimited},
			want:     base.Add(90 * time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextPassNotBefore(base, tc.interval, tc.res))
		})
	}
}
