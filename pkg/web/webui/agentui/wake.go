// wake.go asks for a session back when a viewer's dashboard needs a runner
// that has been reaped.
//
// An agent-defined UI's data bindings are answered BY THE RUNNER, which makes
// a reaped runner unlike an idle conversation: a quiet conversation looks like
// one waiting for you to type, but a dashboard whose runner is gone has every
// section fail at once with no action the viewer can take.
//
// The wake is deliberately the SAME annotation channelsd writes for an inbound
// message — one reconciler path, one meaning — not a second mechanism. Its
// real cost is a model turn (the runner re-enters its loop before settling);
// a serve-only mode remains future work, and until then a turn on a cold
// open beats a dashboard that cannot recover at all.
package agentui

import (
	"context"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// requestWake stamps the wake annotation on an idle session, returning whether
// a wake is now pending — either because this call asked for one, or because
// one was already outstanding.
//
// Bounded to the namespaces this webd may WRITE in: its ClusterRole grants
// agentsessions:["get"] cluster-wide so the shell can serve a session wherever
// a viewer holds interact, but the write verbs live only in per-namespace
// Roles. Patching outside them would 403 once per binding on every page load,
// so the reach is checked here rather than discovered from the apiserver.
//
// Failures report "no wake pending", never an error to the viewer: the binding
// has already failed, and stacking a second failure on the first is worse than
// the plain unreachable message.
func requestWake(ctx context.Context, d Deps, ns, name string) bool {
	if !slices.Contains(d.StartableNamespaces(), ns) && len(d.StartableNamespaces()) > 0 {
		d.Logger().Info("agent-ui bindings: not waking a session outside this server's writable namespaces",
			"ns", ns, "name", name, "startableNamespaces", d.StartableNamespaces())
		return false
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if !apierrors.IsNotFound(err) {
			d.Logger().Info("agent-ui bindings: could not read the session to wake it",
				"ns", ns, "name", name, "err", err.Error())
		}
		return false
	}

	// Only a session that is actually parked with its pod gone. A running
	// session whose runner simply did not answer in time is a DIFFERENT
	// failure — a wedged or overloaded runner — and stamping a wake on it
	// would ask the reconciler for a pod that already exists, achieving
	// nothing while making the logs claim a recovery happened.
	if !spiceboxv1alpha1.WakeEligible(&sess) {
		return false
	}
	// Already asked, by this viewer a moment ago or by another surface. Report
	// pending without writing: a second annotation would be a no-op patch that
	// re-triggers the reconciler for every binding on the page.
	if spiceboxv1alpha1.WakePending(&sess) {
		return true
	}

	patch := client.MergeFrom(sess.DeepCopy())
	if sess.Annotations == nil {
		sess.Annotations = map[string]string{}
	}
	sess.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := d.K8s().Patch(ctx, &sess, patch); err != nil {
		d.Logger().Info("agent-ui bindings: could not request a wake for this session",
			"ns", ns, "name", name, "err", err.Error())
		return false
	}
	d.Logger().Info("agent-ui bindings: the session's runner was gone; requested a wake",
		"ns", ns, "name", name)
	return true
}
