package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	wsregistry "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

// Fallback image + ServiceAccount for the reconcile Job, used ONLY when the
// operator did not resolve them onto status.resolvedWorkspaceSource (e.g. a
// session created before this field existed). Normally the operator supplies
// the git image (from --materialize-image, so a cluster that pins a digest for
// the base-materialize Job gets the same pinned image here) and the SA (from
// --snapshot-service-account) via WorkspaceSourceRuntime — the tool prefers
// those, see reconcileImageOrDefault / reconcileSAOrDefault.
const (
	defaultWorkspaceReconcileImage = "alpine/git:latest"
	defaultWorkspaceReconcileSA    = "ap-snapshotter"
	workspaceReconcileDeadline     = int64(600)
)

func reconcileImageOrDefault(img string) string {
	if img != "" {
		return img
	}
	return defaultWorkspaceReconcileImage
}

func reconcileSAOrDefault(sa string) string {
	if sa != "" {
		return sa
	}
	return defaultWorkspaceReconcileSA
}

// wsToolBase is the runner-resolved, plain-string view of a session's bound
// workspace source. sync_workspace/apply_workspace take these as constructor
// args (not *capability.WorkspaceSourceRuntime) to avoid an import cycle:
// capability already imports meta, so meta must not import capability back.
// capability.Offer is the single place that unpacks WorkspaceSourceRuntime
// into these fields.
type wsToolBase struct {
	kind, locator, revision, workDir, overlayPVC string
	// reconcileImage/reconcileSA are the operator-resolved git image + SA the
	// reconcile Job runs as (from status.resolvedWorkspaceSource); empty falls
	// back to the package defaults.
	reconcileImage, reconcileSA string
}

// --- sync_workspace ---

type syncWorkspaceTool struct{ wsToolBase }

// NewSyncWorkspace builds the sync_workspace meta tool. kind is the
// workspacekinds registry key (e.g. "git"); locator/revision describe the
// bound source; workDir/overlayPVC describe the session's overlay mount;
// reconcileImage/reconcileSA are the operator-resolved Job image + SA (empty ⇒
// package defaults).
func NewSyncWorkspace(kind, locator, revision, workDir, overlayPVC, reconcileImage, reconcileSA string) tool.Tool {
	return &syncWorkspaceTool{wsToolBase{kind, locator, revision, workDir, overlayPVC, reconcileImage, reconcileSA}}
}

func (*syncWorkspaceTool) Name() string    { return "sync_workspace" }
func (*syncWorkspaceTool) Kind() tool.Kind { return tool.KindMeta }
func (*syncWorkspaceTool) Description() string {
	return "Pull the latest changes from the workspace source into your /workspace overlay."
}
func (*syncWorkspaceTool) InputSchema() json.RawMessage {
	return []byte(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// Permission returns Passthrough: sync only pulls the source's latest
// revision into the session's own overlay — it never touches the source
// origin, and it doesn't map to a SpiceDB resource to Check against (there is
// no per-call authz/approval decision to make). Passthrough carries no Check,
// so it's exempt from CheckRequired() and bypasses the runner's PreToolCall
// authz gate entirely — unlike Readonly, which DOES require a Check and would
// deny every call fail-closed ("stateImpact requires a check but none was
// supplied") since sync never declares one. apply_workspace (External) still
// requires a Check and stays gated through that same choke point.
func (*syncWorkspaceTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*syncWorkspaceTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *syncWorkspaceTool) Execute(ctx context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	return runWorkspaceReconcile(ctx, sess, t.wsToolBase, "sync", "")
}

// --- apply_workspace ---

type applyWorkspaceTool struct {
	wsToolBase
	credSecret string
}

// NewApplyWorkspace builds the apply_workspace meta tool. credSecret names
// the per-session Secret (envFrom'd into the reconcile Job) carrying the
// per-session write-back credential; empty is valid (a driver whose Applier
// needs no credential, or a misconfiguration the driver itself will reject).
// reconcileImage/reconcileSA are the operator-resolved Job image + SA (empty ⇒
// package defaults).
func NewApplyWorkspace(kind, locator, revision, workDir, overlayPVC, credSecret, reconcileImage, reconcileSA string) tool.Tool {
	return &applyWorkspaceTool{wsToolBase{kind, locator, revision, workDir, overlayPVC, reconcileImage, reconcileSA}, credSecret}
}

func (*applyWorkspaceTool) Name() string    { return "apply_workspace" }
func (*applyWorkspaceTool) Kind() tool.Kind { return tool.KindMeta }
func (*applyWorkspaceTool) Description() string {
	return "Reconcile your /workspace edits back to the source origin (e.g. git push). Requires human approval."
}
func (*applyWorkspaceTool) InputSchema() json.RawMessage {
	return []byte(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// Permission returns External so the runner's authz hook ALWAYS routes this
// through human approval before Execute runs — never silently applies.
func (*applyWorkspaceTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.External}
}
func (*applyWorkspaceTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *applyWorkspaceTool) Execute(ctx context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	return runWorkspaceReconcile(ctx, sess, t.wsToolBase, "apply", t.credSecret)
}

func wsErr(msg string) tool.Result { return tool.Result{Content: msg, IsError: true, Trusted: true} }

// runWorkspaceReconcile builds the driver's sync/apply commands and runs them
// as a one-shot Job against the session's overlay PVC, polling to a terminal
// condition. op is "sync" or "apply" (used only for naming/messages, never
// branched on for behavior beyond selecting SyncCommands vs ApplyCommands).
func runWorkspaceReconcile(ctx context.Context, sess *tool.SessionContext, b wsToolBase, op, credSecret string) (tool.Result, error) {
	if sess == nil || sess.K8sClient == nil {
		return wsErr("workspace " + op + ": no cluster client available"), nil
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok {
		return wsErr("workspace " + op + ": cluster client of unexpected type"), nil
	}
	drv, ok := wsregistry.Get(b.kind)
	if !ok {
		return wsErr("workspace " + op + ": no driver registered for kind " + b.kind), nil
	}
	spec := workspacekinds.Spec{Kind: b.kind, Locator: b.locator, Ref: b.revision}
	var cmds []workspacekinds.Command
	var err error
	if op == "apply" {
		ap, isApplier := drv.(workspacekinds.Applier)
		if !isApplier {
			return wsErr("workspace apply: driver " + b.kind + " does not support apply"), nil
		}
		cmds, err = ap.ApplyCommands(spec, b.workDir)
	} else {
		cmds, err = drv.SyncCommands(spec, b.workDir)
	}
	if err != nil {
		return wsErr("workspace " + op + ": " + err.Error()), nil
	}

	jobName := "ws-" + op + "-" + sess.Name
	key := client.ObjectKey{Namespace: sess.Namespace, Name: jobName}

	// Clean up any prior run (idempotent re-invoke), wait until it's gone.
	if err := c.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: jobName}},
		client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
		return wsErr("workspace " + op + ": could not clear prior job: " + err.Error()), nil
	}
	if err := waitGone(ctx, c, key, 30*time.Second); err != nil {
		return wsErr("workspace " + op + ": could not clear prior job: " + err.Error()), nil
	}

	credEnvVar, credKey := "", ""
	if op == "apply" {
		if ca, ok := drv.(workspacekinds.CredentialedApplier); ok {
			credEnvVar, credKey = ca.ApplyCredential()
		}
	}
	job := workspace.BuildReconcileJob(
		workspace.CPByPodConfig{Namespace: sess.Namespace, Image: reconcileImageOrDefault(b.reconcileImage), ServiceAccount: reconcileSAOrDefault(b.reconcileSA)},
		jobName, b.overlayPVC, cmds, credEnvVar, credSecret, credKey, workspaceReconcileDeadline)
	if sess.AgentSessionUID != "" {
		tval := true
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
			Name: sess.Name, UID: sess.AgentSessionUID, Controller: &tval, BlockOwnerDeletion: &tval,
		}}
	}
	if err := c.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return wsErr("workspace " + op + ": create job: " + err.Error()), nil
	}

	final, perr := waitJobDone(ctx, c, key, 16*time.Minute)
	if perr != nil {
		return wsErr("workspace " + op + ": " + perr.Error()), nil
	}
	if final.Status.Succeeded > 0 {
		return tool.Result{Content: "workspace " + op + " succeeded", Trusted: true}, nil
	}
	return wsErr("workspace " + op + " failed (job " + jobName + " did not succeed)"), nil
}

// waitGone polls until the Job named by key no longer exists (deletion is
// asynchronous with Foreground propagation). Returns nil once the Get 404s.
func waitGone(ctx context.Context, c client.Client, key client.ObjectKey, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		var j batchv1.Job
		if err := c.Get(ctx, key, &j); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("checking prior reconcile job: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for prior job to delete")
		case <-tick.C:
		}
	}
}

// waitJobDone polls the Job named by key until it reaches a terminal state
// (Succeeded>0, or a JobFailed=True condition), returning the terminal Job.
func waitJobDone(ctx context.Context, c client.Client, key client.ObjectKey, timeout time.Duration) (*batchv1.Job, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		var j batchv1.Job
		if err := c.Get(ctx, key, &j); err != nil {
			return nil, err
		}
		if j.Status.Succeeded > 0 {
			return &j, nil
		}
		for _, cond := range j.Status.Conditions {
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				return &j, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for job to complete")
		case <-tick.C:
		}
	}
}
