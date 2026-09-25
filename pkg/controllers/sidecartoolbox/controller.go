// Package sidecartoolbox reconciles SidecarToolbox CRs: validates the spec
// (sandbox class, provider ref, CEL constraints), then runs a one-shot
// probe Pod to confirm the sidecar image is reachable and its live tool
// list matches the declared allowlist.
package sidecartoolbox

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// declaredImageRef returns the image ref to pin for the given CR: Source.Image
// when set, or Source.Inline.BaseImage for inline sources.
func declaredImageRef(cr *spiceboxv1alpha1.SidecarToolbox) string {
	if cr.Spec.Source.Image != "" {
		return cr.Spec.Source.Image
	}
	if cr.Spec.Source.Inline != nil {
		return cr.Spec.Source.Inline.BaseImage
	}
	return ""
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sidecartoolboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch

// Reconciler reconciles SidecarToolbox objects.
type Reconciler struct {
	Client client.Client
	// APIReader is an UNCACHED reader (mgr.GetAPIReader()). The reachability
	// probe uses it to read the probe Pod, because the informer cache that backs
	// Client can lag badly on a loaded cluster — observed at ~30s on an
	// oap-desktop VM. Reading the just-created, fast-changing probe Pod from the
	// cache returned stale state (a NotFound right after Create, then a stale
	// "Ready + podIP" for a Pod that was already terminating), which failed the
	// probe with "vanished" and then "connection refused" to a dead IP. An
	// uncached read reflects the live API-server state. nil falls back to Client
	// (unit tests that use a single fake store).
	APIReader          client.Reader
	RevalidateInterval time.Duration // 0 → 5m
	// ProbeNetpol configures admission-probe NetworkPolicy stamping (see
	// ProbeNetpolConfig). Zero value = disabled, mirroring the per-session
	// policies' opt-in.
	ProbeNetpol ProbeNetpolConfig
	// ConfigMapReader is the guarded ConfigMap reader. Injected from main.go.
	// Currently SidecarToolbox does not read CR-referenced ConfigMaps, but the
	// field is wired so that when such a read is added the guard is in place
	// without a separate main.go plumbing change. Tests may leave this nil.
	ConfigMapReader *adoptguard.ConfigMapReader
	// SkipProbe disables the live probe-Pod step. Set to true in unit tests
	// that use a fake client (which cannot simulate Pod scheduling).
	// Production leaves this false.
	SkipProbe bool
	// ProbeFunc overrides the default probeOnce implementation. When non-nil
	// it is called instead of r.probeOnce. Used in tests to inject a fixed
	// probe result (including a resolved digest) without running a real Pod.
	// Production leaves this nil.
	ProbeFunc func(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) (ProbeResult, error)
	// dialProbe overrides how probeOnce lists tools from a Ready probe Pod's MCP
	// endpoint (the inner dial, not the whole probe). Tests inject it to simulate
	// the fresh-pod window — a Ready Pod whose endpoint refuses the first dial
	// then accepts a retry — without a live kubelet or CNI. Production leaves it
	// nil → dialAndList builds a real probe.Client.
	dialProbe func(ctx context.Context, url string) ([]probe.Tool, error)
	// RevokePublisher, when non-nil, receives tool-origin revocation events
	// on CR deletion so running sessions deny that origin immediately.
	// Best-effort: a nil publisher is tolerated for local-dev / no-NATS runs.
	RevokePublisher *revocation.Publisher
}

func (r *Reconciler) interval() time.Duration {
	if r.RevalidateInterval == 0 {
		return 5 * time.Minute
	}
	return r.RevalidateInterval
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr spiceboxv1alpha1.SidecarToolbox
	if err := r.Client.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: refuse while any AgentClass in the same namespace
	// references this CR. Operators must edit those AgentClasses to drop
	// the ref before deletion can proceed.
	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}

	// Add the finalizer if missing.
	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &cr, spiceboxv1alpha1.FinalizerSidecarToolbox); added || err != nil {
		return ctrl.Result{Requeue: added}, err
	}

	// secretGated tracks only the SecretInputs half of
	// agentsession.RunModeFor's separate-pod decision -- NOT
	// spec.Isolation=isolated, the other, independent reason RunModeFor also
	// returns RunModeSeparatePod. That is deliberate here, not an oversight:
	// this variable exists to decide whether to SKIP the admission-time probe,
	// and the reason to skip is specific to the gating secret's absence at
	// admission time (the real pod lives in the workload namespace the
	// operator's egress is deliberately locked out of, and a secret-less
	// throwaway probe would validate a fiction anyway) -- a reason Isolation
	// alone does not share, since an isolated-but-not-secret-gated toolbox's
	// image is fully reachable at admission time. A SidecarToolbox with any
	// SecretInputs runs as a per-session SEPARATE POD, not injected into the
	// agent pod. Reachability + tool discovery are instead the RUNNER's job
	// against the live per-session pod (its per-session NetworkPolicy already
	// permits runner→sidecar; results land on
	// AgentSession.status.sidecarReachability). So for secret-gated sidecars
	// the probe/pin phases are skipped and the Reachable condition is set
	// Unknown/DeferredToSession — a distinct, non-failure state the admin UI
	// renders differently from a real ProbeFailed.
	secretGated := len(cr.Spec.SecretInputs) > 0

	// Status as read, for finalize's set-on-meaningful-change stamp of
	// LastValidatedAt.
	statusBefore := *cr.Status.DeepCopy()

	classCheck := func(ctx context.Context) apreconcile.Outcome {
		if cr.Spec.Sandbox.Class == "" {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionValid,
				spiceboxv1alpha1.ReasonSidecarToolboxClassMissing,
				"sandbox.class is required")
			return apreconcile.StopAfter()
		}
		var cls spiceboxv1alpha1.SpiceboxClass
		if err := r.Client.Get(ctx, client.ObjectKey{Name: cr.Spec.Sandbox.Class}, &cls); err != nil {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionValid,
				spiceboxv1alpha1.ReasonSidecarToolboxClassMissing,
				fmt.Sprintf("SpiceboxClass %q not found", cr.Spec.Sandbox.Class))
			return apreconcile.StopAfter()
		}
		valid := false
		for _, c := range cls.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxClassConditionValid && c.Status == metav1.ConditionTrue {
				valid = true
				break
			}
		}
		if !valid {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionValid,
				spiceboxv1alpha1.ReasonSidecarToolboxClassInvalid,
				fmt.Sprintf("SpiceboxClass %q is not Valid=True", cr.Spec.Sandbox.Class))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	providerCheck := func(ctx context.Context) apreconcile.Outcome {
		if cr.Spec.UpstreamAuth.Provider == "" {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionValid,
				spiceboxv1alpha1.ReasonSidecarToolboxProviderMissing,
				"upstreamAuth.provider is required")
			return apreconcile.StopAfter()
		}
		// UpstreamAuthProviderNone is the explicit sentinel for a
		// controller-issued-token sidecar (see the constant's doc): its
		// credential is never resolved from the /providers/ library, so it has
		// no entry there and the ByID lookup below must not run for it.
		if cr.Spec.UpstreamAuth.Provider == spiceboxv1alpha1.UpstreamAuthProviderNone {
			// ...and because nothing ever mints one for it, an envVar here is a
			// self-contradiction that would otherwise dead-end at session boot:
			// resolveSidecarCredential's short-circuit needs an EMPTY envVar, so
			// a non-empty one sends it looking for an AgentIdentity credential
			// that setup-identity structurally never creates for this sentinel —
			// failing the session with "run `oap agent setup-identity`", advice
			// that cannot possibly help. Refuse it here, where the author can
			// still act on it.
			if cr.Spec.UpstreamAuth.EnvVar != "" {
				conditions.SetFalse(&cr, &cr.Status.Conditions,
					spiceboxv1alpha1.SidecarToolboxConditionValid,
					spiceboxv1alpha1.ReasonSidecarToolboxProviderMissing,
					fmt.Sprintf("upstreamAuth.envVar must be empty when upstreamAuth.provider is %q "+
						"(that sentinel means the credential is controller-issued, so no library credential is ever resolved into an env var)",
						spiceboxv1alpha1.UpstreamAuthProviderNone))
				return apreconcile.StopAfter()
			}
			return apreconcile.Continue()
		}
		if _, ok := provider.ByID(cr.Spec.UpstreamAuth.Provider); !ok {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionValid,
				spiceboxv1alpha1.ReasonSidecarToolboxProviderMissing,
				fmt.Sprintf("provider %q not in /providers/ library", cr.Spec.UpstreamAuth.Provider))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	celCheck := func(ctx context.Context) apreconcile.Outcome {
		for ti, t := range cr.Spec.Tools {
			for ci, c := range t.Args.Constraints {
				if _, err := toolspeccel.Compile(c.CEL); err != nil {
					conditions.SetFalse(&cr, &cr.Status.Conditions,
						spiceboxv1alpha1.SidecarToolboxConditionValid,
						spiceboxv1alpha1.ReasonSidecarToolboxConstraintCompileError,
						fmt.Sprintf("tools[%d].args.constraints[%d]: %v", ti, ci, err))
					return apreconcile.StopAfter()
				}
			}
		}
		return apreconcile.Continue()
	}

	// Phase-local state shared between probePhase, pinCheck, and probeGate.
	var (
		probed       bool
		probedDigest string // kubelet-resolved image digest from the probe pod
	)

	probePhase := func(ctx context.Context) apreconcile.Outcome {
		if secretGated {
			// Do not probe: mark reachability deferred to the runner and let the
			// chain finalize Valid=True. Status=Unknown (not False) so this reads
			// as "pending per-session verification", not a failure.
			conditions.Set(&cr, &cr.Status.Conditions, metav1.Condition{
				Type:    spiceboxv1alpha1.SidecarToolboxConditionReachable,
				Status:  metav1.ConditionUnknown,
				Reason:  spiceboxv1alpha1.ReasonSidecarToolboxDeferredToSession,
				Message: "secret-gated sidecar: reachability + tool discovery are verified per-session by the runner against the live pod (see AgentSession.status.sidecarReachability)",
			})
			return apreconcile.Continue()
		}
		if r.SkipProbe {
			return apreconcile.Continue()
		}
		doProbe := r.probeOnce
		if r.ProbeFunc != nil {
			doProbe = r.ProbeFunc
		}
		res, err := doProbe(ctx, &cr)
		if err != nil {
			// Mark Reachable=False and continue so pinCheck can set
			// PinVerifyFailed when a prior baseline exists. probeGate (the
			// phase immediately after pinCheck) then stops the chain, so a
			// failed probe still never reaches finalize.
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionReachable,
				spiceboxv1alpha1.ReasonSidecarToolboxProbeFailed, err.Error())
			return apreconcile.Continue()
		}
		probed = true
		probedDigest = res.Digest
		names := make([]string, 0, len(res.Tools))
		for _, t := range res.Tools {
			names = append(names, t.Name)
		}
		cr.Status.ObservedTools = sortNames(names)
		obs := map[string]bool{}
		for _, n := range names {
			obs[n] = true
		}
		var missing []string
		for _, t := range cr.Spec.Tools {
			if !obs[t.Name] {
				missing = append(missing, t.Name)
			}
		}
		if len(missing) > 0 {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.SidecarToolboxConditionReachable,
				spiceboxv1alpha1.ReasonSidecarToolboxAllowlistDrift,
				fmt.Sprintf("allowlist names missing on server: %v", missing))
			return apreconcile.StopAfter()
		}
		conditions.SetTrue(&cr, &cr.Status.Conditions,
			spiceboxv1alpha1.SidecarToolboxConditionReachable,
			spiceboxv1alpha1.ReasonSidecarToolboxProbeOK)
		return apreconcile.Continue()
	}

	// pinCheck verifies the kubelet-resolved image digest against the recorded
	// baseline and maintains status.Pin + the PinDrift condition.
	//
	// Condition polarity: PinDrift is True=healthy (no drift / verified),
	// False=problem. This matches the Valid/Reachable idiom: True means
	// "everything is fine", False means "something requires attention".
	// Reason PinMatch carries the healthy True; PinDrifted and PinVerifyFailed
	// carry False.
	//
	// pinCheck runs after probePhase regardless of whether the probe succeeded
	// (!probed means probePhase set Reachable=False and continued) — that is
	// what lets a failed probe still report PinVerifyFailed against a prior
	// baseline. probeGate, the phase immediately after, then stops the chain
	// when !probed.
	pinCheck := func(ctx context.Context) apreconcile.Outcome {
		if r.SkipProbe || secretGated {
			// No probe pod was run (SkipProbe, or secret-gated where probing is
			// deferred to the runner), so there is no kubelet-resolved digest to
			// pin here. For secret-gated sidecars the per-session image pin is
			// recorded by the agentsession controller in status.observedPins at
			// session start; admission-time pin work is skipped.
			return apreconcile.Continue()
		}

		ref := declaredImageRef(&cr)
		if ref == "" {
			// No image ref declared (neither source.image nor inline.baseImage) —
			// nothing to pin.
			return apreconcile.Continue()
		}

		if !probed {
			// Probe failed — set PinDrift=False only when there's a prior
			// baseline or frozen assertion to compare against (TOFU: no baseline
			// = nothing to report yet).
			if cr.Status.Pin != nil {
				conditions.SetFalse(&cr, &cr.Status.Conditions,
					spiceboxv1alpha1.PinDriftCondition,
					spiceboxv1alpha1.ReasonPinVerifyFailed,
					"probe failed; kubelet-resolved image digest unknown")
			}
			return apreconcile.Continue()
		}

		// Probe succeeded but kubelet has not yet populated the image digest
		// (status lag or digestless imageID). If we have a prior baseline or a
		// frozen ref to assert against, we cannot verify — surface as
		// PinVerifyFailed. Without a baseline there is nothing to compare yet;
		// leave the PinDrift condition untouched and log at Info.
		if probedDigest == "" {
			logger := log.FromContext(ctx)
			_, _, declaredDigest := imagepin.SplitRef(ref)
			hasFrozenAssertion := declaredDigest != ""
			if cr.Status.Pin != nil || hasFrozenAssertion {
				conditions.SetFalse(&cr, &cr.Status.Conditions,
					spiceboxv1alpha1.PinDriftCondition,
					spiceboxv1alpha1.ReasonPinVerifyFailed,
					"probe pod reported no image digest")
			} else {
				logger.Info("sidecartoolbox: probe returned no image digest; skipping pin check",
					"toolbox", cr.Name)
			}
			return apreconcile.Continue()
		}

		// Refreeze annotation handling: when the operator has set the
		// pin-refreeze annotation, try to re-freeze the baseline to the
		// annotated digest. Must happen BEFORE computeImagePin so that a
		// just-honored refreeze sets PinMatch rather than drift.
		if refreezeVal, hasRefreeze := cr.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]; hasRefreeze && refreezeVal != "" {
			if refreezeVal == probedDigest {
				// Check for conflicting frozen spec ref (declaredDigest takes precedence).
				_, _, declaredDigest := imagepin.SplitRef(ref)
				if declaredDigest != "" && declaredDigest != probedDigest {
					// Spec assertion conflicts: reject; leave annotation so the user
					// sees it pending. Drift message explains the assertion wins.
					conditions.SetFalse(&cr, &cr.Status.Conditions,
						spiceboxv1alpha1.PinDriftCondition,
						spiceboxv1alpha1.ReasonPinDrifted,
						fmt.Sprintf("image digest drifted: resolved %s; refreeze annotation to %s not honored: "+
							"declared frozen ref %s assertion wins; edit spec to accept",
							probedDigest, refreezeVal, declaredDigest))
					return apreconcile.Continue()
				}
				// Honored: clear the annotation via a metadata Update BEFORE
				// mutating status. This updates cr.ResourceVersion so the
				// subsequent RunPhases Status().Update won't conflict.
				delete(cr.Annotations, spiceboxv1alpha1.AnnotationPinRefreeze)
				if err := r.Client.Update(ctx, &cr); err != nil {
					return apreconcile.FailWith(fmt.Errorf("clearing pin-refreeze annotation: %w", err))
				}
				// Rewrite the pin baseline with a fresh ObservedAt.
				ik := &imagepin.Kind{}
				parsedRef, parseErr := ik.ParseRef(ref)
				if parseErr != nil {
					conditions.SetFalse(&cr, &cr.Status.Conditions,
						spiceboxv1alpha1.PinDriftCondition,
						spiceboxv1alpha1.ReasonPinVerifyFailed,
						fmt.Sprintf("image ref parse error: %v", parseErr))
					return apreconcile.Continue()
				}
				_, tag, _ := imagepin.SplitRef(ref)
				now := metav1.Now()
				cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
					Kind:       imagepin.KindName,
					Strength:   string(parsedRef.Strength),
					Digest:     probedDigest,
					Version:    tag,
					ObservedAt: &now,
					Details:    map[string]string{"declaredRef": ref},
				}
				conditions.SetTrue(&cr, &cr.Status.Conditions,
					spiceboxv1alpha1.PinDriftCondition, spiceboxv1alpha1.ReasonPinMatch)
				return apreconcile.Continue()
			}
			// Stale refreeze: live moved; fall through to normal drift handling.
			// The stale refreeze message will be appended below.
		}

		newRec, drifted, summary, err := computeImagePin(ref, probedDigest, cr.Status.Pin)
		if err != nil {
			// ParseRef failure — surface as PinVerifyFailed (not dropped).
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition,
				spiceboxv1alpha1.ReasonPinVerifyFailed,
				fmt.Sprintf("image ref parse error: %v", err))
			return apreconcile.Continue()
		}

		if drifted {
			// If there's a stale refreeze annotation (value != probedDigest), append a note.
			if refreezeVal, hasRefreeze := cr.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]; hasRefreeze && refreezeVal != "" {
				summary += fmt.Sprintf("; refreeze to %s not honored: live identity is %s", refreezeVal, probedDigest)
			}
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.PinDriftCondition,
				spiceboxv1alpha1.ReasonPinDrifted, summary)
			// Record the live digest machine-readably so `oap pin update --current`
			// can accept drift without re-probing. Guard is load-bearing: on a
			// frozen-assert first-observe the pin may be nil and we must not panic.
			if cr.Status.Pin != nil {
				if cr.Status.Pin.Details == nil {
					cr.Status.Pin.Details = make(map[string]string)
				}
				cr.Status.Pin.Details["observedDriftDigest"] = probedDigest
			}
			// Baseline is NOT auto-updated on drift — operator must explicitly accept.
			return apreconcile.Continue()
		}

		if newRec != nil {
			cr.Status.Pin = newRec
		}
		conditions.SetTrue(&cr, &cr.Status.Conditions,
			spiceboxv1alpha1.PinDriftCondition, spiceboxv1alpha1.ReasonPinMatch)
		return apreconcile.Continue()
	}

	// probeGate stops the phase chain when the probe did not succeed, so
	// finalize never stamps Valid=True on a toolbox whose image could not be
	// reached. It is a separate phase rather than a StopAfter inside probePhase
	// because pinCheck, which runs between them, needs the failed-probe case to
	// reach it in order to report PinVerifyFailed against a prior baseline.
	probeGate := func(_ context.Context) apreconcile.Outcome {
		// Secret-gated sidecars are never probed (probed stays false by design),
		// so they must NOT be stopped here — the deferred-reachability path is a
		// success path that finalizes Valid=True.
		if !r.SkipProbe && !probed && !secretGated {
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	finalize := func(ctx context.Context) apreconcile.Outcome {
		cr.Status.ObservedGeneration = cr.Generation
		conditions.SetTrue(&cr, &cr.Status.Conditions,
			spiceboxv1alpha1.SidecarToolboxConditionValid,
			spiceboxv1alpha1.ReasonSidecarToolboxSpecOK)
		// Hold LastValidatedAt at its prior value, then let StampIfMoved bump it
		// only when this pass observed something new. conditions.Set* is already
		// dedupe-on-equal-state, so the clock is the sole per-pass churn.
		cr.Status.LastValidatedAt = statusBefore.LastValidatedAt
		apreconcile.StampIfMoved(statusBefore, cr.Status, &cr.Status.LastValidatedAt)
		return apreconcile.Continue()
	}

	phases := []apreconcile.Phase{classCheck, providerCheck, celCheck, probePhase, pinCheck, probeGate, finalize}
	if err := apreconcile.RunPhases(ctx, r.Client, &cr, phases); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SidecarToolbox{}).
		Watches(&spiceboxv1alpha1.SpiceboxClass{}, handler.EnqueueRequestsFromMapFunc(r.mapClassToToolboxes)).
		Complete(r); err != nil {
		return err
	}
	if r.RevokePublisher != nil {
		inf, err := mgr.GetCache().GetInformer(context.Background(), &spiceboxv1alpha1.SidecarToolbox{})
		if err != nil {
			return err
		}
		revokeLog := ctrl.Log.WithName("sidecartoolbox-revoke")
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj any) {
				cr := SidecarToolboxFromDelete(obj)
				if cr == nil {
					return
				}
				key, scope := SidecarToolboxRevokeKeyScope(cr)
				if emitErr := r.RevokePublisher.Emit(context.Background(), "tool-origin", key, scope); emitErr != nil {
					revokeLog.Info("emit tool-origin revoke on delete failed", "toolbox", cr.Name, "err", emitErr.Error())
				}
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// mapClassToToolboxes re-enqueues every SidecarToolbox whose
// spec.sandbox.class names the changed SpiceboxClass.
//
// classCheck gates the toolbox on the class's Valid condition, so a bundle
// that applies the SpiceboxClass and the SidecarToolbox in the same instant
// can land the toolbox at Valid=False reason=ClassInvalid before the class
// controller has stamped Valid=True. That state is only cleared by the
// reconciler's RevalidateInterval (5m), and because AgentClass mirrors the
// toolbox's Valid condition the whole agent reads "not ready" — pointing at
// a SpiceboxClass that is already Valid=True — for that entire window.
// SpiceboxClass is cluster-scoped, so list toolboxes across all namespaces.
func (r *Reconciler) mapClassToToolboxes(ctx context.Context, o client.Object) []reconcile.Request {
	var list spiceboxv1alpha1.SidecarToolboxList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("list SidecarToolboxes for SpiceboxClass watch failed; dropping re-enqueue (self-heals on next revalidate)",
			"spiceboxclass", o.GetName(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.Sandbox.Class == o.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

// finalize blocks deletion while any AgentClass in the same namespace
// references this CR via spec.sidecarToolboxes. Operators must drop the
// ref from each referencing AgentClass before deletion can proceed. The
// check is deliberately conservative — we only consult AgentClass refs,
// not transitively through running AgentSessions, because every running
// session was admitted via an AgentClass that already references the CR
// and the AgentClass-level guard is the canonical contract.
func (r *Reconciler) finalize(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, spiceboxv1alpha1.FinalizerSidecarToolbox) {
		// Nothing to do — finalizer was never added or already removed.
		return ctrl.Result{}, nil
	}
	var classes spiceboxv1alpha1.AgentClassList
	if err := r.Client.List(ctx, &classes, client.InNamespace(cr.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list AgentClasses: %w", err)
	}
	var blockers []string
	for _, ac := range classes.Items {
		for _, ref := range ac.Spec.SidecarToolboxes {
			if ref.Ref == cr.Name {
				blockers = append(blockers, ac.Name)
				break
			}
		}
	}
	if len(blockers) > 0 {
		conditions.SetFalse(cr, &cr.Status.Conditions,
			spiceboxv1alpha1.SidecarToolboxConditionValid,
			spiceboxv1alpha1.ReasonSidecarToolboxDeletionBlocked,
			fmt.Sprintf("deletion blocked: referenced by AgentClass(es) %v", blockers))
		if err := r.Client.Status().Update(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue so we re-check after admins edit the referencing classes.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(cr, spiceboxv1alpha1.FinalizerSidecarToolbox)
	if err := r.Client.Update(ctx, cr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
