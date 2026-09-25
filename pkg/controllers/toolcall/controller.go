// Package toolcall implements the ToolCall controller.
//
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;create
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=toolcalls,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=toolcalls/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=toolcalls/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxsessions,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
package toolcall

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	goerrors "errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	toolspecregistry "github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// TokenChecker is the subset of *spicedb.Client the ToolCall reconciler
// needs to enforce durable per-exec token-use authorization ahead of every
// sandbox exec. Declared as an interface — not *spicedb.Client — per the
// repo's typed-nil rule (AGENTS.md) and so tests can inject a fake. Mirrors
// pkg/agent/tool/mcp.TokenChecker's shape exactly; redeclared locally rather
// than imported because this package has no other reason to depend on the
// agent-side mcp tool package.
type TokenChecker interface {
	CheckUseToken(ctx context.Context, ns, name, credID, presentedValueHash string, fullyConsistent bool) (bool, error)
}

// Reconciler reconciles ToolCall objects.
type Reconciler struct {
	Client client.Client
	Scheme *apiruntime.Scheme

	// Broker resolves credential descriptors stamped on tc.Spec.Credentials
	// into injectable env vars for the tool exec.
	Broker broker.Broker

	// TokenChecker performs the durable per-exec use_token authorization
	// check against SpiceDB before every sandbox exec that presents a
	// resolved credential — the sandbox-path counterpart to the MCP runner's
	// pre-send check (pkg/agent/tool/mcp/dispatch.go). Nil in test fixtures
	// / when SpiceDB is unconfigured: the check is skipped, mirroring how
	// agentsession.Reconciler.TokenGranter (the grant-writer this check
	// verifies against) is itself nil-gated — if grants were never written,
	// checking against them would be meaningless. Wired to *spicedb.Client
	// in internal/cmd/operator/main.go.
	TokenChecker TokenChecker

	// Runtimes resolves a session's sandbox handle to a bound exec transport
	// (executorFor). One entry per sandbox kind linked into this binary; a
	// kind with no entry, or a session with no handle yet, fails the tool
	// call rather than falling back to a default backend.
	Runtimes sandboxkinds.Runtimes

	// Store receives stdout/stderr payloads.
	Store artifactstore.Store

	// Registry parks active streaming execs for the gateway to read.
	Registry *gateway.Registry
	// GatewayEndpoint is the "host:port" (cluster-internal) clients use to reach
	// the gRPC Gateway service. Surfaced in ToolCall.status.streaming.
	GatewayEndpoint string

	// ToolkitRegistry resolves SpiceboxToolkits (built-ins + CRs) for the
	// toolspec validation step.
	ToolkitRegistry *toolspecregistry.Registry

	// APIReader bypasses the cache for toolspec status lookups, avoiding cache
	// lag between the spiceboxtoolspec reconciler writing Valid=True and the
	// toolcall reconciler seeing it.  Set to mgr.GetAPIReader() in production
	// and tests.  Falls back to r.Client when nil (unit-test convenience).
	APIReader client.Reader

	// Snapshotter takes JIT workspace snapshots before stateful
	// tool dispatches. Required when ToolCall.spec.preDispatchSnapshot
	// is set; if nil and a ToolCall arrives with the field, reconcile
	// fails with a clear error.
	Snapshotter workspace.Snapshotter

	// RecordSnapshotFn writes the tool_dispatch_snapshot memory
	// audit entry once a snapshot Job succeeds. Wired by the operator
	// at start-up (closes over the memory.Memory client). Nil-safe:
	// if unset, audit recording is skipped (test fixtures may omit it).
	RecordSnapshotFn RecordSnapshotFn

	// Now returns the current time. Tests inject a fixed clock.
	Now func() time.Time
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.ToolCall{}).
		Complete(r)
}

// Reconcile drives one ToolCall and makes sure a failure that repeats stops
// being invisible: every error the body returns goes through
// surfaceReconcileFailure, which records it on the ToolCall and finishes the
// call once the failures outlast ReconcileFailureBudget. A caller blocked on
// this ToolCall is then answered with a reason instead of waiting forever.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	if err != nil {
		return r.surfaceReconcileFailure(ctx, req.NamespacedName, res, err)
	}
	r.clearReconcileFailure(ctx, req.NamespacedName)
	return res, nil
}

func (r *Reconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tc spiceboxv1alpha1.ToolCall
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &tc); !cont {
		return ctrl.Result{}, err
	}

	if !tc.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&tc, spiceboxv1alpha1.FinalizerToolCall) {
			return ctrl.Result{}, nil
		}

		// If the ToolCall is mid-execution (Running=True and no terminal), stamp Canceled.
		// Plan 2 cannot actually interrupt the in-flight exec for sync mode; the synchronous
		// Reconcile that launched it will complete and race with the finalizer. The terminal-
		// condition check at the top of the main reconcile path prevents double-writing.
		//
		// For interactive ToolCalls, we CAN stop the exec: the ActiveStream registered in
		// reconcileStreaming carries the exec context's CancelFunc. Cancelling unblocks
		// watchStreamCompletion, which finalizes the status via a conflict-retry update.
		if hasTrueCondition(&tc, spiceboxv1alpha1.ToolCallConditionRunning) &&
			terminalCondition(&tc) == "" {
			if tc.Spec.Mode == spiceboxv1alpha1.ToolCallModeInteractive && r.Registry != nil {
				r.Registry.CancelAndUnregister(tc.Namespace, tc.Name)
			}
			conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
				Type: spiceboxv1alpha1.ToolCallConditionCanceled, Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonUserDelete, Message: "ToolCall deleted while running",
			})
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			if err := r.Client.Status().Update(ctx, &tc); err != nil && !errors.IsConflict(err) {
				return ctrl.Result{}, err
			}
		}

		controllerutil.RemoveFinalizer(&tc, spiceboxv1alpha1.FinalizerToolCall)
		if err := r.Client.Update(ctx, &tc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer. Unlike the other reconcilers this does NOT requeue
	// after adding the finalizer — the subsequent watch event re-enters.
	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &tc, spiceboxv1alpha1.FinalizerToolCall); added || err != nil {
		return ctrl.Result{}, err
	}

	// Short-circuit once the ToolCall has reached a terminal condition.
	if terminalCondition(&tc) != "" {
		return ctrl.Result{}, nil
	}

	// Validate.
	resolved, failReason, err := r.validate(ctx, &tc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if resolved == nil {
		r.setFailed(&tc, failReason, humanMessage(failReason))
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}
	r.setValidated(&tc)

	// Validate spec.env: reject secret-like keys and warn on high-entropy values.
	if reason, key := firstSecretLikeKey(tc.Spec.Env); reason != "" {
		r.setFailed(&tc, reason, fmt.Sprintf("spec.env[%s] looks like a secret; use an AgentIdentity binding", key))
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}
	if highEntropy := highEntropyKeys(tc.Spec.Env); len(highEntropy) > 0 {
		conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.ToolCallConditionEnvHighEntropy,
			Status:  metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ToolCallConditionEnvHighEntropy,
			Message: fmt.Sprintf("env values for %v look high-entropy; use an AgentIdentity binding for secrets", highEntropy),
		})
		// Don't fail; this is a warning. Persist the condition before continuing.
		if err := r.Client.Status().Update(ctx, &tc); err != nil {
			return ctrl.Result{}, err
		}
	}

	acceptedBy, failures, err := r.validateToolspec(ctx, &tc, resolved.session, resolved)
	if err != nil {
		if goerrors.Is(err, errToolspecNotReady) {
			// Toolspec candidates exist but are not yet Valid (cache lag or
			// still being reconciled). Requeue to try again shortly.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if acceptedBy == "" {
		tc.Status.Toolspec = &spiceboxv1alpha1.ToolspecValidation{Failures: failures}
		r.setFailed(&tc, spiceboxv1alpha1.ReasonToolspecDenied,
			summarizeFailures(failures))
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}
	tc.Status.Toolspec = &spiceboxv1alpha1.ToolspecValidation{AcceptedBy: acceptedBy}

	// Resolve credentials via the broker. tc.Spec.Credentials is stamped by
	// the runner; when empty no credentials are injected.
	var agentEnv map[string]string
	if len(tc.Spec.Credentials) > 0 {
		// Defense-in-depth: the admission webhook already pins every credential
		// source to the ToolCall's own namespace AND binds every per-session
		// passthrough Secret to its owning session, but the broker holds
		// cluster-wide Secret read, so we re-check both here before it resolves
		// anything — a cross-namespace source, or a sibling session's
		// passthrough-credential Secret, must never reach the broker even if the
		// webhook was bypassed (disabled/failurePolicy drift).
		if err := tc.ValidateCredentialSourceNamespaces(); err != nil {
			r.setFailed(&tc, spiceboxv1alpha1.ReasonCredentialSourceForbidden, err.Error())
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
		}
		if err := tc.ValidateCredentialSourceOwnership(); err != nil {
			r.setFailed(&tc, spiceboxv1alpha1.ReasonCredentialSourceForbidden, err.Error())
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
		}
		res, berr := r.Broker.Resolve(ctx, broker.Request{Credentials: tc.Spec.Credentials})
		if berr != nil {
			r.setFailed(&tc, spiceboxv1alpha1.ReasonAgentCredentialUnresolved, berr.Error())
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
		}
		agentEnv = res.EnvVars

		// Collision check: tc.spec.env must not include any of the broker-injected keys.
		for k := range agentEnv {
			if _, exists := tc.Spec.Env[k]; exists {
				r.setFailed(&tc, spiceboxv1alpha1.ReasonToolCallEnvShadowsAgent,
					fmt.Sprintf("spec.env[%s] collides with a credential-injected key; rename or remove the conflicting key", k))
				finished := metav1.Now()
				tc.Status.FinishedAt = &finished
				return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
			}
		}

		// Build the masked status record from the injected env vars.
		injected := make([]spiceboxv1alpha1.InjectedEnvKey, 0, len(agentEnv))
		for k, v := range agentEnv {
			injected = append(injected, spiceboxv1alpha1.InjectedEnvKey{
				Key:    k,
				Masked: credmask.Mask(v),
			})
		}
		sort.Slice(injected, func(i, j int) bool { return injected[i].Key < injected[j].Key })
		tc.Status.Agent = &spiceboxv1alpha1.AgentInjection{
			Injected: injected,
		}
	}

	// Workspace snapshot: required when stateImpact ∈ {readwrite, external}.
	// The Snapshot Job must complete before the tool exec runs, so writes
	// during the dispatch can be later rewound by Restart-from-here.
	//
	// Skip it for an isolated (pod-local emptyDir) workspace: there is no shared
	// <session>-workspace PVC to snapshot, so the snapshot Job's pod would sit
	// unschedulable ("persistentvolumeclaim ...-workspace not found") forever and
	// block the tool call for 3m before erroring, every reconcile. The runner
	// sets PreDispatchSnapshot from a tool's StateImpact alone (it does not know
	// the workspace mode), so the coherence check belongs here, where the bundle
	// session's resolved workspace mode is known. Logged, not silent.
	if tc.Spec.PreDispatchSnapshot != nil && workspaceIsIsolated(resolved.session) {
		log.FromContext(ctx).Info(
			"toolcall: skipping pre-dispatch workspace snapshot; session has no shared workspace PVC to snapshot (isolated /work)",
			"toolcall", tc.Namespace+"/"+tc.Name,
			"workspaceMode", string(resolved.session.Spec.Workspace.Mode))
	}
	if tc.Spec.PreDispatchSnapshot != nil && !workspaceIsIsolated(resolved.session) {
		parentSessionName := resolved.session.Labels[agentSessionLabel]
		if parentSessionName == "" {
			r.setFailed(&tc, "PreDispatchSnapshotResolveParent",
				"bundle SpiceboxSession is missing the agentsession label; cannot record snapshot audit")
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
		}
		// The workspace PVC name is derived from the parent AgentSession's
		// name via podspec.WorkspaceClaimName. Construct a PVCRef.
		src := workspace.PVCRef{
			Namespace: tc.Namespace,
			Name:      podspec.WorkspaceClaimName(&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: parentSessionName}}),
		}
		if err := HandlePreDispatchSnapshot(ctx, r, &tc, src, parentSessionName); err != nil {
			return ctrl.Result{}, fmt.Errorf("preDispatchSnapshot launch: %w", err)
		}
		done, err := WaitPreDispatchSnapshot(ctx, r, &tc, src, parentSessionName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// Token-use authorization: before either exec path (sync or streaming) can
	// present a resolved credential, re-verify each one against SpiceDB's
	// durable use_token grant. This is the sandbox-path counterpart to the
	// MCP dispatch.go pre-send check — the operator's grant-writer
	// (reconcileCredentialGrants) authors the grants this call verifies
	// against, so a credential revoked mid-session (identity rebind,
	// credential rotation) is caught here even though the broker itself
	// never consults SpiceDB.
	if proceed, res, cerr := r.checkTokenUse(ctx, &tc, resolved, agentEnv); !proceed {
		return res, cerr
	}

	if tc.Spec.Mode == spiceboxv1alpha1.ToolCallModeStream ||
		tc.Spec.Mode == spiceboxv1alpha1.ToolCallModeInteractive {
		// Persist Validated=True before entering the streaming path, so the caller
		// sees it before status.streaming populates. Skip when already Running to
		// avoid a spurious status bump that would race with the watcher goroutine's
		// terminal-condition update.
		if !hasTrueCondition(&tc, spiceboxv1alpha1.ToolCallConditionRunning) {
			if err := r.Client.Status().Update(ctx, &tc); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.reconcileStreaming(ctx, &tc, resolved, agentEnv)
	}

	// If a previous reconcile had already marked Running=True (operator restart mid-exec),
	// we can't reattach to that stream. Mark Failed so the ToolCall finalizes.
	if hasTrueCondition(&tc, spiceboxv1alpha1.ToolCallConditionRunning) {
		r.setFailed(&tc, spiceboxv1alpha1.ReasonOperatorRestart, "operator restarted mid-execution")
		now := metav1.Now()
		tc.Status.FinishedAt = &now
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}

	// Mark Running + StartedAt, persist immediately so observers can see progress.
	r.setRunning(&tc)
	now := metav1.Now()
	tc.Status.StartedAt = &now
	if err := r.Client.Status().Update(ctx, &tc); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve the exec transport bound to this session's own sandbox. Never a
	// fallback to a default backend: a session with no handle yet, or one
	// whose kind has no runtime, fails the tool call outright.
	executor, err := r.executorFor(resolved.session)
	if err != nil {
		r.setFailed(&tc, spiceboxv1alpha1.ReasonSandboxUnresolved, err.Error())
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonExecClosed,
		})
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}

	// Hydrate input artifacts. If hydration fails, finalize as Failed.
	if err := r.hydrateInputs(ctx, resolved.session, string(tc.UID), executor, tc.Spec.InputArtifacts); err != nil {
		r.setFailed(&tc, spiceboxv1alpha1.ReasonHydrateFailed, err.Error())
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonExecClosed,
		})
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tc)
	}

	// Execute synchronously with deadline from spec.Timeout.
	timeout := tc.Spec.Timeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	callID := string(tc.UID)
	cmd := append([]string{}, resolved.tool.Command...)
	cmd = append(cmd, resolved.tool.DefaultArgs...)
	cmd = append(cmd, tc.Spec.Args...)

	execReq := exec.Request{
		Command: cmd,
		Env:     execEnv(agentEnv, &tc, resolved.session, resolved.toolkit),
	}
	if tc.Spec.Stdin != "" {
		execReq.Stdin = strings.NewReader(tc.Spec.Stdin)
	}

	result, execErr := executor.Exec(execCtx, execReq)

	// Persist stdout/stderr into the ArtifactStore regardless of error — partial captures are useful.
	stdoutKey := fmt.Sprintf("%s/%s/%s/stdout", tc.Namespace, resolved.session.Name, callID)
	stderrKey := fmt.Sprintf("%s/%s/%s/stderr", tc.Namespace, resolved.session.Name, callID)

	if len(result.Stdout) > 0 {
		ref, putErr := r.Store.Put(ctx, stdoutKey, bytes.NewReader(result.Stdout))
		if putErr != nil {
			logger.Error(putErr, "put stdout")
		} else {
			tc.Status.StdoutArtifactRef = string(ref)
		}
	}
	if len(result.Stderr) > 0 {
		ref, putErr := r.Store.Put(ctx, stderrKey, bytes.NewReader(result.Stderr))
		if putErr != nil {
			logger.Error(putErr, "put stderr")
		} else {
			tc.Status.StderrArtifactRef = string(ref)
		}
	}
	tc.Status.StdoutTruncated = result.StdoutTruncated
	tc.Status.StderrTruncated = result.StderrTruncated

	// Harvest output artifacts (best-effort: if harvest fails, ToolCall still completes
	// with exec's exit code but outputArtifacts is empty and an error is logged).
	if outputs, herr := r.harvestOutputs(ctx, resolved.session, &tc); herr != nil {
		logger.Error(herr, "harvest outputs")
	} else {
		tc.Status.OutputArtifacts = outputs
	}

	exitCode := result.ExitCode
	tc.Status.ExitCode = &exitCode
	finished := metav1.Now()
	tc.Status.FinishedAt = &finished

	// Clear Running=True before setting the terminal condition.
	conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonExecClosed,
	})

	switch {
	case goerrors.Is(execErr, context.DeadlineExceeded):
		r.setTimeout(&tc)
	case execErr != nil:
		r.setFailed(&tc, spiceboxv1alpha1.ReasonNonZeroExit, execErr.Error())
	case exitCode == 0:
		r.setSucceeded(&tc)
	default:
		r.setFailed(&tc, spiceboxv1alpha1.ReasonNonZeroExit, fmt.Sprintf("exit code %d", exitCode))
	}

	if err := r.Client.Status().Update(ctx, &tc); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.bumpSessionActivity(ctx, resolved.session); err != nil {
		logger.Error(err, "bump session activity")
	}

	logger.V(1).Info("toolcall complete", "exitCode", exitCode)
	return ctrl.Result{}, nil
}

// argsHashKeySecretDataKey is the per-session -memory-token Secret's data key
// holding the args-hash HMAC key that both the operator's grant-writer
// (agentsession.reconcileCredentialGrants) and this per-exec check key their
// presented-value hashes on. Mirrors agentSessionSecretArgsHashKey in
// pkg/controllers/agentsession/rbac.go — unexported there, duplicated as a
// literal here rather than importing that controller package, which this one
// has no other reason to depend on (agentsession is a peer controller
// package, not a shared library).
const argsHashKeySecretDataKey = "args-hash-key"

// agentSessionLabel is the label the bundle SpiceboxSession carries pointing
// at its parent AgentSession's name. Mirrors the literal used at the
// PreDispatchSnapshot site above (no exported constant exists for it today).
const agentSessionLabel = "agentprimitives.authzed.com/agentsession"

// checkTokenUse re-verifies every externaltoken-backed credential the
// ToolCall is about to hand to the sandbox exec against SpiceDB's durable
// use_token grant, immediately before both exec paths (sync and streaming).
// This is the sandbox-path counterpart to pkg/agent/tool/mcp/dispatch.go's
// pre-send check: the operator's grant-writer
// (agentsession.reconcileCredentialGrants) is the only thing that authors
// these grants, so a credential the operator revoked mid-session (identity
// rebind, credential rotation, a compromised-Secret response) is caught here
// even though the broker itself never consults SpiceDB.
//
// Returns proceed=false whenever the caller must stop and return the given
// (Result, error) immediately rather than continue to exec:
//   - r.TokenChecker == nil (SpiceDB unconfigured — test fixtures or a
//     deliberately SpiceDB-less deployment) or tc.Spec.Credentials is empty:
//     proceed=true, nothing to check.
//   - a definitive deny (CheckUseToken returns false, nil): the ToolCall is
//     marked Failed/ReasonAgentCredentialRevoked and its status persisted.
//     The session is left untouched — this is surgical, matching the MCP
//     path's "deny doesn't fail the session" contract.
//   - indeterminate (CheckUseToken errors, the parent AgentSession's name
//     can't be resolved from resolved.session's label, or the per-session
//     args-hash key can't be read): fail-closed. The ToolCall is marked
//     Failed, and — whenever the parent AgentSession's name IS known — the
//     AgentSession itself is also terminally failed with
//     ReasonAgentSessionTokenAuthzUnavailable via failParentAgentSession, so
//     a runner whose credentials can no longer be verified is not left
//     dispatching further calls.
func (r *Reconciler) checkTokenUse(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, resolved *resolvedCall, agentEnv map[string]string) (bool, ctrl.Result, error) {
	if r.TokenChecker == nil || len(tc.Spec.Credentials) == 0 {
		return true, ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	agentSessionName := resolved.session.Labels[agentSessionLabel]
	if agentSessionName == "" {
		// No parent AgentSession to check against — and none to fail either,
		// since we don't know its name. Fail closed on the ToolCall only,
		// mirroring the PreDispatchSnapshotResolveParent handling above.
		logger.Error(fmt.Errorf("bundle SpiceboxSession %s/%s is missing the %s label", resolved.session.Namespace, resolved.session.Name, agentSessionLabel),
			"toolcall use_token check: cannot resolve parent AgentSession; failing ToolCall",
			"toolCall", tc.Name, "namespace", tc.Namespace)
		r.setFailed(tc, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable,
			"bundle SpiceboxSession is missing the agentsession label; cannot verify token-use authorization")
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return false, ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	var keySecret corev1.Secret
	keySecretKey := types.NamespacedName{Namespace: tc.Namespace, Name: agentSessionName + spiceboxv1alpha1.MemoryTokenSecretSuffix}
	if err := r.Client.Get(ctx, keySecretKey, &keySecret); err != nil {
		logger.Error(err, "toolcall use_token check: read args-hash-key Secret failed; failing closed",
			"toolCall", tc.Name, "namespace", tc.Namespace, "secret", keySecretKey.Name)
		res, ferr := r.failIndeterminate(ctx, tc, tc.Namespace, agentSessionName,
			fmt.Sprintf("read args-hash-key Secret %s: %v", keySecretKey.Name, err))
		return false, res, ferr
	}
	argsHashKey := keySecret.Data[argsHashKeySecretDataKey]
	if len(argsHashKey) == 0 {
		logger.Error(fmt.Errorf("Secret %s missing data key %q", keySecretKey.Name, argsHashKeySecretDataKey),
			"toolcall use_token check: args-hash-key Secret data missing; failing closed",
			"toolCall", tc.Name, "namespace", tc.Namespace, "secret", keySecretKey.Name)
		res, ferr := r.failIndeterminate(ctx, tc, tc.Namespace, agentSessionName,
			fmt.Sprintf("Secret %s missing data key %q", keySecretKey.Name, argsHashKeySecretDataKey))
		return false, res, ferr
	}

	for _, d := range tc.Spec.Credentials {
		if d.Inject.EnvVar == "" {
			// Header-injected descriptors are the MCP path's concern
			// (pkg/agent/tool/mcp/dispatch.go already checks those); nothing
			// for the sandbox exec to present here.
			continue
		}
		credID := externaltoken.CredID(d.Source)
		presented := externaltoken.ValueHash(argsHashKey, agentEnv[d.Inject.EnvVar])
		allowed, err := r.TokenChecker.CheckUseToken(ctx, tc.Namespace, agentSessionName, credID, presented, true)
		if err != nil {
			logger.Error(err, "toolcall use_token check errored; failing closed",
				"toolCall", tc.Name, "namespace", tc.Namespace, "credID", credID)
			res, ferr := r.failIndeterminate(ctx, tc, tc.Namespace, agentSessionName, err.Error())
			return false, res, ferr
		}
		if !allowed {
			logger.Info("toolcall use_token check denied; failing ToolCall (session continues)",
				"toolCall", tc.Name, "namespace", tc.Namespace, "credID", credID, "envVar", d.Inject.EnvVar)
			r.setFailed(tc, spiceboxv1alpha1.ReasonAgentCredentialRevoked,
				fmt.Sprintf("credential for %s has been revoked", d.Inject.EnvVar))
			finished := metav1.Now()
			tc.Status.FinishedAt = &finished
			return false, ctrl.Result{}, r.Client.Status().Update(ctx, tc)
		}
	}
	return true, ctrl.Result{}, nil
}

// failIndeterminate handles a use_token check that could not be confirmed
// either way (a SpiceDB RPC error, or the args-hash key was unreadable): it
// fails the parent AgentSession with ReasonAgentSessionTokenAuthzUnavailable
// (so the runner is not left presenting further, unverifiable credentials),
// then fails this ToolCall with the same reason so the exec does not run.
// Logs loudly at every step per AGENTS.md's no-silent-errors rule; a failure
// to update the AgentSession's status is returned directly (Reconcile will
// retry) rather than falling through to the ToolCall write, since leaving the
// session live with no authz confirmation is the worse failure mode.
func (r *Reconciler) failIndeterminate(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, ns, agentSessionName, detail string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	msg := fmt.Sprintf("token-use authorization unavailable: %s", detail)
	if err := r.failParentAgentSession(ctx, ns, agentSessionName, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, msg); err != nil {
		logger.Error(err, "toolcall use_token check: failing parent AgentSession errored",
			"agentSession", agentSessionName, "namespace", ns)
		return ctrl.Result{}, err
	}
	r.setFailed(tc, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, msg)
	finished := metav1.Now()
	tc.Status.FinishedAt = &finished
	return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
}

// failParentAgentSession terminally fails the named AgentSession
// (Phase=Failed, FailureReason=reason, a Failed=True condition), via an
// optimistic-lock retried status merge patch — the same fresh-Get + patch
// idiom bumpSessionActivity below already uses to safely touch a sibling
// resource's status without clobbering concurrent writers. Idempotent: a
// session already in a terminal phase, or already gone, is left alone.
func (r *Reconciler) failParentAgentSession(ctx context.Context, ns, name, reason, msg string) error {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: ns, Name: name}
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var sess spiceboxv1alpha1.AgentSession
		if err := r.Client.Get(ctx, key, &sess); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("failParentAgentSession: AgentSession already gone; nothing to fail",
					"agentSession", name, "namespace", ns)
				return nil
			}
			return err
		}
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
			sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed {
			return nil
		}
		patch := client.MergeFromWithOptions(sess.DeepCopy(), client.MergeFromWithOptimisticLock{})
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
		sess.Status.FailureReason = reason
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason: reason, Message: msg,
		})
		err := r.Client.Status().Patch(ctx, &sess, patch)
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("failParentAgentSession: exceeded %d retries on conflict", maxAttempts)
}

// resolvedCall holds the validated pieces needed to exec.
type resolvedCall struct {
	session *spiceboxv1alpha1.SpiceboxSession
	tool    spiceboxv1alpha1.SpiceboxTool
	// toolkit is the toolkit behind the toolspec that ALLOWED this call. Set by
	// validateToolspec, so it is nil until that has run and stays nil for a
	// denied call. execEnv reads its EnvDefaults; nothing else may assume it is
	// populated.
	toolkit *toolkit.Toolkit
}

// validate returns a resolvedCall or a terminal error with a condition reason.
// The returned reason is one of ReasonSessionNotActive, ReasonSessionGone, ReasonToolUnknown, ReasonInputArtifactMissing.
// A nil error + nil resolvedCall never happens: on any failure, *resolvedCall is nil and the reason is set.
func (r *Reconciler) validate(ctx context.Context, tc *spiceboxv1alpha1.ToolCall) (*resolvedCall, string, error) {
	// Resolve session (same namespace).
	var sess spiceboxv1alpha1.SpiceboxSession
	sessKey := types.NamespacedName{Name: tc.Spec.Session, Namespace: tc.Namespace}
	if err := r.Client.Get(ctx, sessKey, &sess); err != nil {
		if errors.IsNotFound(err) {
			return nil, spiceboxv1alpha1.ReasonSessionGone, nil
		}
		return nil, "", err
	}

	// The bundle this call names must belong to the AgentSession that owns the
	// call. Checked BEFORE readiness and before anything privileged runs: both
	// fields are creator-written, and executorFor and checkTokenUse both key on
	// spec.session, so a mismatch would exec in another session's sandbox under
	// that session's grant. The admission webhook binds the creator too; this
	// runs unconditionally so a downed webhook is not a bypass.
	if err := validateSessionBinding(tc, &sess); err != nil {
		log.FromContext(ctx).Info("toolcall: refused — session binding mismatch",
			"toolcall", tc.Namespace+"/"+tc.Name, "err", err.Error())
		return nil, spiceboxv1alpha1.ReasonSessionBindingMismatch, nil
	}

	// Input artifacts are fetched with the operator's unrestricted store, so
	// the ref must be shown to belong to this session before hydration reaches
	// it. Same scoping rule pkg/x/debug applies on the direct read path.
	if err := validateInputArtifactRefs(r.Store, tc.Namespace, tc.Spec.Session, inputArtifactRefs(tc.Spec.InputArtifacts)); err != nil {
		log.FromContext(ctx).Info("toolcall: refused — input artifact outside this session's scope",
			"toolcall", tc.Namespace+"/"+tc.Name, "err", err.Error())
		return nil, spiceboxv1alpha1.ReasonInputArtifactMissing, nil
	}

	// Session must be Ready.
	ready := false
	for _, c := range sess.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionReady && c.Status == "True" {
			ready = true
			break
		}
	}
	if !ready {
		return nil, spiceboxv1alpha1.ReasonSessionNotActive, nil
	}

	// ResolvedClass snapshot must exist (it's set by the Session controller on bind).
	rc := sess.Status.ResolvedClass
	if rc == nil {
		return nil, spiceboxv1alpha1.ReasonSessionNotActive, nil
	}

	// The sandbox handle must exist too — a session is not dispatchable without
	// one. Checked HERE, alongside the session's other readiness preconditions,
	// rather than being left to executorFor much further down.
	//
	// The window is real: on an in-place operator upgrade an already-Ready
	// session carries no handle until its next reconcile stamps one. Reaching
	// executorFor in that window fails the call as ReasonSandboxUnresolved —
	// which reads as "this session's backend is broken" — and does so only
	// AFTER Running=True and StartedAt have been persisted, so a tool that
	// never executed is recorded as having started. Failing at validation
	// instead reports the accurate reason and leaves no false start behind.
	//
	// Deliberately only the missing-handle case. A handle naming a kind with no
	// registered runtime stays SandboxUnresolved in executorFor: that is a
	// backend not linked into this binary, which no amount of waiting fixes,
	// and it is correctly fail-closed there.
	if sess.Status.Sandbox == nil {
		return nil, spiceboxv1alpha1.ReasonSessionNotActive, nil
	}

	// Find the tool.
	var tool *spiceboxv1alpha1.SpiceboxTool
	for i := range rc.Tools {
		if rc.Tools[i].Name == tc.Spec.Tool {
			tool = &rc.Tools[i]
			break
		}
	}
	if tool == nil {
		return nil, spiceboxv1alpha1.ReasonToolUnknown, nil
	}

	return &resolvedCall{session: &sess, tool: *tool}, "", nil
}

func (r *Reconciler) setValidated(tc *spiceboxv1alpha1.ToolCall) {
	conditions.Set(tc, &tc.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.ToolCallConditionValidated,
		Status:  metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonToolCallValid,
		Message: "spec validated",
	})
}

func (r *Reconciler) setFailed(tc *spiceboxv1alpha1.ToolCall, reason, msg string) {
	conditions.Set(tc, &tc.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.ToolCallConditionFailed, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg,
	})
}

func (r *Reconciler) setRunning(tc *spiceboxv1alpha1.ToolCall) {
	conditions.SetTrue(tc, &tc.Status.Conditions,
		spiceboxv1alpha1.ToolCallConditionRunning, spiceboxv1alpha1.ReasonExecStarted)
}

func (r *Reconciler) setSucceeded(tc *spiceboxv1alpha1.ToolCall) {
	conditions.SetTrue(tc, &tc.Status.Conditions,
		spiceboxv1alpha1.ToolCallConditionSucceeded, spiceboxv1alpha1.ReasonProcessExited)
}

func (r *Reconciler) setTimeout(tc *spiceboxv1alpha1.ToolCall) {
	conditions.Set(tc, &tc.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.ToolCallConditionTimeout, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonDeadlineExceeded, Message: "tool exceeded spec.timeout",
	})
}

// terminalCondition returns the name of the first terminal condition found on tc
// (Succeeded, Failed, Timeout, Canceled), or "" if none.
func terminalCondition(tc *spiceboxv1alpha1.ToolCall) string {
	for _, c := range tc.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			continue
		}
		switch c.Type {
		case spiceboxv1alpha1.ToolCallConditionSucceeded,
			spiceboxv1alpha1.ToolCallConditionFailed,
			spiceboxv1alpha1.ToolCallConditionTimeout,
			spiceboxv1alpha1.ToolCallConditionCanceled:
			return c.Type
		}
	}
	return ""
}

func hasTrueCondition(tc *spiceboxv1alpha1.ToolCall, condType string) bool {
	for _, c := range tc.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func humanMessage(reason string) string {
	switch reason {
	case spiceboxv1alpha1.ReasonSessionGone:
		return "referenced SpiceboxSession does not exist"
	case spiceboxv1alpha1.ReasonSessionNotActive:
		// Covers every way a session is not dispatchable: Ready is not True, the
		// resolved-class snapshot is not frozen yet, or no sandbox handle has
		// been stamped. Worded to fit all three rather than naming Ready alone.
		return "referenced SpiceboxSession is not ready to accept tool calls"
	case spiceboxv1alpha1.ReasonToolUnknown:
		return "tool name is not in the session class catalog"
	}
	return reason
}

func summarizeFailures(failures []spiceboxv1alpha1.ToolspecFailure) string {
	if len(failures) == 1 && failures[0].SpecName == "" {
		// fail-closed "no spec authorizes" path
		return failures[0].Reason
	}
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, fmt.Sprintf("%s: %s", f.SpecName, f.Reason))
	}
	return fmt.Sprintf("all %d candidate spec(s) denied — %s", len(failures), strings.Join(parts, "; "))
}

// bumpSessionActivity increments the parent session's callCount and sets
// lastActivityAt = now. Called once per terminal transition per ToolCall
// (the terminal short-circuit at the top of Reconcile prevents double counting).
func (r *Reconciler) bumpSessionActivity(ctx context.Context, sess *spiceboxv1alpha1.SpiceboxSession) error {
	key := client.ObjectKeyFromObject(sess)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var fresh spiceboxv1alpha1.SpiceboxSession
		if err := r.Client.Get(ctx, key, &fresh); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()
		fresh.Status.LastActivityAt = &now
		fresh.Status.CallCount++

		err := r.Client.Status().Patch(ctx, &fresh, patch)
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("bumpSessionActivity: exceeded %d retries on conflict", maxAttempts)
}

// firstSecretLikeKey returns the first env key whose name looks like a secret,
// along with the canonical reason. Returns empty strings when none match.
func firstSecretLikeKey(env map[string]string) (reason, key string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if IsSecretLikeKey(k) {
			return spiceboxv1alpha1.ReasonToolCallEnvSecretLike, k
		}
	}
	return "", ""
}

// highEntropyKeys returns the env keys whose values pass the entropy heuristic.
// Sorted for deterministic status output.
func highEntropyKeys(env map[string]string) []string {
	var out []string
	for k, v := range env {
		if IsHighEntropyValue(v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// mergeEnv returns the union of a (agent-injected, takes precedence) and b
// (tc.spec.env). Agent keys win on collision; the caller has typically
// already rejected colliding tc.spec.env keys with ToolCallEnvShadowsAgent.
func mergeEnv(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range b {
		out[k] = v
	}
	for k, v := range a {
		out[k] = v
	}
	return out
}
