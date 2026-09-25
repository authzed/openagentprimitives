package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

type demoProps struct {
	Label string `json:"label"`
}

// register installs c for the duration of the test and restores the registry
// afterwards, so a test that registers a fixture cannot leak into its siblings
// or into the platform registrations made in init().
func register(t *testing.T, c uicomponents.Component) {
	t.Helper()
	prior := registry.All()
	t.Cleanup(func() {
		registry.Reset()
		for _, p := range prior {
			registry.Register(p)
		}
	})
	registry.Register(c)
}

func TestRegisterThenGetReturnsTheComponent(t *testing.T) {
	c := uicomponents.Component{Type: "ap:demo", Props: demoProps{}}
	register(t, c)

	got, ok := registry.Get("ap:demo")
	require.True(t, ok, "a registered type must be retrievable")
	assert.Equal(t, "ap:demo", got.Type)
}

func TestGetUnknownTypeReportsNotFound(t *testing.T) {
	_, ok := registry.Get("ap:does-not-exist")
	assert.False(t, ok, "an unregistered type must not resolve")
}

func TestRegisterDuplicateTypePanics(t *testing.T) {
	c := uicomponents.Component{Type: "ap:demo", Props: demoProps{}}
	register(t, c)

	assert.Panics(t, func() {
		registry.Register(uicomponents.Component{Type: "ap:demo", Props: demoProps{}})
	}, "a duplicate registration is a programmer error and must panic at init time")
}

func TestKeyReturnsTheType(t *testing.T) {
	c := uicomponents.Component{Type: "ap:demo", Props: demoProps{}}
	assert.Equal(t, "ap:demo", c.Key())
}
