package webassets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManifestHasSystemAndHealth(t *testing.T) {
	m, err := Manifest()
	require.NoError(t, err, "parse embedded manifest")
	assert.Contains(t, m, "system", "system app must be built")
	assert.Contains(t, m, "health", "health prove-it app must be built")
	require.NotEmpty(t, m["system"].Scripts, "system app must have a script")
}

func TestAssetFilesExist(t *testing.T) {
	m, err := Manifest()
	require.NoError(t, err)
	for app, e := range m {
		for _, s := range e.Scripts {
			_, rerr := FS().Open("dist" + trimAssetsPrefix(s))
			assert.NoError(t, rerr, "script for %q must exist: %s", app, s)
		}
	}
}
