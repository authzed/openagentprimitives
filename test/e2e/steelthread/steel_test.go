//go:build e2e

package steelthread_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e/threadrun"
)

// TestSteelthread discovers every CAPTURED bundle under testdata/ and runs it.
//
// Identical in shape to TestBronzethread, and deliberately a separate suite
// rather than a shared testdata tree: a green steel bundle is evidence a real
// model produced the interaction, which a bronze bundle explicitly is not.
// Keeping them apart is what lets that difference be reported rather than
// merely commented.
//
// An EMPTY testdata/ is not a failure here, unlike bronze. A capture is taken
// from a live cluster, so a fresh clone legitimately has none — but a suite
// that found bundles and ran none is the same lie it is anywhere else.
func TestSteelthread(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	require.NoError(t, err)

	var found, completed int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("testdata", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "bundle.json")); err != nil {
			continue
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			threadrun.Run(t, dir, threadrun.Load(t, dir))
			completed++
		})
	}
	if found == 0 {
		t.Skip("no captured bundles; run `oap session capture` against a live session to add one")
	}
	require.NotZero(t, completed,
		"every bundle skipped: %d found, 0 ran. The suite proved nothing but exited green", found)
}
