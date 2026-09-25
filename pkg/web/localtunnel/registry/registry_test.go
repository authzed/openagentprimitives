package registry_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"

	// Blank-imported so their init() self-registrations run for this test
	// binary only. Neither import belongs in a production binary alongside
	// the other: the operator blank-imports ngrok (see
	// internal/cmd/operator/main.go); stub is never blank-imported into any
	// production binary (see stub.go's doc comment) and is pulled in here,
	// and by cmd/oap's own *_test.go files, purely for tests.
	_ "github.com/authzed/openagentprimitives/pkg/web/localtunnel/ngrok"
	_ "github.com/authzed/openagentprimitives/pkg/web/localtunnel/stub"
)

func TestRegistry_UnknownProviderIsAnErrorNamingIt(t *testing.T) {
	_, err := registry.Get("nosuch")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nosuch",
		"a typo'd provider must name itself; a silent fallback would open the wrong tunnel")
	assert.Contains(t, err.Error(), "ngrok", "and list what is registered")
}

func TestRegistry_GetReturnsAFreshTunnelPerCall(t *testing.T) {
	f, err := registry.Get("stub")
	require.NoError(t, err)
	a := f(registry.Options{AuthToken: "t"})
	b := f(registry.Options{AuthToken: "t"})
	assert.NotNil(t, a, "the factory must never hand back a nil Tunnel")
	assert.NotNil(t, b, "the factory must never hand back a nil Tunnel")
	assert.NotSame(t, a, b,
		"one tunnel per endpoint: a shared instance would have two reconciles fight over one session")
}

// TestRegistry_EmptyProviderNameIsADistinctErrorFromUnknown covers the
// "what does Get do with an empty name" decision: PublicEndpointSpec.Provider
// is +kubebuilder:validation:MinLength=1 so the apiserver refuses an empty
// value, but a caller holding a zero-valued spec in memory (a stale cache
// read, a test fixture) can still reach Get(""). That must fail closed, and
// must not read as the same misconfiguration as a typo'd name — an operator
// diagnosing "empty" needs to look at where the spec came from, not at the
// list of registered provider names.
func TestRegistry_EmptyProviderNameIsADistinctErrorFromUnknown(t *testing.T) {
	_, errEmpty := registry.Get("")
	require.Error(t, errEmpty, "an empty name must fail closed, never silently resolve to a default provider")
	assert.Contains(t, errEmpty.Error(), "empty",
		"the message must name the problem as an empty provider, not just an unrecognized one")

	_, errUnknown := registry.Get("nosuch")
	require.Error(t, errUnknown)
	assert.NotEqual(t, errEmpty.Error(), errUnknown.Error(),
		"empty and unknown are different misconfigurations and must not collapse to the same message")
}

// TestRegistry_NgrokFactoryNeverHandsBackATypedNilTunnel guards the exact
// hazard AGENTS.md calls out: a Factory that returns a typed-nil *Tunnel
// produces a non-nil localtunnel.Tunnel interface (the {type, value} pair is
// {*ngrok.Tunnel, nil}, and the interface itself is non-nil), so a plain
// `if tn == nil` guard at the call site would not catch it — only a
// reflection-aware nil check (which testify's assert.NotNil performs) does.
// This constructs a Tunnel but never calls Start, so it never touches the
// network.
func TestRegistry_NgrokFactoryNeverHandsBackATypedNilTunnel(t *testing.T) {
	f, err := registry.Get("ngrok")
	require.NoError(t, err)

	tn := f(registry.Options{AuthToken: "t"})
	assert.NotNil(t, tn, "a typed-nil *ngrok.Tunnel boxed in this interface would panic on first method call")
}

func TestRegistry_NamesIsSortedAndListsRegisteredProviders(t *testing.T) {
	names := registry.Names()
	assert.Contains(t, names, "ngrok")
	assert.Contains(t, names, "stub")
	assert.True(t, sort.StringsAreSorted(names), "Names must be sorted, matching every other kind registry in this repo")
}
