// pkg/controllers/guardian/bootstrap_drift_test.go
//
// Unit tests for SpiceDBBootstrap read-back drift detection
// (bootstrap_drift.go). fakeReader/fakeReadStream below are the read-side
// counterpart to fakeWriter in spicedbbootstrap_controller_test.go (same
// package, same file layout convention): record every call, return canned
// data, never talk to a real SpiceDB.
package guardian_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
)

// fakeReadStream is the minimal v1.PermissionsService_ReadRelationshipsClient
// (a type alias for grpc.ServerStreamingClient[ReadRelationshipsResponse]):
// serves canned responses off a slice, then io.EOF. grpc.ClientStream is
// embedded as a nil interface purely to satisfy the rest of that
// interface's method set (Header/Trailer/CloseSend/Context/SendMsg/
// RecvMsg) — production code (readManagedRelation in bootstrap_drift.go)
// only ever calls Recv, so those are never invoked and never need a real
// implementation.
type fakeReadStream struct {
	grpc.ClientStream
	resps []*v1.ReadRelationshipsResponse
	idx   int
}

func (s *fakeReadStream) Recv() (*v1.ReadRelationshipsResponse, error) {
	if s.idx >= len(s.resps) {
		return nil, io.EOF
	}
	r := s.resps[s.idx]
	s.idx++
	return r, nil
}

// fakeReader implements guardian.Reader. Canned relationships are seeded
// per (resourceType, resourceID, relation) triple via seed; readErr, when
// set, is returned by every call instead of canned data — used to prove a
// read-back failure is non-fatal. Every call's filter is recorded so tests
// can assert on the ABSENCE of a read, not just the absence of a finding.
type fakeReader struct {
	mu       sync.Mutex
	byTriple map[string][]*v1.ReadRelationshipsResponse
	reads    []*v1.RelationshipFilter
	readErr  error
}

func newFakeReader() *fakeReader {
	return &fakeReader{byTriple: map[string][]*v1.ReadRelationshipsResponse{}}
}

func triKey(resourceType, resourceID, relation string) string {
	return resourceType + ":" + resourceID + "#" + relation
}

// seed registers the tuples SpiceDB "holds" for one (resourceType,
// resourceID, relation) triple. A triple never seeded reads back empty,
// not erroring — that's the ordinary "nothing there" case.
func (f *fakeReader) seed(resourceType, resourceID, relation string, tuples ...spicedb.Tuple) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var resps []*v1.ReadRelationshipsResponse
	for _, t := range tuples {
		subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: t.SubjectType, ObjectId: t.SubjectID}}
		if t.SubjectRelation != "" {
			subj.OptionalRelation = t.SubjectRelation
		}
		resps = append(resps, &v1.ReadRelationshipsResponse{
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: t.ResourceType, ObjectId: t.ResourceID},
				Relation: t.Relation,
				Subject:  subj,
			},
		})
	}
	f.byTriple[triKey(resourceType, resourceID, relation)] = resps
}

func (f *fakeReader) ReadRelationships(_ context.Context, in *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, in.GetRelationshipFilter())
	if f.readErr != nil {
		return nil, f.readErr
	}
	filter := in.GetRelationshipFilter()
	key := triKey(filter.GetResourceType(), filter.GetOptionalResourceId(), filter.GetOptionalRelation())
	return &fakeReadStream{resps: f.byTriple[key]}, nil
}

func (f *fakeReader) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func (f *fakeReader) readFilters() []*v1.RelationshipFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*v1.RelationshipFilter, len(f.reads))
	copy(out, f.reads)
	return out
}

// tupMember builds a group:engineering#member@user:<id> tuple — the same
// shape relGroupMember (spicedbbootstrap_controller_test.go) resolves a
// SpiceDBBootstrap CR's relationship into, but built directly as the
// spicedb.Tuple drift detection deals in rather than through a CR.
func tupMember(subjectID string) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: "group",
		ResourceID:   "engineering",
		Relation:     "member",
		SubjectType:  "user",
		SubjectID:    subjectID,
	}
}

func desiredWith(tuples ...spicedb.Tuple) *guardian.DesiredMap {
	d := guardian.NewDesiredMap()
	for _, t := range tuples {
		d.Add(t, guardian.Owner{CR: "default/cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})
	}
	return d
}

// Desired, absent in SpiceDB. The surface believes it wrote this.
func TestDrift_ReportsADesiredTupleThatIsAbsent(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader() // group:engineering#member seeded with nothing
	r.Reader = fr

	tup := tupMember("alice-canon")
	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(tup))

	require.Len(t, report.Missing, 1, "the desired tuple was never found on read-back")
	assert.Equal(t, tup, report.Missing[0])
	assert.Empty(t, report.Unexpected)
}

// Present on a (resource, relation) the surface manages, desired by
// nobody.
func TestDrift_ReportsAnUnexpectedTupleOnAManagedRelation(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	desiredTup := tupMember("alice-canon")
	rogueTup := tupMember("mallory-canon") // in SpiceDB, claimed by no CR
	fr.seed("group", "engineering", "member", desiredTup, rogueTup)
	r.Reader = fr

	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(desiredTup))

	assert.Empty(t, report.Missing, "the desired tuple IS present")
	require.Len(t, report.Unexpected, 1)
	assert.Equal(t, rogueTup, report.Unexpected[0])
}

// Bounded by what is desired: a relation the surface does not manage is
// never read. Asserted by the ABSENCE of a read, not by the absence of a
// report — a test checking only the report would pass even if the
// implementation read the whole graph and filtered client-side.
func TestDrift_DoesNotReadARelationTheSurfaceDoesNotManage(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	// SpiceDB holds data on a SECOND relation of the same resource that
	// nothing in `desired` claims. If drift detection read broadly instead
	// of exactly the managed triples, this would show up as a read.
	fr.seed("group", "engineering", "lead", spicedb.Tuple{
		ResourceType: "group", ResourceID: "engineering", Relation: "lead",
		SubjectType: "user", SubjectID: "someone-canon",
	})
	r.Reader = fr

	guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(tupMember("alice-canon")))

	filters := fr.readFilters()
	require.Len(t, filters, 1, "only the one managed relation is ever read")
	assert.Equal(t, "group", filters[0].GetResourceType())
	assert.Equal(t, "engineering", filters[0].GetOptionalResourceId())
	assert.Equal(t, "member", filters[0].GetOptionalRelation(),
		"group:engineering#lead was never queried even though SpiceDB holds data there")
}

// The whole point: it observes, it does not act. Load-bearing — a test
// that only checked the report would pass if a delete (or a re-write) were
// added later.
func TestDrift_IssuesNoWrites(t *testing.T) {
	r, _, _, w := newBootReconciler(t)
	fr := newFakeReader()
	missingTup := tupMember("alice-canon") // desired, absent
	rogueTup := tupMember("mallory-canon") // present, undesired
	fr.seed("group", "engineering", "member", rogueTup)
	r.Reader = fr

	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(missingTup))
	require.Len(t, report.Missing, 1, "precondition: this pass found drift in the missing direction")
	require.Len(t, report.Unexpected, 1, "precondition: this pass found drift in the unexpected direction")

	assert.Equal(t, 0, w.WriteCount(),
		"drift detection must never re-write a missing tuple; that's the existing touch path's job")
	assert.Equal(t, 0, w.DeleteCount(),
		"drift detection must never delete an unexpected tuple; deleting on suspicion is the unsafe behaviour this design refuses")
}

// No bootstrap resources means no reads at all.
func TestDrift_ReadsNothingWhenThereAreNoBootstraps(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	r.Reader = fr

	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), guardian.NewDesiredMap())

	assert.Empty(t, report.Missing)
	assert.Empty(t, report.Unexpected)
	assert.Zero(t, fr.readCount(), "an empty desired set claims no relation, so nothing is ever read")
}

// A nil Reader means drift detection does not run, checked once at a
// sensible level rather than silently.
//
// r.Reader is deliberately left at its zero value here — a TRUE nil
// interface. Assigning a nil-valued *fakeReader instead would produce a
// NON-nil interface wrapping a nil pointer: `r.Reader != nil` cannot see
// through that, which is exactly the typed-nil trap AGENTS.md documents
// against this file's Writer field (originally hit in this exact
// package). Leaving the field untouched is the only correct way to
// construct "no reader configured" in a test.
func TestDrift_NilReaderDoesNotRun(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)

	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(tupMember("alice-canon")))

	assert.Empty(t, report.Missing)
	assert.Empty(t, report.Unexpected)
}

// A read-back failure reports and continues: it must never hold a
// bootstrap's convergence, and must never change Reconcile's outcome — no
// error returned, no requeue, no condition flipped. Proven by running the
// exact same scenario twice, once with drift detection off (nil Reader)
// and once with it on and every read failing, and requiring an identical
// Reconcile result.
func TestDrift_ReadBackFailureDoesNotChangeReconcileOutcome(t *testing.T) {
	newBoot := func() *spiceboxv1alpha1.SpiceDBBootstrap {
		return mkBoot("seeds", relGroupMember("alice-canon"))
	}
	tick := ctrl.Request{NamespacedName: types.NamespacedName{Name: "__bootstrap_tick__"}}

	rOff, _, _, _ := newBootReconciler(t, newBoot())
	resOff, errOff := rOff.Reconcile(context.Background(), tick)
	require.NoError(t, errOff)

	rOn, _, _, _ := newBootReconciler(t, newBoot())
	fr := newFakeReader()
	fr.readErr = errors.New("spicedb unavailable")
	rOn.Reader = fr
	resOn, errOn := rOn.Reconcile(context.Background(), tick)
	require.NoError(t, errOn)

	assert.Equal(t, resOff, resOn, "a read-back failure must never change Reconcile's outcome")
	assert.Positive(t, fr.readCount(), "the read really was attempted, and really did fail")
}

// capturePublish returns a channelevents.PublishFunc that decodes every
// published MonitoringEvent into the slice it also returns, then returns err
// to the caller — mirrors useridentity's capturePublish
// (pkg/controllers/useridentity/attested_edge_test.go) so a test can assert
// both that a publish was attempted and, separately, that its failure
// changed nothing about the reconcile.
func capturePublish(t *testing.T, err error) (channelevents.PublishFunc, *[]channelevents.MonitoringEvent) {
	t.Helper()
	var got []channelevents.MonitoringEvent
	return func(_ string, data []byte) error {
		var ev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(data, &ev), "monitoring event must decode")
		got = append(got, ev)
		return err
	}, &got
}

// logCapture records every JSON log line funcr emits as a decoded
// map[string]any, not a raw string — so a test can assert on individual
// fields (msg, missing, missingSample[0].resourceType, …) the same way it
// would assert on decoded JSON from the wire, rather than pattern-matching
// a formatted line.
type logCapture struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (c *logCapture) write(obj string) {
	var m map[string]any
	if err := json.Unmarshal([]byte(obj), &m); err != nil {
		// Record the failure itself rather than dropping the line — a test
		// reading zero lines back must not be mistaken for "nothing logged".
		m = map[string]any{"_raw": obj, "_decodeErr": err.Error()}
	}
	c.mu.Lock()
	c.lines = append(c.lines, m)
	c.mu.Unlock()
}

func (c *logCapture) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.lines...)
}

// findByMsgContains returns the first captured line whose "msg" field
// contains substr, or nil.
func (c *logCapture) findByMsgContains(substr string) map[string]any {
	for _, l := range c.snapshot() {
		if msg, _ := l["msg"].(string); strings.Contains(msg, substr) {
			return l
		}
	}
	return nil
}

// contextWithLogCapture wires a JSON-capturing logr.Logger into ctx via
// controller-runtime's log.IntoContext, the same route detectBootstrapDrift
// reads with log.FromContext(ctx).WithName(...) — so the capture sees
// exactly what a real controller run would emit, WithName included.
func contextWithLogCapture() (context.Context, *logCapture) {
	lc := &logCapture{}
	logger := funcr.NewJSON(lc.write, funcr.Options{})
	return log.IntoContext(context.Background(), logger), lc
}

// decomposedSample asserts that field v (expected to be a []any of
// map[string]any, the shape funcr.NewJSON renders a []tupleSample as)
// carries wantLen entries and that entry 0's resourceType/resourceID/
// relation/subjectType/subjectID match the given tuple — proving the
// sample arrived as individually-keyed fields, not Tuple.Key()'s packed
// "type:id#relation@subjType:subjID#subjRelation" string.
func decomposedSample(t *testing.T, v any, wantLen int, want spicedb.Tuple) {
	t.Helper()
	sample, ok := v.([]any)
	require.True(t, ok, "sample must be a decomposed list of objects, not a packed string: got %T", v)
	require.Len(t, sample, wantLen)
	entry, ok := sample[0].(map[string]any)
	require.True(t, ok, "each sample entry must decode as an object with its own keys")
	assert.Equal(t, want.ResourceType, entry["resourceType"])
	assert.Equal(t, want.ResourceID, entry["resourceID"])
	assert.Equal(t, want.Relation, entry["relation"])
	assert.Equal(t, want.SubjectType, entry["subjectType"])
	assert.Equal(t, want.SubjectID, entry["subjectID"])
}

// The brief's own test: a non-empty drift report becomes exactly one
// MonitoringEvent naming both directions' counts, with the sample bounded
// (driftSampleSize == 5) even though each direction here carries 7 tuples —
// large enough that an unbounded sample would make the event unreadable,
// and large enough to prove the bound rather than merely not exceed it by
// accident.
func TestDrift_PublishesAMonitoringEventNamingCountsAndASample(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()

	keep := tupMember("keep-canon") // desired AND present: neither missing nor unexpected
	seeded := []spicedb.Tuple{keep}
	desired := []spicedb.Tuple{keep}
	for i := 0; i < 7; i++ {
		desired = append(desired, tupMember(fmt.Sprintf("miss-%d-canon", i))) // desired, never seeded: missing
		seeded = append(seeded, tupMember(fmt.Sprintf("rogue-%d-canon", i)))  // seeded, never desired: unexpected
	}
	fr.seed("group", "engineering", "member", seeded...)
	r.Reader = fr

	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	report := guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(desired...))
	require.Len(t, report.Missing, 7, "precondition: 7 missing tuples, more than the sample bound")
	require.Len(t, report.Unexpected, 7, "precondition: 7 unexpected tuples, more than the sample bound")

	require.Len(t, *published, 1, "one report, one event — not one event per tuple")
	ev := (*published)[0]
	assert.Equal(t, "reconcile", ev.Category)
	assert.Equal(t, channelevents.MonitoringLevelWarning, ev.Level)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
	assert.NotEmpty(t, ev.Condition, "Validate requires a non-empty Condition")
	assert.False(t, ev.Timestamp.IsZero())

	// Counts: both directions' true totals, not just the sample size.
	assert.Contains(t, ev.Summary, "7 desired tuple(s) missing")
	assert.Contains(t, ev.Summary, "7 tuple(s) present on a surface-managed relation")

	// Bounded sample: sorted order puts miss-0..miss-4 / rogue-0..rogue-4 in
	// the first 5 and miss-5/miss-6, rogue-5/rogue-6 past the bound.
	assert.Contains(t, ev.Summary, "miss-0-canon", "the sample is not simply absent")
	assert.NotContains(t, ev.Summary, "miss-5-canon", "the sample is bounded at 5, not all 7")
	assert.NotContains(t, ev.Summary, "miss-6-canon")
	assert.Contains(t, ev.Summary, "rogue-0-canon")
	assert.NotContains(t, ev.Summary, "rogue-5-canon")
	assert.NotContains(t, ev.Summary, "rogue-6-canon")
}

// TestDrift_TimestampUsesTheInjectedClock is the seam SetClockForTest exists
// for: r.clock is unexported and never assigned in production (mirrors
// pkg/controllers/useridentity's own clock field), so without a test setter
// `r.clock != nil` is permanently false and the event's Timestamp is
// untestable beyond "not zero" (see TestDrift_PublishesAMonitoringEventNamingCountsAndASample).
// Pinning an exact, non-time.Now value proves the field is actually
// consulted, not just declared.
func TestDrift_TimestampUsesTheInjectedClock(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	r.Reader = fr // nothing seeded: tup reads back absent -> missing

	want := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	guardian.SetClockForTest(r, func() time.Time { return want })

	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(tupMember("alice-canon")))

	require.Len(t, *published, 1)
	assert.True(t, want.Equal((*published)[0].Timestamp),
		"Timestamp must come from the injected clock, not time.Now: got %s, want %s", (*published)[0].Timestamp, want)
}

// TestDrift_MonitoringEventSourceIsNotAReportOnASpecificBootstrap pins the
// resolution the corrections to this task call out explicitly: Validate
// requires Source.Kind and Source.Name to be non-empty, but drift is not
// attributable to any one CR (TestDrift_StampsNoConditionOnAnyBootstrap,
// below), so neither field may borrow a real bootstrap's name — here,
// "default/cr1", the owner CR desiredWith stamps on every tuple.
func TestDrift_MonitoringEventSourceIsNotAReportOnASpecificBootstrap(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	tup := tupMember("alice-canon")
	r.Reader = fr // nothing seeded: tup reads back absent -> missing

	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	guardian.DetectBootstrapDriftForTest(r, context.Background(), desiredWith(tup))

	require.Len(t, *published, 1)
	ev := (*published)[0]
	require.NotEmpty(t, ev.Source.Kind, "Validate requires a non-empty Source.Kind")
	require.NotEmpty(t, ev.Source.Name, "Validate requires a non-empty Source.Name")
	assert.NotEqual(t, "SpiceDBBootstrap", ev.Source.Kind,
		"must not claim the kind of the CR surface it cannot attribute drift to")
	assert.NotContains(t, ev.Source.Name, "cr1",
		"must not borrow the owning CR's own name (desiredWith's Owner.CR)")
}

// The bus may be unconfigured; the observation must still reach a log, with
// the sample decomposed the same way the per-relation read-failure log a
// few lines above decomposes resourceType/resourceID/relation — not packed
// into Tuple.Key().
func TestDrift_LogsWhenThePublisherIsNil(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	missingTup := tupMember("alice-canon")
	rogueTup := tupMember("mallory-canon")
	fr.seed("group", "engineering", "member", rogueTup)
	r.Reader = fr
	// r.MonitoringPublish deliberately left nil.

	ctx, lc := contextWithLogCapture()
	report := guardian.DetectBootstrapDriftForTest(r, ctx, desiredWith(missingTup))
	require.Len(t, report.Missing, 1, "precondition")
	require.Len(t, report.Unexpected, 1, "precondition")

	found := lc.findByMsgContains("not reported to monitoring")
	require.NotNil(t, found, "the observation must still reach a log when the publisher is nil")
	assert.EqualValues(t, 1, found["missing"])
	assert.EqualValues(t, 1, found["unexpected"])
	decomposedSample(t, found["missingSample"], 1, missingTup)
	decomposedSample(t, found["unexpectedSample"], 1, rogueTup)
}

// A publish failure never affects the reconcile: logged, not fatal, and the
// caller's report is exactly what it would have been had publishing
// succeeded.
//
// missingTup is passed as BOTH desired and alreadyKnown: this represents a
// tuple the surface has ALREADY converged on a prior pass (unlike a
// brand-new claim, which the first-reconcile exemption — see
// detectBootstrapDrift and TestDrift_FirstReconcileOfNewBootstrapPublishesNoEvent
// below — would exclude from Missing) and that a read-back now finds absent:
// genuine drift, which is what this test needs in order to exercise "a
// publish failure is not fatal" at all. An unseeded reader with no
// alreadyKnown would, after that fix, report nothing and never attempt a
// publish — exactly the false positive this test used to accept as normal.
func TestDrift_PublishFailureIsNotFatal(t *testing.T) {
	r, _, _, _ := newBootReconciler(t)
	fr := newFakeReader()
	missingTup := tupMember("alice-canon")
	r.Reader = fr // nothing seeded: missingTup reads back absent

	publish, published := capturePublish(t, errors.New("nats down"))
	r.MonitoringPublish = publish

	ctx, lc := contextWithLogCapture()
	report := guardian.DetectBootstrapDriftWithKnownForTest(r, ctx, desiredWith(missingTup), desiredWith(missingTup))

	require.Len(t, report.Missing, 1, "the report itself is unaffected by the publish failure")
	require.Len(t, *published, 1, "the publish was attempted")
	found := lc.findByMsgContains("publish bootstrap drift monitoring event failed")
	require.NotNil(t, found, "a publish failure is logged, never silently dropped")
	assert.Equal(t, "nats down", found["err"])

	// Same contract at the Reconcile level: a publish failure changes
	// nothing about the reconcile's outcome — but the drift it reacts to
	// must be GENUINE, not the first-reconcile exemption (see
	// detectBootstrapDrift's alreadyKnown doc comment and
	// TestDrift_FirstReconcileOfNewBootstrapPublishesNoEvent, which pins
	// that exemption directly). A single reconcile of a brand-new bootstrap
	// is exactly that exempted case: alreadyKnown (r.lastDesired) starts
	// empty, so the tuple is "freshly claimed this pass" and detected as no
	// drift at all — the failing publisher is never even called, and this
	// half asserted nothing beyond "two reconciles that both published
	// nothing return the same result". rOn instead reconciles TWICE: pass 1
	// converges the tuple for real (fakeWriter never errors), landing it in
	// r.lastDesired; only THEN does pass 2's Reader — reporting the
	// now-converged tuple absent — count as drift rather than an ordinary
	// first-converge, so failPublish is actually exercised.
	tick := ctrl.Request{NamespacedName: types.NamespacedName{Name: "__bootstrap_tick__"}}
	rOff, _, _, _ := newBootReconciler(t, mkBoot("seeds", relGroupMember("alice-canon")))
	resOff, errOff := rOff.Reconcile(context.Background(), tick)
	require.NoError(t, errOff)

	rOn, _, _, _ := newBootReconciler(t, mkBoot("seeds", relGroupMember("alice-canon")))
	_, errPass1 := rOn.Reconcile(context.Background(), tick) // pass 1: converges the tuple, no Reader yet
	require.NoError(t, errPass1)

	frOn := newFakeReader() // nothing seeded: the now-converged tuple reads back absent
	rOn.Reader = frOn
	failPublish, publishedOn := capturePublish(t, errors.New("nats down"))
	rOn.MonitoringPublish = failPublish
	resOn, errOn := rOn.Reconcile(context.Background(), tick) // pass 2: genuine drift, failing publisher invoked
	require.NoError(t, errOn)

	require.Len(t, *publishedOn, 1, "drift was genuinely detected on pass 2, and the failing publisher was actually invoked")
	assert.Equal(t, resOff, resOn, "a publish failure must never change Reconcile's outcome")
}

// TestDrift_FirstReconcileOfNewBootstrapPublishesNoEvent is the pin for the
// "Important" fix: the exact first-pass state TestDrift_PublishFailureIsNotFatal
// used to build (a brand-new SpiceDBBootstrap, nothing seeded in SpiceDB
// yet) must publish NO drift event at all. Every relationship here is
// freshly claimed — this process has never converged any of them — so
// reading them back absent is the expected state of a healthy, converging
// surface, not drift; the ordinary sync path (ComputeDiff -> applyTouches)
// is about to TOUCH every one of them later in this SAME reconcile.
//
// Runs through full Reconcile, not DetectBootstrapDriftForTest: "first
// reconcile" is a property of the whole controller flow — r.lastDesired
// only becomes the correct, non-nil, EMPTY convergence history via
// seedPriorClaimsForDeleting, which only Reconcile calls.
func TestDrift_FirstReconcileOfNewBootstrapPublishesNoEvent(t *testing.T) {
	boot := mkBoot("seeds", relGroupMember("alice-canon"), relGroupMember("bob-canon"))
	r, _, _, _ := newBootReconciler(t, boot)
	fr := newFakeReader() // nothing seeded: every relationship reads back absent
	r.Reader = fr
	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	reconcileBootstrapTick(t, r)

	assert.Empty(t, *published, "a first reconcile of a new bootstrap must not report drift")
	assert.Positive(t, fr.readCount(), "the read-back really happened; this isn't passing because the Reader went unused")
}

// Not a per-resource condition: drift is not attributable to any one CR
// (an unexpected tuple could have come from anywhere; a missing one may
// have been claimed by several), so no SpiceDBBootstrap's status is
// touched by a drift pass — in either direction, and regardless of whether
// a publisher is configured.
func TestDrift_StampsNoConditionOnAnyBootstrap(t *testing.T) {
	boot := mkBoot("seeds", relGroupMember("alice-canon"))
	r, c, _, _ := newBootReconciler(t, boot)
	fr := newFakeReader()
	// The desired tuple (from boot's own relationship) is seeded so the
	// ordinary sync path succeeds without drift; a SECOND, undesired tuple
	// on the same managed relation is also seeded so read-back reports
	// unexpected drift this pass, without touching `boot`'s own relationship.
	fr.seed("group", "engineering", "member",
		relTuple(relGroupMember("alice-canon")),
		tupMember("mallory-canon"),
	)
	r.Reader = fr
	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	tick := ctrl.Request{NamespacedName: types.NamespacedName{Name: "__bootstrap_tick__"}}
	_, err := r.Reconcile(context.Background(), tick)
	require.NoError(t, err)
	require.Len(t, *published, 1, "precondition: drift was detected and reported this pass")

	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "seeds"}, &got))
	for _, cond := range got.Status.Conditions {
		assert.NotContains(t, cond.Reason, "Drift", "no condition stamped by drift detection")
		assert.NotContains(t, cond.Message, "unexpected", "no condition message carries the drift finding")
	}
}

// relTuple converts a SpiceDBBootstrapRelationship into the spicedb.Tuple
// it resolves to, matching relGroupMember's own shape (group:engineering
// #member@user:<canonical>) so a test can seed read-back with exactly what
// the reconciler's own sync path will TOUCH.
func relTuple(rel spiceboxv1alpha1.SpiceDBBootstrapRelationship) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: rel.Resource.Type,
		ResourceID:   rel.Resource.ID,
		Relation:     rel.Relation,
		SubjectType:  rel.Subject.Type,
		SubjectID:    rel.Subject.ID,
	}
}

// TestDrift_RefusedRelationshipNeverReportedAcrossMultiplePasses confirms
// the branch-fix-3 brief's prediction that fixing the Major deletion wedge
// (a refused tuple wrongly entering r.lastDesired) also closes the paired
// Minor: the same refused tuple reported as drift on EVERY pass.
//
// Before the Major fix, a refused relationship entered lastDesired
// unconditionally on schema success, so from the second reconcile onward
// alreadyKnown.Has(key) was true and a read-back correctly finding it
// absent (it was never actually written — pttagmint owns pt_tag#session,
// not this CR) was reported as Missing: a Failed monitoring event fired on
// every non-debounced reconcile, forever, duplicating what
// RelationshipsApplied/ReasonRelationOwnedByAnotherSource already says once.
// After the fix, a refused relationship never enters lastDesired at all, so
// it stays exempt from Missing (detectBootstrapDrift's "freshly claimed
// this pass, not yet written" rule) on every pass, not just the first —
// three consecutive reconciles here stand in for "forever".
func TestDrift_RefusedRelationshipNeverReportedAcrossMultiplePasses(t *testing.T) {
	boot := ownershipBoot(ownershipRel("pt_tag", "session")) // claimed by pttagmint
	r, _, _, w := newBootReconciler(t, boot)
	r.Writer = &guardedFakeWriter{fakeWriter: w, src: spicedb.BootstrapSource}
	fr := newFakeReader() // nothing seeded: the refused (never-written) tuple reads back absent
	r.Reader = fr
	publish, published := capturePublish(t, nil)
	r.MonitoringPublish = publish

	for i := 0; i < 3; i++ {
		reconcileBootstrapTick(t, r)
	}

	assert.Positive(t, fr.readCount(), "the read-back really happened across every pass; this isn't passing because the Reader went unused")
	assert.Empty(t, *published,
		"a relationship refused at write time was never written, so its absence on read-back must never be "+
			"reported as drift — on the first pass or any later one")
}
