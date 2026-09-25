// Package artifactrender hosts the ArtifactRender CRD reconciler. It
// drives the phase machine Pending → Rendering → Ready/Failed by
// dispatching to the registered renderer plug-in (pkg/channels/channelassets/registry)
// and persisting rendered bytes to the operator's artifactstore.
//
// v1 supports only InProcess renderers. PodSpawn renderers fail fast
// with FailureReason=RendererUnknown for forward compatibility — the
// CRD shape doesn't change when PodSpawn lands.
package artifactrender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// toCRWarnings maps the renderer's structured warnings onto the CRD status
// mirror type (the apis package can't import channelassets).
func toCRWarnings(ws []channelassets.Warning) []spiceboxv1alpha1.SanitizerWarning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.SanitizerWarning, 0, len(ws))
	for _, w := range ws {
		out = append(out, spiceboxv1alpha1.SanitizerWarning{
			Kind: w.Kind, Name: w.Name, Action: w.Action, Count: w.Count, Note: w.Note,
		})
	}
	return out
}

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=artifactrenders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=artifactrenders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=artifactrenders/finalizers,verbs=update

// defaultMaxOutputAbsolute is the operator-side cap that backstops every
// renderer's MaxInputSize / MaxOutputSize. 10 MiB matches the figure
// quoted in the channelassets design doc and the Renderer interface
// godoc.
const defaultMaxOutputAbsolute int64 = 10 << 20

// Reconciler implements the ArtifactRender controller.
type Reconciler struct {
	Client client.Client
	Store  artifactstore.Store

	// MaxOutputAbsolute caps both input and output byte size in addition
	// to per-renderer caps. <=0 falls back to defaultMaxOutputAbsolute.
	MaxOutputAbsolute int64
}

// Reconcile drives the phase machine for a single ArtifactRender.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr spiceboxv1alpha1.ArtifactRender
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &cr); !cont {
		return ctrl.Result{}, err
	}

	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}

	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &cr, spiceboxv1alpha1.FinalizerArtifactRender); added || err != nil {
		return ctrl.Result{Requeue: added}, err
	}

	if cr.Status.Phase == spiceboxv1alpha1.ArtifactRenderPhaseReady ||
		cr.Status.Phase == spiceboxv1alpha1.ArtifactRenderPhaseFailed {
		return ctrl.Result{}, nil
	}

	return r.runRender(ctx, &cr)
}

// runRender resolves the renderer, reads the payload, dispatches with a
// deadline, and persists the outcome in a SINGLE Status().Update call.
//
// The Pending → Rendering → Ready/Failed transitions are tracked
// in-memory only; we don't push an interim Rendering status to the API
// server. Reasons:
//
//   - InProcess renderers complete in milliseconds, so the Rendering
//     state is barely visible to consumers anyway.
//   - A two-phase status update sequence (Rendering, then Ready/Failed)
//     bumps ResourceVersion between calls and risks an optimistic-
//     concurrency conflict on the second update — especially with the
//     fake client used by tests.
//
// StartedAt is still recorded so observability of long-running future
// PodSpawn renderers stays meaningful — it's set in-memory at dispatch,
// FinishedAt at outcome, and both persist together.
func (r *Reconciler) runRender(ctx context.Context, cr *spiceboxv1alpha1.ArtifactRender) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	cr.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseRendering
	cr.Status.StartedAt = &now
	cr.Status.ObservedGeneration = cr.Generation

	renderer, ok := registry.ByKind(cr.Spec.Kind)
	if !ok {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderRendererUnknown,
			fmt.Sprintf("no renderer registered for kind %q", cr.Spec.Kind))
	}
	if renderer.ExecutionMode() != channelassets.ExecutionModeInProcess {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderRendererUnknown,
			fmt.Sprintf("renderer %q ExecutionMode=%s not supported in v1",
				cr.Spec.Kind, renderer.ExecutionMode()))
	}

	payload, err := r.readPayload(ctx, cr)
	if err != nil {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderInternalError,
			fmt.Sprintf("read payload: %v", err))
	}
	if int64(len(payload)) > renderer.MaxInputSize() || int64(len(payload)) > r.absoluteCap() {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderPayloadTooLarge,
			fmt.Sprintf("input %d bytes exceeds limits (renderer max %d, operator absolute %d)",
				len(payload), renderer.MaxInputSize(), r.absoluteCap()))
	}

	timeout := time.Duration(cr.Spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, rerr := renderer.Render(rctx, channelassets.Input{
		Payload:  payload,
		Filename: cr.Spec.Filename,
		AltText:  cr.Spec.AltText,
	})
	if rerr != nil {
		reason := spiceboxv1alpha1.ReasonArtifactRenderRendererError
		switch {
		case errors.Is(rerr, context.DeadlineExceeded):
			reason = spiceboxv1alpha1.ReasonArtifactRenderTimeout
		case errors.Is(rerr, channelassets.ErrMalformedPayload):
			reason = spiceboxv1alpha1.ReasonArtifactRenderMalformedInput
		}
		return r.markFailed(ctx, cr, reason, rerr.Error())
	}

	if int64(len(out.Bytes)) > renderer.MaxOutputSize() || int64(len(out.Bytes)) > r.absoluteCap() {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderOutputTooLarge,
			fmt.Sprintf("output %d bytes exceeds limits (renderer max %d, operator absolute %d)",
				len(out.Bytes), renderer.MaxOutputSize(), r.absoluteCap()))
	}

	key := path.Join("artifactrender", cr.Namespace, cr.Name, "out")
	ref, perr := r.Store.Put(ctx, key, bytes.NewReader(out.Bytes))
	if perr != nil {
		return r.markFailed(ctx, cr, spiceboxv1alpha1.ReasonArtifactRenderInternalError,
			fmt.Sprintf("artifactstore.Put: %v", perr))
	}

	finished := metav1.NewTime(time.Now())
	cr.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
	cr.Status.OutputRef = string(ref)
	cr.Status.OutputMIME = out.MIME
	cr.Status.OutputSize = int64(len(out.Bytes))
	cr.Status.OutputFilename = out.Filename
	cr.Status.Warnings = toCRWarnings(out.Warnings)
	cr.Status.FinishedAt = &finished
	conditions.SetTrue(cr, &cr.Status.Conditions,
		spiceboxv1alpha1.ArtifactRenderConditionReady,
		spiceboxv1alpha1.ReasonArtifactRenderRendered)
	return ctrl.Result{}, r.Client.Status().Update(ctx, cr)
}

// markFailed transitions to Failed with reason+msg and persists in a
// single Status().Update.
func (r *Reconciler) markFailed(ctx context.Context, cr *spiceboxv1alpha1.ArtifactRender, reason, msg string) (ctrl.Result, error) {
	finished := metav1.NewTime(time.Now())
	cr.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseFailed
	cr.Status.FailureReason = reason
	cr.Status.FailureMessage = msg
	cr.Status.FinishedAt = &finished
	conditions.SetFalse(cr, &cr.Status.Conditions,
		spiceboxv1alpha1.ArtifactRenderConditionReady, reason, msg)
	return ctrl.Result{}, r.Client.Status().Update(ctx, cr)
}

// readPayload returns the input bytes from inline Spec.Payload or by
// fetching Spec.PayloadRef from the artifactstore. Exactly one must be
// set.
func (r *Reconciler) readPayload(ctx context.Context, cr *spiceboxv1alpha1.ArtifactRender) ([]byte, error) {
	if len(cr.Spec.Payload) > 0 && cr.Spec.PayloadRef != "" {
		return nil, errors.New("spec: only one of payload or payloadRef may be set")
	}
	if len(cr.Spec.Payload) > 0 {
		return cr.Spec.Payload, nil
	}
	if cr.Spec.PayloadRef != "" {
		rc, err := r.Store.Get(ctx, artifactstore.Ref(cr.Spec.PayloadRef))
		if err != nil {
			return nil, fmt.Errorf("artifactstore.Get: %w", err)
		}
		defer rc.Close()
		buf := &bytes.Buffer{}
		if _, err := buf.ReadFrom(rc); err != nil {
			return nil, fmt.Errorf("read payloadRef: %w", err)
		}
		return buf.Bytes(), nil
	}
	return nil, errors.New("spec: payload and payloadRef both empty")
}

func (r *Reconciler) absoluteCap() int64 {
	if r.MaxOutputAbsolute <= 0 {
		return defaultMaxOutputAbsolute
	}
	return r.MaxOutputAbsolute
}

// finalize deletes the rendered output bytes from the artifactstore
// (and PayloadRef bytes if any), then drops the finalizer.
func (r *Reconciler) finalize(ctx context.Context, cr *spiceboxv1alpha1.ArtifactRender) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, spiceboxv1alpha1.FinalizerArtifactRender) {
		return ctrl.Result{}, nil
	}
	if cr.Status.OutputRef != "" {
		if err := r.Store.Delete(ctx, artifactstore.Ref(cr.Status.OutputRef)); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete output bytes: %w", err)
		}
	}
	if cr.Spec.PayloadRef != "" {
		if err := r.Store.Delete(ctx, artifactstore.Ref(cr.Spec.PayloadRef)); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete payload bytes: %w", err)
		}
	}
	controllerutil.RemoveFinalizer(cr, spiceboxv1alpha1.FinalizerArtifactRender)
	if err := r.Client.Update(ctx, cr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller into mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.ArtifactRender{}).
		Complete(r)
}
