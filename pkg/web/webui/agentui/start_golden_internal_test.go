package agentui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startGoldenPath is the SHARED start-response golden — one file, read by
// three parties: this test (agentui's producer), pkg/web/webui/sessions' own
// golden test (the other producer) and SessionShell.test.tsx (the browser's
// single parse helper). It lives beside the browser code that reads it
// because the TS suite can only reach files under its own package tree, and
// nothing stops a Go test from reaching across.
//
// The point of pinning BOTH producers against one file is that the browser
// has exactly one parse helper for two routes: if either producer renames a
// key or changes the href shape, that helper silently yields undefined for
// one of them and the navigation dead-ends. A per-package golden would let
// the two drift apart and both stay green.
const startGoldenPath = "../sessions/ui/testdata/start.golden.json"

// TestStartResponseMatchesTheSharedGolden asserts this package's
// startResponse marshals to exactly the shared golden's shape — keys AND the
// href format, since SessionShellHref is what both producers call.
func TestStartResponseMatchesTheSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(startGoldenPath))
	require.NoError(t, err, "the shared start-response golden must exist at %s", startGoldenPath)

	var want map[string]any
	require.NoError(t, json.Unmarshal(raw, &want))

	ns, _ := want["ns"].(string)
	name, _ := want["name"].(string)
	require.NotEmpty(t, ns, "the golden must carry a non-empty ns")
	require.NotEmpty(t, name, "the golden must carry a non-empty name")

	got, err := json.Marshal(startResponse{Ns: ns, Name: name, Href: SessionShellHref(ns, name)})
	require.NoError(t, err)

	var gotMap map[string]any
	require.NoError(t, json.Unmarshal(got, &gotMap))
	assert.Equal(t, want, gotMap,
		"startResponse's wire shape must match the shared golden; regenerate the golden only when BOTH producers and the browser's parse helper change together")
}
