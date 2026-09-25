package instance

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

const (
	testDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func agentClass(t *testing.T, name string, subagents []string, modes map[string][]string) *unstructured.Unstructured {
	t.Helper()
	modeValues := make(map[string]any, len(modes))
	for agent, allowed := range modes {
		values := make([]any, len(allowed))
		for i, mode := range allowed {
			values[i] = mode
		}
		modeValues[agent] = values
	}
	cr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{},
	}}
	require.NoError(t, unstructured.SetNestedStringSlice(cr.Object, subagents, "spec", "subagents"))
	require.NoError(t, unstructured.SetNestedMap(cr.Object, modeValues, "spec", "subagentModes"))
	return cr
}

func nestedStrings(t *testing.T, cr *unstructured.Unstructured, fields ...string) []string {
	t.Helper()
	got, found, err := unstructured.NestedStringSlice(cr.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "expected field %s", strings.Join(fields, "."))
	return got
}

func nestedStringSliceMap(t *testing.T, cr *unstructured.Unstructured, fields ...string) map[string][]string {
	t.Helper()
	raw, found, err := unstructured.NestedMap(cr.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "expected field %s", strings.Join(fields, "."))
	got := make(map[string][]string, len(raw))
	for key, value := range raw {
		values, ok := value.([]any)
		require.Truef(t, ok, "field %s.%s must be a string slice", strings.Join(fields, "."), key)
		for _, item := range values {
			text, ok := item.(string)
			require.Truef(t, ok, "field %s.%s must contain only strings", strings.Join(fields, "."), key)
			got[key] = append(got[key], text)
		}
	}
	return got
}

func TestDependencyName(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		path    oap.DependencyPath
		digest  string
		want    string
		wantErr string
	}{
		{name: "default root and direct child", root: "test-coordinator", path: oap.DependencyPath{"reviewer"}, digest: testDigestA, want: "test-coordinator-reviewer"},
		{name: "nested dependency path", root: "test-coordinator", path: oap.DependencyPath{"reviewer", "bat-translator"}, digest: testDigestA, want: "test-coordinator-reviewer-bat-translator"},
		{name: "explicit root install name", root: "black-pearl", path: oap.DependencyPath{"reviewer"}, digest: testDigestA, want: "black-pearl-reviewer"},
		{name: "logical components use shared normalization", root: "private-root", path: oap.DependencyPath{"--Review__Agent...", "BAT Translator"}, digest: testDigestA, want: "private-root-review-agent-bat-translator"},
		{name: "invalid root is rejected", root: "Private_Root", path: oap.DependencyPath{"child"}, digest: testDigestA, wantErr: "root"},
		{name: "empty normalized component is rejected", root: "private-root", path: oap.DependencyPath{"___"}, digest: testDigestA, wantErr: "normalizes to empty"},
		{name: "root path is not a dependency", root: "private-root", path: nil, digest: testDigestA, wantErr: "empty dependency path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DependencyName(tc.root, tc.path, tc.digest)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Empty(t, validation.IsDNS1123Subdomain(got))
		})
	}
}

func TestDependencyNameTruncatesWithStableDigestSuffix(t *testing.T) {
	logical := strings.Repeat("a", 300)
	got, err := DependencyName("root", oap.DependencyPath{logical}, testDigestA)
	require.NoError(t, err)

	assert.Len(t, got, 253)
	assert.Equal(t, "root-"+strings.Repeat("a", 237)+"-c6f9994dc1", got)
	assert.Empty(t, validation.IsDNS1123Subdomain(got))

	again, err := DependencyName("root", oap.DependencyPath{logical}, testDigestA)
	require.NoError(t, err)
	assert.Equal(t, got, again, "the same root, path, and digest must be stable across reinstall")

	differentArtifact, err := DependencyName("root", oap.DependencyPath{logical}, testDigestB)
	require.NoError(t, err)
	assert.Equal(t, "root-"+strings.Repeat("a", 237)+"-0b8082589f", differentArtifact)
	assert.NotEqual(t, got, differentArtifact, "the same truncated logical name must remain distinct for a different artifact")
}

func TestBuildGraphNamesDisambiguatesFlattenedEqualDigestEdges(t *testing.T) {
	grandchild := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "grand"}}}
	child := &oap.Bundle{
		Manifest: &oap.Manifest{Agent: oap.Agent{Name: "child"}},
		Dependencies: []*oap.Dependency{{
			Descriptor: oap.RequiredAgent{Name: "grand", Digest: testDigestA},
			Path:       oap.DependencyPath{"child", "grand"},
			Bundle:     grandchild,
		}},
	}
	direct := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "child-grand"}}}
	root := &oap.Bundle{
		Manifest: &oap.Manifest{Agent: oap.Agent{Name: "fixture-root"}},
		Dependencies: []*oap.Dependency{
			{Descriptor: oap.RequiredAgent{Name: "child-grand", Digest: testDigestA}, Path: oap.DependencyPath{"child-grand"}, Bundle: direct},
			{Descriptor: oap.RequiredAgent{Name: "child", Digest: testDigestC}, Path: oap.DependencyPath{"child"}, Bundle: child},
		},
	}

	got, err := BuildGraphNames("private-root", root)
	require.NoError(t, err)
	assert.Equal(t, "private-root", got[""])
	assert.Equal(t, "private-root-child", got[oap.DependencyPath{"child"}.String()])
	assert.Equal(t, "private-root-child-grand-8977496002", got[oap.DependencyPath{"child-grand"}.String()])
	assert.Equal(t, "private-root-child-grand-1232cf4e49", got[oap.DependencyPath{"child", "grand"}.String()])
}

func TestBuildGraphNamesDisambiguatesTruncatedEqualDigestEdges(t *testing.T) {
	firstPath := strings.Repeat("a", 300)
	secondPath := strings.Repeat("a", 299) + "b"
	root := &oap.Bundle{
		Manifest: &oap.Manifest{Agent: oap.Agent{Name: "fixture-root"}},
		Dependencies: []*oap.Dependency{
			{
				Descriptor: oap.RequiredAgent{Name: firstPath, Digest: testDigestA},
				Path:       oap.DependencyPath{firstPath},
				Bundle:     &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: firstPath}}},
			},
			{
				Descriptor: oap.RequiredAgent{Name: secondPath, Digest: testDigestA},
				Path:       oap.DependencyPath{secondPath},
				Bundle:     &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: secondPath}}},
			},
		},
	}

	got, err := BuildGraphNames("root", root)
	require.NoError(t, err)
	assert.Equal(t, "root-"+strings.Repeat("a", 237)+"-c6f9994dc1", got[oap.DependencyPath{firstPath}.String()])
	assert.Equal(t, "root-"+strings.Repeat("a", 237)+"-45985c0143", got[oap.DependencyPath{secondPath}.String()])
}

func TestBuildGraphNamesKeepsEqualDigestEdgesPrivate(t *testing.T) {
	root := &oap.Bundle{
		Manifest: &oap.Manifest{Agent: oap.Agent{Name: "fixture-root"}},
		Dependencies: []*oap.Dependency{
			{
				Descriptor: oap.RequiredAgent{Name: "alpha", Digest: testDigestA},
				Path:       oap.DependencyPath{"alpha"},
				Bundle:     &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "alpha"}}},
			},
			{
				Descriptor: oap.RequiredAgent{Name: "beta", Digest: testDigestA},
				Path:       oap.DependencyPath{"beta"},
				Bundle:     &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "beta"}}},
			},
		},
	}

	got, err := BuildGraphNames("private-root", root)
	require.NoError(t, err)
	assert.Equal(t, "private-root-alpha", got[oap.DependencyPath{"alpha"}.String()])
	assert.Equal(t, "private-root-beta", got[oap.DependencyPath{"beta"}.String()])
}

func TestNameMapForDependencyPrefixesLocalResourcesWithPrivateAgent(t *testing.T) {
	class := agentClass(t, "bat-translator", nil, nil)
	prompt := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "prompt"},
	}}

	got, err := NameMapForDependency(
		[]*unstructured.Unstructured{class, prompt},
		"black-pearl",
		oap.DependencyPath{"Review__Agent", "bat-translator"},
		testDigestA,
		"black-pearl-reviewer-bat-translator",
	)
	require.NoError(t, err)
	assert.Equal(t, NameMap{
		"AgentClass/bat-translator": "black-pearl-reviewer-bat-translator",
		"ConfigMap/prompt":          "black-pearl-reviewer-bat-translator-prompt",
	}, got)
}

func TestNameMapForDependencyTruncatesEachResourceWithArtifactDigest(t *testing.T) {
	resourceName := strings.Repeat("r", 250)
	resource := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": resourceName},
	}}

	got, err := NameMapForDependency(
		[]*unstructured.Unstructured{resource},
		"root",
		oap.DependencyPath{"child"},
		testDigestA,
		"root-child",
	)
	require.NoError(t, err)
	physical := got["ConfigMap/"+resourceName]
	assert.Len(t, physical, 253)
	assert.Equal(t, "root-child-"+strings.Repeat("r", 231)+"-01873f16ac", physical)
	assert.Empty(t, validation.IsDNS1123Subdomain(physical))
}

func TestNameMapForDependencyUsesGraphResolvedAgentClassName(t *testing.T) {
	class := agentClass(t, "child-grand", nil, nil)
	physicalClass := "private-root-child-grand-8977496002"

	got, err := NameMapForDependency(
		[]*unstructured.Unstructured{class},
		"private-root",
		oap.DependencyPath{"child-grand"},
		testDigestA,
		physicalClass,
	)
	require.NoError(t, err)
	assert.Equal(t, physicalClass, got["AgentClass/child-grand"])
}

func TestNameMapForDependencyKeepsSiblingResourcesPrivate(t *testing.T) {
	for _, kind := range []string{"Secret", "ConfigMap"} {
		t.Run(kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": "shared-local"}}}
			first, err := NameMapForDependency([]*unstructured.Unstructured{obj}, "root", oap.DependencyPath{"first"}, testDigestA, "root-first")
			require.NoError(t, err)
			second, err := NameMapForDependency([]*unstructured.Unstructured{obj}, "root", oap.DependencyPath{"second"}, testDigestA, "root-second")
			require.NoError(t, err)
			assert.Equal(t, "root-first-shared-local", first[kind+"/shared-local"])
			assert.Equal(t, "root-second-shared-local", second[kind+"/shared-local"])
		})
	}
}

func TestRewriteSubagentRefsLeavesExternalRosterMembers(t *testing.T) {
	cr := agentClass(t, "coordinator", []string{"reviewer", "shared-reviewer"}, map[string][]string{"reviewer": {"conversation"}})
	err := RewriteSubagentRefs(cr, map[string]string{"reviewer": "coordinator-reviewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"coordinator-reviewer", "shared-reviewer"}, nestedStrings(t, cr, "spec", "subagents"))
	assert.Equal(t, []string{"conversation"}, nestedStringSliceMap(t, cr, "spec", "subagentModes")["coordinator-reviewer"])
	assert.NotContains(t, nestedStringSliceMap(t, cr, "spec", "subagentModes"), "reviewer")
}

func TestRewriteSubagentRefsPreservesDigestPins(t *testing.T) {
	pin := "sha256:" + strings.Repeat("d", 64)
	cr := agentClass(t, "parent", []string{"child@" + pin}, map[string][]string{"child": {"task"}})

	require.NoError(t, RewriteSubagentRefs(cr, map[string]string{"child": "private-root-child"}))
	assert.Equal(t, []string{"private-root-child@" + pin}, nestedStrings(t, cr, "spec", "subagents"))
	assert.Equal(t, []string{"task"}, nestedStringSliceMap(t, cr, "spec", "subagentModes")["private-root-child"])
}
