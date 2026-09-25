package toolcall

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// RecordSnapshotFn is the closure the operator wires at start-up to
// record a tool_dispatch_snapshot memory entry once a snapshot Job
// succeeds. The implementation closes over a memory.Memory client and
// builds the audit entry at the parent AgentSession's scope. Wiring it
// as a callback keeps this package free of memory-store knowledge: the
// only thing it knows is that the callee reaches the facade, so the
// context handed to it carries the operator's capability approval (see
// WaitPreDispatchSnapshot).
type RecordSnapshotFn func(ctx context.Context, parentSessionName, namespace string, h workspace.SnapshotHandle, toolUseID, bundleName string) error

// workspaceIsIsolated reports whether the bundle session runs an isolated
// (pod-local emptyDir) workspace rather than a shared RWX PVC. A pre-dispatch
// snapshot mounts the shared <session>-workspace PVC as its source; an isolated
// session has no such PVC, so launching the snapshot Job would leave its pod
// unschedulable ("persistentvolumeclaim ...-workspace not found") forever,
// blocking the tool call for 3m before erroring — repeated every reconcile.
// Only a genuinely shared workspace (mode "shared" WITH a claim name) has
// something to snapshot; an unset mode or an empty claim is isolated.
func workspaceIsIsolated(session *spiceboxv1alpha1.SpiceboxSession) bool {
	return session.Spec.Workspace.Mode != spiceboxv1alpha1.WorkspaceShared ||
		session.Spec.Workspace.SharedClaimName == ""
}

// HandlePreDispatchSnapshot launches the snapshot Job for tc's
// PreDispatchSnapshot if not already launched. Idempotent: a second
// call no-ops via Snapshotter's AlreadyExists handling. No-op when
// tc.Spec.PreDispatchSnapshot is nil.
//
// parentSessionName is the AgentSession name the bundle session belongs
// to — used to derive the snapshot-store PVC name via
// podspec.SnapshotStoreClaimName.
func HandlePreDispatchSnapshot(ctx context.Context, r *Reconciler, tc *spiceboxv1alpha1.ToolCall, src workspace.PVCRef, parentSessionName string) error {
	if tc.Spec.PreDispatchSnapshot == nil {
		return nil
	}
	if r.Snapshotter == nil {
		return fmt.Errorf("HandlePreDispatchSnapshot: ToolCall %q requested a workspace snapshot, but no Snapshotter is configured on the operator", tc.Name)
	}
	// The Job runs in the SESSION's namespace and references ap-snapshotter,
	// which the install bundle creates only in the operator's. Every session
	// elsewhere therefore produced a Job that could not create a pod at all —
	// and a Job with no pod is indistinguishable from a slow one, so the call
	// hung instead of failing. Ensured here, next to the launch, because
	// namespaces are created dynamically and a static manifest cannot cover
	// them.
	if err := ensureSnapshotServiceAccount(ctx, r, src.Namespace); err != nil {
		return fmt.Errorf("HandlePreDispatchSnapshot: %w", err)
	}
	storePVC := podspec.SnapshotStoreClaimName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: parentSessionName}})
	h := handleFromSpec(tc.Spec.PreDispatchSnapshot, storePVC)
	return r.Snapshotter.Snapshot(ctx, src, h)
}

// WaitPreDispatchSnapshot checks the snapshot Job for completion.
// Returns (true, nil) when Succeeded (and writes the audit entry via
// r.RecordSnapshotFn); (false, nil) when still running (caller
// should requeue); (false, err) when Failed.
//
// parentSessionName is the AgentSession name the bundle session
// belongs to — used to scope the audit entry. The reconciler resolves
// it once (from the bundle's label) and threads it in.
func WaitPreDispatchSnapshot(ctx context.Context, r *Reconciler, tc *spiceboxv1alpha1.ToolCall, src workspace.PVCRef, parentSessionName string) (bool, error) {
	if tc.Spec.PreDispatchSnapshot == nil {
		return true, nil
	}
	storePVC := podspec.SnapshotStoreClaimName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: parentSessionName}})
	h := handleFromSpec(tc.Spec.PreDispatchSnapshot, storePVC)
	var job batchv1.Job
	jobName := workspace.SnapshotJobName(h)
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: tc.Namespace, Name: jobName}, &job); err != nil {
		return false, fmt.Errorf("WaitPreDispatchSnapshot: get Job %q: %w", jobName, err)
	}
	switch {
	case job.Status.Succeeded > 0:
		if r.RecordSnapshotFn != nil && !conditions.IsTrue(tc.Status.Conditions, spiceboxv1alpha1.ToolCallConditionSnapshotAuditRecorded) {
			toolUseID := tc.Labels[spiceboxv1alpha1.LabelToolUseID]
			// The recorder reaches the operator's in-process memory facade,
			// whose capability door denies any caller that arrives without an
			// approval — and a reconcile context carries none of its own. The
			// entry is append-only, so the write also SEEDS the operator's
			// hash chain for the scope, which reads it; both halves need the
			// mint. Every other operator controller that touches the facade
			// makes the same one (agentsession, sessionhold).
			recordCtx := memory.WithSystemApproval(ctx, "operator:toolcall-controller")
			if rerr := r.RecordSnapshotFn(recordCtx, parentSessionName, tc.Namespace, h, toolUseID, tc.Spec.Session); rerr != nil {
				return false, fmt.Errorf("WaitPreDispatchSnapshot: record audit: %w", rerr)
			}
			// Stamp the condition so subsequent reconciles skip the
			// record call. The status update is best-effort; even if it
			// races, the next reconcile sees the persisted condition
			// and skips, and Put is idempotent anyway.
			conditions.SetTrue(tc, &tc.Status.Conditions, spiceboxv1alpha1.ToolCallConditionSnapshotAuditRecorded, "Recorded")
			if err := r.Client.Status().Update(ctx, tc); err != nil {
				// Log but don't fail — a status update conflict is recoverable
				// on next reconcile; the audit Put already happened.
				log.FromContext(ctx).Info("WaitPreDispatchSnapshot: status update after audit record failed; continuing",
					"toolcall", tc.Namespace+"/"+tc.Name, "err", err.Error())
			}
		}
		return true, nil
	case job.Status.Failed > 0:
		return false, fmt.Errorf("WaitPreDispatchSnapshot: snapshot Job %q failed (%d failures)", jobName, job.Status.Failed)
	default:
		// Succeeded=0 AND Failed=0 is ambiguous: a Job still copying looks
		// exactly like one whose pods were never created. Without a deadline
		// the caller requeues on both, forever — the ToolCall never resolves
		// and the agent goes silent mid-turn with nothing surfaced to the user.
		//
		// Seen in production: `ap-snapshotter` exists only in the system
		// namespace, so a Job in a session namespace logged FailedCreate x15
		// over 9m39s while the operator logged "toolspec allowed" every 2s. A
		// watchdog cannot help — the reconcile loop is healthy, it is the WORK
		// that is stuck.
		// A zero CreationTimestamp means the age is unknown (a fake client, a
		// partially-decoded object). Unknown is not the same as old: failing on
		// it would turn every such call into an error, so it requeues instead.
		if !job.CreationTimestamp.IsZero() && time.Since(job.CreationTimestamp.Time) > snapshotStartDeadline {
			// active>0 means a pod WAS created but never ran — it is stuck
			// Pending, typically unschedulable on an unbound PersistentVolumeClaim
			// (e.g. a snapshot-store PVC below its StorageClass floor) or for lack
			// of node capacity. That is a DIFFERENT failure from active==0, where
			// no pod was created at all — the missing-ServiceAccount/RBAC case. A
			// single "missing ServiceAccount" hint for both sent operators chasing
			// the wrong thing (the SA was present; the PVC was unbound), so branch.
			if job.Status.Active > 0 {
				return false, fmt.Errorf(
					"WaitPreDispatchSnapshot: snapshot Job %q could not start within %s "+
						"(its pod was created but never ran: succeeded=0 failed=0 active=%d) — "+
						"inspect it with `kubectl -n %s describe pod -l job-name=%s`; "+
						"typically an unschedulable pod: an unbound PersistentVolumeClaim "+
						"(e.g. the snapshot-store PVC below its StorageClass minimum) or insufficient capacity",
					jobName, snapshotStartDeadline, job.Status.Active, tc.Namespace, jobName)
			}
			return false, fmt.Errorf(
				"WaitPreDispatchSnapshot: snapshot Job %q could not start within %s "+
					"(no pod was ever created: succeeded=0 failed=0 active=0) — check its events, "+
					"typically a missing %q ServiceAccount in namespace %q",
				jobName, snapshotStartDeadline,
				workspace.SnapshotServiceAccount, tc.Namespace)
		}
		return false, nil
	}
}

// snapshotStartDeadline bounds how long a snapshot Job may sit without its pod
// producing any outcome.
//
// Generous on purpose: a large workspace legitimately takes a while to copy,
// and turning a slow snapshot into a failed tool call would be worse than the
// hang it replaces. What it catches is the case that can NEVER finish — an
// unschedulable Job — which is otherwise indistinguishable from progress.
const snapshotStartDeadline = 3 * time.Minute

func handleFromSpec(p *spiceboxv1alpha1.PreDispatchSnapshot, storePVC string) workspace.SnapshotHandle {
	return workspace.SnapshotHandle{
		SessionUID:       p.SessionUID,
		TurnIndex:        int(p.TurnIndex),
		Sequence:         int(p.Sequence),
		SnapshotStorePVC: storePVC,
	}
}

// ensureSnapshotServiceAccount creates the snapshot ServiceAccount in ns if it
// is absent.
//
// The account carries NO permissions — snapshot Jobs run cp/rm against mounted
// PVCs and never call the apiserver — so creating it is not a privilege grant.
// It exists to give tighter installs a name to bind pod-security and
// NetworkPolicy against, which is precisely why the Job's namespace must have
// its own rather than borrowing the operator's.
//
// AlreadyExists is success: several ToolCalls in one namespace race here, and
// the first to win has done the work.
func ensureSnapshotServiceAccount(ctx context.Context, r *Reconciler, ns string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspace.SnapshotServiceAccount,
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/component": "workspace-snapshot"},
		},
	}
	if err := r.Client.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure ServiceAccount %s/%s: %w", ns, workspace.SnapshotServiceAccount, err)
	}
	return nil
}
