package steelthread_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// liveFixture returns one realistic live FixtureInput: an AgentClass with a
// real provider, one MCPServer with a real URL, an AgentIdentity, a demo-chat
// Channel of kind slack and a demo-hooks Channel of kind demoforge. mutate, if
// non-nil, is applied after construction, so every table case below differs
// from this base by exactly the thing it tests — which is what keeps a
// failure legible.
func liveFixture(t *testing.T, mutate func(*steelthread.FixtureInput)) steelthread.FixtureInput {
	t.Helper()

	const ns = "acme-prod"

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demoforge-agent", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: "DemoForgeBot",
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "demoprovider",
				Name:     "demo-model-1",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "demoforge-model-key", Key: "api-key"},
			},
			SystemPrompt:  spiceboxv1alpha1.PromptSource{Inline: "You are DemoForgeBot, a fixture agent."},
			AgentIdentity: "demoforge-identity",
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "demoforge", Ref: "demoforge-mcp"},
			},
		},
	}

	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demoforge-mcp", Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "demoforge",
			Version: "v1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.demoforge.invalid/v1", Transport: "http"},
			Auth:    spiceboxv1alpha1.MCPServerAuth{Credential: "demoforge-bearer"},
			Tools:   []spiceboxv1alpha1.MCPServerTool{{Name: "list_widgets", Intent: "List widgets."}},
		},
	}

	identity := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "demoforge-identity", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "demoforge-bearer",
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demoforge-mcp-token", Key: "access_token"},
					},
				},
			},
		},
	}

	chat := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-chat", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     "demoforge-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-chat-slack-creds"},
		},
	}
	hooks := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-hooks", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "demoforge",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "demoforge-agent",
			AuthzSubject:   "service:demoforge-hooks",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-hooks-creds"},
		},
	}

	in := steelthread.FixtureInput{
		Class:      class,
		MCPServers: []*spiceboxv1alpha1.MCPServer{mcp},
		Identity:   identity,
		Channels:   []*spiceboxv1alpha1.Channel{chat, hooks},
	}
	if mutate != nil {
		mutate(&in)
	}
	return in
}

// withServerSideMetadata stamps a UID, resourceVersion, generation, ownerRef,
// managedFields and a status condition onto the AgentClass, so the stripping
// case has something to strip.
func withServerSideMetadata(f *steelthread.FixtureInput) {
	f.Class.UID = types.UID("11111111-2222-3333-4444-555555555555")
	f.Class.ResourceVersion = "999"
	f.Class.Generation = 7
	f.Class.CreationTimestamp = metav1.Now()
	f.Class.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "owning-configmap", UID: types.UID("owner-uid")},
	}
	f.Class.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
	}
	f.Class.Status.Conditions = []metav1.Condition{
		{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Ready", Message: "ok", LastTransitionTime: metav1.Now()},
	}
}

// fixtureWithSecretValue returns liveFixture with live smuggled into an
// annotation/label on every kind of live object RewriteFixture handles.
//
// RewriteFixture never reads a live Secret's bytes at all — FixtureInput
// carries only CRs, never a Secret — so the only way live credential material
// could ever reach an emitted file is by riding along on a live object's own
// metadata: an operator who once pasted a live token into an annotation for
// debugging must not have it survive into a captured fixture. That is exactly
// the "annotation, a label, or a comment" case the package doc warns a
// field-by-field object check would miss, which is why this test scans raw
// file bytes instead.
func fixtureWithSecretValue(t *testing.T, live string) steelthread.FixtureInput {
	t.Helper()
	f := liveFixture(t, nil)
	leaked := map[string]string{"leaked-during-capture": live}
	f.Class.Annotations = leaked
	f.Class.Labels = leaked
	f.Identity.Annotations = leaked
	f.MCPServers[0].Annotations = leaked
	for _, ch := range f.Channels {
		ch.Annotations = leaked
	}
	return f
}

// decodeAgentClass finds the emitted AgentClass and unmarshals it, failing
// the test when absent rather than returning a zero value an assertion could
// pass against vacuously.
func decodeAgentClass(t *testing.T, files []steelthread.FixtureFile) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	var out spiceboxv1alpha1.AgentClass
	findDoc(t, files, "AgentClass", "", &out)
	return &out
}

// decodeMCPServer finds the emitted MCPServer and unmarshals it.
func decodeMCPServer(t *testing.T, files []steelthread.FixtureFile) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	var out spiceboxv1alpha1.MCPServer
	findDoc(t, files, "MCPServer", "", &out)
	return &out
}

// decodeSidecarToolbox finds the emitted SidecarToolbox named name.
func decodeSidecarToolbox(t *testing.T, files []steelthread.FixtureFile, name string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	var out spiceboxv1alpha1.SidecarToolbox
	findDoc(t, files, "SidecarToolbox", name, &out)
	return &out
}

// withSidecarToolbox adds a resolved sidecar toolbox to a live fixture, in the
// shape a real session's status.resolvedSidecarToolboxes carries: an LLM-facing
// Name that differs from the CR Ref, and a spec snapshot with its own tool
// allowlist.
func withSidecarToolbox(f *steelthread.FixtureInput) {
	f.Class.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
		{Name: "kube", Ref: "kube-tb"},
	}
	f.SidecarToolboxes = []spiceboxv1alpha1.ResolvedSidecarToolbox{{
		Name: "kube",
		Ref:  "kube-tb",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:      "kube",
			Version:   "1",
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "demo-kube-mcp:dev"},
			Sandbox:   spiceboxv1alpha1.SidecarToolboxSandbox{Class: "demo-sandbox"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080, Path: "/mcp"},
			// TWO tools, and only one is ever called by the capture tests. The
			// uncalled one is not padding: mcp.Synthesize returns an error when
			// the allowlist names a tool the live server does not expose, so a
			// declared-but-uncalled sidecar tool with no placeholder entry
			// takes down synthesis for the WHOLE toolbox at replay — including
			// the tools that were called.
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "pods", Intent: "List pods."},
				{Name: "nodes", Intent: "List nodes."},
			},
			// A secret input, because in production this is the ordinary shape:
			// a sidecar reaching a cluster is gated on a producer publishing the
			// credential. RewriteFixture drops it — see the case asserting so —
			// and the fixture carries one precisely so that dropping it is
			// something a test can observe.
			SecretInputs: []spiceboxv1alpha1.SidecarToolboxSecretInput{
				{Name: "kubeconfig", Deliver: "file:/var/run/kubeconfig", From: "kubeconfig"},
			},
		},
	}}
}

// decodeChannel finds the emitted Channel named name and unmarshals it.
func decodeChannel(t *testing.T, files []steelthread.FixtureFile, name string) *spiceboxv1alpha1.Channel {
	t.Helper()
	var out spiceboxv1alpha1.Channel
	findDoc(t, files, "Channel", name, &out)
	return &out
}

// findDoc scans every emitted file's YAML documents for the one carrying
// kind (optionally narrowed to metadata.name == name) and unmarshals it into
// out, failing the test outright when nothing matches — a zeroed out would
// make the caller's assertion pass vacuously instead of reporting the real
// defect: the rewrite never emitted the object at all.
func findDoc(t *testing.T, files []steelthread.FixtureFile, kind, name string, out any) {
	t.Helper()
	for _, f := range files {
		for _, doc := range splitYAMLDocs(f.YAML) {
			var probe struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal(doc, &probe); err != nil {
				continue
			}
			if probe.Kind != kind {
				continue
			}
			if name != "" && probe.Metadata.Name != name {
				continue
			}
			require.NoErrorf(t, yaml.Unmarshal(doc, out), "unmarshal %s %q", kind, name)
			return
		}
	}
	t.Fatalf("no %s %q found among %d emitted files", kind, name, len(files))
}

// splitYAMLDocs splits a "---\n"-joined multi-document YAML stream (the
// shape marshalDocs in fixture.go produces) into its individual documents.
func splitYAMLDocs(b []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(b, []byte("\n---\n")) {
		part = bytes.TrimSpace(part)
		if len(part) > 0 {
			docs = append(docs, part)
		}
	}
	return docs
}

// TestRewriteFixture covers every row of the rewrite table plus the exception.
// A table because the cases share exactly one shape: one live object in, one
// rewritten object out, differing only in which field is asserted.
func TestRewriteFixture(t *testing.T) {
	cases := []struct {
		name  string
		in    steelthread.FixtureInput
		check func(t *testing.T, files []steelthread.FixtureFile)
	}{
		{
			name: "the model provider becomes test/scripted, or the harness operator refuses the class",
			in: liveFixture(t, func(f *steelthread.FixtureInput) {
				f.Class.Spec.Model.Provider = "demoprovider"
				f.Class.Spec.Model.Name = "demo-model-1"
			}),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				cls := decodeAgentClass(t, files)
				assert.Equal(t, "test", cls.Spec.Model.Provider)
				assert.Equal(t, "scripted", cls.Spec.Model.Name)
			},
		},
		{
			name: "an MCPServer url becomes the {{MCP_URL}} sentinel the harness substitutes",
			in:   liveFixture(t, func(f *steelthread.FixtureInput) { f.MCPServers[0].Spec.Server.URL = "https://mcp.demo.invalid/v1" }),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				assert.Equal(t, "{{MCP_URL}}", decodeMCPServer(t, files).Spec.Server.URL)
			},
		},
		{
			name: "a conversational Channel becomes kind fake",
			in:   liveFixture(t, nil),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				assert.Equal(t, "fake", decodeChannel(t, files, "demo-chat").Spec.Kind)
			},
		},
		{
			// THE exception, and the one that decides whether trigger bundles
			// work. bt.Trigger says the Channel's OWN spec.kind selects the
			// delivery mechanics, and the driver signs with that Channel's own
			// credentials so HMAC verification, event filtering and channel-key
			// derivation all run for real. Rewriting it to fake would delete
			// the only thing a trigger bundle exists to exercise.
			name: "the INPUT Channel of a triggered session keeps its real kind",
			in:   liveFixture(t, func(f *steelthread.FixtureInput) { f.TriggerChannel = "demo-hooks" }),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				assert.Equal(t, "demoforge", decodeChannel(t, files, "demo-hooks").Spec.Kind,
					"rewriting the trigger's input Channel to fake would delete the delivery path under test")
			},
		},
		{
			// Without this CR the AgentClass never reaches Valid=True: its
			// sidecarToolboxes[].ref validation requires a SidecarToolbox to
			// exist, so a captured sidecar bundle would hang at boot.
			//
			// metadata.name is the REF, not the LLM-facing name — the two
			// differ, and emitting the wrong one leaves the ref dangling while
			// still producing a plausible-looking file.
			name: "a resolved sidecar toolbox is emitted as the CR its ref resolves to",
			in:   liveFixture(t, withSidecarToolbox),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				tb := decodeSidecarToolbox(t, files, "kube-tb")
				assert.Equal(t, "kube-tb", tb.Name, "the CR name must be the ref the class points at")
				assert.Equal(t, "default", tb.Namespace)
				assert.Equal(t, []spiceboxv1alpha1.MCPServerTool{
					{Name: "pods", Intent: "List pods."},
					{Name: "nodes", Intent: "List nodes."},
				}, tb.Spec.Tools,
					"the tool allowlist is what declaredTools reads server-side names from")
				assert.Equal(t, "/mcp", tb.Spec.Transport.Path,
					"transport.path rides through: EndpointPath appends it to the dispatch URL and the "+
						"harness stub answers every path")
			},
		},
		{
			// The gate this drops is what made a real capture unreplayable: a
			// secret-gated sidecar's tools are synthesized only once a PRODUCER
			// publishes the per-session secret-output Secret, and a bundle has no
			// producer, so the tools would be absent for the whole replay and
			// every call to one would fail as unknown.
			//
			// Dropping the DECLARATION is a fixture rewrite of the same class as
			// replacing an MCPServer's real auth with a placeholder Secret: at
			// replay the sidecar is the fake MCP stub and needs no credential.
			// It is specifically NOT the driver manufacturing a Secret behind a
			// gate that still stands, which would mask a regression in the gate.
			name: "a secret-gated sidecar's gate is dropped, because the replay has no producer to open it",
			in:   liveFixture(t, withSidecarToolbox),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				tb := decodeSidecarToolbox(t, files, "kube-tb")
				assert.Empty(t, tb.Spec.SecretInputs,
					"a gate the fixture cannot satisfy must not be declared: it would hold the sidecar's "+
						"tools back for the entire replay")
				assert.Len(t, tb.Spec.Tools, 2,
					"dropping the gate must not disturb the allowlist the tools are synthesized from")
			},
		},
		{
			// The negative control for the case above. Dropping secretInputs is
			// a REWRITE of the emitted document, and a rewrite that reached back
			// into the caller's slice would corrupt the resolved status the
			// self-check and declaredTools both read from the same FixtureInput.
			name: "dropping the gate does not mutate the caller's resolved status",
			in:   liveFixture(t, withSidecarToolbox),
			check: func(t *testing.T, _ []steelthread.FixtureFile) {
				// in is rebuilt per case, so re-derive the same input and assert
				// the live value is untouched after a rewrite of it.
				live := liveFixture(t, withSidecarToolbox)
				_, err := steelthread.RewriteFixture(live)
				require.NoError(t, err)
				assert.Len(t, live.SidecarToolboxes[0].Spec.SecretInputs, 1,
					"RewriteFixture must not strip the gate from the input it was handed")
			},
		},
		{
			// The AgentClass validates its refs against objects applied
			// earlier, and the harness applies a directory's *.yaml in sorted
			// order, so the toolbox file must sort BEFORE 03-agent.yaml.
			name: "the sidecar toolbox file sorts before the AgentClass that references it",
			in:   liveFixture(t, withSidecarToolbox),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				var tbAt, classAt = -1, -1
				for i, f := range files {
					switch f.Name {
					case "02a-sidecartoolbox.yaml":
						tbAt = i
					case "03-agent.yaml":
						classAt = i
					}
				}
				require.NotEqual(t, -1, tbAt, "no sidecar toolbox file was emitted")
				require.NotEqual(t, -1, classAt, "no agent file was emitted")
				assert.Less(t, files[tbAt].Name, files[classAt].Name,
					"the harness applies *.yaml in sorted name order; a class applied before its "+
						"SidecarToolbox never reaches Valid=True")
			},
		},
		{
			name: "server-side metadata is stripped so the fixture applies to a fresh envtest",
			in:   liveFixture(t, withServerSideMetadata),
			check: func(t *testing.T, files []steelthread.FixtureFile) {
				cls := decodeAgentClass(t, files)
				assert.Empty(t, cls.Status.Conditions)
				assert.Empty(t, cls.UID)
				assert.Empty(t, cls.ResourceVersion)
				assert.Empty(t, cls.OwnerReferences)
				assert.Empty(t, cls.ManagedFields)
				assert.Zero(t, cls.Generation)
				assert.Equal(t, "default", cls.Namespace, "envtest does not auto-create namespaces")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, err := steelthread.RewriteFixture(tc.in)
			require.NoError(t, err)
			require.NotEmpty(t, rewritten.Files)
			tc.check(t, rewritten.Files)
		})
	}
}

// TestRewriteFixture_RefusesASidecarWithNoRef pins the one guard on this path
// that a valid-looking fixture would otherwise sail past.
//
// The emitted CR's metadata.name IS the ref. With an empty one the file still
// writes — a SidecarToolbox named "" — and the failure surfaces much later as
// an AgentClass that never reaches Valid=True, naming a ref that resolves to
// nothing. Refusing at the point the fact is known is the difference between a
// message about the capture and a timeout inside a replay.
func TestRewriteFixture_RefusesASidecarWithNoRef(t *testing.T) {
	in := liveFixture(t, withSidecarToolbox)
	in.SidecarToolboxes[0].Ref = ""

	_, err := steelthread.RewriteFixture(in)
	require.Error(t, err, "a resolved sidecar with no ref cannot produce a CR the class can point at")
	assert.Contains(t, err.Error(), "no ref",
		"the error must name what is wrong, not merely fail")
}

// TestRewriteFixture_NoLiveSecretMaterialSurvives is the security-shaped test,
// and the reason it is separate from the table: it asserts an ABSENCE across
// every emitted file rather than one field of one object. A capture writes
// files into a repo, so a credential that survives the rewrite is committed.
func TestRewriteFixture_NoLiveSecretMaterialSurvives(t *testing.T) {
	const live = "live-token-value-must-not-survive"
	rewritten, err := steelthread.RewriteFixture(fixtureWithSecretValue(t, live))
	require.NoError(t, err)

	for _, f := range rewritten.Files {
		assert.NotContains(t, string(f.YAML), live,
			"file %s carries live credential material into a repo", f.Name)
	}
}

// TestRewriteFixture_ShadowedSecretsNamesOnlyPlaceholdersStandingInForLiveOnes
// pins the derivation the leak scan's gate runs on.
//
// The distinction is the whole point. A placeholder whose NAME came from a live
// reference stands in for material that exists, so the capture must have read
// that material to scan for it. A placeholder the rewrite INVENTED — because
// the class declared no apiKey, its model and credential coming from the
// settings tiers instead — stands in for nothing, and demanding a value for it
// refuses a valid capture for failing to supply something that cannot be had.
// That refusal is not hypothetical: it is what a live capture hit.
func TestRewriteFixture_ShadowedSecretsNamesOnlyPlaceholdersStandingInForLiveOnes(t *testing.T) {
	t.Run("every reference named: each one is reported", func(t *testing.T) {
		rewritten, err := steelthread.RewriteFixture(liveFixture(t, nil))
		require.NoError(t, err)

		assert.Equal(t, []string{
			"demo-chat-slack-creds", "demo-hooks-creds", "demoforge-mcp-token", "demoforge-model-key",
		}, rewritten.ShadowedSecrets,
			"sorted and deduplicated, so a re-capture of one session is byte-identical")
	})

	t.Run("a class with no model at all: its invented placeholder is not reported", func(t *testing.T) {
		rewritten, err := steelthread.RewriteFixture(liveFixture(t, func(f *steelthread.FixtureInput) {
			f.Class.Spec.Model = nil
		}))
		require.NoError(t, err)

		require.NotEmpty(t, rewritten.Files)
		assert.Contains(t, string(fileNamed(t, rewritten.Files, "00-secret.yaml").YAML), "demoforge-agent-placeholder",
			"the rewrite still emits a placeholder Secret, because the emitted class's apiKey must point somewhere")
		assert.NotContains(t, rewritten.ShadowedSecrets, "demoforge-agent-placeholder",
			"an invented name shadows nothing, so no live value can exist for it")
		assert.NotContains(t, rewritten.ShadowedSecrets, "demoforge-model-key",
			"the class named no apiKey, so nothing was substituted for that Secret either")
	})

	t.Run("a Channel with no credentialsRef: its invented placeholder is not reported", func(t *testing.T) {
		rewritten, err := steelthread.RewriteFixture(liveFixture(t, func(f *steelthread.FixtureInput) {
			f.Channels[0].Spec.CredentialsRef.SecretName = ""
		}))
		require.NoError(t, err)

		assert.NotContains(t, rewritten.ShadowedSecrets, "demo-chat-creds")
		assert.Contains(t, rewritten.ShadowedSecrets, "demo-hooks-creds",
			"the OTHER Channel still named one, and must still be required")
	})
}
