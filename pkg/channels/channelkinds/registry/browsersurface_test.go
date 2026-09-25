package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestIsBrowserSurface is table-driven over registry.All() rather than a
// hardcoded list, so a kind added later is covered without editing this
// test.
func TestIsBrowserSurface(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"the kind registry must be populated by init — an empty registry would "+
			"make the 'every other kind reports false' row vacuously true")

	t.Run("browser reports true", func(t *testing.T) {
		assert.True(t, registry.IsBrowserSurface(browser.KindName),
			"a session bound to the browser kind is, by definition, already being "+
				"read on webd's own trusted origin")
	})

	for _, k := range kinds {
		if k.Name() == browser.KindName {
			continue
		}
		t.Run(k.Name()+" reports false", func(t *testing.T) {
			assert.False(t, registry.IsBrowserSurface(k.Name()),
				"kind %q does not implement channelkinds.BrowserSurface; the safe "+
					"default is false", k.Name())
		})
	}

	t.Run("an unregistered name reports false, not a panic", func(t *testing.T) {
		assert.False(t, registry.IsBrowserSurface("no-such-kind"))
	})

	t.Run("the empty name reports false", func(t *testing.T) {
		assert.False(t, registry.IsBrowserSurface(""))
	})
}
