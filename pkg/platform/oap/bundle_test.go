package oap

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/oaptest"

	// Registers the credkind.Kinds so FromFolder's DeriveInherited resolves the
	// static credential fixture below to its Secret; a test binary does not
	// inherit another package's blank import.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

func TestUnpackValidatesEmbeddedChildrenIndependently(t *testing.T) {
	packed := embeddedArchiveFixture(t)
	bad := repackOuter(t, packed, func(files map[string][]byte) {
		mutateManifest(t, files, func(man *ocispec.Manifest) {
			layer := &man.Layers[len(man.Layers)-1]
			child, err := Unpack(files[blobKey(layer.Digest)])
			require.NoError(t, err)
			child.Manifests = append(child.Manifests, []byte("\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: forbidden\n")...)
			raw, err := Pack(child)
			require.NoError(t, err)
			delete(files, blobKey(layer.Digest))
			layer.Digest = digest.FromBytes(raw)
			layer.Size = int64(len(raw))
			files[blobKey(layer.Digest)] = raw
		})
		man := readManifest(t, files)
		mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0].Digest = man.Layers[len(man.Layers)-1].Digest.String() })
	})
	_, err := Unpack(bad)
	assert.ErrorContains(t, err, "disallowed resource kind")
	assert.ErrorContains(t, err, "test-coordinator > reviewer")
}

func TestFromFolder_LoadsManifestCRsAndAssets(t *testing.T) {
	b, err := FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	require.NotNil(t, b.Manifest)
	// agent.name is derived from the bundled AgentClass (metadata.name), so the
	// fixture's agent is named after its class, "demo-class".
	assert.Equal(t, "demo-class", b.Manifest.Agent.Name)

	crs, err := b.CRs()
	require.NoError(t, err)
	require.Len(t, crs, 1)
	assert.Equal(t, "AgentClass", crs[0].GetKind())
	assert.Equal(t, "demo-class", crs[0].GetName())

	require.Contains(t, b.Assets, "assets/logo.txt")
}

// TestFromFolder_InheritsDerivedFields proves an oap.yaml that omits the
// derived fields gets them from the bundled AgentClass graph, while its own
// package-level fields (version, logo) are preserved.
func TestFromFolder_InheritsDerivedFields(t *testing.T) {
	b, err := FromFolder("testdata/inherit-folder")
	require.NoError(t, err)
	require.NoError(t, b.Validate())

	assert.Equal(t, "demo-agent", b.Manifest.Agent.Name)
	assert.Equal(t, "Demo Agent", b.Manifest.Agent.DisplayName)
	assert.Equal(t, "does demo things", b.Manifest.Agent.Description)
	assert.Equal(t, "2.3.0", b.Manifest.Agent.Version, "authored version is preserved")
	assert.Equal(t, "assets/logo.svg", b.Manifest.Agent.Logo, "authored logo is preserved")

	require.Len(t, b.Manifest.Requires.Skills, 1)
	assert.Equal(t, "github.com/fakeorg/fakerepo//skills/planner@v1", b.Manifest.Requires.Skills[0].Canonical)

	require.Len(t, b.Manifest.Requires.Secrets, 1)
	assert.Equal(t, "demo-agent-secret", b.Manifest.Requires.Secrets[0].Name)
	assert.Equal(t, []string{"api-key"}, b.Manifest.Requires.Secrets[0].Keys)
	assert.Equal(t, "why we need the api key", b.Manifest.Requires.Secrets[0].Purpose)
}

// TestFromFolder_LinksDerivedSecretToAuthoredQuestion proves a derived
// RequiredSecret is linked to an authored secret question that materializes it,
// so install collects it through that question (with its own prompt) instead of
// synthesizing a duplicate collector for the same Secret.
func TestFromFolder_LinksDerivedSecretToAuthoredQuestion(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-agent\nspec:\n  agentIdentity: demo-agent-id\n"+
			"---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentIdentity\nmetadata:\n  name: demo-agent-id\nspec:\n  credentials:\n    - name: gh\n      type: static\n      static:\n        secretRef:\n          name: gh-pat\n          key: token\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\nquestions:\n  - name: githubToken\n    type: secret\n    prompt: \"GitHub PAT\"\n    secret:\n      createSecret: {name: gh-pat, key: token}\n"), 0o644))

	b, err := FromFolder(dir)
	require.NoError(t, err)
	require.Len(t, b.Manifest.Requires.Secrets, 1)
	assert.Equal(t, "gh-pat", b.Manifest.Requires.Secrets[0].Name)
	assert.Equal(t, "githubToken", b.Manifest.Requires.Secrets[0].Question,
		"a derived RequiredSecret must adopt the authored secret question that materializes it")
}

// TestFromFolder_RejectsAuthoredDerivedField is the fail-closed control: an
// oap.yaml that also writes a derived field fails to load, naming the field.
func TestFromFolder_RejectsAuthoredDerivedField(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	src, err := os.ReadFile("testdata/inherit-folder/manifests/agent.yaml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), src, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n  description: duplicated here\n"), 0o644))

	_, err = FromFolder(dir)
	require.Error(t, err)
	assert.ErrorContains(t, err, "agent.description")
	assert.ErrorContains(t, err, "derived")
}

func TestBundleValidate(t *testing.T) {
	good, err := FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	require.NoError(t, good.Validate())

	// Binding target referencing a CR that is not in the bundle must fail.
	bad := &Bundle{
		Manifest: &Manifest{
			OapFormatVersion: "1",
			Agent:            Agent{Name: "x", Version: "1"},
			Questions: []Question{{
				Name: "who", Type: QString,
				Binding: []Binding{{Target: "AgentIdentity/missing#spec.description"}},
			}},
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: x\n"),
	}
	err = bad.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AgentIdentity/missing")
}

// TestBundleValidate_RejectsZeroOrMultipleAgentClasses enforces the one-class
// rule: a .oap describes exactly one agent, and the sole AgentClass is the
// canonical source every derived field (DeriveInherited) inherits from.
func TestBundleValidate_RejectsZeroOrMultipleAgentClasses(t *testing.T) {
	oneClass := "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-a\nspec:\n  systemPrompt:\n    inline: hi\n"
	twoClasses := oneClass + "---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-b\nspec:\n  systemPrompt:\n    inline: hi\n"

	cases := []struct {
		name      string
		manifests string
	}{
		{name: "zero AgentClasses: rejected", manifests: ""},
		{name: "two AgentClasses: rejected", manifests: twoClasses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{
				Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "demo-a", Version: "1.0.0"}},
				Manifests: []byte(tc.manifests),
			}
			err := b.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "exactly one AgentClass")
		})
	}
}

// TestBundleValidate_RejectsDisallowedKinds is the confused-deputy guard: a
// bundle carrying anything outside the agent-graph + ConfigMap allowlist must
// fail Validate (the invariant install.Preflight/install.Install rely on to
// keep a crafted .oap from smuggling a privileged Pod or an RBAC-escalating
// binding past the operator's elevated ServiceAccount). Matching is on
// group+Kind: a core Pod and a core ClusterRoleBinding are both refused even
// though a throwaway AgentClass rides alongside them.
func TestBundleValidate_RejectsDisallowedKinds(t *testing.T) {
	agentClass := "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: x\nspec:\n  systemPrompt:\n    inline: hi\n"

	cases := []struct {
		name     string
		manifest string
	}{
		{
			name:     "core Pod alongside an AgentClass → rejected",
			manifest: agentClass + "---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: evil\nspec:\n  containers:\n  - name: c\n    image: busybox\n",
		},
		{
			name:     "RBAC ClusterRoleBinding → rejected",
			manifest: agentClass + "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: pwn\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cluster-admin\nsubjects:\n- kind: ServiceAccount\n  name: sa\n  namespace: default\n",
		},
		{
			name:     "core Secret is not a bundle kind → rejected",
			manifest: agentClass + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\nstringData:\n  token: abc\n",
		},
		{
			// Channels are per-install deployment config selected in the install
			// UI (which workspace, which tokens), never baked into a portable
			// agent container — so a bundled Channel is refused.
			name:     "Channel is no longer a bundle kind → rejected",
			manifest: agentClass + "---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: Channel\nmetadata:\n  name: slack\nspec:\n  agentClass: x\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{
				Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "x", Version: "1"}},
				Manifests: []byte(tc.manifest),
			}
			err := b.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "disallowed resource kind")
		})
	}
}

// TestBundleValidate_AllowsAgentGraphAndConfigMaps confirms the allowlist does
// not over-reject: a bundle carrying the full agent graph plus a ConfigMap
// still validates.
func TestBundleValidate_AllowsAgentGraphAndConfigMaps(t *testing.T) {
	kinds := []string{"AgentClass", "AgentIdentity", "AgentUI", "MCPServer", "SidecarToolbox", "SkillSource", "SpiceboxClass", "SpiceboxToolkit", "SpiceboxToolspec"}
	var stream string
	for i, k := range kinds {
		if i > 0 {
			stream += "---\n"
		}
		stream += "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: " + k + "\nmetadata:\n  name: n" + k + "\n"
	}
	stream += "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: prompt\ndata:\n  x: y\n"

	b := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "x", Version: "1"}},
		Manifests: []byte(stream),
	}
	require.NoError(t, b.Validate(), "every agent-graph kind + ConfigMap must be allowed")
}

// TestBundleValidate_AllowsAgentUI confirms a bundled AgentUI manifest
// round-trips through the CR stream decode and is not rejected by
// checkAllowedKinds — a .oap agent bundle ships its AgentUI CR alongside the
// AgentClass it serves (AgentClass.spec.agentUI.ref names it by name).
func TestBundleValidate_AllowsAgentUI(t *testing.T) {
	agentClass := "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: x\nspec:\n  systemPrompt:\n    inline: hi\n"
	agentUI := "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentUI\nmetadata:\n  name: demo-ui\nspec:\n  slots:\n  - name: main\n"

	b := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "x", Version: "1"}},
		Manifests: []byte(agentClass + "---\n" + agentUI),
	}

	crs, err := b.CRs()
	require.NoError(t, err)
	require.Len(t, crs, 2)
	assert.Equal(t, "AgentUI", crs[1].GetKind())
	assert.Equal(t, "demo-ui", crs[1].GetName())

	assert.NoError(t, b.Validate(), "a bundled AgentUI must not be rejected as an unknown kind")
}

// TestAllowedBundleKinds_AdmitsSkillRejectsChannel confirms a hand-authored
// Skill CR — first-party content, no git fetch — is bundle-allowed alongside
// SkillSource, while Channel (per-install deployment config) stays refused.
func TestAllowedBundleKinds_AdmitsSkillRejectsChannel(t *testing.T) {
	skill := &unstructured.Unstructured{}
	skill.SetAPIVersion(spiceboxv1alpha1.GroupName + "/v1alpha1")
	skill.SetKind("Skill")
	assert.NoError(t, checkAllowedKinds([]*unstructured.Unstructured{skill}),
		"a hand-authored Skill CR is bundle-allowed (first-party, no git fetch)")

	channel := &unstructured.Unstructured{}
	channel.SetAPIVersion(spiceboxv1alpha1.GroupName + "/v1alpha1")
	channel.SetKind("Channel")
	assert.Error(t, checkAllowedKinds([]*unstructured.Unstructured{channel}),
		"Channel stays disallowed in a bundle")
}

// TestAllowedBundleKindNames_IncludesAgentUI confirms the exported accessor
// pkg/platform/oap/install's managedKinds coverage test depends on actually reflects
// the live allowedBundleKinds set — it must include AgentUI, exclude the
// core ConfigMap kind (a fixed pair documented separately, not part of this
// growing set), and be sorted (a stable diff on failure).
func TestAllowedBundleKindNames_IncludesAgentUI(t *testing.T) {
	names := AllowedBundleKindNames()
	assert.Contains(t, names, "AgentUI")
	assert.Contains(t, names, "AgentClass")
	assert.NotContains(t, names, "ConfigMap", "core kinds are excluded — only the agentprimitives.authzed.com group is returned")
	assert.True(t, sort.StringsAreSorted(names), "names must be sorted for a deterministic coverage-test diff")
}

// TestBundleValidate_KindlessDocumentSkipped covers the document that is not a
// CR at all: no `kind`, so there is nothing for the allowlist to rule on. Such
// a document has to be dropped by the splitter, the way the other multi-document
// splitters in this repo drop it. Letting it through hands checkAllowedKinds an
// empty Kind, which is not on the allowlist, and the bundle is refused with
// `disallowed resource kind ""` — a security-shaped rejection naming a Kind
// nobody wrote, for a stray document that carries no resource.
func TestBundleValidate_KindlessDocumentSkipped(t *testing.T) {
	agentClass := "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-class\nspec:\n  systemPrompt:\n    inline: hi\n"
	stray := "---\n# left over from rendering the manifests stream\nsomeKey: someValue\n"

	b := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "demo-agent", Version: "1"}},
		Manifests: []byte(agentClass + stray),
	}

	require.NoError(t, b.Validate(), "a kind-less document must be skipped, not refused as a disallowed empty Kind")

	crs, err := b.CRs()
	require.NoError(t, err)
	require.Len(t, crs, 1, "only the AgentClass is a CR; the kind-less document is not one")
	assert.Equal(t, "AgentClass", crs[0].GetKind())
}
