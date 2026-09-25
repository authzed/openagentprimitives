package installcmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	// Marker ConfigMap fields.
	workspaceMarkerNamespace = "agentprimitives-system"
	workspaceMarkerName      = "ap-workspace-config"
	workspaceMarkerKey       = "storageClassName"

	// Label key used by install manifests; oap install filters on this to
	// apply the bundled-provisioner region conditionally. Aliased to the
	// pkg/platform/cloud constants rather than respelled: the writer (the GKE stateful
	// strategy) and the reader (oap clean's label selector) live in different
	// packages, so a literal on either side could drift with no compile error
	// and orphan the StorageClass.
	installTierLabelKey             = cloud.InstallTierLabelKey
	installTierWorkspaceProvisioner = cloud.InstallTierWorkspaceProvisioner
)

// WorkspaceResolveOptions controls workspace storage resolution behavior.
// All fields are CLI-driven from oap install's flags.
type WorkspaceResolveOptions struct {
	// ExplicitClass, when non-empty, short-circuits detection and is
	// returned as-is. Driven by --workspace-storage-class.
	ExplicitClass string
	// NoRWX, when true, short-circuits detection and returns an isolated
	// decision. Driven by --no-workspace-rwx.
	NoRWX bool
	// Recheck forces detection to re-run even if the marker ConfigMap is
	// present. Driven by --workspace-storage-class-recheck.
	Recheck bool
	// Preconfirmed suppresses the interactive reselect picker: the caller has
	// ALREADY presented the workspace decision up front and folded the answer
	// into these options, so re-asking mid-install (the way a flag-driven
	// `oap install` legitimately offers the reselect) would double-ask. Set by
	// the unified init wizard, whose own Workspace screen replaces the reselect;
	// mirrors WebdRoutingOpts.preconfirmed for confirmPlan / askWebdRoutingInputs.
	Preconfirmed bool
}

// workspaceChoice is the FULLY-resolved workspace-storage decision: the
// short-circuits, the cloud Resolve, AND the interactive Filestore cost-confirm
// prompt have all already run. It is produced up-front (before the data-plane
// comes up) and consumed by the in-place WORK block, which only performs the
// side-effecting work (bundled provisioner apply, provisioning probe, marker
// write, operator patch) — it never re-resolves or re-prompts.
type workspaceChoice struct {
	// ClassName is the candidate RWX StorageClass; "" means isolated /work.
	// For a declined cost-confirm or a degraded decision this is "".
	ClassName string
	// NeedsBundled requests applying the bundled local-path provisioner before
	// probing ClassName. Only ever set on a probe (WorkspaceProbeBeforeUse)
	// decision — a cost-confirm (managed Filestore) decision never needs it.
	NeedsBundled bool
	// Probe requests the patient provisioning probe against ClassName before
	// trusting it. When false, ClassName is used directly (explicit override,
	// cached marker, or an accepted cost-confirm) with no probe.
	Probe bool
	// Message, when non-empty, is a single informational line for the WORK block
	// to emit (degrade guidance, or the declined-Filestore note). It preserves
	// the exact text the in-place block used to print.
	Message string
}

// injectWorkspaceClassUpFront reports whether the resolved workspace class is
// safe to stamp onto the operator Deployment doc BEFORE the base apply (via
// injectOperatorWorkspaceClass). Only a class we already trust EXISTS is stamped
// up-front: a cached marker, a cost-confirmed managed class, or a kept-current
// reselect — all Verify=WorkspaceUseDirectly, so Probe is false.
//
// A ProbeBeforeUse class (a fresh bundled provisioner, an explicit
// --workspace-storage-class, or a newly-picked reselect class) is WITHHELD: its
// StorageClass may not exist until applyWorkspaceProvisioner runs later in the
// WORK block, so stamping it onto the serving operator first would point every
// session PVC at a nonexistent class (worse than the isolated fallback), and it
// would break workspace.go's explicit-class contract ("fail LOUDLY at the probe,
// don't stamp it onto the operator first"). For those, patchOperatorWorkspaceClass
// AFTER the probe stays the setter. On a fresh cluster there is no prior operator
// flag, so withholding the up-front stamp strips nothing that was correct.
//
// An empty class name (isolated / degraded) is Probe=false too; the up-front call
// is then a harmless no-op (the base manifest carries no workspace flag to strip).
func injectWorkspaceClassUpFront(wsChoice workspaceChoice) bool {
	return !wsChoice.Probe
}

// resolveWorkspaceDecision computes the raw cloud.Decision: the cloud-agnostic
// short-circuits (explicit override, --no-workspace-rwx opt-out, or a cached
// marker from a prior verified install), else the cloud strategy's Resolve.
// Pure detection plus a best-effort marker read — no prompts, no provisioner
// applies. rep carries the rare marker-read warning AND the cloud strategy's
// Resolve narration, routed through the reporter (not a raw writer) so it does
// not corrupt the install checklist's live region.
func resolveWorkspaceDecision(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, wsOpts WorkspaceResolveOptions, rep progress.Reporter) (cloud.Decision, error) {
	if wsOpts.ExplicitClass != "" {
		// Probe an explicitly-named class before trusting it. A class that cannot
		// actually provision — a disabled cloud API (Filestore), a request below
		// the class floor, a typo'd/missing class — must fail `oap install`
		// LOUDLY here, not be stamped onto the operator and discovered only when
		// the first session's PVC hangs Pending. The marker path below stays
		// WorkspaceUseDirectly: a prior install already probed it.
		return cloud.Decision{ClassName: wsOpts.ExplicitClass, Verify: cloud.WorkspaceProbeBeforeUse}, nil
	}
	if wsOpts.NoRWX {
		return cloud.Decision{
			Degraded: true,
			Message:  "workspace storage: isolated (--no-workspace-rwx); multi-bundle agents use pod-local /work only",
		}, nil
	}
	if !wsOpts.Recheck {
		if got, ok, rerr := readWorkspaceMarker(ctx, bundle.Typed); rerr != nil {
			rep.Warn("read workspace marker: %v; re-detecting storage class", rerr)
		} else if ok {
			return cloud.Decision{
				ClassName:    got,
				NeedsBundled: got == cloud.BundledWorkspaceStorageClass,
				Verify:       cloud.WorkspaceUseDirectly,
			}, nil
		}
	}
	dec, wsErr := strat.WorkspaceStorage().Resolve(ctx, cloud.WorkspaceParams{
		Clients:  cloudClients(bundle),
		Reporter: newCloudReporterRep(rep),
	})
	if wsErr != nil {
		return cloud.Decision{}, fmt.Errorf("resolve workspace storage: %w", wsErr)
	}
	return dec, nil
}

// billableWorkspaceWarning is the cost heads-up the wizard shows before pinning
// a Filestore class its Workspace screen labelled but never priced. The linear
// path prints the strategy's own dec.CostWarning; the wizard's Workspace screen
// has no strategy decision to carry one, so this static line stands in.
const billableWorkspaceWarning = "This is a billable managed Filestore (cloud NFS) class — it provisions storage that accrues cost for as long as it exists."

// billableWorkspaceConfirmPrompt is the shared cost-confirm question for a
// billable managed RWX (Filestore) class. Used by BOTH the up-front
// decisionToChoice cost-confirm (a strategy that returns
// WorkspaceCostConfirmBeforeUse) and the wizard's own confirm when a user picks
// a Filestore class from the Workspace screen, so the two consent paths never
// drift on wording.
func billableWorkspaceConfirmPrompt(class string) string {
	return fmt.Sprintf("Use billable Filestore class %q for shared workspaces?", class)
}

// decisionToChoice maps a resolved cloud.Decision onto a workspaceChoice,
// performing the interactive Filestore cost-confirm prompt inline (the ONLY
// side effect — a cluster-free, prompt-only transform). A declined cost-confirm
// degrades to isolated /work (ClassName ""). This mirrors the original in-place
// switch exactly; only the cost WARNING + the prompt move up-front with it,
// while the outcome Message is emitted later by the WORK block.
func decisionToChoice(dec cloud.Decision, rep progress.Reporter, out io.Writer, in io.Reader, assumeYes, interactive bool) workspaceChoice {
	switch {
	case dec.Degraded:
		// No usable RWX on this cluster: isolated /work. The guidance message is
		// emitted during the WORK block (same as before).
		return workspaceChoice{Message: dec.Message}
	case dec.Verify == cloud.WorkspaceUseDirectly:
		// Explicit override or a cached marker: trust ClassName directly. No probe
		// and no bundled apply — even a marker pointing at the bundled class is
		// used as-is (the prior install already verified it).
		return workspaceChoice{ClassName: dec.ClassName}
	case dec.Verify == cloud.WorkspaceCostConfirmBeforeUse:
		// Billable managed RWX (Filestore): warn + confirm up-front, then trust.
		// A cost-confirm decision never carries NeedsBundled (it's a managed
		// class), so this never reorders relative to a bundled provisioner apply.
		rep.Warn("%s", dec.CostWarning)
		// Run the prompt under Suspend so the checklist's live region quiesces
		// (clears, then redraws) around it — a direct read/write mid-region
		// desyncs the cursor from the region's line count and corrupts the
		// redraw. The prompt keeps using the caller's own in/out (which are the
		// reporter's own streams in production); Suspend only supplies the
		// clear/redraw discipline around them.
		var confirmed bool
		rep.Suspend(func(_ io.Writer, _ io.Reader) {
			confirmed = apcmd.Confirm(in, out, billableWorkspaceConfirmPrompt(dec.ClassName), assumeYes, interactive)
		})
		if confirmed {
			return workspaceChoice{ClassName: dec.ClassName}
		}
		return workspaceChoice{Message: "declined Filestore; using isolated /work (pass --workspace-storage-class or -y to use it)"}
	default:
		// WorkspaceProbeBeforeUse: probe ClassName before trusting it; apply the
		// bundled local-path provisioner first when NeedsBundled.
		return workspaceChoice{ClassName: dec.ClassName, NeedsBundled: dec.NeedsBundled, Probe: true}
	}
}

// resolveWorkspaceChoice fully resolves the workspace-storage decision up-front:
// short-circuits + cloud Resolve + the interactive Filestore cost-confirm prompt.
// It is read-only on the cluster except for the prompt, and is called ONCE
// before the data-plane comes up so the install never stops to ask mid-flow.
func resolveWorkspaceChoice(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, wsOpts WorkspaceResolveOptions, rep progress.Reporter, out io.Writer, in io.Reader, assumeYes, interactive bool) (workspaceChoice, error) {
	// Before detection, offer to make a durable RWX class available (GKE: enable
	// the Filestore CSI addon). Consent-gated and side-effecting, so it lives
	// here — outside the pure resolveWorkspaceDecision — and runs only when
	// detection actually would.
	if err := maybeEnableWorkspaceStorage(ctx, bundle, strat.WorkspaceStorage(), wsOpts, rep, in, assumeYes); err != nil {
		return workspaceChoice{}, err
	}
	// Interactive upgrade path: when a prior install's marker pins the class and
	// the user hasn't forced a recheck or an explicit class, offer a picker
	// (current pre-selected) instead of silently honoring the marker. This is
	// what lets `oap init` / `oap install` on an existing cluster switch to a
	// better RWX class that was installed after the first install — the marker
	// short-circuit alone never re-evaluates. Gated to an interactive, non-`-y`
	// run so a scripted re-install stays a silent no-op, preserving the "never
	// silently enable billable Filestore" contract the marker exists for.
	// Preconfirmed suppresses it entirely: the wizard already presented the
	// Workspace decision, so re-asking here would double-prompt (and, on
	// --accept-existing, stall the install on a stdin read the flag promised to
	// skip).
	if interactive && !assumeYes && wsOpts.ExplicitClass == "" && !wsOpts.NoRWX && !wsOpts.Recheck && !wsOpts.Preconfirmed {
		if choice, ok, err := maybeReselectWorkspaceClass(ctx, bundle, strat, rep, out, in); err != nil {
			return workspaceChoice{}, err
		} else if ok {
			return choice, nil
		}
	}
	dec, err := resolveWorkspaceDecision(ctx, bundle, strat, wsOpts, rep)
	if err != nil {
		return workspaceChoice{}, err
	}
	return decisionToChoice(dec, rep, out, in, assumeYes, interactive), nil
}

// maybeReselectWorkspaceClass runs the interactive workspace-storage picker when
// a marker from a prior install is present. It returns (choice, true, nil) once
// the picker has been shown and the user has chosen (keeping the current class
// is a valid choice, and downstream a no-op). It returns (zero, false, nil) when
// there is no marker to re-select from — leaving the caller to fall through to
// normal detection — and also on a soft failure (marker unreadable, storage
// classes unlistable): it warns and falls through so the run degrades to the
// silent marker short-circuit rather than aborting an install that would
// otherwise have succeeded with the cached class.
func maybeReselectWorkspaceClass(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, rep progress.Reporter, out io.Writer, in io.Reader) (workspaceChoice, bool, error) {
	current, ok, err := readWorkspaceMarker(ctx, bundle.Typed)
	if err != nil {
		rep.Warn("read workspace marker: %v; re-detecting storage class", err)
		return workspaceChoice{}, false, nil
	}
	if !ok {
		return workspaceChoice{}, false, nil // no prior choice — normal detection runs
	}
	classes, err := cloud.ListRWXClasses(ctx, bundle.Typed)
	if err != nil {
		rep.Warn("list workspace storage classes: %v; keeping the current class", err)
		return workspaceChoice{}, false, nil
	}
	recommended := detectRecommendedWorkspaceClass(ctx, bundle, strat, newCloudReporterRep(rep))
	// Run the picker under Suspend so the checklist's live region quiesces around
	// the multi-line menu + stdin read instead of the picker printing into a live
	// region and desyncing the cursor. The picker keeps using the caller's own
	// in/out; Suspend only supplies the clear/redraw discipline around them.
	var choice workspaceChoice
	rep.Suspend(func(_ io.Writer, _ io.Reader) {
		choice = selectWorkspaceClass(in, out, classes, current, recommended)
	})
	return choice, true, nil
}

// detectRecommendedWorkspaceClass asks the cloud strategy which RWX class it
// would recommend (GKE: the Filestore multishare class), used only to annotate
// the picker menu. Best-effort: any error yields "" (no annotation), never an
// aborted install. reporter carries the strategy's Resolve narration — passed
// through the install reporter (newCloudReporterRep) when a live checklist is
// up, or a plain writer-backed reporter in the wizard's config phase.
func detectRecommendedWorkspaceClass(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, reporter cloud.Reporter) string {
	dec, err := strat.WorkspaceStorage().Resolve(ctx, cloud.WorkspaceParams{
		Clients:  cloudClients(bundle),
		Reporter: reporter,
	})
	if err != nil {
		return ""
	}
	return dec.ClassName
}

// maybeEnableWorkspaceStorage runs the optional, consent-gated enable step (a
// WorkspaceStorage that implements cloud.WorkspaceStorageEnabler — only GKE
// today) BEFORE detection, so a class it creates is picked up by Resolve. It is
// gated to the SAME condition under which detection runs: skipped on an explicit
// class (--workspace-storage-class), an opt-out (--no-workspace-rwx), or a
// cached marker from a prior verified install (unless --workspace-storage-class-
// recheck). That gate matters most under --yes: without it a plain re-install
// would silently enable billable storage. A WorkspaceStorage without the
// capability is a no-op.
func maybeEnableWorkspaceStorage(ctx context.Context, bundle *kube.Bundle, ws cloud.WorkspaceStorage, wsOpts WorkspaceResolveOptions, rep progress.Reporter, in io.Reader, assumeYes bool) error {
	if wsOpts.ExplicitClass != "" || wsOpts.NoRWX {
		return nil
	}
	if !wsOpts.Recheck {
		if _, ok, err := readWorkspaceMarker(ctx, bundle.Typed); err == nil && ok {
			return nil // a prior install already chose a class; don't re-offer
		}
	}
	enabler, ok := ws.(cloud.WorkspaceStorageEnabler)
	if !ok {
		return nil
	}
	return enabler.EnsureWorkspaceStorage(ctx, cloud.WorkspaceEnableParams{
		Clients:   cloudClients(bundle),
		Reporter:  newCloudReporterRep(rep),
		In:        in,
		AssumeYes: assumeYes,
	})
}

// readWorkspaceMarker returns the cached workspace StorageClass name from
// the marker ConfigMap, or ("", false, nil) if the ConfigMap doesn't exist
// yet. Errors other than NotFound surface.
func readWorkspaceMarker(ctx context.Context, kc kubernetes.Interface) (string, bool, error) {
	cm, err := kc.CoreV1().ConfigMaps(workspaceMarkerNamespace).Get(ctx, workspaceMarkerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read workspace marker: %w", err)
	}
	if cm.Data == nil {
		return "", false, nil
	}
	v, ok := cm.Data[workspaceMarkerKey]
	if !ok {
		return "", false, nil
	}
	return v, true, nil
}

// dynApplyFunc abstracts kube.Apply for tests. Production wires it to
// kube.Apply.
type dynApplyFunc func(ctx context.Context, dyn dynamic.Interface, d *unstructured.Unstructured, fieldOwner string) error

// applyWorkspaceProvisioner applies the workspace-provisioner manifests
// (the docs slice should already be filtered to the workspace-provisioner-rwx
// install tier). Waits up to 60s for the provisioner Deployment to reach
// 1 ready replica.
func applyWorkspaceProvisioner(
	ctx context.Context,
	docs []*unstructured.Unstructured,
	dyn dynamic.Interface,
	kc kubernetes.Interface,
	apply dynApplyFunc,
) error {
	for _, d := range docs {
		if err := apply(ctx, dyn, d, "ap-install"); err != nil {
			return fmt.Errorf("apply workspace-provisioner %s/%s: %w", d.GetKind(), d.GetName(), err)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		dep, err := kc.AppsV1().Deployments("ap-workspace-storage").Get(ctx, "ap-workspace-provisioner", metav1.GetOptions{})
		if err == nil && dep.Status.ReadyReplicas >= 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("workspace-provisioner Deployment did not become ready within 60s")
}

// writeWorkspaceMarker persists the chosen StorageClass name to the
// marker ConfigMap. Idempotent: re-writing the same value is a no-op
// from the user's perspective.
func writeWorkspaceMarker(ctx context.Context, kc kubernetes.Interface, className string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspaceMarkerName,
			Namespace: workspaceMarkerNamespace,
		},
		Data: map[string]string{workspaceMarkerKey: className},
	}
	_, err := kc.CoreV1().ConfigMaps(workspaceMarkerNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, gerr := kc.CoreV1().ConfigMaps(workspaceMarkerNamespace).Get(ctx, workspaceMarkerName, metav1.GetOptions{})
		if gerr != nil {
			return fmt.Errorf("get workspace marker for update: %w", gerr)
		}
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		existing.Data[workspaceMarkerKey] = className
		if _, uerr := kc.CoreV1().ConfigMaps(workspaceMarkerNamespace).Update(ctx, existing, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("update workspace marker: %w", uerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create workspace marker: %w", err)
	}
	return nil
}

const (
	operatorNamespace   = apcmd.SystemNamespace
	operatorDeployment  = apcmd.OperatorDeployment
	operatorContainer   = "operator"
	workspaceFlagPrefix = "--workspace-storage-class="
)

// patchOperatorWorkspaceClass updates the operator Deployment's container
// args to set --workspace-storage-class=<className>. An empty className
// removes the flag entirely (operator falls back to "", which means
// isolated workspaces only). Idempotent.
func patchOperatorWorkspaceClass(ctx context.Context, kc kubernetes.Interface, className string) error {
	dep, err := kc.AppsV1().Deployments(operatorNamespace).Get(ctx, operatorDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get operator Deployment: %w", err)
	}
	containerIdx := -1
	for i, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == operatorContainer {
			containerIdx = i
			break
		}
	}
	if containerIdx < 0 {
		return fmt.Errorf("operator Deployment has no container named %q", operatorContainer)
	}
	existing := dep.Spec.Template.Spec.Containers[containerIdx].Args
	updated := make([]string, 0, len(existing)+1)
	for _, a := range existing {
		if !strings.HasPrefix(a, workspaceFlagPrefix) {
			updated = append(updated, a)
		}
	}
	if className != "" {
		updated = append(updated, workspaceFlagPrefix+className)
	}
	if argsEqual(existing, updated) {
		return nil // no-op
	}
	dep.Spec.Template.Spec.Containers[containerIdx].Args = updated
	if _, err := kc.AppsV1().Deployments(operatorNamespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update operator Deployment args: %w", err)
	}
	return nil
}

func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// awaitProvisioningProbe drives a WorkspaceProbeBeforeUse (or stateful-storage
// probe) path with a patient wait: it polls probe until the PVC binds, offers
// to keep waiting on each deadline (skipped in non-interactive / --yes mode),
// and NEVER silently falls back. On failure it returns a descriptive error so
// the caller can surface it and let the user decide — automatic downgrade is
// prohibited by the no-automatic-fallbacks design rule.
//
// A transient ProvisioningFailed event is surfaced as diagnosis context (the
// provisioner may retry), NOT treated as terminal.
func awaitProvisioningProbe(
	ctx context.Context,
	recheckCtx context.Context,
	phaseLabel string,
	className string,
	probe cloud.ProbeStep,
	rep progress.Reporter,
) (chosen string, err error) {
	ph := rep.Phase(phaseLabel)

	poll := progress.Poll(func(pctx context.Context) (bool, error) {
		bound, _, perr := probe(pctx)
		return bound, perr
	})
	diagnose := progress.Diagnose(func(dctx context.Context) (wait.Diagnosis, error) {
		// Read the latest transient reason from the probe and surface it as a
		// ProvisioningFailed event so the operator can see why the PVC is still
		// not bound. Probe errors are propagated so awaitLoopRecheck can surface
		// "couldn't gather diagnostics" rather than silently dropping them.
		bound, why, perr := probe(dctx)
		if perr != nil {
			return wait.Diagnosis{}, perr
		}
		if why == "" || bound {
			return wait.Diagnosis{}, nil
		}
		return wait.Diagnosis{
			Events: []wait.EventNote{{Reason: "ProvisioningFailed", Message: why}},
		}, nil
	})

	if werr := ph.Await(ctx, recheckCtx, installWaitDeadline, 0, poll, diagnose); werr != nil {
		ph.Fail()
		return "", fmt.Errorf("workspace storage class %q: %w", className, werr)
	}
	ph.Done()
	return className, nil
}

// handleBundledProvisionerFailure enforces the §A3 no-automatic-fallbacks rule
// when applyWorkspaceProvisioner returns an error. Degrading to isolated /work
// requires an explicit user opt-out (wsOpts.NoRWX) or affirmative interactive
// consent. Every other path — non-interactive, --yes, or interactive decline —
// returns a hard error so the caller can surface it and let the user decide;
// automatic silent downgrade is prohibited.
//
// wsOpts.NoRWX already short-circuits workspace resolution before the bundled
// provisioner path is reached, so that branch is unreachable in practice and is
// present only as a defence-in-depth guard and to make the contract testable.
//
// Returns (true, nil) when the caller should degrade to isolated /work, or
// (false, err) to abort installation.
func handleBundledProvisionerFailure(cause error, wsOpts WorkspaceResolveOptions, assumeYes, interactive bool, rep progress.Reporter, in io.Reader, out io.Writer) (bool, error) {
	if wsOpts.NoRWX {
		// Explicit opt-out: user chose isolated /work up-front.
		return true, nil
	}
	if interactive && !assumeYes {
		// Interactive TTY without --yes: let the user decide. Run the prompt under
		// Suspend so the checklist's live region quiesces around the read instead
		// of the prompt corrupting a live region. Read from the caller's `in` (the
		// reporter's stdin may be a different stream — see the workspace tests).
		var consented bool
		rep.Suspend(func(_ io.Writer, _ io.Reader) {
			consented = apcmd.Confirm(in, out,
				"Bundled workspace provisioner failed — continue WITHOUT a shared (RWX) workspace, using isolated /work? Multi-bundle agents will lose shared state.",
				assumeYes, interactive)
		})
		if consented {
			return true, nil // user consented to degradation
		}
	}
	// Non-interactive, --yes without explicit opt-out, or user declined: hard error.
	return false, fmt.Errorf("bundled workspace provisioner failed: %w", cause)
}
