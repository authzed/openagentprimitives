package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcpdispatch "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// providerWithAuthFailure / providerWithoutAuthFailure are IDs from the
// embedded catalog. If a future catalog edit gives the second one an
// authFailure: block, the negative cases below start passing vacuously — hence
// the preconditions asserted in the first subtest.
const (
	providerWithAuthFailure    = "github-pat"
	providerWithoutAuthFailure = "kubectl-kubeconfig"
)

func TestAuthFailureOriginsRecordsOnlyDeclaredShapes(t *testing.T) {
	t.Run("catalog preconditions the negative cases depend on", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		af.RecordProvider("probe/a", providerWithAuthFailure)
		af.RecordProvider("probe/b", providerWithoutAuthFailure)
		require.NotNil(t, af.Lookup("probe/a"), "%s must declare an authFailure block", providerWithAuthFailure)
		require.Nil(t, af.Lookup("probe/b"), "%s must NOT declare one", providerWithoutAuthFailure)
	})

	t.Run("a provider declaring a block is recorded under its origin", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		assert.True(t, af.RecordProvider("sidecartoolbox/box", providerWithAuthFailure))
		assert.NotNil(t, af.Lookup("sidecartoolbox/box"))
	})

	t.Run("a provider declaring NO block leaves the origin absent", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		assert.False(t, af.RecordProvider("toolkit/kubectl", providerWithoutAuthFailure))
		assert.Nil(t, af.Lookup("toolkit/kubectl"),
			"absent means no corroboration available, which is what the recorder must see")
	})

	t.Run("an unknown provider ID leaves the origin absent", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		assert.False(t, af.RecordProvider("toolkit/ghost", "no-such-provider"))
		assert.Nil(t, af.Lookup("toolkit/ghost"))
	})

	t.Run("an EMPTY origin never becomes a key", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		assert.False(t, af.RecordProvider("", providerWithAuthFailure))
		assert.Empty(t, af, "Origin() returns \"\" for a tool with no managed credential behind it")
	})

	t.Run("an empty provider ID leaves the origin absent", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		assert.False(t, af.RecordProvider("toolkit/gh", ""))
		assert.Empty(t, af)
	})

	t.Run("requirements: the FIRST provider-backed requirement wins", func(t *testing.T) {
		// Order must not decide the shape: a later requirement may not overwrite
		// an earlier one, or the answer depends on slice order no caller controls.
		af := NewAuthFailureOrigins()
		af.RecordRequirements("toolkit/gh", []authkind.CredentialRequirement{
			{ProviderID: providerWithAuthFailure},
			{ProviderID: providerWithoutAuthFailure},
		})
		first := af.Lookup("toolkit/gh")
		require.NotNil(t, first)

		af2 := NewAuthFailureOrigins()
		af2.RecordRequirements("toolkit/gh", []authkind.CredentialRequirement{
			{ProviderID: providerWithoutAuthFailure},
			{ProviderID: providerWithAuthFailure},
		})
		assert.NotNil(t, af2.Lookup("toolkit/gh"),
			"a requirement whose provider declares nothing must not consume the origin's one slot")
	})

	t.Run("requirements with no ProviderID leave the origin absent", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		af.RecordRequirements("toolkit/gh", []authkind.CredentialRequirement{{InlinePrompt: "paste a token"}})
		assert.Empty(t, af)
	})

	t.Run("a second Record for one origin does not replace the first", func(t *testing.T) {
		af := NewAuthFailureOrigins()
		require.True(t, af.RecordProvider("toolkit/gh", providerWithAuthFailure))
		assert.True(t, af.RecordProvider("toolkit/gh", providerWithoutAuthFailure),
			"already-present reports true; the point is that it does not overwrite")
		assert.NotNil(t, af.Lookup("toolkit/gh"))
	})

	t.Run("Lookup on a nil map is a safe miss, not a panic", func(t *testing.T) {
		var af AuthFailureOrigins
		assert.NotPanics(t, func() { assert.Nil(t, af.Lookup("toolkit/gh")) })
	})
}

// TestAuthFailureOriginKeysMatchToolOrigins is the drift guard. The recorder
// looks a failed call's origin up in this map using the EXACT string
// tool.OriginTool.Origin() returned. If a key is composed with a different
// prefix — "sandbox/" instead of "toolkit/", say — nothing errors: the lookup
// simply misses forever and every CLI credential silently becomes
// uncorroborable. So the keys are asserted against real tools' Origin(), not
// against string literals repeated from the wiring.
func TestAuthFailureOriginKeysMatchToolOrigins(t *testing.T) {
	t.Run("toolkit origin key matches SandboxTool.Origin()", func(t *testing.T) {
		st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
			BundleName: "code", Suffix: "gh",
			Toolkit: &toolkit.Toolkit{Name: "gh"},
		})
		assert.Equal(t, st.Origin(), originfmt.ForToolkit("gh"))
	})

	t.Run("a toolkit-less sandbox tool has no origin to key on", func(t *testing.T) {
		st := sandbox.NewSandboxTool(sandbox.SandboxOpts{BundleName: "code", Suffix: "bash"})
		require.Empty(t, st.Origin())

		af := NewAuthFailureOrigins()
		af.RecordProvider(st.Origin(), providerWithAuthFailure)
		assert.Empty(t, af)
	})

	t.Run("an unnamed subject composes to \"\", never to a bare prefix", func(t *testing.T) {
		// A bare "toolkit/" would be a live map key no tool's Origin() can ever
		// equal — silently unreachable rather than obviously wrong.
		assert.Empty(t, originfmt.ForToolkit(""))
		assert.Empty(t, originfmt.ForMCPServer(""))
		assert.Empty(t, originfmt.ForSidecar(spiceboxv1alpha1.ResolvedSidecarToolbox{Name: "box"}))

		af := NewAuthFailureOrigins()
		af.RecordProvider(originfmt.ForToolkit(""), providerWithAuthFailure)
		af.RecordProvider(originfmt.ForMCPServer(""), providerWithAuthFailure)
		assert.Empty(t, af)
	})

	t.Run("mcpserver origin key matches the synthesized MCPTool's Origin()", func(t *testing.T) {
		// The origin kind with the most credential traffic, and the one whose
		// key the runner builds from the REAL CR name rather than the
		// LLM-prefixed copy it hands Synthesize — so WithOriginName is part of
		// what this pins.
		cr := &spiceboxv1alpha1.MCPServer{}
		cr.Name = "demo-llm-prefixed"
		cr.Spec.Server.URL = "https://mcp.invalid"
		cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
			{Name: "search", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
		}
		res, err := mcpdispatch.Synthesize(cr, []mcpprobe.Tool{{Name: "search"}},
			mcpdispatch.WithOriginName("demo-cr"))
		require.NoError(t, err)
		require.NotEmpty(t, res.LLMTools, "precondition: the allowlisted tool synthesized")

		ot, ok := res.LLMTools[0].(tool.OriginTool)
		require.True(t, ok, "precondition: MCP tools carry an origin")
		assert.Equal(t, ot.Origin(), originfmt.ForMCPServer("demo-cr"))
	})

	t.Run("sidecar origin key matches the synthesized tool's Origin()", func(t *testing.T) {
		rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
			Name: "box", Ref: "demo-toolbox", Port: 9099,
			Spec: spiceboxv1alpha1.SidecarToolboxSpec{
				Name:         "demo",
				UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: providerWithAuthFailure},
				Tools:        []spiceboxv1alpha1.MCPServerTool{{Name: "ping"}},
			},
		}
		built, err := sidecartoolboxsynth.Synthesize(rt, []mcpprobe.Tool{{Name: "ping"}}, nil)
		require.NoError(t, err)
		require.NotEmpty(t, built, "precondition: the allowlisted tool synthesized")

		ot, ok := built[0].(tool.OriginTool)
		require.True(t, ok, "precondition: sidecar tools carry an origin")
		assert.Equal(t, ot.Origin(), originfmt.ForSidecar(rt))
	})
}
