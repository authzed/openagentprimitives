package admind

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// fakeClock is a hand-advanced clock for the TTL sweep. The sweep is the only
// time-dependent behavior in the aggregator, and it is exercised here rather
// than from the external test package because it needs the unexported
// constructor seam — the exported NewAggregator stays a one-argument
// constructor for its production callers.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// foldActivity delivers a turn_activity envelope for ns/name on ns/name's OWN
// out subject — the legitimate case, subject and envelope agreeing.
func foldActivity(t *testing.T, ag *Aggregator, ns, name string) {
	t.Helper()
	payload, err := json.Marshal(channelevents.TurnActivityPayload{Active: true})
	require.NoError(t, err)
	b, err := json.Marshal(channelevents.Envelope{
		Version: 1, Kind: channelevents.KindTurnActivity,
		Session: channelevents.SessionRef{Namespace: ns, Name: name}, Payload: payload,
	})
	require.NoError(t, err)
	ag.HandleEnvelopeBytes(
		channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindTurnActivity), b)
}

func runningSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = ns, name
	s.Spec.Class = "demo-agent"
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	return s
}

// TestAggregator_PrunesStubsTheCRDWatchNeverConfirms covers the second
// resurrection route: the NATS subscription is cluster-wide while the informer
// is scoped by --watch-namespaces, so an out-of-scope session's envelopes
// create a stub no UpsertSession ever confirms and no DeleteSession ever
// removes. Without the sweep that is unbounded growth in a long-running
// operator, plus a blank row and an inflated ActiveSessions KPI.
func TestAggregator_PrunesStubsTheCRDWatchNeverConfirms(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
	ag := newAggregatorWithClock(testr.New(t), clk.now)

	foldActivity(t, ag, "unwatched", "s1")
	ag.UpsertSession(runningSession("default", "s2"))
	require.Len(t, ag.Snapshot(), 2, "precondition: lazy creation still stubs an unknown session")

	// Past the stub TTL, a later fold sweeps the unconfirmed entry — and only
	// that one.
	clk.advance(stubTTL + sweepInterval)
	ag.UpsertSession(runningSession("default", "s2"))

	_, _, ok := ag.Get("unwatched", "s1")
	assert.False(t, ok, "a stub the CRD watch never confirmed must not live forever")
	_, _, ok = ag.Get("default", "s2")
	assert.True(t, ok, "a session the informer confirmed is never swept")
}

// TestAggregator_TombstoneExpiresAndLazyCreationResumes pins the tombstone as a
// bounded suppression window, not a permanent denylist: once no runner can
// still be publishing for the deleted session, the key behaves like any other
// unknown session again.
func TestAggregator_TombstoneExpiresAndLazyCreationResumes(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
	ag := newAggregatorWithClock(testr.New(t), clk.now)

	ag.UpsertSession(runningSession("default", "s1"))
	ag.DeleteSession("default", "s1")
	foldActivity(t, ag, "default", "s1")
	_, _, ok := ag.Get("default", "s1")
	require.False(t, ok, "precondition: the tombstone suppresses the post-delete envelope")

	clk.advance(tombstoneTTL + sweepInterval)
	// Any fold runs the sweep; this one also proves the expired tombstone no
	// longer suppresses lazy creation.
	foldActivity(t, ag, "default", "s1")

	_, _, ok = ag.Get("default", "s1")
	assert.True(t, ok, "an expired tombstone must not permanently deny a key")

	ag.mu.RLock()
	defer ag.mu.RUnlock()
	assert.Empty(t, ag.tombstones, "expired tombstones are pruned, not accumulated")
}
