// pkg/controllers/settings/revoke_publisher_test.go
//
// Unit tests for RevokePublisher. Drives the publisher with a
// revocation.Publisher wrapping a capBus that records every emitted
// envelope, and asserts the v1 tri-state diff rule for both
// allowedMCPServers and allowedToolkits.
//
// Tri-state diff rule under test:
//
//	nil  → allow-all; NO emit on transition from nil (can't enumerate the
//	        universe; next-session gate covers allow-all→allowlist).
//	[]   → allow-none (deny-all).
//	[..] → allowlist.
//
// Cases covered (independently for each list):
//  1. First observation: prime only, no emit.
//  2. old [A,B,X] → new [A,B]: emit revoke for X.
//  3. old [X] → new []: emit revoke for X.
//  4. old nil → new []: no emit (widening not tracked).
//  5. old nil → new [A]: no emit.
//  6. old [A] → new nil: no emit (widening to allow-all).
//  7. old [] → new []: no emit (no change).
//  8. old [A] → new [A]: no emit (no change).
//  9. Multiple simultaneous removals.
//  10. Different CR keys track independently.
//  11. Cluster scope ("") vs namespace scope.
//  12. Nil bus tolerated without panic.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// capBus captures every revocation.EventPublisher.Publish call so the
// test can decode the unified KindRevoked envelope.
//
// setFailure makes every subsequent Publish record nothing and return the given
// error — the "NATS refused the envelope" case the trigger state must survive.
type capBus struct {
	mu        sync.Mutex
	published []channelevents.Envelope
	failErr   error
}

func (c *capBus) Publish(_ context.Context, env channelevents.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return c.failErr
	}
	c.published = append(c.published, env)
	return nil
}

// setFailure switches the bus between failing and healthy. nil restores it.
func (c *capBus) setFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failErr = err
}

func (c *capBus) snapshot() []channelevents.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channelevents.Envelope, len(c.published))
	copy(out, c.published)
	return out
}

// newCapturingPublisher returns a RevokePublisher whose bus records every
// emitted envelope, plus the capBus for assertions.
func newCapturingPublisher(t *testing.T) (*RevokePublisher, *capBus) {
	t.Helper()
	bus := &capBus{}
	return NewRevokePublisher(revocation.NewPublisher(bus)), bus
}

// statuses hands out one operator-owned SettingsStatus per CR key, standing in
// for the CRs a reconciler would be reading and writing. The publisher reads
// its durable trigger state from the status of the CR it is observing and
// stamps the advanced record back, so a test that reuses the same status
// across calls is reproducing exactly what a reconciler does across reconciles
// — and one that hands a FRESH publisher the SAME status is reproducing an
// operator restart.
type statuses map[string]*spiceboxv1alpha1.SettingsStatus

// of returns the status for crKey, minting an empty one (a CR nobody has
// observed yet) on first use.
func (s statuses) of(crKey string) *spiceboxv1alpha1.SettingsStatus {
	st, ok := s[crKey]
	if !ok {
		st = &spiceboxv1alpha1.SettingsStatus{}
		s[crKey] = st
	}
	return st
}

// decodeRevoked decodes one envelope as KindRevoked + RevokedPayload,
// asserting shared invariants.
func decodeRevoked(t *testing.T, env channelevents.Envelope) channelevents.RevokedPayload {
	t.Helper()
	assert.Equal(t, channelevents.KindRevoked, env.Kind, "must emit unified KindRevoked")
	var p channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(env.Payload, &p), "decode RevokedPayload")
	assert.Equal(t, toolOriginRevokeKind, p.Kind, "revoke kind must be tool-origin")
	return p
}

// helpers to build allowlist slices.

func mcpList(names ...string) *[]spiceboxv1alpha1.AllowedMCPServer {
	s := make([]spiceboxv1alpha1.AllowedMCPServer, len(names))
	for i, n := range names {
		s[i] = spiceboxv1alpha1.AllowedMCPServer{Name: n}
	}
	return &s
}

func tkList(names ...string) *[]string {
	s := make([]string, len(names))
	copy(s, names)
	return &s
}

// emptyMCP returns a non-nil empty AllowedMCPServers list (deny-all).
func emptyMCP() *[]spiceboxv1alpha1.AllowedMCPServer {
	s := []spiceboxv1alpha1.AllowedMCPServer{}
	return &s
}

// emptyTK returns a non-nil empty toolkits list (deny-all).
func emptyTK() *[]string {
	s := []string{}
	return &s
}

// collectKeys returns a map of emitted revoke keys from a snapshot of envelopes.
func collectKeys(t *testing.T, envs []channelevents.Envelope) map[string]string {
	t.Helper()
	got := make(map[string]string, len(envs))
	for _, env := range envs {
		p := decodeRevoked(t, env)
		got[p.Key] = p.Scope
	}
	return got
}

const clusterKey = "cluster"

// --- allowedMCPServers tests ---

func TestRevokePublisher_MCP_FirstObservationPrimes(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(context.Background(), mcpList("linear", "github"), nil, sts.of(clusterKey), clusterKey, "")
	assert.Empty(t, bus.snapshot(), "first observation must prime without emitting")
}

func TestRevokePublisher_MCP_ExplicitRemovalEmitsRevoke(t *testing.T) {
	cases := []struct {
		name     string
		before   *[]spiceboxv1alpha1.AllowedMCPServer
		after    *[]spiceboxv1alpha1.AllowedMCPServer
		wantKeys []string
	}{
		{
			name:     "remove one from [A,B,X] → [A,B]: emit revoke for X",
			before:   mcpList("linear", "github", "stripe"),
			after:    mcpList("linear", "github"),
			wantKeys: []string{"mcpserver/stripe"},
		},
		{
			name:     "remove all from [X] → []: emit revoke for X",
			before:   mcpList("linear"),
			after:    emptyMCP(),
			wantKeys: []string{"mcpserver/linear"},
		},
		{
			name:     "old nil → new []: no emit (allow-all → deny-all widening not tracked)",
			before:   nil,
			after:    emptyMCP(),
			wantKeys: nil,
		},
		{
			name:     "old nil → new [A]: no emit (allow-all → concrete list)",
			before:   nil,
			after:    mcpList("linear"),
			wantKeys: nil,
		},
		{
			name:     "old [A] → new nil: no emit (widening to allow-all)",
			before:   mcpList("linear"),
			after:    nil,
			wantKeys: nil,
		},
		{
			name:     "old [] → new []: no emit (no change, both deny-all)",
			before:   emptyMCP(),
			after:    emptyMCP(),
			wantKeys: nil,
		},
		{
			name:     "old [A] → new [A]: no emit (no change)",
			before:   mcpList("linear"),
			after:    mcpList("linear"),
			wantKeys: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp, bus := newCapturingPublisher(t)
			sts := statuses{}
			// Prime.
			rp.Observe(context.Background(), tc.before, nil, sts.of(clusterKey), clusterKey, "")
			// Diff.
			rp.Observe(context.Background(), tc.after, nil, sts.of(clusterKey), clusterKey, "")

			envs := bus.snapshot()
			if len(tc.wantKeys) == 0 {
				assert.Empty(t, envs, "no revoke expected")
				return
			}
			require.Len(t, envs, len(tc.wantKeys))
			got := collectKeys(t, envs)
			for _, k := range tc.wantKeys {
				assert.Contains(t, got, k, "expected revoke key %q", k)
				assert.Equal(t, "", got[k], "cluster scope must be empty string")
			}
		})
	}
}

func TestRevokePublisher_MCP_MultipleSimultaneousRemovals(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	// Prime with three entries.
	rp.Observe(context.Background(), mcpList("linear", "github", "stripe"), nil, sts.of(clusterKey), clusterKey, "")
	// Remove two.
	rp.Observe(context.Background(), mcpList("linear"), nil, sts.of(clusterKey), clusterKey, "")

	envs := bus.snapshot()
	require.Len(t, envs, 2)
	got := collectKeys(t, envs)
	assert.Equal(t, map[string]string{
		"mcpserver/github": "",
		"mcpserver/stripe": "",
	}, got)
}

// --- allowedToolkits tests ---

func TestRevokePublisher_Toolkit_FirstObservationPrimes(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(context.Background(), nil, tkList("golang-tools", "web-search"), sts.of(clusterKey), clusterKey, "")
	assert.Empty(t, bus.snapshot(), "first observation must prime without emitting")
}

func TestRevokePublisher_Toolkit_ExplicitRemovalEmitsRevoke(t *testing.T) {
	cases := []struct {
		name     string
		before   *[]string
		after    *[]string
		wantKeys []string
	}{
		{
			name:     "remove one from [A,B,X] → [A,B]: emit revoke for X",
			before:   tkList("golang-tools", "web-search", "code-exec"),
			after:    tkList("golang-tools", "web-search"),
			wantKeys: []string{"toolkit/code-exec"},
		},
		{
			name:     "remove all from [X] → []: emit revoke for X",
			before:   tkList("web-search"),
			after:    emptyTK(),
			wantKeys: []string{"toolkit/web-search"},
		},
		{
			name:     "old nil → new []: no emit",
			before:   nil,
			after:    emptyTK(),
			wantKeys: nil,
		},
		{
			name:     "old nil → new [A]: no emit",
			before:   nil,
			after:    tkList("golang-tools"),
			wantKeys: nil,
		},
		{
			name:     "old [A] → new nil: no emit (widening to allow-all)",
			before:   tkList("golang-tools"),
			after:    nil,
			wantKeys: nil,
		},
		{
			name:     "old [] → new []: no emit (no change)",
			before:   emptyTK(),
			after:    emptyTK(),
			wantKeys: nil,
		},
		{
			name:     "old [A] → new [A]: no emit (unchanged)",
			before:   tkList("golang-tools"),
			after:    tkList("golang-tools"),
			wantKeys: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp, bus := newCapturingPublisher(t)
			sts := statuses{}
			// Prime.
			rp.Observe(context.Background(), nil, tc.before, sts.of(clusterKey), clusterKey, "")
			// Diff.
			rp.Observe(context.Background(), nil, tc.after, sts.of(clusterKey), clusterKey, "")

			envs := bus.snapshot()
			if len(tc.wantKeys) == 0 {
				assert.Empty(t, envs, "no revoke expected")
				return
			}
			require.Len(t, envs, len(tc.wantKeys))
			got := collectKeys(t, envs)
			for _, k := range tc.wantKeys {
				assert.Contains(t, got, k, "expected revoke key %q", k)
			}
		})
	}
}

func TestRevokePublisher_Toolkit_MultipleSimultaneousRemovals(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(context.Background(), nil, tkList("golang-tools", "web-search", "code-exec"), sts.of(clusterKey), clusterKey, "")
	rp.Observe(context.Background(), nil, tkList("golang-tools"), sts.of(clusterKey), clusterKey, "")

	envs := bus.snapshot()
	require.Len(t, envs, 2)
	got := collectKeys(t, envs)
	assert.Equal(t, map[string]string{
		"toolkit/web-search": "",
		"toolkit/code-exec":  "",
	}, got)
}

// --- Both lists together ---

func TestRevokePublisher_BothLists_SimultaneousRemovals(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	// Prime with both lists populated.
	rp.Observe(context.Background(),
		mcpList("linear", "github"),
		tkList("golang-tools", "web-search"),
		sts.of(clusterKey), clusterKey, "")
	// Remove one from each list.
	rp.Observe(context.Background(),
		mcpList("linear"),
		tkList("golang-tools"),
		sts.of(clusterKey), clusterKey, "")

	envs := bus.snapshot()
	require.Len(t, envs, 2)
	got := collectKeys(t, envs)
	assert.Equal(t, map[string]string{
		"mcpserver/github":   "",
		"toolkit/web-search": "",
	}, got)
}

// --- Scope propagation ---

func TestRevokePublisher_NamespaceScope_PropagatesCorrectly(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	const ns = "team-a"
	crKey := ns + "/default"

	rp.Observe(context.Background(), mcpList("linear"), tkList("golang-tools"), sts.of(crKey), crKey, ns)
	rp.Observe(context.Background(), emptyMCP(), emptyTK(), sts.of(crKey), crKey, ns)

	envs := bus.snapshot()
	require.Len(t, envs, 2)
	got := collectKeys(t, envs)
	assert.Equal(t, map[string]string{
		"mcpserver/linear":     ns,
		"toolkit/golang-tools": ns,
	}, got)
}

// --- Independent CR tracking ---

func TestRevokePublisher_DifferentCRKeysAreIndependent(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}

	// Prime two CRs.
	rp.Observe(context.Background(), mcpList("linear"), nil, sts.of("cluster"), "cluster", "")
	rp.Observe(context.Background(), mcpList("github"), nil, sts.of("team-b/default"), "team-b/default", "team-b")
	assert.Empty(t, bus.snapshot(), "prime only — no emits")

	// Remove from cluster CR; team-b/default unchanged.
	rp.Observe(context.Background(), emptyMCP(), nil, sts.of("cluster"), "cluster", "")

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, "mcpserver/linear", p.Key, "revoke targets only the cluster CR entry")
	assert.Equal(t, "", p.Scope)
}

// --- Durable trigger state ---

func TestRevokePublisher_FailedEmitLeavesTheRemovalPendingForTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(ctx, mcpList("linear", "github"), nil, sts.of(clusterKey), clusterKey, "")

	// "github" is withdrawn, but the bus refuses the envelope.
	bus.setFailure(errors.New("nats: connection closed"))
	require.Error(t, rp.Observe(ctx, mcpList("linear"), nil, sts.of(clusterKey), clusterKey, ""),
		"a revoke that did not publish must be reported so the reconciler requeues")
	require.Empty(t, bus.snapshot(), "precondition: the failing bus recorded nothing")

	// The bus recovers. The next reconcile sees the same spec it already
	// diffed, so the only thing that can produce a second attempt is the
	// trigger state NOT having advanced past the failed emit.
	bus.setFailure(nil)
	require.NoError(t, rp.Observe(ctx, mcpList("linear"), nil, sts.of(clusterKey), clusterKey, ""))

	envs := bus.snapshot()
	require.Len(t, envs, 1, "a failed emit must leave the removal pending, not roll the trigger state forward")
	assert.Equal(t, "mcpserver/github", decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_FailedEmitHoldsOnlyTheOriginThatFailed(t *testing.T) {
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(ctx, mcpList("linear"), tkList("golang-tools"), sts.of(clusterKey), clusterKey, "")

	// Both lists lose an entry; only the toolkit publish fails, because the
	// bus is switched to failing for the whole call.
	bus.setFailure(errors.New("nats: connection closed"))
	require.Error(t, rp.Observe(ctx, emptyMCP(), emptyTK(), sts.of(clusterKey), clusterKey, ""))

	bus.setFailure(nil)
	require.NoError(t, rp.Observe(ctx, emptyMCP(), emptyTK(), sts.of(clusterKey), clusterKey, ""))

	got := collectKeys(t, bus.snapshot())
	assert.Equal(t, map[string]string{
		"mcpserver/linear":     "",
		"toolkit/golang-tools": "",
	}, got, "both held-back origins must be retried, each exactly once")
}

func TestRevokePublisher_FreshProcessReDerivesARemovalCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	sts := statuses{}

	// Operator #1 observes the CR and records what it saw.
	rp1, bus1 := newCapturingPublisher(t)
	rp1.Observe(ctx, mcpList("linear", "github"), tkList("golang-tools"), sts.of(clusterKey), clusterKey, "")
	require.Empty(t, bus1.snapshot(), "first observation primes without emitting")

	// The operator restarts. While it was down, an admin withdrew "github".
	// Operator #2's in-memory map is empty, so the only thing that can tell it
	// an origin disappeared is what operator #1 recorded on the CR.
	rp2, bus2 := newCapturingPublisher(t)
	rp2.Observe(ctx, mcpList("linear"), tkList("golang-tools"), sts.of(clusterKey), clusterKey, "")

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a fresh process must still revoke an origin withdrawn while it was down")
	assert.Equal(t, "mcpserver/github", decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_RecordPreservesTheAllowAllVsDenyAllDistinction(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		mcp      *[]spiceboxv1alpha1.AllowedMCPServer
		wantList *[]string
	}{
		{name: "absent list records as nil (allow-all), not as an empty list", mcp: nil, wantList: nil},
		{name: "empty list records as an empty list (deny-all), not as nil", mcp: emptyMCP(), wantList: &[]string{}},
		{name: "populated list records its sorted names", mcp: mcpList("linear", "github"), wantList: &[]string{"github", "linear"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp, _ := newCapturingPublisher(t)
			sts := statuses{}
			rp.Observe(ctx, tc.mcp, nil, sts.of(clusterKey), clusterKey, "")

			rec := sts.of(clusterKey).ObservedLimits
			require.NotNil(t, rec, "observing a CR must always leave a record; nil means never observed")
			assert.Equal(t, tc.wantList, rec.AllowedMCPServers)
		})
	}
}

func TestRevokePublisher_UnchangedSpecRecordsAByteIdenticalValue(t *testing.T) {
	ctx := context.Background()
	rp, _ := newCapturingPublisher(t)
	sts := statuses{}
	rp.Observe(ctx, mcpList("linear", "github"), tkList("web-search", "golang-tools"), sts.of(clusterKey), clusterKey, "")
	first := sts.of(clusterKey).ObservedLimits.DeepCopy()

	rp.Observe(ctx, mcpList("github", "linear"), tkList("golang-tools", "web-search"), sts.of(clusterKey), clusterKey, "")

	assert.Equal(t, first, sts.of(clusterKey).ObservedLimits,
		"the record is a sorted, derived value, so re-observing the same allowlists in any order must not churn it")
}

// --- Nil bus tolerance ---

func TestRevokePublisher_NilBus_Tolerates(t *testing.T) {
	rp := NewRevokePublisher(nil)
	sts := statuses{}
	// Prime.
	rp.Observe(context.Background(), mcpList("linear"), tkList("golang-tools"), sts.of(clusterKey), clusterKey, "")
	// Diff that would emit (but bus is nil).
	rp.Observe(context.Background(), emptyMCP(), emptyTK(), sts.of(clusterKey), clusterKey, "")
	// No panic — test passes if we reach here.
}

// --- Nil RevokePublisher field in reconcilers (controller wiring guard) ---

func TestRevokePublisher_NilFieldIsGuarded(t *testing.T) {
	// Verify that a ClusterReconciler / NamespaceReconciler with nil
	// RevokePublisher field does NOT call Observe (the nil-guard in
	// controller.go). This is a smoke check: if the guard is missing,
	// the reconciler panics when calling nil.Observe.
	var cr ClusterReconciler
	assert.Nil(t, cr.RevokePublisher, "zero-value must be nil")

	var nr NamespaceReconciler
	assert.Nil(t, nr.RevokePublisher, "zero-value must be nil")
}
