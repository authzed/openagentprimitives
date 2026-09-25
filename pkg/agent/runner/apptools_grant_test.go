package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
)

// apptoolsGrantTestScheme builds a scheme carrying the AgentUI CRD type.
// package runner (this file) cannot reuse status_test.go's newScheme helper
// — that helper lives in the separate runner_test (external) package, which
// this internal-test file cannot import.
func apptoolsGrantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// TestMaterializeAppTools_IsTheThreeWayGrant pins the enforcement of
// pkg/web/uigrant.Materialize at the single point Loop.AppTools is assigned
// from — reusing fakeAppTool (apptoolcall_test.go, same package) rather than
// a second hand-rolled tool.Tool stub.
//
// This table intentionally does NOT also assert Loop.Tools is unchanged: an
// earlier version built a Loop with a fixed Tools value and asserted it
// equaled itself, which cannot fail under any mutation of this function's
// body (MaterializeAppTools's signature has no Tools/mergedTools parameter
// at all — that guarantee is structural, at the type level, not a runtime
// fact worth re-asserting per row). See the doc comment on
// MaterializeAppTools in apptools_grant.go.
func TestMaterializeAppTools_IsTheThreeWayGrant(t *testing.T) {
	// Both tools are always accumulated (as mcpdispatch.Synthesize would
	// produce for two opted-in-origin, app-visible MCP tools) so every row
	// exercises MaterializeAppTools's own filtering, not upstream absence.
	mcpAppTools := []tool.Tool{
		&fakeAppTool{name: "widgets_list_items"},
		&fakeAppTool{name: "widgets_delete_item"},
	}
	bothEnabled := []uigrant.Origin{
		{Name: "widgets", AppToolsEnabled: true, AppVisibleTools: []string{"widgets_list_items", "widgets_delete_item"}},
	}
	fullGrant := &spiceboxv1alpha1.AgentClassUIGrant{
		Ref:          "widget-ui",
		GrantedTools: []string{"widgets_list_items", "widgets_delete_item"},
	}

	cases := []struct {
		name        string
		requested   []string
		origins     []uigrant.Origin
		grant       *spiceboxv1alpha1.AgentClassUIGrant
		mcpAppTools []tool.Tool
		want        []string
	}{
		{
			name:      "no AgentUI resolvable (requested nil): AppTools empty",
			requested: nil,
			origins:   bothEnabled,
			grant:     fullGrant,
			want:      nil,
		},
		{
			name:      "AgentUI resolved but AgentClass carries no grant: AppTools empty",
			requested: []string{"widgets_list_items", "widgets_delete_item"},
			origins:   bothEnabled,
			grant:     nil,
			want:      nil,
		},
		{
			name:      "grant names a tool the UI never requested: that tool excluded",
			requested: []string{"widgets_list_items"},
			origins:   bothEnabled,
			grant:     fullGrant, // grants BOTH tools; UI only asked for one
			want:      []string{"widgets_list_items"},
		},
		{
			name:      "grant names a tool whose origin opted out: that tool excluded",
			requested: []string{"widgets_list_items", "widgets_delete_item"},
			origins: []uigrant.Origin{
				{Name: "widgets", AppToolsEnabled: true, AppVisibleTools: []string{"widgets_list_items"}},
				{Name: "widgets-admin", AppToolsEnabled: false, AppVisibleTools: []string{"widgets_delete_item"}},
			},
			grant: fullGrant,
			want:  []string{"widgets_list_items"},
		},
		{
			name:      "full intersection: both tools survive",
			requested: []string{"widgets_list_items", "widgets_delete_item"},
			origins:   bothEnabled,
			grant:     fullGrant,
			want:      []string{"widgets_list_items", "widgets_delete_item"},
		},
		// --- I2: normalization rows ---
		{
			// "Widgets_List_Items" lowercases to "widgets_list_items" — the
			// same registry key — proving a mixed-case author string still
			// matches, on BOTH the request and the grant sides.
			name:      "mixed-case authored request+grant still matches the lowercased registry key",
			requested: []string{"Widgets_List_Items"},
			origins:   bothEnabled,
			grant: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "widget-ui",
				GrantedTools: []string{"WIDGETS_LIST_ITEMS"},
			},
			want: []string{"widgets_list_items"},
		},
		{
			// The registry key "widgets_create-issue" is what
			// synthesize.Build would actually produce for an upstream MCP
			// tool literally named "create.issue" under ref "widgets" (the
			// dot is outside [a-z0-9_-], so Build's own NormalizeName call
			// rewrites it to '-'). An author who types the tool's raw
			// upstream name (with the dot, and in whatever case) into
			// spec.tools/grantedTools must still match that key.
			name:      "authored name containing a NormalizeName-rewritten character still matches",
			requested: []string{"Widgets_Create.Issue"},
			mcpAppTools: []tool.Tool{
				&fakeAppTool{name: "widgets_create-issue"},
			},
			origins: []uigrant.Origin{
				{Name: "widgets", AppToolsEnabled: true, AppVisibleTools: []string{"widgets_create-issue"}},
			},
			grant: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "widget-ui",
				GrantedTools: []string{"widgets_create.issue"},
			},
			want: []string{"widgets_create-issue"},
		},
		{
			// Normalization must not become a way to smuggle a match: the
			// grant names a DIFFERENT tool ("Widgets_Delete_Items", plural)
			// that normalizes to "widgets_delete_items" — not equal to the
			// real registry key "widgets_delete_item" (singular) — so the
			// requested tool must still be denied.
			name:      "normalization does not smuggle a match for a genuinely different tool: still denied",
			requested: []string{"Widgets_Delete_Item"},
			origins:   bothEnabled,
			grant: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "widget-ui",
				GrantedTools: []string{"Widgets_Delete_Items"},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appTools := tc.mcpAppTools
			if appTools == nil {
				appTools = mcpAppTools
			}
			got := MaterializeAppTools(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), AppToolsLogContext{},
				tc.requested, tc.origins, tc.grant, appTools)

			gotNames := make([]string, 0, len(got))
			for _, tl := range got {
				gotNames = append(gotNames, tl.Name())
			}
			assert.ElementsMatch(t, tc.want, gotNames, "MaterializeAppTools result key set")
		})
	}
}

// TestMaterializeAppTools_LogsExplainOnPartialDenial pins I1: a denial that
// shrinks AppTools below what was requested must not be silent.
// uigrant.Explain had zero non-test callers before this — a UI requesting 5
// tools and getting 0 produced not one log line.
func TestMaterializeAppTools_LogsExplainOnPartialDenial(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	origins := []uigrant.Origin{
		{Name: "widgets", AppToolsEnabled: true, AppVisibleTools: []string{"widgets_list_items", "widgets_delete_item"}},
	}
	grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widget-ui", GrantedTools: []string{"widgets_list_items"}}
	mcpAppTools := []tool.Tool{
		&fakeAppTool{name: "widgets_list_items"},
		&fakeAppTool{name: "widgets_delete_item"},
	}

	got := MaterializeAppTools(logger, AppToolsLogContext{Session: "ns/s1", AgentClass: "ns/ac1", AgentUI: "ns/widget-ui"},
		[]string{"widgets_list_items", "widgets_delete_item"}, origins, grant, mcpAppTools)

	require.Len(t, got, 1, "only the granted tool survives")

	logged := buf.String()
	assert.Contains(t, logged, "ns/s1", "session must be in the denial log")
	assert.Contains(t, logged, "ns/ac1", "agentClass must be in the denial log")
	assert.Contains(t, logged, "ns/widget-ui", "agentUI must be in the denial log")
	assert.Contains(t, logged, "widgets_delete_item", "the denied tool's name must be in the log")
	assert.Contains(t, logged, "not in the deployment grant", "uigrant.Explain's reason must be in the log")
}

// TestMaterializeAppTools_NoLogOnFullGrant pins that a fully-satisfied
// request produces NO denial log line — the log exists to diagnose a
// shortfall, not to narrate every call.
func TestMaterializeAppTools_NoLogOnFullGrant(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	origins := []uigrant.Origin{
		{Name: "widgets", AppToolsEnabled: true, AppVisibleTools: []string{"widgets_list_items"}},
	}
	grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widget-ui", GrantedTools: []string{"widgets_list_items"}}
	mcpAppTools := []tool.Tool{&fakeAppTool{name: "widgets_list_items"}}

	got := MaterializeAppTools(logger, AppToolsLogContext{Session: "ns/s1"},
		[]string{"widgets_list_items"}, origins, grant, mcpAppTools)

	require.Len(t, got, 1)
	assert.Empty(t, buf.String(), "a fully-satisfied request must not log a denial")
}

// TestAppToolOrigin pins that an origin's uigrant.Origin is built from the
// already LLM-prefixed tool names (t.Name()) — the exact vocabulary
// requested/granted use — not the bare upstream MCPServerTool.Name. Feeding
// the bare name here would make uigrant.Materialize match zero keys: fail
// closed, but silently so.
func TestAppToolOrigin(t *testing.T) {
	appTools := []tool.Tool{
		&fakeAppTool{name: "widgets_list_items"},
		&fakeAppTool{name: "widgets_delete_item"},
	}

	got := AppToolOrigin("widgets", true, appTools)

	assert.Equal(t, "widgets", got.Name)
	assert.True(t, got.AppToolsEnabled)
	assert.ElementsMatch(t, []string{"widgets_list_items", "widgets_delete_item"}, got.AppVisibleTools,
		"Origin.AppVisibleTools must carry the LLM-prefixed name, not the bare upstream tool name")
}

// TestResolveAgentUITools covers the AgentUI-resolution path directly (M2):
// nil grant, Get-succeeds, and Get-errors (not-found and forbidden), none of
// which had coverage before this — the block was inlined in
// internal/cmd/runner/main.go with nothing exercising the Get-succeeds or
// error-flows-to-nil path.
func TestResolveAgentUITools(t *testing.T) {
	const ns = "default"
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-ui", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentUISpec{Tools: []string{"widgets_list_items", "widgets_delete_item"}},
	}
	scheme := apptoolsGrantTestScheme(t)

	t.Run("nil grant: no AgentUI configured at all, no error, no tools", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		tools, err := ResolveAgentUITools(context.Background(), c, ns, nil)
		require.NoError(t, err)
		assert.Nil(t, tools)
	})

	t.Run("grant resolves: Spec.Tools returned, no error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aui).Build()
		grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widget-ui"}
		tools, err := ResolveAgentUITools(context.Background(), c, ns, grant)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"widgets_list_items", "widgets_delete_item"}, tools)
	})

	t.Run("grant.Ref not found: error returned, no tools", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build() // no AgentUI seeded
		grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widget-ui"}
		tools, err := ResolveAgentUITools(context.Background(), c, ns, grant)
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err), "must be a NotFound error: %v", err)
		assert.Nil(t, tools)
	})

	t.Run("runner SA missing RBAC (forbidden Get): error returned, no tools", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aui).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gvr := schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "agentuis"}
					return apierrors.NewForbidden(gvr, key.Name, errors.New("runner SA lacks agentuis get"))
				},
			}).Build()
		grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widget-ui"}
		tools, err := ResolveAgentUITools(context.Background(), c, ns, grant)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "must be a Forbidden error: %v", err)
		assert.Nil(t, tools)
	})
}
