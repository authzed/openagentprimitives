//go:build e2e

package bronzethread_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e/threadrun"
)

// TestBronzethread discovers every bundle under testdata/ and runs it.
//
// A new scenario is a directory, never a new Go function: the point of the
// bundle format is that scenarios are data, so they can later be CAPTURED
// rather than authored without the driver changing.
func TestBronzethread(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	require.NoError(t, err)

	var found, completed int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("testdata", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "bundle.json")); err != nil {
			continue // a shared agent fixture, not a scenario
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			threadrun.Run(t, dir, threadrun.Load(t, dir))
			completed++
		})
	}
	require.NotZero(t, found, "no bundles found; the suite would pass vacuously")

	// A SKIPPED bundle exits its subtest via Goexit, so it never reaches the
	// counter. Without this the whole suite can skip — on a missing envtest
	// asset, say — and still exit 0, which is the precise failure mode these
	// tests exist to catch in the product. A harness allowed to lie about
	// having run is worse than no harness.
	require.NotZero(t, completed,
		"every bundle skipped: %d found, 0 ran. The suite proved nothing but exited green", found)
}
