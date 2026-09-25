package workshopmcp

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// newInventoryTestServer builds a Server around a fake client seeded with a
// ClusterAgentSettings singleton (handleInventory Gets it unconditionally, so
// every test below needs one to reach a success response) plus whatever
// extra objects the caller supplies.
func newInventoryTestServer(t *testing.T, namespace string, cas *spiceboxv1alpha1.ClusterAgentSettings, extra ...client.Object) *Server {
	t.Helper()
	if cas == nil {
		cas = &spiceboxv1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		}
	}
	objs := append([]client.Object{cas}, extra...)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(objs...).Build()
	return &Server{
		K8s: c,
		Identity: WorkshopIdentity{
			Namespace:        namespace,
			SessionNamespace: "b",
			SessionName:      "x",
			WorkshopID:       namespace,
		},
		FieldOwner: defaultFieldOwner,
	}
}

// callInventory invokes the handler directly (no MCP transport round trip —
// server_test.go's TestServer_HealthAndToolsList already proves the tool is
// reachable over the wire) and decodes its JSON body.
func callInventory(t *testing.T, s *Server) inventorySnapshot {
	t.Helper()
	res, err := s.handleInventory(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err, "handleInventory must not return a protocol-level error")
	require.False(t, res.IsError, "handleInventory must not return an error result for a well-formed fixture")
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "inventory's content block must be text")
	var snap inventorySnapshot
	require.NoError(t, json.Unmarshal([]byte(text.Text), &snap), "inventory body must be valid JSON")
	return snap
}

// TestInventory_ReportsRegisteredSets seeds a fake cluster with one of each
// CR-backed field and asserts inventory reports it — including that it
// EXCLUDES an MCPServer living outside the workshop's own namespace (never
// enumerate another agent's tools).
func TestInventory_ReportsRegisteredSets(t *testing.T) {
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{
				{Name: "demo-model-opus", Provider: "anthropic", Default: true},
				{Name: "demo-model-haiku", Provider: "anthropic"},
			},
			Limits: &spiceboxv1alpha1.SettingsLimits{
				DeniedModels: []string{"demo-model-forbidden"},
				Budget:       &spiceboxv1alpha1.SettingsBudgetCeiling{MaxTurns: 40},
			},
		},
	}
	clusterSkill := &spiceboxv1alpha1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-skill"},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: "example.test/demo//skills/demo@v1"},
	}
	inWorkshop := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp-in-workshop", Namespace: "ws-demo123"},
	}
	outsideWorkshop := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-mcp-other-agent", Namespace: "ws-someoneelse"},
	}
	classA := &spiceboxv1alpha1.SpiceboxClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-class-standard"}}
	classB := &spiceboxv1alpha1.SpiceboxClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-class-gpu"}}

	s := newInventoryTestServer(t, "ws-demo123", cas, clusterSkill, inWorkshop, outsideWorkshop, classA, classB)
	snap := callInventory(t, s)

	assert.NotEmpty(t, snap.ChannelKinds, "ChannelKinds must equal the registered set, non-empty")
	assert.ElementsMatch(t, chregistry.Names(), snap.ChannelKinds)
	assert.NotEmpty(t, snap.Renderers, "Renderers must equal the registered set, non-empty")
	assert.ElementsMatch(t, rendererNames(), snap.Renderers)
	assert.NotEmpty(t, snap.Capabilities, "Capabilities must equal the registered set, non-empty")
	assert.ElementsMatch(t, capabilityNames(), snap.Capabilities)
	assert.NotEmpty(t, snap.SandboxBackends, "SandboxBackends must equal the registered set, non-empty")
	assert.ElementsMatch(t, sandboxregistry.Keys(), snap.SandboxBackends)
	assert.ElementsMatch(t, []string{"demo-class-standard", "demo-class-gpu"}, snap.SandboxClasses,
		"SandboxClasses must report SpiceboxClass CR NAMES, not sandbox backend kinds — a SidecarToolbox names one of these via spec.sandbox.class")

	assert.ElementsMatch(t, []string{"demo-model-opus", "demo-model-haiku"}, snap.Models)
	assert.ElementsMatch(t, []string{"example.test/demo//skills/demo@v1"}, snap.ClusterSkills)
	assert.ElementsMatch(t, []string{"demo-mcp-in-workshop"}, snap.MCPCatalog,
		"MCPCatalog must list only this workshop's own MCPServers, never another agent's")

	require.NotNil(t, snap.Ceilings)
	deniedModels, _ := snap.Ceilings["deniedModels"].([]any)
	require.Len(t, deniedModels, 1)
	assert.Equal(t, "demo-model-forbidden", deniedModels[0])
	budget, _ := snap.Ceilings["budget"].(map[string]any)
	require.NotNil(t, budget, "ceilings must carry the budget ceiling")
	assert.Equal(t, float64(40), budget["maxTurns"])
}

// TestInventory_NilLimitsReportsEmptyCeilings pins the nil-safe path: a
// cluster with no Limits at all still answers with a non-nil, empty ceilings
// map — never a nil map (which would marshal as JSON null and force every
// caller to nil-check) and never an error.
func TestInventory_NilLimitsReportsEmptyCeilings(t *testing.T) {
	s := newInventoryTestServer(t, "ws-demo123", nil)
	snap := callInventory(t, s)
	assert.NotNil(t, snap.Ceilings)
	assert.Empty(t, snap.Ceilings)
	assert.NotNil(t, snap.Models)
	assert.Empty(t, snap.Models)
}

// TestInventory_MissingClusterAgentSettingsFailsClosed proves inventory
// surfaces the read failure rather than silently reporting an empty/partial
// snapshot as if it were a real answer, when the singleton is entirely
// absent.
func TestInventory_MissingClusterAgentSettingsFailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := &Server{K8s: c, Identity: WorkshopIdentity{Namespace: "ws-demo123"}, FieldOwner: defaultFieldOwner}

	res, err := s.handleInventory(context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError, "a missing ClusterAgentSettings singleton must fail closed, not report an empty snapshot")
}

// TestInventory_RegistryParityWithRunner is the drift test: it proves two
// separate claims.
//
//  1. Runtime parity — the handler reports EXACTLY what each in-process
//     registry holds in this binary, not a hardcoded or filtered subset. This
//     catches a bug in handleInventory itself (e.g. a typo'd allowlist).
//  2. Source parity — registries.go's blank-imported channel-kind and
//     artifact-renderer packages are the SAME set internal/cmd/runner/main.go
//     blank-imports. This is the drift registries.go's own header comment
//     promises a test for: add a channel kind to the runner and forget the
//     sidecar (or the reverse) and this fails, because runtime parity alone
//     cannot catch it — a package neither binary links is "in sync" by
//     runtime comparison alone, since both sides of that comparison would
//     read from the SAME under-populated registry.
//
// Sandbox kinds are checked against the OPERATOR's blank imports instead of
// the runner's: the runner never provisions a sandbox (SpiceboxSession/
// ToolCall are operator-reconciled), so there is no runner-side import list
// for SandboxClasses to mirror — the operator is the actual production
// source of truth for which sandbox backends exist.
func TestInventory_RegistryParityWithRunner(t *testing.T) {
	repoRoot := repoRootFromWorkshopPkg(t)
	runnerMain := filepath.Join(repoRoot, "internal", "cmd", "runner", "main.go")
	operatorMain := filepath.Join(repoRoot, "internal", "cmd", "operator", "main.go")
	registries := filepath.Join(repoRoot, "internal", "cmd", "workshop", "registries.go")

	t.Run("source: workshop links the same channel kinds as the runner", func(t *testing.T) {
		want := blankImportsUnderPrefix(t, runnerMain, "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/")
		got := blankImportsUnderPrefix(t, registries, "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/")
		assert.NotEmpty(t, want, "sanity: the runner must itself link at least one channel kind")
		assert.ElementsMatch(t, want, got, "registries.go's channel-kind blank imports must match internal/cmd/runner/main.go's")
	})

	t.Run("source: workshop links the same artifact renderers as the runner", func(t *testing.T) {
		want := blankImportsUnderPrefix(t, runnerMain, "github.com/authzed/openagentprimitives/pkg/channels/channelassets/")
		got := blankImportsUnderPrefix(t, registries, "github.com/authzed/openagentprimitives/pkg/channels/channelassets/")
		assert.NotEmpty(t, want, "sanity: the runner must itself link at least one renderer")
		assert.ElementsMatch(t, want, got, "registries.go's renderer blank imports must match internal/cmd/runner/main.go's")
	})

	t.Run("source: workshop links the same sandbox kinds as the operator (the runner provisions none)", func(t *testing.T) {
		want := blankImportsUnderPrefix(t, operatorMain, "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/")
		got := blankImportsUnderPrefix(t, registries, "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/")
		assert.NotEmpty(t, want, "sanity: the operator must itself link at least one sandbox kind")
		assert.ElementsMatch(t, want, got, "registries.go's sandbox-kind blank imports must match internal/cmd/operator/main.go's")
	})

	t.Run("runtime: reported sets equal each registry's own listing, non-empty", func(t *testing.T) {
		s := newInventoryTestServer(t, "ws-demo123", nil)
		snap := callInventory(t, s)

		assert.ElementsMatch(t, chregistry.Names(), snap.ChannelKinds)
		assert.NotEmpty(t, snap.ChannelKinds)
		assert.ElementsMatch(t, rendererNames(), snap.Renderers)
		assert.NotEmpty(t, snap.Renderers)
		assert.ElementsMatch(t, capabilityNames(), snap.Capabilities)
		assert.NotEmpty(t, snap.Capabilities)
		assert.ElementsMatch(t, sandboxregistry.Keys(), snap.SandboxBackends)
		assert.NotEmpty(t, snap.SandboxBackends)
	})
}

// TestInventory_SidecarImages_ReportsAdmissibleRefs proves inventory reports
// exactly apimage.WorkshopSidecarImages — derived here, never transcribed, so
// a third image added there is caught by this test automatically rather than
// silently passing unchanged — with a bare local Ref when
// WORKSHOP_TRUSTED_IMAGE_REGISTRY is unset and a registry-qualified Ref when
// it is set, and a non-empty Purpose for every entry (a third image whose
// apimage.Image.WorkshopPurpose is left unset would report an empty Purpose
// here and fail this test, rather than silently passing).
func TestInventory_SidecarImages_ReportsAdmissibleRefs(t *testing.T) {
	wantNames := make([]string, 0, len(apimage.WorkshopSidecarImages))
	for _, img := range apimage.WorkshopSidecarImages {
		wantNames = append(wantNames, img.Name)
	}

	t.Run("no trusted registry: bare local refs", func(t *testing.T) {
		s := newInventoryTestServer(t, "ws-demo123", nil)
		snap := callInventory(t, s)

		require.Len(t, snap.SidecarImages, len(apimage.WorkshopSidecarImages))
		gotNames := make([]string, 0, len(snap.SidecarImages))
		for i, si := range snap.SidecarImages {
			gotNames = append(gotNames, si.Name)
			assert.Equal(t, apimage.WorkshopSidecarImages[i].LocalRef(), si.Ref,
				"Ref must be the bare local tag when this cluster has no trusted registry")
			assert.NotEmpty(t, si.Purpose, "every reported sidecar image must carry a non-empty Purpose")
		}
		assert.ElementsMatch(t, wantNames, gotNames,
			"SidecarImages must report exactly apimage.WorkshopSidecarImages, no more, no less")
	})

	t.Run("trusted registry set: registry-qualified refs", func(t *testing.T) {
		t.Setenv("WORKSHOP_TRUSTED_IMAGE_REGISTRY", "registry.example.test")
		s := newInventoryTestServer(t, "ws-demo123", nil)
		snap := callInventory(t, s)

		require.Len(t, snap.SidecarImages, len(apimage.WorkshopSidecarImages))
		for i, si := range snap.SidecarImages {
			assert.Equal(t, apimage.WorkshopSidecarImages[i].RegistryRef("registry.example.test"), si.Ref,
				"Ref must be registry-qualified when this cluster has a trusted registry")
		}
	})
}

// TestInventory_ReportsProviderCatalog proves inventory reports the /providers/
// catalog — derived from provider.All(), never transcribed, so a provider
// added to the embed is reported automatically rather than silently passing
// unchanged — with a non-empty Name for every entry, and the generic OAuth-MCP
// sign-in present with shape "oauth", the fact the builder-tools skill's
// provider guidance rests on (name oauth-mcp for a per-person OAuth service).
func TestInventory_ReportsProviderCatalog(t *testing.T) {
	s := newInventoryTestServer(t, "ws-demo123", nil)
	snap := callInventory(t, s)

	require.NotEmpty(t, snap.Providers, "inventory must report the provider catalog")

	wantIDs := make([]string, 0, len(provider.All()))
	for _, p := range provider.All() {
		wantIDs = append(wantIDs, p.ID)
	}
	gotIDs := make([]string, 0, len(snap.Providers))
	var oauthMCP *providerInfo
	for i := range snap.Providers {
		pi := &snap.Providers[i]
		assert.NotEmpty(t, pi.Name, "every reported provider must carry a non-empty name")
		gotIDs = append(gotIDs, pi.Name)
		if pi.Name == "oauth-mcp" {
			oauthMCP = pi
		}
	}
	assert.ElementsMatch(t, wantIDs, gotIDs,
		"Providers must report exactly provider.All(), no more, no less")

	require.NotNil(t, oauthMCP, "the generic OAuth-MCP sign-in must be in the catalog")
	assert.Equal(t, "oauth", oauthMCP.Shape,
		"oauth-mcp is the generic per-person OAuth sign-in the builder-tools skill names")
}

// blankImportsUnderPrefix parses the Go source file at path and returns every
// blank ("_") import whose path starts with prefix, with prefix stripped —
// e.g. "slack" from ".../pkg/channels/channelkinds/slack". Only the import
// block is parsed (parser.ImportsOnly): this reads real source, not build
// output, so it works whether or not the parsed file's own package compiles
// in isolation.
func blankImportsUnderPrefix(t *testing.T, path, prefix string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	require.NoError(t, err, "parse %s", path)

	var out []string
	for _, imp := range f.Imports {
		if imp.Name == nil || imp.Name.Name != "_" {
			continue
		}
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err, "unquote import path in %s", path)
		if rest, ok := strings.CutPrefix(p, prefix); ok && rest != "" {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

// repoRootFromWorkshopPkg returns the repo root, derived from this test
// file's own package location (pkg/tools/workshopmcp, three levels below
// root — the same depth internal/cmd/workshop was, which is why the ".."
// count below didn't need to change when this file moved) rather than
// hardcoded as an absolute path — go test's working directory is always the
// package directory.
func repoRootFromWorkshopPkg(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join(".", "..", "..", ".."))
	require.NoError(t, err)
	return root
}
