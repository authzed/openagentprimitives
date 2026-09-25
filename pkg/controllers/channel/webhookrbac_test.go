package channel_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// tenantNS is a namespace no shipped webd Role covers — the discriminator
// this whole file is about. "default" would pass on role-default.yaml's grant
// and prove nothing.
const tenantNS = "tenant-a"

// secretGetAllowed evaluates a Role's rules the way the apiserver's RBAC
// authorizer does for one concrete request, ResourceNames included.
//
// Written as a decision rather than a shape comparison on purpose: asserting
// that some rule mentions "secrets" would pass for a rule missing the verb, or
// naming a different Secret, or naming none at all (which would grant EVERY
// Secret in the namespace — the widening this design exists to avoid).
func secretGetAllowed(rules []rbacv1.PolicyRule, verb, secretName string) bool {
	covers := func(vals []string, want string) bool {
		return slices.Contains(vals, want) || slices.Contains(vals, "*")
	}
	for _, r := range rules {
		if !covers(r.APIGroups, "") || !covers(r.Resources, "secrets") || !covers(r.Verbs, verb) {
			continue
		}
		// An empty ResourceNames means "every object of this resource".
		if len(r.ResourceNames) == 0 || slices.Contains(r.ResourceNames, secretName) {
			return true
		}
	}
	return false
}

func githubChannelIn(ns, name, secretName string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "ch-uid-1"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "github",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "agent-cls",
			AuthzSubject:   "service:demo-reviewbot-github",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			GitHub:         &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"},
		},
	}
}

// TestBuildWebhookSecretRBAC_AuthorizesExactlyTheOneSecretRead is the core
// claim: the stamped Role answers YES to the read channelwebhook actually
// performs (get on the Channel's credentialsRef Secret, in the Channel's own
// namespace) and NO to everything adjacent.
func TestBuildWebhookSecretRBAC_AuthorizesExactlyTheOneSecretRead(t *testing.T) {
	ch := githubChannelIn(tenantNS, "demo-reviewbot-gh", "gh-creds")
	role, rb := channel.BuildWebhookSecretRBAC(ch)

	require.Equal(t, tenantNS, role.Namespace,
		"the grant must land in the Channel's namespace — that is the whole point")
	assert.True(t, secretGetAllowed(role.Rules, "get", "gh-creds"),
		"webd must be able to get the Channel's credentials Secret, or every delivery 500s")

	assert.False(t, secretGetAllowed(role.Rules, "get", "some-other-secret"),
		"the grant must be pinned to this Channel's Secret, not to every Secret in the namespace")
	for _, verb := range []string{"list", "watch", "update", "patch", "create", "delete"} {
		assert.Falsef(t, secretGetAllowed(role.Rules, verb, "gh-creds"),
			"a browser-facing pod must hold read-only, get-only access; %q must not be granted", verb)
	}

	// The binding must actually reach webd's ServiceAccount. A subject that
	// names the wrong SA is not an error at the apiserver — it simply grants
	// nothing, and the symptom is the identical 500.
	require.Equal(t, tenantNS, rb.Namespace)
	require.Equal(t, role.Name, rb.RoleRef.Name)
	assert.Equal(t, "Role", rb.RoleRef.Kind)
	require.Len(t, rb.Subjects, 1)
	assert.Equal(t, rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      channel.WebdServiceAccountName,
		Namespace: channel.WebdServiceAccountNamespace,
	}, rb.Subjects[0])
}

// TestBuildWebhookSecretRBAC_OwnedByTheChannelAndPureInItsInputs pins the two
// lifecycle properties: the objects are garbage-collected with the Channel,
// and a re-apply is an SSA no-op (AGENTS.md's server-side-apply rule).
func TestBuildWebhookSecretRBAC_OwnedByTheChannelAndPureInItsInputs(t *testing.T) {
	ch := githubChannelIn(tenantNS, "demo-reviewbot-gh", "gh-creds")
	role, rb := channel.BuildWebhookSecretRBAC(ch)

	for _, obj := range []client.Object{role, rb} {
		refs := obj.GetOwnerReferences()
		require.Lenf(t, refs, 1, "%T must be owned by the Channel so it is GC'd with it", obj)
		assert.Equal(t, "Channel", refs[0].Kind)
		assert.Equal(t, ch.Name, refs[0].Name)
		assert.Equal(t, ch.UID, refs[0].UID,
			"an owner ref with no UID is silently ignored by the garbage collector")
		require.NotNil(t, refs[0].Controller)
		assert.True(t, *refs[0].Controller)
	}

	role2, rb2 := channel.BuildWebhookSecretRBAC(ch)
	assert.Equal(t, role, role2, "a volatile field here would make every re-apply rewrite the Role")
	assert.Equal(t, rb, rb2)
}

// TestReconcile_StampsTheWebhookSecretGrantOffTheRegistryPredicate drives the
// real reconcile and asserts the grant appears for a webhook-routable kind and
// does not for one that receives nothing over HTTP.
//
// Both rows run in a namespace no shipped webd Role covers, which is the
// condition under which the bug fired.
func TestReconcile_StampsTheWebhookSecretGrantOffTheRegistryPredicate(t *testing.T) {
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	adopted := func(name string, data map[string][]byte) *corev1.Secret {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: name},
			Data:       data,
		}
		adoptguard.WithAdoptedLabel(sec)
		return sec
	}
	ghCreds := adopted("gh-creds", map[string][]byte{
		"app-id":          []byte("12345"),
		"private-key":     []byte("-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"),
		"webhook-secret":  []byte("whsec_demo"),
		"installation-id": []byte("67890"),
	})
	slackCreds := adopted("slack-creds", map[string][]byte{
		"bot-token": []byte("xoxb-test"),
		"app-token": []byte("xapp-test"),
	})

	slackCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: "demo-reviewbot-slack", UID: "ch-uid-2"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass:     "agent-cls",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"},
			},
		},
	}

	cases := []struct {
		name      string
		channel   *spiceboxv1alpha1.Channel
		wantGrant bool
	}{
		{
			name:      "kind=github (webhook-routable): a Role + RoleBinding land in the Channel's namespace",
			channel:   githubChannelIn(tenantNS, "demo-reviewbot-gh", "gh-creds"),
			wantGrant: true,
		},
		{
			name:      "kind=slack (no webhook receiver): nothing is stamped",
			channel:   slackCh,
			wantGrant: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := newValidAgentClass("agent-cls", nil)
			ac.Namespace = tenantNS
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ac, tc.channel, ghCreds.DeepCopy(), slackCreds.DeepCopy()).
				WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
				Build()
			r := newUnitReconciler(c)
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: tenantNS, Name: tc.channel.Name},
			})
			require.NoError(t, err)

			key := client.ObjectKey{Namespace: tenantNS, Name: channel.WebhookSecretRoleName(tc.channel.Name)}
			var role rbacv1.Role
			var rb rbacv1.RoleBinding
			roleErr := c.Get(context.Background(), key, &role)
			rbErr := c.Get(context.Background(), key, &rb)

			if !tc.wantGrant {
				assert.Error(t, roleErr, "a kind with no webhook receiver must not be granted a Secret read")
				assert.Error(t, rbErr)
				return
			}
			require.NoError(t, roleErr, "without this Role every delivery to a Channel outside a webd-covered namespace 500s")
			require.NoError(t, rbErr, "a Role nothing binds grants nothing")
			assert.True(t, secretGetAllowed(role.Rules, "get", tc.channel.Spec.CredentialsRef.SecretName))
			require.Len(t, rb.Subjects, 1)
			assert.Equal(t, channel.WebdServiceAccountName, rb.Subjects[0].Name)
			assert.Equal(t, channel.WebdServiceAccountNamespace, rb.Subjects[0].Namespace)
		})
	}
}

// TestReconcile_WebhookSecretGrantIsIdempotent proves a second reconcile
// changes nothing: an SSA payload carrying a volatile value would rewrite the
// object every pass and churn field ownership.
func TestReconcile_WebhookSecretGrantIsIdempotent(t *testing.T) {
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	ghCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: "gh-creds"},
		Data: map[string][]byte{
			"app-id":          []byte("12345"),
			"private-key":     []byte("-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n"),
			"webhook-secret":  []byte("whsec_demo"),
			"installation-id": []byte("67890"),
		},
	}
	adoptguard.WithAdoptedLabel(ghCreds)
	ac := newValidAgentClass("agent-cls", nil)
	ac.Namespace = tenantNS
	ch := githubChannelIn(tenantNS, "demo-reviewbot-gh", "gh-creds")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ac, ch, ghCreds).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
		Build()
	r := newUnitReconciler(c)
	req := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: tenantNS, Name: ch.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	key := client.ObjectKey{Namespace: tenantNS, Name: channel.WebhookSecretRoleName(ch.Name)}
	var first rbacv1.Role
	require.NoError(t, c.Get(context.Background(), key, &first))

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	var second rbacv1.Role
	require.NoError(t, c.Get(context.Background(), key, &second))

	// Compared on CONTENT, not ResourceVersion: controller-runtime's fake
	// client re-writes the object on every Apply regardless of whether the
	// payload changed, so an RV comparison here would assert a property of the
	// fake rather than of this reconcile. Content equality is the fact that
	// matters — a volatile value in an SSA-applied field (a timestamp, a
	// generated name) would differ between the two passes and would churn
	// field ownership against a real apiserver.
	assert.Equal(t, first.Rules, second.Rules)
	assert.Equal(t, first.OwnerReferences, second.OwnerReferences)
	assert.Equal(t, first.Labels, second.Labels)
	assert.Equal(t, first.Annotations, second.Annotations)
}
