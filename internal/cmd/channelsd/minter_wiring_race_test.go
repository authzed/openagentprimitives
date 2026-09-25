// The link minters are wired into the channelManager and the senderResolver
// AFTER the goroutines that read them are already running: internal/cmd/channelsd's
// run() does `go mgr.Run(...)` (whose Run calls reconcile immediately, before
// its first tick) and relay.Start / `go wd.Run` well before it has a
// passthroughlink.Signer and a webd URL to build the minters from.
//
// So every one of those Set* calls races its own readers. These tests run the
// two sides concurrently under -race, and pin the durable consequence the race
// leaves behind: a Sender resolved in that window is CACHED with a nil minter,
// so "View live" reports not-configured for the life of the process.
package main

import (
	"context"
	"github.com/go-logr/logr"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// stubArtifactViewMinter is a distinguishable ArtifactViewMinter: tests assert
// on instance identity, not behavior.
type stubArtifactViewMinter struct{ id string }

func (s *stubArtifactViewMinter) MintArtifactViewLink(
	_, _ string, _ identity.Principal, _ string,
) (string, error) {
	return "https://webd.example/artifact-view?stub=" + s.id, nil
}

// TestChannelManagerMinterWiringIsRaceFree runs buildListenerDeps — the
// listener-start path, reached from the manager's own reconcile goroutine —
// concurrently with the Set* calls main.go makes after that goroutine is
// already live. Fails under -race while the reads at buildListenerDeps are
// unsynchronised against the mutex-guarded writes.
func TestChannelManagerMinterWiringIsRaceFree(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	ch := &spiceboxv1alpha1.Channel{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = m.buildListenerDeps(ch, nil)
		}
	}()
	go func() {
		defer wg.Done()
		// The same four setters, in the order internal/cmd/channelsd/main.go calls them.
		m.SetPortalMinter(nil)
		m.SetExternalBaseURL(func() string { return "https://webd.example" })
		m.SetArtifactViewMinter(&viewlink.Minter{})
		m.SetSessionViewMinter(newSessionViewMinter(func() string { return "https://webd.example" }))
	}()
	wg.Wait()
}

// TestSenderResolverMinterWiringIsRaceFree does the same for the outbound
// side. resolveDeps is reached from relay.Start's subscription goroutine and
// from the watchdog's ticker, both of which are running before main.go wires
// the minters; the setters there take no lock at all.
func TestSenderResolverMinterWiringIsRaceFree(t *testing.T) {
	r := newSenderResolver(nil, nil, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = r.depsSnapshot()
		}
	}()
	go func() {
		defer wg.Done()
		r.SetApproverFanoutLimit(7)
		r.SetArtifactViewMinter(&stubArtifactViewMinter{id: "a"})
		r.SetSessionViewMinter(newSessionViewMinter(func() string { return "https://webd.example" }))
	}()
	wg.Wait()
}

// TestSetArtifactViewMinterInvalidatesCachedSenders pins the durable half of
// the finding, which outlives the race itself: SenderFor caches the
// constructed Sender per Channel and never rebuilds it, so a Sender resolved
// before the minter was wired keeps a nil ArtifactViewMinter forever — the
// "View live" button is then never offered on that Channel for the life of
// the process, with nothing in the logs to say why.
//
// A minter arriving after the fact must drop the senders that were built
// without it. Sinks are deliberately NOT dropped: they own per-thread
// debounce and buffer state, and a fresh sink would re-flush from empty.
func TestSetArtifactViewMinterInvalidatesCachedSenders(t *testing.T) {
	r := newSenderResolver(nil, nil, nil)

	// Stand in for a Sender resolved during the boot window, before wiring.
	r.mu.Lock()
	r.cache["ns/chan"] = nil
	r.cache["ns/chan|live_view_offer"] = nil
	r.sinkCache["ns/chan"] = nil
	r.mu.Unlock()

	r.SetArtifactViewMinter(&stubArtifactViewMinter{id: "late"})

	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Empty(t, r.cache,
		"senders built before the minter was wired must be dropped so the next "+
			"resolve picks the minter up; otherwise View live stays unconfigured for good")
	assert.Len(t, r.sinkCache, 1,
		"stream-delta sinks must survive: they hold per-thread buffer state a rebuild would erase")
}

// TestSetSessionViewMinterInvalidatesCachedSenders mirrors the above for the
// session-view minter, which reaches the session_view_offer sub-channel sender
// through the same cached-Deps path.
func TestSetSessionViewMinterInvalidatesCachedSenders(t *testing.T) {
	r := newSenderResolver(nil, nil, nil)
	r.mu.Lock()
	r.cache["ns/chan|session_view_offer"] = nil
	r.mu.Unlock()

	r.SetSessionViewMinter(newSessionViewMinter(func() string { return "https://webd.example" }))

	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Empty(t, r.cache, "a late session-view minter must invalidate senders built without it")
}

// TestDepsSnapshotCarriesTheWiredMinters keeps the invalidation honest: after
// wiring, a freshly built Deps must carry the exact instances that were set.
// Without this, "invalidate the cache" could pass while still handing the new
// Sender a nil minter.
func TestDepsSnapshotCarriesTheWiredMinters(t *testing.T) {
	r := newSenderResolver(nil, nil, nil)
	av := &stubArtifactViewMinter{id: "wired"}
	sv := newSessionViewMinter(func() string { return "https://webd.example" })
	au := newAgentUIMinter(func() string { return "https://webd.example" })

	r.SetArtifactViewMinter(av)
	r.SetSessionViewMinter(sv)
	r.SetAgentUIMinter(au)
	r.SetApproverFanoutLimit(3)

	got := r.depsSnapshot()
	require.NotNil(t, got.ArtifactViewMinter)
	assert.Same(t, av, got.ArtifactViewMinter)
	assert.Same(t, sv, got.SessionViewMinter)
	// NotNil first: assert.Same against a nil interface reports only "Both
	// arguments must be pointers", which does not say which field was left
	// out of the snapshot.
	assert.NotNil(t, got.AgentUIMinter, "depsSnapshot must copy the wired agent-UI minter")
	assert.Same(t, au, got.AgentUIMinter)
	assert.Equal(t, 3, got.ApproverFanoutLimit)
}

// TestBuildListenerDepsCarriesTheWiredMinters is the manager-side twin.
func TestBuildListenerDepsCarriesTheWiredMinters(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	av := &viewlink.Minter{}
	m.SetArtifactViewMinter(av)

	deps := m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil)
	var want channelkinds.ArtifactViewMinter = av
	assert.Same(t, want, deps.ArtifactViewMinter)
}

// TestMissingWiringDetectsAListenerStartedBeforeTheMintersArrived covers the
// durable half on the listener side. A listener started in the boot window
// gets nil minters and is never rebuilt — startListener early-returns for a
// running key and no spec edit restarts it — so the Slack App Home for that
// Channel has no "Manage my connections" button and every live-view click
// answers "not configured", permanently.
//
// The mask only grows (each Set* runs once at startup), so the check must
// report the gap exactly once and then stay quiet: a predicate that keeps
// reporting would restart the listener on every 5s tick forever.
func TestMissingWiringDetectsAListenerStartedBeforeTheMintersArrived(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	const key = "ns/chan"

	// A listener that started before any wiring existed.
	m.mu.Lock()
	m.listeners[key] = noopTestListener{}
	m.startedWiring[key] = 0
	m.mu.Unlock()

	assert.Equal(t, wiringSet(0), m.missingWiring(key),
		"nothing has been wired yet, so nothing is missing")

	m.SetArtifactViewMinter(&stubArtifactViewMinter{id: "late"})
	m.SetExternalBaseURL(func() string { return "https://webd.example" })

	missing := m.missingWiring(key)
	assert.Equal(t, wiringArtifactViewMinter|wiringExternalBaseURL, missing,
		"a minter wired after the listener started must be reported as missing")
	assert.Equal(t, "artifactViewMinter,externalBaseURL", missing.String(),
		"the log line must name what arrived, or the restart reads as a flap")

	// Restarting records the current wiring, after which the check goes quiet.
	m.mu.Lock()
	m.startedWiring[key] = m.optionalWiringLocked()
	m.mu.Unlock()
	assert.Equal(t, wiringSet(0), m.missingWiring(key),
		"a listener rebuilt with current wiring must not be restarted again")
}

// TestMissingWiringIgnoresChannelsWithNoListener keeps the restart check from
// firing on a Channel whose listener never started (resolve failure,
// client-hosted kind): there is nothing to restart, and startListener runs
// normally on the same tick.
func TestMissingWiringIgnoresChannelsWithNoListener(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	m.SetArtifactViewMinter(&stubArtifactViewMinter{id: "x"})
	assert.Equal(t, wiringSet(0), m.missingWiring("ns/never-started"))
}

// TestStopListenerForgetsRecordedWiring stops a stale entry outliving its
// listener: a Channel deleted and recreated must be judged on the wiring its
// new listener got, not its predecessor's.
func TestStopListenerForgetsRecordedWiring(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	const key = "ns/chan"
	m.mu.Lock()
	m.listeners[key] = noopTestListener{}
	m.startedWiring[key] = wiringArtifactViewMinter
	m.mu.Unlock()

	m.stopListener(logr.Discard(), key)

	m.mu.Lock()
	defer m.mu.Unlock()
	assert.NotContains(t, m.startedWiring, key,
		"stopListener must forget the wiring it recorded, or a recreated Channel inherits it")
}

// noopTestListener stands in for a running listener in the bookkeeping tests
// above, which exercise the wiring ledger rather than any transport.
type noopTestListener struct{}

func (noopTestListener) Start(context.Context) error { return nil }
func (noopTestListener) Stop(context.Context) error  { return nil }
