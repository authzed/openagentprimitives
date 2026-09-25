package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

func TestMCPServerSpec_ToSpec_RoundTrip(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: linear-readonly
spec:
  name: linear-readonly
  version: "1"
  intent: "Read-only Linear access."
  server:
    url: https://mcp.linear.example/v1
    transport: streamable-http
  auth:
    provider: oauth-mcp
    header: Authorization
    valuePrefix: "Bearer "
  callTimeout: 60s
  tools:
    - name: search_issues
      intent: "Find issues."
      args:
        allowedFields: [teamId, q]
        constraints:
          - cel: 'args.teamId == "TEAM1"'
            message: "team restricted"
        sensitiveFields: [body]
      deny:
        effects:
          destructive: false
          writes: [network]
          creds: { writes: true }
`)
	var cr MCPServer
	require.NoError(t, yaml.Unmarshal(src, &cr), "unmarshal MCPServer YAML")
	require.Equal(t, "linear-readonly", cr.Spec.Name, "cr.Spec.Name must be set before ToSpec round-trip")

	sp, err := cr.Spec.ToSpec()
	require.NoError(t, err, "ToSpec")
	var _ *mcpspec.Spec = sp

	assert.Equal(t, "linear-readonly", sp.Name, "sp.Name")
	require.Len(t, sp.Tools, 1, "Tools length")
	assert.Equal(t, "search_issues", sp.Tools[0].Name, "Tools[0].Name")
	require.Len(t, sp.Tools[0].Args.Constraints, 1, "Tools[0].Args.Constraints length")
	assert.NotEmpty(t, sp.Tools[0].Args.Constraints[0].CEL, "Tools[0].Args.Constraints[0].CEL")
	assert.True(t, sp.Tools[0].Deny.Effects.Creds.Writes, "Deny.Effects.Creds.Writes should round-trip as true")
}

func TestMCPServerSpec_ToSpec_PreservesEffectsAndGeneration(t *testing.T) {
	src := MCPServerSpec{
		Name:    "x",
		Version: "1",
		Server:  MCPServerServer{URL: "https://e.x/mcp", Transport: "streamable-http"},
		Tools: []MCPServerTool{
			{Name: "t", Effects: MCPServerToolEffects{Destructive: true, OpenWorld: true}},
		},
		Generation: &MCPServerSpecGeneration{
			TestCases: []MCPServerSpecTestCase{
				{Intent: "i", ToolName: "t", ExpectedAllow: true},
			},
		},
	}
	got, err := src.ToSpec()
	require.NoError(t, err, "ToSpec")

	assert.Equal(t,
		mcpspec.Effects{Destructive: true, OpenWorld: true},
		got.Tools[0].Effects,
		"Effects round-trip",
	)
	require.NotNil(t, got.Generation, "Generation")
	require.Len(t, got.Generation.TestCases, 1, "Generation.TestCases length")
	assert.Equal(t, "i", got.Generation.TestCases[0].Intent, "Generation.TestCases[0].Intent")
}

// TestMCPServerSpec_ToSpec_PreservesVisibility verifies that
// MCPServerTool.Visibility (populated by the authoring flow from the
// probed tool's _meta.ui.visibility) survives the ToSpec conversion
// into mcpspec.Tool.Visibility. Task 4's runtime routing depends on
// this carrying through with zero hand-wiring beyond the shared
// `json:"visibility,omitempty"` tag.
func TestMCPServerSpec_ToSpec_PreservesVisibility(t *testing.T) {
	src := MCPServerSpec{
		Name:    "x",
		Version: "1",
		Server:  MCPServerServer{URL: "https://e.x/mcp", Transport: "streamable-http"},
		Tools: []MCPServerTool{
			{Name: "search", Visibility: []string{"app"}},
			{Name: "delete_item"},
		},
	}
	got, err := src.ToSpec()
	require.NoError(t, err, "ToSpec")

	require.Len(t, got.Tools, 2, "Tools length")
	assert.Equal(t, []string{"app"}, got.Tools[0].Visibility, "Tools[0].Visibility round-trip")
	assert.Nil(t, got.Tools[1].Visibility, "Tools[1].Visibility should stay nil when unset")
}

// TestMCPServerSpec_ToSpec_PreservesObserves pins the CR half of
// mcpspec.Tool's observes mirror. ToSpec is a JSON round-trip (like ToSpec
// is for SpiceboxToolspecSpec and ToToolkit is for SpiceboxToolkitSpec) — a
// CRD field with no matching json tag on mcpspec.Tool is not "unset", it is
// silently dropped. Asserts every field survives, not merely that the
// slice is non-empty.
func TestMCPServerSpec_ToSpec_PreservesObserves(t *testing.T) {
	src := MCPServerSpec{
		Name:    "x",
		Version: "1",
		Server:  MCPServerServer{URL: "https://e.x/mcp", Transport: "streamable-http"},
		Tools: []MCPServerTool{
			{
				Name: "search_issues",
				Observes: []ObservesBlock{{
					When:    "result.success",
					ForEach: "result.results",
					Subjects: []ObserveSubject{
						{ResourceType: `"github_pr"`, ResourceID: "item.id"},
					},
					Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
				}},
			},
		},
	}
	got, err := src.ToSpec()
	require.NoError(t, err, "ToSpec")

	require.Len(t, got.Tools[0].Observes, 1,
		"observes must survive the JSON round-trip into mcpspec.Tool")
	ob := got.Tools[0].Observes[0]
	assert.Equal(t, "result.success", ob.When, "when must round-trip")
	assert.Equal(t, "result.results", ob.ForEach, "forEach must round-trip")
	require.Len(t, ob.Subjects, 1, "subjects must round-trip")
	assert.Equal(t, `"github_pr"`, ob.Subjects[0].ResourceType)
	assert.Equal(t, "item.id", ob.Subjects[0].ResourceID)
	assert.Equal(t, "item.isCrossRepository", ob.Facts["is_cross_repository"],
		"facts must round-trip")
}

func TestMCPServerToolResourceMap_RoundTrip(t *testing.T) {
	bypass := true
	s := MCPServerSpec{
		Name:    "linear",
		Version: "v1",
		Server:  MCPServerServer{},
		Tools:   []MCPServerTool{},
		ToolResourceMap: []ToolResourceMapping{
			{
				Tool: "get_issue",
				Reads: &ToolReads{
					ResourceType:         "linear_issue",
					IDArg:                "issueId",
					Permission:           "view",
					BypassRequesterCheck: &bypass,
				},
			},
			{Tool: "get_current_time", NoTaint: true},
		},
	}
	buf, err := json.Marshal(s)
	require.NoError(t, err)

	var back MCPServerSpec
	require.NoError(t, json.Unmarshal(buf, &back))

	require.Len(t, back.ToolResourceMap, 2)
	assert.Equal(t, "get_issue", back.ToolResourceMap[0].Tool)
	require.NotNil(t, back.ToolResourceMap[0].Reads)
	assert.Equal(t, "linear_issue", back.ToolResourceMap[0].Reads.ResourceType)
	assert.Equal(t, "issueId", back.ToolResourceMap[0].Reads.IDArg)
	assert.Equal(t, "view", back.ToolResourceMap[0].Reads.Permission)
	require.NotNil(t, back.ToolResourceMap[0].Reads.BypassRequesterCheck)
	assert.True(t, *back.ToolResourceMap[0].Reads.BypassRequesterCheck)
	assert.True(t, back.ToolResourceMap[1].NoTaint)
}

// TestMCPUIAppToolsSpec_NilByDefault verifies the absent-⇒-disabled
// idiom: a MCPServerSpec with no MCPUIAppTools set round-trips as nil,
// which callers must treat as disabled (Task 3, Phase A).
func TestMCPUIAppToolsSpec_NilByDefault(t *testing.T) {
	s := MCPServerSpec{
		Name:    "x",
		Version: "1",
		Server:  MCPServerServer{URL: "https://e.x/mcp", Transport: "streamable-http"},
	}
	buf, err := json.Marshal(s)
	require.NoError(t, err)

	var back MCPServerSpec
	require.NoError(t, json.Unmarshal(buf, &back))

	assert.Nil(t, back.MCPUIAppTools, "MCPUIAppTools must stay nil (⇒ disabled) when unset")
}

// TestMCPUIAppToolsSpec_RoundTrip verifies that an explicit opt-in with
// both fields set survives a JSON marshal/unmarshal round-trip.
func TestMCPUIAppToolsSpec_RoundTrip(t *testing.T) {
	maxCalls := int32(30)
	s := MCPServerSpec{
		Name:    "x",
		Version: "1",
		Server:  MCPServerServer{URL: "https://e.x/mcp", Transport: "streamable-http"},
		MCPUIAppTools: &MCPUIAppToolsSpec{
			Enabled:        true,
			MaxCallsPerMin: &maxCalls,
		},
	}
	buf, err := json.Marshal(s)
	require.NoError(t, err)

	var back MCPServerSpec
	require.NoError(t, json.Unmarshal(buf, &back))

	require.NotNil(t, back.MCPUIAppTools, "MCPUIAppTools must survive round-trip")
	assert.True(t, back.MCPUIAppTools.Enabled, "Enabled must round-trip as true")
	require.NotNil(t, back.MCPUIAppTools.MaxCallsPerMin, "MaxCallsPerMin must round-trip")
	assert.Equal(t, int32(30), *back.MCPUIAppTools.MaxCallsPerMin, "MaxCallsPerMin value")
}

func TestMCPServerLookupToolResourceMapping(t *testing.T) {
	s := MCPServerSpec{
		ToolResourceMap: []ToolResourceMapping{
			{Tool: "a", Reads: &ToolReads{ResourceType: "t1"}},
			{Tool: "b", NoTaint: true},
		},
	}
	got := s.LookupToolResourceMapping("a")
	require.NotNil(t, got)
	assert.Equal(t, "a", got.Tool)
	assert.Equal(t, "t1", got.Reads.ResourceType)

	got2 := s.LookupToolResourceMapping("b")
	require.NotNil(t, got2)
	assert.True(t, got2.NoTaint)

	got3 := s.LookupToolResourceMapping("missing")
	assert.Nil(t, got3)
}
