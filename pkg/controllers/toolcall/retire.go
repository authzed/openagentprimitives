package toolcall

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// CacheSelector is the label selector the operator's informer cache uses for
// ToolCalls: only calls this controller has NOT retired. A retired call drops
// out of the informer, which delivers that as a delete and evicts it, so the
// cache holds in-flight work rather than every call of every session still on
// the cluster. Wire it as the ToolCall entry of cache.Options.ByObject.
//
// Nothing in the operator reads a ToolCall it has retired. This controller is
// the only cached reader, and a terminal call is a no-op for it. Every other
// reader (the runner, channelsd, the CLI) has its own client and still sees the
// object. A Get through this cache for a retired call returns NotFound, so any
// new operator-side reader of finished ToolCalls must use the APIReader.
func CacheSelector() labels.Selector {
	req, err := labels.NewRequirement(spiceboxv1alpha1.LabelToolCallRetired, selection.DoesNotExist, nil)
	if err != nil {
		// The key is a compile-time constant, so this only fires if someone
		// edits it into an invalid label key.
		panic("toolcall: building the retired-label cache selector: " + err.Error())
	}
	return labels.NewSelector().Add(*req)
}

// retire marks a terminal ToolCall as finished with, in a single metadata
// write: it removes the finalizer and stamps LabelToolCallRetired.
//
// Both have to happen in the same write. Once the label lands, CacheSelector
// drops the object from the cache and this controller never sees it again. A
// finalizer left on it then could never be removed, and the owner-ref cascade
// that GCs the call with its AgentSession would leave it Terminating forever.
// The finalizer has nothing to protect at this point anyway: its only work is
// cancelling a call that is still running, and a terminal call is not.
func (r *Reconciler) retire(ctx context.Context, tc *spiceboxv1alpha1.ToolCall) error {
	if tc.Labels[spiceboxv1alpha1.LabelToolCallRetired] == "true" &&
		!controllerutil.ContainsFinalizer(tc, spiceboxv1alpha1.FinalizerToolCall) {
		return nil
	}
	if tc.Labels == nil {
		tc.Labels = map[string]string{}
	}
	tc.Labels[spiceboxv1alpha1.LabelToolCallRetired] = "true"
	controllerutil.RemoveFinalizer(tc, spiceboxv1alpha1.FinalizerToolCall)
	if err := r.Client.Update(ctx, tc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		log.FromContext(ctx).Info("toolcall: retiring a terminal ToolCall failed; it stays cached until a retry succeeds",
			"toolcall", tc.Namespace+"/"+tc.Name, "terminal", tc.TerminalCondition(), "err", err.Error())
		return err
	}
	return nil
}

// strandedRetiredSweep removes FinalizerToolCall from every ToolCall that
// already carries LabelToolCallRetired, once, at startup.
//
// The admission webhook refuses a write that arrives at label+finalizer, but
// an object can still be in that state from before it ran: an operator rolled
// back to a version that adds the finalizer to every cached call (and caches
// retired ones, since it has no selector), then rolled forward again. Such a
// call is invisible to this controller's cache, so without this sweep its
// finalizer is never removed and a session GC or namespace delete leaves it
// Terminating for good. It lists through the APIReader for the same reason.
//
// Best-effort: a failure is logged and the sweep moves on, because returning
// an error from a Runnable stops the manager, and a stranded finished call is
// not worth taking the operator down for. The next restart retries.
type strandedRetiredSweep struct {
	reader client.Reader
	writer client.Client
}

// NeedLeaderElection keeps the sweep to the replica that owns ToolCall writes.
func (s *strandedRetiredSweep) NeedLeaderElection() bool { return true }

func (s *strandedRetiredSweep) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("toolcall-retired-sweep")
	req, err := labels.NewRequirement(spiceboxv1alpha1.LabelToolCallRetired, selection.Exists, nil)
	if err != nil {
		logger.Info("building the retired-label selector failed; skipping the stranded-finalizer sweep", "err", err.Error())
		return nil
	}
	var calls spiceboxv1alpha1.ToolCallList
	if err := s.reader.List(ctx, &calls, client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*req)}); err != nil {
		logger.Info("listing retired ToolCalls failed; stranded finalizers are retried on the next restart", "err", err.Error())
		return nil
	}
	healed := 0
	for i := range calls.Items {
		tc := &calls.Items[i]
		if !controllerutil.ContainsFinalizer(tc, spiceboxv1alpha1.FinalizerToolCall) {
			continue
		}
		base := tc.DeepCopy()
		controllerutil.RemoveFinalizer(tc, spiceboxv1alpha1.FinalizerToolCall)
		if err := s.writer.Patch(ctx, tc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			logger.Info("removing the finalizer from a retired ToolCall failed; retried on the next restart",
				"toolcall", tc.Namespace+"/"+tc.Name, "err", err.Error())
			continue
		}
		healed++
	}
	if healed > 0 {
		logger.Info("removed the finalizer from retired ToolCalls an older operator left it on", "count", healed)
	}
	return nil
}
