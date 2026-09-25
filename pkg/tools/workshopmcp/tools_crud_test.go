package workshopmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
)

// callTool round-trips args through JSON and invokes handler directly — no
// MCP transport hop, mirroring tools_inventory_test.go's callInventory.
func callTool(t *testing.T, handler func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error), args any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := handler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}})
	require.NoError(t, err, "handler must not return a protocol-level error")
	return res
}

func decodeResultBody(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "result content block must be text")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &body), "result body must be valid JSON")
	return body
}

// seededMCPServer builds a minimal, well-formed MCPServer in ns for the CRUD
// tests to read/delete.
func seededMCPServer(ns, name string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: name, Version: "v1", Intent: "demo",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example.test/mcp", Transport: "streamable-http"},
		},
	}
}

// TestHandleGet_ReturnsCRInWorkshop proves workshop_get reads a CR that
// lives in the workshop namespace.
func TestHandleGet_ReturnsCRInWorkshop(t *testing.T) {
	ns, name := "ws-demo123", "demo-tool"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(seededMCPServer(ns, name)).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleGet, kindNameArgs{Kind: "MCPServer", Name: name})
	require.False(t, res.IsError, "reading an existing in-workshop CR must not fail")
	body := decodeResultBody(t, res)
	metadata, ok := body["metadata"].(map[string]any)
	require.True(t, ok, "typed object JSON must carry metadata")
	assert.Equal(t, name, metadata["name"])
	assert.Equal(t, ns, metadata["namespace"])
}

// TestHandleGet_UnknownName_ReportsNotFound proves a name absent from the
// workshop surfaces a clear error, not a silent empty success.
func TestHandleGet_UnknownName_ReportsNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-demo123", c)

	res := callTool(t, s.handleGet, kindNameArgs{Kind: "MCPServer", Name: "nope"})
	require.True(t, res.IsError)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "nope")
}

// TestHandleGet_Denied_SurfacesVerbatim: a fake apiserver Forbidden on Get
// (RBAC outside the workshop's bound scope) comes back through
// deniedResult, unchanged.
func TestHandleGet_Denied_SurfacesVerbatim(t *testing.T) {
	denyErr := apierrors.NewForbidden(schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "mcpservers"}, "demo-tool", errors.New("RBAC denies reads outside the workshop"))
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return denyErr
			},
		}).Build()
	s := newApplyTestServer("ws-demo123", c)

	res := callTool(t, s.handleGet, kindNameArgs{Kind: "MCPServer", Name: "demo-tool"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"])
	assert.Equal(t, denyErr.Error(), body["message"])
}

// TestHandleList_ReturnsCRsInWorkshop proves workshop_list enumerates every
// CR of the requested kind in W.
func TestHandleList_ReturnsCRsInWorkshop(t *testing.T) {
	ns := "ws-demo123"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(seededMCPServer(ns, "tool-a"), seededMCPServer(ns, "tool-b")).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleList, listArgs{Kind: "MCPServer"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	items, ok := body["items"].([]any)
	require.True(t, ok, "typed list JSON must carry items")
	assert.Len(t, items, 2)
}

// TestHandleList_UnknownKind_FailsClosed proves an unrecognized kind is
// refused rather than silently returning an empty list.
func TestHandleList_UnknownKind_FailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-demo123", c)

	res := callTool(t, s.handleList, listArgs{Kind: "NoSuchKind"})
	require.True(t, res.IsError)
}

// seededAgentClass builds a minimal AgentClass in ns for the MINOR-5
// standin-marker tests below.
func seededAgentClass(ns, name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: name},
	}
}

// TestHandleGet_AgentClass_SurfacesStandinMarker is MINOR-5: `get` on an
// AgentClass now carries a STRUCTURAL "standin" marker sourced from the
// Workshop CR's own status.standins (BLOCKER-1's registry), so a model does
// not have to parse the Description's stand-in-first sentence to tell a
// rehearsal double apart from a real, authored class.
func TestHandleGet_AgentClass_SurfacesStandinMarker(t *testing.T) {
	ns := "ws-demo123"
	authored := seededAgentClass(ns, "authored-class")
	standin := seededAgentClass(ns, "standin-class")
	ws := fixtureWorkshopForApplyTestServer(spiceboxv1alpha1.WorkshopStandin{
		Name: "standin-class", SourceNamespace: "default", SourceName: "standin-class",
	})
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(authored, standin, ws).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleGet, kindNameArgs{Kind: "AgentClass", Name: "authored-class"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, false, body["standin"], "a class status never recorded as a stand-in must report standin=false")

	res = callTool(t, s.handleGet, kindNameArgs{Kind: "AgentClass", Name: "standin-class"})
	require.False(t, res.IsError)
	body = decodeResultBody(t, res)
	assert.Equal(t, true, body["standin"], "a class status DOES record as a stand-in must report standin=true")
}

// TestHandleList_AgentClass_SurfacesStandinMarkerPerItem is MINOR-5's list
// counterpart: every item in the list carries its own standin marker.
func TestHandleList_AgentClass_SurfacesStandinMarkerPerItem(t *testing.T) {
	ns := "ws-demo123"
	authored := seededAgentClass(ns, "authored-class")
	standin := seededAgentClass(ns, "standin-class")
	ws := fixtureWorkshopForApplyTestServer(spiceboxv1alpha1.WorkshopStandin{
		Name: "standin-class", SourceNamespace: "default", SourceName: "standin-class",
	})
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(authored, standin, ws).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleList, listArgs{Kind: "AgentClass"})
	require.False(t, res.IsError)

	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &items))
	require.Len(t, items, 2)

	got := make(map[string]bool, len(items))
	for _, item := range items {
		resource, ok := item["resource"].(map[string]any)
		require.True(t, ok, "each list entry must wrap the resource")
		metadata, ok := resource["metadata"].(map[string]any)
		require.True(t, ok)
		name, ok := metadata["name"].(string)
		require.True(t, ok)
		standin, ok := item["standin"].(bool)
		require.True(t, ok, "each list entry must carry a structural standin marker")
		got[name] = standin
	}
	assert.Equal(t, map[string]bool{"authored-class": false, "standin-class": true}, got)
}

// TestHandleDelete_DeletesCRInWorkshop proves workshop_delete removes the
// named CR, and a subsequent read confirms it is gone.
func TestHandleDelete_DeletesCRInWorkshop(t *testing.T) {
	ns, name := "ws-demo123", "demo-tool"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(seededMCPServer(ns, name)).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleDelete, kindNameArgs{Kind: "MCPServer", Name: name})
	require.False(t, res.IsError, "deleting an existing in-workshop CR must not fail")
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["deleted"])

	var check spiceboxv1alpha1.MCPServer
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &check)
	assert.True(t, apierrors.IsNotFound(err), "the object must actually be gone after workshop_delete")
}

// TestHandleDelete_Denied_SurfacesVerbatim: a fake apiserver Forbidden on
// Delete comes back through deniedResult, unchanged.
func TestHandleDelete_Denied_SurfacesVerbatim(t *testing.T) {
	ns, name := "ws-demo123", "demo-tool"
	denyErr := apierrors.NewForbidden(schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "mcpservers"}, name, errors.New("workshop policy forbids deleting this resource"))
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(seededMCPServer(ns, name)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return denyErr
			},
		}).Build()
	s := newApplyTestServer(ns, c)

	res := callTool(t, s.handleDelete, kindNameArgs{Kind: "MCPServer", Name: name})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"])
	assert.Equal(t, denyErr.Error(), body["message"])

	// Never claimed a partial success: the object must still be present.
	var check spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &check),
		"a denied delete must leave the object untouched")
}

// The workshop's closed kind set is the design's (§1.2): the namespaced kinds
// a builder authors in W, which the Workshop controller's Role already grants
// (pkg/controllers/workshop/rbac.go). The table drives apply/get/list/delete,
// so a kind missing here is a kind no builder can author — observed live:
// request_credential needs an AgentIdentity "authored in this workshop", and
// apply refused the kind by name, so no credential could ever be connected;
// the Agent phase's own Skills and AgentUI would have hit the same wall.
func TestWorkshopKindTable_CoversTheDesignsAuthoredKinds(t *testing.T) {
	for _, kind := range []string{"AgentClass", "MCPServer", "SidecarToolbox", "SpiceboxToolspec", "AgentIdentity", "Skill", "AgentUI"} {
		entry, ok := workshopKindTable[kind]
		if assert.True(t, ok, "kind %s must be authorable in the workshop", kind) {
			assert.True(t, strings.HasSuffix(fmt.Sprintf("%T", entry.newObject()), "."+kind), "%s: newObject must construct that kind, got %T", kind, entry.newObject())
			assert.NotNil(t, entry.newList(), "%s: newList must construct a list", kind)
		}
	}
}
