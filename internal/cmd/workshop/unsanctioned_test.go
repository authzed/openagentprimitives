// unsanctioned_test.go pins the fifth agent-builder security property named
// by the design spec's §9 (plan 8a, task 4): an AgentClass that grants the
// agent_builder capability AND references the workshop SidecarToolbox, but
// carries no ClusterAgentSettings sanction, ends up with ZERO workshop_*
// tools reaching the model — not merely no Workshop CR, and not merely a
// capability that itself does nothing.
//
// pkg/agent/tool/meta/capability/agentbuilder_test.go already proves
// agent_builder is the sole sanction-gated capability and that its own
// Offer() never contributes a tool. pkg/controllers/agentsession's
// workshop_hook_test.go and sidecarpod_test.go already prove the first two
// hops of the chain:
//
//   - TestEnsureWorkshop_UnsanctionedClassCreatesNoWorkshop: no sanction ⇒ no
//     Workshop CR.
//   - TestWorkshopIdentityFor's "no Workshop at all" case: no Workshop CR ⇒
//     workshopIdentityFor returns a nil sidecar identity.
//   - TestBuildSidecarPod_NilIdentityIsByteIdenticalToBefore: a nil identity
//     ⇒ BuildSidecarPod's pod carries no workshop-sa-token volume/mount and
//     none of the WORKSHOP_*/OPERATOR_MEMORY_URL env vars.
//
// Nothing previously named the LAST hop: fed that exact (real,
// unmodified-production-code) pod's env, does THIS binary's own identity
// gate — one of three gates run() clears before constructing a Server
// (after newWorkshopK8sClient and the memory URL/token check, before ever
// calling Register in server.go) — actually fail closed? That gate
// is the ENTIRE reason an unsanctioned session's sidecar never becomes
// reachable, so the runner's probe/synthesize pass
// (pkg/agent/tool/sidecartoolbox.Synthesize, pkg/agent/runner.ProbeSynthSidecar)
// never has a live server to enumerate tools from. This file closes that
// gap: it drives the real agentsession.BuildSidecarPod with a nil identity
// (the proven consequence of "unsanctioned"), feeds the resulting pod's own
// env into this binary's real workshopIdentityFromEnv, and shows it refuses
// — meaning NewServer/Register (which would register inventory, apply,
// render_summary, validate_spec, request_credential, …) is never reached.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func TestUnsanctionedBuilderClass_GetsNoWorkshopTools(t *testing.T) {
	// --- Half 1: agent_builder's own Offer contributes nothing, granted or
	// not (mirrors pkg/agent/tool/meta/capability/agentbuilder_test.go's
	// TestAgentBuilderCapability_Offer, reproduced here so the whole security
	// property reads as one test). ---
	agentBuilder, ok := capability.Lookup("agent_builder")
	require.True(t, ok, "agent_builder must be registered")
	capTools, skip := agentBuilder.Offer(capability.OfferContext{Granted: true})
	assert.Empty(t, capTools, "granting agent_builder must never itself contribute a tool")
	require.NotNil(t, skip, "a granted-but-inert capability must log a skip reason")
	assert.Equal(t, "agent_builder", skip.Capability)

	// --- Half 2: the class ALSO references the workshop SidecarToolbox, with
	// no ClusterAgentSettings sanction. Reproduce (not just cite) the
	// nil-identity pod ensureWorkshop's own tests prove this combination
	// yields — this test needs the pod's real env for the next link. ---
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: types.UID("sess-uid-1")},
	}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		// Name is the LLM-facing prefix an AgentClass author chose for their
		// SidecarToolboxRef — "workshop" is what makes a synthesized tool's
		// name "workshop_inventory", "workshop_apply", etc.
		Name: "workshop",
		Ref:  "workshop-toolbox",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/workshop:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Port: 18080},
			// The real tool names workshopmcp's Register wires (server.go), so
			// "sanctioned would synthesize something real" isn't a fixture
			// artifact — these literals mirror pkg/tools/workshopmcp's own
			// unexported toolInventory/toolWorkshopApply/toolRenderSummary/
			// toolValidateSpec/toolRequestCredential/toolCloseOthers constants
			// (tools_inventory.go, tools_apply.go, tools_render.go,
			// tools_validate.go, tools_credential.go, tools_close.go). This
			// file stays in package main (it tests workshopIdentityFromEnv,
			// which stays in main.go) and so cannot import those unexported
			// constants from workshopmcp — workshopmcp's
			// exported surface is deliberately limited to
			// Server/NewServer/Register/WorkshopIdentity, so the literals are
			// spelled out here instead of gaining a fifth export just for this
			// test.
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "inventory"},
				{Name: "apply"},
				{Name: "render_summary"},
				{Name: "validate_spec"},
				{Name: "request_credential"},
				{Name: "close_others"},
			},
		},
	}

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "", nil,
		"http://spicebox-operator.agentprimitives-system.svc:8082", "ws-abc123def456", "registry.example.test")
	require.NoError(t, err)
	assert.Empty(t, pod.Spec.ServiceAccountName, "no SA name without a sanction")
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, "workshop-sa-token", v.Name, "no workshop SA-token volume without a sanction")
	}
	require.Len(t, pod.Spec.Containers, 1)
	podEnv := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		podEnv[e.Name] = e.Value
	}
	for _, name := range []string{"WORKSHOP_NAMESPACE", "WORKSHOP_SESSION_NAMESPACE", "WORKSHOP_SESSION_NAME", "WORKSHOP_ID"} {
		_, present := podEnv[name]
		assert.False(t, present, "an unsanctioned pod must carry no %s", name)
	}

	// --- Half 3: feed that EXACT (absent) env into this binary's own
	// identity gate — the previously-untested link. run() resolves this
	// before ever constructing a Server or calling Register, so a failure
	// here means the MCP server that would expose workshop_* tools is never
	// built at all. ---
	for _, name := range []string{"WORKSHOP_NAMESPACE", "WORKSHOP_SESSION_NAMESPACE", "WORKSHOP_SESSION_NAME", "WORKSHOP_ID"} {
		v, present := podEnv[name]
		if !present {
			v = "" // matches the pod's real (absent) env exactly
		}
		t.Setenv(name, v)
	}
	_, err = workshopIdentityFromEnv()
	require.Error(t, err, "an unsanctioned pod's env must fail workshopIdentityFromEnv — the gate run() checks before ever registering a tool")
	assert.Contains(t, err.Error(), "WORKSHOP_NAMESPACE")

	// --- Positive control: had this class been sanctioned, the SAME
	// allowlist synthesizes real, workshop_-prefixed tools — so the zero
	// below is a withheld grant, not a fixture that could never produce a
	// tool in the first place. ---
	wantNames := sidecartoolbox.LLMToolNames(rt)
	require.NotEmpty(t, wantNames, "a sanctioned class's allowlist synthesizes real tools")
	for _, n := range wantNames {
		assert.True(t, strings.HasPrefix(n, "workshop_"), "sanctioned names carry the class-chosen prefix: %q", n)
	}

	// --- And the runner's own synthesis, given the sidecar was never
	// reachable (guaranteed by the identity gate above — run() never gets
	// far enough to listen on any port), returns ZERO tools. Not a partial
	// or degraded set: with an empty probed tool list (`live=nil`, standing
	// in for a server nobody ever dialed), synthesis refuses rather than
	// returning a partial set — sidecartoolbox.Synthesize never dials
	// anything itself; it just looks up each allowlisted name in `live`
	// (pkg/agent/tool/mcp/synthesize.go's byName lookup) and errors the
	// instant one is missing. ---
	got, serr := sidecartoolbox.Synthesize(rt, nil, nil)
	require.Error(t, serr, "an empty probed tool list must fail synthesis, not silently return fewer tools")
	assert.Empty(t, got, "zero workshop_* tools ever reach the model")

	// --- The genuinely-unreachable case, named separately because it is a
	// different code path: runner.ProbeSynthSidecar is what actually DIALS a
	// sidecar (Synthesize above never does), so this is the one place that
	// can show a real dial failure — not just an empty allowlist lookup —
	// also fails closed. ---
	erroringProber := func(context.Context, string) ([]mcpprobe.Tool, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	builtTools, perr := runner.ProbeSynthSidecar(context.Background(), "http://unreachable.invalid:18080", rt, erroringProber, nil, nil, nil, nil)
	require.Error(t, perr, "a sidecar that never answers must fail the probe/synth pass, not return an empty-but-successful result")
	assert.Nil(t, builtTools, "a failed probe must return no tools, not a partial set")
}
