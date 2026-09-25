package workshopmcp

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// newProbeTestServer builds a Server around c, scoped to the workshop
// namespace ns, with hc wired as the probe_mcp HTTP client override (nil
// leaves probe_mcp on the real SSRF-guarded default).
func newProbeTestServer(ns string, c client.Client, hc *http.Client) *Server {
	return &Server{
		K8s: c,
		Identity: WorkshopIdentity{
			Namespace:        ns,
			SessionNamespace: "b",
			SessionName:      "x",
			WorkshopID:       ns,
		},
		FieldOwner: defaultFieldOwner,
		ProbeHTTP:  hc,
	}
}

// shrinkProbePollForTest lowers awaitProbe's poll interval so a test
// exercising the poll/timeout path doesn't have to wait out
// probePollMargin's real 30s, and restores the production value on cleanup.
func shrinkProbePollForTest(t *testing.T) {
	t.Helper()
	orig := probePollInterval
	probePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { probePollInterval = orig })
}

// newProbeControllerStubClient builds a fake client that stands in for the
// plan-3a WorkshopProbe controller this package's unit tests never run: its
// Create interceptor lets the real Create land, then immediately stamps the
// given terminal status onto the just-created object via Status().Update —
// so awaitProbe's very first Get already observes a terminal phase, exactly
// as if the controller had already finished. WithStatusSubresource is
// required here (mirrors pkg/controllers/workshopprobe/prober_test.go's own
// fake-client setup): without it, the fake client's tracker treats a bare
// Status().Update as a lookup against a resource kind it never registered a
// status subresource for, and returns NotFound instead of writing.
func newProbeControllerStubClient(t *testing.T, status spiceboxv1alpha1.WorkshopProbeStatus) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.WorkshopProbe{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := cl.Create(ctx, obj, opts...); err != nil {
					return err
				}
				wp, ok := obj.(*spiceboxv1alpha1.WorkshopProbe)
				if !ok {
					return nil
				}
				wp.Status = status
				return cl.Status().Update(ctx, wp)
			},
		}).Build()
}

// TestHandleProbeMCP_ListsTools drives probe_mcp against a real go-sdk MCP
// server (mcptest, httptest-backed) with the SSRF guard overridden to
// http.DefaultClient — the only way a loopback destination is reachable in a
// test — and asserts the discovered tool set round-trips.
func TestHandleProbeMCP_ListsTools(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{
		Tools: []mcptest.Tool{
			{Name: "search_issues", Description: "find them"},
			{Name: "get_issue"},
		},
	})
	t.Cleanup(srv.Close)

	s := newProbeTestServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(), http.DefaultClient)

	res := callTool(t, s.handleProbeMCP, probeMCPArgs{URL: srv.URL})
	require.False(t, res.IsError, "a clean probe must not return an error result")
	body := decodeResultBody(t, res)
	tools, ok := body["tools"].([]any)
	require.True(t, ok, "tools must be a list")
	require.Len(t, tools, 2)
	names := make(map[string]bool, len(tools))
	for _, raw := range tools {
		m, ok := raw.(map[string]any)
		require.True(t, ok)
		names[m["name"].(string)] = true
	}
	assert.True(t, names["search_issues"], "search_issues present; got %v", names)
	assert.True(t, names["get_issue"], "get_issue present; got %v", names)
}

// TestHandleProbeMCP_RefusesLoopbackTarget is the SSRF-guard test: with NO
// HTTP override (nil — production shape), a loopback target must be refused
// by the guarded dialer as an ERROR RESULT, promptly, never a hang. Elapsed
// time (not a goroutine/select race) proves "promptly" — the guard itself
// fails fast at dial time (pkg/x/safehttp's own tests establish this), so a
// generous wall-clock ceiling is enough to catch a hang without flaking.
func TestHandleProbeMCP_RefusesLoopbackTarget(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "x"}}})
	t.Cleanup(srv.Close)

	s := newProbeTestServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(), nil)

	start := time.Now()
	res := callTool(t, s.handleProbeMCP, probeMCPArgs{URL: srv.URL})
	elapsed := time.Since(start)

	require.True(t, res.IsError, "an SSRF-blocked target must surface as an error result")
	assert.Less(t, elapsed, 10*time.Second, "an SSRF-blocked target must be refused promptly, not hang")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "probe_mcp", "error must be attributed to probe_mcp")
}

// TestHandleProbeMCP_RequiresURL pins the argument guard: an empty url is
// refused before any network call is attempted.
func TestHandleProbeMCP_RequiresURL(t *testing.T) {
	s := newProbeTestServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(), nil)
	res := callTool(t, s.handleProbeMCP, probeMCPArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "url is required")
}

// TestHandleProbeImage_SucceededStatus_ReturnsTools seeds the fake client's
// Create interceptor to stamp a terminal Succeeded status (with tools) the
// instant the WorkshopProbe lands — standing in for the plan-3a controller,
// which this package's fake client never runs — so awaitProbe's very first
// Get already finds a terminal phase.
func TestHandleProbeImage_SucceededStatus_ReturnsTools(t *testing.T) {
	c := newProbeControllerStubClient(t, spiceboxv1alpha1.WorkshopProbeStatus{
		Phase: spiceboxv1alpha1.WorkshopProbePhaseSucceeded,
		Tools: []spiceboxv1alpha1.WorkshopProbedTool{
			{Name: "search", Description: "search things"},
		},
	})
	s := newProbeTestServer("ws-demo123", c, nil)

	res := callTool(t, s.handleProbeImage, probeImageArgs{Image: "example.test/probe-target:latest"})
	require.False(t, res.IsError, "a succeeded probe must not return an error result")
	body := decodeResultBody(t, res)
	tools, ok := body["tools"].([]any)
	require.True(t, ok, "tools must be a list")
	require.Len(t, tools, 1)
	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "search", first["name"])
}

// TestHandleProbeImage_PodFailure_SurfacesAsToolError proves the "caller
// falls to the next tier" contract (design spec §7's failure table): a
// terminal Failed WorkshopProbe with podFailure must come back as a tool
// ERROR naming the failure, never as a silent empty success.
func TestHandleProbeImage_PodFailure_SurfacesAsToolError(t *testing.T) {
	c := newProbeControllerStubClient(t, spiceboxv1alpha1.WorkshopProbeStatus{
		Phase:      spiceboxv1alpha1.WorkshopProbePhaseFailed,
		PodFailure: "image pull backoff: manifest not found",
	})
	s := newProbeTestServer("ws-demo123", c, nil)

	res := callTool(t, s.handleProbeImage, probeImageArgs{Image: "example.test/does-not-exist:latest"})
	require.True(t, res.IsError, "a failed probe must be an error result")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "manifest not found")
}

// TestHandleProbeImage_RequiresImage pins the argument guard.
func TestHandleProbeImage_RequiresImage(t *testing.T) {
	s := newProbeTestServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(), nil)
	res := callTool(t, s.handleProbeImage, probeImageArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "image is required")
}

// TestHandleProbeImage_DeniedCreate_SurfacesVerbatim proves the RBAC-denial
// path: a Forbidden Create (the apiserver's own refusal) must come back as
// {denied:true, message:<verbatim>}, exactly like workshop_apply's own
// refusing test.
func TestHandleProbeImage_DeniedCreate_SurfacesVerbatim(t *testing.T) {
	denyErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "workshopprobes"},
		"", errors.New("RBAC forbids create on workshopprobes"))

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return denyErr
			},
		}).Build()
	s := newProbeTestServer("ws-demo123", c, nil)

	res := callTool(t, s.handleProbeImage, probeImageArgs{Image: "example.test/probe-target:latest"})
	require.True(t, res.IsError, "a denied create must be an error result")
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"], "denial must be flagged")
	assert.Contains(t, body["message"], "RBAC forbids create")
}

// TestHandleCLIHelp_SucceededStatus_ReturnsHelpText is probe_image's
// TestHandleProbeImage_SucceededStatus_ReturnsTools twin for cli_help's own
// status field (helpText, not tools).
func TestHandleCLIHelp_SucceededStatus_ReturnsHelpText(t *testing.T) {
	c := newProbeControllerStubClient(t, spiceboxv1alpha1.WorkshopProbeStatus{
		Phase:    spiceboxv1alpha1.WorkshopProbePhaseSucceeded,
		HelpText: "Usage: democli [OPTIONS] COMMAND",
	})
	s := newProbeTestServer("ws-demo123", c, nil)

	res := callTool(t, s.handleCLIHelp, cliHelpArgs{Image: "example.test/democli:latest", Binary: "democli", Args: []string{"sub"}})
	require.False(t, res.IsError, "a succeeded probe must not return an error result")
	body := decodeResultBody(t, res)
	assert.Equal(t, "Usage: democli [OPTIONS] COMMAND", body["help_text"])
}

// TestHandleCLIHelp_RequiresImageAndBinary pins the argument guard.
func TestHandleCLIHelp_RequiresImageAndBinary(t *testing.T) {
	s := newProbeTestServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(), nil)
	res := callTool(t, s.handleCLIHelp, cliHelpArgs{Image: "example.test/democli:latest"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "image and binary are both required")
}

// TestAwaitProbe_TimesOutWithoutTerminalPhase proves the wait-deadline path:
// a WorkshopProbe that never leaves its zero-value phase (no controller ever
// runs against this fake client) must return an error — not hang, and not
// silently report an empty success — once the deadline elapses.
func TestAwaitProbe_TimesOutWithoutTerminalPhase(t *testing.T) {
	shrinkProbePollForTest(t)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newProbeTestServer("ws-demo123", c, nil)

	wp := &spiceboxv1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "wprobe-stuck", Namespace: "ws-demo123"},
		Spec:       spiceboxv1alpha1.WorkshopProbeSpec{Image: "example.test/probe-target:latest", TimeoutSeconds: 5},
	}
	require.NoError(t, c.Create(context.Background(), wp))

	// Bound the WAIT this test does for the deadline, independent of
	// probePollMargin's real 30s constant — awaitProbe reads
	// wp.Spec.TimeoutSeconds + probePollMargin itself; this ctx just stops the
	// test from actually sitting through the full margin.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := s.awaitProbe(ctx, wp)
	require.Error(t, err, "a WorkshopProbe that never reaches a terminal phase must error, not hang or fabricate a result")
}
