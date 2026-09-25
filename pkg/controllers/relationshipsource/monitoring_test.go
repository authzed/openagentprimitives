// pkg/controllers/relationshipsource/monitoring_test.go
package relationshipsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// -----------------------------------------------------------------------
// intervalClock: a clock that advances a full sync interval on every read.
//
// The Reconcile-driven tests below are about what gets REPORTED across
// successive passes — emit on transition, stay quiet while unchanged, report
// the recovery — not about pacing. The controller now refuses to call upstream
// twice inside one interval (passPacer), so against a FIXED clock every second
// reconcile in these tests would be a held no-op and the assertions would be
// measuring the hold instead of the transition logic. That is not hypothetical
// politeness: TestMonitoring_SecondConsecutiveFailureEmitsNothing kept passing
// when the hold silently ate its second pass, which is precisely a test that
// has stopped reaching the code it names.
//
// Advancing per read models what each of these tests describes in its own
// comment anyway: a source polled once per interval, pass after pass.
// -----------------------------------------------------------------------

type intervalClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newIntervalClock() *intervalClock {
	// A fixed, arbitrary base so a failure message reads the same every run.
	return &intervalClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: defaultSyncInterval}
}

func (c *intervalClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.t
	c.t = c.t.Add(c.step)
	return out
}

// -----------------------------------------------------------------------
// publishCapture: the fixture every test in this file uses in place of a
// real NATS publish — same shape as controller_test.go's own inline
// closure in TestReconcile_PublishesKindClaimConflictOnlyOnce, pulled out
// once here since every test below needs it.
// -----------------------------------------------------------------------

type publishCapture struct {
	mu     sync.Mutex
	events []channelevents.MonitoringEvent
}

func (p *publishCapture) publish(_ string, data []byte) error {
	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
	return nil
}

func (p *publishCapture) snapshot() []channelevents.MonitoringEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]channelevents.MonitoringEvent, len(p.events))
	copy(out, p.events)
	return out
}

// -----------------------------------------------------------------------
// classifyScopeError unit tests: guard refusal and CAS failure ALSO get a
// dedicated Reconcile-driven test further down
// (TestMonitoring_GuardRefusalAndCASFailureReachMonitoringThroughReconcile),
// but this is where the classification itself is pinned — every case here
// wraps the REAL exported sentinel (relsource.ErrRefused /
// relsync.ErrRefusedPrune), never a hand-typed string that merely mirrors
// today's wording (fix round 1, IMPORTANT 3): a future reword of either
// package's message text cannot break this test, only removing the %w
// wrap around the sentinel can.
// -----------------------------------------------------------------------

func TestClassifyScopeError_ClassifiesEachIssueByItsRealShape(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want passIssue
	}{
		{
			name: "a relsource refusal wrapping ErrRefused, wrapped again exactly as processScope wraps a write error -> guard refusal",
			err:  fmt.Errorf("write: %w", fmt.Errorf("relsource: fakekindsync cannot write fake_scope#member: owned by other: %w", relsource.ErrRefused)),
			want: issueGuardRefusal,
		},
		{
			name: "a gRPC FailedPrecondition from the underlying WriteRelationships call, %w-wrapped -> CAS failure",
			err:  fmt.Errorf("write: %w", status.Error(codes.FailedPrecondition, "unable to satisfy write precondition `relhash`")),
			want: issueCASFailure,
		},
		{
			name: "relsync's own ErrRefusedPrune sentinel -> refused prune",
			err:  relsync.ErrRefusedPrune,
			want: issueRefusedPrune,
		},
		{
			name: "an ordinary upstream fetch error -> generic scope failure",
			err:  fmt.Errorf("fetch: %w", errors.New("upstream: 500 internal server error")),
			want: issueScopeFailure,
		},
		{
			name: "relsource's claim-table-incomplete WIRING error must NOT be misclassified as a guard refusal (fix round 1 Minor)",
			err:  fmt.Errorf("write: %w", relsource.ErrClaimTableIncomplete),
			want: issueScopeFailure,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyScopeError(tc.err))
		})
	}
}

// -----------------------------------------------------------------------
// Reconcile-driven tests: drive the real Reconcile loop with the same
// fakeKind/fakeSpiceDB/fakeRelWriter/fakeReader fixtures
// controller_test.go already established, injecting a scope failure via
// fakeKind's existing fetchErrs map — no new writer-fault fixture needed.
// -----------------------------------------------------------------------

// newScopeFailureFixtures builds a RelationshipSource whose one scope's
// FetchScope call fails with the SAME error every pass, until the test
// clears it.
//
// The injected error wraps relsource.ErrRefused so classifyScopeError buckets
// it as issueGuardRefusal, which is an EMITTED issue (emittedPassIssues).
// These three tests exercise the transition machinery — emit once, stay quiet,
// emit the recovery — and that machinery needs an issue that actually reaches
// the bus. A plain scope failure no longer does, deliberately, and
// TestMonitoring_PlainScopeFailureIsNotPublishedPerScope is what pins that.
func newScopeFailureFixtures(t *testing.T, kindName string) (*Reconciler, types.NamespacedName, *fakeKind, *publishCapture) {
	t.Helper()
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages:  []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
		// members is left nil deliberately: fakeKind.FetchScope returns
		// before ever consulting it while fetchErrs has an entry for this
		// scope, and once TestMonitoring_RecoveryEmitsRecovered deletes that
		// entry, reading a nil map back is a safe zero-value (no tuples) —
		// this scenario only needs FetchScope to fail, then to succeed with
		// nothing, never to succeed with real membership content.
		fetchErrs: map[relsync.ScopeID]error{
			"C1": fmt.Errorf("write: %w: relation claimed by another source", relsource.ErrRefused),
		},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}
	return r, types.NamespacedName{Namespace: "ns", Name: "src"}, fk, pub
}

// Emit on TRANSITION, not per pass. A source failing every fifteen minutes
// must not alert every fifteen minutes; that is how people learn to filter
// the channel.
func TestMonitoring_SecondConsecutiveFailureEmitsNothing(t *testing.T) {
	r, key, _, pub := newScopeFailureFixtures(t, "fakekind-scopefail-repeat")

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	first := pub.snapshot()
	require.Len(t, first, 1, "the first failing reconcile must publish exactly one event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, first[0].Transition)

	// Same scope, same error, unchanged — nothing new to report.
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	second := pub.snapshot()
	assert.Len(t, second, 1,
		"a second consecutive reconcile failing the SAME way must publish zero further events")
}

// Recovery IS expressible here, unlike drift: the CR carries conditions, so
// failing→healthy is observable without inventing bookkeeping.
func TestMonitoring_RecoveryEmitsRecovered(t *testing.T) {
	r, key, fk, pub := newScopeFailureFixtures(t, "fakekind-scopefail-recover")

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Len(t, pub.snapshot(), 1, "precondition: the first pass reports the failure")

	// Fix the upstream: the next pass's FetchScope succeeds.
	delete(fk.fetchErrs, "C1")

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 2, "the recovery must publish exactly one further event")
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, events[1].Transition,
		"the second event must be the recovery, not another failure")
}

// An ordinary per-scope fetch failure must NOT be published per scope. The
// failure that motivated the PartialFailure condition was 156 repositories
// refusing from one revoked permission; per-scope events would post 156 chat
// messages saying the same thing, and the condition posts one.
//
// Both halves are asserted, because either alone would pass a broken
// implementation: publishing nothing is only correct if the fact still lands
// somewhere, and the somewhere is the CR's own condition (which
// pkg/controllers/monitoring then reports once for the whole source).
func TestMonitoring_PlainScopeFailureIsNotPublishedPerScope(t *testing.T) {
	const kindName = "fakekind-plain-scopefail"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "fake_scope"},
				{ID: "C2", ResourceType: "fake_scope"},
			},
			Complete: true,
		}},
		// Plain upstream errors: classifyScopeError buckets both as
		// issueScopeFailure, which emittedPassIssues deliberately omits.
		fetchErrs: map[relsync.ScopeID]error{
			"C1": errors.New("upstream: 403 forbidden"),
			"C2": errors.New("upstream: 403 forbidden"),
		},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	assert.Empty(t, pub.snapshot(),
		"two scopes failing one way must not become two channel events; the condition reports the source once")

	var cur v1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &cur))
	partial := conditions.Find(cur.Status.Conditions, v1.RelationshipSourceConditionPartialFailure)
	require.NotNil(t, partial, "not publishing per scope is only correct because the condition carries the fact")
	assert.Equal(t, metav1.ConditionTrue, partial.Status)
	assert.Equal(t, int32(2), cur.Status.Sync.LastPass.ScopeErrors)
}

// Unlike drift, a sync failure is attributable: Source names the CR, so an
// operator goes from the alert straight to kubectl describe.
func TestMonitoring_EventNamesTheRelationshipSource(t *testing.T) {
	const kindName = "fakekind-credfail-name"
	src := newSrc("ns", "credfail-src", kindName)
	// spec.kind must be a REGISTERED relsync.Kind, or Reconcile parks on
	// KindUnregistered before ever reaching resolveCreds. spec.auth then
	// names an AgentIdentity that does not exist in this fixture, so
	// resolveCreds is what actually fails.
	relsync.Register(&fakeKind{name: kindName, source: relsource.Source{Name: kindName + "-sync"}})
	c := newClient(t, src)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "credfail-src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 1, "precondition: credential resolution failure must publish one event")
	assert.Equal(t, "RelationshipSource", events[0].Source.Kind)
	assert.Equal(t, "ns", events[0].Source.Namespace)
	assert.Equal(t, "credfail-src", events[0].Source.Name)
}

// Every field must satisfy MonitoringEvent.Validate, or PublishMonitoring
// drops the event while the code logs "publish failed" — monitoring sees
// nothing and nothing looks broken.
//
// This asserts on the CAPTURED event actually reaching the publish
// transport, not on a hand-built MonitoringEvent calling Validate() on
// itself — the latter would stay green even if the production code that
// builds ev forgot to set a required field, since PublishMonitoring drops
// an invalid event silently (a logged line, not a returned error the
// caller here would ever see). Asserting `len(events) == 1` is what turns
// a validation regression in monitoring.go into a failing test rather than
// a quiet log line.
func TestMonitoring_EventPassesValidation(t *testing.T) {
	const kindName = "fakekind-credfail-validate"
	src := newSrc("ns", "validate-src", kindName)
	relsync.Register(&fakeKind{name: kindName, source: relsource.Source{Name: kindName + "-sync"}})
	c := newClient(t, src)
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "validate-src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 1, "the event must actually reach the publish transport, proving PublishMonitoring's internal Validate() accepted it")
	assert.NoError(t, events[0].Validate(), "the captured event must independently pass MonitoringEvent.Validate")
}

// -----------------------------------------------------------------------
// Fix round 1 tests.
// -----------------------------------------------------------------------

// IMPORTANT 1: a NEWLY failing scope inside an already-active category must
// be its own signal, not silently absorbed into "something is still
// failing". Probe (as reported): pass 1, scope C1 fails → Failed(C1). Pass
// 2, C1 RECOVERS and a DIFFERENT scope C2 fails with a DIFFERENT error, in
// the SAME pass. A tracker that only asks "is anything failing" (true on
// both passes) emits nothing here — no Recovered for C1, no Failed for
// C2 — leaving an operator staring at pass 1's stale alert about a scope
// that is now fine while a different one silently broke.
func TestMonitoring_ANewFailureInAnActiveCategoryEmitsRecoveredAndFailed(t *testing.T) {
	const kindName = "fakekind-scopefail-interleave"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "fake_scope"},
				{ID: "C2", ResourceType: "fake_scope"},
			},
			Complete: true,
		}},
		fetchErrs: map[relsync.ScopeID]error{
			"C1": fmt.Errorf("write C1: %w: relation claimed by another source", relsource.ErrRefused),
		},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	first := pub.snapshot()
	require.Len(t, first, 1, "precondition: pass 1 reports exactly C1's failure and nothing else")
	require.Equal(t, channelevents.MonitoringTransitionFailed, first[0].Transition)
	require.Contains(t, first[0].Summary, "C1")

	// C1 recovers; C2 starts failing, with a DIFFERENT error, in the SAME
	// pass.
	delete(fk.fetchErrs, "C1")
	fk.fetchErrs["C2"] = fmt.Errorf("write C2: %w: a different relation, also claimed", relsource.ErrRefused)

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	second := pub.snapshot()
	require.Len(t, second, 3, "pass 2 must publish exactly two MORE events: a Recovered for C1 and a Failed for C2")

	var gotRecovered, gotFailed bool
	for _, ev := range second[1:] {
		switch ev.Transition {
		case channelevents.MonitoringTransitionRecovered:
			gotRecovered = true
			assert.Contains(t, ev.Summary, "C1", "the recovery must name the scope that cleared, not the one still failing")
		case channelevents.MonitoringTransitionFailed:
			gotFailed = true
			assert.Contains(t, ev.Summary, "C2", "the failure must name the NEWLY failing scope, not the one that just recovered")
		default:
			t.Fatalf("unexpected transition %q", ev.Transition)
		}
	}
	assert.True(t, gotRecovered, "C1 recovering must be reported")
	assert.True(t, gotFailed, "C2 newly failing must be reported")
}

// IMPORTANT 2: the join-miss count-change rule (the brief's second headline
// rule) was implemented in observeJoinMisses but had no way to be reached
// through a real Reconcile — fakeKind couldn't set JoinMisses at all — so a
// regression collapsing the rule to "emit whenever nonzero" (the exact
// alert-storm shape the brief warns against) would have passed the whole
// suite. Drives it end to end via fakeKind.joinMisses.
func TestMonitoring_JoinMissesEmitOnlyOnCountChange(t *testing.T) {
	const kindName = "fakekind-joinmisses"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}},
			Complete: true,
		}},
		joinMisses: map[relsync.ScopeID]int{"C1": 5},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	first := pub.snapshot()
	require.Len(t, first, 1, "precondition: the first pass with a nonzero JoinMisses count must publish exactly one event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, first[0].Transition)
	assert.Contains(t, first[0].Summary, "5")

	// Same count held steady: no further event — this is the alert-storm
	// shape the whole rule exists to prevent.
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Len(t, pub.snapshot(), 1, "an UNCHANGED JoinMisses count must not re-alert")

	// Count CHANGES (still nonzero): new information, must publish again —
	// reverting to "emit only when it clears to zero" would fail here.
	fk.joinMisses["C1"] = 8
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	second := pub.snapshot()
	require.Len(t, second, 2, "a CHANGED (still nonzero) count must publish a fresh Failed event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, second[1].Transition)
	assert.Contains(t, second[1].Summary, "8")

	// Count clears to zero: Recovered — reverting to "emit whenever
	// nonzero" would never reach this branch at all.
	fk.joinMisses["C1"] = 0
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	third := pub.snapshot()
	require.Len(t, third, 3)
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, third[2].Transition)
}

// IMPORTANT 4: guard refusal and CAS failure previously only reached
// classifyScopeError in isolation
// (TestClassifyScopeError_ClassifiesEachIssueByItsRealShape) — this drives
// BOTH through a real Reconcile via fakeRelWriter.writeErr, so a future
// change to processScope's "write: %w" wrap, or to what
// relWriter.WriteRelationships returns, actually breaks a test here.
func TestMonitoring_GuardRefusalAndCASFailureReachMonitoringThroughReconcile(t *testing.T) {
	cases := []struct {
		name     string
		writeErr error
	}{
		{
			name:     "a relsource-style guard refusal wrapping relsource.ErrRefused",
			writeErr: fmt.Errorf("relsource: fakekindwriteerrsync cannot write fake_scope#member: owned by other: %w", relsource.ErrRefused),
		},
		{
			name:     "a gRPC FailedPrecondition from the write RPC itself",
			writeErr: status.Error(codes.FailedPrecondition, "unable to satisfy write precondition `relhash`"),
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kindName := fmt.Sprintf("fakekind-writeerr-%d", i)
			srcName := fmt.Sprintf("src-%d", i)
			src := newSrc("ns", srcName, kindName)
			id, sec := authFixtures("ns")
			c := newClient(t, src, id, sec)

			fk := &fakeKind{
				name:    kindName,
				source:  relsource.Source{Name: kindName + "-sync"},
				pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
				members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
			}
			relsync.Register(fk)

			writer := &fakeRelWriter{writeErr: tc.writeErr}
			sdb := &fakeSpiceDB{writer: writer, reader: &fakeReader{}}
			pub := &publishCapture{}
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

			key := types.NamespacedName{Namespace: "ns", Name: srcName}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)

			events := pub.snapshot()
			require.Len(t, events, 1, "the write failure must reach monitoring as exactly one event")
			assert.Equal(t, channelevents.MonitoringLevelError, events[0].Level,
				"both a guard refusal and a CAS failure report at Error level")
			assert.Equal(t, channelevents.MonitoringTransitionFailed, events[0].Transition)
		})
	}
}

// Minor: KindUnregistered has no matching Failed report — its Reconcile
// branch requeues directly with no publish call — so publishRecovered must
// not manufacture a Recovered for it either: an operator who never
// received the original alert must not see its "resolution".
func TestMonitoring_KindUnregisteredRecoveryIsNotReported(t *testing.T) {
	const kindName = "fakekind-unregistered-then-registered"
	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish, Now: newIntervalClock().now}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}

	// First reconcile: spec.kind names a relsync.Kind that is not yet
	// registered anywhere in this test binary.
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Empty(t, pub.snapshot(), "precondition: KindUnregistered never publishes a Failed report")

	require.NoError(t, c.Get(context.Background(), key, src))
	ready := conditions.Find(src.Status.Conditions, v1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status, "precondition: Ready is False after the first reconcile")

	// Register the kind; the next pass succeeds cleanly (one scope, no
	// scope errors, no join misses) and Ready flips True.
	relsync.Register(&fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages:  []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
	})

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	require.NoError(t, c.Get(context.Background(), key, src))
	ready = conditions.Find(src.Status.Conditions, v1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status, "precondition: Ready actually flipped True this pass")

	assert.Empty(t, pub.snapshot(),
		"Ready recovering from KindUnregistered must not be reported: monitoring was never told it failed in the first place")
}
