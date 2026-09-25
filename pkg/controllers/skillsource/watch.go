// pkg/controllers/skillsource/watch.go
//
// Dependency watches for SkillSource. The clone credential is reached through
// two hops — SkillSource.spec.auth → AgentIdentity.spec.credentials[] → Secret —
// and a change at either hop changes the bytes the next clone will use. Watching
// only SkillSource means a rotated PAT is invisible to this controller: the
// source keeps serving the Ready=False it recorded on its last failure until the
// resync interval elapses, long after the credential behind it was repaired.
//
// Both mappers are namespace-scoped. SkillSource is namespaced and resolves its
// AgentIdentity in its own namespace (see resolveToken), so a Secret or identity
// in another namespace can never feed one here.
package skillsource

import (
	"context"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// credentialSecretName returns the Secret a credential reads its bytes from,
// or "" when this type has no backing Secret to watch (federated, or a
// malformed block) — the mappers skip it rather than guessing. An
// unregistered type is logged: unlike "no backing Secret," that is a config
// problem the watch would otherwise mask by silently never firing for it.
//
// Dispatches through credkindregistry.SecretNameFor rather than switching on
// cred.Static/OAuth/Federated itself — see credkind/guard_test.go's Guard 6.
func credentialSecretName(ctx context.Context, cred v1.AgentCredential) string {
	name, err := credkindregistry.SecretNameFor(cred)
	if err != nil {
		log.FromContext(ctx).Info("SkillSource Secret watch: credential has an unregistered type; a rotation behind it will not be seen until the next resync",
			"credential", cred.Name, "type", cred.Type, "err", err.Error())
		return ""
	}
	return name
}

// mapSecretToSources enqueues every SkillSource in the Secret's namespace whose
// credential resolves to it. This is the watch that carries a PAT rotation
// through to a re-clone.
//
// Two cached Lists, not a Get per source: the identities are indexed once and
// matched in memory, so the fan-out cost does not grow with the number of
// SkillSources sharing an identity.
func (r *Reconciler) mapSecretToSources(ctx context.Context, o client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	var ids v1.AgentIdentityList
	if err := r.Client.List(ctx, &ids, client.InNamespace(o.GetNamespace())); err != nil {
		logger.Info("list AgentIdentities for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
			"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	// identity name → the credential names within it backed by this Secret.
	backed := map[string]map[string]bool{}
	for i := range ids.Items {
		id := &ids.Items[i]
		for _, cred := range id.Spec.Credentials {
			if credentialSecretName(ctx, cred) != o.GetName() {
				continue
			}
			if backed[id.Name] == nil {
				backed[id.Name] = map[string]bool{}
			}
			backed[id.Name][cred.Name] = true
		}
	}
	if len(backed) == 0 {
		return nil
	}

	var srcs v1.SkillSourceList
	if err := r.Client.List(ctx, &srcs, client.InNamespace(o.GetNamespace())); err != nil {
		logger.Info("list SkillSources for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
			"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range srcs.Items {
		src := &srcs.Items[i]
		if src.Spec.Auth == nil {
			continue
		}
		if backed[src.Spec.Auth.AgentIdentity][src.Spec.Auth.Credential] {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
		}
	}
	return out
}

// mapIdentityToSources enqueues every SkillSource in the AgentIdentity's
// namespace that authenticates through it. This catches the hop the Secret watch
// cannot see: repointing a credential's secretRef at different bytes, which
// leaves the old Secret untouched and so raises no Secret event for this source.
func (r *Reconciler) mapIdentityToSources(ctx context.Context, o client.Object) []reconcile.Request {
	var srcs v1.SkillSourceList
	if err := r.Client.List(ctx, &srcs, client.InNamespace(o.GetNamespace())); err != nil {
		log.FromContext(ctx).Info("list SkillSources for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
			"agentidentity", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range srcs.Items {
		src := &srcs.Items[i]
		if src.Spec.Auth != nil && src.Spec.Auth.AgentIdentity == o.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
		}
	}
	return out
}

// compile-time guard that the Secret watch is registered against the type the
// mapper reads.
var _ client.Object = (*corev1.Secret)(nil)
