// pkg/platform/manifests/webd_channel_read_test.go pins one grant in the SHIPPED
// bundle, because losing it is invisible until a user opens a conversation in
// a namespace nobody tested.
package manifests_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/controllers/channel"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// webdClusterRole finds the spicebox-webd ClusterRole in the embedded bundle
// oap install actually applies — not config/, which a regeneration could have
// failed to carry through.
func webdClusterRole(t *testing.T) rbacv1.ClusterRole {
	t.Helper()
	objs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split the embedded install bundle")
	for _, o := range objs {
		if o.GetKind() != "ClusterRole" || o.GetName() != "spicebox-webd" {
			continue
		}
		var cr rbacv1.ClusterRole
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &cr),
			"decode the spicebox-webd ClusterRole")
		return cr
	}
	t.Fatal("the embedded install bundle contains no spicebox-webd ClusterRole")
	return rbacv1.ClusterRole{}
}

// grants reports whether any rule in the role covers (group, resource, verb).
func grants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	has := func(vals []string, want string) bool {
		for _, v := range vals {
			if v == want || v == "*" {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if has(r.APIGroups, group) && has(r.Resources, resource) && has(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestWebdClusterRoleReadsChannelsClusterWide pins the read that makes a
// conversation OPENABLE outside "default".
//
// pkg/web/webui/chat's session-scoped routes address whatever namespace the
// viewer holds agentsession#interact in, and Registry.rehydrate Gets the bound
// Channel there. The namespaced spicebox-webd-default Role covers only
// "default", so without this cluster-wide read the websocket, message,
// interrupt and decision routes all 403 — which the registry maps to "session
// not found". The transcript and info panel keep working (operator memory, plus
// the cluster-wide agentsessions read), so the symptom is a conversation that
// renders but cannot be opened, and the 404 says nothing about RBAC.
//
// The companion assertions are the non-vacuity control: this must stay a READ.
// Channel/Secret CREATION belongs to the namespaced Role — a browser-facing pod
// holding cluster-wide create/delete on either is standing privilege.
func TestWebdClusterRoleReadsChannelsClusterWide(t *testing.T) {
	cr := webdClusterRole(t)
	const g = "agentprimitives.authzed.com"

	assert.True(t, grants(cr.Rules, g, "channels", "get"),
		"the webd ClusterRole must grant channels:get cluster-wide, or every conversation outside \"default\" is readable but unopenable")

	for _, verb := range []string{"create", "delete", "update", "patch"} {
		assert.Falsef(t, grants(cr.Rules, g, "channels", verb),
			"the webd ClusterRole must NOT grant channels:%s — per-conversation creation stays in the namespaced Role", verb)
	}
	assert.False(t, grants(cr.Rules, "", "secrets", "get"),
		"the webd ClusterRole must hold no cluster-wide Secret access")
}

// TestWebdServiceAccountMatchesTheShippedBundle pins the two constants the
// operator uses as the SUBJECT of every per-Channel webhook-secret RoleBinding
// (pkg/controllers/channel/webhookrbac.go) against the ServiceAccount the
// bundle actually ships.
//
// A RoleBinding subject is a plain string the apiserver does not resolve: bind
// to a ServiceAccount that does not exist and the object is accepted, grants
// nothing, and webd goes on answering 500 to every inbound delivery with
// nothing anywhere saying why. Renaming webd's ServiceAccount in config/
// without updating those constants is exactly that mistake, and this is where
// it gets caught.
func TestWebdServiceAccountMatchesTheShippedBundle(t *testing.T) {
	objs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split the embedded install bundle")

	var found []string
	for _, o := range objs {
		if o.GetKind() != "ServiceAccount" {
			continue
		}
		if o.GetName() == channel.WebdServiceAccountName && o.GetNamespace() == channel.WebdServiceAccountNamespace {
			return
		}
		found = append(found, o.GetNamespace()+"/"+o.GetName())
	}
	t.Fatalf("the embedded install bundle ships no ServiceAccount %q in %q — the per-Channel webhook-secret RoleBinding would bind to nothing; bundle has: %v",
		channel.WebdServiceAccountName, channel.WebdServiceAccountNamespace, found)
}
