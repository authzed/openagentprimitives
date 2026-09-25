//go:build integration

package source_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
)

// clusterExportNamespace is fresh to this test's OWN envtest instance:
// testenv.Start boots an isolated apiserver per call (unlike
// testenv.Shared, which reuses one cluster across a package), so there is
// no cross-test collision risk despite the fixed name.
const clusterExportNamespace = "oap-cluster-export"

// minimalToolkitSubcommand mirrors minimalSubcommand in
// pkg/controllers/spiceboxtoolkit's controller_test.go. Every array-typed
// required field must be an explicit empty slice, not a nil one: a nil Go slice
// marshals to JSON `null`, and the real apiserver's structural schema (type:
// array, no `nullable: true`) rejects that with "must be of type array". The
// fake client cluster_test.go uses does not enforce this at all, which is the
// gap this integration test exists to close.
func minimalToolkitSubcommand() v1alpha1.ToolkitSubcommand {
	return v1alpha1.ToolkitSubcommand{
		Path: []string{},
		Effects: v1alpha1.ToolkitEffects{
			Reads:      []string{},
			Writes:     []string{},
			Network:    v1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
			Filesystem: v1alpha1.ToolkitFsEffect{Paths: []string{}},
			Creds:      v1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
		},
	}
}

// TestOpenCluster_ExportsSanitizedGraph_NoSecretValues walks a real AgentClass
// ref graph (AgentClass -> AgentIdentity, MCPServer, SidecarToolbox, and a
// toolBundle -> SpiceboxClass + SpiceboxToolspec -> a non-builtin
// SpiceboxToolkit) through a live envtest apiserver.
//
// It complements cluster_test.go's fake-client unit test in two ways the fake
// client cannot: the real apiserver enforces CRD structural schema, so a fixture
// that merely compiles against the Go types is not enough — it must also be a
// valid CR; and the real apiserver stamps server-managed metadata (uid,
// resourceVersion) the fake client never populates the same way, so asserting
// Sanitize stripped them proves something the unit test cannot.
//
// No corev1.Secret is ever created for the "model-key" secret that
// spec.model.apiKey references. OpenCluster records secret NAMES and never Gets
// a Secret, so Bundle() must succeed with the referenced secret entirely absent
// and the packed bytes must carry no secret value.
func TestOpenCluster_ExportsSanitizedGraph_NoSecretValues(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: clusterExportNamespace}}
	require.NoError(t, env.Client.Create(ctx, ns), "create namespace")

	ai := &v1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-id", Namespace: clusterExportNamespace},
		Spec: v1alpha1.AgentIdentitySpec{
			Credentials: []v1alpha1.AgentCredential{
				{
					Name: "github",
					Type: "static",
					Static: &v1alpha1.StaticCredentialSource{
						SecretRef: v1alpha1.SecretKeyRef{Name: "demo-pat", Key: "token"},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")

	// A SECOND AgentIdentity, referenced only via a per-bundle
	// spec.toolBundles[].agentIdentity override. Its credential secret
	// ("bundle-pat") is likewise never created — the override edge must be
	// followed and its secret NAME recorded without ever reading a Secret.
	bundleAI := &v1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "bundle-id", Namespace: clusterExportNamespace},
		Spec: v1alpha1.AgentIdentitySpec{
			Credentials: []v1alpha1.AgentCredential{
				{
					Name: "bundle-cred",
					Type: "static",
					Static: &v1alpha1.StaticCredentialSource{
						SecretRef: v1alpha1.SecretKeyRef{Name: "bundle-pat", Key: "token"},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, bundleAI), "create per-bundle AgentIdentity")

	// A ConfigMap backing the AgentClass system prompt. ConfigMaps are
	// non-secret and their VALUES are embedded verbatim in the export.
	promptCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prompt-cm", Namespace: clusterExportNamespace},
		Data:       map[string]string{"prompt": "You are a fake test agent."},
	}
	require.NoError(t, env.Client.Create(ctx, promptCM), "create prompt ConfigMap")

	mcp := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: clusterExportNamespace},
		Spec: v1alpha1.MCPServerSpec{
			Name:    "gh",
			Version: "1.0.0",
			Server: v1alpha1.MCPServerServer{
				URL:       "https://example.invalid/mcp",
				Transport: "http",
			},
			Tools: []v1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer")

	sbx := &v1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: clusterExportNamespace},
		Spec: v1alpha1.SidecarToolboxSpec{
			Name:         "sbx",
			Version:      "1.0.0",
			Source:       v1alpha1.SidecarToolboxSource{Image: "example.invalid/sbx:latest"},
			Sandbox:      v1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    v1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: v1alpha1.SidecarToolboxUpstream{Provider: "github-pat"},
			Tools:        []v1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sbx), "create SidecarToolbox")

	class := &v1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-class"},
		Spec:       v1alpha1.SpiceboxClassSpec{Image: "example.invalid/sandbox:latest"},
	}
	require.NoError(t, env.Client.Create(ctx, class), "create SpiceboxClass")

	// Non-builtin toolkit: the CR's metadata.name ("custom-toolkit-cr")
	// deliberately differs from spec.name ("custom-tool") to prove
	// OpenCluster resolves the Toolspec -> Toolkit edge by spec
	// (name, revision), not by object name.
	toolkit := &v1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-toolkit-cr"},
		Spec: v1alpha1.SpiceboxToolkitSpec{
			Name:            "custom-tool",
			ToolkitRevision: "v1",
			Target:          v1alpha1.ToolkitTarget{Binary: "/usr/bin/custom-tool"},
			Parser:          v1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:             v1alpha1.ToolkitEnv{Allowed: []v1alpha1.ToolkitEnvVar{}},
			Subcommands:     []v1alpha1.ToolkitSubcommand{minimalToolkitSubcommand()},
		},
	}
	require.NoError(t, env.Client.Create(ctx, toolkit), "create SpiceboxToolkit")

	toolspec := &v1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-ts"},
		Spec: v1alpha1.SpiceboxToolspecSpec{
			Name:             "custom-ts",
			Version:          "1",
			Toolkit:          v1alpha1.ToolspecToolkitRef{Name: "custom-tool", Revision: "v1"},
			AllowSubcommands: []string{"run"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, toolspec), "create SpiceboxToolspec")

	ac := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: clusterExportNamespace},
		Spec: v1alpha1.AgentClassSpec{
			DisplayName:   "Demo Agent",
			Description:   "A fake test agent.",
			SystemPrompt:  v1alpha1.PromptSource{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "prompt-cm", Key: "prompt"}},
			AgentIdentity: "pm-id",
			Model: &v1alpha1.ModelConfig{
				Provider: "test",
				Name:     "test-model",
				APIKey:   v1alpha1.SecretKeyRef{Name: "model-key", Key: "token"},
			},
			MCPServers: []v1alpha1.AgentClassMCPServerRef{
				{Name: "gh", Ref: "gh"},
			},
			SidecarToolboxes: []v1alpha1.AgentClassSidecarToolboxRef{
				{Name: "sbx", Ref: "sbx"},
			},
			ToolBundles: []v1alpha1.ToolBundle{
				{Name: "custom", Class: "pm-class", Toolspecs: []string{"custom-ts"}, AgentIdentity: "bundle-id"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	// Prove the real apiserver -- unlike cluster_test.go's fake client --
	// actually stamped server-managed metadata, so the "sanitize stripped
	// it" assertions below prove something real rather than trivially
	// passing because the fixture never had a uid/resourceVersion to
	// begin with.
	var rawAC v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ac), &rawAC))
	require.NotEmpty(t, rawAC.UID, "envtest should have assigned a real UID")
	require.NotEmpty(t, rawAC.ResourceVersion, "envtest should have assigned a real resourceVersion")

	// The whole point of this fixture: spec.model.apiKey names a Secret
	// ("model-key") that is never created. OpenCluster only ever records
	// the NAME -- if it ever attempted to read the value it would 404
	// here, so confirm the secret really is absent before exercising it.
	var missing corev1.Secret
	getErr := env.Client.Get(ctx, client.ObjectKey{Namespace: clusterExportNamespace, Name: "model-key"}, &missing)
	require.Error(t, getErr, "the referenced secret must not exist for this test to prove anything")

	b, err := source.OpenCluster(env.Client, clusterExportNamespace, "demo-agent").Bundle(ctx)
	require.NoError(t, err, "OpenCluster(...).Bundle must succeed with zero Secrets in the cluster")
	require.NoError(t, b.Validate())

	crs, err := b.CRs()
	require.NoError(t, err)
	byKindName := map[string]*unstructured.Unstructured{}
	for _, u := range crs {
		byKindName[u.GetKind()+"/"+u.GetName()] = u
	}
	for _, want := range []string{
		"AgentClass/demo-agent",
		"AgentIdentity/pm-id",
		"AgentIdentity/bundle-id", // spec.toolBundles[].agentIdentity override
		"MCPServer/gh",
		"SidecarToolbox/sbx",
		"SpiceboxClass/pm-class",
		"SpiceboxToolspec/custom-ts",
		"SpiceboxToolkit/custom-toolkit-cr", // resolved by spec (name, revision), not metadata.name
		"ConfigMap/prompt-cm",               // spec.systemPrompt.configMapRef, value embedded
	} {
		require.Contains(t, byKindName, want, "expected %s in the exported CR set", want)
	}
	assert.Len(t, crs, 9, "exactly the nine reachable CRs (no Channel in this fixture)")
	assert.NotContains(t, byKindName, "Secret/model-key", "no Secret CR should ever be exported")

	// The prompt ConfigMap's non-secret value is embedded verbatim.
	promptData, found, err := unstructured.NestedStringMap(byKindName["ConfigMap/prompt-cm"].Object, "data")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "You are a fake test agent.", promptData["prompt"])

	for key, u := range byKindName {
		_, hasStatus := u.Object["status"]
		assert.False(t, hasStatus, "%s: status should be stripped", key)
		assert.Empty(t, u.GetUID(), "%s: uid should be stripped even though envtest assigned one", key)
		assert.Empty(t, u.GetResourceVersion(), "%s: resourceVersion should be stripped even though envtest assigned one", key)
		assert.NotEmpty(t, u.GetAPIVersion(), "%s: apiVersion should be stamped", key)
	}

	secretKeys := map[string][]string{}
	for _, rs := range b.Manifest.Requires.Secrets {
		secretKeys[rs.Name] = rs.Keys
	}
	require.Contains(t, secretKeys, "model-key", "spec.model.apiKey should be recorded as a required secret")
	assert.ElementsMatch(t, []string{"token"}, secretKeys["model-key"])
	require.Contains(t, secretKeys, "demo-pat", "class-default identity secret should be recorded")
	require.Contains(t, secretKeys, "bundle-pat", "per-bundle identity override secret must be complete")
	assert.ElementsMatch(t, []string{"token"}, secretKeys["bundle-pat"])

	var sawQuestion bool
	for _, q := range b.Manifest.Questions {
		if q.Name != "model-key" {
			continue
		}
		sawQuestion = true
		assert.Equal(t, oap.QSecret, q.Type)
		require.NotNil(t, q.Secret)
		assert.True(t, q.Secret.OrExisting)
		// The question must name where its answer goes, or install collects
		// the credential and has nowhere to put it — see
		// TestClusterSource_Bundle_SecretQuestionsMaterializeTheirSecrets,
		// which drives the answer all the way through install.Resolve.
		require.NotNil(t, q.Secret.CreateSecret, "the question must be materializable")
		assert.Equal(t, "model-key", q.Secret.CreateSecret.Name)
		assert.Equal(t, "token", q.Secret.CreateSecret.Key)
	}
	assert.True(t, sawQuestion, "expected a secret Question for model-key")

	packed, err := oap.Pack(b)
	require.NoError(t, err)

	// A stand-in for what a real Secret VALUE would look like -- never
	// written anywhere in this test, since no Secret object exists at
	// all -- asserted absent from the packed bytes as a literal
	// regression guard on top of the structural proof above (Bundle
	// succeeded with the secret entirely absent from the cluster, which
	// is only possible because OpenCluster never Gets one).
	const wouldBeSecretValue = "sk-fake-example-should-never-leak-4f9c2a"
	assert.NotContains(t, string(packed), wouldBeSecretValue)

	unpacked, err := oap.Unpack(packed)
	require.NoError(t, err, "round-trip Pack -> Unpack")
	require.NoError(t, unpacked.Validate())
	assert.Equal(t, "demo-agent", unpacked.Manifest.Agent.Name)
}
