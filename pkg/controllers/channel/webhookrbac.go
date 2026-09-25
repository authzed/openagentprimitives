// pkg/controllers/channel/webhookrbac.go grants webd the ONE Secret read an
// inbound webhook delivery needs, in the namespace the Channel actually lives
// in.
//
// webd serves /webhooks/<kind>/<ns>/<name> for every namespace (the route is
// mounted once and addresses the namespace in the URL), but its shipped Secret
// grants are namespace-scoped: config/webd/role.yaml covers
// agentprimitives-identities and config/webd/role-default.yaml covers
// "default". A Channel anywhere else made every delivery fail at step 4 of the
// handler with a 500 — GitHub retries, then disables the hook, and nothing on
// the Channel says why.
//
// Widening webd's ClusterRole is NOT the fix and must not become one. webd is
// browser-facing; a cluster-wide Secret read on that ServiceAccount is
// standing privilege that outlives every request, and
// pkg/platform/manifests/rbac_sufficiency_test.go pins "no Secret get in a
// tenant namespace" as forbidden for exactly that reason. So the grant is
// minted per Channel instead: a Role naming that one Secret by resourceName,
// bound to webd's ServiceAccount, owner-referenced to the Channel so it is
// garbage-collected with it. The blast radius of the whole mechanism is the
// credentials Secrets of Channels an operator deliberately created.
package channel

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// WebdServiceAccountName / WebdServiceAccountNamespace identify the subject
// these grants bind — the ServiceAccount shipped in
// config/webd/serviceaccount.yaml, which is what runs the channelwebhook
// route.
//
// Named here rather than read from anywhere at runtime, because a RoleBinding
// subject is a plain string: a mismatch does not error, it simply grants
// nothing, and the symptom would be the identical 500 this file exists to
// remove. TestWebdServiceAccountMatchesTheShippedBundle (in
// pkg/platform/manifests) pins these two constants against the bundle
// oap install actually applies.
const (
	WebdServiceAccountName      = "spicebox-webd"
	WebdServiceAccountNamespace = "agentprimitives-system"
)

// webhookSecretRBACFieldOwner is the SSA field manager for the stamped Role
// and RoleBinding. Distinct from any other manager touching these namespaces,
// so ownership of these two objects is attributable to this reconcile.
const webhookSecretRBACFieldOwner = "channel-webhook-secret-rbac"

// WebhookSecretRoleName is the deterministic name of the per-Channel Role and
// RoleBinding. Derived purely from the Channel name so a re-apply is
// byte-identical, and so an operator reading `kubectl get role` can tell at a
// glance which Channel a grant belongs to.
func WebhookSecretRoleName(channelName string) string {
	return channelName + "-webhook-secret-reader"
}

// BuildWebhookSecretRBAC produces the Role + RoleBinding that let webd read
// ch's credentials Secret — and nothing else — in ch's own namespace.
//
// Pure function of its inputs: no timestamps, no randomness, no generated
// names, so a byte-identical re-apply is an SSA no-op (see AGENTS.md's
// server-side-apply rule). Exported for the same reason
// agentsession.BuildRunnerRBAC is: the shape of a grant is worth asserting in
// a test without standing up a reconcile.
func BuildWebhookSecretRBAC(ch *spiceboxv1alpha1.Channel) (*rbacv1.Role, *rbacv1.RoleBinding) {
	owner := []metav1.OwnerReference{{
		APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:               "Channel",
		Name:               ch.Name,
		UID:                ch.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
	name := WebhookSecretRoleName(ch.Name)

	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ch.Namespace,
			OwnerReferences: owner,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			// Pinned by name to this Channel's credentials Secret. Without
			// ResourceNames this would be "read every Secret in the
			// namespace", which is the grant the ClusterRole deliberately
			// does not hold.
			ResourceNames: []string{ch.Spec.CredentialsRef.SecretName},
			Verbs:         []string{"get"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ch.Namespace,
			OwnerReferences: owner,
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      WebdServiceAccountName,
			Namespace: WebdServiceAccountNamespace,
		}},
	}
	return role, rb
}

// channelNeedsWebhookSecretRBAC reports whether ch's kind receives deliveries
// over HTTP at webd, and so needs the grant.
//
// Asked of the registry — "does this kind return a WebhookReceiver?" — never
// as an `if kind == "github"`. Capability here is declared by implementing
// (see channelkinds.WebhookReceiver's doc), so a second webhook-routable
// transport gets the grant by existing, the same way channelwebhook routes to
// it without changing a line.
//
// A zero Deps is passed deliberately: this asks only whether the kind HAS a
// receiver, not for one that can serve a delivery. channelwebhook builds the
// real one, with real Deps, per request.
func channelNeedsWebhookSecretRBAC(ch *spiceboxv1alpha1.Channel) bool {
	if ch.Spec.CredentialsRef.SecretName == "" {
		return false // nothing to grant a read of
	}
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return false // unknown kind; the Valid condition already covers this
	}
	return k.WebhookReceiver(channelkinds.Deps{}) != nil
}

// ensureWebhookSecretRBAC stamps (SSA) the per-Channel Role + RoleBinding for
// a webhook-routable Channel. A kind with no webhook receiver is a no-op.
//
// Returns the error rather than swallowing it: without this grant every
// delivery 500s, and a 500 the provider retries and then gives up on is
// exactly the failure that must not be silent. The caller logs it and
// requeues.
//
// There is no delete path, matching ensureRunnerNetworkPolicy: the owner
// reference reaps both objects when the Channel goes away. The one residue
// this leaves is a Channel edited in place from a webhook-routable kind to a
// non-routable one, which keeps a get on its own credentials Secret in its own
// namespace until the Channel is deleted — a narrow, deliberately-created
// grant, not a widening.
func (r *Reconciler) ensureWebhookSecretRBAC(ctx context.Context, ch *spiceboxv1alpha1.Channel) error {
	if !channelNeedsWebhookSecretRBAC(ch) {
		return nil
	}
	role, rb := BuildWebhookSecretRBAC(ch)
	for _, obj := range []client.Object{role, rb} {
		if err := r.Client.Patch(ctx, obj,
			client.Apply, client.ForceOwnership, client.FieldOwner(webhookSecretRBACFieldOwner),
		); err != nil {
			return fmt.Errorf("apply webhook-secret %T %q in %q: %w", obj, obj.GetName(), ch.Namespace, err)
		}
	}
	log.FromContext(ctx).V(1).Info("webhook-secret RBAC applied",
		"channel", ch.Namespace+"/"+ch.Name, "role", role.Name, "secret", ch.Spec.CredentialsRef.SecretName)
	return nil
}
