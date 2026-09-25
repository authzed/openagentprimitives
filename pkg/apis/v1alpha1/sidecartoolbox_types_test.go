package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/yaml"
)

func TestSidecarToolbox_DecodeYAML_InlineSource(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata:
  name: reddit-readonly
  namespace: default
spec:
  name: reddit-readonly
  version: "1"
  intent: "Read-only Reddit access."
  source:
    inline:
      baseImage: python:3.12-slim
      script:
        configMapRef: { name: reddit-mcp-src, key: server.py }
      entrypoint: ["python", "/app/server.py"]
  sandbox:
    class: sidecar-sandbox-default
    network:
      allowedHosts: [oauth.reddit.com, www.reddit.com]
  transport:
    port: 8080
    healthcheck:
      path: /healthz
      timeoutSeconds: 30
  upstreamAuth:
    provider: reddit-app
  tools:
    - name: search_posts
      args:
        allowedFields: [subreddit, query, limit]
        constraints:
          - cel: 'args.limit <= 50'
            message: "limit must be <= 50"
      deny:
        effects:
          destructive: false
          writes: [network]
`)
	var out spiceboxv1alpha1.SidecarToolbox
	if err := yaml.Unmarshal(src, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Spec.Source.Inline == nil || out.Spec.Source.Image != "" {
		t.Fatalf("expected inline source, got %+v", out.Spec.Source)
	}
	if out.Spec.Source.Inline.BaseImage != "python:3.12-slim" {
		t.Errorf("BaseImage=%q want python:3.12-slim", out.Spec.Source.Inline.BaseImage)
	}
	if got, want := out.Spec.Sandbox.Class, "sidecar-sandbox-default"; got != want {
		t.Errorf("Sandbox.Class=%q want %q", got, want)
	}
	if got, want := out.Spec.UpstreamAuth.Provider, "reddit-app"; got != want {
		t.Errorf("UpstreamAuth.Provider=%q want %q", got, want)
	}
	if len(out.Spec.Tools) != 1 || out.Spec.Tools[0].Name != "search_posts" {
		t.Fatalf("Tools mismatch: %+v", out.Spec.Tools)
	}
}

// TestSidecarToolbox_SecretInputs_YAML asserts that the secretInputs field
// decodes correctly from YAML and that its fields are populated as expected.
func TestSidecarToolbox_SecretInputs_YAML(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata:
  name: kube-access
  namespace: default
spec:
  name: kube-access
  version: "1"
  source:
    image: ghcr.io/example/kube-mcp:v1.0.0
  sandbox:
    class: sidecar-sandbox-default
  transport:
    port: 8080
  upstreamAuth:
    provider: ""
  tools:
    - name: list_pods
  secretInputs:
    - name: KUBECONFIG
      deliver: file:/root/.kube/config
      from: kubeconfig
`)
	var out spiceboxv1alpha1.SidecarToolbox
	if err := yaml.Unmarshal(src, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out.Spec.SecretInputs) != 1 {
		t.Fatalf("SecretInputs len=%d want 1", len(out.Spec.SecretInputs))
	}
	si := out.Spec.SecretInputs[0]
	if si.Name != "KUBECONFIG" {
		t.Errorf("SecretInputs[0].Name=%q want KUBECONFIG", si.Name)
	}
	if si.Deliver != "file:/root/.kube/config" {
		t.Errorf("SecretInputs[0].Deliver=%q want file:/root/.kube/config", si.Deliver)
	}
	if si.From != "kubeconfig" {
		t.Errorf("SecretInputs[0].From=%q want kubeconfig", si.From)
	}
}

func TestSidecarToolbox_DecodeYAML_ImageSource(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata:
  name: reddit-readonly
  namespace: default
spec:
  name: reddit-readonly
  version: "1"
  source:
    image: ghcr.io/example/reddit-mcp:v1.2.0
  sandbox:
    class: sidecar-sandbox-default
  transport:
    port: 8080
  upstreamAuth:
    provider: reddit-app
  tools:
    - name: search_posts
`)
	var out spiceboxv1alpha1.SidecarToolbox
	if err := yaml.Unmarshal(src, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Spec.Source.Image != "ghcr.io/example/reddit-mcp:v1.2.0" {
		t.Errorf("Source.Image=%q", out.Spec.Source.Image)
	}
	if out.Spec.Source.Inline != nil {
		t.Errorf("Source.Inline should be nil, got %+v", out.Spec.Source.Inline)
	}
}

// TestSidecarToolboxCarriesTheSameAppToolsOptIn asserts that SidecarToolbox
// reuses MCPServer's MCPUIAppToolsSpec verbatim, including the exact
// "mcpUiAppTools" JSON key — the grant's origin-permits condition (see
// pkg/web/uigrant) must be checkable the same way regardless of which origin
// kind produced the tool.
func TestSidecarToolboxCarriesTheSameAppToolsOptIn(t *testing.T) {
	max := int32(30)
	in := spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			MCPUIAppTools: &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true, MaxCallsPerMin: &max},
		},
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"mcpUiAppTools"`, "the stanza must serialize under the same key as MCPServer's")

	var out spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Spec.MCPUIAppTools)
	assert.True(t, out.Spec.MCPUIAppTools.Enabled)
	require.NotNil(t, out.Spec.MCPUIAppTools.MaxCallsPerMin)
	assert.Equal(t, int32(30), *out.Spec.MCPUIAppTools.MaxCallsPerMin)
}

// TestSidecarToolboxAppToolsAbsentMeansDisabled asserts the permanent
// fail-closed default: a nil stanza must never be read as "unset, so allow".
func TestSidecarToolboxAppToolsAbsentMeansDisabled(t *testing.T) {
	var in spiceboxv1alpha1.SidecarToolbox
	assert.Nil(t, in.Spec.MCPUIAppTools,
		"absent must mean disabled — the permanent fail-closed default")
}

// TestSidecarToolboxSpec_CarriesResourceDeclarations pins that a SidecarToolbox
// can declare toolResourceMap + spicedbSchema (the fields that let its tools
// participate in per-datum egress) and that the lookup helper resolves a tool.
func TestSidecarToolboxSpec_CarriesResourceDeclarations(t *testing.T) {
	s := spiceboxv1alpha1.SidecarToolboxSpec{
		ToolResourceMap: []spiceboxv1alpha1.ToolResourceMapping{{
			Tool:  "read_thing",
			Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "thing", IDArg: "id", Permission: "view"},
		}},
		SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{Name: "thing", Standing: "session-only"}},
		},
	}
	got := s.LookupToolResourceMapping("read_thing")
	require.NotNil(t, got, "declared tool must resolve")
	require.NotNil(t, got.Reads)
	assert.Equal(t, "thing", got.Reads.ResourceType)
	assert.Nil(t, s.LookupToolResourceMapping("absent"), "undeclared tool resolves to nil")
}
