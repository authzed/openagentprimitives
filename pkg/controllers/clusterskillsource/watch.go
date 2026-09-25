// pkg/controllers/clusterskillsource/watch.go
//
// Dependency watch for ClusterSkillSource, the cluster-scoped mirror of the
// namespaced watch in pkg/controllers/skillsource/watch.go. Watching only the
// ClusterSkillSource means a rotated PAT is invisible to this controller: the
// source keeps serving the Ready=False from its last failure until the resync
// interval elapses, long after the credential behind it was repaired.
//
// One hop, not two: a cluster source reads its Secret directly at
// spec.auth.namespace (AgentIdentity is namespaced and unreachable from cluster
// scope — see resolveToken), so there is no identity watch to mirror.
package clusterskillsource

import (
	"context"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// mapSecretToSources enqueues every ClusterSkillSource whose auth Secret is the
// one that changed. The match is namespace AND name: a cluster source names the
// namespace it reads from, so an identically-named Secret in any other namespace
// is a different credential and must not wake it.
func (r *Reconciler) mapSecretToSources(ctx context.Context, o client.Object) []reconcile.Request {
	var srcs v1.ClusterSkillSourceList
	if err := r.Client.List(ctx, &srcs); err != nil {
		log.FromContext(ctx).Info("list ClusterSkillSources for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
			"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range srcs.Items {
		src := &srcs.Items[i]
		auth := src.Spec.Auth
		if auth == nil {
			continue
		}
		if auth.Namespace == o.GetNamespace() && auth.SecretRef.Name == o.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
		}
	}
	return out
}
