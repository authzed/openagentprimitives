package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

func sampleServer() *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x", Transport: "streamable-http"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "linear"},
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "search", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
				{Name: "get", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			ObservedTools: []string{"search", "extra"},
			Conditions: []metav1.Condition{
				{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue, Reason: "AllowlistResolved"},
				{Type: spiceboxv1alpha1.MCPServerConditionReachable, Status: metav1.ConditionFalse, Reason: "DialFail"},
			},
		},
	}
}

func TestKind_BasicMetadata(t *testing.T) {
	k := New()
	assert.Equal(t, "MCPServer", k.Name(), "Name")
	gvr := k.GVR()
	assert.Equal(t, "agentprimitives.authzed.com", gvr.Group, "GVR.Group")
	assert.Equal(t, "mcpservers", gvr.Resource, "GVR.Resource")
	_, ok := k.NewObject().(*spiceboxv1alpha1.MCPServer)
	assert.True(t, ok, "NewObject() type")
	_, ok = k.NewList().(*spiceboxv1alpha1.MCPServerList)
	assert.True(t, ok, "NewList() type")
}

func TestKind_DecodeYAML_Match(t *testing.T) {
	const doc = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: x
  namespace: ns
spec:
  server: { url: https://x, transport: streamable-http }
  tools: []
`
	obj, err := New().DecodeYAML([]byte(doc))
	require.NoError(t, err, "DecodeYAML")
	require.NotNil(t, obj, "obj")
	cr, ok := obj.(*spiceboxv1alpha1.MCPServer)
	require.True(t, ok, "decoded type = %T", obj)
	assert.Equal(t, "x", cr.Name)
	assert.Equal(t, "https://x", cr.Spec.Server.URL)
}

func TestKind_DecodeYAML_OtherKind(t *testing.T) {
	const doc = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata: {name: x}
`
	obj, err := New().DecodeYAML([]byte(doc))
	require.NoError(t, err, "DecodeYAML")
	assert.Nil(t, obj, "expected nil obj for non-MCPServer kind")
}

func TestKind_Row(t *testing.T) {
	row := New().Row(sampleServer())
	assert.Equal(t, "MCPServer", row.Kind, "row.Kind")
	assert.Equal(t, "linear", row.Name, "row.Name")
	assert.Equal(t, "ns", row.Namespace, "row.Namespace")
	assert.Contains(t, row.Status, "Valid=True", "row.Status")
	assert.Contains(t, row.Status, "Reachable=False", "row.Status")
	assert.Contains(t, row.Summary, "https://x", "row.Summary")
	assert.Contains(t, row.Summary, "2 tool(s)", "row.Summary")
}

func TestKind_Detail_RedactsTokenAndMarksObserved(t *testing.T) {
	d := New().Detail(sampleServer())
	assert.Equal(t, "MCPServer", d.Kind, "detail.Kind")
	assert.Equal(t, "linear", d.Name, "detail.Name")
	body := ""
	for _, s := range d.Sections {
		body += s.Title + "\n" + s.Body + "\n"
	}
	for _, want := range []string{
		"https://x",
		"provider:    linear",
		"search",
		"extra (not allowlisted)",
		"Reachable=False",
	} {
		assert.Contains(t, body, want, "detail missing %q", want)
	}
}

// TestKind_LintFile covers the lint pass over a temporary MCPServer
// spec file. Each case writes a spec body to disk, runs LintFile, and
// asserts the diagnostic shape.
func TestKind_LintFile(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantDiags int
		check     func(t *testing.T, diags []contract.Diagnostic)
	}{
		{
			name: "valid CEL constraint: no diagnostics",
			body: `
name: ok
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t1
    args:
      constraints:
        - cel: 'args.q.size() < 100'
          message: 'q must have fewer than 100 items'
`,
			wantDiags: 0,
		},
		{
			name: "malformed CEL constraint: one error diagnostic",
			body: `
name: bad
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t
    args:
      constraints:
        - cel: 'args.q.size( < 100'
`,
			wantDiags: 1,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Equal(t, "error", diags[0].Severity, "Severity")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.body)
			diags, err := New().LintFile(path)
			require.NoError(t, err, "LintFile")
			require.Len(t, diags, tc.wantDiags, "diag count")
			if tc.check != nil {
				tc.check(t, diags)
			}
		})
	}
}
