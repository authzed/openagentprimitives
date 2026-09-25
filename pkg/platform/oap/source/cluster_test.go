package source_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind.Kinds so
	// collectDepSecretRefs's registry dispatch (credkindregistry.Get) resolves
	// the Static/OAuth fixtures below instead of failing closed on every one
	// as an unregistered type — a test binary is its own process, and another
	// package's blank import of credkind/imports does not leak in.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

// fixtureSet holds every CR the ref-graph walk should reach, so individual
// tests can drop one (set its field to nil) to exercise a dangling ref before
// building the client. Coverage per field:
//   - agentIdentity     : spec.agentIdentity (class default)
//   - bundleIdentity    : spec.toolBundles[].agentIdentity (per-bundle override)
//   - agentUI           : spec.agentUI.ref
//   - promptCM          : spec.systemPrompt.configMapRef
//   - sidecarScriptCM   : SidecarToolbox source.inline.script.configMapRef
//   - toolkit           : the non-builtin SpiceboxToolkit a Toolspec resolves to
//
// A non-builtin toolkit ("custom-tool"@"v1") is used deliberately so the walk
// must resolve it via a SpiceboxToolkit CR (whose metadata.name differs from
// its spec.name, proving spec-based resolution). Every fixture carries status +
// uid/resourceVersion (and the sidecar a finalizer) so tests can assert Sanitize
// stripped them.
type fixtureSet struct {
	agentClass      *v1alpha1.AgentClass
	agentIdentity   *v1alpha1.AgentIdentity
	bundleIdentity  *v1alpha1.AgentIdentity
	agentUI         *v1alpha1.AgentUI
	mcpServer       *v1alpha1.MCPServer
	sidecarToolbox  *v1alpha1.SidecarToolbox
	spiceboxClass   *v1alpha1.SpiceboxClass
	toolspec        *v1alpha1.SpiceboxToolspec
	toolkit         *v1alpha1.SpiceboxToolkit
	promptCM        *corev1.ConfigMap
	sidecarScriptCM *corev1.ConfigMap
}

// all returns the non-nil fixtures as client.Objects for fake.WithObjects. A
// field set to nil is omitted (so a nil typed pointer never becomes a non-nil
// client.Object interface).
func (f *fixtureSet) all() []client.Object {
	var objs []client.Object
	if f.agentClass != nil {
		objs = append(objs, f.agentClass)
	}
	if f.agentIdentity != nil {
		objs = append(objs, f.agentIdentity)
	}
	if f.bundleIdentity != nil {
		objs = append(objs, f.bundleIdentity)
	}
	if f.agentUI != nil {
		objs = append(objs, f.agentUI)
	}
	if f.mcpServer != nil {
		objs = append(objs, f.mcpServer)
	}
	if f.sidecarToolbox != nil {
		objs = append(objs, f.sidecarToolbox)
	}
	if f.spiceboxClass != nil {
		objs = append(objs, f.spiceboxClass)
	}
	if f.toolspec != nil {
		objs = append(objs, f.toolspec)
	}
	if f.toolkit != nil {
		objs = append(objs, f.toolkit)
	}
	if f.promptCM != nil {
		objs = append(objs, f.promptCM)
	}
	if f.sidecarScriptCM != nil {
		objs = append(objs, f.sidecarScriptCM)
	}
	return objs
}

// staticIdentity builds an AgentIdentity with one static credential pointing at
// secret secretName/"token".
func staticIdentity(name, uid, secretName string) *v1alpha1.AgentIdentity {
	return &v1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			UID:             types.UID(uid),
			ResourceVersion: "7",
		},
		Spec: v1alpha1.AgentIdentitySpec{
			Credentials: []v1alpha1.AgentCredential{
				{
					Name:   "cred",
					Type:   "static",
					Static: &v1alpha1.StaticCredentialSource{SecretRef: v1alpha1.SecretKeyRef{Name: secretName, Key: "token"}},
				},
			},
		},
		Status: v1alpha1.AgentIdentityStatus{ResolvedCredentials: []string{"cred"}},
	}
}

// agentUIFixture builds an AgentUI with one slot, carrying status +
// uid/resourceVersion like the rest of the fixtures so tests can assert
// Sanitize stripped them.
func agentUIFixture(name string) *v1alpha1.AgentUI {
	return &v1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			UID:             types.UID("fake-uid-" + name),
			ResourceVersion: "13",
		},
		Spec: v1alpha1.AgentUISpec{
			DisplayName: "Demo Chat",
			Slots:       []v1alpha1.AgentUISlot{{Name: "main"}},
		},
		Status: v1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAgentUISpecOK, Message: "ok", LastTransitionTime: metav1.Now()},
			},
		},
	}
}

func promptConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			UID:             types.UID("fake-uid-" + name),
			ResourceVersion: "11",
		},
		Data: map[string]string{"prompt": "You are a fake test agent."},
	}
}

func fixtures() *fixtureSet {
	ac := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "demo-agent",
			Namespace:       "default",
			UID:             types.UID("fake-uid-agentclass"),
			ResourceVersion: "42",
		},
		Spec: v1alpha1.AgentClassSpec{
			DisplayName:   "Demo Agent",
			Description:   "A fake test agent.",
			SystemPrompt:  v1alpha1.PromptSource{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "prompt-cm", Key: "prompt"}},
			AgentIdentity: "pm-id",
			AgentUI:       &v1alpha1.AgentClassUIGrant{Ref: "chat-ui", GrantedTools: []string{"read_file"}},
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
				// Per-bundle identity override -> bundle-id.
				{Name: "custom", Class: "pm-class", Toolspecs: []string{"custom-ts"}, AgentIdentity: "bundle-id"},
			},
			Skills: []v1alpha1.AgentSkill{{Name: "planner", Ref: "github.com/fakeorg/fakerepo//skills/planner@v1.0.0"}},
		},
		Status: v1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{
				{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK", Message: "ok", LastTransitionTime: metav1.Now()},
			},
		},
	}

	mcp := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gh",
			Namespace:       "default",
			UID:             types.UID("fake-uid-mcpserver"),
			ResourceVersion: "3",
		},
		Spec: v1alpha1.MCPServerSpec{
			Name:    "gh",
			Version: "1.0.0",
			Server: v1alpha1.MCPServerServer{
				URL:       "https://example.invalid/mcp",
				Transport: "http",
			},
		},
		Status: v1alpha1.MCPServerStatus{ObservedTools: []string{"list_issues"}},
	}

	sbx := &v1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "sbx",
			Namespace:       "default",
			UID:             types.UID("fake-uid-sidecar"),
			ResourceVersion: "9",
			Finalizers:      []string{"spicebox.authzed.com/sidecartoolbox"},
		},
		Spec: v1alpha1.SidecarToolboxSpec{
			Name:    "sbx",
			Version: "1.0.0",
			// Inline source backed by a ConfigMap script.
			Source: v1alpha1.SidecarToolboxSource{
				Inline: &v1alpha1.SidecarToolboxInlineSource{
					BaseImage:  "example.invalid/base:latest",
					Script:     v1alpha1.SidecarToolboxScriptSource{ConfigMapRef: v1alpha1.SidecarToolboxConfigMapKeyRef{Name: "sidecar-script-cm", Key: "run.sh"}},
					Entrypoint: []string{"/run.sh"},
				},
			},
			Sandbox: v1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
		},
		Status: v1alpha1.SidecarToolboxStatus{ObservedTools: []string{"echo"}},
	}

	class := &v1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pm-class",
			UID:             types.UID("fake-uid-class"),
			ResourceVersion: "5",
		},
		Spec: v1alpha1.SpiceboxClassSpec{Image: "example.invalid/sandbox:latest"},
		Status: v1alpha1.SpiceboxClassStatus{
			Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "OK"}},
		},
	}

	ts := &v1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "custom-ts",
			UID:             types.UID("fake-uid-toolspec"),
			ResourceVersion: "6",
		},
		Spec: v1alpha1.SpiceboxToolspecSpec{
			Name:             "custom-ts",
			Toolkit:          v1alpha1.ToolspecToolkitRef{Name: "custom-tool", Revision: "v1"},
			AllowSubcommands: []string{"run"},
		},
		Status: v1alpha1.SpiceboxToolspecStatus{ObservedGeneration: 1},
	}

	tk := &v1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "custom-toolkit-cr", // deliberately != spec.name
			UID:             types.UID("fake-uid-toolkit"),
			ResourceVersion: "4",
		},
		Spec: v1alpha1.SpiceboxToolkitSpec{
			Name:            "custom-tool",
			ToolkitRevision: "v1",
		},
		Status: v1alpha1.SpiceboxToolkitStatus{ObservedGeneration: 1},
	}

	sidecarCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "sidecar-script-cm",
			Namespace:       "default",
			UID:             types.UID("fake-uid-sidecar-cm"),
			ResourceVersion: "12",
		},
		Data: map[string]string{"run.sh": "#!/bin/sh\necho hi\n"},
	}

	return &fixtureSet{
		agentClass:      ac,
		agentIdentity:   staticIdentity("pm-id", "fake-uid-agentidentity", "demo-pat"),
		bundleIdentity:  staticIdentity("bundle-id", "fake-uid-bundle-id", "bundle-pat"),
		agentUI:         agentUIFixture("chat-ui"),
		mcpServer:       mcp,
		sidecarToolbox:  sbx,
		spiceboxClass:   class,
		toolspec:        ts,
		toolkit:         tk,
		promptCM:        promptConfigMap("prompt-cm"),
		sidecarScriptCM: sidecarCM,
	}
}

func buildBundle(t *testing.T, f *fixtureSet) (*oap.Bundle, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(f.all()...).Build()
	return source.OpenCluster(c, "default", "demo-agent").Bundle(context.Background())
}

func TestClusterSource_Bundle_HappyPath(t *testing.T) {
	b, err := buildBundle(t, fixtures())
	require.NoError(t, err)
	require.NotNil(t, b)
	require.NoError(t, b.Validate())

	assert.Equal(t, "demo-agent", b.Manifest.Agent.Name)
	assert.Equal(t, "0.1.0", b.Manifest.Agent.Version, "no version annotation/label set -> default")
	assert.Equal(t, "1", b.Manifest.OapFormatVersion)

	crs, err := b.CRs()
	require.NoError(t, err)

	byKindName := map[string]*unstructured.Unstructured{}
	for _, u := range crs {
		byKindName[u.GetKind()+"/"+u.GetName()] = u
	}
	// Every reachable dep: root + the forward ref kinds + the toolkit recursion +
	// both ConfigMaps + both AgentIdentities (class default, per-bundle override).
	// Channels are NOT exported (per-install UI config), so no Channel and no
	// channel-scoped identity appear here.
	for _, want := range []string{
		"AgentClass/demo-agent",
		"AgentIdentity/pm-id",
		"AgentIdentity/bundle-id", // spec.toolBundles[].agentIdentity
		"AgentUI/chat-ui",         // spec.agentUI.ref
		"MCPServer/gh",
		"SidecarToolbox/sbx",
		"SpiceboxClass/pm-class",
		"SpiceboxToolspec/custom-ts",
		"SpiceboxToolkit/custom-toolkit-cr", // resolved by spec (name,revision)
		"ConfigMap/prompt-cm",               // spec.systemPrompt.configMapRef
		"ConfigMap/sidecar-script-cm",       // sidecar inline source
	} {
		require.Contains(t, byKindName, want, "expected %s in the exported CR set", want)
	}
	assert.NotContains(t, byKindName, "Channel/pm-slack", "Channels are not exported into a bundle")
	assert.Len(t, crs, 11, "exactly the eleven reachable CRs, de-duplicated (no Channel, no channel identity)")

	for key, u := range byKindName {
		_, hasStatus := u.Object["status"]
		assert.False(t, hasStatus, "%s: status should be stripped", key)
		assert.Empty(t, u.GetUID(), "%s: uid should be stripped", key)
		assert.Empty(t, u.GetResourceVersion(), "%s: resourceVersion should be stripped", key)
		assert.Empty(t, u.GetFinalizers(), "%s: finalizers should be stripped", key)
		assert.NotEmpty(t, u.GetAPIVersion(), "%s: apiVersion should be stamped", key)
	}
	// The ConfigMap VALUE is embedded (non-secret), unlike a Secret.
	promptData, found, err := unstructured.NestedStringMap(byKindName["ConfigMap/prompt-cm"].Object, "data")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "You are a fake test agent.", promptData["prompt"])

	secretKeys := map[string][]string{}
	for _, rs := range b.Manifest.Requires.Secrets {
		secretKeys[rs.Name] = rs.Keys
	}
	assert.ElementsMatch(t, []string{"token"}, secretKeys["model-key"], "spec.model.apiKey")
	assert.ElementsMatch(t, []string{"token"}, secretKeys["demo-pat"], "class-default identity secret")
	assert.ElementsMatch(t, []string{"token"}, secretKeys["bundle-pat"], "per-bundle identity override secret must be complete")
	assert.NotContains(t, secretKeys, "slack-creds", "a Channel's credentials secret is not exported (Channels aren't bundled)")
	assert.NotContains(t, secretKeys, "channel-pat", "a channel-scoped identity's secret is not exported")

	var sawModelKeyQuestion bool
	for _, q := range b.Manifest.Questions {
		if q.Name != "model-key" {
			continue
		}
		sawModelKeyQuestion = true
		assert.Equal(t, oap.QSecret, q.Type)
		require.NotNil(t, q.Secret)
		assert.True(t, q.Secret.OrExisting)
		require.NotNil(t, q.Secret.CreateSecret, "the question must name where its answer goes")
		assert.Equal(t, oap.SecretTarget{Name: "model-key", Key: "token"}, *q.Secret.CreateSecret)
	}
	assert.True(t, sawModelKeyQuestion, "expected a secret Question for model-key")

	require.Len(t, b.Manifest.Requires.Skills, 1)
	assert.Equal(t, "github.com/fakeorg/fakerepo//skills/planner@v1.0.0", b.Manifest.Requires.Skills[0].Canonical)
}

// TestClusterSource_Bundle_SecretQuestionsMaterializeTheirSecrets is the join
// between the two halves of an exported bundle's credential story: this
// exporter writes the questions, and install.Resolve is what turns the answers
// into Secrets. Neither side's own tests can see the seam.
//
// An exported question that asks for a value the resolver has nowhere to put
// is worse than no question at all: the operator types every credential, the
// install reports success, and the applied CRs reference Secrets nothing
// created. So every question this exporter emits must name the Secret name and
// key the exported CRs actually read.
func TestClusterSource_Bundle_SecretQuestionsMaterializeTheirSecrets(t *testing.T) {
	b, err := buildBundle(t, fixtures())
	require.NoError(t, err)

	// Answer every question the exporter emitted, exactly as an operator
	// installing this bundle into a fresh cluster would.
	sets := map[string]string{}
	for _, q := range b.Manifest.Questions {
		require.Equal(t, oap.QSecret, q.Type, "this fixture's only questions are the exported secrets")
		sets[q.Name] = "typed-value-for-" + q.Name
	}
	require.NotEmpty(t, sets, "the fixture references secrets, so questions must have been emitted")

	_, secrets, err := install.Resolve(b.Manifest.Questions, "", sets, false)
	require.NoError(t, err, "an exported bundle's own questions must resolve")

	got := map[string]string{}
	for _, s := range secrets {
		got[s.Name+"/"+s.Key] = s.Value
	}
	assert.Contains(t, got, "model-key/token", "spec.model.apiKey's value must be created")
	assert.Contains(t, got, "demo-pat/token", "the class-default identity's credential must be created")
	assert.Contains(t, got, "bundle-pat/token", "the per-bundle identity override's credential must be created")

	// Every keyed required secret is covered — the check that keeps this test
	// honest as the fixture grows.
	for _, rs := range b.Manifest.Requires.Secrets {
		for _, k := range rs.Keys {
			assert.Contains(t, got, rs.Name+"/"+k,
				"requires.secrets[%s].keys[%s] is declared but no answer materializes it", rs.Name, k)
		}
	}
}

// TestClusterSource_Bundle_SecretQuestionShapes covers what an exported
// question must look like for each shape of Secret reference the ref-graph
// walk can produce, since the shape decides whether install can collect the
// value at all.
func TestClusterSource_Bundle_SecretQuestionShapes(t *testing.T) {
	questionsFor := func(t *testing.T, b *oap.Bundle, secretName string) []oap.Question {
		t.Helper()
		var out []oap.Question
		for _, q := range b.Manifest.Questions {
			if q.Secret != nil && q.Secret.CreateSecret != nil && q.Secret.CreateSecret.Name == secretName {
				out = append(out, q)
			}
		}
		return out
	}

	t.Run("one key: one question named for the Secret, targeting that key", func(t *testing.T) {
		b, err := buildBundle(t, fixtures())
		require.NoError(t, err)

		qs := questionsFor(t, b, "demo-pat")
		require.Len(t, qs, 1, "one referenced key, one question")
		assert.Equal(t, "demo-pat", qs[0].Name, "a single-key Secret's question keeps the Secret's own name")
		assert.Equal(t, "token", qs[0].Secret.CreateSecret.Key)
		assert.True(t, qs[0].Secret.OrExisting, "the Secret may already exist in the target namespace")
	})

	t.Run("two keys on one Secret: a question per key, each named by key", func(t *testing.T) {
		f := fixtures()
		// A second static credential on the same Secret, different key: two
		// values are needed, so one question cannot collect them both.
		f.agentIdentity.Spec.Credentials = append(f.agentIdentity.Spec.Credentials, v1alpha1.AgentCredential{
			Name:   "cred2",
			Type:   "static",
			Static: &v1alpha1.StaticCredentialSource{SecretRef: v1alpha1.SecretKeyRef{Name: "demo-pat", Key: "org"}},
		})
		b, err := buildBundle(t, f)
		require.NoError(t, err)

		qs := questionsFor(t, b, "demo-pat")
		require.Len(t, qs, 2, "each referenced key needs its own answer")
		byKey := map[string]string{}
		for _, q := range qs {
			byKey[q.Secret.CreateSecret.Key] = q.Name
		}
		assert.Equal(t, map[string]string{"org": "demo-pat.org", "token": "demo-pat.token"}, byKey)
	})

	t.Run("whole-Secret OAuth reference: recorded as required, but no question asks for it", func(t *testing.T) {
		f := fixtures()
		f.agentIdentity.Spec.Credentials = []v1alpha1.AgentCredential{{
			Name:  "cred",
			Type:  "oauth",
			OAuth: &v1alpha1.OAuthCredentialSource{SecretRef: v1alpha1.SecretRef{Name: "oauth-creds"}},
		}}
		b, err := buildBundle(t, f)
		require.NoError(t, err)
		require.NoError(t, b.Validate())

		var required *oap.RequiredSecret
		for i, rs := range b.Manifest.Requires.Secrets {
			if rs.Name == "oauth-creds" {
				required = &b.Manifest.Requires.Secrets[i]
			}
		}
		require.NotNil(t, required, "the OAuth Secret is still recorded as required")
		assert.Empty(t, required.Keys, "the reference names the whole Secret, not a key")
		assert.Empty(t, required.Question,
			"no typed answer can reconstruct an OAuth token set; the identity's setup flow mints it")
		assert.Empty(t, questionsFor(t, b, "oauth-creds"), "nothing may ask for a value it cannot materialize")

		// And the bundle this produces still installs: a keyless requirement
		// with no question is coherent, not a preflight failure.
		require.NoError(t, install.Preflight(context.Background(), b, ""))
	})

	t.Run("unregistered credential type: its Secret is excluded, not a Bundle error", func(t *testing.T) {
		f := fixtures()
		f.agentIdentity.Spec.Credentials = []v1alpha1.AgentCredential{{
			Name: "cred",
			Type: "totally-unknown",
		}}
		b, err := buildBundle(t, f)
		require.NoError(t, err, "an unregistered credential type must not fail the whole bundle")
		require.NotNil(t, b)

		for _, rs := range b.Manifest.Requires.Secrets {
			assert.NotEqual(t, "cred", rs.Name,
				"an unregistered type names no Secret to require, whole-guessed or otherwise")
		}
	})

	t.Run("federated credential (defensive): the IdP-identity Secret is still recorded as required", func(t *testing.T) {
		f := fixtures()
		f.agentIdentity.Spec.Credentials = []v1alpha1.AgentCredential{{
			Name: "cred",
			Type: "federated",
			Federated: &v1alpha1.FederatedCredentialSource{
				Resource:          "https://example.invalid/resource",
				ResourceServerURL: "https://example.invalid/as",
				IdPSecretRef:      v1alpha1.SecretRef{Name: "idp-identity"},
			},
		}}
		b, err := buildBundle(t, f)
		require.NoError(t, err)

		var required *oap.RequiredSecret
		for i, rs := range b.Manifest.Requires.Secrets {
			if rs.Name == "idp-identity" {
				required = &b.Manifest.Requires.Secrets[i]
			}
		}
		// credkind.Kind.SecretRef() deliberately reports federated as having NO
		// backing Secret (the IdP Secret is subject material, not the
		// credential's own store) — this assertion is pinned to
		// collectDepSecretRefs's own defensive inclusion of it, not to the
		// registry dispatch.
		require.NotNil(t, required, "the IdP-identity Secret the mint reads is still recorded as required")
		assert.Empty(t, required.Keys, "the reference names the whole Secret, not a key")
	})
}

func TestClusterSource_Bundle_DanglingRequiredAgentIdentity(t *testing.T) {
	f := fixtures()
	f.agentIdentity = nil // class-default spec.agentIdentity, not registered

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pm-id", "error should name the missing ref")
}

func TestClusterSource_Bundle_DanglingRequiredBundleIdentity(t *testing.T) {
	f := fixtures()
	f.bundleIdentity = nil // spec.toolBundles[].agentIdentity override, not registered

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle-id", "error should name the missing per-bundle identity override")
}

func TestClusterSource_Bundle_DanglingRequiredAgentUI(t *testing.T) {
	f := fixtures()
	f.agentUI = nil // spec.agentUI.ref names "chat-ui", now absent

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat-ui", "error should name the missing AgentUI")
}

func TestClusterSource_Bundle_NoAgentUI_ExportsNothingExtraAndDoesNotError(t *testing.T) {
	// spec.agentUI is +optional as a whole: an AgentClass that never sets it
	// must export cleanly with no AgentUI in the CR set and no error — Names()
	// returning nil for a nil grant means the walk never even attempts a Get.
	f := fixtures()
	f.agentClass.Spec.AgentUI = nil
	f.agentUI = nil // no dependent CR either, to prove the walk never Gets it

	b, err := buildBundle(t, f)
	require.NoError(t, err)

	crs, err := b.CRs()
	require.NoError(t, err)
	for _, u := range crs {
		assert.NotEqual(t, "AgentUI", u.GetKind(), "no spec.agentUI set -> no AgentUI CR should be exported")
	}
	assert.Len(t, crs, 10, "same ten CRs as the AgentUI-less baseline: root + forward refs + toolkit + both ConfigMaps, minus AgentUI")
}

func TestClusterSource_Bundle_DanglingRequiredPromptConfigMap(t *testing.T) {
	f := fixtures()
	f.promptCM = nil // spec.systemPrompt.configMapRef, not registered

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt-cm", "error should name the missing prompt ConfigMap")
	assert.Contains(t, err.Error(), "systemPrompt", "error should name the referencing field")
}

func TestClusterSource_Bundle_DanglingRequiredSidecarConfigMap(t *testing.T) {
	f := fixtures()
	f.sidecarScriptCM = nil // sidecar inline source.script.configMapRef, not registered

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sidecar-script-cm", "error should name the missing sidecar ConfigMap")
	assert.Contains(t, err.Error(), "sbx", "error should name the referencing SidecarToolbox")
}

func TestClusterSource_Bundle_DanglingRequiredToolkit(t *testing.T) {
	f := fixtures()
	f.toolkit = nil // Toolspec references non-builtin custom-tool@v1, now absent

	_, err := buildBundle(t, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom-tool", "error should name the missing toolkit")
	assert.Contains(t, err.Error(), "custom-ts", "error should name the referencing Toolspec")
}

func TestClusterSource_Bundle_RootNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := source.OpenCluster(c, "default", "does-not-exist").Bundle(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}
