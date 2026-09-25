package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

// TestRegenerateOriginsGoldenScratch is a scratch generator, not part of
// this package's real coverage: it writes ui/testdata/origins.golden.json
// FROM agentui.AllBranches' current value, exactly the way
// pkg/web/webui/agentui's own TestRegenerateGoldensScratch
// (props_golden_internal_test.go) regenerates its three goldens. Gated
// behind REGEN_GOLDEN so it never runs as part of the normal suite; run it
// with `REGEN_GOLDEN=1 go test ./pkg/web/webui/sessions/... -run
// TestRegenerateOriginsGoldenScratch` after adding or reordering a Branch in
// agentui.AllBranches, then re-check TestOriginsGolden_PinsAllBranches
// (view_test.go) still passes and commit the regenerated file.
func TestRegenerateOriginsGoldenScratch(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") == "" {
		t.Skip("set REGEN_GOLDEN=1 to regenerate")
	}

	branches := make([]string, len(agentui.AllBranches))
	for i, b := range agentui.AllBranches {
		branches[i] = string(b)
	}

	raw, err := json.Marshal(branches)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, raw, "", "  "))
	buf.WriteByte('\n')
	require.NoError(t, os.WriteFile(goldenOriginsPath, buf.Bytes(), 0o644))
}
