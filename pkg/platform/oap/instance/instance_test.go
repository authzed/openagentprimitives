package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// agentIdentityFixture, mcpServerFixture, and agentClassFixture build the
// bundled CRs used across Rename tests. Field shapes mirror
// pkg/apis/v1alpha1/agentclass_types.go exactly (see the ref-field delta
// noted in the package doc / task report: MCPServer/SidecarToolbox refs
// resolve CRs via .ref, not the LLM-facing .name).
func agentIdentityFixture(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": name},
	}}
}

func mcpServerFixture(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "MCPServer",
		"metadata":   map[string]any{"name": name},
	}}
}

func agentUIFixture(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentUI",
		"metadata":   map[string]any{"name": name},
	}}
}

func agentClassFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "agent"},
		"spec": map[string]any{
			"agentIdentity": "id",
			"mcpServers": []any{
				map[string]any{"name": "github", "ref": "gh"},
			},
			"sidecarToolboxes": []any{
				map[string]any{"name": "sidecar", "ref": "sc-external"},
			},
			"toolBundles": []any{
				map[string]any{
					"name":          "bundle",
					"class":         "cls-external",
					"toolspecs":     []any{"ts-external"},
					"agentIdentity": "id-external",
				},
			},
		},
	}}
}

func TestStamp_SetsBothLabelsAndPreservesExisting(t *testing.T) {
	withLabel := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "AgentIdentity",
		"metadata": map[string]any{"name": "id", "labels": map[string]any{"foo": "bar"}},
	}}
	noLabels := agentClassFixture()

	Stamp([]*unstructured.Unstructured{withLabel, noLabels}, "myinstall", "apps")

	for _, cr := range []*unstructured.Unstructured{withLabel, noLabels} {
		labels := cr.GetLabels()
		assert.Equal(t, "myinstall", labels["app.kubernetes.io/instance"])
		assert.Equal(t, "myinstall", labels["agentprimitives.authzed.com/oap-install"])
		// The install NAMESPACE, which is what separates two cluster-scoped
		// objects that share an install name (see LabelInstallNamespace).
		assert.Equal(t, "apps", labels["agentprimitives.authzed.com/oap-install-namespace"])
	}
	assert.Equal(t, "bar", withLabel.GetLabels()["foo"], "pre-existing label must be preserved")
}

func TestRename_PrefixesNamesAndRewritesBundledRefs(t *testing.T) {
	id := agentIdentityFixture("id")
	gh := mcpServerFixture("gh")
	ac := agentClassFixture()

	crs := []*unstructured.Unstructured{id, gh, ac}
	require.NoError(t, Rename(crs, "demo-"))

	assert.Equal(t, "demo-id", id.GetName())
	assert.Equal(t, "demo-gh", gh.GetName())
	assert.Equal(t, "demo-agent", ac.GetName())

	agentIdentity, _, _ := unstructured.NestedString(ac.Object, "spec", "agentIdentity")
	assert.Equal(t, "demo-id", agentIdentity, "spec.agentIdentity must be rewritten to the renamed AgentIdentity")

	mcpServers, _, err := unstructured.NestedSlice(ac.Object, "spec", "mcpServers")
	require.NoError(t, err)
	require.Len(t, mcpServers, 1)
	mcp0 := mcpServers[0].(map[string]any)
	assert.Equal(t, "demo-gh", mcp0["ref"], "spec.mcpServers[].ref must be rewritten to the renamed MCPServer")
	assert.Equal(t, "github", mcp0["name"], "spec.mcpServers[].name is the LLM-facing prefix, not a CR ref — must be untouched")
}

func TestRenameMappedUsesExactNamesAndRewritesBundledRefs(t *testing.T) {
	id := agentIdentityFixture("id")
	gh := mcpServerFixture("gh")
	ac := agentClassFixture()
	crs := []*unstructured.Unstructured{id, gh, ac}
	names := NameMap{
		"AgentIdentity/id": "private-agent-identity",
		"MCPServer/gh":     "private-github-server",
		"AgentClass/agent": "private-agent-class",
	}

	require.NoError(t, RenameMapped(crs, names, nil))

	assert.Equal(t, "private-agent-identity", id.GetName())
	assert.Equal(t, "private-github-server", gh.GetName())
	assert.Equal(t, "private-agent-class", ac.GetName())
	agentIdentity, _, _ := unstructured.NestedString(ac.Object, "spec", "agentIdentity")
	assert.Equal(t, "private-agent-identity", agentIdentity)
	mcpServers, _, err := unstructured.NestedSlice(ac.Object, "spec", "mcpServers")
	require.NoError(t, err)
	require.Len(t, mcpServers, 1)
	assert.Equal(t, "private-github-server", mcpServers[0].(map[string]any)["ref"])
}

func TestRename_LeavesExternalNonBundledRefsUntouched(t *testing.T) {
	ac := agentClassFixture()
	crs := []*unstructured.Unstructured{ac} // AgentIdentity "id" and MCPServer "gh" are NOT in this bundle

	require.NoError(t, Rename(crs, "demo-"))

	assert.Equal(t, "demo-agent", ac.GetName())

	// spec.agentIdentity names an AgentIdentity CR not present in this bundle
	// (external/pre-existing) — must be left exactly as-is.
	agentIdentity, _, _ := unstructured.NestedString(ac.Object, "spec", "agentIdentity")
	assert.Equal(t, "id", agentIdentity)

	sidecarToolboxes, _, err := unstructured.NestedSlice(ac.Object, "spec", "sidecarToolboxes")
	require.NoError(t, err)
	sc0 := sidecarToolboxes[0].(map[string]any)
	assert.Equal(t, "sc-external", sc0["ref"], "ref to a non-bundled SidecarToolbox must be untouched")

	toolBundles, _, err := unstructured.NestedSlice(ac.Object, "spec", "toolBundles")
	require.NoError(t, err)
	tb0 := toolBundles[0].(map[string]any)
	assert.Equal(t, "cls-external", tb0["class"], "ref to a non-bundled SpiceboxClass must be untouched")
	assert.Equal(t, "id-external", tb0["agentIdentity"], "ref to a non-bundled AgentIdentity override must be untouched")
	assert.Equal(t, []any{"ts-external"}, tb0["toolspecs"], "ref to a non-bundled SpiceboxToolspec must be untouched")
}

func TestRename_RewritesToolBundleAgentIdentityAndClassAndToolspecs(t *testing.T) {
	id := agentIdentityFixture("bundleid")
	class := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "SpiceboxClass",
		"metadata": map[string]any{"name": "cls"},
	}}
	toolspec := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "SpiceboxToolspec",
		"metadata": map[string]any{"name": "ts1"},
	}}
	ac := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "AgentClass",
		"metadata": map[string]any{"name": "agent"},
		"spec": map[string]any{
			"toolBundles": []any{
				map[string]any{
					"name":          "bundle",
					"class":         "cls",
					"toolspecs":     []any{"ts1", "ts-external"},
					"agentIdentity": "bundleid",
				},
			},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{id, class, toolspec, ac}, "demo-"))

	toolBundles, _, err := unstructured.NestedSlice(ac.Object, "spec", "toolBundles")
	require.NoError(t, err)
	tb0 := toolBundles[0].(map[string]any)
	assert.Equal(t, "demo-cls", tb0["class"])
	assert.Equal(t, "demo-bundleid", tb0["agentIdentity"])
	assert.Equal(t, []any{"demo-ts1", "ts-external"}, tb0["toolspecs"])
	assert.Equal(t, "bundle", tb0["name"], "toolBundles[].name is the LLM-facing prefix, not a CR ref — must be untouched")
}

func TestRename_RewritesAgentClassAgentUIRef(t *testing.T) {
	ui := agentUIFixture("chat-ui")
	ac := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "agent"},
		"spec": map[string]any{
			"agentUI": map[string]any{
				"ref":          "chat-ui",
				"grantedTools": []any{"read_file"},
			},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{ui, ac}, "demo-"))

	assert.Equal(t, "demo-chat-ui", ui.GetName())
	gotRef, _, _ := unstructured.NestedString(ac.Object, "spec", "agentUI", "ref")
	assert.Equal(t, "demo-chat-ui", gotRef, "spec.agentUI.ref must be rewritten to the renamed AgentUI")
	assert.Equal(t, ui.GetName(), gotRef, "the rewritten ref must resolve to the bundled, renamed AgentUI CR — not merely change value")

	grantedTools, _, err := unstructured.NestedStringSlice(ac.Object, "spec", "agentUI", "grantedTools")
	require.NoError(t, err)
	assert.Equal(t, []string{"read_file"}, grantedTools, "grantedTools authorizes tool NAMES, not a CR ref — must be untouched")
}

func TestRename_AgentUIExternalRefUntouched(t *testing.T) {
	// A ref naming an AgentUI NOT present in this bundle (external/pre-existing
	// in the cluster) must be left exactly as-is.
	ac := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": "agent"},
		"spec": map[string]any{
			"agentUI": map[string]any{"ref": "cluster-shared-ui"},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{ac}, "demo-"))

	gotRef, _, _ := unstructured.NestedString(ac.Object, "spec", "agentUI", "ref")
	assert.Equal(t, "cluster-shared-ui", gotRef, "a ref to a non-bundled AgentUI must be untouched")
}

func TestRename_RewritesSkillSourceAuthAgentIdentity(t *testing.T) {
	id := agentIdentityFixture("id")
	sksrc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SkillSource",
		"metadata":   map[string]any{"name": "fidelity"},
		"spec": map[string]any{
			"repoURL": "https://github.com/example/messaging",
			"auth": map[string]any{
				"agentIdentity": "id",
				"credential":    "github-token",
			},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{id, sksrc}, "demo-"))

	assert.Equal(t, "demo-fidelity", sksrc.GetName())
	gotIdentity, _, _ := unstructured.NestedString(sksrc.Object, "spec", "auth", "agentIdentity")
	assert.Equal(t, "demo-id", gotIdentity, "SkillSource.spec.auth.agentIdentity must be rewritten to the renamed AgentIdentity")
	gotCred, _, _ := unstructured.NestedString(sksrc.Object, "spec", "auth", "credential")
	assert.Equal(t, "github-token", gotCred, "auth.credential is a credential name on the identity, not a CR name — must be untouched")
}

func TestRename_SkillSourceExternalIdentityRefUntouched(t *testing.T) {
	// A SkillSource whose auth.agentIdentity names an identity NOT present in
	// the bundle (external/pre-existing) is left as-is.
	sksrc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SkillSource",
		"metadata":   map[string]any{"name": "fidelity"},
		"spec": map[string]any{
			"auth": map[string]any{"agentIdentity": "cluster-shared-id"},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{sksrc}, "demo-"))

	assert.Equal(t, "demo-fidelity", sksrc.GetName())
	gotIdentity, _, _ := unstructured.NestedString(sksrc.Object, "spec", "auth", "agentIdentity")
	assert.Equal(t, "cluster-shared-id", gotIdentity, "an external identity ref must not be rewritten")
}

func TestRename_RewritesToolspecToolkitName(t *testing.T) {
	toolkit := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxToolkit",
		"metadata":   map[string]any{"name": "gh-toolkit"},
	}}
	toolspec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxToolspec",
		"metadata":   map[string]any{"name": "gh-spec"},
		"spec": map[string]any{
			"toolkit": map[string]any{"name": "gh-toolkit", "revision": "v1"},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{toolkit, toolspec}, "demo-"))

	assert.Equal(t, "demo-gh-toolkit", toolkit.GetName())
	gotToolkit, _, _ := unstructured.NestedString(toolspec.Object, "spec", "toolkit", "name")
	assert.Equal(t, "demo-gh-toolkit", gotToolkit, "SpiceboxToolspec.spec.toolkit.name must be rewritten to the renamed SpiceboxToolkit")
	gotRev, _, _ := unstructured.NestedString(toolspec.Object, "spec", "toolkit", "revision")
	assert.Equal(t, "v1", gotRev, "toolkit.revision is not a name ref — must be untouched")
}

func TestRename_RewritesAgentIdentityCredentialSecretRefs(t *testing.T) {
	staticSec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "static-secret"},
	}}
	oauthSec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "oauth-secret"},
	}}
	id := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": "id"},
		"spec": map[string]any{
			"credentials": []any{
				map[string]any{
					"name":   "static-cred",
					"type":   "static",
					"static": map[string]any{"secretRef": map[string]any{"name": "static-secret", "key": "token"}},
				},
				map[string]any{
					"name":  "oauth-cred",
					"type":  "oauth",
					"oauth": map[string]any{"secretRef": map[string]any{"name": "oauth-secret"}},
				},
				map[string]any{
					"name":   "external-cred",
					"type":   "static",
					"static": map[string]any{"secretRef": map[string]any{"name": "external-secret", "key": "token"}},
				},
			},
		},
	}}

	require.NoError(t, Rename([]*unstructured.Unstructured{staticSec, oauthSec, id}, "demo-"))

	creds, _, err := unstructured.NestedSlice(id.Object, "spec", "credentials")
	require.NoError(t, err)
	require.Len(t, creds, 3)

	gotStatic, _, _ := unstructured.NestedString(creds[0].(map[string]any), "static", "secretRef", "name")
	assert.Equal(t, "demo-static-secret", gotStatic, "static.secretRef.name must be rewritten to the renamed bundled Secret")
	gotKey, _, _ := unstructured.NestedString(creds[0].(map[string]any), "static", "secretRef", "key")
	assert.Equal(t, "token", gotKey, "secretRef.key is not a name ref — must be untouched")

	gotOAuth, _, _ := unstructured.NestedString(creds[1].(map[string]any), "oauth", "secretRef", "name")
	assert.Equal(t, "demo-oauth-secret", gotOAuth, "oauth.secretRef.name must be rewritten to the renamed bundled Secret")

	gotExternal, _, _ := unstructured.NestedString(creds[2].(map[string]any), "static", "secretRef", "name")
	assert.Equal(t, "external-secret", gotExternal, "a secretRef naming a non-bundled Secret must be left untouched")
}

// TestRename_RewritesEveryRegisteredCredentialKindsSecretRef is the
// generalization of the test above, and the one that would have caught the
// githubApp gap: it drives EVERY registered credkind through Rename, so a
// credential type the rewrite does not cover fails here rather than in a
// cluster as Valid=False/SecretMissing with no token ever minted.
//
// The table is built from the registry, not written out, so a fifth kind is
// covered the day it is registered — which is the whole reason the rewrite
// asks the kind where its Secret ref lives instead of enumerating two paths.
func TestRename_RewritesEveryRegisteredCredentialKindsSecretRef(t *testing.T) {
	kinds := credkindregistry.All()
	require.NotEmpty(t, kinds, "the registry must be populated or this test is vacuous")

	objs := []*unstructured.Unstructured{}
	creds := []any{}
	withSecret := map[string]string{} // credential name -> bundled Secret name

	for _, k := range kinds {
		cred := map[string]any{"name": k.Type() + "-cred", "type": k.Type()}
		if path := k.SecretRefPath(); len(path) > 0 {
			secretName := k.Type() + "-secret"
			require.NoError(t, unstructured.SetNestedField(cred, secretName, path...))
			withSecret[k.Type()] = secretName
			objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": secretName},
			}})
		}
		creds = append(creds, cred)
	}
	require.NotEmpty(t, withSecret, "at least one kind must have a backing Secret or nothing is being rewritten")

	id := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": "id"},
		"spec":       map[string]any{"credentials": creds},
	}}
	objs = append(objs, id)

	require.NoError(t, Rename(objs, "demo-"))

	got, _, err := unstructured.NestedSlice(id.Object, "spec", "credentials")
	require.NoError(t, err)
	require.Len(t, got, len(kinds))

	for i, k := range kinds {
		path := k.SecretRefPath()
		if len(path) == 0 {
			continue
		}
		name, found, nerr := unstructured.NestedString(got[i].(map[string]any), path...)
		require.NoError(t, nerr)
		require.Truef(t, found, "%s: the Secret ref disappeared from %v", k.Type(), path)
		assert.Equalf(t, "demo-"+withSecret[k.Type()], name,
			"%s: a --name install must repoint this credential at its own prefixed Secret; "+
				"left un-rewritten it names a Secret the install never creates", k.Type())
	}
}

// TestRename_UnregisteredCredentialType_Errors: skipping an unknown type is
// what a dangling secretRef looks like from the inside, so the rename refuses
// the bundle instead of quietly emitting one.
func TestRename_UnregisteredCredentialType_Errors(t *testing.T) {
	id := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": "id"},
		"spec": map[string]any{
			"credentials": []any{map[string]any{
				"name": "cred", "type": "not-a-registered-kind",
				"notARegisteredKind": map[string]any{"secretRef": map[string]any{"name": "sec"}},
			}},
		},
	}}
	sec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sec"},
	}}

	err := Rename([]*unstructured.Unstructured{sec, id}, "demo-")
	require.Error(t, err, "an unknown credential type must fail the rename, not be silently skipped")
	assert.Contains(t, err.Error(), "not-a-registered-kind")
}

func TestRename_AbsentRefFieldsAreSkippedGracefully(t *testing.T) {
	ac := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "AgentClass",
		"metadata": map[string]any{"name": "agent"},
		"spec":     map[string]any{},
	}}
	assert.NoError(t, Rename([]*unstructured.Unstructured{ac}, "demo-"))
	assert.Equal(t, "demo-agent", ac.GetName())
}

func TestRename_StructuralSurprise_Errors(t *testing.T) {
	ac := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "AgentClass",
		"metadata": map[string]any{"name": "agent"},
		"spec": map[string]any{
			"agentIdentity": 12345, // not a string: a structural surprise
		},
	}}
	err := Rename([]*unstructured.Unstructured{ac}, "demo-")
	require.Error(t, err)
}
