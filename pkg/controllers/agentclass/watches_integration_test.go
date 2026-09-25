//go:build integration

// Watch-wiring integration tests for the AgentClass controller. These
// tests exist because AgentClass validation reads several other CRs
// (Secret, ConfigMap, Channel, AgentIdentity, MCPServer,
// SpiceboxToolspec, SpiceboxClass, SidecarToolbox); if any of
// those watches is forgotten in SetupWithManager, the AgentClass
// status goes stale until something else nudges the controller. We
// hit this in production with MCPServer: AgentClass.status was stuck
// at Valid=False reason=AgentClassMCPServerInvalid because the
// referenced MCPServer flipped to Valid=True after the AgentClass's
// last reconcile, and the controller did not watch MCPServer so it
// never re-reconciled. We hit it AGAIN with SpiceboxToolspec:
// `oap agent install` applies an AgentClass and its toolspecs in the
// same instant, the AgentClass reconciled before the toolspec
// controller stamped Valid, and the class wedged at
// Valid=False reason=ToolspecMissing forever.
//
// The TestAgentClass_ReReconcilesOn* tests below let the manager
// dispatch reconciles via real watches rather than calling
// r.Reconcile directly. A missing watch surfaces as a timeout in the
// eventually() poll, exactly the way the production bug surfaced.

package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// startManager wires the AgentClass reconciler to a real manager and
// blocks until the cache has synced.
func startManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&agentclass.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr), "agentclass.SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

func eventuallyValid(t *testing.T, c client.Client, name string, want metav1.ConditionStatus, wantReason string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *metav1.Condition
	for time.Now().Before(deadline) {
		var got spiceboxv1alpha1.AgentClass
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got); err == nil {
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			last = cond
			if cond != nil && cond.Status == want && (wantReason == "" || cond.Reason == wantReason) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("AgentClass %s never reached Valid=%s reason=%s within %v; last=%+v", name, want, wantReason, d, last)
}

// mcpServerWithValid creates an MCPServer and sets its Valid status
// condition to the requested value. Used to drive the watch tests.
func mcpServerWithValid(t *testing.T, c client.Client, name string, valid metav1.ConditionStatus) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, c.Create(context.Background(), mcp), "create MCPServer %s", name)
	if valid != "" {
		mcp.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{
			{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: valid, Reason: "OK", LastTransitionTime: metav1.Now()},
		}}
		require.NoError(t, c.Status().Update(context.Background(), mcp),
			"status update MCPServer %s to %s", name, valid)
	}
	return mcp
}

// TestAgentClass_ReReconcilesOnMCPServerStatusChange is the
// regression test for the production bug: AgentClass should flip to
// Valid=True after the dependency MCPServer transitions to Valid=True,
// without any manual poke. Fails (times out) when SetupWithManager
// does not register a watch on MCPServer.
func TestAgentClass_ReReconcilesOnMCPServerStatusChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	aiPM := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm", Namespace: "default"},
	}
	require.NoError(t, env.Client.Create(ctx, aiPM), "create AgentIdentity")
	// The class propagates identity validity; stamp Valid=True so the
	// MCPServer-status flip is what drives the class to Valid=True.
	aiPM.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, aiPM), "stamp AgentIdentity pm Valid=True")
	// MCPServer exists but has no Valid condition yet — same shape as
	// the production race where the AgentClass reconciled before the
	// MCPServer controller had set its status.
	mcp := mcpServerWithValid(t, env.Client, "linear", "")

	ac := newClass("ac-mcp-watch")
	ac.Spec.AgentIdentity = "pm"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	// First the AgentClass should land at Valid=False with the
	// MCPServerInvalid reason — this confirms the controller actually
	// reconciled and saw a not-yet-Valid MCPServer.
	eventuallyValid(t, env.Client, "ac-mcp-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonAgentClassMCPServerInvalid, 5*time.Second)

	// Now flip the MCPServer to Valid=True. The AgentClass controller
	// MUST observe this through a watch and re-reconcile.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(mcp), mcp),
		"refresh MCPServer before status flip")
	mcp.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue,
		Reason: "OK", LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, mcp), "flip MCPServer to Valid=True")

	eventuallyValid(t, env.Client, "ac-mcp-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// spiceboxClassFixture creates a cluster-scoped SpiceboxClass with a
// single "git" tool, matching the toolkit the toolspec fixtures target.
func spiceboxClassFixture(t *testing.T, c client.Client, name string) {
	t.Helper()
	require.NoError(t, c.Create(context.Background(), &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	}), "create SpiceboxClass %s", name)
}

// toolspecFixture creates a cluster-scoped SpiceboxToolspec targeting the
// "git" toolkit, with no Valid condition — the same shape a freshly
// `oap agent install`ed toolspec has before its controller reconciles it.
func toolspecFixture(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.SpiceboxToolspec {
	t.Helper()
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: name, Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	require.NoError(t, c.Create(context.Background(), ts), "create SpiceboxToolspec %s", name)
	return ts
}

// identityFixture creates an AgentIdentity stamped Valid=True so bundle
// tests exercise the dependency under test, not identity propagation. It
// carries the "git-token" credential the embedded git toolkit requires
// (GIT_TOKEN is sensitive), so credential-coverage validation passes.
func identityFixture(t *testing.T, c client.Client, name string) {
	t.Helper()
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "git-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "some-secret", Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, c.Create(context.Background(), ai), "create AgentIdentity %s", name)
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, c.Status().Update(context.Background(), ai), "stamp AgentIdentity %s Valid=True", name)
}

// TestAgentClass_ReReconcilesOnSpiceboxToolspecStatusChange is the
// regression test for the `oap agent install` wedge: the bundle applies
// the AgentClass and its SpiceboxToolspecs in the same instant, the
// AgentClass reconciles before the toolspec controller has stamped
// Valid, and lands Valid=False reason=ToolspecMissing ("has no Valid
// condition yet"). When the toolspec then flips to Valid=True the
// AgentClass MUST re-reconcile through a watch. Fails (times out)
// when SetupWithManager does not register a watch on SpiceboxToolspec.
func TestAgentClass_ReReconcilesOnSpiceboxToolspecStatusChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	identityFixture(t, env.Client, "ts-watch-id")
	spiceboxClassFixture(t, env.Client, "tb-ts-watch")
	ts := toolspecFixture(t, env.Client, "git-ts-watch") // no Valid condition yet

	ac := newClass("ac-ts-watch")
	ac.Spec.AgentIdentity = "ts-watch-id"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-ts-watch", Toolspecs: []string{"git-ts-watch"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	// The class must first land at ToolspecMissing — proving the
	// controller reconciled and saw the not-yet-Valid toolspec.
	eventuallyValid(t, env.Client, "ac-ts-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonToolspecMissing, 5*time.Second)

	// Now stamp the toolspec Valid=True, as its controller would. The
	// AgentClass controller MUST observe this through a watch.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ts), ts),
		"refresh SpiceboxToolspec before status flip")
	ts.Status = spiceboxv1alpha1.SpiceboxToolspecStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.SpiceboxToolspecConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ts), "flip SpiceboxToolspec to Valid=True")

	eventuallyValid(t, env.Client, "ac-ts-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// TestAgentClass_ReReconcilesOnSpiceboxClassCreate exercises the
// SpiceboxClass watch: the bundle's class does not exist yet, so the
// AgentClass lands Valid=False reason=ClassMissing; creating the
// SpiceboxClass must re-reconcile the AgentClass through a watch.
func TestAgentClass_ReReconcilesOnSpiceboxClassCreate(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	identityFixture(t, env.Client, "cls-watch-id")
	ts := toolspecFixture(t, env.Client, "git-cls-watch")
	stampToolspecValid(ctx, env.Client, ts)

	ac := newClass("ac-cls-watch")
	ac.Spec.AgentIdentity = "cls-watch-id"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-cls-watch", Toolspecs: []string{"git-cls-watch"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass before its SpiceboxClass")
	eventuallyValid(t, env.Client, "ac-cls-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonClassMissing, 5*time.Second)

	spiceboxClassFixture(t, env.Client, "tb-cls-watch")
	eventuallyValid(t, env.Client, "ac-cls-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// TestAgentClass_ReReconcilesOnSidecarToolboxStatusChange exercises the
// SidecarToolbox watch: the referenced toolbox exists but has no Valid
// condition yet (same install-ordering race as the toolspec case), so
// the AgentClass lands Valid=False reason=AgentClassSidecarToolboxInvalid;
// stamping the toolbox Valid=True must re-reconcile the AgentClass.
func TestAgentClass_ReReconcilesOnSidecarToolboxStatusChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-watch", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "tb-watch",
			Version:      "v0.1.0",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "example.com/tb-watch:v0.1.0"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "toolbelt"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{},
			Tools:        []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox (no Valid condition yet)")

	ac := newClass("ac-stb-watch")
	ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
		{Name: "tb", Ref: "tb-watch"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	eventuallyValid(t, env.Client, "ac-stb-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonAgentClassSidecarToolboxInvalid, 5*time.Second)

	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(tb), tb),
		"refresh SidecarToolbox before status flip")
	tb.Status = spiceboxv1alpha1.SidecarToolboxStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.SidecarToolboxConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, tb), "flip SidecarToolbox to Valid=True")

	eventuallyValid(t, env.Client, "ac-stb-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// TestAgentClass_ReReconcilesOnSecretCreate exercises the Secret
// watch. AgentClass references a Secret that does not exist yet, so
// it lands Valid=False reason=SecretMissing; then we create the
// Secret and the AgentClass should flip to Valid=True via the watch.
func TestAgentClass_ReReconcilesOnSecretCreate(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	ac := newClass("ac-secret-watch")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass without secret")
	eventuallyValid(t, env.Client, "ac-secret-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonSecretMissing, 5*time.Second)

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds after AgentClass exists")
	eventuallyValid(t, env.Client, "ac-secret-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// TestAgentClass_ReReconcilesOnAgentIdentityChange exercises the
// AgentIdentity watch: the binding is missing, then added, and the
// AgentClass must re-reconcile through the watch. This watch
// already exists today; the test pins it down so a future refactor
// cannot silently drop it.
func TestAgentClass_ReReconcilesOnAgentIdentityChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-id", Namespace: "default"},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity (no credentials yet)")
	// Stamp Valid=True so this test exercises the credential-coverage
	// watch path (BindingMissing → Valid after the credential is added),
	// not the identity-validity propagation.
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ai), "stamp AgentIdentity pm-id Valid=True")
	// MCPServer declares an explicit Auth.Credential so the AgentClass controller
	// requires a credential named "linear" on the identity. The credential name is
	// spec.auth.credential VERBATIM — there is no metadata.name fallback (a server
	// with auth.provider set but auth.credential empty is rejected at MCPServer
	// admission, reason=AuthCredentialMissing). See passthroughcatalog.CredentialNameForServer.
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "linear-oauth", Credential: "linear"},
		},
	}
	require.NoError(t, env.Client.Create(context.Background(), mcp), "create MCPServer linear")
	mcp.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(context.Background(), mcp), "flip MCPServer linear to Valid=True")

	ac := newClass("ac-id-watch")
	ac.Spec.AgentIdentity = "pm-id"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	eventuallyValid(t, env.Client, "ac-id-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonAgentIdentityBindingMissing, 5*time.Second)

	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ai), ai),
		"refresh AgentIdentity before credential update")
	ai.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{{
		Name: "linear", Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "dummy", Key: "token"},
		},
	}}
	require.NoError(t, env.Client.Update(ctx, ai), "update AgentIdentity with linear credential")
	eventuallyValid(t, env.Client, "ac-id-watch", metav1.ConditionTrue, "", 5*time.Second)
}

// toolkitFixture creates a cluster-scoped SpiceboxToolkit carrying one
// state-mutating subcommand. When perm is nil the toolkit is invalid under
// enforcing mode (a destructive subcommand with no permission block); passing
// a permission makes it valid. No sensitive env vars, so the class's
// credential-coverage check stays out of the way of the dependency under test.
func toolkitFixture(t *testing.T, c client.Client, name string, perm *authz.Permission) *spiceboxv1alpha1.SpiceboxToolkit {
	t.Helper()
	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            name,
			ToolkitRevision: "1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/local/bin/" + name},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:             spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{{
				Path: []string{"apply"},
				Effects: spiceboxv1alpha1.ToolkitEffects{
					Destructive: true,
					Reads:       []string{},
					Writes:      []string{"cluster"},
					Network:     spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
					Filesystem:  spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
					Creds:       spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
				},
				Permission: perm,
			}},
		},
	}
	require.NoError(t, c.Create(context.Background(), tk), "create SpiceboxToolkit %s", name)
	return tk
}

// TestAgentClass_ReReconcilesOnSpiceboxToolkitChange exercises the
// SpiceboxToolkit watch. AgentClass validation walks toolBundles → toolspecs →
// SpiceboxToolkit and, in enforcing mode (the default), rejects a toolkit
// whose state-mutating subcommand carries no permission block. Repairing the
// toolkit must re-reconcile the AgentClass through a watch: nothing about the
// AgentClass, its bundle class, or its toolspec changes, and the AgentClass
// invalid path returns a bare ctrl.Result{} with no requeue — so without a
// SpiceboxToolkit watch the class stays Valid=False permanently.
func TestAgentClass_ReReconcilesOnSpiceboxToolkitChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")
	identityFixture(t, env.Client, "tk-watch-id")

	// The bundle's SpiceboxClass must carry a tool named after the toolkit —
	// bundle validation checks the toolspec's toolkit against the class's
	// tool catalog before the toolkit walk is ever reached.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-tk-watch"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "sre-tk-watch", Command: []string{"/usr/local/bin/sre-tk-watch"}}},
		},
	}), "create SpiceboxClass tb-tk-watch")

	// Toolkit starts invalid: destructive subcommand, no permission block.
	tk := toolkitFixture(t, env.Client, "sre-tk-watch", nil)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "sre-ts-tk-watch"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "sre-ts-tk-watch",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "sre-tk-watch", Revision: "1"},
			AllowSubcommands: []string{"apply"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ts), "create SpiceboxToolspec")
	stampToolspecValid(ctx, env.Client, ts)

	ac := newClass("ac-tk-watch")
	ac.Spec.AgentIdentity = "tk-watch-id"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "sre", Class: "tb-tk-watch", Toolspecs: []string{"sre-ts-tk-watch"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	eventuallyValid(t, env.Client, "ac-tk-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonToolPermissionMissing, 5*time.Second)

	// Repair the toolkit — the only object that changes.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(tk), tk),
		"refresh SpiceboxToolkit before repair")
	tk.Spec.Subcommands[0].Permission = &authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType:       "cluster",
			Permission:         "write",
			ResourceIDTemplate: "fixed",
		},
	}
	require.NoError(t, env.Client.Update(ctx, tk), "add permission block to toolkit subcommand")

	eventuallyValid(t, env.Client, "ac-tk-watch", metav1.ConditionTrue, "", 10*time.Second)
}

// TestAgentClass_ReReconcilesOnAgentUIStatusChange exercises the AgentUI
// watch. The referenced AgentUI exists but has no Valid condition yet (the
// same install-ordering race as the toolspec and sidecar-toolbox cases), so
// the AgentClass lands Valid=False reason=AgentUIInvalid; stamping the AgentUI
// Valid=True must re-reconcile the AgentClass. Without the watch the class
// stays parked forever — the invalid path returns a bare ctrl.Result{} with no
// requeue, and nothing about the AgentClass itself changes.
func TestAgentClass_ReReconcilesOnAgentUIStatusChange(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")

	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "aui-watch", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "main"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, aui), "create AgentUI (no Valid condition yet)")

	ac := newClass("ac-aui-watch")
	ac.Spec.AgentUI = &spiceboxv1alpha1.AgentClassUIGrant{Ref: "aui-watch"}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	eventuallyValid(t, env.Client, "ac-aui-watch", metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid, 5*time.Second)

	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(aui), aui),
		"refresh AgentUI before status flip")
	aui.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentUIConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAgentUISpecOK,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, aui), "flip AgentUI to Valid=True")

	eventuallyValid(t, env.Client, "ac-aui-watch", metav1.ConditionTrue, "", 10*time.Second)
}
