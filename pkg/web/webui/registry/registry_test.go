package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type fakeUI struct{ name string }

func (f fakeUI) Name() string                  { return f.name }
func (fakeUI) Routes(webui.Deps) []webui.Route { return nil }

func TestRegistry_RegisterAndAllSorted(t *testing.T) {
	registry.Reset()
	registry.Register(fakeUI{name: "zebra"})
	registry.Register(fakeUI{name: "alpha"})
	all := registry.All()
	require.Len(t, all, 2)
	assert.Equal(t, "alpha", all[0].Name(), "All must be sorted by Name")
	assert.Equal(t, "zebra", all[1].Name())
}

func TestRegistry_PanicsOnDuplicate(t *testing.T) {
	registry.Reset()
	registry.Register(fakeUI{name: "dup"})
	assert.PanicsWithValue(t, `webui.Register: duplicate WebUI name "dup"`, func() {
		registry.Register(fakeUI{name: "dup"})
	})
}

func TestRegistry_PanicsOnEmptyName(t *testing.T) {
	registry.Reset()
	assert.Panics(t, func() { registry.Register(fakeUI{name: ""}) })
}
