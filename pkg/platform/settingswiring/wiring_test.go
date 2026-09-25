package settingswiring

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1.AddToScheme(s)
	return s
}

func TestFetchTiers_BothAbsent_ReturnsNil(t *testing.T) {
	c := newClient()
	cl, ns, err := FetchTiers(context.Background(), c, "team-a")
	require.NoError(t, err)
	assert.Nil(t, cl)
	assert.Nil(t, ns)
}

func TestClassRefs_MCPPins(t *testing.T) {
	// An MCPServer without a pinnedManifestHash → DeclaredPin strength=unpinned.
	// An MCPServer with a pinnedManifestHash → DeclaredPin strength=frozen.
	unasserted := &v1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gh"},
		Spec:       v1.MCPServerSpec{Name: "gh", Version: "1"},
	}
	asserted := &v1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "linear"},
		Spec: v1.MCPServerSpec{
			Name:               "linear",
			Version:            "1",
			PinnedManifestHash: "sha256:abc123",
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			MCPServers: []v1.AgentClassMCPServerRef{
				{Ref: "gh"},
				{Ref: "linear"},
			},
		},
	}
	c := newClient(unasserted, asserted, class)
	_, _, pins, err := ClassRefs(context.Background(), c, class)
	require.NoError(t, err)
	require.Len(t, pins, 2, "one DeclaredPin per MCPServer ref")

	byName := map[string]string{}
	for _, p := range pins {
		byName[p.Name] = p.Strength
	}
	assert.Equal(t, "unpinned", byName["gh"], "unasserted server → unpinned")
	assert.Equal(t, "frozen", byName["linear"], "asserted server → frozen")
}

func TestResolveForClass_MCPPinBlocksUnasserted(t *testing.T) {
	// A cluster rule {kind: mcp, minStrength: frozen, mode: block} + an
	// AgentClass referencing an unasserted MCPServer → fatal PinningRequired violation.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "mcp",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	unassertedSrv := &v1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gh"},
		Spec:       v1.MCPServerSpec{Name: "gh", Version: "1"},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			MCPServers: []v1.AgentClassMCPServerRef{{Ref: "gh"}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, unassertedSrv, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	require.True(t, HasFatal(vs), "unasserted MCPServer against frozen-required rule must produce a fatal violation")
	require.Equal(t, settings.ReasonPinningRequired, FirstFatal(vs).Reason)
}

func TestResolveForClass_MCPPinAllowsAsserted(t *testing.T) {
	// Same cluster rule but the MCPServer has a pinnedManifestHash → no violation.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "mcp",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	assertedSrv := &v1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gh"},
		Spec: v1.MCPServerSpec{
			Name:               "gh",
			Version:            "1",
			PinnedManifestHash: "sha256:deadbeef",
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			MCPServers: []v1.AgentClassMCPServerRef{{Ref: "gh"}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, assertedSrv, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	assert.False(t, HasFatal(vs), "asserted MCPServer satisfies frozen floor; unexpected violations: %+v", vs)
}

func TestClassRefs_CLIPins(t *testing.T) {
	// A SpiceboxToolkit with only a VersionRange → named; with PinnedBinaryHash → frozen.
	// A toolkit name whose SpiceboxToolkit CR does not exist → unpinned (embedded-catalog case).
	namedTk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tool"},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "my-tool", ToolkitRevision: "v1",
			Target: v1.ToolkitTarget{Binary: "/usr/bin/my-tool", VersionRange: ">=1.0.0"},
		},
	}
	frozenTk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "locked-tool"},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "locked-tool", ToolkitRevision: "v1",
			Target: v1.ToolkitTarget{Binary: "/usr/bin/locked-tool", PinnedBinaryHash: "sha256:abc123"},
		},
	}
	// Both fields set: frozen wins (hash takes priority over version range).
	bothTk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "both-tool"},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "both-tool", ToolkitRevision: "v1",
			Target: v1.ToolkitTarget{
				Binary:           "/usr/bin/both-tool",
				PinnedBinaryHash: "sha256:deadbeef",
				VersionRange:     ">=1.0.0",
			},
		},
	}
	// Toolspec CRs pointing to the above toolkits + one embedded-catalog toolkit (no CR).
	tsNamed := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-named"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "my-tool"}},
	}
	tsFrozen := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-frozen"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "locked-tool"}},
	}
	tsEmbedded := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-embedded"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "echo"}},
	}
	tsBoth := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-both"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "both-tool"}},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			ToolBundles: []v1.ToolBundle{
				{Toolspecs: []string{"ts-named", "ts-frozen", "ts-embedded", "ts-both"}},
			},
		},
	}
	// Only namedTk, frozenTk, and bothTk exist; "echo" toolkit CR is absent (embedded).
	c := newClient(namedTk, frozenTk, bothTk, tsNamed, tsFrozen, tsEmbedded, tsBoth, class)
	_, _, pins, err := ClassRefs(context.Background(), c, class)
	require.NoError(t, err)

	byName := map[string]string{}
	for _, p := range pins {
		if p.Kind == "cli" {
			byName[p.Name] = p.Strength
		}
	}
	assert.Equal(t, "named", byName["my-tool"], "VersionRange-only toolkit → named")
	assert.Equal(t, "frozen", byName["locked-tool"], "PinnedBinaryHash toolkit → frozen")
	assert.Equal(t, "unpinned", byName["echo"], "embedded-catalog toolkit (no CR) → unpinned")
	assert.Equal(t, "frozen", byName["both-tool"], "both PinnedBinaryHash+VersionRange → frozen wins")
}

func TestClassRefs_ImagePins(t *testing.T) {
	// A SidecarToolbox with a digest ref → frozen; with a non-latest tag → named;
	// with :latest → unpinned; with NotFound → skipped (no pin entry).
	frozenTb := &v1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tb-frozen"},
		Spec: v1.SidecarToolboxSpec{
			Name: "tb-frozen", Version: "1",
			Source:       v1.SidecarToolboxSource{Image: "ghcr.io/org/tool@sha256:deadbeef"},
			Sandbox:      v1.SidecarToolboxSandbox{Class: "default"},
			Transport:    v1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: v1.SidecarToolboxUpstream{Provider: "none"},
		},
	}
	namedTb := &v1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tb-named"},
		Spec: v1.SidecarToolboxSpec{
			Name: "tb-named", Version: "1",
			Source:       v1.SidecarToolboxSource{Image: "ghcr.io/org/tool:v1.2.3"},
			Sandbox:      v1.SidecarToolboxSandbox{Class: "default"},
			Transport:    v1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: v1.SidecarToolboxUpstream{Provider: "none"},
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			SidecarToolboxes: []v1.AgentClassSidecarToolboxRef{
				{Name: "frozen", Ref: "tb-frozen"},
				{Name: "named", Ref: "tb-named"},
				{Name: "missing", Ref: "tb-missing"}, // NotFound → skipped
			},
		},
	}
	c := newClient(frozenTb, namedTb, class)
	_, _, pins, err := ClassRefs(context.Background(), c, class)
	require.NoError(t, err)

	byName := map[string]string{}
	for _, p := range pins {
		if p.Kind == "image" {
			byName[p.Name] = p.Strength
		}
	}
	assert.Equal(t, "frozen", byName["tb-frozen"], "digest ref → frozen")
	assert.Equal(t, "named", byName["tb-named"], "tag ref → named")
	assert.NotContains(t, byName, "tb-missing", "NotFound toolbox → no pin entry")
}

func TestResolveForClass_CLIPinBlocksUnasserted(t *testing.T) {
	// A cluster rule {kind: cli, minStrength: frozen, mode: block} + an
	// AgentClass referencing a toolkit with only VersionRange → fatal PinningRequired.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "cli",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	namedTk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tool"},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "my-tool", ToolkitRevision: "v1",
			Target: v1.ToolkitTarget{Binary: "/usr/bin/my-tool", VersionRange: ">=1.0.0"},
		},
	}
	ts := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-tool"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "my-tool"}},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			ToolBundles: []v1.ToolBundle{{Toolspecs: []string{"ts-tool"}}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, namedTk, ts, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	require.True(t, HasFatal(vs), "named CLI toolkit against frozen-required rule must produce a fatal violation")
	assert.Equal(t, settings.ReasonPinningRequired, FirstFatal(vs).Reason)
}

func TestResolveForClass_CLIPinAllowsFrozen(t *testing.T) {
	// Same cluster rule but the SpiceboxToolkit has a PinnedBinaryHash → no violation.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "cli",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	frozenTk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tool"},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "my-tool", ToolkitRevision: "v1",
			Target: v1.ToolkitTarget{Binary: "/usr/bin/my-tool", PinnedBinaryHash: "sha256:abc"},
		},
	}
	ts := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-tool"},
		Spec:       v1.SpiceboxToolspecSpec{Toolkit: v1.ToolspecToolkitRef{Name: "my-tool"}},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			ToolBundles: []v1.ToolBundle{{Toolspecs: []string{"ts-tool"}}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, frozenTk, ts, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	assert.False(t, HasFatal(vs), "frozen CLI toolkit satisfies frozen floor; unexpected violations: %+v", vs)
}

func TestResolveForClass_ImagePinBlocksUnasserted(t *testing.T) {
	// A cluster rule {kind: image, minStrength: frozen, mode: block} + an
	// AgentClass with a tag-only SidecarToolbox → fatal PinningRequired.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "image",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	namedTb := &v1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tb"},
		Spec: v1.SidecarToolboxSpec{
			Name: "tb", Version: "1",
			Source:       v1.SidecarToolboxSource{Image: "ghcr.io/org/tool:v1"},
			Sandbox:      v1.SidecarToolboxSandbox{Class: "default"},
			Transport:    v1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: v1.SidecarToolboxUpstream{Provider: "none"},
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			SidecarToolboxes: []v1.AgentClassSidecarToolboxRef{{Name: "tb", Ref: "tb"}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, namedTb, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	require.True(t, HasFatal(vs), "named image toolbox against frozen-required rule must produce a fatal violation")
	assert.Equal(t, settings.ReasonPinningRequired, FirstFatal(vs).Reason)
}

func TestResolveForClass_ImagePinAllowsFrozen(t *testing.T) {
	// Same rule but SidecarToolbox uses a digest ref → no violation.
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			Limits: &v1.SettingsLimits{
				Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{
						Kind:        "image",
						MinStrength: "frozen",
						Mode:        v1.PinModeBlock,
					}},
				},
			},
		},
	}
	frozenTb := &v1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tb"},
		Spec: v1.SidecarToolboxSpec{
			Name: "tb", Version: "1",
			Source:       v1.SidecarToolboxSource{Image: "ghcr.io/org/tool@sha256:deadbeef"},
			Sandbox:      v1.SidecarToolboxSandbox{Class: "default"},
			Transport:    v1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: v1.SidecarToolboxUpstream{Provider: "none"},
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			SidecarToolboxes: []v1.AgentClassSidecarToolboxRef{{Name: "tb", Ref: "tb"}},
			Model: &v1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8",
				APIKey: v1.SecretKeyRef{Name: "k", Key: "v"}},
			Budget: &v1.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
	}
	c := newClient(cluster, frozenTb, class)
	_, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	assert.False(t, HasFatal(vs), "frozen image toolbox satisfies frozen floor; unexpected violations: %+v", vs)
}

// Tier 4 is read here because Resolve may not do I/O. Every bundle must appear,
// including one whose class declares no sandbox.
func TestBuildBundleSandboxInputs_ReadsSpiceboxClasses(t *testing.T) {
	withSandbox := &v1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-with"},
		Spec: v1.SpiceboxClassSpec{
			Sandbox: v1.SandboxBackend{Kind: "declared-kind"},
		},
	}
	without := &v1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-without"},
	}
	c := newClient(withSandbox, without)

	bundles := []v1.ToolBundle{
		{Name: "with-override", Class: "class-with",
			Sandbox: &v1.SandboxBackend{Kind: "override-kind"}},
		{Name: "class-only", Class: "class-with"},
		{Name: "neither", Class: "class-without"},
		{Name: "missing-class", Class: "absent"},
	}

	got, err := buildBundleSandboxInputs(context.Background(), c, bundles)
	require.NoError(t, err)
	require.Len(t, got, 4, "every bundle must appear, even one whose class is absent")

	assert.Equal(t, "override-kind", got["with-override"].FromAgentClass.Kind)
	assert.Equal(t, "declared-kind", got["with-override"].FromSpiceboxClass.Kind)

	assert.Nil(t, got["class-only"].FromAgentClass)
	assert.Equal(t, "declared-kind", got["class-only"].FromSpiceboxClass.Kind)

	assert.Nil(t, got["neither"].FromAgentClass)
	assert.Nil(t, got["neither"].FromSpiceboxClass,
		"a class declaring no kind contributes no tier-4 preference")

	assert.Nil(t, got["missing-class"].FromSpiceboxClass,
		"an absent class contributes nothing; the AgentClass controller reports it")
}

// A class that sets ONLY warmPool (no kind override, no config) contributes NO
// tier-4 preference: the per-bundle fold does not resolve WarmPool at all (see
// foldSandboxTiers in pkg/platform/settings/sandbox.go), because nothing reads
// it off effectiveSettings. Widening this gate to admit such a class would feed
// a field the fold then discards, reporting a warmPool as effective on a path
// that never sizes a pool. Pool sizing reads the class's spec directly, through
// settings.ResolveClassWarmPool.
func TestBuildBundleSandboxInputs_WarmPoolOnlyClassContributesNoPreference(t *testing.T) {
	warmPoolOnly := &v1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-warmpool-only"},
		Spec: v1.SpiceboxClassSpec{
			Sandbox: v1.SandboxBackend{
				WarmPool: &v1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-a"}},
			},
		},
	}
	c := newClient(warmPoolOnly)

	got, err := buildBundleSandboxInputs(context.Background(), c, []v1.ToolBundle{
		{Name: "warmpool-only", Class: "class-warmpool-only"},
	})
	require.NoError(t, err)
	assert.Nil(t, got["warmpool-only"].FromSpiceboxClass,
		"warmPool alone is not a sandbox preference this fold can act on")
}

// forbiddenReader wraps a client.Reader and turns every Get into a Forbidden
// error, regardless of target type. Used to prove buildBundleSandboxInputs
// propagates a non-NotFound error instead of folding it into "no tier-4
// preference" the way a genuine NotFound is.
type forbiddenReader struct {
	client.Reader
}

func (forbiddenReader) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "spiceboxclasses"},
		key.Name, errors.New("denied"))
}

func TestBuildBundleSandboxInputs_NonNotFoundErrorIsReturned(t *testing.T) {
	bundles := []v1.ToolBundle{
		{Name: "with-override", Class: "class-with"},
	}

	got, err := buildBundleSandboxInputs(context.Background(), forbiddenReader{}, bundles)
	require.Error(t, err, "a Forbidden (or any non-NotFound) error from the SpiceboxClass Get must be returned, not swallowed as 'no tier-4 preference'")
	assert.Nil(t, got)
}

func TestResolveForClass_ModelForbidden_IsFatal(t *testing.T) {
	cluster := &v1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterAgentSettingsName},
		Spec: v1.SettingsSpec{
			ModelCatalog: &[]v1.ModelCatalogEntry{
				{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				},
				{
					Name: "claude-haiku-4-5", Provider: "anthropic",
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				},
			},
			Limits: &v1.SettingsLimits{DeniedModels: []string{"claude-haiku-4-5"}},
		},
	}
	class := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"},
		Spec: v1.AgentClassSpec{
			Model:  &v1.ModelConfig{FromCatalog: "claude-haiku-4-5"},
			Budget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: 0}},
		},
	}
	c := newClient(cluster, class)
	eff, vs, err := ResolveForClass(context.Background(), c, class)
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", eff.Model.Name)
	require.True(t, HasFatal(vs))
	assert.Equal(t, settings.ReasonModelForbidden, FirstFatal(vs).Reason)
}

// budgetFixture builds a BudgetConfig with just the two dimensions these
// rootBudgetOf tests care about.
func budgetFixture(turns int32, tokens int64) v1.BudgetConfig {
	return v1.BudgetConfig{MaxTurns: turns, MaxTokens: tokens}
}

func TestRootBudgetOf_NoParent_ReturnsNilNil(t *testing.T) {
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "root-session"}}
	c := newClient(sess)
	got, err := rootBudgetOf(context.Background(), c, sess)
	require.NoError(t, err)
	assert.Nil(t, got, "a session with no parent IS the root; nothing pools over it")
}

func TestRootBudgetOf_TwoLevel_UsesParentAsRoot(t *testing.T) {
	root := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "root"},
		Status: v1.AgentSessionStatus{
			EffectiveSettings: &v1.EffectiveSettings{Budget: budgetFixture(10, 20000)},
		},
	}
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "child"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "root"}},
	}
	c := newClient(root, child)
	got, err := rootBudgetOf(context.Background(), c, child)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int32(10), got.MaxTurns)
	assert.Equal(t, int64(20000), got.MaxTokens)
}

// TestRootBudgetOf_ThreeLevel_ClimbsPastImmediateParentToRoot pins the
// property that matters most for pooling: a grandchild's ceiling must come
// from the ROOT, not the middle node, even though the middle node has its
// own (looser) resolved budget that a naive immediate-parent walk would pick
// up instead.
func TestRootBudgetOf_ThreeLevel_ClimbsPastImmediateParentToRoot(t *testing.T) {
	root := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "root"},
		Status: v1.AgentSessionStatus{
			EffectiveSettings: &v1.EffectiveSettings{Budget: budgetFixture(10, 20000)},
		},
	}
	middle := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "middle"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "root"}},
		Status: v1.AgentSessionStatus{
			// If rootBudgetOf mistakenly stopped at the immediate parent, the
			// grandchild would see 50/100000 here instead of the root's ceiling.
			EffectiveSettings: &v1.EffectiveSettings{Budget: budgetFixture(50, 100000)},
		},
	}
	grandchild := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "grandchild"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "middle"}},
	}
	c := newClient(root, middle, grandchild)
	got, err := rootBudgetOf(context.Background(), c, grandchild)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int32(10), got.MaxTurns, "must come from the root, not the middle node")
	assert.Equal(t, int64(20000), got.MaxTokens, "must come from the root, not the middle node")
}

func TestRootBudgetOf_RootNotYetResolved_ReturnsNilNilNotError(t *testing.T) {
	root := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "root"}}
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "child"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "root"}},
	}
	c := newClient(root, child)
	got, err := rootBudgetOf(context.Background(), c, child)
	require.NoError(t, err)
	assert.Nil(t, got, "root exists but hasn't resolved EffectiveSettings yet: no ceiling, not an error")
}

func TestRootBudgetOf_UnreadableAncestor_ReturnsErrorNotNoCeiling(t *testing.T) {
	child := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "child"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "root"}},
	}
	_, err := rootBudgetOf(context.Background(), forbiddenReader{}, child)
	require.Error(t, err, "a failed ancestor read must be returned, never folded into 'no ceiling' — that would fail the cost control open")
}

// TestRootBudgetOf_CycleIsBoundedNotInfinite proves the walk cannot hang the
// reconcile even if a cycle somehow escaped admission's DAG validation
// (agentclass.ValidateRoster): two sessions naming each other as parent must
// return an error within v1.MaxLineageWalk hops, not loop forever.
func TestRootBudgetOf_CycleIsBoundedNotInfinite(t *testing.T) {
	a := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "a"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "b"}},
	}
	b := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "b"},
		Spec:       v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "team-a", Name: "a"}},
	}
	c := newClient(a, b)
	_, err := rootBudgetOf(context.Background(), c, a)
	require.Error(t, err, "a parent cycle must terminate with an error, not hang the reconcile")
	assert.Contains(t, err.Error(), "cycle")
}
