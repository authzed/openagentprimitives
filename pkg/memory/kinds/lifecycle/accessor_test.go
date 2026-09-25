package lifecycle_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	kind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

const testUID = "uid-1"

// runnerKey / opPreKey / opPostKey build the ordering keys the runner and
// operator sequencers stamp, so tests exercise the same fold order production
// does. Runner events carry their real (memTurnIndex, blockIndex); operator
// boundary events anchor to a turn with a start (pre) or end (post) block.
func runnerKey(turn, block int) kind.OrderKey {
	return kind.OrderKey{Seq: channelevents.PackSeq(turn, block), Region: string(lc.RegionRunner), SessionUID: testUID}
}
func opPreKey(turn int) kind.OrderKey {
	return kind.OrderKey{Seq: channelevents.PackSeq(turn, channelevents.SeqBlockStart), Region: string(lc.RegionOperatorPre), SessionUID: testUID}
}
func opPostKey(turn int) kind.OrderKey {
	return kind.OrderKey{Seq: channelevents.PackSeq(turn, channelevents.SeqBlockEnd), Region: string(lc.RegionOperatorPost), SessionUID: testUID}
}

// step is one appended lifecycle event: its type, its wall-clock createdAt, and
// its cross-publisher ordering key.
type step struct {
	ev  lc.Event
	at  time.Time
	key kind.OrderKey
}

// foldSteps appends every step to a fresh scope, reads them back through the
// accessor (which orders by the skew-immune key, not createdAt), and returns
// the projected phase.
func foldSteps(t *testing.T, steps []step) string {
	t.Helper()
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/fold"}
	for _, s := range steps {
		require.NoError(t, kind.Append(memory.WithSystemApproval(context.Background(), "test"), m, scope, s.ev, s.at, s.key), "append %T", s.ev)
	}
	events, err := kind.Events(memory.WithSystemApproval(context.Background(), "test"), m, scope)
	require.NoError(t, err, "Events")
	require.Len(t, events, len(steps), "every appended event is read back")
	return lc.Project(lc.Fold(events)).Phase
}

// TestAppendThenEventsRoundTrips appends typed events and reads them back in
// order, ready to fold.
func TestAppendThenEventsRoundTrips(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	base := time.Now().UTC().Truncate(time.Second)

	in := []lc.Event{
		lc.SettingsAccepted{},
		lc.CredsMissing{},
		lc.CredsLinked{},
		lc.RunnerClaimed{},
	}
	for i, ev := range in {
		require.NoError(t, kind.Append(memory.WithSystemApproval(context.Background(), "test"), m, scope, ev, base.Add(time.Duration(i)*time.Second), kind.OrderKey{}))
	}

	got, err := kind.Events(memory.WithSystemApproval(context.Background(), "test"), m, scope)
	require.NoError(t, err, "Events")
	require.Len(t, got, len(in))
	for i := range in {
		assert.IsType(t, in[i], got[i], "event %d preserves concrete type/order", i)
	}

	// Folding the read-back stream lands on the expected phase.
	assert.Equal(t, "Running", lc.Project(lc.Fold(got)).Phase)
}

// TestFold_SkewedCreatedAt_CrossPublisherMisorder is the regression guard for
// the two-publisher fold-ordering defect. Both the operator (system:operator,
// pre/post regions) and the runner (session:<ns/name>, live region) append typed
// events to the same scope, from independent clocks. The operator records a
// duplicate RunnerClaimed on the reconcile that first observes the pod becoming
// Ready, which can lag the runner's own progress — so the operator's
// RunnerClaimed can land with a createdAt LATER than a runner live-region event
// that causally follows the handoff.
//
// Ordering the fold by wall-clock createdAt then reset the yielded session back
// to Running: RunnerClaimed unconditionally sets Phase=Running, and Idle is not a
// sticky terminal phase. The fold now orders by the skew-immune OrderKey instead
// (see order.go): both RunnerClaimed events sit at the operator_pre/runner
// handoff of turn 0, which sorts before turn 0's terminal AgentWorkComplete, so
// the session correctly folds to Idle even though the operator's duplicate
// RunnerClaimed carries the LATEST createdAt.
//
// This test locked in the mis-order (folding to Running) before the ordering key
// was adopted; it now asserts the region-correct Idle. See
// .superpowers/sdd/audit-A1-report.md and .superpowers/sdd/audit-A1fix-report.md.
func TestFold_SkewedCreatedAt_CrossPublisherMisorder(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)

	// Region/causal order: both handoff claims precede the runner's terminal
	// AgentWorkComplete, which is the last state-moving event, so the session
	// parks at Idle.
	regionOrder := []lc.Event{
		lc.SettingsAccepted{},                // operator pre-region
		lc.RunnerClaimed{},                   // runner's own claim
		lc.RunnerClaimed{},                   // operator's duplicate claim, same handoff
		lc.AgentWorkComplete{Kubectl: false}, // runner live region -> Idle
	}
	require.Equal(t, "Idle", lc.Project(lc.Fold(regionOrder)).Phase,
		"region-ordered fold: a channel session that finished its work parks at Idle")

	// Same events, appended with an inverted createdAt — the operator's duplicate
	// RunnerClaimed stamped AFTER the runner's AgentWorkComplete (the inversion a
	// lagging operator reconcile, or an operator clock running ahead of the
	// runner's, produces near the handoff). The OrderKey folds it back to Idle.
	got := foldSteps(t, []step{
		{lc.SettingsAccepted{}, base, opPreKey(0)},
		{lc.RunnerClaimed{}, base.Add(2 * time.Second), runnerKey(0, channelevents.SeqBlockStart)},
		{lc.AgentWorkComplete{Kubectl: false}, base.Add(3 * time.Second), runnerKey(0, channelevents.SeqBlockEnd)},
		{lc.RunnerClaimed{}, base.Add(5 * time.Second), opPreKey(0)}, // operator duplicate, stamped late
	})
	assert.Equal(t, "Idle", got,
		"skew-immune OrderKey folds the late operator RunnerClaimed before the same-turn terminal -> Idle")
}

// TestFold_CrossPublisherOrdering_RegionCorrect exercises the three ordering
// invariants that distinguish a stale claim from a genuine wake, plus a
// scrambled multi-turn interleave, all under adversarial createdAt.
func TestFold_CrossPublisherOrdering_RegionCorrect(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)

	cases := []struct {
		name  string
		steps []step
		want  string
	}{
		{
			// A stale operator RunnerClaimed of the SAME incarnation as the Idle
			// (same turn) must sort before the runner's terminal and leave the
			// session at Idle — even stamped with a later createdAt.
			name: "stale same-turn operator RunnerClaimed after Idle: Idle",
			steps: []step{
				{lc.RunnerClaimed{}, base, runnerKey(0, channelevents.SeqBlockStart)},
				{lc.AgentWorkComplete{Kubectl: false}, base.Add(1 * time.Second), runnerKey(0, channelevents.SeqBlockEnd)},
				{lc.RunnerClaimed{}, base.Add(9 * time.Second), opPreKey(0)}, // stale duplicate, late clock
			},
			want: "Idle",
		},
		{
			// A genuine wake: a new message respawns the runner, which claims a
			// LATER turn. That RunnerClaimed must fold after the prior turn's Idle
			// and move the session to Running — proven by giving it the EARLIEST
			// createdAt, so only the turn index (not the clock) can order it last.
			name: "wake operator+runner RunnerClaimed of a later turn: Running",
			steps: []step{
				{lc.RunnerClaimed{}, base.Add(3 * time.Second), runnerKey(0, channelevents.SeqBlockStart)},
				{lc.AgentWorkComplete{Kubectl: false}, base.Add(4 * time.Second), runnerKey(0, channelevents.SeqBlockEnd)},
				{lc.WakeRequested{}, base.Add(5 * time.Second), opPostKey(0)},
				{lc.RunnerClaimed{}, base, runnerKey(1, channelevents.SeqBlockStart)}, // wake claim, earliest clock
			},
			want: "Running",
		},
		{
			// Two turns interleaved with operator pre/post boundary events, all
			// appended in scrambled createdAt order. The fold must recover the
			// region order: turn 0 rests Idle, a wake advances to turn 1, whose
			// runner is mid-turn (no terminal) -> Running.
			name: "scrambled multi-turn interleave: Running (mid turn 1)",
			steps: []step{
				{lc.RunnerClaimed{}, base.Add(20 * time.Second), runnerKey(1, channelevents.SeqBlockStart)},                // turn 1 claim, late clock
				{lc.AgentWorkComplete{Kubectl: false}, base.Add(2 * time.Second), runnerKey(0, channelevents.SeqBlockEnd)}, // turn 0 terminal
				{lc.SettingsAccepted{}, base.Add(50 * time.Second), opPreKey(0)},                                           // provisioning, latest clock
				{lc.RunnerClaimed{}, base.Add(10 * time.Second), opPreKey(1)},                                              // operator dup claim, turn 1
				{lc.WakeRequested{}, base.Add(1 * time.Second), opPostKey(0)},                                              // wake, earliest-ish clock
				{lc.RunnerClaimed{}, base.Add(30 * time.Second), runnerKey(0, channelevents.SeqBlockStart)},                // turn 0 claim, latest-ish clock
			},
			want: "Running",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, foldSteps(t, tc.steps))
		})
	}
}

// TestFold_LegacyUnstampedEntries_FoldByCreatedAt proves entries written before
// the OrderKey existed (no key) still fold — by wall-clock createdAt, exactly as
// the pre-key accessor did. Appended out of createdAt order to show the fallback
// re-sorts them.
func TestFold_LegacyUnstampedEntries_FoldByCreatedAt(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	got := foldSteps(t, []step{
		{lc.AgentWorkComplete{Kubectl: false}, base.Add(2 * time.Second), kind.OrderKey{}}, // causally last, appended first
		{lc.SettingsAccepted{}, base, kind.OrderKey{}},
		{lc.RunnerClaimed{}, base.Add(1 * time.Second), kind.OrderKey{}},
	})
	assert.Equal(t, "Idle", got,
		"legacy unstamped entries fold by createdAt: Settings -> Claim(Running) -> AgentWorkComplete(Idle)")
}

// TestFold_MixedKeyedAndLegacy_RegionCorrect exercises the realistic upgrade
// transition: a single session's log holds BOTH legacy zero-key entries (written
// before the OrderKey existed, folding by createdAt) AND keyed entries (written
// after), plus the every-session audit entries — a zero-key ScopeMutated and, on
// SIGTERM, a Stopped now stamped with the terminal-stop sentinel key.
//
// The scenario: a session boots and runs (legacy), mutates scope (legacy audit),
// completes a kubectl turn to Succeeded (keyed), then a SIGTERM records Stopped.
// Stopped is stamped with an EARLIER createdAt than the keyed Succeeded to prove
// the key, not the wall clock, orders it: the sentinel Seq sorts Stopped after
// every keyed event, so the sticky Succeeded wins and the fold is Succeeded.
//
// Without FIX 1 (a zero-key Stopped at Seq 0) this same log would sort Stopped in
// among the legacy entries — before the keyed Succeeded — and fold to Failed. This
// test therefore fails unless the terminal-stop sentinel is in place.
func TestFold_MixedKeyedAndLegacy_RegionCorrect(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	got := foldSteps(t, []step{
		// Legacy prefix (written by the pre-key runner/operator): fold by createdAt.
		{lc.SettingsAccepted{}, base, kind.OrderKey{}},
		{lc.RunnerClaimed{}, base.Add(1 * time.Second), kind.OrderKey{}},
		// Legacy audit-only entry every session may emit: no phase effect, zero key.
		{lc.ScopeMutated{}, base.Add(2 * time.Second), kind.OrderKey{}},
		// Post-upgrade keyed terminal: a kubectl turn completes -> Succeeded.
		{lc.AgentWorkComplete{Kubectl: true}, base.Add(5 * time.Second), runnerKey(0, channelevents.SeqBlockEnd)},
		// SIGTERM Stopped, stamped with the terminal-stop sentinel. Earlier clock
		// than the Succeeded above, so only the key can order it last.
		{lc.Stopped{}, base.Add(3 * time.Second), kind.TerminalStopKey("uid-legacy-mixed")},
	})
	assert.Equal(t, "Succeeded", got,
		"mixed legacy+keyed log: the terminal-stop Stopped sorts after the keyed Succeeded, "+
			"which stays sticky -> Succeeded (a zero-key Stopped would wrongly fold Failed)")
}

// TestFold_SignedRoundTrip_OrderKeyVerifies proves the OrderKey is covered by the
// append-only Ed25519 signature (it rides inside the signed Content), so a signed
// round-trip verifies and the key survives read-back — no change to the digest or
// the provenance chain.
func TestFold_SignedRoundTrip_OrderKeyVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "generate signing key")
	signer := provenance.NewSigner(priv, "session:ns/sign")
	signed := provenance.NewSigningMemory(memory.NewLocal(inmem.NewBackend()), signer)
	scope := memory.Scope{Kind: "session", ID: "ns/sign"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	base := time.Now().UTC().Truncate(time.Second)
	in := []step{
		{lc.RunnerClaimed{}, base, runnerKey(0, channelevents.SeqBlockStart)},
		{lc.AgentWorkComplete{Kubectl: false}, base.Add(time.Second), runnerKey(0, channelevents.SeqBlockEnd)},
	}
	for _, s := range in {
		// SigningMemory.Put seeds the chain and signs the entry (Content includes
		// the OrderKey) before it reaches the store.
		require.NoError(t, kind.Append(ctx, signed, scope, s.ev, s.at, s.key), "signed append %T", s.ev)
	}

	// The key survives the signed round-trip and still folds region-correct.
	ordered, err := kind.ReadOrdered(ctx, signed, scope)
	require.NoError(t, err, "ReadOrdered")
	require.Len(t, ordered, len(in))
	assert.Equal(t, string(lc.RegionRunner), ordered[0].Key.Region, "OrderKey region round-trips")
	assert.Equal(t, channelevents.PackSeq(0, channelevents.SeqBlockEnd), ordered[1].Key.Seq, "OrderKey seq round-trips")

	// Every stored entry's signature verifies over the digest of its Content —
	// which now includes the OrderKey — so verify-on-write / `oap audit verify`
	// still pass with the key present.
	res, err := signed.Query(ctx, memory.Query{Scope: scope, Kinds: []string{kind.New().Name()}})
	require.NoError(t, err, "query raw entries")
	require.Len(t, res.Entries, len(in))
	for _, e := range res.Entries {
		require.NotNil(t, e.Provenance, "append-only entry is signed")
		digest, derr := decodeDigest(t, e)
		require.NoError(t, derr)
		assert.True(t, ed25519.Verify(pub, digest, e.Provenance.Sig),
			"signature over the Content-with-OrderKey verifies for %s", e.ID)
	}
}

// decodeDigest returns the raw bytes of an entry's canonical provenance digest —
// the message the Ed25519 signature covers.
func decodeDigest(t *testing.T, e memory.Entry) ([]byte, error) {
	t.Helper()
	return hex.DecodeString(provenance.EntryDigest(e))
}

// TestEventsSkipsRecordedSignals proves a signal recording (written by the
// ScopeHooks, no EventTag) is not returned by Events and never fed to the
// envelope decoder.
func TestEventsSkipsRecordedSignals(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/b"}
	now := time.Now().UTC().Truncate(time.Second)

	// A recorded signal lands via the ScopeHooks path (tagged with the signal
	// kind, content is the raw signal payload — NOT an Event envelope).
	kind.Setup(m)
	defer kind.Teardown()
	require.NoError(t, m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
		Kind: kind.SigSessionStarted, Scope: scope, At: now,
	}))
	// Plus a real transition event.
	require.NoError(t, kind.Append(memory.WithSystemApproval(context.Background(), "test"), m, scope, lc.SettingsAccepted{}, now.Add(time.Second), kind.OrderKey{}))

	got, err := kind.Events(memory.WithSystemApproval(context.Background(), "test"), m, scope)
	require.NoError(t, err, "Events must skip the recorded signal, not error on it")
	require.Len(t, got, 1)
	assert.IsType(t, lc.SettingsAccepted{}, got[0])
}
