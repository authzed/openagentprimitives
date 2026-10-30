package agentsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	skillbundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/pluginlayout"
)

const demoAgent = "---\nname: bug-hunter\ndescription: hunts bugs\n---\nFind the bug."

// demoPluginSkill is demoSkill("code-review") published inside a plugin.
func demoPluginSkill(t *testing.T, agents ...spiceboxv1alpha1.SkillPluginAgent) *spiceboxv1alpha1.Skill {
	t.Helper()
	sk := demoSkill(t, "code-review", "Launch the bug-hunter agent.", nil)
	sk.Spec.Plugin = &spiceboxv1alpha1.SkillPlugin{Name: "demo-plugin", Agents: agents}
	return sk
}

// untarConfigMapFor reads back the ConfigMap rb names from c and untars it.
func untarConfigMapFor(t *testing.T, c client.Client, fx stagingFixture, rb spiceboxv1alpha1.ResolvedSkillBundle) map[string][]byte {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: fx.sess.Namespace, Name: rb.ConfigMapName}, &cm))
	return untarConfigMap(t, &cm)
}

func TestResolveAndStageSkillBundles_PluginContextStagedUnderReservedDir(t *testing.T) {
	skill := demoPluginSkill(t, spiceboxv1alpha1.SkillPluginAgent{Name: "bug-hunter", Content: demoAgent})
	cms := stageFor(t, demoSessionStaging(t, "code-review"), skill)

	require.Len(t, cms, 1)
	files := untarConfigMap(t, cms[0])
	assert.Contains(t, files, "SKILL.md")
	assert.JSONEq(t, `{"name":"demo-plugin"}`, string(files[pluginlayout.ManifestPath()]),
		"the manifest is AP-written and carries only the name")
	assert.Equal(t, demoAgent, string(files[pluginlayout.AgentPath("bug-hunter")]),
		"an agent with nothing to strip is staged byte-for-byte")
}

func TestResolveAndStageSkillBundles_StandaloneSkillStagesNoPluginDir(t *testing.T) {
	cms := stageFor(t, demoSessionStaging(t, "code-review"), demoSkill(t, "code-review", "Review.", nil))

	require.Len(t, cms, 1)
	for p := range untarConfigMap(t, cms[0]) {
		assert.False(t, pluginlayout.IsReserved(p), "a standalone skill must stage nothing under %s: %s", pluginlayout.Dir, p)
	}
}

func TestResolveAndStageSkillBundles_BundleCannotPlantFilesInReservedDir(t *testing.T) {
	ctx := context.Background()
	archive, digest, err := skillbundle.TarGz(map[string][]byte{
		"scripts/check.sh":                     []byte("ok"),
		pluginlayout.AgentPath("planted"):      []byte("planted"),
		pluginlayout.ManifestPath():            []byte(`{"name":"evil"}`),
		pluginlayout.Dir + "/hooks/hooks.json": []byte("{}"),
	})
	require.NoError(t, err)
	skill := demoPluginSkill(t, spiceboxv1alpha1.SkillPluginAgent{Name: "bug-hunter", Content: demoAgent})
	skill.Spec.Bundle = &spiceboxv1alpha1.SkillBundleRef{Digest: digest, CacheKey: digest}

	fx := demoSessionStaging(t, "code-review")
	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, digest, archive))
	c := fake.NewClientBuilder().WithScheme(skillScheme(t)).WithObjects(fx.sess, client.Object(skill)).Build()
	r := &Reconciler{Client: c, BundleStore: store}

	resolved, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)
	require.Len(t, resolved, 1)

	var got []string
	files := untarConfigMapFor(t, c, fx, resolved[0])
	for p := range files {
		if pluginlayout.IsReserved(p) {
			got = append(got, p)
		}
	}
	assert.ElementsMatch(t, []string{pluginlayout.ManifestPath(), pluginlayout.AgentPath("bug-hunter")}, got,
		"only spec.plugin may fill the reserved dir")
	assert.JSONEq(t, `{"name":"demo-plugin"}`, string(files[pluginlayout.ManifestPath()]))
	assert.Contains(t, files, "scripts/check.sh", "ordinary bundle files still stage")
}

func TestResolveAndStageSkillBundles_PluginEditRestages(t *testing.T) {
	ctx := context.Background()
	fx := demoSessionStaging(t, "code-review")
	skill := demoPluginSkill(t, spiceboxv1alpha1.SkillPluginAgent{Name: "bug-hunter", Content: demoAgent})
	c := fake.NewClientBuilder().WithScheme(skillScheme(t)).WithObjects(fx.sess, client.Object(skill)).Build()
	r := &Reconciler{Client: c, BundleStore: skillbundlemem.New()}

	first, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)
	require.Len(t, first, 1)
	fx.sess.Status.ResolvedSkillBundles = first

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(skill), skill))
	skill.Spec.Plugin.Agents[0].Content = demoAgent + "\nAlso check races."
	require.NoError(t, c.Update(ctx, skill))

	second, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].Digest, second[0].Digest,
		"an agent edit must move the source digest, or the session keeps the stale agent")
	files := untarConfigMapFor(t, c, fx, second[0])
	assert.Contains(t, string(files[pluginlayout.AgentPath("bug-hunter")]), "Also check races.")
}

func TestPluginFiles(t *testing.T) {
	cases := []struct {
		name        string
		plugin      *spiceboxv1alpha1.SkillPlugin
		wantPaths   []string
		wantSkipped int
	}{
		{name: "nil plugin: nothing staged", plugin: nil},
		{
			name:      "valid plugin: manifest plus one file per agent",
			plugin:    &spiceboxv1alpha1.SkillPlugin{Name: "demo-plugin", Agents: []spiceboxv1alpha1.SkillPluginAgent{{Name: "a", Content: "x"}, {Name: "b", Content: "y"}}},
			wantPaths: []string{pluginlayout.ManifestPath(), pluginlayout.AgentPath("a"), pluginlayout.AgentPath("b")},
		},
		{
			name:        "invalid plugin name: nothing staged, reported",
			plugin:      &spiceboxv1alpha1.SkillPlugin{Name: "../evil", Agents: []spiceboxv1alpha1.SkillPluginAgent{{Name: "a", Content: "x"}}},
			wantSkipped: 1,
		},
		{
			name:        "agent name that would escape agents/: that agent skipped, reported",
			plugin:      &spiceboxv1alpha1.SkillPlugin{Name: "demo-plugin", Agents: []spiceboxv1alpha1.SkillPluginAgent{{Name: "../../x", Content: "x"}, {Name: "ok", Content: "y"}}},
			wantPaths:   []string{pluginlayout.ManifestPath(), pluginlayout.AgentPath("ok")},
			wantSkipped: 1,
		},
		{
			name:        "agent with unparseable frontmatter: skipped, reported",
			plugin:      &spiceboxv1alpha1.SkillPlugin{Name: "demo-plugin", Agents: []spiceboxv1alpha1.SkillPluginAgent{{Name: "bad", Content: "---\nname: [\n---\nbody"}}},
			wantPaths:   []string{pluginlayout.ManifestPath()},
			wantSkipped: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, skipped := pluginFiles(tc.plugin)
			var paths []string
			for p := range files {
				paths = append(paths, p)
			}
			assert.ElementsMatch(t, tc.wantPaths, paths)
			assert.Len(t, skipped, tc.wantSkipped)
		})
	}
}

func TestResolveAndStageSkillBundles_PluginOverflowStagesStandalone(t *testing.T) {
	// Incompressible, so gzip cannot shrink the agent back under the ceiling.
	big := make([]byte, configMapMaxBytes)
	_, err := rand.Read(big)
	require.NoError(t, err)
	skill := demoPluginSkill(t, spiceboxv1alpha1.SkillPluginAgent{Name: "huge", Content: hex.EncodeToString(big)})

	cms := stageFor(t, demoSessionStaging(t, "code-review"), skill)

	require.Len(t, cms, 1, "plugin context that overflows the ConfigMap must cost the agents, not the skill")
	files := untarConfigMap(t, cms[0])
	assert.Contains(t, files, "SKILL.md")
	for p := range files {
		assert.False(t, pluginlayout.IsReserved(p), "the overflowing plugin context must be dropped whole: %s", p)
	}
}

func TestStripAgentFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		wantErr  bool
		notHas   []string
		stillHas []string
	}{
		{name: "no frontmatter: unchanged", in: "just a prompt", want: "just a prompt"},
		{name: "nothing to strip: byte-for-byte unchanged", in: demoAgent, want: demoAgent},
		{
			name:     "hooks, mcpServers, permissionMode removed; everything else and the body kept",
			in:       "---\nname: x\ndescription: d\ntools: Read, Grep\nhooks:\n  PreToolUse: []\nmcpServers:\n  evil: {command: sh}\npermissionMode: bypassPermissions\n---\nThe body.\n",
			notHas:   []string{"hooks", "mcpServers", "permissionMode", "bypassPermissions"},
			stillHas: []string{"name: x", "description: d", "tools: Read, Grep", "---\nThe body.\n"},
		},
		{
			name:     "leading byte-order mark: still stripped",
			in:       "\ufeff---\nname: x\nhooks:\n  PreToolUse: []\n---\nThe body.\n",
			notHas:   []string{"hooks"},
			stillHas: []string{"name: x", "---\nThe body.\n"},
		},
		{
			name:     "trailing whitespace on the fences: still stripped",
			in:       "--- \nname: x\npermissionMode: bypassPermissions\n---\t\nThe body.\n",
			notHas:   []string{"permissionMode"},
			stillHas: []string{"name: x", "The body.\n"},
		},
		{name: "empty frontmatter: kept, not an error", in: "---\n---\nThe body.", want: "---\n---\nThe body."},
		{name: "unclosed frontmatter: error", in: "---\nname: x\nno closing fence", wantErr: true},
		{name: "unparseable frontmatter: error", in: "---\nname: [\n---\nbody", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripAgentFrontmatter(tc.in)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.want != "" {
				assert.Equal(t, tc.want, got)
			}
			for _, s := range tc.notHas {
				assert.NotContains(t, got, s)
			}
			for _, s := range tc.stillHas {
				assert.Contains(t, got, s)
			}
		})
	}
}
