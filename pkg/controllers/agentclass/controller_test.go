//go:build integration

// pkg/controllers/agentclass/controller_test.go
package agentclass_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent" // register agent kind (UserAttributable=false, SpawnsSessionOnInbound=false)
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"  // register fake kind (UserAttributable=true)
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind (UserAttributable=true)
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

func TestValidWhenSecretExists(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	require.NoError(t, env.Client.Create(ctx, sec), "create llm-creds")
	ac := newClass("ac1")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac1"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac1"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got conditions=%+v", got.Status.Conditions)
}

func TestInvalidWhenSecretMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	ac := newClass("ac2")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass without secret")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac2"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac2"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonSecretMissing),
		"want Valid=False/SecretMissing; got conditions=%+v", got.Status.Conditions)
}

func TestValidWithBundles(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	// Pre-create: Secret, AgentIdentity, SpiceboxClass, SpiceboxToolspec.
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	aiBundles := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "code-id", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			// Credential for the toolspec the bundle references — required
			// by the agentclass controller's credential-coverage check.
			// The git toolkit declares GIT_TOKEN as sensitive with an
			// explicit credential: of "git-token".
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "git-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "some-secret", Key: "token"},
				},
			}},
		},
	}
	_ = env.Client.Create(ctx, aiBundles)
	// The class now propagates identity validity; stamp the identity Valid=True
	// so this coverage test reaches Valid=True rather than AgentIdentityInvalid.
	stampIdentityValid(ctx, env.Client, aiBundles)
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "toolbelt"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-readonly"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-readonly", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	_ = env.Client.Create(ctx, ts)
	ts.Status = spiceboxv1alpha1.SpiceboxToolspecStatus{
		Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.SpiceboxToolspecConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()}},
	}
	_ = env.Client.Status().Update(ctx, ts)

	ac := newClass("ac-bundles")
	ac.Spec.AgentIdentity = "code-id"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "toolbelt", Toolspecs: []string{"git-readonly"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-bundles"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-bundles"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got conditions=%+v", got.Status.Conditions)
}

func TestInvalidWhenBundleClassMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	ac := newClass("ac-no-class")
	ac.Spec.AgentIdentity = "anything"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "missing", Toolspecs: []string{"git-readonly"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-no-class"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-no-class"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonClassMissing),
		"want Valid=False/ClassMissing; got conditions=%+v", got.Status.Conditions)
}

func TestBoundChannelsReconciles(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	// Pre-create the Secret the AgentClass references.
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-test")},
	})

	// Create AgentClass "ac-bc".
	ac := newClass("ac-bc")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	// Create 2 Channels targeting "ac-bc" and 1 targeting a different AgentClass.
	newChannel := func(name, agentClass, kind string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           kind,
				AgentClass:     agentClass,
				SessionScope:   "auto",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "llm-creds"},
				Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			},
		}
	}
	require.NoError(t, env.Client.Create(ctx, newChannel("ch-beta", "ac-bc", "fake")), "create ch-beta")
	require.NoError(t, env.Client.Create(ctx, newChannel("ch-alpha", "ac-bc", "fake")), "create ch-alpha")
	require.NoError(t, env.Client.Create(ctx, newChannel("ch-other", "ac-other", "fake")), "create ch-other")

	// Reconcile — should compute BoundChannels.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-bc"}})
	require.NoError(t, err, "Reconcile after channel creates")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-bc"}, &got),
		"Get AgentClass after reconcile")
	require.Len(t, got.Status.BoundChannels, 2,
		"want 2 BoundChannels; got=%+v", got.Status.BoundChannels)
	// Alphabetical order: ch-alpha before ch-beta.
	assert.Equal(t, "ch-alpha", got.Status.BoundChannels[0].Name, "BoundChannels[0].Name (alphabetical order)")
	assert.Equal(t, "ch-beta", got.Status.BoundChannels[1].Name, "BoundChannels[1].Name")
	for _, ref := range got.Status.BoundChannels {
		assert.Equal(t, "fake", ref.Kind, "BoundChannels[%s].Kind", ref.Name)
	}

	// Delete ch-alpha and reconcile again — list should shrink to 1.
	toDelete := newChannel("ch-alpha", "ac-bc", "fake")
	require.NoError(t, env.Client.Delete(ctx, toDelete), "delete ch-alpha")
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-bc"}})
	require.NoError(t, err, "Reconcile after delete")

	var got2 spiceboxv1alpha1.AgentClass
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-bc"}, &got2),
		"Get AgentClass after delete")
	require.Len(t, got2.Status.BoundChannels, 1,
		"after delete: want 1 BoundChannel; got=%+v", got2.Status.BoundChannels)
	assert.Equal(t, "ch-beta", got2.Status.BoundChannels[0].Name, "remaining BoundChannel after delete")
}

func TestAgentClass_InvalidWhenMCPServerMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})

	ac := newClass("ac-mcp-missing")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "linear", Ref: "missing-server"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-mcp-missing"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-mcp-missing"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassMCPServerMissing, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "missing-server", "message should name the missing ref")
}

// TestAgentClass_InvalidWhenAgentIdentityMissingMCPCredential regression-tests
// the scenario where an AgentClass references an authenticated MCPServer,
// but the bound AgentIdentity has no credential for it.
// Without this check the failure surfaces only at session start as
// MCPAuthResolutionFailed in the runner logs; with the check it shows up
// as Valid=False on the AgentClass at apply time.
func TestAgentClass_InvalidWhenAgentIdentityMissingMCPCredential(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	// AgentIdentity exists (and is Valid) but has no credential for the
	// linear MCPServer. Stamp Valid=True so the test exercises the
	// credential-coverage branch (BindingMissing), not the new
	// identity-validity propagation (AgentIdentityInvalid).
	aiNoMCP := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-tools-no-mcp", Namespace: "default"},
	}
	_ = env.Client.Create(ctx, aiNoMCP)
	stampIdentityValid(ctx, env.Client, aiNoMCP)
	// MCPServer with an auth provider (authenticated) that's already Valid=True.
	// The default credential name for this server is "linear" (srv.Name).
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer")
	mcp.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, mcp), "stamp MCPServer Valid=True")

	ac := newClass("ac-no-mcp-cred")
	ac.Spec.AgentIdentity = "pm-tools-no-mcp"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-no-mcp-cred"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-no-mcp-cred"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentIdentityBindingMissing, cond.Reason,
		"Valid reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "linear", "message should name the missing credential")
}

// TestAgentClass_ValidAfterMCPCredentialAdded covers the recovery path:
// after the user runs `setup-identity` and the OAuth flow writes the
// named credential into the AgentIdentity, the AgentClass re-validates
// to Valid=True.
func TestAgentClass_ValidAfterMCPCredentialAdded(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	// AgentIdentity has the "linear" credential that matches the server's
	// declared spec.auth.credential. Per the no-inference contract, the
	// credential name must be declared explicitly on the MCPServer.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-bound", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name:  "linear",
				Type:  "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "linear-oauth"}},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")
	stampIdentityValid(ctx, env.Client, ai)
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp", Credential: "linear"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer")
	mcp.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, mcp), "stamp MCPServer Valid=True")

	ac := newClass("ac-bound")
	ac.Spec.AgentIdentity = "pm-bound"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-bound"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-bound"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got conditions=%+v", got.Status.Conditions)
}

// TestAgentClass_PropagatesAgentIdentityValidity covers the validity-chain
// link this task adds: a class bound to an AgentIdentity that is NOT
// Valid=True (the empty-Secret case its own controller stamps as
// CredentialEmpty/Valid=False) must itself go Valid=False with reason
// AgentIdentityInvalid — even though the identity carries the right
// credential NAME and would pass the name-coverage check. Flipping the
// identity to Valid=True re-validates the class to Valid=True. Together
// these complete the AgentSession start gate (which parks any session
// whose AgentClass is not Valid).
func TestAgentClass_PropagatesAgentIdentityValidity(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	// AgentIdentity has the "linear" credential the MCPServer needs (name
	// coverage WOULD pass), but is NOT Valid=True — model the empty-Secret
	// case where the identity's own controller left it Valid=False.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-invalid", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name:  "linear",
				Type:  "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "linear-oauth"}},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentIdentityConditionValid,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonCredentialEmpty,
		LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(ctx, ai), "stamp AgentIdentity Valid=False")

	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp", Credential: "linear"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer")
	mcp.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{
		{Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Now()},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, mcp), "stamp MCPServer Valid=True")

	ac := newClass("ac-id-invalid")
	ac.Spec.AgentIdentity = "pm-invalid"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	// 1) Identity Valid=False → class Valid=False/AgentIdentityInvalid.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-id-invalid"}})
	require.NoError(t, err, "Reconcile (identity invalid)")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-id-invalid"}, &got),
		"Get AgentClass (identity invalid)")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status with invalid identity")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentIdentityInvalid, cond.Reason,
		"Valid reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "pm-invalid", "message should name the identity")

	// 2) Flip the identity to Valid=True → class re-validates to Valid=True.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ai), ai),
		"refresh AgentIdentity before flipping Valid")
	ai.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentIdentityConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "OK",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ai), "flip AgentIdentity Valid=True")

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-id-invalid"}})
	require.NoError(t, err, "Reconcile (identity valid)")
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-id-invalid"}, &got),
		"Get AgentClass (identity valid)")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"after identity flips Valid=True: want class Valid=True; got conditions=%+v", got.Status.Conditions)
}

// TestAgentClass_InvalidWhenAgentIdentityMissingMCPCredential's bundle
// sibling: a non-passthrough class whose per-bundle identity is not
// Valid=True must inherit AgentIdentityInvalid from the bundle's resolved
// identity, mirroring the MCPServer path.
func TestAgentClass_PropagatesAgentIdentityValidity_Bundle(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "code-id-invalid", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "git-token", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "some-secret", Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentIdentityConditionValid,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonCredentialEmpty,
		LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(ctx, ai), "stamp AgentIdentity Valid=False")

	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-id-invalid"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-readonly-id-invalid"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-readonly-id-invalid", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	_ = env.Client.Create(ctx, ts)
	stampToolspecValid(ctx, env.Client, ts)

	ac := newClass("ac-bundle-id-invalid")
	ac.Spec.AgentIdentity = "code-id-invalid"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-id-invalid", Toolspecs: []string{"git-readonly-id-invalid"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-bundle-id-invalid"}})
	require.NoError(t, err, "Reconcile")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-bundle-id-invalid"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonAgentIdentityInvalid),
		"bundle-bound invalid identity must yield AgentIdentityInvalid; conditions=%+v", got.Status.Conditions)
}

// TestAgentClass_InvalidWhenSidecarToolboxMissing verifies that an AgentClass
// referencing a nonexistent SidecarToolbox CR is marked Valid=False with
// reason AgentClassSidecarToolboxMissing, mirroring the MCPServer missing-ref
// check.
func TestAgentClass_InvalidWhenSidecarToolboxMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")

	ac := newClass("ac-stb-missing")
	ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
		{Name: "tb", Ref: "missing"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-stb-missing"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-stb-missing"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassSidecarToolboxMissing, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "missing", "message should name the missing ref")
}

// TestAgentClass_InvalidWhenSidecarToolboxNotValid verifies that an AgentClass
// referencing a SidecarToolbox CR that exists but has no Valid condition yet
// is marked Valid=False with reason AgentClassSidecarToolboxInvalid.
func TestAgentClass_InvalidWhenSidecarToolboxNotValid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")

	// SidecarToolbox exists but has no Valid condition yet.
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "my-tb",
			Version:      "v0.1.0",
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "example.com/my-tb:v0.1.0"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "toolbelt"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{},
			Tools:        []spiceboxv1alpha1.MCPServerTool{},
		},
	}
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := newClass("ac-stb-invalid")
	ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
		{Name: "tb", Ref: "my-tb"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-stb-invalid"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-stb-invalid"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassSidecarToolboxInvalid, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "my-tb", "message should name the SidecarToolbox")
}

func hasCondition(cs []metav1.Condition, t string, s metav1.ConditionStatus) bool {
	for _, c := range cs {
		if c.Type == t && c.Status == s {
			return true
		}
	}
	return false
}

// stampToolspecValid marks a freshly-created SpiceboxToolspec Valid=True
// in its status subresource. The agentclass controller's binding-coverage
// check requires this; without it, fixture-only tests that don't run the
// full toolspec controller would fail with ToolspecMissing.
func stampToolspecValid(ctx context.Context, c client.Client, ts *spiceboxv1alpha1.SpiceboxToolspec) {
	ts.Status = spiceboxv1alpha1.SpiceboxToolspecStatus{
		Conditions: []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "OK",
			LastTransitionTime: metav1.Now(),
		}},
	}
	_ = c.Status().Update(ctx, ts)
}

// stampIdentityValid marks a freshly-created AgentIdentity Valid=True in its
// status subresource. The agentclass controller now propagates the bound
// identity's validity into the class (reason AgentIdentityInvalid when the
// identity is not Valid=True); fixture-only tests that don't run the full
// AgentIdentity reconciler must stamp it so coverage/permission/conflict
// paths still exercise their intended branch.
func stampIdentityValid(ctx context.Context, c client.Client, ai *spiceboxv1alpha1.AgentIdentity) {
	ai.Status = spiceboxv1alpha1.AgentIdentityStatus{
		Conditions: []metav1.Condition{{
			Type:               spiceboxv1alpha1.AgentIdentityConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "OK",
			LastTransitionTime: metav1.Now(),
		}},
	}
	_ = c.Status().Update(ctx, ai)
}

// validIdentityCondition returns a Valid=True condition slice for inline
// AgentIdentity status in fake-client tests (which honor
// WithStatusSubresource for objects created via WithObjects).
func validIdentityCondition() []metav1.Condition {
	return []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentIdentityConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "OK",
		LastTransitionTime: metav1.Now(),
	}}
}

func conditionReason(cs []metav1.Condition, t, reason string) bool {
	for _, c := range cs {
		if c.Type == t && c.Status == metav1.ConditionFalse && c.Reason == reason {
			return true
		}
	}
	return false
}

func TestInvalidWhenBundleToolspecMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-tm", Namespace: "default"},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-tm"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	ac := newClass("ac-tm")
	ac.Spec.AgentIdentity = "id-tm"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-tm", Toolspecs: []string{"missing-toolspec"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-tm"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-tm"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonToolspecMissing),
		"want Valid=False/ToolspecMissing; got conditions=%+v", got.Status.Conditions)
}

func TestInvalidWhenBundleAgentIdentityMissing(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-aim"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	tsAim := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-readonly-aim"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-readonly-aim", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	_ = env.Client.Create(ctx, tsAim)
	stampToolspecValid(ctx, env.Client, tsAim)
	ac := newClass("ac-aim")
	// Note: AgentIdentity not set on either AgentClass or per-bundle.
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-aim", Toolspecs: []string{"git-readonly-aim"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-aim"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-aim"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonAgentIdentityMissing),
		"want Valid=False/AgentIdentityMissing; got conditions=%+v", got.Status.Conditions)
}

func TestValidUnderUserPassthroughWithoutAgentIdentity(t *testing.T) {
	// Slice 3 follow-up: under identityMode=userPassthrough, an AgentClass
	// with toolBundles must NOT be rejected for missing AgentIdentity.
	// Credentials are sourced per-session from the invoking user's
	// UserIdentity; the operator's passthrough gate does the per-session
	// credential check at parkAwaitingCredentials time.
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-passthrough"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	tsPass := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-readonly-passthrough"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-readonly-passthrough", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	_ = env.Client.Create(ctx, tsPass)
	stampToolspecValid(ctx, env.Client, tsPass)

	ac := newClass("ac-passthrough")
	// No AgentIdentity — and identityMode=userPassthrough.
	ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-passthrough", Toolspecs: []string{"git-readonly-passthrough"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-passthrough"}})

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-passthrough"}, &got),
		"Get AgentClass")
	assert.False(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonAgentIdentityMissing),
		"userPassthrough must NOT yield AgentIdentityMissing; conditions=%+v", got.Status.Conditions)
	assert.False(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonAgentIdentityBindingMissing),
		"userPassthrough must NOT yield AgentIdentityBindingMissing; conditions=%+v", got.Status.Conditions)
}

func TestInvalidWhenBundleToolNameCollision(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-coll", Namespace: "default"},
	})
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-coll"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	// Two toolspecs that both target the "git" class tool.
	tsRead := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-readonly-c"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-readonly-c", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
	}
	_ = env.Client.Create(ctx, tsRead)
	stampToolspecValid(ctx, env.Client, tsRead)
	tsWrite := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-write-c"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "git-write-c", Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"push"},
		},
	}
	_ = env.Client.Create(ctx, tsWrite)
	stampToolspecValid(ctx, env.Client, tsWrite)
	ac := newClass("ac-coll")
	ac.Spec.AgentIdentity = "id-coll"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "tb-coll", Toolspecs: []string{"git-readonly-c", "git-write-c"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-coll"}})
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-coll"}, &got),
		"Get AgentClass")
	assert.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonBundleToolNameCollision),
		"want Valid=False/BundleToolNameCollision; got conditions=%+v", got.Status.Conditions)
}

func TestAgentClass_SessionInteractPermissionValidation(t *testing.T) {
	cases := []struct {
		val   string
		valid bool
	}{
		{"", true},
		{"group:engineering#member", true},
		{"team:authzed/spicedb#member", true},
		{"org:my-org/team#admin", true},
		{"user:abc", false},         // missing #relation
		{"#member", false},          // missing type:id
		{"group:eng#", false},       // empty relation
		{"group:eng", false},        // no # at all
		{"Group:eng#member", false}, // uppercase type not allowed
		{":eng#member", false},      // empty type
	}

	for _, tc := range cases {
		tc := tc
		t.Run("val="+tc.val, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

			_ = env.Client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk")},
			})

			// Build a k8s-valid metadata.name from tc.val by lowercasing
			// and replacing every non-[a-z0-9-] rune with '-'. Earlier
			// versions did `ReplaceAll(":","-")` and `ReplaceAll("#","_")`
			// in two halves, which left one bad character unreplaced
			// per half (and `_` itself isn't a valid DNS-1123 char).
			var nb strings.Builder
			nb.WriteString("ac-sip")
			if tc.val != "" {
				nb.WriteByte('-')
				for _, r := range strings.ToLower(tc.val) {
					switch {
					case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
						nb.WriteRune(r)
					default:
						nb.WriteByte('-')
					}
				}
			}
			acName := strings.Trim(nb.String(), "-")
			if len(acName) > 63 {
				acName = strings.Trim(acName[:63], "-")
			}
			if acName == "" {
				acName = "ac-sip-empty"
			}

			ac := newClass(acName)
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: tc.val}}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: acName}})
			var got spiceboxv1alpha1.AgentClass
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: acName}, &got),
				"Get AgentClass")

			if tc.valid {
				assert.True(t,
					hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
					"val=%q: want Valid=True; got %+v", tc.val, got.Status.Conditions)
			} else {
				assert.True(t,
					conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonSpecInvalid),
					"val=%q: want Valid=False/SpecInvalid; got %+v", tc.val, got.Status.Conditions)
			}
		})
	}
}

// TestAgentClass_OwnerCeilingValidation covers the mutually-exclusive and
// subject-ref-format constraints on spec.authz.ownerCeiling.
func TestAgentClass_OwnerCeilingValidation(t *testing.T) {
	cases := []struct {
		name    string
		ceiling *spiceboxv1alpha1.OwnerCeiling
		valid   bool
	}{
		{
			name:    "nil ceiling: valid",
			ceiling: nil,
			valid:   true,
		},
		{
			name:    "starterOnly only: valid",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
			valid:   true,
		},
		{
			name:    "fixed valid subject ref: valid",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "user:alice"},
			valid:   true,
		},
		{
			name:    "fixed with relation: valid",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:engineering#member"},
			valid:   true,
		},
		{
			name:    "starterOnly and fixed both set: invalid (mutually exclusive)",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true, Fixed: "user:x"},
			valid:   false,
		},
		{
			name:    "fixed missing colon: invalid subject ref format",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "useronly"},
			valid:   false,
		},
		{
			name:    "fixed empty id: invalid subject ref format",
			ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "user:"},
			valid:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

			_ = env.Client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk")},
			})

			// Build a DNS-1123-safe name from the test case name.
			var nb strings.Builder
			nb.WriteString("ac-oc")
			for _, r := range strings.ToLower(tc.name) {
				switch {
				case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
					nb.WriteRune(r)
				default:
					nb.WriteByte('-')
				}
			}
			acName := strings.Trim(nb.String(), "-")
			if len(acName) > 63 {
				acName = strings.Trim(acName[:63], "-")
			}

			ac := newClass(acName)
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{OwnerCeiling: tc.ceiling}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: acName}})
			var got spiceboxv1alpha1.AgentClass
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: acName}, &got),
				"Get AgentClass")

			if tc.valid {
				assert.True(t,
					hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
					"%s: want Valid=True; got %+v", tc.name, got.Status.Conditions)
			} else {
				assert.True(t,
					conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonSpecInvalid),
					"%s: want Valid=False/SpecInvalid; got %+v", tc.name, got.Status.Conditions)
			}
		})
	}
}

// --- Per-tool permission validation tests ---

// createValidMCPServer creates an MCPServer CR and stamps its Valid condition
// True. Returns the created MCPServer.
func createValidMCPServer(t *testing.T, ctx context.Context, c client.Client, name string, tools []spiceboxv1alpha1.MCPServerTool) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	// Every resource type these tools check is declared session-only. Standing
	// has no default, so a class whose slot names an undeclared type is refused
	// before the property under test is ever reached; session-only is truthful
	// for these fixtures, which seed no tuples for anyone to hold.
	seen := map[string]bool{}
	var resources []spiceboxv1alpha1.SpiceDBResource
	for _, tl := range tools {
		if tl.Permission == nil || tl.Permission.Check == nil {
			continue
		}
		rt := tl.Permission.Check.ResourceType
		if rt == "" || seen[rt] {
			continue
		}
		seen[rt] = true
		resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
			Name: rt, Standing: spiceboxv1alpha1.StandingSessionOnly,
		})
	}
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
			Tools:  tools,
		},
	}
	if len(resources) > 0 {
		srv.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources}
	}
	require.NoError(t, c.Create(ctx, srv), "create MCPServer %s", name)
	srv.Status = spiceboxv1alpha1.MCPServerStatus{
		Conditions: []metav1.Condition{
			{
				Type:               spiceboxv1alpha1.MCPServerConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "OK",
				LastTransitionTime: metav1.Now(),
			},
		},
	}
	require.NoError(t, c.Status().Update(ctx, srv), "stamp MCPServer Valid=True")
	return srv
}

// createLLMSecret creates the llm-creds Secret that every AgentClass requires.
func createLLMSecret(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	require.NoError(t, c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds secret")
}

// assertCondition reconciles ac by name and asserts a Valid condition with
// the expected status and reason.
func assertCondition(t *testing.T, ctx context.Context, r *agentclass.Reconciler, c client.Client, acName string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: acName}})
	require.NoError(t, err, "Reconcile %s", acName)

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: acName}, &got),
		"Get AgentClass %s after reconcile", acName)
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition should be set")
	assert.Equal(t, wantStatus, cond.Status, "Valid condition status")
	assert.Equal(t, wantReason, cond.Reason, "Valid condition reason; msg=%q", cond.Message)
}

// TestAgentClass_PerToolPermissionValidation collapses the original six
// per-tool permission validation tests (slice-1 / slice-2 rules: missing
// permission, missing check, mismatched stateImpact, unknown transform)
// into a single table-driven sweep. Each case configures an MCPServer
// with one tool whose Permission shape varies, then asserts the Valid
// condition surfaced on the bound AgentClass.
func TestAgentClass_PerToolPermissionValidation(t *testing.T) {
	cases := []struct {
		name       string
		permission *authz.Permission // nil → tool with no Permission field
		acSuffix   string
		mcpSuffix  string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "tool with no Permission block → Valid=False/ToolPermissionMissing (Rule 1)",
			permission: nil,
			acSuffix:   "no-perm",
			mcpSuffix:  "no-perm",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonToolPermissionMissing,
		},
		{
			name: "stateImpact=external + check → Valid=True (slice-2 supported variant)",
			permission: &authz.Permission{
				StateImpact: authz.External,
				Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "write"},
			},
			acSuffix:   "external-ok",
			mcpSuffix:  "external-ok",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
		{
			name: "stateImpact=external without check → Valid=False/PermissionSpecInvalid",
			permission: &authz.Permission{
				StateImpact: authz.External, // Check intentionally nil
			},
			acSuffix:   "external-no-check",
			mcpSuffix:  "external-no-check",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonPermissionSpecInvalid,
		},
		{
			name: "stateImpact=readonly without check → Valid=False/PermissionSpecInvalid (Rule 3)",
			permission: &authz.Permission{
				StateImpact: authz.Readonly, // Check intentionally nil
			},
			acSuffix:   "ro-no-check",
			mcpSuffix:  "ro-no-check",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonPermissionSpecInvalid,
		},
		{
			name: "stateImpact=stateless WITH check → Valid=False/PermissionSpecInvalid (Rule 3 reverse)",
			permission: &authz.Permission{
				StateImpact: authz.Stateless,
				Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"}, // must not be set
			},
			acSuffix:   "stateless-check",
			mcpSuffix:  "stateless-check",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonPermissionSpecInvalid,
		},
		{
			name: "unknown ResourceIDTransform name → Valid=False/PermissionTransformUnknown (Rule 5)",
			permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:         "linear_team",
					ResourceIDTemplate:   "{teamId}",
					ResourceIDTransforms: []string{"bogus"},
					Permission:           "read",
				},
			},
			acSuffix:   "bad-transform",
			mcpSuffix:  "bad-transform",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonPermissionTransformUnknown,
		},
		{
			// A value slot mints its object id from whatever the agent supplies,
			// so the transform chain decides which values ARE the same resource.
			// basename makes /etc/passwd and /home/u/passwd one object, and an
			// approval for one would authorize the other.
			name: "resourceIDExpr with a non-injective transform → Valid=False/PermissionSpecInvalid",
			permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:         "path",
					ResourceIDExpr:       "args.path",
					ResourceIDTransforms: []string{"basename"},
					Permission:           "write",
				},
			},
			acSuffix:   "expr-noninjective",
			mcpSuffix:  "expr-noninjective",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonPermissionSpecInvalid,
		},
		{
			// The shape the design actually calls for: canonicalize, then hash.
			name: "resourceIDExpr with normalize_url+sha256 → Valid=True",
			permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:         "http_target",
					ResourceIDExpr:       "args.url",
					ResourceIDTransforms: []string{"normalize_url", "sha256"},
					Permission:           "reachable",
				},
			},
			acSuffix:   "expr-injective",
			mcpSuffix:  "expr-injective",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
		{
			// The template branch is untouched: there the id comes from a named
			// argument that already is a distinct resource, and tidying it is
			// the normal use of these transforms.
			name: "resourceIDTemplate with a non-injective transform stays legal",
			permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:         "linear_team",
					ResourceIDTemplate:   "{teamId}",
					ResourceIDTransforms: []string{"lowercase"},
					Permission:           "read",
				},
			},
			acSuffix:   "tmpl-noninjective",
			mcpSuffix:  "tmpl-noninjective",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

			createLLMSecret(t, ctx, env.Client)
			tool := spiceboxv1alpha1.MCPServerTool{Name: "tool"}
			if tc.permission != nil {
				tool.Permission = tc.permission
			}
			mcpName := "mcp-" + tc.mcpSuffix
			createValidMCPServer(t, ctx, env.Client, mcpName, []spiceboxv1alpha1.MCPServerTool{tool})

			acName := "ac-" + tc.acSuffix
			ac := newClass(acName)
			ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: mcpName}}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass %s", acName)

			assertCondition(t, ctx, r, env.Client, acName, tc.wantStatus, tc.wantReason)
		})
	}
}

// TestAgentClass_CreatesOwnedAgentSessionGrants verifies that once
// the AgentClass passes per-tool permission validation, the reconciler
// writes an owned "<className>-grants" AgentSessionGrants CR carrying
// the dedup'd (resourceType, permission) pairs extracted from every
// referenced MCPServer's Permission.Check. See slice 2 plan T13 /
// spec §3.8.
func TestAgentClass_CreatesOwnedAgentSessionGrants(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)
	createValidMCPServer(t, ctx, env.Client, "mcp-grants", []spiceboxv1alpha1.MCPServerTool{
		{
			Name: "read-repo",
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType: "github_repo",
					Permission:   "read",
				},
			},
		},
		{
			// Dedup check: a second tool with the same (rt, perm) must
			// not produce a duplicate pair in spec.
			Name: "read-repo-deep",
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType: "github_repo",
					Permission:   "read",
				},
			},
		},
		{
			Name: "admin-repo",
			Permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType: "github_repo",
					Permission:   "admin",
				},
			},
		},
	})

	ac := newClass("test-class")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "gh", Ref: "mcp-grants"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-class"}})
	require.NoError(t, err, "Reconcile")

	// AgentClass should be Valid=True (per-tool permission validation
	// passes; SpiceDBSchema is nil so the live-schema branch is skipped).
	var gotClass spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-class"}, &gotClass),
		"Get AgentClass")
	assert.True(t,
		hasCondition(gotClass.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"AgentClass should be Valid=True; conditions=%+v", gotClass.Status.Conditions)
	assert.True(t,
		hasCondition(gotClass.Status.Conditions, spiceboxv1alpha1.AgentClassConditionAgentSessionGrantsWritten, metav1.ConditionTrue),
		"AgentSessionGrantsWritten should be True; conditions=%+v", gotClass.Status.Conditions)

	// Owned AgentSessionGrants exists with the expected spec.
	var asg spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "test-class-grants", Namespace: "default"}, &asg),
		"Get owned AgentSessionGrants")

	// Owner reference points back at the AgentClass.
	require.Len(t, asg.OwnerReferences, 1,
		"OwnerReferences: %+v", asg.OwnerReferences)
	or := asg.OwnerReferences[0]
	assert.Equal(t, "AgentClass", or.Kind, "OwnerReference.Kind")
	assert.Equal(t, "test-class", or.Name, "OwnerReference.Name")
	assert.Equal(t, gotClass.UID, or.UID, "OwnerReference.UID")
	require.NotNil(t, or.Controller, "OwnerReference.Controller should be set")
	assert.True(t, *or.Controller, "OwnerReference.Controller should be true")
	require.NotNil(t, or.BlockOwnerDeletion, "OwnerReference.BlockOwnerDeletion should be set")
	assert.True(t, *or.BlockOwnerDeletion, "OwnerReference.BlockOwnerDeletion should be true")

	// spec.pairs is dedup'd + sorted.
	want := []spiceboxv1alpha1.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"},
		{ResourceType: "github_repo", Permission: "read"},
	}
	assert.Equal(t, want, asg.Spec.Pairs, "spec.pairs should be dedup'd and sorted")
}

// TestValidation_DisabledStatusCondition verifies that an AgentClass with
// spec.toolAuthMode="disabled" gains a True ToolAuthDisabled condition
// with the ToolAuthBypassed reason, and that switching to "enforcing"
// flips the condition to False.
func TestValidation_DisabledStatusCondition(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)
	ac := newClass("ac-disabled")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "disabled"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-disabled"}})
	require.NoError(t, err, "Reconcile (disabled mode)")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-disabled"}, &got),
		"Get AgentClass")
	require.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionToolAuthDisabled, metav1.ConditionTrue),
		"want ToolAuthDisabled=True; got %+v", got.Status.Conditions)

	// Reason + message must match the operator-facing constants.
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionToolAuthDisabled)
	require.NotNil(t, cond, "ToolAuthDisabled condition")
	assert.Equal(t, spiceboxv1alpha1.ReasonToolAuthBypassed, cond.Reason, "ToolAuthDisabled reason")
	assert.Contains(t, cond.Message, "disabled", "message should mention 'disabled'")
	assert.Contains(t, cond.Message, "SpiceDB", "message should mention 'SpiceDB'")

	// Flip to enforcing; the condition should turn False with reason
	// ToolAuthEnforced.
	got.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "enforcing"}}
	require.NoError(t, env.Client.Update(ctx, &got), "update AgentClass to enforcing")
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-disabled"}})
	require.NoError(t, err, "Reconcile (enforcing mode)")
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-disabled"}, &got),
		"Get AgentClass after switch to enforcing")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionToolAuthDisabled, metav1.ConditionFalse),
		"want ToolAuthDisabled=False after flipping to enforcing; got %+v", got.Status.Conditions)
}

// TestValidation_PermissiveAllowsToolkitWithoutPerSubcommandPerms is the
// integration-level counterpart to TestValidation_PermissiveSkipsToolkitWalk:
// a permissive AgentClass referencing a toolkit whose destructive
// subcommand lacks a per-subcommand Permission MUST validate to True.
func TestValidation_PermissiveAllowsToolkitWithoutPerSubcommandPerms(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)

	// AgentIdentity with no credentials needed — the toolkit (SpiceboxToolkit "git")
	// declares no sensitive env vars, so the credential-coverage check passes.
	// Stamp Valid=True so the class's identity-validity propagation passes too.
	aiPermissive := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "code-id", Namespace: "default"},
	}
	_ = env.Client.Create(ctx, aiPermissive)
	stampIdentityValid(ctx, env.Client, aiPermissive)
	_ = env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "toolbelt-permissive"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	})
	// SpiceboxToolkit with a destructive subcommand and NO per-subcommand
	// permission. Under permissive this is fine; under enforcing this
	// would be rejected. All effects sub-fields use non-nil slices to
	// satisfy the CRD's required constraints.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "git"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "git",
			ToolkitRevision: "X",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/git"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "builtin", Name: "git"},
			Env:             spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{Path: []string{"rm"}, Effects: spiceboxv1alpha1.ToolkitEffects{
					Destructive: true,
					Reads:       []string{},
					Writes:      []string{},
					Filesystem:  spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
					Network:     spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
					Creds:       spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
				}},
			},
		},
	}), "create SpiceboxToolkit")
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git-permissive"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "git-permissive",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"rm"},
		},
	}
	_ = env.Client.Create(ctx, ts)
	stampToolspecValid(ctx, env.Client, ts)

	ac := newClass("ac-permissive")
	ac.Spec.AgentIdentity = "code-id"
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "permissive"}}
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "toolbelt-permissive", Toolspecs: []string{"git-permissive"}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-permissive"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-permissive"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"permissive: want Valid=True; got %+v", got.Status.Conditions)
}

// TestWriteAgentSessionGrants_NoMCPServers verifies the slice-2 task-13
// path is well-defined when an AgentClass declares no MCPServers (the
// common "channel-only" or toolBundle-only configuration). The
// reconciler must:
//   - reach Valid=True without panicking
//   - write an empty-pairs AgentSessionGrants CR named "<class>-grants"
//   - flip AgentSessionGrantsWritten=True on the AgentClass
//
// This catches the silent-crash regression motivating the slice-2
// investigation: a panic inside writeAgentSessionGrants when the
// resolved-server list was empty would be swallowed by
// controller-runtime's per-reconcile recovery and leave the operator
// looping silently.
func TestWriteAgentSessionGrants_NoMCPServers(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")

	ac := newClass("ac-no-tools")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass with no MCPServers")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-no-tools"}})
	require.NoError(t, err, "Reconcile (no MCPServers — silent-crash regression check)")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-no-tools"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got %+v", got.Status.Conditions)
	// The owned AgentSessionGrants CR should exist with zero pairs.
	var asg spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-no-tools-grants"}, &asg),
		"AgentSessionGrants/ac-no-tools-grants should exist (empty-pairs)")
	assert.Empty(t, asg.Spec.Pairs, "expected empty pairs when no MCPServers")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionAgentSessionGrantsWritten, metav1.ConditionTrue),
		"expected AgentSessionGrantsWritten=True; got %+v", got.Status.Conditions)
}

// bentoStubKind is a minimal channelkinds.Kind impl registered under the
// name "bento" so the AgentClass validator's UserAttributable check has a
// non-user-attributable kind to test against. The real bento package is
// added in a later task; we register the stub here so this test compiles
// and runs in isolation.
type bentoStubKind struct{}

func (bentoStubKind) Name() string                                                        { return "bento" }
func (bentoStubKind) DefaultSessionScope() string                                         { return "singleton" }
func (bentoStubKind) Capabilities() []string                                              { return []string{"text"} }
func (bentoStubKind) NewListener(channelkinds.Deps) channelkinds.Listener                 { return nil }
func (bentoStubKind) NewSender(channelkinds.Deps) channelkinds.Sender                     { return nil }
func (bentoStubKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender      { return nil }
func (bentoStubKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink   { return nil }
func (bentoStubKind) SupportsMonitoring() bool                                            { return false }
func (bentoStubKind) SupportsLiveViewOffer() bool                                         { return false }
func (bentoStubKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender { return nil }
func (bentoStubKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (bentoStubKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (bentoStubKind) SupportedRoles() []string                                       { return spiceboxv1alpha1.AllChannelRoles() }
func (bentoStubKind) ValidateSpec(*spiceboxv1alpha1.Channel) error                   { return nil }
func (bentoStubKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string            { return nil }
func (bentoStubKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string          { return nil }
func (bentoStubKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (bentoStubKind) RenderMention(externalID string) string                    { return externalID }
func (bentoStubKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (bentoStubKind) LookupUser(
	context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (bentoStubKind) MentionToolDescription() string { return "" }
func (bentoStubKind) UserAttributable() bool         { return false }
func (bentoStubKind) DeliversToHuman() bool          { return false }
func (bentoStubKind) AllowsSyntheticIdentity() bool  { return false }
func (bentoStubKind) RelayedByChannelsd() bool       { return true }
func (bentoStubKind) SpawnsSessionOnInbound() bool   { return true }
func (bentoStubKind) Wizard() channelkinds.Wizard    { return nil }

func init() {
	// Idempotent: the channelkinds registry panics on duplicate
	// registration, so only register if absent (another test in the
	// same binary may have registered already).
	if _, ok := chregistry.Get("bento"); !ok {
		chregistry.Register(bentoStubKind{})
	}
}

// TestAgentClass_UserLessChannelRequiresAuthz exercises the validator
// rule: when an AgentClass binds an input-role Channel whose kind
// reports UserAttributable=false AND SpawnsSessionOnInbound=true (bento
// today), BOTH AgentClass.spec.sessionInteractPermission AND
// Channel.spec.authzSubject MUST be set.
//
// The two halves of that predicate are asserted against each other on purpose.
// The `agent` rows below are the kind the skip exists for — a conversational
// subagent's Channel, which the operator creates itself and binds to the
// child's class, and which would otherwise park every session of that class.
// The `bento` rows are what keep the skip honest: bento is the other
// UserAttributable=false kind and it DOES spawn sessions on inbound, so if the
// predicate were ever widened from "spawns nothing" to "not user-attributable"
// those rows fail rather than the rule silently going away cluster-wide.
func TestAgentClass_UserLessChannelRequiresAuthz(t *testing.T) {
	type scenario struct {
		name              string
		channelKind       string
		channelRole       string
		channelAuthzSubj  string
		sessionInteract   string
		wantValidTrue     bool
		wantReasonOnFalse string
	}
	scenarios := []scenario{
		{
			name:              "bento input + no SIP + no authzSubject is invalid",
			channelKind:       "bento",
			channelRole:       spiceboxv1alpha1.ChannelRoleInput,
			wantValidTrue:     false,
			wantReasonOnFalse: spiceboxv1alpha1.ReasonAgentClassUserLessMissingAuthz,
		},
		{
			name:             "bento input + SIP set + authzSubject set is valid",
			channelKind:      "bento",
			channelRole:      spiceboxv1alpha1.ChannelRoleInput,
			channelAuthzSubj: "service:foo-bot",
			sessionInteract:  "group:engineering#member",
			wantValidTrue:    true,
		},
		{
			// UserAttributable=true → rule is moot regardless of SIP/authzSubject.
			name:          "slack input without SIP/authzSubject is valid",
			channelKind:   "slack",
			channelRole:   spiceboxv1alpha1.ChannelRoleInput,
			wantValidTrue: true,
		},
		{
			// role=output skips the rule even when UserAttributable=false.
			name:          "bento output-only channel is valid without SIP/authzSubject",
			channelKind:   "bento",
			channelRole:   spiceboxv1alpha1.ChannelRoleOutput,
			wantValidTrue: true,
		},
		{
			// role=both is treated the same as input: the channel can receive
			// user messages, so the attribution rules still apply.
			name:              "bento role=both + missing both fields is invalid",
			channelKind:       "bento",
			channelRole:       spiceboxv1alpha1.ChannelRoleBoth,
			wantValidTrue:     false,
			wantReasonOnFalse: spiceboxv1alpha1.ReasonAgentClassUserLessMissingAuthz,
		},
		{
			// The conversational-subagent shape, verbatim: role=both, no
			// interactPermission on the class, no authzSubject on the Channel.
			// agent is UserAttributable=false like bento, but nothing is ever
			// born on it — the SubagentRequest controller pre-creates the child
			// with its own attribution — so the rule has nothing to protect and
			// must not fire. If it did, the CHILD'S OWN CLASS would go
			// Valid=False and every session of that class, delegated or not,
			// would park.
			name:          "agent role=both + missing both fields is valid: nothing is born on this kind",
			channelKind:   "agent",
			channelRole:   spiceboxv1alpha1.ChannelRoleBoth,
			wantValidTrue: true,
		},
		{
			// Same for role=input, so the skip is not an accident of the
			// role=both arm.
			name:          "agent input + missing both fields is valid: nothing is born on this kind",
			channelKind:   "agent",
			channelRole:   spiceboxv1alpha1.ChannelRoleInput,
			wantValidTrue: true,
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

			require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk")},
			}), "create llm-creds")

			acName := "ac-userless-" + strings.ReplaceAll(strings.ReplaceAll(sc.name, " ", "-"), "+", "")
			// Trim to k8s-valid metadata.name characters.
			var nb strings.Builder
			for _, r := range strings.ToLower(acName) {
				switch {
				case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
					nb.WriteRune(r)
				default:
					nb.WriteByte('-')
				}
			}
			acName = strings.Trim(nb.String(), "-")
			if len(acName) > 63 {
				acName = strings.Trim(acName[:63], "-")
			}
			chName := acName + "-ch"
			if len(chName) > 63 {
				chName = strings.Trim(chName[:63], "-")
			}

			ac := newClass(acName)
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: sc.sessionInteract}}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass %s", acName)

			ch := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: chName, Namespace: "default"},
				Spec: spiceboxv1alpha1.ChannelSpec{
					Kind:           sc.channelKind,
					Role:           sc.channelRole,
					AuthzSubject:   sc.channelAuthzSubj,
					AgentClass:     acName,
					SessionScope:   "auto",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "llm-creds"},
				},
			}
			// Provide the kind-specific spec block so CRD validation
			// doesn't reject the create.
			switch sc.channelKind {
			case "bento":
				ch.Spec.Bento = &spiceboxv1alpha1.BentoChannelConfig{}
			case "slack":
				ch.Spec.Slack = &spiceboxv1alpha1.SlackChannelConfig{}
			}
			require.NoError(t, env.Client.Create(ctx, ch), "create Channel %s", chName)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: acName}})
			require.NoError(t, err, "Reconcile")

			var got spiceboxv1alpha1.AgentClass
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: acName}, &got),
				"Get AgentClass %s", acName)
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, cond, "Valid condition; conditions=%+v", got.Status.Conditions)
			if sc.wantValidTrue {
				assert.Equal(t, metav1.ConditionTrue, cond.Status,
					"want Valid=True; got reason=%s msg=%q", cond.Reason, cond.Message)
				return
			}
			require.Equal(t, metav1.ConditionFalse, cond.Status,
				"want Valid=False; got reason=%s", cond.Reason)
			assert.Equal(t, sc.wantReasonOnFalse, cond.Reason,
				"Valid reason; msg=%q", cond.Message)
			assert.Contains(t, cond.Message, chName,
				"message should name the Channel %q", chName)
		})
	}
}

// TestReconcile_SchemaConflictAcrossMCPServers_Invalid covers slice-4 T19:
// when two MCPServers bound to the same AgentClass declare the SAME SpiceDB
// resource name with non-identical definitions, the AgentClass controller
// must surface Valid=False with reason=SpicedbSchemaConflict — the
// guardian/schema composer cannot merge them into a single coherent schema.
//
// Uses fake.NewClientBuilder rather than envtest because the latter is
// unreliable on developer machines and this case is a pure unit check on
// the reconcile decision path.
func TestReconcile_SchemaConflictAcrossMCPServers_Invalid(t *testing.T) {
	ctx := context.Background()

	scheme := testfixtures.NewScheme(t)

	// Two MCPServers both declaring resource "crm_company" but with a
	// DIFFERENT contact_access permission expression — the composer
	// rejects this as a conflict.
	mkServer := func(name, contactAccessExpr string) *spiceboxv1alpha1.MCPServer {
		srv := &spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x"},
				Tools:  []spiceboxv1alpha1.MCPServerTool{},
				SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
					Resources: []spiceboxv1alpha1.SpiceDBResource{{
						Standing: spiceboxv1alpha1.StandingSessionOnly,
						Name:     "crm_company",
						Permissions: []spiceboxv1alpha1.SpiceDBPermission{{
							Name: "contact_access",
							Expr: contactAccessExpr,
						}},
					}},
				},
			},
			Status: spiceboxv1alpha1.MCPServerStatus{
				Conditions: []metav1.Condition{{
					Type:               spiceboxv1alpha1.MCPServerConditionValid,
					Status:             metav1.ConditionTrue,
					Reason:             "OK",
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
		return srv
	}
	srvA := mkServer("mcp-a", "a")
	srvB := mkServer("mcp-b", "b")

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}

	// AgentIdentity exists and is Valid=True; neither MCPServer declares
	// Auth.Provider or Auth.Credential so no credential check fires — we want
	// validatePermissions to fall through so the schema-conflict check fires.
	// The Valid=True stamp keeps the new identity-validity propagation from
	// short-circuiting validateMCPServers ahead of the conflict check.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-conflict", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentIdentityStatus{
			Conditions: validIdentityCondition(),
		},
	}

	ac := newClass("ac-schema-conflict")
	ac.Spec.AgentIdentity = "ai-conflict"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "mcp-a", Ref: "mcp-a"},
		{Name: "mcp-b", Ref: "mcp-b"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sec, ai, srvA, srvB, ac).
		WithStatusSubresource(
			&spiceboxv1alpha1.AgentClass{},
			&spiceboxv1alpha1.MCPServer{},
		).
		Build()

	r := &agentclass.Reconciler{Client: c, APIReader: c}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-schema-conflict"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-schema-conflict"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassSpicedbSchemaConflict, cond.Reason,
		"Valid reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "crm_company", "message should name conflicting resource")
	assert.Contains(t, cond.Message, "conflicting", "message should describe issue as conflicting")
}

// TestAgentClass_TestProvider_RejectedByDefault verifies the production
// safety gate: an AgentClass with model.provider="test" must be rejected
// with Valid=False/TestProviderNotAllowed when the reconciler's
// AllowTestProvider gate is false (the production binary default). The
// e2e harness flips the gate to true; production never does.
func TestAgentClass_TestProvider_RejectedByDefault(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "centerdot", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test",
				Name:     "scripted",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "test"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 1, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("unused")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		WithObjects(ac, secret).Build()
	r := &agentclass.Reconciler{Client: c, AllowTestProvider: false}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "centerdot", Namespace: "default"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "centerdot", Namespace: "default"}, &got))
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonTestProviderNotAllowed, cond.Reason)
}

// TestAgentClass_TestProvider_AllowedWhenFlagSet mirrors the
// rejection test but with AllowTestProvider=true (what the e2e harness
// sets). With every other validation passing, the class reaches
// Valid=True — confirming the gate is the only thing blocking it in
// the rejection case above.
func TestAgentClass_TestProvider_AllowedWhenFlagSet(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "centerdot", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test",
				Name:     "scripted",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "test"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 1, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("unused")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		WithObjects(ac, secret).Build()
	r := &agentclass.Reconciler{Client: c, AllowTestProvider: true}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "centerdot", Namespace: "default"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "centerdot", Namespace: "default"}, &got))
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond)
	// With the flag, validation reaches Valid=True (no other config errors).
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "reason=%s msg=%s", cond.Reason, cond.Message)
}

// TestAgentClass_SettingsAccepted_ModelForbidden verifies that when a
// ClusterAgentSettings catalog denies the class's model, the reconciler
// stamps SettingsAccepted=False with reason ModelForbidden and records the
// class model name in status.effectiveSettings.model.name.
func TestAgentClass_SettingsAccepted_ModelForbidden(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-test")},
	}), "create llm-creds")

	// ClusterAgentSettings with a catalog that denies claude-haiku-4-5 (the class model).
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &spiceboxv1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
			}},
			Limits: &spiceboxv1alpha1.SettingsLimits{DeniedModels: []string{"claude-haiku-4-5"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-settings-model", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-haiku-4-5",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:  10,
				MaxTokens: 50000,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-settings-model"}})
	require.NoError(t, err, "Reconcile must not return an error")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-settings-model"}, &got),
		"Get AgentClass after reconcile")

	// SettingsAccepted must be False/ModelForbidden.
	settingsCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present")
	assert.Equal(t, metav1.ConditionFalse, settingsCond.Status, "SettingsAccepted should be False")
	assert.Equal(t, "ModelForbidden", settingsCond.Reason, "SettingsAccepted reason should be ModelForbidden")

	// effectiveSettings must be stamped with the class model name.
	require.NotNil(t, got.Status.EffectiveSettings, "effectiveSettings must be stamped")
	assert.Equal(t, "claude-haiku-4-5", got.Status.EffectiveSettings.Model.Name,
		"effectiveSettings.model.name should reflect the class's model")

	// Valid should still be True — settings violation does NOT block validity.
	validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, validCond, "Valid condition must be present")
	assert.Equal(t, metav1.ConditionTrue, validCond.Status, "Valid should remain True despite settings violation")
}

// TestAgentClass_AllowedCatalogModel_ReconcilesValid is the regression test for
// the empty-apiKey reconcile path: a model resolved from the cluster catalog
// carries no tenant apiKey (its central token lives in
// status.effectiveSettings.modelTokenSource), so the reconciler must NOT try to
// adopt an empty-named Secret. Before the APIKey.Name guard this errored the
// reconcile with "resource name may not be empty".
func TestAgentClass_AllowedCatalogModel_ReconcilesValid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	// Shared envtest: clear any leftover singleton first, and clean up on exit.
	_ = env.Client.Delete(ctx, &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName}})

	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &spiceboxv1alpha1.NamespacedSecretKeyRef{
					Namespace: "agentprimitives-system", Name: "central-llm", Key: "token"},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, cas) })

	// AgentClass OMITS spec.model → inherits the catalog default, which has no
	// tenant apiKey (its token is central, in modelTokenSource).
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-catalog-inherit", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget:       &spiceboxv1alpha1.BudgetConfig{MaxTurns: 10, MaxTokens: 50000},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-catalog-inherit"}})
	require.NoError(t, err, "Reconcile must not error on a catalog model with no tenant apiKey")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-catalog-inherit"}, &got),
		"Get AgentClass after reconcile")

	// Allowed catalog model → SettingsAccepted True.
	settingsCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present")
	assert.Equal(t, metav1.ConditionTrue, settingsCond.Status,
		"SettingsAccepted should be True (reason=%s msg=%s)", settingsCond.Reason, settingsCond.Message)

	// The resolved model is the catalog default, with its central token source
	// recorded and NO tenant apiKey.
	require.NotNil(t, got.Status.EffectiveSettings, "effectiveSettings must be stamped")
	assert.Equal(t, "claude-opus-4-8", got.Status.EffectiveSettings.Model.Name)
	assert.Empty(t, got.Status.EffectiveSettings.Model.APIKey.Name, "catalog model carries no tenant apiKey")
	require.NotNil(t, got.Status.EffectiveSettings.ModelTokenSource, "catalog model records its token source")
	assert.Equal(t, "central-llm", got.Status.EffectiveSettings.ModelTokenSource.Name)

	// Valid True — the catalog token's reachability is the operator's concern at
	// pod create, not this class-level validation.
	validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, validCond, "Valid condition must be present")
	assert.Equal(t, metav1.ConditionTrue, validCond.Status, "reason=%s msg=%s", validCond.Reason, validCond.Message)
}

// TestValidWhenSecretExists_WithGuardedReader exercises the adopt+guarded-read
// path for the model API key Secret. The Secret is pre-stamped with the
// adoptguard label before creation so the label-filtered SecretReader can
// return it; a reconciler wired with SecretReader and ConfigMapReader must
// reach Valid=True.
func TestValidWhenSecretExists_WithGuardedReader(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	secretReader := adoptguard.NewSecretReader(
		env.Client, env.Client,
		adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)
	configMapReader := adoptguard.NewConfigMapReader(
		env.Client, env.Client,
		adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)
	r := &agentclass.Reconciler{
		Client:          env.Client,
		APIReader:       env.Client,
		SecretReader:    secretReader,
		ConfigMapReader: configMapReader,
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds-guarded", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	adoptguard.WithAdoptedLabel(sec)
	require.NoError(t, env.Client.Create(ctx, sec), "create llm-creds-guarded")

	ac := newClass("ac-guarded-secret")
	ac.Spec.Model.APIKey.Name = "llm-creds-guarded"
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-guarded-secret"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-guarded-secret"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True with guarded reader; got conditions=%+v", got.Status.Conditions)
}

// TestValidWhenConfigMapExists_WithGuardedReader exercises the adopt+guarded-read
// path for the systemPrompt ConfigMap. The ConfigMap is pre-stamped with the
// adoptguard label before creation so the label-filtered ConfigMapReader can
// return it; a reconciler wired with both readers must reach Valid=True.
func TestValidWhenConfigMapExists_WithGuardedReader(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	secretReader := adoptguard.NewSecretReader(
		env.Client, env.Client,
		adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)
	configMapReader := adoptguard.NewConfigMapReader(
		env.Client, env.Client,
		adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)
	r := &agentclass.Reconciler{
		Client:          env.Client,
		APIReader:       env.Client,
		SecretReader:    secretReader,
		ConfigMapReader: configMapReader,
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds-cm", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	adoptguard.WithAdoptedLabel(sec)
	require.NoError(t, env.Client.Create(ctx, sec), "create llm-creds-cm")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prompt-cm", Namespace: "default"},
		Data:       map[string]string{"prompt.txt": "you are an agent"},
	}
	adoptguard.WithAdoptedLabel(cm)
	require.NoError(t, env.Client.Create(ctx, cm), "create prompt-cm")

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-guarded-cm", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds-cm", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{
				ConfigMapRef: &spiceboxv1alpha1.ConfigMapKeyRef{
					Name: "prompt-cm",
					Key:  "prompt.txt",
				},
			},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-guarded-cm"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-guarded-cm"}, &got),
		"Get AgentClass")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True with guarded ConfigMap reader; got conditions=%+v", got.Status.Conditions)
}

// TestAgentClass_PublishesResolvedSlotValueKeying is the wiring half of the
// value→id seam. resolveSlotValueKeying has unit tests that pass whether or not
// anything calls it, and this feature has already shipped several validators
// that were logic-correct and unreachable — so this drives the real reconciler
// and reads the result off status.
//
// The seam exists because a slot grant names an object id, and for a value slot
// that id is minted from the value by the TOOL. Anything writing a grant ahead
// of the call has to reproduce the same chain; without this published, a seeder
// would guess, and a wrong guess is SILENT — the grant is written, never
// matched, and nothing errors.
func TestAgentClass_PublishesResolvedSlotValueKeying(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)
	createValidMCPServer(t, ctx, env.Client, "mcp-valuekey", []spiceboxv1alpha1.MCPServerTool{{
		Name: "curl",
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:         "http_target",
				ResourceIDExpr:       "args.url",
				ResourceIDTransforms: []string{"normalize_url", "sha256"},
				Permission:           "reachable",
			},
		},
	}})

	ac := newClass("ac-valuekey")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "net", Ref: "mcp-valuekey"}}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "http_target", Description: "a URL the agent may reach",
			Permission: "reachable", FillFrom: []string{"channel_thread"},
			AutoGrantFrom: []string{"owner"},
		}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-valuekey"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-valuekey"}, &got))
	require.Len(t, got.Status.ResolvedSlots, 1, "the declared slot must be published")
	assert.Equal(t, "http_target", got.Status.ResolvedSlots[0].ResourceType)
	assert.Equal(t, "reachable", got.Status.ResolvedSlots[0].Permission)
	assert.Equal(t, []string{"normalize_url", "sha256"}, got.Status.ResolvedSlots[0].ValueTransforms,
		"a grant writer must be able to mint the id the tool's own Check will compute")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, got.Status.ResolvedSlots[0].Standing,
		"no fragment declares standing for http_target and no admin veto names it -> default session-only")
}

// TestAgentClass_PublishesResolvedSlotValueKeying_NamespaceVetoForcesRequired
// is the wiring half of the standing seam, mirroring
// TestAgentClass_PublishesResolvedSlotValueKeying above: resolveStandingFor has
// unit tests that pass whether or not the reconciler ever calls it with a real
// veto set. Deleting `Standing:` from the ResolvedSlot literal in
// resolveSlotValueKeying, or passing nil instead of the resolved
// EffectiveSettings.RequireStandingFor at the controller.go call site, leaves
// every other test in this package green — this is the only test that drives
// the real reconciler with a namespace-tier veto and reads the result off
// status, so it is the only place that would catch either regression.
func TestAgentClass_PublishesResolvedSlotValueKeying_NamespaceVetoForcesRequired(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)
	createValidMCPServer(t, ctx, env.Client, "mcp-valuekey-veto", []spiceboxv1alpha1.MCPServerTool{{
		Name: "curl",
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:         "http_target",
				ResourceIDExpr:       "args.url",
				ResourceIDTransforms: []string{"normalize_url", "sha256"},
				Permission:           "reachable",
			},
		},
	}})

	// Shared envtest: clear any leftover singleton first, and clean up on exit
	// (same defensive pattern as the ClusterAgentSettings tests above).
	_ = env.Client.Delete(ctx, &spiceboxv1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: spiceboxv1alpha1.AgentSettingsName}})
	nsSettings := &spiceboxv1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: spiceboxv1alpha1.AgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{RequireStandingFor: []string{"http_target"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, nsSettings), "create namespace AgentSettings")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, nsSettings) })

	ac := newClass("ac-valuekey-veto")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "net", Ref: "mcp-valuekey-veto"}}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "http_target", Description: "a URL the agent may reach",
			Permission: "reachable", FillFrom: []string{"channel_thread"},
			AutoGrantFrom: []string{"owner"},
		}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-valuekey-veto"}})
	require.NoError(t, err, "Reconcile")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-valuekey-veto"}, &got))

	// The veto is UNSATISFIABLE here, and that is the assertion.
	//
	// http_target is declared session-only, so it names no approverPermission.
	// Forcing it to `required` would leave the approval router with no subject
	// set to resolve, dead-ending every approval for the type. Refused out loud
	// — with the type named — rather than written to status as a standing that
	// silently bricks it.
	//
	// This case used to assert the veto simply flipped the published standing to
	// required. That was safe only while `#owner` was hardcoded and every type
	// had an owner-set by assumption; with the permission declared, a type that
	// declares none cannot be governed at all.
	valid := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, valid, "the class must carry a Valid condition")
	assert.Equal(t, metav1.ConditionFalse, valid.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, valid.Reason)
	assert.Contains(t, valid.Message, "http_target", "the message must name the type an admin has to fix")
	assert.Contains(t, valid.Message, "no approverPermission")
}

// The divergence rule reaching the reconciler: two tools keying one type
// differently makes the same URL two resources, so no grant can serve both.
func TestAgentClass_DivergentValueKeyingBlocksTheClass(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	createLLMSecret(t, ctx, env.Client)
	mk := func(name string, transforms []string) spiceboxv1alpha1.MCPServerTool {
		return spiceboxv1alpha1.MCPServerTool{
			Name: name,
			Permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType: "http_target", ResourceIDExpr: "args.url",
					ResourceIDTransforms: transforms, Permission: "reachable",
				},
			},
		}
	}
	createValidMCPServer(t, ctx, env.Client, "mcp-divergent", []spiceboxv1alpha1.MCPServerTool{
		mk("curl", []string{"normalize_url", "sha256"}),
		mk("fetch", []string{"sha256"}),
	})

	ac := newClass("ac-divergent")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "net", Ref: "mcp-divergent"}}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "http_target", Description: "a URL", Permission: "reachable",
		}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	assertCondition(t, ctx, r, env.Client, "ac-divergent",
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonSlotDeclarationInvalid)
}
