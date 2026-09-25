package gatewayhealth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeProvisioner is a named no-op Provisioner used to exercise the registry in
// isolation (the real GKE provisioner lives in package main and isn't linked
// here). The name field lets a test assert which provisioner For returned.
type fakeProvisioner struct{ name string }

func (fakeProvisioner) Provision(context.Context, Params) error { return nil }

// resetRegistry clears the package-global registry so each test starts clean.
// The real registry is populated by this package's init() (the no-op default)
// plus package main's init()s, never by this test binary, so resetting here is
// safe.
func resetRegistry(t *testing.T) {
	t.Helper()
	registry.byCloud = map[string]Provisioner{}
	registry.def = nil
}

func TestFor_DispatchesByCloudWithDefaultFallback(t *testing.T) {
	resetRegistry(t)
	def := fakeProvisioner{name: "default"}
	gke := fakeProvisioner{name: "gke"}
	Register(def)        // no clouds → default
	Register(gke, "gke") // claims gke

	got, ok := For("gke").(fakeProvisioner)
	assert.True(t, ok, `For("gke") must return the registered GKE provisioner`)
	assert.Equal(t, "gke", got.name)

	for _, cloud := range []string{"eks", "aks", "unknown"} {
		d, ok := For(cloud).(fakeProvisioner)
		assert.True(t, ok, "an unclaimed cloud (%s) falls back to the default", cloud)
		assert.Equal(t, "default", d.name, "an unclaimed cloud (%s) falls back to the default", cloud)
	}
}

func TestFor_NeverNilWithDefaultRegistered(t *testing.T) {
	// With a default registered, an unclaimed cloud still gets it — proving For
	// is never nil so the consumer needs no nil check.
	resetRegistry(t)
	Register(fakeProvisioner{name: "default"})
	assert.NotNil(t, For("eks"), "default registered → For is never nil for an unclaimed cloud")
}

func TestRegister_PanicsOnDuplicateCloud(t *testing.T) {
	resetRegistry(t)
	Register(fakeProvisioner{name: "a"}, "gke")
	assert.PanicsWithValue(t,
		`gatewayhealth: cloud "gke" already registered to gatewayhealth.fakeProvisioner; cannot also register gatewayhealth.fakeProvisioner`,
		func() { Register(fakeProvisioner{name: "b"}, "gke") })
}

func TestRegister_PanicsOnSecondDefault(t *testing.T) {
	resetRegistry(t)
	Register(fakeProvisioner{name: "a"})
	assert.PanicsWithValue(t,
		"gatewayhealth: default provisioner already registered (gatewayhealth.fakeProvisioner); cannot also register gatewayhealth.fakeProvisioner as default",
		func() { Register(fakeProvisioner{name: "b"}) })
}

func TestRegister_PanicsOnNil(t *testing.T) {
	resetRegistry(t)
	assert.PanicsWithValue(t, "gatewayhealth: Register(nil)", func() { Register(nil) })
}

// TestDefaultProvisionerIsNoop confirms the package's own default (registered in
// init) is the no-op: a default-only registry returns a Provisioner whose
// Provision is a clean no-op (non-GKE Envoy Gateways need nothing).
func TestDefaultProvisionerIsNoop(t *testing.T) {
	resetRegistry(t)
	Register(noopProvisioner{})
	assert.NoError(t, For("eks").Provision(context.Background(), Params{}))
}
