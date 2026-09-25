package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// TestRoundTrip_RealSynthesisPathServesExactlyConfiguredOperations proves
// ap-api-adapter end to end through the REAL runner-side synthesis path:
// a config parsed by the real apiadapter.Parse, served as MCP by this
// binary's own Server (Task 7a-2), discovered live via pkg/tools/mcp/probe
// (the same client the runner uses to build a ResolvedSidecarToolbox's
// `live` set), synthesized into agent tools by sidecartoolbox.Synthesize,
// and dispatched by calling one of those tools exactly as the runner would.
//
// # The loopback trap, and why this test is split honestly
//
// Pointing baseURL at the upstream httptest.Server below — so a tools/call
// could be proven to reach it — is impossible: the engine's production
// client is safehttp.Client(), whose seam vars newHTTPClient/guardHost are
// unexported in pkg/tools/apiadapter and so unreachable from package main.
// There is an EARLIER wall too: apiadapter.Config.Validate — which
// apiadapter.Parse always runs — calls safehttp.GuardHost on baseURL itself,
// and httptest.Server always binds 127.0.0.1. So a config whose baseURL is
// the upstream server's own URL fails to PARSE at all (confirmed
// empirically: Parse returns `apiadapter: baseURL host: safehttp: host
// "127.0.0.1" is a blocked (private/loopback/link-local) address` for
// exactly that config). There is no way to get a Parse-approved Config whose
// baseURL both (a) passes the SSRF guard's static check and (b) actually
// dials the loopback upstream below — that is the guard working as
// designed, not a gap in this test.
//
// So this test proves what it can reach, split at the true boundary:
//
//  1. The whole MCP half, end to end: Synthesize's advertised tool set is
//     exactly the config's two operations (would fail on a dropped, renamed,
//     or extra tool), and a tools/call for one of them flows all the way
//     through Register's handler into the REAL engine (buildRequest, auth
//     injection, the guarded client) — proven by the returned tool error
//     naming the operation AND the safehttp package that produced it, which
//     could only appear if the call actually reached e.client.Do. The
//     upstream recorder below is asserted NEVER hit, ruling out an
//     accidental fallback path silently answering instead.
//  2. The final dial — the one leg that needs a config the guard would
//     actually let through to a live loopback server — is NOT exercised
//     here; it is covered by pkg/tools/apiadapter's own executor tests
//     (execute_test.go), which legitimately swap newHTTPClient/guardHost
//     because they live inside the package that owns the seam. Together the
//     two suites cover the whole path from config to upstream response; ONE
//     package deliberately keeps the SSRF guard un-bypassable from outside
//     it, and this test does not attempt to work around that.
func TestRoundTrip_RealSynthesisPathServesExactlyConfiguredOperations(t *testing.T) {
	// The upstream API. Recorded so this test can assert it is NEVER reached
	// — see the doc comment above: the config below cannot legally name it,
	// so a hit here would mean some other, unintended path reached it.
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	// The config: parsed by the REAL apiadapter.Parse. baseURL is a
	// fixture-grade ".test" hostname — not upstream.URL, which Parse would
	// reject outright (see the doc comment above) — declaring two operations:
	// one GET with a path param, one POST with a body param.
	cfg, err := apiadapter.Parse([]byte(`
baseURL: https://widgets.example.test
auth: {type: header, envVar: WIDGETS_API_KEY, name: X-Api-Key}
operations:
  - name: get_widget
    description: Fetch one widget.
    method: GET
    path: "/widgets/{id}"
    params:
      - {name: id, in: path, required: true, type: string}
  - name: create_widget
    description: Create a widget.
    method: POST
    path: /widgets
    params:
      - {name: name, in: body, required: true, type: string}
`))
	require.NoError(t, err, "apiadapter.Parse")

	// The adapter: this binary's own Server, wired exactly as main.go wires
	// it, served over a real httptest MCP endpoint. newTestMux is
	// server_test.go's helper — same package, same mux shape a real request
	// would see.
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-api-adapter", Version: "test"}, nil)
	NewServer(cfg, "test-widgets-key").Register(mcpSrv)
	adapterSrv := newTestMux(t, mcpSrv, "/mcp")

	// Discover the adapter's live tool surface exactly as the runner does:
	// pkg/tools/mcp/probe.Client, with HTTP set explicitly to a
	// loopback-permitting client — the probe package's own doc comment says
	// only tests and operator-controlled reach paths may do that.
	live, err := (&probe.Client{HTTP: adapterSrv.Client(), URL: adapterSrv.URL + "/mcp"}).ListTools(context.Background(), "", "")
	require.NoError(t, err, "probe.Client.ListTools against the adapter")

	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "widgets",
		Ref:  "widgets",
		Port: adapterPort(t, adapterSrv.URL),
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:      "widgets",
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Path: "/mcp"},
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{
					Name:       "get_widget",
					Args:       spiceboxv1alpha1.MCPServerToolArgs{AllowedFields: []string{"id"}},
					Permission: &authz.Permission{StateImpact: authz.Passthrough},
				},
				{
					Name:       "create_widget",
					Args:       spiceboxv1alpha1.MCPServerToolArgs{AllowedFields: []string{"name"}},
					Permission: &authz.Permission{StateImpact: authz.Passthrough},
				},
			},
		},
	}

	tools, err := sidecartoolbox.Synthesize(rt, live, nil)
	require.NoError(t, err, "sidecartoolbox.Synthesize")

	// Assertion 1: the advertised set is EXACTLY the config's operations,
	// prefixed by the sidecar handle — fails on a dropped, renamed, or extra
	// tool.
	wantNames := []string{"widgets_get_widget", "widgets_create_widget"}
	gotNames := make([]string, len(tools))
	for i, tl := range tools {
		gotNames[i] = tl.Name()
	}
	assert.ElementsMatch(t, wantNames, gotNames, "Synthesize must offer exactly the config's operations, nothing more or less")

	var getWidget agenttool.Tool
	for _, tl := range tools {
		if tl.Name() == "widgets_get_widget" {
			getWidget = tl
		}
	}
	require.NotNil(t, getWidget, "widgets_get_widget must be present to call it")

	// Assertion 2: a tools/call reaches the adapter's REAL handler and
	// dispatches into the REAL engine. It comes back as a tool error naming
	// both the operation and the safehttp guard that produced it — proof the
	// request was built (path param bound, auth header injected) and
	// dispatched all the way to the guarded client, stopping only at the
	// final dial (see the doc comment above for why that leg can't be
	// exercised from this package).
	reg := operations.New(nil, nil)
	op := reg.Begin("roundtrip-test")
	sess := &agenttool.SessionContext{Operations: reg}
	envelope, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "round-trip test",
		"args":         map[string]any{"id": "w-1"},
	})
	require.NoError(t, err)

	res, err := getWidget.Execute(context.Background(), envelope, sess)
	require.NoError(t, err, "Execute must not return a bare Go error — every engine failure comes back as a tool result")
	assert.True(t, res.IsError, "expected the real safehttp-guarded dial to fail for an unresolvable .test host")
	assert.Contains(t, res.Content, "get_widget", "the tool error must name the operation that was dispatched")
	assert.Contains(t, res.Content, "safehttp:", "the tool error must come from the real safehttp guard, proving dispatch reached it")
	assert.Equal(t, int32(0), upstreamHits.Load(), "the httptest upstream must never be reached — the config cannot legally name it (see doc comment)")

	// Assertion 3: a tool NOT in the config is absent from the adapter's own
	// surface — not merely unlisted, but unreachable. Calling it directly
	// (bypassing Synthesize's own allowlist) proves Register wired only the
	// configured operations, nothing hand-written or left over.
	_, err = probe.CallTool(context.Background(), adapterSrv.URL+"/mcp", adapterSrv.Client(),
		"delete_widget", map[string]any{}, "", "", nil)
	assert.Error(t, err, "a tool absent from the config must be absent from the adapter's MCP surface")
}

// adapterPort extracts the port httptest bound srv's URL to; httptest
// always binds 127.0.0.1, mirroring
// pkg/agent/tool/sidecartoolbox/synthesize_test.go's own extraction.
func adapterPort(t *testing.T, url string) int32 {
	t.Helper()
	var port int32
	if _, err := fmt.Sscanf(url, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("could not parse port from %q: %v", url, err)
	}
	return port
}
