package oap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/oaptest"
)

func TestFromFolderLoadsPrivateDependencyTree(t *testing.T) {
	b, err := FromFolder(oaptest.WriteDependencyBundle(t))
	require.NoError(t, err)
	require.Len(t, b.Dependencies, 1)
	assert.Equal(t, oaptest.DependencyChildName, b.Dependencies[0].Bundle.Manifest.Agent.Name)
	assert.Equal(t, DependencyPath{oaptest.DependencyChildName}, b.Dependencies[0].Path)
}

func TestFromFolderRejectsMissingDependency(t *testing.T) {
	dir := writeFolderBundle(t, "fixture-root", []string{"fixture-child"}, []string{"dependencies/fixture-child"})

	_, err := FromFolder(dir)
	require.ErrorContains(t, err, "fixture-root > fixture-child")
	require.ErrorContains(t, err, "dependency path")
}

func TestFromFolderRejectsSymlinkDependency(t *testing.T) {
	dir := writeFolderBundle(t, "fixture-root", []string{"fixture-child"}, []string{"dependencies/fixture-child"})
	real := writeFolderBundle(t, "fixture-child", nil, nil)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dependencies"), 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(dir, "dependencies", "fixture-child")))

	_, err := FromFolder(dir)
	require.ErrorContains(t, err, "fixture-root > fixture-child")
	require.ErrorContains(t, err, "symlink")
}

func TestFromFolderRejectsSymlinkInDependencyPath(t *testing.T) {
	dir := writeFolderBundle(t, "fixture-root", []string{"fixture-child"}, []string{"dependencies/private/fixture-child"})
	realParent := t.TempDir()
	child := filepath.Join(realParent, "fixture-child")
	require.NoError(t, os.MkdirAll(filepath.Join(child, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(child, "oap.yaml"), []byte("oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(child, "manifests", "agent.yaml"), []byte(agentClassYAML("fixture-child", nil)), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dependencies"), 0o755))
	require.NoError(t, os.Symlink(realParent, filepath.Join(dir, "dependencies", "private")))

	_, err := FromFolder(dir)
	require.ErrorContains(t, err, "fixture-root > fixture-child")
	require.ErrorContains(t, err, "symlink component \"private\"")
}

func TestFromFolderRejectsChildWithTwoAgentClasses(t *testing.T) {
	dir := writeFolderBundle(t, "fixture-root", []string{"fixture-child"}, []string{"dependencies/fixture-child"})
	child := filepath.Join(dir, "dependencies", "fixture-child")
	require.NoError(t, os.MkdirAll(filepath.Join(child, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(child, "oap.yaml"), []byte("oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	two := agentClassYAML("fixture-child", nil) + "\n---\n" + agentClassYAML("second-child", nil)
	require.NoError(t, os.WriteFile(filepath.Join(child, "manifests", "agent.yaml"), []byte(two), 0o644))

	_, err := FromFolder(dir)
	require.ErrorContains(t, err, "fixture-root > fixture-child")
	require.ErrorContains(t, err, "exactly one AgentClass; found 2")
}

func TestValidateDependencyGraphRejectsDuplicateSiblingName(t *testing.T) {
	b := dependencyBundle(t, "root", dependencyBundle(t, "helper"), dependencyBundle(t, "helper"))

	err := ValidateDependencyGraph(b)
	require.ErrorContains(t, err, "duplicate sibling dependency name")
}

func TestValidateDependencyGraphRejectsNormalizedSiblingCollision(t *testing.T) {
	b := dependencyBundle(t, "root", dependencyBundle(t, "Foo_Bar"), dependencyBundle(t, "foo-bar"))
	err := ValidateDependencyGraph(b)
	require.ErrorContains(t, err, "normalize to the same Kubernetes name")
}

func TestNormalizeDependencyName(t *testing.T) {
	assert.Equal(t, "foo-bar-baz", NormalizeDependencyName("--Foo__Bar...Baz--"))
	assert.Empty(t, NormalizeDependencyName("___"))
}

func TestValidateDependencyGraphRejectsChildAbsentFromRoster(t *testing.T) {
	b := dependencyBundle(t, "fixture-root", dependencyBundle(t, "fixture-child"))
	b.Manifests = []byte(agentClassYAML("fixture-root", []string{"external-worker"}))

	err := ValidateDependencyGraph(b)
	require.ErrorContains(t, err, "fixture-root > fixture-child")
	require.ErrorContains(t, err, "spec.subagents")
}

func TestValidateDependencyGraphAllowsExternalRosterEntry(t *testing.T) {
	b := dependencyBundle(t, "fixture-root", dependencyBundle(t, "fixture-child"))
	b.Manifests = []byte(agentClassYAML("fixture-root", []string{"fixture-child", "external-worker"}))

	require.NoError(t, ValidateDependencyGraph(b))
}

func TestValidateDependencyGraphRejectsDepthLimit(t *testing.T) {
	b := dependencyBundle(t, "leaf")
	for i := 0; i < maxDependencyDepth+1; i++ {
		b = dependencyBundle(t, fmt.Sprintf("parent-%d", i), b)
	}

	err := ValidateDependencyGraph(b)
	require.ErrorContains(t, err, "maximum depth")
}

func TestValidateDependencyGraphRejectsArtifactLimit(t *testing.T) {
	children := make([]*Bundle, maxDependencyArtifacts)
	for i := range children {
		children[i] = dependencyBundle(t, fmt.Sprintf("child-%d", i))
	}
	b := dependencyBundle(t, "root", children...)

	err := ValidateDependencyGraph(b)
	require.ErrorContains(t, err, "maximum artifact count")
}

func TestWalkDependenciesOrder(t *testing.T) {
	b := dependencyBundle(t, "root",
		dependencyBundle(t, "first", dependencyBundle(t, "leaf")),
		dependencyBundle(t, "second"),
	)

	var parents []string
	require.NoError(t, WalkDependencies(b, ParentsFirst, func(_ DependencyPath, node *Bundle) error {
		parents = append(parents, node.Manifest.Agent.Name)
		return nil
	}))
	assert.Equal(t, []string{"root", "first", "leaf", "second"}, parents)

	var leaves []string
	require.NoError(t, WalkDependencies(b, LeavesFirst, func(_ DependencyPath, node *Bundle) error {
		leaves = append(leaves, node.Manifest.Agent.Name)
		return nil
	}))
	assert.Equal(t, []string{"leaf", "first", "second", "root"}, leaves)
}

func dependencyBundle(t *testing.T, name string, children ...*Bundle) *Bundle {
	t.Helper()
	roster := make([]string, 0, len(children))
	b := &Bundle{
		Manifest: &Manifest{
			OapFormatVersion: "1",
			Agent:            Agent{Name: name, Version: "1.0.0"},
		},
		Manifests: []byte(agentClassYAML(name, nil)),
		Assets:    map[string][]byte{},
	}
	for _, child := range children {
		roster = append(roster, child.Manifest.Agent.Name)
		packed, err := Pack(child)
		require.NoError(t, err)
		descriptor := RequiredAgent{
			Name:      child.Manifest.Agent.Name,
			Version:   child.Manifest.Agent.Version,
			MediaType: DependencyMediaType,
			Digest:    digest.FromBytes(packed).String(),
		}
		b.Manifest.Requires.Agents = append(b.Manifest.Requires.Agents, descriptor)
		b.Dependencies = append(b.Dependencies, &Dependency{
			Descriptor: descriptor,
			Bundle:     child,
			Packed:     packed,
		})
	}
	b.Manifests = []byte(agentClassYAML(name, roster))
	assignDependencyPaths(b, nil)
	return b
}

func mustFromFolder(t *testing.T, path string) *Bundle {
	t.Helper()
	b, err := FromFolder(path)
	require.NoError(t, err)
	return b
}

func assignDependencyPaths(b *Bundle, prefix DependencyPath) {
	for _, dep := range b.Dependencies {
		dep.Path = append(append(DependencyPath(nil), prefix...), dep.Bundle.Manifest.Agent.Name)
		assignDependencyPaths(dep.Bundle, dep.Path)
	}
}

func writeFolderBundle(t *testing.T, name string, roster, dependencyPaths []string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(agentClassYAML(name, roster)), 0o644))
	var manifest strings.Builder
	manifest.WriteString("oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n")
	if len(dependencyPaths) > 0 {
		manifest.WriteString("requires:\n  agents:\n")
		for _, path := range dependencyPaths {
			fmt.Fprintf(&manifest, "    - path: %s\n", path)
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(manifest.String()), 0o644))
	return dir
}

func agentClassYAML(name string, roster []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: %s\nspec:\n", name)
	if len(roster) == 0 {
		out.WriteString("  subagents: []\n")
		return out.String()
	}
	out.WriteString("  subagents:\n")
	for _, child := range roster {
		fmt.Fprintf(&out, "    - %s\n", child)
	}
	return out.String()
}
