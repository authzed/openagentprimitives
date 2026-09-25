package oap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func agentClassFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "pm"},
		"spec": map[string]any{
			"boundEntities": []any{
				map[string]any{"resourceType": "github_repo", "defaults": []any{"authzed/spicedb"}},
			},
		},
	}}
}

// mcpFixture is a generic bindable CR (an MCPServer) with a nested map+scalar
// field, used to exercise the overlay mechanics (Apply/setAtPath) independent of
// any real CRD schema — Apply does not validate against the schema.
func mcpFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "MCPServer",
		"metadata":   map[string]any{"name": "pm-linear"},
		"spec":       map[string]any{"upstream": map[string]any{"endpoint": "OLD"}},
	}}
}

func TestApply_ListSelectorAndScalar(t *testing.T) {
	ac := agentClassFixture()
	mcp := mcpFixture()
	qs := []Question{
		{Name: "repos", Type: QResourceList, Binding: []Binding{{Target: "AgentClass/pm#spec.boundEntities[github_repo].defaults"}}},
		{Name: "endpoint", Type: QString, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream.endpoint"}}},
		{Name: "tok", Type: QSecret, Secret: &SecretQuestion{OrExisting: true}}, // ignored by Apply
	}
	ans := Answers{"repos": []string{"authzed/spicedb", "authzed/docs"}, "endpoint": "https://linear.invalid", "tok": "should-be-ignored"}

	require.NoError(t, Apply([]*unstructured.Unstructured{ac, mcp}, qs, ans))

	defaults, _, _ := unstructured.NestedSlice(ac.Object, "spec", "boundEntities")
	got := defaults[0].(map[string]any)["defaults"]
	assert.Equal(t, []any{"authzed/spicedb", "authzed/docs"}, got)

	endpoint, _, _ := unstructured.NestedString(mcp.Object, "spec", "upstream", "endpoint")
	assert.Equal(t, "https://linear.invalid", endpoint)
}

func spiceboxClassFixture(toolchains ...string) *unstructured.Unstructured {
	tc := make([]any, len(toolchains))
	for i, s := range toolchains {
		tc[i] = s
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxClass",
		"metadata":   map[string]any{"name": "codelike-bundle"},
		"spec":       map[string]any{"toolchains": tc},
	}}
}

func TestApply_AppendMarker_UnionsOntoExisting(t *testing.T) {
	// A class statically declares a mandatory toolchain (claude); the install
	// question APPENDS the user's language selection instead of clobbering it.
	cls := spiceboxClassFixture("claude")
	qs := []Question{{Name: "toolchains", Binding: []Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains[]"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{cls}, qs, Answers{"toolchains": []string{"go", "node"}}))

	got, _, err := unstructured.NestedStringSlice(cls.Object, "spec", "toolchains")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "go", "node"}, got, "existing first, then the appended selection")
}

func TestApply_AppendMarker_DedupsForIdempotency(t *testing.T) {
	// Re-selecting an already-declared toolchain must not duplicate it — a
	// re-install must be a no-op, not grow the list.
	cls := spiceboxClassFixture("claude", "go")
	qs := []Question{{Name: "toolchains", Binding: []Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains[]"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{cls}, qs, Answers{"toolchains": []string{"go", "node"}}))

	got, _, err := unstructured.NestedStringSlice(cls.Object, "spec", "toolchains")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "go", "node"}, got, "go already present is not duplicated")
}

func TestApply_AppendMarker_EmptyExisting(t *testing.T) {
	// No static toolchains → append behaves like a plain set.
	cls := spiceboxClassFixture()
	qs := []Question{{Name: "toolchains", Binding: []Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains[]"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{cls}, qs, Answers{"toolchains": []string{"go"}}))

	got, _, err := unstructured.NestedStringSlice(cls.Object, "spec", "toolchains")
	require.NoError(t, err)
	assert.Equal(t, []string{"go"}, got)
}

func TestApply_AppendMarker_NestedMapField(t *testing.T) {
	// A binding into a nested map followed by an append — spec.config.<key>[] —
	// the shape a generic config plane uses (the class pre-declares an empty
	// list so the field is always present; the install answer appends onto it).
	cls := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "demo-agent"},
		"spec": map[string]any{
			"config": map[string]any{"allowedRepos": []any{}},
		},
	}}
	qs := []Question{{Name: "allowedRepos", Type: QResourceList,
		Binding: []Binding{{Target: "AgentClass/demo-agent#spec.config.allowedRepos[]"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{cls}, qs,
		Answers{"allowedRepos": []string{"myorg/*", "other/repo"}}))

	got, _, err := unstructured.NestedStringSlice(cls.Object, "spec", "config", "allowedRepos")
	require.NoError(t, err)
	assert.Equal(t, []string{"myorg/*", "other/repo"}, got,
		"answer appended onto the pre-declared empty list at spec.config.allowedRepos")
}

func TestApply_MissingTargetCR_Errors(t *testing.T) {
	qs := []Question{{Name: "endpoint", Type: QString, Binding: []Binding{{Target: "MCPServer/nope#spec.upstream.endpoint"}}}}
	err := Apply([]*unstructured.Unstructured{agentClassFixture()}, qs, Answers{"endpoint": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found among bundle manifests")
}

func TestApply_UnansweredOptional_NoChange(t *testing.T) {
	mcp := mcpFixture()
	qs := []Question{{Name: "endpoint", Type: QString, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream.endpoint"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{mcp}, qs, Answers{})) // no answer
	got, _, _ := unstructured.NestedString(mcp.Object, "spec", "upstream", "endpoint")
	assert.Equal(t, "OLD", got, "sentinel default preserved when unanswered")
}

// namedListFixture has a list whose elements are selected by the default "name"
// discriminator (not boundEntities' resourceType), exercising discriminatorFor's
// default branch.
func namedListFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "MCPServer",
		"metadata":   map[string]any{"name": "pm-linear"},
		"spec": map[string]any{
			"members": []any{
				map[string]any{"name": "lead", "role": "OLD"},
			},
		},
	}}
}

// ambiguousFixture has two boundEntities sharing resourceType=github_repo.
func ambiguousFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "pm"},
		"spec": map[string]any{
			"boundEntities": []any{
				map[string]any{"resourceType": "github_repo", "defaults": []any{"a"}},
				map[string]any{"resourceType": "github_repo", "defaults": []any{"b"}},
			},
		},
	}}
}

// TestApply_SecretQuestionWithBinding_NotOverlaid proves the QSecret skip guard
// is load-bearing: a secret question WITH a real binding + a supplied answer must
// leave its target CR byte-for-byte unchanged (secrets drive Secret creation
// elsewhere, never a CR field overlay).
func TestApply_SecretQuestionWithBinding_NotOverlaid(t *testing.T) {
	mcp := mcpFixture()
	before := mcp.DeepCopy()
	qs := []Question{{
		Name:    "tok",
		Type:    QSecret,
		Secret:  &SecretQuestion{OrExisting: true},
		Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream.endpoint"}},
	}}
	require.NoError(t, Apply([]*unstructured.Unstructured{mcp}, qs, Answers{"tok": "SECRET"}))
	assert.Equal(t, before.Object, mcp.Object, "secret answer must not overlay a CR field")
}

func TestApply_NoMatchingListElement_Errors(t *testing.T) {
	ac := agentClassFixture()
	qs := []Question{{Name: "repos", Type: QResourceList, Binding: []Binding{{Target: "AgentClass/pm#spec.boundEntities[nomatch].defaults"}}}}
	err := Apply([]*unstructured.Unstructured{ac}, qs, Answers{"repos": []string{"x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no boundEntities element with resourceType="nomatch"`)
}

func TestApply_AmbiguousSelector_FailsClosed(t *testing.T) {
	ac := ambiguousFixture()
	qs := []Question{{Name: "repos", Type: QResourceList, Binding: []Binding{{Target: "AgentClass/pm#spec.boundEntities[github_repo].defaults"}}}}
	err := Apply([]*unstructured.Unstructured{ac}, qs, Answers{"repos": []string{"x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `multiple boundEntities elements with resourceType="github_repo"`)
}

func TestApply_IntCoercedToInt64(t *testing.T) {
	mcp := mcpFixture()
	qs := []Question{{Name: "timeout", Type: QInt, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream.timeout"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{mcp}, qs, Answers{"timeout": 30}))
	upstream := mcp.Object["spec"].(map[string]any)["upstream"].(map[string]any)
	assert.Equal(t, int64(30), upstream["timeout"], "int answer must be coerced to int64")
}

func TestApply_DefaultDiscriminatorName(t *testing.T) {
	mcp := namedListFixture()
	qs := []Question{{Name: "role", Type: QString, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.members[lead].role"}}}}
	require.NoError(t, Apply([]*unstructured.Unstructured{mcp}, qs, Answers{"role": "admin"}))
	members, _, _ := unstructured.NestedSlice(mcp.Object, "spec", "members")
	assert.Equal(t, "admin", members[0].(map[string]any)["role"])
}

func TestApply_IntermediateNotAMap_Errors(t *testing.T) {
	mcp := mcpFixture() // spec.upstream.endpoint is a string
	qs := []Question{{Name: "x", Type: QString, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream.endpoint.deeper"}}}}
	err := Apply([]*unstructured.Unstructured{mcp}, qs, Answers{"x": "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `path element "endpoint" is not a map`)
}

func TestApply_SelectorOnNonList_Errors(t *testing.T) {
	mcp := mcpFixture() // spec.upstream is a map, not a list
	qs := []Question{{Name: "x", Type: QString, Binding: []Binding{{Target: "MCPServer/pm-linear#spec.upstream[x].foo"}}}}
	err := Apply([]*unstructured.Unstructured{mcp}, qs, Answers{"x": "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `path element "upstream" is not a list`)
}

func TestApply_SelectorAsFinalSegment_Errors(t *testing.T) {
	ac := agentClassFixture()
	qs := []Question{{Name: "repos", Type: QResourceList, Binding: []Binding{{Target: "AgentClass/pm#spec.boundEntities[github_repo]"}}}}
	err := Apply([]*unstructured.Unstructured{ac}, qs, Answers{"repos": []string{"x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be the final path segment")
}
