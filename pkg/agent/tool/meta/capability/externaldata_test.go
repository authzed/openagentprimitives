package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	websearchregistry "github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
)

// stubBackend is a minimal websearch.Backend used only to flip the registry
// non-empty for one test. Never a real Provider: New is never called by
// this capability (Offer never dispatches — see externaldata.go's type doc).
type stubBackend struct{ name string }

func (s stubBackend) Name() string                                   { return s.name }
func (s stubBackend) New(websearch.Deps) (websearch.Provider, error) { return nil, nil }

func TestExternalData_OptInDefaultOff(t *testing.T) {
	c, ok := Lookup("external_data")
	require.True(t, ok, "external_data must be registered")
	assert.Equal(t, "external_data", c.Name())
	assert.False(t, c.DefaultOn(), "a class must opt in explicitly")
	assert.False(t, c.Infrastructural())
}

// TestExternalData_RegistryEmptyIsTheDesignedRunnerState pins the fact this
// capability's Offer relies on: in a real runner build today, no test in
// this file or elsewhere in this package's dependency graph blank-imports a
// websearch backend (e.g. pkg/tools/websearch/bravesearch), by design — R2
// keeps that code and its credential out of the runner process entirely. If
// a future change DOES add such an import (to this package, or anything it
// imports), this test starts failing and forces that change to reconcile
// itself with externaldata.go's Offer doc comment rather than silently
// changing this capability's oldest, most common SkipReason.
func TestExternalData_RegistryEmptyIsTheDesignedRunnerState(t *testing.T) {
	assert.Empty(t, websearchregistry.Keys(),
		"no websearch backend should be registered in this process — see externaldata.go's Offer doc comment")
}

func TestExternalData_Offer_Ungranted(t *testing.T) {
	c, _ := Lookup("external_data")
	tools, skip := c.Offer(OfferContext{Granted: false})
	assert.Nil(t, tools)
	assert.Nil(t, skip, "ungranted: no tool, no skip reason")
}

// TestExternalData_Offer_GrantedNoBackendRegistered is the production-shaped
// case: granted, and (per the test above) no backend registered in this
// process. Offer must never contribute a tool — search/fetch ship via the
// sidecar reference, never through this capability — and must name the
// specific reason a builder can act on.
func TestExternalData_Offer_GrantedNoBackendRegistered(t *testing.T) {
	require.Empty(t, websearchregistry.Keys(), "precondition: registry must be empty")

	c, _ := Lookup("external_data")
	tools, skip := c.Offer(OfferContext{Granted: true})
	assert.Nil(t, tools, "granting the capability alone never contributes a tool")
	require.NotNil(t, skip)
	assert.Equal(t, "external_data", skip.Capability)
	assert.Contains(t, skip.Reason, "no websearch backend is registered")
	assert.Contains(t, skip.Reason, "ap-websearchd")
}

// TestExternalData_Offer_GrantedBackendRegistered proves the SkipReason
// changes to the sidecar-attachment explanation once a backend IS
// registered in-process — still never contributing a tool, because Offer
// structurally cannot (see externaldata.go's type doc: NonMetaTools is
// appended by Assemble unconditionally, ungated by any capability).
func TestExternalData_Offer_GrantedBackendRegistered(t *testing.T) {
	websearchregistry.Register(stubBackend{name: "stub-for-external-data-test"})
	t.Cleanup(websearchregistry.Reset)

	c, _ := Lookup("external_data")
	tools, skip := c.Offer(OfferContext{Granted: true})
	assert.Nil(t, tools, "granting the capability alone never contributes a tool")
	require.NotNil(t, skip)
	assert.Equal(t, "external_data", skip.Capability)
	assert.Contains(t, skip.Reason, "ship with the ap-websearchd sidecar")
	assert.NotContains(t, skip.Reason, "no websearch backend is registered",
		"the reason must be the MORE SPECIFIC one once a backend is actually registered")
}

func TestExternalData_ParseConfig(t *testing.T) {
	c, _ := Lookup("external_data")

	cfg, err := c.ParseConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, externalDataConfig{}, cfg)
	assert.True(t, cfg.(externalDataConfig).FetchEnabled(), "R3: the URL space is open by default")

	cfg, err = c.ParseConfig(json.RawMessage(`{"fetch": false}`))
	require.NoError(t, err)
	edc, ok := cfg.(externalDataConfig)
	require.True(t, ok)
	assert.False(t, edc.FetchEnabled(), `explicit "fetch": false narrows to search-only`)

	cfg, err = c.ParseConfig(json.RawMessage(`{"allowedDomains": ["example.test", "docs.example.test"]}`))
	require.NoError(t, err)
	edc, ok = cfg.(externalDataConfig)
	require.True(t, ok)
	assert.Equal(t, []string{"example.test", "docs.example.test"}, edc.AllowedDomains)
	assert.True(t, edc.FetchEnabled(), "allowedDomains alone must not implicitly disable fetch")

	_, err = c.ParseConfig(json.RawMessage(`{"fetch": "not-a-bool"}`))
	assert.Error(t, err)
}

func TestExternalData_RegisteredAndValidates(t *testing.T) {
	require.NoError(t, ValidateGrant("external_data", json.RawMessage(`{}`)))
	require.NoError(t, ValidateGrant("external_data", json.RawMessage(`{"fetch": false}`)))
	require.NoError(t, ValidateGrant("external_data", json.RawMessage(`{"allowedDomains": ["a.test"]}`)))
	require.Error(t, ValidateGrant("external_data", json.RawMessage(`{"fetch": "nope"}`)))
}

// TestExternalDataConfig_FetchEnabled_NilIsOpen pins R3 directly against
// the zero value, independent of ParseConfig's own JSON round trip.
func TestExternalDataConfig_FetchEnabled_NilIsOpen(t *testing.T) {
	assert.True(t, externalDataConfig{}.FetchEnabled())
	no := false
	assert.False(t, externalDataConfig{Fetch: &no}.FetchEnabled())
	yes := true
	assert.True(t, externalDataConfig{Fetch: &yes}.FetchEnabled())
}
