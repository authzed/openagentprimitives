package workshopmcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeclarationOnly_ServesListRefusesEveryCall pins the admission-probe
// contract that lets the shipped ap-workshop image pass the SidecarToolbox
// reachability probe: buildProbePod mounts no workshop identity (no projected
// SA token, no operator bearer, no WORKSHOP_* env) and the probe only ever
// asks tools/list — so a Server built by NewDeclarationOnly must
//
//   - announce EXACTLY the same tool set the fully-wired server announces
//     (the probe records this set; a declaration-only server that hid or
//     added tools would make admission lie about the session surface), and
//   - refuse EVERY tool call with a structured error naming the mode — the
//     fail-closed guarantee moves from "refuse to boot" to "refuse to act",
//     it does not disappear.
//
// The calls go over a real Streamable-HTTP round trip, NOT via s.handleX:
// the refusal is receiving middleware, and a direct handler call would
// bypass it — passing forever regardless of whether the guard exists.
func TestDeclarationOnly_ServesListRefusesEveryCall(t *testing.T) {
	declSrv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	NewDeclarationOnly().Register(declSrv)
	decl := newTestMux(t, declSrv)

	fullSrv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	newTestWorkshopServer(t).Register(fullSrv)
	full := newTestMux(t, fullSrv)

	declNames := listToolNames(t, decl)
	assert.ElementsMatch(t, listToolNames(t, full), declNames,
		"declaration-only must announce exactly the fully-wired tool set — admission records this list")

	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: decl.URL + "/mcp", HTTPClient: decl.Client()}, nil)
	require.NoError(t, err)
	defer sess.Close()

	for _, name := range declNames {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		require.NoError(t, err, "%s: the refusal must be a tool RESULT, not a protocol error", name)
		require.True(t, res.IsError, "%s: a declaration-only server must refuse every call", name)
		require.Len(t, res.Content, 1, "%s", name)
		text, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok, "%s", name)
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(text.Text), &body), "%s: refusal must be structured", name)
		assert.Contains(t, body["error"], "declaration-only",
			"%s: the refusal must name the mode so the operator reading a transcript knows WHY", name)
	}
}
