// The AgentSession reconciler: resolves the AgentClass and gates on Valid=True;
// owns the per-session ServiceAccount, Role, RoleBinding, memory-token Secret
// and bundle SpiceboxSessions; creates the runner Pod; and tracks runner
// restarts through RunnerReady / Failed=RunnerCrashed. On deletion the
// finalizer revokes the memory token and deletes the memory entry.
package agentsession

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	contentguardregistry "github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/auditkey"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=useridentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionuseridentities,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionuseridentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteragentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolspecs,verbs=get;list;watch
// CredentialUpdateRequest: `list` backs awaitingCredentialUpdateRequestFor
// ("is a request of mine still waiting on a human"); `get;watch` back the
// Watches below (mapCredentialUpdateRequestToSession). Read-only — this
// controller only reads the phase, to park/unpark the session.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=credentialupdaterequests,verbs=get;list;watch
// SessionHold: `list` backs activeHoldFor ("is an unreleased hold naming this
// session"); `get;watch` back the Watches below (mapSessionHoldToSession).
// Read-only on the main resource — this controller never creates or updates a
// SessionHold's spec, only its status (TrippedAt/Phase/Determination, stamped
// once per hold).
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionholds,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionholds/status,verbs=get;update;patch
// AgentIdentity: `get` resolves the tool AgentIdentity named by the AgentClass
// when building per-session RBAC; `list;watch` back the Watches below, which
// re-enqueues sessions on credential rotation/revocation
// (sessionsForAgentIdentityChange).
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// The passthrough Role/RoleBinding live in agentprimitives-identities and have
// no AgentSession owner ref (cross-namespace), so GC cannot reap them and the
// finalizer deletes them by name — hence `delete` on top of create;patch.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=channels,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=channels/status,verbs=update;patch
// Skill bundles: opted-in Skills are read for their bundle digests, and
// per-session ConfigMaps (binaryData tarball) that the sandbox pod mounts and
// untars are create-or-updated. Each ConfigMap carries an AgentSession owner
// ref and is reaped by GC; `delete` is held so an explicit cleanup path stays
// open without an RBAC change.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskills,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
//
// Per-session ServiceAccount / Role / RoleBinding: kept current by SSA, which
// needs `create` (first apply) plus `patch` (subsequent ones); get/list/watch
// serve the Owns() cache informer. Deliberately NO `update` (all mutation is
// SSA patch) and NO `delete` (each carries an AgentSession owner ref and is
// reaped by GC).
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;patch
// The per-session toolspec-reader ClusterRoleBinding is cluster-scoped, so it
// cannot carry a namespaced owner ref: GC will not reap it and the finalizer
// must delete it explicitly — hence `delete` here but not on the namespaced
// RBAC above. K8s privilege-escalation prevention is satisfied because the
// operator already holds spiceboxtoolspecs get;list;watch, i.e. everything the
// bound spicebox-toolspec-reader ClusterRole confers.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create;delete
// The per-session memory-token Secret is created (client.Create) and read
// (client.Get) here; list/watch back the Owns() cache informer. No
// `delete` — the Secret carries an AgentSession owner ref and is reaped by
// GC. This grant must be cluster-wide (a ClusterRole, not a namespaced
// Role) because AgentSessions can be created in ANY namespace, and the
// operator must create/read each session's Secret in that session's own
// namespace; per-session Roles keep the runner SA pinned to its own Secret by
// name. `update` covers the operator's OWN writes to existing Secrets — the
// args-hash-key and audit-signing-key ensures on the update path, and
// writeSidecarSecret's rewrite of rotated sidecar credentials. It is NOT
// delegated onward: BuildPassthroughSecretRBAC grants the runner `get` and
// nothing more, deliberately, so a prompt-injected runner cannot clobber a
// human's stored credential.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// deployments + statefulsets (get;list) back admind's cluster-health snapshot
// (pkg/web/admind/health), which reads first-party replica/ready counts to
// report Healthy/Degraded/Down. Read-only. StatefulSets are included because
// NATS and Neo4j ship as StatefulSets.
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list
// +kubebuilder:rbac:groups="apps",resources=statefulsets,verbs=get;list
// metrics.k8s.io pods (get;list) backs the same snapshot's CPU/Memory rollup,
// summing live PodMetrics for the namespace. Best-effort: absent
// metrics-server the List degrades to "n/a" rather than failing.
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// nodes (list) backs the provable-unschedulable fast-fail: compare a stuck
// bundle pod's resource request to the largest node's allocatable capacity.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list
// The shared-workspace PVC is created and read here; list/watch back the
// Owns() cache. No `delete` — it carries an AgentSession owner ref and is
// reaped by GC.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=list
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch;delete
// The runner ServiceAccount's Role grants create;delete on toolcalls for
// sandbox tool dispatch, and K8s privilege-escalation prevention requires the
// operator to hold a verb before it can grant it.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=toolcalls,verbs=create;delete
// Same delegation for artifactrenders (created by artifact_prepare in the
// runner; the operator never calls Create itself).
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=artifactrenders,verbs=create
// Same delegation for credentialupdaterequests (created by the
// request_credential_update meta-tool — see BuildRunnerRBAC). `create` exists
// here solely to satisfy privilege-escalation prevention; the reconciler
// itself only reads them.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=credentialupdaterequests,verbs=create
// Same delegation for subagentrequests (created by the delegate meta-tool —
// see BuildRunnerRBAC). `create` exists here solely to satisfy
// privilege-escalation prevention; the AgentSession reconciler itself never
// creates a SubagentRequest — that is the SubagentRequest controller's job.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=subagentrequests,verbs=create

const maxRunnerRestarts = 3

// earlyRunnerCrashRestarts is the restart count at which a runner crash with a
// CAPTURED error message is surfaced as Failed, ahead of the maxRunnerRestarts
// backstop. 1 means "crashed, restarted, and failing again" — a terminal
// startup error repeats identically, so the user sees the real cause in
// seconds, while a single transient crash that would recover is not reported.
const earlyRunnerCrashRestarts = 1

// mcpAgentIdentityRequeueAfter is the interval between retries when the MCP
// AgentIdentity named by the AgentClass does not exist yet.
const mcpAgentIdentityRequeueAfter = 5 * time.Second

// detectorReadyDeadline bounds how long a session may sit in
// RunnerReady=False/AwaitingDetector waiting for a content-guard detector pod
// to report a Ready PodIP. A detector that never becomes Ready (e.g.
// ImagePullBackOff) would otherwise requeue forever; past the deadline the
// reconcile fails closed via ContentGuardHalt. The session never runs unscanned.
//
// Measured from PROVISIONING START — the earliest not-ready detector pod's
// CreationTimestamp — not the AgentSession's. Under identityMode=userPassthrough
// the detector pods are not created until the AwaitingCredentials gate clears,
// so measuring from session creation would charge the interactive credential-link
// wait against the budget and fail a session whose detector came up in seconds.
// The pod timestamp is post-credentials for passthrough and ≈ session creation
// for identityMode=agent, so it is correct for both with no extra CRD field.
const detectorReadyDeadline = 5 * time.Minute

// sidecarTerminalRequeue is the reflection interval for a separate-pod sidecar
// wedged in a terminal state while the runner is live. The session is
// deliberately NOT failed then (the agent is told instead), which would
// otherwise hold the 2s not-ready cadence for the rest of the session's life.
// The pod is owned and watched, so a real recovery still wakes the reconciler
// immediately; this only bounds the idle polling in between.
const sidecarTerminalRequeue = 30 * time.Second

// bundleReadyDeadline bounds how long a session may sit in
// BundlesReady=False/BundlesProvisioning waiting for its sandbox bundle
// SpiceboxSessions to become Ready. A bundle pod stuck Pending — its request
// exceeding every node's allocatable, so it is permanently Unschedulable and
// the autoscaler will not add a node that fits — is NOT Failed, so the terminal
// BundleFailed branch never fires and the reconcile would requeue forever with
// no feedback. Past the deadline it fails closed via BundleFailed, naming the
// offending bundle(s).
//
// Like detectorReadyDeadline, measured from PROVISIONING START (the earliest
// not-ready bundle SpiceboxSession's CreationTimestamp), so a userPassthrough
// session's credential-link wait is never counted against it. The window is
// generous: a cold first session may need a node scale-up plus an image pull
// before the bundle pod even starts.
const bundleReadyDeadline = 8 * time.Minute

// filestoreBundleReadyDeadline is the extended window for a workspace on a
// slow-cold-start StorageClass (GKE Filestore multishare): the FIRST PVC on a
// cold pool triggers a backing-instance create that alone takes ~5-10 min
// (observed ~8.3 min), which would blow the default 8m and fail a healthy
// session on timing. A genuinely-stuck (permanent) provisioning failure does
// not wait this out — firstPVCProvisioningFailure fast-fails it — so the extra
// slack only ever benefits a slow-but-progressing provision.
const filestoreBundleReadyDeadline = 20 * time.Minute

// bundleRetryRequeue is how soon the reconciler re-runs after deleting a Failed
// bundle SpiceboxSession for its one allowed retry: long enough for the delete
// to land in the informer cache, so the next pass re-applies a FRESH instance
// instead of reading the deleted one.
const bundleRetryRequeue = 5 * time.Second

// bundleProvisioningTimedOut reports whether bundle provisioning has exceeded
// deadline, measured from provisioningStarted. A zero provisioningStarted (no
// not-ready bundle observed yet) never times out — fail-safe: the deadline trips
// only once a real start is known. deadline is class-dependent
// (effectiveBundleReadyDeadline) so a slow-cold-start workspace class gets more
// room.
func bundleProvisioningTimedOut(provisioningStarted, now time.Time, deadline time.Duration) bool {
	return !provisioningStarted.IsZero() && now.Sub(provisioningStarted) > deadline
}

// detectorProvisioningTimedOut is the detector twin of
// bundleProvisioningTimedOut, measured against detectorReadyDeadline. A zero
// provisioningStarted never times out (fail-safe).
func detectorProvisioningTimedOut(provisioningStarted, now time.Time) bool {
	return !provisioningStarted.IsZero() && now.Sub(provisioningStarted) > detectorReadyDeadline
}

// earlierNonZero returns the earlier of cur and candidate, treating the zero
// time as "absent": a zero candidate never displaces a real timestamp, and a
// real candidate always replaces a zero cur.
func earlierNonZero(cur, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return cur
	}
	if cur.IsZero() || candidate.Before(cur) {
		return candidate
	}
	return cur
}

// provablyUnschedulableBundle checks whether any named bundle pod is permanently
// Unschedulable because its request exceeds what the largest node can offer,
// letting the reconcile fail fast instead of waiting out bundleReadyDeadline.
//
// "Largest node" is the honest bound and nothing more: where an autoscaler can
// add a bigger machine a pod above it may still schedule, so this can be late
// but never wrong the other way. FAIL-SAFE throughout — any error (no nodes
// RBAC, a pod not yet created, no capacity data) returns ("", false), leaving
// the deadline as the catch-all so a transient state never fails a session.
func (r *Reconciler) provablyUnschedulableBundle(ctx context.Context, namespace string, podNames []string) (string, bool) {
	if len(podNames) == 0 {
		return "", false
	}
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		log.FromContext(ctx).Info("bundle fast-fail: list nodes failed; relying on the deadline backstop", "err", err.Error())
		return "", false
	}
	ceiling := schedfit.CeilingFromNodes(nodes.Items)
	if !ceiling.Known {
		return "", false
	}
	for _, name := range podNames {
		var pod corev1.Pod
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			continue
		}
		// Check the POD, not each container in turn: Kubernetes places a pod
		// atomically, so its containers hold their requests at once and sum.
		// Weighing them singly under-detects, letting a pod whose containers
		// each fit — but whose total cannot — wait out the full deadline.
		if reason, exceeds := schedfit.ExceedsPod(fmt.Sprintf("pod %q", name), &pod, ceiling); exceeds {
			return reason, true
		}
	}
	return "", false
}

// podSchedulingFailureReason reports whether pod is stuck Pending because the
// scheduler could not place it (Phase=Pending with PodScheduled=False). The
// reason is the scheduler's own message, falling back to the condition Reason
// when the message is empty. Anything else returns ("", false) — FAIL-SAFE, so
// the caller keeps the generic waiting message rather than a spurious reason.
// Reads the pod's own status, not the Event stream, so it needs no extra RBAC.
func podSchedulingFailureReason(pod *corev1.Pod) (string, bool) {
	if pod == nil || pod.Status.Phase != corev1.PodPending {
		return "", false
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			if msg := strings.TrimSpace(c.Message); msg != "" {
				return msg, true
			}
			if c.Reason != "" {
				return c.Reason, true
			}
			return "", false
		}
	}
	return "", false
}

// schedulingStall describes a bundle/detector pod stuck Pending because the
// scheduler cannot place it. ok=false from the producers means "no stall".
type schedulingStall struct {
	podName string
	podUID  types.UID
	reason  string        // the scheduler's message, e.g. "0/3 nodes are available: Insufficient cpu."
	pending time.Duration // how long the pod has been alive (≈ how long it has been Pending)
}

// message renders the session-visible condition message: pod name plus the real
// scheduler reason, which already carries the node count ("0/3 nodes …").
func (s schedulingStall) message() string {
	return fmt.Sprintf("%s pod unschedulable: %s; waiting for capacity",
		s.podName, strings.TrimSuffix(s.reason, "."))
}

// schedulingStallForPod fetches the named pod and reports whether it is stuck
// Pending on a scheduling failure. FAIL-SAFE: a missing, unreadable, or
// non-pending pod returns (zero, false), so callers fall back to the generic
// waiting message.
func (r *Reconciler) schedulingStallForPod(ctx context.Context, namespace, podName string) (schedulingStall, bool) {
	if podName == "" {
		return schedulingStall{}, false
	}
	var pod corev1.Pod
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		return schedulingStall{}, false
	}
	reason, ok := podSchedulingFailureReason(&pod)
	if !ok {
		return schedulingStall{}, false
	}
	return schedulingStall{
		podName: pod.Name,
		podUID:  pod.UID,
		reason:  reason,
		pending: r.now().Sub(pod.CreationTimestamp.Time),
	}, true
}

// containerTermination is a container death kubelet has already recorded on a
// bundle pod — the observed cause the terminal Failed message names instead of
// guessing ("likely a slow image pull / OOM kill / …").
type containerTermination struct {
	podName   string
	container string
	reason    string // kubelet's Terminated.Reason, e.g. "OOMKilled"; may be ""
	exitCode  int32
}

func (t containerTermination) message() string {
	reason := t.reason
	if reason == "" {
		reason = "Error"
	}
	return fmt.Sprintf("container %q in pod %q terminated: %s (exit code %d)", t.container, t.podName, reason, t.exitCode)
}

// firstContainerTermination returns the first failed container termination
// recorded across the named pods: a currently-terminated container, or a
// restarting container's LastTerminationState — which is how an OOM-killed
// container appears while its restart backs off. Exit-code-0 terminations are
// skipped (a Completed init/sidecar container is not the failure). FAIL-SAFE:
// pod misses are skipped; ok=false keeps the caller's generic message.
func (r *Reconciler) firstContainerTermination(ctx context.Context, namespace string, podNames []string) (containerTermination, bool) {
	for _, name := range podNames {
		if name == "" {
			continue
		}
		var pod corev1.Pod
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			continue
		}
		statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
		statuses = append(statuses, pod.Status.InitContainerStatuses...)
		statuses = append(statuses, pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			term := cs.State.Terminated
			if term == nil {
				term = cs.LastTerminationState.Terminated
			}
			if term == nil || term.ExitCode == 0 {
				continue
			}
			return containerTermination{
				podName:   pod.Name,
				container: cs.Name,
				reason:    term.Reason,
				exitCode:  term.ExitCode,
			}, true
		}
	}
	return containerTermination{}, false
}

// firstSchedulingStall returns the first pod in podNames stuck Pending on a
// scheduling failure. FAIL-SAFE (each pod miss is skipped); ok=false means none
// are stalled, so the caller keeps the generic waiting message.
func (r *Reconciler) firstSchedulingStall(ctx context.Context, namespace string, podNames []string) (schedulingStall, bool) {
	for _, name := range podNames {
		if stall, ok := r.schedulingStallForPod(ctx, namespace, name); ok {
			return stall, true
		}
	}
	return schedulingStall{}, false
}

// maybeEmitUnschedulable sets the durable SandboxScheduling=False condition and
// fans a one-shot "capacity" warning to the monitoring channel once a
// bundle/detector pod has been stuck unschedulable past unschedulableGrace.
// Setting the condition is deliberately UNGATED by r.MonitoringPublish and by
// the per-pod-UID dedup below: channelsd reacts to CR status, not NATS, so it
// must land even where monitoring is unconfigured. The lifecycle-log append and
// the publish stay deduped per pod UID via r.unschedEmitted so the 2s requeue
// loop emits once per episode. A publish error is logged, never swallowed.
func (r *Reconciler) maybeEmitUnschedulable(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, condition string, stall schedulingStall) {
	if stall.pending < unschedulableGrace {
		return
	}
	r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
		spiceboxv1alpha1.ReasonSandboxUnschedulable, stall.message())

	if r.MonitoringPublish == nil {
		return
	}
	if _, loaded := r.unschedEmitted.LoadOrStore(stall.podUID, struct{}{}); loaded {
		return
	}
	// Record the unschedulable surface once per pod episode. The phase stays
	// Pending (the core's Unschedulable transition is surface-only); this is a
	// best-effort audit record, so a log-append failure is logged, not fatal.
	if err := r.applyEvent(ctx, sess, lifecyclecore.Unschedulable{}); err != nil {
		log.FromContext(ctx).Info("agentsession: append unschedulable lifecycle event failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "capacity",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "AgentSession", Namespace: sess.Namespace, Name: sess.Name,
		},
		Condition: condition,
		Reason:    "Unschedulable",
		Summary:   stall.message(),
		Hint:      "free capacity or add a node",
		Timestamp: r.now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		log.FromContext(ctx).Info("agentsession: publish unschedulable monitoring event failed",
			"session", sess.Namespace+"/"+sess.Name, "pod", stall.podName, "err", err.Error())
	}
}

// SpiceDBDeleter is the controller's narrow view of SpiceDB cleanup.
// Satisfied by *spicedb.Client.
//
// Two calls, not one, because a session's relationships live on two sides.
// DeleteAgentSessionRelationships wipes everything whose RESOURCE is the
// agentsession — owner, participant, denied, authorized_token. A slot grant is
// the other direction: the resource is an external instance and the session is
// the SUBJECT, so that filter never matches it. Without the second call every
// session that bound an instance would leave a live grant behind on somebody
// else's resource, and the AgentSession CR is retained after completion, so
// slot_grant->interact would keep resolving indefinitely.
// DeleteDataSlotGrants is a THIRD call for the same reason, and neither of the
// other two reaches it. A data slot grant is `pt_tag:<T>#granted_to@agentsession
// :<ns/name>`: the session is the SUBJECT, so the agentsession-as-resource
// filter misses it, and `granted_to` is not a `slot_grant_<perm>` relation, so
// the slot-grant sweep drops it client-side. Without it a child that was handed
// data keeps `access` on those tags indefinitely.
type SpiceDBDeleter interface {
	DeleteAgentSessionRelationships(ctx context.Context, ns, name string) error
	DeleteSlotGrants(ctx context.Context, ns, name string) error
	DeleteDataSlotGrants(ctx context.Context, ns, name string) error
}

// ScopeDeleter is the controller's narrow view of memory teardown, retained for
// an ephemeral retention GC. The finalizer deliberately deletes NONE of a
// session's memory: the tamper-evident audit log is PERMANENT and a session
// disappearing must never take its logs with it. DeleteScope itself structurally
// refuses append-only kinds, so this seam can only ever drop mutable memory.
// Satisfied by *memory.Local (in-process, operator-only — never over HTTP).
type ScopeDeleter interface {
	DeleteScope(ctx context.Context, scope memory.Scope) error
}

// Reconciler reconciles AgentSession objects.
type GoalSessionValidator interface {
	ValidateGoalSession(context.Context, *spiceboxv1alpha1.AgentSession) error
	ConstrainGoalSettings(context.Context, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.EffectiveSettings) error
}

type Reconciler struct {
	GoalValidator GoalSessionValidator
	Client        client.Client
	APIReader     client.Reader

	// UsesLocalDevImages mirrors the resolved cloud kind's
	// InstallProfile().UsesLocalDevImages(): true on local/desktop clusters,
	// where first-party and sidecar images are `oap image load`ed by mutable tag
	// and are NOT pullable by digest. When true, the per-session SidecarToolbox
	// by-digest launch rewrite is skipped (see ResolveSidecarImageRef) — a
	// `repo@sha256:…` ref there ErrImagePulls and the sidecar never boots. The
	// operator wires this from clusterStrategy.InstallProfile(); the zero value
	// (false) is the production/registry default, so tests and the e2e harness
	// keep the digest-pin behavior unless they opt in.
	UsesLocalDevImages bool

	// SecretReader is the guarded Secret reader. When set, reads of operator-
	// created and user-referenced Secrets are gated to adopted objects. When nil,
	// falls back to the unguarded reader (for tests that do not inject adoptguard).
	SecretReader *adoptguard.SecretReader

	// ConfigMapReader is the guarded ConfigMap reader. When set, reads of
	// operator-created and user-referenced ConfigMaps are gated to adopted
	// objects. When nil, falls back to the unguarded reader.
	ConfigMapReader *adoptguard.ConfigMapReader

	Tokens *tokens.Registry

	// Memory tears down a session's memory scope on finalization.
	// Wired to the operator's single in-process *memory.Local.
	Memory ScopeDeleter

	// RunnerFactory creates/destroys the per-session runner. Required: must be
	// non-nil before SetupWithManager. The operator wires PodRunnerFactory, the
	// e2e harness InProcessRunnerFactory.
	RunnerFactory RunnerFactory

	// DefaultChannelArchiveAfter is the fallback archive interval for
	// channel-attached sessions with no AgentClass.spec.channels.archiveAfter.
	// 0 disables archival. From --default-channel-archive-after (4h).
	DefaultChannelArchiveAfter time.Duration

	// DefaultSessionSleepAfter is the fallback idle-sleep grace for
	// channel-attached sessions with no AgentClass override, from
	// --default-session-sleep-after (10m). 0 disables idle-sleep entirely, so
	// pods stay warm for the whole Idle window.
	DefaultSessionSleepAfter time.Duration

	// DefaultSessionStorageRetention is the fallback for how long a TERMINAL
	// session's workspace + snapshot-store PVCs are kept past FinishedAt
	// before reconcileStorageReclaim deletes them. Overridden per class by
	// spec.channels.storageRetention (> 0), same precedence archive uses.
	// 0 disables the sweep. From --session-storage-retention (72h).
	DefaultSessionStorageRetention time.Duration

	// FailedSandboxReapGrace is how long a Failed session's sandbox pods are kept
	// for debugging before teardown. Measured from status.finishedAt; past it the
	// bundle SpiceboxSessions and runner pod are reaped so a dead session stops
	// stranding CPU, while the AgentSession itself stays for inspection. 0 or
	// negative disables reaping. From --failed-sandbox-reap-grace (1m).
	FailedSandboxReapGrace time.Duration

	// SpiceDBDeleter, when non-nil, wipes all agentsession relationships in
	// SpiceDB during finalization, before the finalizer is removed. Nil skips
	// SpiceDB cleanup, logged rather than silent.
	SpiceDBDeleter SpiceDBDeleter

	// WorkspaceStorageClass is the RWX StorageClass for AgentSession workspace
	// PVCs. Empty disables shared workspaces (isolated fallback).
	WorkspaceStorageClass string
	// WorkspaceSize is the requested size for workspace PVCs. Empty defaults to 2Gi.
	WorkspaceSize string
	// SnapshotImage is the cp-by-pod image used to run the workspace overlay-cut
	// Job that seeds a session's workspace PVC from a WorkspaceSource's base PVC.
	// Wired from the operator's --snapshot-image flag.
	SnapshotImage string
	// SnapshotServiceAccount is the ServiceAccount the overlay-cut Job runs as.
	// Wired from the operator's --snapshot-service-account flag.
	SnapshotServiceAccount string
	// WorkspaceReconcileImage is the git-capable image the runner's per-session
	// sync_workspace/apply_workspace Job runs, from --materialize-image. It is
	// snapshotted onto status.resolvedWorkspaceSource so the runner uses the
	// operator-configured, digest-pinnable image rather than a hard-coded one.
	WorkspaceReconcileImage string
	// SnapshotStoreSize is the requested size of each session's snapshot-store
	// PVC, the RWX volume the cp-by-pod Snapshotter writes per-dispatch
	// subdirectories into. Empty defaults to "8Gi".
	SnapshotStoreSize string

	// NATSIdentity is the install-time NATS trust material (account public key
	// plus signing seed) used to mint per-session runner user JWTs scoped to the
	// session's subjects. Nil when the operator started without the
	// spicebox-nats-identity Secret, in which case channel-attached runners are
	// spawned without NATS creds and the reconcile logs rather than fails.
	NATSIdentity *apnats.Identity
	// NATSCAPEM is the install-time NATS server CA certificate, stored in
	// the per-session Secret under "nats.ca" so the runner can verify the
	// NATS server's TLS cert. Empty when NATSIdentity is nil.
	NATSCAPEM []byte

	// SpiceDBToken is the operator's resolved SpiceDB preshared gRPC token. It is
	// written into every per-session Secret under "spicedb-token" and mounted as
	// a file the runner reads via SPICEDB_TOKEN_PATH, so the token is never
	// delivered as a plaintext env var. Every runner needs SpiceDB, so every
	// session gets it.
	SpiceDBToken string

	// Snapshotter restores workspace PVCs from snapshots taken during the parent
	// session's life. Required for restart-from-here when post-cut stateful tool
	// calls happened; nil disables restart entirely.
	Snapshotter workspace.Snapshotter

	// RestartMemory is the memory facade used by the restart reconciler
	// for the prefix copy and lineage edges. Nil disables restart.
	RestartMemory memory.Memory

	// LifecycleMemory is the signing memory facade the sequencer appends typed
	// transition events through (the append-only "lifecycle" kind, signed as
	// system:operator) and folds to compute phase. Nil disables durable event
	// recording: applyEvent still projects phase from the CR's current phase as
	// the floor, but writes nothing to the log.
	LifecycleMemory memory.Memory

	// AuditKeyMemory is the signing memory facade the per-session audit key
	// binding is witnessed through (the append-only "audit_key" kind, signed as
	// system:operator). The witness is what keeps this session's records
	// verifiable once its CR — which holds the only other copy of the public key
	// — is gone; see pkg/memory/kinds/auditkey. Nil records nothing, which costs
	// verifiability of a re-created session's inherited chain and nothing else.
	AuditKeyMemory memory.Memory

	// AuthzGranter handles per-session SpiceDB writes for the child
	// session during a restart fork. Nil disables restart.
	AuthzGranter authz.Granter

	// OrgViewerSyncer levels the agentsession#artifact_org_viewer wildcard
	// tuple from the class's spec.authz.session.artifactVisibility on every
	// reconcile — terminal sessions included (see syncArtifactOrgViewer for
	// why the hook sits above the terminal reap short-circuit). Declared as
	// the interface (typed-nil-interface rule); nil disables the sync, which
	// is the SpiceDB-disabled wiring.
	OrgViewerSyncer authz.ArtifactOrgViewerSyncer

	// TokenGranter reconciles this session's externaltoken authorized_token
	// grants before the runner is dispatched. Declared as the interface, not the
	// pointer, so an unwired operator leaves a true nil (typed-nil-interface
	// rule); nil skips the grant reconcile.
	TokenGranter TokenGranter

	// TokenChecker performs materializeSidecarSecret's one-time pre-handout
	// use_token check: between resolving a sidecar's upstream credential and
	// writing it into the per-session Secret the sidecar envFrom's, it verifies
	// the operator's own authorized_token grant (written by
	// reconcileCredentialGrants earlier in the same reconcile) still allows it.
	// There is no per-use recheck — the sidecar freezes the credential at
	// pod-create time, so ongoing revocation is pod-recreate. Declared as the
	// interface (typed-nil-interface rule); nil skips the check, since checking
	// against grants that were never written would be meaningless.
	TokenChecker TokenChecker

	// DeniedLister reads the parent session's agentsession#denied blocklist so the
	// fork reconciler can copy it onto the child. REQUIRED for restart: a nil
	// lister makes ReconcileRestart fail closed rather than materialize a child
	// with a weaker blocklist, which would leak the parent's transcript. Declared
	// as the interface (typed-nil-interface rule).
	DeniedLister authz.DeniedLister

	// SlotGrantCopier carries a parent session's bound instances onto its child
	// on a restart/inherit fork. NEVER used for takeover — see
	// authz.CopySlotGrants. Nil disables the copy (the child simply re-asks).
	SlotGrantCopier authz.SlotGrantCopier

	// ForkChecker gates restart-from-here (fork) on the session owner
	// (agentsession#fork = started_by) via the SessionFork pipeline point.
	// REQUIRED for restart: a nil checker makes ReconcileRestart fail closed.
	// Declared as the interface (typed-nil-interface rule).
	ForkChecker authz.ForkChecker

	// ForkNoticePublish delivers the SessionFork hook's requester-facing deny
	// notice to channelsd (the parent session's out.metaagent_notice). nil ⇒
	// notice logged + dropped (delivery is best-effort; the deny still stands).
	ForkNoticePublish func(ctx context.Context, parentNs, parentName, requester, body string) error

	// StartChecker answers the start gate for a class that declares
	// spec.authz.session.allowedStarters. nil ⇒ every such session is refused
	// (fail closed); classes without an allowlist are unaffected. Declared as
	// the interface (typed-nil rule).
	StartChecker StartChecker

	// StartRefusedNoticePublish tells the person whose start was refused, on
	// the session's out.metaagent_notice — the same delivery ForkNoticePublish
	// uses, and wired to the same closure. nil ⇒ logged + dropped; the refusal
	// still stands.
	StartRefusedNoticePublish func(ctx context.Context, ns, name, requesterCanonical, body string) error

	// PublisherKeys resolves the component publisher keys that authenticate a
	// status.pendingRestart marker. REQUIRED for restart: a nil lookup makes
	// ReconcileRestart refuse every marker, because a marker whose author cannot
	// be checked is exactly the forgery this gate exists to stop. Declared as the
	// interface (typed-nil-interface rule); the operator wires the same
	// publisherkeys.Registry it builds for provenance verify-on-write.
	PublisherKeys restartmarker.KeyLookup

	// BundleStore is the durable skill-bundle store each opted-in skill's tarball
	// is read from before staging into a per-session ConfigMap; the SkillSource
	// controller Puts into the same store. Postgres-backed when POSTGRES_URI is
	// set, else in-memory. Nil disables skill-bundle staging.
	BundleStore skillbundle.Store

	// Netpol configures per-session NetworkPolicy stamping. Zero value
	// disables stamping (test/e2e harness default); the operator wires
	// it from --session-network-policies + derived endpoint ports.
	Netpol NetpolConfig

	// AuditChainHeads computes the per-publisher final chain heads for a
	// completed session's scope (operator wires it from memLocal; nil in
	// tests skips chain-head recording).
	AuditChainHeads func(ctx context.Context, scope memory.Scope) (map[string]provenance.ChainHead, error)

	// Minter mints federated (ID-JAG) upstream credentials for sidecar toolboxes
	// declaring type=federated upstream auth. Nil when the cluster IdP has no
	// federation configured, and a federated sidecar credential then fails
	// closed. Declared as the interface (typed-nil-interface rule).
	Minter federation.Minter

	// GitHubApp mints GitHub App installation access tokens for sidecar
	// toolboxes declaring type=githubApp upstream auth — the exact mirror of
	// Minter above. Nil when no GitHub App minter is configured, and a
	// githubApp sidecar credential then fails closed.
	GitHubApp credkind.GitHubAppMinter

	// RevokePublisher publishes credential-invalidation and session-hold
	// events so in-flight sessions receive revocation notices when a
	// credential is rescinded or a forensic hold trips. Wired from the
	// operator's shared revocation bus; nil disables event publishing
	// (credentials are retained until expiry; a tripped hold still parks and
	// reaps on the next reconcile, just without the fast-path halt).
	RevokePublisher *revocation.Publisher

	// Now returns the reconcile-time clock; nil defaults to time.Now. Injectable
	// so the provisioning deadline backstops are testable in envtest without
	// waiting wall-clock minutes.
	Now func() time.Time

	// ImagePullSecret is an optional pull Secret name threaded into the detector
	// and cosidecar pods only. User-supplied sidecar-toolbox pods are
	// deliberately untouched: they pull from the user's own registry and manage
	// their own credentials. Empty means no imagePullSecrets. The runner pod's
	// come from PodRunnerFactory.ImagePullSecret.
	ImagePullSecret string

	// OperatorURL is the operator's own in-cluster address (the same value
	// PodRunnerFactory.OperatorURL stamps onto every runner as
	// OPERATOR_MEMORY_URL, podspec.go:401). reconcileSidecarPod stamps it onto
	// the ONE workshop-identity sidecar pod (BuildSidecarPod's identity
	// branch) as OPERATOR_MEMORY_URL too, so internal/cmd/workshop can reach
	// the operator with its bearer. Empty when the operator's debug port is
	// disabled — the same "memory API not available" condition
	// PodRunnerFactory.OperatorURL documents.
	OperatorURL string

	// TrustedImageRegistry is the registry this operator was installed under
	// (trustedImageRegistryFrom(apimage.Sandbox, cfg.sandboxImage) in
	// internal/cmd/operator/main.go — the same value the WorkshopProbe
	// controller and the Workshop admission webhook already use), threaded
	// through to BuildSidecarPod's workshop-identity branch as
	// WORKSHOP_TRUSTED_IMAGE_REGISTRY. Not a credential — a registry hostname
	// — it exists so the workshop sidecar's `inventory` tool can report
	// apimage.WorkshopSidecarImages refs the builder can use verbatim. Empty on
	// a local/desktop install (no trusted registry): the env var is then
	// omitted entirely, never sent as "".
	TrustedImageRegistry string

	// MonitoringPublish publishes framework MonitoringEvents onto the fixed
	// monitoring subject, which channelsd fans out to role=monitoring Channels.
	// Nil when NATS is unconfigured; capacity/scheduling events are then skipped,
	// though the session condition message still carries the reason. It is a func
	// type, not an interface, so a nil here is a true nil.
	MonitoringPublish channelevents.PublishFunc

	// unschedEmitted dedupes the pod-unschedulable monitoring event to once per
	// pod UID: the bundle/detector wait reconciles every 2s, but a stuck pod is
	// one episode. A recreated pod (new UID) is a fresh episode. The Reconciler
	// is always held by pointer, so the sync.Map is never copied.
	unschedEmitted sync.Map // types.UID → struct{}

	// diskPressureEmitted dedupes the node-workspace-disk-pressure monitoring
	// warning (diskguard.go) to once per episode, keyed by node NAME: a node
	// over the ephemeral-storage watermark is one episode until it drops back
	// under, when the marker is cleared so a later crossing warns again. Same
	// one-shot-per-episode contract as unschedEmitted.
	diskPressureEmitted sync.Map // node name → struct{}

	// NodePinnedStorageReclaimGrace caps how long a TERMINAL session's scratch
	// PVCs are kept when the workspace class is node-pinned/local-path
	// (cloud.BundledWorkspaceStorageClass) — the disk-pressure-prone case. It is
	// deliberately SHORT (minutes) so those volumes cannot pile onto one node's
	// disk, overriding the longer DefaultSessionStorageRetention (and any
	// per-class override) for that class only. 0 disables the cap, leaving the
	// normal retention. From --node-pinned-storage-reclaim-grace (15m).
	NodePinnedStorageReclaimGrace time.Duration

	// IdleStorageReclaimAfter is how long a SLEPT, Idle channel session on
	// node-pinned/local-path storage (cloud.BundledWorkspaceStorageClass) keeps
	// its workspace + snapshot-store PVCs after SleptAt before
	// reconcileIdleStorageReclaim deletes them, while the session stays Idle and
	// wakeable. It bounds the disk an unbounded population of parked, wakeable
	// sessions holds: the idle-sleep reaper frees their CPU but keeps their
	// PVCs, so without this their scratch accumulates on node-local disks until a
	// node crosses the ephemeral-storage watermark. Only the node-pinned class is
	// swept (a cross-node RWX class puts no single node at risk); 0 disables it.
	// From --idle-storage-reclaim-after (30m).
	IdleStorageReclaimAfter time.Duration

	// watchingSince is when THIS operator process began reconciling, recorded on
	// the first Reconcile pass. It is the witness window shouldFastFailRunner
	// checks a runner crash against — see witnessing(). A test may set it
	// directly before the first Reconcile; witnessing() only fills a zero value.
	watchOnce     sync.Once
	watchingSince time.Time
}

// witnessing returns the moment this operator process started reconciling.
//
// The operator serves the agentsessionidentity admission webhook itself, at
// replicas: 1 with strategy: Recreate, so while it is down that webhook —
// failurePolicy: Fail, matching exactly the `-runner-sa` principals — refuses
// every runner's status and annotation writes. A runner that exhausts its retry
// budget in that window exits and the kubelet restarts it into the same closed
// door, so on return the operator can be looking at a crashloop ITS OWN ABSENCE
// CAUSED and its return has already fixed.
//
// Recorded on the first Reconcile, not at process start, because that is when
// the operator is genuinely serving: caches synced, leader election won, webhook
// server up. Everything before that is still part of the outage.
func (r *Reconciler) witnessing() time.Time {
	r.watchOnce.Do(func() {
		if r.watchingSince.IsZero() {
			r.watchingSince = r.now()
		}
	})
	return r.watchingSince
}

// shouldFastFailRunner reports whether a crashing runner should be surfaced as
// Failed ahead of the maxRunnerRestarts backstop, so a genuinely broken runner
// (unresolved toolkit, bad image) shows its real cause in seconds. It rests on
// one premise: a clean startup crash repeats identically and never recovers.
//
// That premise holds only for a crash caused by something about THIS SESSION.
// It is false when the operator itself was down (see witnessing()), and that is
// exactly where firing is worst: on its first pass after an install the operator
// would terminalize a live session, stamp finishedAt, and relay `failed calling
// webhook …` to the user. So the crash must have FINISHED while this process was
// watching; a crash it did not witness is not evidence, because its own absence
// is a candidate cause.
//
// Cost in the true-positive case is one kubelet retry: a really broken runner
// crashes again within seconds of the operator's return, that crash IS
// witnessed, and the fast path fires then. overCap remains the backstop.
//
// A crash the kubelet published no FinishedAt for counts as unwitnessed: an
// unknown crash time cannot establish that it postdates the operator's return,
// and the safe direction is to leave the session alive for overCap to decide.
func shouldFastFailRunner(pod *corev1.Pod, restarts int32, crashMsg string, crashing bool, watchingSince time.Time) bool {
	if !crashing || crashMsg == "" || restarts < earlyRunnerCrashRestarts {
		return false
	}
	at := runnerCrash(pod).finishedAt
	return !at.IsZero() && at.After(watchingSince)
}

// unschedulableGrace is how long a bundle/detector pod must stay stuck
// Pending-on-scheduling before a capacity warning fans out to the monitoring
// channel: long enough to ride out a blip the autoscaler resolves itself. Only
// that emit waits; the session condition message carries the reason at once.
const unschedulableGrace = 30 * time.Second

// now returns the reconcile-time clock, defaulting to time.Now when unset.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// sessionsForUserIdentityChange returns reconcile requests for every
// NON-TERMINAL AgentSession started by subject. A UserIdentity link, replace, or
// revoke must promptly reconcile all the subject's live sessions — not just
// parked (AwaitingCredentials) ones — so passthrough credentials re-project and
// a credential invalidation is emitted on a real change. Terminal sessions are
// excluded: they have no live credential consumer to refresh.
func sessionsForUserIdentityChange(items []spiceboxv1alpha1.AgentSession, subject string) []reconcile.Request {
	var out []reconcile.Request
	for i := range items {
		s := &items[i]
		if spiceboxv1alpha1.StartedBySubject(s).String() == subject &&
			!isTerminalPhase(s.Status.Phase) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(s)})
		}
	}
	return out
}

// sessionsForAgentIdentityChange returns reconcile requests for every
// NON-TERMINAL AgentSession in aiNamespace. An AgentIdentity change
// (credential rotation / revocation) must promptly re-run
// reconcileCredentialGrants for its bound sessions so a revoked or rotated
// credential's grant is corrected without waiting for the next periodic
// resync.
//
// This is intentionally namespace-scoped rather than matched against a
// specific session field: the identity that actually drives
// reconcileCredentialGrants is the AgentClass's class-level default
// (sessionRuntimeIdentity in credential_grants.go resolves
// ac.Spec.AgentIdentity, in sess.Namespace — it does not consult
// sess.Spec.AgentIdentity). Precisely matching "sessions bound to this
// AgentIdentity" would require loading each session's AgentClass inside the
// map func. Since AgentIdentity changes are rare (rotation/revocation, not a
// per-turn event) and reconcileCredentialGrants is idempotent (a no-op write
// when the desired grant set is unchanged — see applyGrantDiff), a
// namespace-wide over-enqueue on every non-terminal session is correct and
// far simpler than threading class resolution through the watch. Terminal
// sessions (per isTerminalPhase) are excluded: they have no live credential
// consumer to refresh.
func sessionsForAgentIdentityChange(items []spiceboxv1alpha1.AgentSession, aiNamespace string) []reconcile.Request {
	var out []reconcile.Request
	for i := range items {
		s := &items[i]
		if s.Namespace == aiNamespace && !isTerminalPhase(s.Status.Phase) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(s)})
		}
	}
	return out
}

// watchedObjectRequests maps a change on one of the non-owned objects this
// controller watches to the AgentSession reconcile requests that change must
// trigger. SetupWithManager registers one Watches(...) per type handled here
// and hands every one of them this single func.
//
// It is a method on the Reconciler rather than closures inside SetupWithManager
// so that "does a change to X wake the sessions that read X?" is answerable in a
// plain unit test against a fake client, with no manager and no envtest. A watch
// registered with no mapping — or one that quietly matches nothing — is
// otherwise invisible until a session wedges waiting out the ~10h resync.
//
// An unhandled type returns nil. controller-runtime delivers only the types
// SetupWithManager watches, so reaching that arm means a Watches(...) was added
// without its mapping.
func (r *Reconciler) watchedObjectRequests(ctx context.Context, o client.Object) []reconcile.Request {
	switch obj := o.(type) {
	case *spiceboxv1alpha1.UserIdentity:
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentSessions for UserIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
				"useridentity", obj.Name, "err", err.Error())
			return nil
		}
		return sessionsForUserIdentityChange(list.Items, obj.Spec.Subject)

	// A ClusterAgentSettings change may affect all AgentSessions (cross-namespace).
	case *spiceboxv1alpha1.ClusterAgentSettings:
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list AgentSessions for ClusterAgentSettings watch failed; dropping re-enqueue (self-heals on next resync)",
				"clusteragentsettings", obj.GetName(), "err", err.Error())
			return nil
		}
		return requestsForAll(list.Items)

	// An AgentSettings change affects AgentSessions in its namespace only.
	case *spiceboxv1alpha1.AgentSettings:
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.Client.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentSessions for AgentSettings watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentsettings", obj.GetName(), "namespace", obj.GetNamespace(), "err", err.Error())
			return nil
		}
		return requestsForAll(list.Items)

	// An AgentIdentity change (credential rotation / revocation) may affect any
	// AgentSession in its namespace whose AgentClass resolves credentials
	// against it (see sessionsForAgentIdentityChange for why this is
	// namespace-wide rather than a precise per-session match).
	case *spiceboxv1alpha1.AgentIdentity:
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.Client.List(ctx, &list, client.InNamespace(obj.Namespace)); err != nil {
			log.FromContext(ctx).Info("list AgentSessions for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
				"agentidentity", obj.Name, "namespace", obj.Namespace, "err", err.Error())
			return nil
		}
		return sessionsForAgentIdentityChange(list.Items, obj.Namespace)

	// An AgentClass change un-parks every session gated on it. Reconcile stops at
	// ClassResolved=False while the class is missing and again while it is not
	// Valid=True, returning NO RequeueAfter from either branch, so this watch is
	// the only path back for a parked session: a class creation covers the
	// missing branch, a status write the not-Valid one. It also reaches LIVE
	// sessions, which a class respec must.
	case *spiceboxv1alpha1.AgentClass:
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.Client.List(ctx, &list, client.InNamespace(obj.Namespace)); err != nil {
			log.FromContext(ctx).Info("list AgentSessions for AgentClass watch failed; dropping re-enqueue (sessions parked on this class stay parked until the next resync)",
				"agentclass", obj.Name, "namespace", obj.Namespace, "err", err.Error())
			return nil
		}
		return sessionsForAgentClassChange(list.Items, obj.Namespace, obj.Name)

	case *spiceboxv1alpha1.CredentialUpdateRequest:
		return mapCredentialUpdateRequestToSession(ctx, obj)
	}
	log.FromContext(ctx).Info("watchedObjectRequests: no mapping for watched type; dropping re-enqueue",
		"type", fmt.Sprintf("%T", o), "object", o.GetNamespace()+"/"+o.GetName())
	return nil
}

// requestsForAll re-enqueues every session in items, without filtering.
func requestsForAll(items []spiceboxv1alpha1.AgentSession) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(items))
	for i := range items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&items[i])})
	}
	return out
}

// sessionsForAgentClassChange returns the AgentSessions in acNs whose
// spec.class names acName — terminal ones included.
//
// Unlike the AgentIdentity mapping, this match is exact rather than
// namespace-wide: spec.class is the very field Reconcile resolves the class by,
// so there is nothing to over-approximate. Terminal sessions used to be
// excluded ("past every gate the class could re-open"), but that stopped
// being true with spec.authz.session.artifactVisibility: the org-wide
// audience levels on completed sessions too — their artifacts are exactly the
// ones shared after the fact — and, since this map func only sees the NEW
// class object, a flip back to "session" is indistinguishable here from any
// other write; only the per-session reconcile can apply the revocation, so
// every flip must reach every session. A terminal session's reconcile levels
// the tuple and then short-circuits at the pod reap as before.
func sessionsForAgentClassChange(items []spiceboxv1alpha1.AgentSession, acNs, acName string) []reconcile.Request {
	var out []reconcile.Request
	for i := range items {
		s := &items[i]
		if s.Namespace == acNs && s.Spec.Class == acName {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(s)})
		}
	}
	return out
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(r.watchedObjectRequests)
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentSession{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&spiceboxv1alpha1.SpiceboxSession{}).
		Owns(&spiceboxv1alpha1.SessionUserIdentity{}).
		Watches(&spiceboxv1alpha1.UserIdentity{}, enqueue).
		Watches(&spiceboxv1alpha1.ClusterAgentSettings{}, enqueue).
		Watches(&spiceboxv1alpha1.AgentSettings{}, enqueue).
		Watches(&spiceboxv1alpha1.AgentIdentity{}, enqueue).
		Watches(&spiceboxv1alpha1.AgentClass{}, enqueue).
		Watches(&spiceboxv1alpha1.CredentialUpdateRequest{}, enqueue).
		Watches(&spiceboxv1alpha1.SessionHold{}, handler.EnqueueRequestsFromMapFunc(mapSessionHoldToSession)).
		Complete(r)
}

// getSecret returns the Secret identified by nn, using the guarded reader when
// SecretReader is set, and falling back to the unguarded client otherwise (e.g.
// tests that do not inject adoptguard).
func (r *Reconciler) getSecret(ctx context.Context, nn types.NamespacedName) (*corev1.Secret, error) {
	if r.SecretReader != nil {
		return r.SecretReader.Get(ctx, nn)
	}
	var s corev1.Secret
	return &s, r.Client.Get(ctx, nn, &s)
}

// getConfigMap returns the ConfigMap identified by nn, using the guarded reader
// when ConfigMapReader is set, and falling back to the unguarded client otherwise.
func (r *Reconciler) getConfigMap(ctx context.Context, nn types.NamespacedName) (*corev1.ConfigMap, error) {
	if r.ConfigMapReader != nil {
		return r.ConfigMapReader.Get(ctx, nn)
	}
	var cm corev1.ConfigMap
	return &cm, r.Client.Get(ctx, nn, &cm)
}

// Reconcile is the main reconciliation loop for AgentSession.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// This reconciler reaches *memory.Local directly rather than over HTTP —
	// DeleteScope on teardown, the audit-chain-heads read, and ReconcileRestart's
	// memory copies all run inside this Reconcile on this ctx. Mint the system
	// approval up front so they pass the facade's capability door, which denies
	// any caller that arrives without one.
	ctx = memory.WithSystemApproval(ctx, "operator:agentsession-controller")

	var sess spiceboxv1alpha1.AgentSession
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &sess); !cont {
		return ctrl.Result{}, err
	}

	if !sess.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &sess)
	}
	if sess.Spec.GoalExecution != nil {
		if r.GoalValidator == nil {
			return ctrl.Result{}, fmt.Errorf("goal session validator unavailable")
		}
		if err := r.GoalValidator.ValidateGoalSession(ctx, &sess); err != nil {
			return ctrl.Result{}, fmt.Errorf("goal session activation refused: %w", err)
		}
	}
	// Snapshot the session as read so every status write below sends only the
	// fields this reconcile actually changes; agentstatus.WriteOwned diffs
	// against it.
	ctx = withReconcileOriginal(ctx, sess.DeepCopy())
	if addedFinalizer, ferr := apreconcile.EnsureFinalizer(ctx, r.Client, &sess, spiceboxv1alpha1.FinalizerAgentSession); addedFinalizer || ferr != nil {
		if ferr != nil {
			return ctrl.Result{}, ferr
		}
		// Admission sweep: this is the ONE reconcile per creation where the
		// finalizer transitions absent -> present (EnsureFinalizer's contract),
		// so it is the natural once-per-(ns,name) hook for wiping whatever a
		// DEAD PREDECESSOR with this same name left behind in SpiceDB. A
		// slot_pin tuple never expires, so a reused name would otherwise
		// inherit a stale predecessor's pin and be refused its own first
		// bind — DeleteSlotGrants is the same call finalize() makes on
		// teardown, and sweeping it again here, before this session's own
		// first grant can be written, is what closes that window. Fail
		// closed: an error here must requeue rather than let the new session
		// proceed while a leftover pin from someone else's session is still
		// live under its name.
		//
		// EXCEPT a fork child. The parent's ReconcileRestart copies the
		// parent's slot grants AND pin onto the child's (ns, name) —
		// authz.CopySlotGrants, restart.go step 6c — and BuildChildSession
		// creates the child with no finalizer, so that copy lands BEFORE this
		// reconcile runs. Sweeping here would be the last writer and would
		// silently strip every non-takeover fork of its inherited authority
		// (and could double-pin under the PVC-restore requeue, wedging
		// EnsurePin's one-pin read). The sweep guards against a stale write
		// from a dead predecessor REUSING this name; a fork child's name is
		// generated (PendingRestart.TargetSessionName), so that reuse cannot
		// happen to it, and the tuples already present under its name are
		// legitimate by construction. ForkedFrom is the same signal
		// BuildChildSession stamps on every restart child.
		if r.SpiceDBDeleter != nil && sess.Spec.ForkedFrom == "" {
			if err := r.SpiceDBDeleter.DeleteSlotGrants(ctx, sess.Namespace, sess.Name); err != nil {
				log.FromContext(ctx).Error(err, "admission sweep: delete stale slot grants/pins failed",
					"session", sess.Namespace+"/"+sess.Name)
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Restore this session's memory-API bearer token into the in-process registry
	// when this operator does not already hold it. Placed HERE — above every
	// short-circuit below, and immediately under the deletion check that routes a
	// deleting session to finalize, the sole caller of Tokens.Revoke — because
	// the authoritative registration at step 4 is unreachable for a terminal
	// session: the reap a few blocks down returns before it, on every reconcile,
	// forever. See reregisterMemoryToken for why it can neither mint a token nor
	// bring a revoked one back.
	r.reregisterMemoryToken(ctx, &sess)

	// Restart-from-here: when PendingRestart is set, run the fork reconciler.
	// proceed=false short-circuits the normal reconcile; the next watch event
	// re-enters either to continue (idempotent) or to operate on the
	// now-superseded parent.
	if proceed, res, err := r.ReconcileRestart(ctx, &sess); err != nil || !proceed {
		return res, err
	}

	// StartFailure signal: channelsd sets status.startFailure on a terminal
	// pre-start failure, but the operator is the sole producer of the Failed
	// state, so the transition happens here — channelsd never writes
	// phase/failureReason/finishedAt. The !isTerminalPhase guard keeps a
	// crash-restart that re-reads an already-Failed session idempotent.
	if sess.Status.StartFailure != nil && !isTerminalPhase(sess.Status.Phase) {
		return r.markBootFailed(ctx, &sess, sess.Status.StartFailure.Reason, sess.Status.StartFailure.Message)
	}

	// Chain-head anchoring: once a session is terminal, compute its per-publisher
	// final chain heads one time and stamp them on status.
	//
	// The nil check is a compute gate, not a trust decision — walking every
	// publisher's chain should not repeat on each reconcile of a terminal
	// session. Treating a non-nil map as ours is safe only because
	// status.auditChainHeads is refused to session runners at admission
	// (pinnedStatusFields in pkg/controllers/webhooks/agentsession); otherwise a
	// runner could pre-stamp any map and permanently displace the real truncation
	// anchors, which are never recomputed once set. Runs before the AgentClass
	// gate so a class deleted after the session finished cannot block anchoring.
	if isTerminalPhase(sess.Status.Phase) && sess.Status.AuditChainHeads == nil && r.AuditChainHeads != nil {
		if err := r.recordAuditChainHeads(ctx, &sess); err != nil {
			// A chain-head compute/persist error must NOT fail the reconcile of a
			// completed session: reaching terminal matters more than the anchor,
			// which is a verification aid. Log and move on.
			log.FromContext(ctx).Info("audit: failed to record chain heads on completed session",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}
	}

	// Level the opt-in org-wide artifact audience from the class's current
	// artifactVisibility. Placed HERE — above the terminal reap short-circuit,
	// like reregisterMemoryToken and for the same reason: a completed
	// session's reconciles never get past the reap, and completed sessions
	// are precisely the ones whose artifacts get shared after the fact. Both
	// directions matter at this placement: a class flipped to "organization"
	// must expose finished sessions' artifacts, and a class flipped back must
	// REVOKE on them. Errors fail the reconcile (see syncArtifactOrgViewer).
	if err := r.syncArtifactOrgViewer(ctx, &sess); err != nil {
		return ctrl.Result{}, err
	}

	// Reap a terminal session's pods once eligible: Succeeded immediately, Failed
	// after the configured grace so a crash stays debuggable. Otherwise the
	// bundle SpiceboxSessions, runner pod and detector/cosidecar pods sit Running
	// until retention GCs the AgentSession, and accumulated dead sessions strand
	// CPU and block fresh pods from scheduling. The AgentSession itself — with
	// its PVCs, Secrets, RBAC and SpiceDB relationships — is KEPT for inspection.
	// This short-circuits the rest of reconcile so the bundle-provisioning loop
	// cannot re-create what was just reaped; within the Failed grace it requeues
	// after the remaining time so the reap needs no external event. Runs before
	// the AgentClass gate so a terminal session whose class was deleted reaps.
	//
	// EXCEPT a wake-annotated archive-swept session. The sweep parks a channel
	// session at Succeeded and a fresh inbound resumes it, but this reap
	// short-circuits before the wake handler runs, so reaping here would strand
	// the resume at Succeeded forever. Fall through instead; the wake handler
	// tears down any prior pod itself. Only a Succeeded+Archived session is both
	// reapable and wakeable — a Failed one is never wakeable — so this narrows to
	// exactly the resume case.
	if !shouldWake(&sess) {
		// Storage retention runs alongside the pod reap and must thread its
		// requeue through every branch below: after the pods are reaped, a
		// terminal session's reconciles keep short-circuiting in this block,
		// so a deadline scheduled anywhere later would never fire. An error
		// is logged, never fatal — reaping the pods matters more, and both
		// the deletes and the marker write are idempotent on the next pass.
		reclaimAfter, rerr := r.reconcileStorageReclaim(ctx, &sess)
		if rerr != nil {
			log.FromContext(ctx).Info("storage reclaim failed; retried on the next reconcile",
				"session", sess.Namespace+"/"+sess.Name, "err", rerr.Error())
		}
		switch action, remaining := terminalReapAction(sess.Status.Phase, sess.Status.FinishedAt, sess.CreationTimestamp, r.FailedSandboxReapGrace, r.now()); action {
		case reapActionRequeue:
			if reclaimAfter > 0 && reclaimAfter < remaining {
				remaining = reclaimAfter
			}
			return ctrl.Result{RequeueAfter: remaining}, nil
		case reapActionReap:
			res, err := r.reapSessionPods(ctx, &sess)
			if err == nil && reclaimAfter > 0 && (res.RequeueAfter == 0 || reclaimAfter < res.RequeueAfter) {
				res.RequeueAfter = reclaimAfter
			}
			return res, err
		case reapActionNone:
			// Not eligible (non-terminal phase, no finishedAt, or reaping
			// disabled) — fall through to normal reconcile. A pending
			// retention deadline still needs a wakeup that nothing later in
			// the terminal path schedules, so return it here for a terminal
			// session rather than falling through to a reconcile that ends
			// with no requeue.
			if reclaimAfter > 0 && isTerminalPhase(sess.Status.Phase) {
				return ctrl.Result{RequeueAfter: reclaimAfter}, nil
			}
		}
	}

	// 1. Resolve AgentClass + gate on Valid=True.
	var ac spiceboxv1alpha1.AgentClass
	acKey := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}
	if err := r.Client.Get(ctx, acKey, &ac); err != nil {
		if errors.IsNotFound(err) {
			r.setFalseCondition(&sess, spiceboxv1alpha1.AgentSessionConditionClassResolved,
				spiceboxv1alpha1.ReasonAgentClassMissing, "AgentClass "+sess.Spec.Class+" not found")
			return ctrl.Result{}, r.applyStatus(ctx, &sess)
		}
		return ctrl.Result{}, err
	}
	// Revocation sweep: reconcile externaltoken grants HERE, above every gate
	// that can park the reconcile, so a revoked credential loses its grant
	// regardless of session state. Best-effort; see sweepRevokedGrants for why
	// the authoritative call site below does not suffice alone.
	r.sweepRevokedGrants(ctx, &sess, &ac)

	if !classIsValid(&ac) {
		r.setFalseCondition(&sess, spiceboxv1alpha1.AgentSessionConditionClassResolved,
			spiceboxv1alpha1.ReasonAgentClassNotValid, "AgentClass "+ac.Name+" is not Valid=True yet")
		return ctrl.Result{}, r.applyStatus(ctx, &sess)
	}
	r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionClassResolved,
		spiceboxv1alpha1.ReasonAllReferencesResolve, "")

	// 1b. The start gate. Every entry path lands here; nothing below this line
	// may cost a pod for a session its class's allowlist refuses.
	// `halted || err != nil`, not `halted` alone: the gate's indeterminate arm
	// returns an error, and a non-nil error must reach controller-runtime for
	// the backoff requeue whatever halted says. Dropping it here would turn an
	// unanswered authorization check into a silent fall-through.
	if res, halted, err := r.EnforceStartGate(ctx, &sess, &ac); halted || err != nil {
		return res, err
	}

	// 1c. The workshop sanction (spec layer 1.1). Only after the start gate:
	// an unlisted starter must not cost a namespace.
	if res, halted, err := r.ensureWorkshop(ctx, &sess, &ac); halted || err != nil {
		return res, err
	}

	// 1a. The per-session authz config snapshot authzd derives this session's
	// cold-start authorization policy from. Written HERE — as high as the
	// resolved AgentClass allows, and well above the runner pod — so the record
	// is durable before the process that reads it exists, on a fresh session and
	// a restart alike. See reconcileAuthzSessionConfig for why the operator, not
	// the runner, must author it and why a failure requeues.
	if err := r.reconcileAuthzSessionConfig(ctx, &sess, &ac); err != nil {
		log.FromContext(ctx).Info("authz session config snapshot failed; requeuing before starting the runner",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return ctrl.Result{}, err
	}

	// Settings resolution and enforcement gate: refuses fresh sessions on a fatal
	// violation while grandfathering running ones. Resolves the full 4-tier chain
	// (session budget included) and stamps the snapshot plus SettingsAccepted.
	// The image-pin block gate is evaluated HERE, before the single persist, so
	// SettingsAccepted settles in one write instead of flapping True→False every
	// reconcile. Persisted BEFORE the runner pod is created, because the runner
	// reads status.effectiveSettings.
	//
	// llmAPIKey is declared outside the block so the create-path BuildRunnerRBAC
	// call below can consume it.
	llmAPIKey := ""
	{
		eff, vs, err := settingswiring.ResolveForSession(ctx, r.Client, &ac, &sess)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolve settings: %w", err)
		}
		sess.Status.EffectiveSettings = &eff
		if sess.Spec.GoalExecution != nil {
			if err := r.GoalValidator.ConstrainGoalSettings(ctx, &sess, &eff); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Catalog token: when the effective model came from the model catalog
		// rather than bring-your-own, read the central token from its
		// system-namespace Secret and repoint Model.APIKey at the per-session
		// Secret key that will hold it. The runner reads APIKey.Name/Key from
		// status.effectiveSettings, so its mount is unaffected; the value is
		// threaded to BuildRunnerRBAC below, which writes it into the Secret.
		if src := sess.Status.EffectiveSettings.ModelTokenSource; src != nil {
			v, mErr := r.materializeCatalogToken(ctx, src)
			if mErr != nil {
				log.FromContext(ctx).Info("model catalog token resolve failed",
					"session", sess.Namespace+"/"+sess.Name, "tokenRef", src.Namespace+"/"+src.Name, "err", mErr.Error())
				return ctrl.Result{}, fmt.Errorf("materialize catalog token: %w", mErr)
			}
			llmAPIKey = v
			sess.Status.EffectiveSettings.Model.APIKey = spiceboxv1alpha1.SecretKeyRef{
				Name: MemoryTokenSecretName(&sess),
				Key:  agentSessionSecretLLMAPIKey,
			}
		}
		fatal := settingswiring.StampAccepted(&sess, &sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted, vs)
		if fatal && !podAlreadyStarted(&sess) {
			if sess.Status.Phase == "" {
				sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
			}
			return ctrl.Result{}, r.applyStatus(ctx, &sess)
		}
		// Image-pin block gate: evaluated before the persist so the condition
		// settles to False in a single write (no True→False churn on every
		// reconcile). Mirrors the fatal-settings gate above: blocks fresh
		// sessions only; grandfathers running ones (!podAlreadyStarted).
		if !podAlreadyStarted(&sess) {
			if pinMsg, pinErr := r.imagePinGateViolation(ctx, &sess, &ac, &eff); pinErr != nil {
				return ctrl.Result{}, pinErr
			} else if pinMsg != "" {
				conditions.SetFalse(&sess, &sess.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionSettingsAccepted,
					spiceboxv1alpha1.ReasonAgentSessionImagePinDrifted, pinMsg)
				if sess.Status.Phase == "" {
					sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
				}
				// Re-evaluate periodically: the gate's input is SidecarToolbox
				// status, which this controller does not watch; a refreeze or
				// un-drift on the toolbox unblocks the session within a minute.
				if err := r.applyStatus(ctx, &sess); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
		}
		// Settings accepted: record the operator-pre boundary event once, on the
		// transition. The projection sets the phase floor; later reconciles see
		// the condition already True and do not re-append.
		if conditionBecameTrue(reconcileOriginal(ctx), &sess, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted) {
			if err := r.applyEvent(ctx, &sess, lifecyclecore.SettingsAccepted{}); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Persist the snapshot now so the about-to-be-created runner pod can read it.
		if err := r.applyStatus(ctx, &sess); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 1a-start. Start-approval gate. A session an org non-member started sits
	// parked (no runner, no owner resolution, no standing) until a platform
	// admin approves — channelsd's Create-time marker annotation is the park
	// signal, its removal the unpark. Runs BEFORE the identity gates: an
	// unapproved starter must not be prompted for identity choices.
	if res, proceed, err := r.reconcileStartApproval(ctx, &sess); err != nil || !proceed {
		return res, err
	}

	// 1a-identity. Identity-choice gate. A static class mirrors spec.identityMode
	// onto status.effectiveIdentityMode once. An ask|dynamic class parks in
	// AwaitingIdentityChoice while the runner drives the 3-way prompt, enforces
	// the choice deadline as a backstop, and mirrors the resolved choice onto
	// status. Runs BEFORE the passthrough gate so a userPassthrough choice flows
	// into it.
	if res, proceed, err := r.reconcileIdentityChoice(ctx, &sess, &ac); err != nil || !proceed {
		return res, err
	}

	// 1b. Passthrough-identity gate. For identityMode=userPassthrough this
	// binds the starter's UserIdentity subset and may park the session in
	// AwaitingCredentials. proceed=false → return immediately.
	if res, proceed, err := r.reconcilePassthroughIdentity(ctx, &sess, &ac); err != nil || !proceed {
		return res, err
	}

	// 1c. Skill bundles. Resolve the AgentClass's opted-in skills and stage each
	// bundled one into a per-session ConfigMap. Runs BEFORE the bundle
	// SpiceboxSessions are created so the resolved {mountName, configMapName}
	// pairs can be threaded onto each bundle's spec.skillBundles, which the
	// sandbox pod builder mounts and untars at /skills. Idempotent, so a
	// wake-respawn re-stages harmlessly. Per-skill misses are logged and skipped
	// — one bundle miss must never fail the session — and a nil BundleStore is a
	// no-op, leaving skillBundles nil and the sandbox pods without /skills.
	if r.BundleStore != nil {
		staged, stageErr := r.resolveAndStageSkillBundles(ctx, &sess, &ac)
		if stageErr != nil {
			return ctrl.Result{}, fmt.Errorf("stage skill bundles: %w", stageErr)
		}
		sess.Status.ResolvedSkillBundles = staged
	}

	// Stamp per-session NetworkPolicies before any session pod exists,
	// so an enforcing CNI never sees an unconstrained pod.
	//
	// The namespace-wide floor goes first and is a separate opt-in: the
	// per-session policies only select pods carrying session labels, so a pod
	// created in this namespace by anything else — a Job an agent was able to
	// create, or one of the platform's own workspace Jobs — matches no selector
	// and is unrestricted in both directions. See NetpolConfig.NamespaceDefaultDeny
	// for why AP does not turn that on for an operator.
	if err := r.ensureNamespaceDefaultDeny(ctx, &sess); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRunnerNetworkPolicy(ctx, &sess); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve the bound Channel CRs so the per-session Role can pin rules for
	// them and their credentials Secrets, and so owner resolution can inspect
	// channel.spec.owner. A lookup failure proceeds without the pinned rules and
	// the runner's mention lookup degrades gracefully.
	//
	// BOTH input and output Channels are resolved: the runner picks its channel
	// via resolve.ForSession, which prefers OutputChannel, and authenticates the
	// directory API with THAT channel's bot token. Granting only the input pair
	// leaves a cron session unable to read either, silently disabling every
	// channel-sourced capability.
	var channelSecretNames []string
	var inputCh *spiceboxv1alpha1.Channel
	for _, b := range []*spiceboxv1alpha1.ChannelBinding{sess.Spec.InputChannel, sess.Spec.OutputChannel} {
		if b == nil || b.Name == "" {
			continue
		}
		var ch spiceboxv1alpha1.Channel
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: sess.Namespace, Name: b.Name,
		}, &ch); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("get channel %s: %w", b.Name, err)
			}
			continue
		}
		if n := ch.Spec.CredentialsRef.SecretName; n != "" {
			channelSecretNames = append(channelSecretNames, n)
		}
		// inputCh feeds owner resolution, which reads the INPUT channel's policy.
		if b == sess.Spec.InputChannel {
			inputCh = ch.DeepCopy()
		}
	}

	// Resolve and write agentsession#owner — an idempotent TOUCH, run every
	// reconcile so a policy change on the Channel lands promptly. An error is
	// logged and skipped, fail-closed: no owner means the SpiceDB gates deny.
	//
	// ORDERING IS LOAD-BEARING: this MUST stay ahead of the bundle/detector
	// provisioning gates below. Those return early on every pass while a bundle
	// comes up, on the retry-once path, and terminally via markBootFailed, so
	// anything after them never runs at all for a session whose bundles never
	// reach Ready. That leaves a session with a started_by tuple and no owner,
	// denying its OWN starter every gate resolved through #owner: fork, interact,
	// manage_scope, approve.
	if err := r.ResolveAndWriteOwners(ctx, &sess, inputCh, &ac); err != nil {
		log.FromContext(ctx).Info("owner resolution failed; session left ownerless (fail-closed)",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}

	// 2. Bundle SpiceboxSessions, or the vacuous NoBundles path. Only an
	// active/pending turn provisions; every parked phase (Idle, AwaitingRetry,
	// AwaitingDecision, AwaitingCredentials) and every terminal phase skips this
	// block, so a reaped or parked Idle session stays at zero pods instead of
	// bouncing straight back — see sandboxProvisioningDesired.
	if sandboxProvisioningDesired(sess.Status.Phase) {
		if len(ac.Spec.ToolBundles) == 0 {
			r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionBundlesReady, "NoBundles", "")
		} else {
			// Provision the shared workspace PVC once per AgentSession. An empty
			// claim name means no StorageClass is configured, so bundle sessions
			// fall back to Mode: isolated — a pod-local /work emptyDir.
			claimName, err := r.ensureWorkspacePVC(ctx, &sess)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure workspace PVC: %w", err)
			}
			// Backstop for the node-local workspace class: warn cluster monitors
			// when a node hosting session workspaces is running out of ephemeral
			// storage, before it deadlocks. Self-gating (node-pinned class +
			// monitoring configured) and best-effort — never fails provisioning.
			r.maybeWarnNodeDiskPressure(ctx)
			if res, done, err := r.ensureWorkspaceOverlay(ctx, &sess, &ac, claimName); err != nil || !done {
				return res, err
			}
			if _, err := EnsureSnapshotStorePVC(ctx, r, &sess); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure snapshot-store PVC: %w", err)
			}

			// Resolve identity per bundle and create per-bundle SpiceboxSessions.
			allReady := true
			notReadyBundles := make([]string, 0, len(ac.Spec.ToolBundles))
			notReadyPods := make([]string, 0, len(ac.Spec.ToolBundles))
			// earliestBundleStart is the epoch the readiness deadline is measured
			// from: the earliest CreationTimestamp among the not-ready bundle
			// SpiceboxSessions. Under userPassthrough these are not created until
			// the AwaitingCredentials gate clears, so this — not
			// sess.CreationTimestamp — is when provisioning actually began.
			var earliestBundleStart time.Time
			bundleSessionStatuses := make([]spiceboxv1alpha1.ResolvedBundle, 0, len(ac.Spec.ToolBundles))
			for _, b := range ac.Spec.ToolBundles {
				identity := b.AgentIdentity
				if identity == "" {
					identity = sess.Spec.AgentIdentity
				}
				if identity == "" {
					identity = ac.Spec.AgentIdentity
				}

				if err := r.ensureSandboxNetworkPolicy(ctx, &sess, b); err != nil {
					return ctrl.Result{}, err
				}
				// The tier-resolved sandbox backend for this bundle, or nil when no
				// tier expressed a preference — BuildBundleSession then leaves
				// spec.sandbox unset, so the SpiceboxClass's own setting stands.
				var resolvedSandbox *spiceboxv1alpha1.SandboxBackend
				if eff := sess.Status.EffectiveSettings; eff != nil {
					if backend, ok := eff.Sandbox[b.Name]; ok {
						resolvedSandbox = &backend
					}
				}
				desired := BuildBundleSession(&sess, b, identity, claimName, sess.Status.ResolvedSkillBundles, resolvedSandbox, ac.Spec.Config)
				// Server-side apply (idempotent re-create).
				if err := r.Client.Patch(ctx, desired,
					client.Apply, client.ForceOwnership, client.FieldOwner("agentsession-bundle"),
				); err != nil {
					return ctrl.Result{}, fmt.Errorf("apply bundle SpiceboxSession %q: %w", desired.Name, err)
				}

				// Re-fetch and check Ready.
				var live spiceboxv1alpha1.SpiceboxSession
				if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: desired.Name}, &live); err != nil {
					return ctrl.Result{}, err
				}
				// Carry the retry ledger forward: bundleSessionStatuses is rebuilt
				// every pass, but Restarts and RetriedSessionUID are
				// controller-owned observations that must survive the rebuild.
				var prior spiceboxv1alpha1.ResolvedBundle
				for _, pb := range sess.Status.BundleSessions {
					if pb.Name == b.Name {
						prior = pb
						break
					}
				}

				switch {
				case !live.DeletionTimestamp.IsZero():
					// Mid-teardown, ours or external: not a failure signal, since
					// the SSA apply above re-creates it once the delete completes.
					// Treat as not-ready and move on.

				case hasFailedSession(&live) && !isTerminalForPodLifecycle(sess.Status.Phase) &&
					string(live.UID) == prior.RetriedSessionUID:
					// Either a stale cache read of the instance the retry already
					// deleted, or a retry delete that never landed because the
					// reconcile errored between the ledger write and the delete.
					// Re-issuing with a UID precondition covers both: a stale read
					// no-ops with NotFound, a real leftover is removed so the retry
					// proceeds, and the precondition makes it impossible to delete
					// the replacement.
					uid := live.UID
					if err := r.Client.Delete(ctx, &live, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
						return ctrl.Result{}, fmt.Errorf("re-delete retried bundle SpiceboxSession %q: %w", desired.Name, err)
					}

				case hasFailedSession(&live) && !isTerminalForPodLifecycle(sess.Status.Phase) &&
					prior.Restarts == 0:
					// First failure: retry once. Persist the ledger BEFORE deleting,
					// fail-closed — a failed status write has destroyed nothing, and
					// a delete that fails after it is re-issued by the retried-UID
					// case above, so the retry still happens, bounded by the ledger
					// rather than looping unboundedly. The SSA apply at the top of
					// this loop re-creates the bundle on the next pass.
					logger := log.FromContext(ctx)
					logger.Info("bundle SpiceboxSession failed; retrying once",
						"session", sess.Namespace+"/"+sess.Name,
						"bundle", b.Name, "spiceboxSession", desired.Name,
						"detail", failedSessionDetail(&live))
					prior.Restarts = 1
					prior.RetriedSessionUID = string(live.UID)
					// Record the updated ledger on this pass's status slice; the
					// entry for this bundle is appended below with these values.
					retrying := prior
					retrying.Name = b.Name
					retrying.SpiceboxSessionName = desired.Name
					retrying.AgentIdentity = identity
					sess.Status.BundleSessions = upsertResolvedBundle(sess.Status.BundleSessions, retrying)
					if err := r.applyStatus(ctx, &sess); err != nil {
						return ctrl.Result{}, fmt.Errorf("record bundle retry for %q: %w", desired.Name, err)
					}
					uid := live.UID
					if err := r.Client.Delete(ctx, &live, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
						return ctrl.Result{}, fmt.Errorf("delete failed bundle SpiceboxSession %q for retry: %w", desired.Name, err)
					}
					return ctrl.Result{RequeueAfter: bundleRetryRequeue}, nil

				case hasFailedSession(&live) && !isTerminalForPodLifecycle(sess.Status.Phase):
					// Second failure — the replacement failed too — so terminal.
					// Record the coarse provisioning-failure transition; the precise
					// cause goes on the Failed condition below.
					if err := r.applyEvent(ctx, &sess, lifecyclecore.ProvablyUnschedulable{}); err != nil {
						return ctrl.Result{}, err
					}
					sess.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionBundleFail
					now := metav1.Now()
					sess.Status.FinishedAt = &now
					msg := fmt.Sprintf("bundle SpiceboxSession %q is Failed (after 1 retry)", desired.Name)
					if detail := failedSessionDetail(&live); detail != "" {
						// Carry the real cause (e.g. "PodStartFailed: ImagePullBackOff — …")
						// so channelsd relays WHY to the user, not just that a bundle failed.
						msg = fmt.Sprintf("bundle SpiceboxSession %q failed after 1 retry: %s", desired.Name, detail)
					}
					conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
						Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
						Reason:  spiceboxv1alpha1.ReasonAgentSessionBundleFail,
						Message: msg,
					})
					return ctrl.Result{}, r.applyStatus(ctx, &sess)
				}
				if !isSessionReady(&live) {
					allReady = false
					notReadyBundles = append(notReadyBundles, desired.Name)
					if live.Status.PodName != "" {
						notReadyPods = append(notReadyPods, live.Status.PodName)
					}
					// Measured from when this bundle's provisioning started, not
					// from session creation.
					earliestBundleStart = earlierNonZero(earliestBundleStart, live.CreationTimestamp.Time)
				}
				bundleSessionStatuses = append(bundleSessionStatuses, spiceboxv1alpha1.ResolvedBundle{
					Name:                b.Name,
					SpiceboxSessionName: desired.Name,
					AgentIdentity:       identity,
					Restarts:            prior.Restarts,
					RetriedSessionUID:   prior.RetriedSessionUID,
				})
			}

			if !allReady {
				// Storage fast-fail: a bundle pod can never schedule while its
				// workspace volume never binds, so if the workspace PVC has a
				// ProvisioningFailed event (the provider rejected it — Filestore's
				// minimum share size, a disabled cloud API, quota, a missing class)
				// fail NOW with that ACTUAL reason instead of waiting out
				// bundleReadyDeadline behind the generic "did not become Ready".
				// claimName is "" under the isolated fallback (no StorageClass), and
				// firstPVCProvisioningFailure is fail-safe on an empty name / no
				// event, so this cleanly no-ops into the checks below when there is
				// no storage problem.
				if msg, ok := r.firstPVCProvisioningFailure(ctx, sess.Namespace, []string{claimName}); ok {
					sess.Status.BundleSessions = bundleSessionStatuses
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonWorkspaceProvisioningFailed, msg)
				}
				// Provable-unschedulable fast-fail: if a not-ready bundle pod requests
				// more cpu/memory/ephemeral-storage than the largest node in the
				// cluster today can ever offer, fail NOW with the specific reason
				// instead of waiting out bundleReadyDeadline. This is the honest
				// bound and nothing more — on an autoscaling cluster a bigger node
				// could still show up later, so the check can be late but is never
				// wrong in the other direction. Fail-safe (no nodes RBAC / pod absent
				// / transient → falls through to the deadline backstop below).
				if reason, ok := r.provablyUnschedulableBundle(ctx, sess.Namespace, notReadyPods); ok {
					sess.Status.BundleSessions = bundleSessionStatuses
					// The condition Reason/FailureReason token stays the CamelCase
					// "BundleFailed" constant — it must satisfy the k8s Condition.Reason
					// pattern (no spaces/punctuation) AND it is the exact-match lookup
					// key pkg/agent/session/lifecycle's transientBootFailures uses to
					// classify a follow-up message as recoverable (DispNewInheriting)
					// rather than a hard refuse; changing it would silently downgrade a
					// transient capacity blip into a dead-end session. Only the Message
					// (what channelsd relays to the user's thread as "⚠️ Agent failed:
					// …") changes: lead with a clean, user-facing capacity explanation,
					// then keep the scheduler's technical detail for operators running
					// `kubectl describe`.
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionBundleFail,
						fmt.Sprintf("couldn't get compute capacity to run this — try again shortly (%s; reduce the bundle's resource request or use larger nodes)", reason))
				}
				// No-silent-hang backstop: a bundle pod that never becomes Ready (a
				// cold image pull that never completes, a crashing container, …) is
				// not Failed — the terminal branch above never fires — so without a
				// deadline this requeues every 2s forever and the user waits with no
				// feedback. Past bundleReadyDeadline measured from provisioning start
				// (the earliest not-ready bundle's CreationTimestamp, NOT session
				// creation — a userPassthrough session's credential-link wait must not
				// count), fail closed and name the offending bundle(s): markBootFailed
				// sets Phase=Failed, which channelsd relays to the channel.
				bundleDeadline := r.effectiveBundleReadyDeadline(ctx)
				if bundleProvisioningTimedOut(earliestBundleStart, r.now(), bundleDeadline) {
					sess.Status.BundleSessions = bundleSessionStatuses
					// Name the ACTUAL blocker. provablyUnschedulableBundle above only
					// fast-fails a RESOURCE-ceiling block (request exceeds the largest
					// node); a pod stuck on a taint or a volume-node-affinity conflict
					// is invisible to it and lands here. So ask the scheduler directly:
					// if a pod is still Pending unschedulable, report THAT reason. The
					// message becomes the terminal Failed condition, which channelsd
					// relays to the user's thread — asserting "the pod is schedulable,
					// likely an image pull" for a taint-blocked pod (as this once did)
					// sends the user chasing a cause that isn't the problem.
					var msg string
					bundlesWereReady := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
					if stall, ok := r.firstSchedulingStall(ctx, sess.Namespace, notReadyPods); ok {
						msg = fmt.Sprintf("sandbox bundle(s) %v did not become Ready within %s of provisioning start: %s is unschedulable: %s",
							notReadyBundles, bundleDeadline, stall.podName, strings.TrimSuffix(stall.reason, "."))
					} else if bundlesWereReady != nil && bundlesWereReady.Status == metav1.ConditionTrue {
						// The bundles all became Ready earlier in this session's life —
						// the durable BundlesReady condition is the record — so this is
						// not a provisioning problem at all: a bundle that was serving
						// turned unhealthy mid-session (an OOM-killed or crashed
						// container, an evicted pod). The deadline math cannot tell the
						// two apart — mid-session, "now − bundle creation" is always
						// past the deadline — and blaming a slow image pull here once
						// sent an operator chasing image pulls for a cgroup OOM kill.
						readySince := bundlesWereReady.LastTransitionTime.UTC().Format(time.RFC3339)
						if term, ok := r.firstContainerTermination(ctx, sess.Namespace, notReadyPods); ok {
							// kubelet already recorded the death — name it (an OOM
							// kill shows as OOMKilled/137) instead of guessing.
							msg = fmt.Sprintf("sandbox bundle(s) %v became unhealthy after running (Ready since %s): %s",
								notReadyBundles, readySince, term.message())
						} else {
							msg = fmt.Sprintf("sandbox bundle(s) %v became unhealthy after running (Ready since %s): a pod stopped being Ready mid-session — likely a crashed or OOM-killed container, or an evicted pod",
								notReadyBundles, readySince)
						}
					} else if term, ok := r.firstContainerTermination(ctx, sess.Namespace, notReadyPods); ok {
						msg = fmt.Sprintf("sandbox bundle(s) %v did not become Ready within %s of provisioning start: %s",
							notReadyBundles, bundleDeadline, term.message())
					} else {
						msg = fmt.Sprintf("sandbox bundle(s) %v did not become Ready within %s of provisioning start; the pod(s) scheduled but never became Ready, so the likely cause is a slow/cold image pull or a crashing container",
							notReadyBundles, bundleDeadline)
					}
					if len(notReadyPods) > 0 {
						msg += fmt.Sprintf(" — run `kubectl -n %s describe pod %s` and `kubectl -n %s logs <pod>` to see why",
							sess.Namespace, strings.Join(notReadyPods, " "), sess.Namespace)
					}
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionBundleFail, msg)
				}
				// No silent "starting…": when a not-ready bundle pod is stuck Pending
				// because the scheduler cannot place it, surface the scheduler's own
				// reason on the condition message rather than the generic "waiting
				// for bundles" text, so the channel indicator and `kubectl get
				// agentsession` show WHY. Past a short grace it also fans a one-shot
				// capacity warning to the monitoring channel. Fail-safe: no stall, or
				// no pod, falls back to the generic message.
				bundlesMsg := "waiting for bundle SpiceboxSessions to become Ready"
				if stall, ok := r.firstSchedulingStall(ctx, sess.Namespace, notReadyPods); ok {
					bundlesMsg = stall.message()
					r.maybeEmitUnschedulable(ctx, &sess, spiceboxv1alpha1.AgentSessionConditionBundlesReady, stall)
				} else {
					// No scheduling stall right now — still starting for a benign
					// reason, or no not-ready pod at all. Reflect that a
					// previously-stuck capacity problem has cleared, even though the
					// bundle itself is not Ready yet.
					r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
						spiceboxv1alpha1.ReasonSandboxScheduled, "")
				}
				r.setFalseCondition(&sess, spiceboxv1alpha1.AgentSessionConditionBundlesReady,
					"BundlesProvisioning", bundlesMsg)
				sess.Status.BundleSessions = bundleSessionStatuses
				return ctrl.Result{RequeueAfter: 2 * time.Second}, r.applyStatus(ctx, &sess)
			}

			sess.Status.BundleSessions = bundleSessionStatuses
			r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionBundlesReady,
				"AllBundlesReady", "")
			// All bundle pods reached Ready this pass, so clear any capacity stall
			// recorded earlier — this pass may have jumped straight from "stalled"
			// to "Ready" with no intermediate not-stalled pass.
			r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
				spiceboxv1alpha1.ReasonSandboxScheduled, "")
		}
	}

	// 2b. Metaagent channel membership: with scope enabled on the AgentClass,
	// keep the MetaagentChannelMembership condition on the bound Channel
	// current. Best-effort — errors are logged and never block startup.
	if err := r.ensureMetaagentChannelMembership(ctx, &sess, &ac); err != nil {
		log.FromContext(ctx).Info("ensureMetaagentChannelMembership errored; continuing",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}

	// 3. RBAC + memory-token Secret. Token is generated once and stored.

	// Resolve the tool credential pin set. When the AgentClass references tools
	// that resolve credentials runner-side — MCPServers or sandbox toolBundles —
	// and names an AgentIdentity, the runner reads that identity and its
	// credential-backed Secrets, so the per-session Role pins
	// agentidentities/secrets `get` to exactly those names. A not-yet-created
	// AgentIdentity requeues rather than falling back to an unpinned Role.
	// SidecarToolboxes resolve creds controller-side and do not drive this pin.
	mcpAgentIdentityName := ""
	var mcpSecretNames []string
	if (len(ac.Spec.MCPServers) > 0 || len(ac.Spec.ToolBundles) > 0) && ac.Spec.AgentIdentity != "" {
		var ai spiceboxv1alpha1.AgentIdentity
		aiKey := types.NamespacedName{Namespace: sess.Namespace, Name: ac.Spec.AgentIdentity}
		if err := r.Client.Get(ctx, aiKey, &ai); err != nil {
			if errors.IsNotFound(err) {
				log.FromContext(ctx).Info(
					"tool AgentIdentity not found yet; requeuing before building per-session RBAC",
					"session", sess.Namespace+"/"+sess.Name,
					"agentIdentity", ac.Spec.AgentIdentity)
				return ctrl.Result{RequeueAfter: mcpAgentIdentityRequeueAfter}, nil
			}
			return ctrl.Result{}, fmt.Errorf("get tool AgentIdentity %s: %w", ac.Spec.AgentIdentity, err)
		}
		mcpAgentIdentityName = ac.Spec.AgentIdentity
		mcpSecretNames = collectCredentialSecretNames(ctx, &ai)
	}

	memTokenSecretName := MemoryTokenSecretName(&sess)
	var existingSec corev1.Secret
	// argsHashKeyBytes is the per-session 32-byte HMAC key used for both
	// approval-grant arguments_hash values and externaltoken token-value hashing.
	// Hoisted out of the create/update branches so the token-grant reconcile,
	// which runs before RunnerFactory.Start, can bind grant values under it.
	var argsHashKeyBytes []byte
	tokenStr := ""
	bundleNames := make([]string, 0, len(sess.Status.BundleSessions))
	for _, b := range sess.Status.BundleSessions {
		bundleNames = append(bundleNames, b.SpiceboxSessionName)
	}
	// Use the guarded SecretReader when wired (production); fall back to the
	// unguarded client in tests that do not inject adoptguard.
	var err error
	if r.SecretReader != nil {
		var ptr *corev1.Secret
		ptr, err = r.SecretReader.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: memTokenSecretName})
		if err == nil {
			existingSec = *ptr
		}
	} else {
		err = r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: memTokenSecretName}, &existingSec)
	}
	switch {
	case errors.IsNotFound(err):
		tokenStr, err = generateToken()
		if err != nil {
			return ctrl.Result{}, err
		}
		// Per-session HMAC key for approval-grant arguments_hash values.
		// Same shape as the memory token: 32 random bytes, hex-encoded.
		argsHashKey, genErr := generateToken()
		if genErr != nil {
			return ctrl.Result{}, fmt.Errorf("mint args-hash key: %w", genErr)
		}
		argsHashKeyBytes = []byte(argsHashKey)
		// Per-session Ed25519 keypair signing this session's append-only audit
		// entries. The seed goes into the per-session Secret; the public key and
		// keyID anchor on status as the K8s-witnessed trust root for offline
		// verification.
		auditSeedHex, auditPubB64, auditKeyID, genErr := generateAuditKeypair()
		if genErr != nil {
			return ctrl.Result{}, fmt.Errorf("mint audit signing key: %w", genErr)
		}
		sess.Status.AuditPublicKey = auditPubB64
		sess.Status.AuditKeyID = auditKeyID
		// Mint a per-session NATS user JWT scoped to this session's subject tree,
		// for EVERY session (not only channel-attached), whenever the operator
		// holds the install-time NATS identity. A kubectl / `oap agent run` session
		// now DOES touch the bus: it publishes an approval interaction and awaits
		// the decision so a human can drive it with `oap session approve`. Without
		// creds its NATS connect authenticates to nothing, the publish buffers on a
		// dead connection, channelsd never parks the interaction, and the approval
		// can never be answered. The grant is session-scoped either way. A missing
		// identity is a misconfiguration this degrades past.
		natsCreds := ""
		if r.NATSIdentity != nil {
			grant, grantErr := runnerNATSUserGrant(sess.Namespace, sess.Name)
			if grantErr != nil {
				return ctrl.Result{}, fmt.Errorf("build per-session nats grant: %w", grantErr)
			}
			creds, mintErr := apnats.MintUser(r.NATSIdentity, grant)
			if mintErr != nil {
				return ctrl.Result{}, fmt.Errorf("mint per-session nats creds: %w", mintErr)
			}
			natsCreds = creds
		} else if sess.Spec.InputChannel != nil {
			log.FromContext(ctx).Info(
				"NATSIdentity not configured; runner will not get NATS creds",
				"session", sess.Namespace+"/"+sess.Name)
		}
		// Every runner needs SpiceDB, so the operator's resolved token goes into
		// every per-session Secret; the runner mounts it as a file rather than
		// receiving a plaintext env var.
		sa, role, rb, sec, crb := BuildRunnerRBAC(&sess, &ac, tokenStr, bundleNames, channelSecretNames, natsCreds, r.SpiceDBToken, argsHashKey, auditSeedHex, llmAPIKey, mcpAgentIdentityName, mcpSecretNames)
		if natsCreds != "" {
			// nats.ca rides in the same per-session Secret as the creds.
			// BuildRunnerRBAC always initializes sec.Data, so no nil guard.
			sec.Data["nats.ca"] = r.NATSCAPEM
		}
		for _, obj := range []client.Object{sa, role, rb} {
			if err := r.Client.Patch(ctx, obj,
				client.Apply, client.ForceOwnership, client.FieldOwner("agentsession-rbac"),
			); err != nil {
				return ctrl.Result{}, fmt.Errorf("apply RBAC %T %s: %w", obj, obj.GetName(), err)
			}
		}
		// Stamp the adoption label so this operator-minted Secret enters the
		// label-filtered cache and passes the SecretReader guard on later reads.
		adoptguard.WithAdoptedLabel(sec)
		if createErr := r.Client.Create(ctx, sec); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, createErr
		}
		// The toolspec-reader ClusterRoleBinding is cluster-scoped and
		// cannot carry a namespaced owner ref, so it isn't GC'd with the
		// session — it's created here and reaped by the finalizer.
		if createErr := r.Client.Create(ctx, crb); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, fmt.Errorf("create toolspec-reader ClusterRoleBinding %s: %w", crb.Name, createErr)
		}
	case err != nil:
		return ctrl.Result{}, err
	default:
		tokenStr = string(existingSec.Data[agentSessionSecretMemoryToken])
		// A Secret without the args-hash key would hang the runner pod in
		// ContainerCreating on its unconditional SubPath mount, so ensure it
		// here — the only Secret mutation this update path performs.
		if len(existingSec.Data[agentSessionSecretArgsHashKey]) == 0 {
			argsHashKey, genErr := generateToken()
			if genErr != nil {
				return ctrl.Result{}, fmt.Errorf("mint args-hash key: %w", genErr)
			}
			if existingSec.Data == nil {
				existingSec.Data = map[string][]byte{}
			}
			existingSec.Data[agentSessionSecretArgsHashKey] = []byte(argsHashKey)
			if updErr := r.Client.Update(ctx, &existingSec); updErr != nil {
				return ctrl.Result{}, fmt.Errorf("ensure args-hash-key on per-session Secret: %w", updErr)
			}
		}
		// Whether just-minted above or already present, the live Secret now
		// carries the key; surface it for the token-grant reconcile below.
		argsHashKeyBytes = existingSec.Data[agentSessionSecretArgsHashKey]
		// Per-session audit signing key: ensured here for the same reason as the
		// args-hash key, and kept in sync with status.AuditPublicKey/AuditKeyID so
		// the K8s-witnessed trust root survives status loss.
		switch {
		case len(existingSec.Data[agentSessionSecretAuditSigningKey]) == 0:
			// No seed yet: mint a fresh keypair, write the seed, and anchor the
			// public key on status.
			seedHex, pubB64, keyID, genErr := generateAuditKeypair()
			if genErr != nil {
				return ctrl.Result{}, fmt.Errorf("mint audit signing key: %w", genErr)
			}
			if existingSec.Data == nil {
				existingSec.Data = map[string][]byte{}
			}
			existingSec.Data[agentSessionSecretAuditSigningKey] = []byte(seedHex)
			if updErr := r.Client.Update(ctx, &existingSec); updErr != nil {
				return ctrl.Result{}, fmt.Errorf("ensure audit-signing-key on per-session Secret: %w", updErr)
			}
			sess.Status.AuditPublicKey = pubB64
			sess.Status.AuditKeyID = keyID
		default:
			// Seed present: re-derive the public key and stamp status whenever the
			// two disagree. That covers status LOST (truncation) and status FORGED
			// — the runner can patch its own /status, and the operator registers
			// what it finds there as a trusted verify key. The Secret is the source
			// of truth and the seed is never rotated under a live session, so a
			// healthy session converges on the first pass. See syncAuditKeyStatus.
			changed, deriveErr := syncAuditKeyStatus(&sess, string(existingSec.Data[agentSessionSecretAuditSigningKey]))
			if deriveErr != nil {
				return ctrl.Result{}, fmt.Errorf("derive audit public key from existing seed: %w", deriveErr)
			}
			if changed {
				log.FromContext(ctx).Info("audit: status audit key did not match the per-session signing seed; re-derived from the Secret",
					"session", sess.Namespace+"/"+sess.Name, "keyID", sess.Status.AuditKeyID)
			}
		}
		// Keep SA, Role and RoleBinding current; SSA absorbs bundle list changes.
		// The per-session Secret already exists from the first reconcile, so the
		// empty natsCreds/spicedbToken/argsHashKey/auditSigningKey/llmAPIKey
		// arguments make this path discard the returned Secret and leave the live
		// one's data untouched, beyond the ensures above.
		sa, role, rb, _, crb := BuildRunnerRBAC(&sess, &ac, tokenStr, bundleNames, channelSecretNames, "", "", "", "", "", mcpAgentIdentityName, mcpSecretNames)
		for _, obj := range []client.Object{sa, role, rb} {
			if err := r.Client.Patch(ctx, obj,
				client.Apply, client.ForceOwnership, client.FieldOwner("agentsession-rbac"),
			); err != nil {
				return ctrl.Result{}, fmt.Errorf("apply RBAC %T %s: %w", obj, obj.GetName(), err)
			}
		}
		// Re-ensure the toolspec-reader ClusterRoleBinding here too: it has no
		// owner ref keeping it alive, so a session that lost it to a manual
		// deletion or a partial first reconcile must have it restored.
		// Idempotent — AlreadyExists is success.
		if createErr := r.Client.Create(ctx, crb); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, fmt.Errorf("create toolspec-reader ClusterRoleBinding %s: %w", crb.Name, createErr)
		}
	}

	// 4. Register the token with the operator's in-memory authenticator,
	// authorizing artifact reads under the AgentSession's own path and each
	// per-bundle SpiceboxSession path — the runner fetches stdout/stderr
	// artifacts produced by ToolCalls dispatched against those bundle sessions.
	bundleKeys := make([]memory.NamespacedName, 0, len(bundleNames))
	for _, n := range bundleNames {
		bundleKeys = append(bundleKeys, memory.NamespacedName{Namespace: sess.Namespace, Name: n})
	}
	r.Tokens.Set(memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, tokenStr, "", bundleKeys...)

	// Register the per-session audit public key for verify-on-write. Read from
	// status every reconcile — cheap and idempotent — so a restarted operator
	// re-registers a key it never minted.
	if pub, decErr := provenance.DecodePubKey(sess.Status.AuditPublicKey); decErr == nil {
		r.Tokens.SetPublisherKey(provenance.SessionPublisher(sess.Namespace, sess.Name), sess.Status.AuditKeyID, pub)
		// …and witness the same binding DURABLY, in the session's own scope. The
		// registration above is process memory and the status field above it dies
		// with the CR, while the records this key signs are permanent and sit in a
		// scope the NEXT session of this name inherits. Without the witness those
		// records read as unknown-key to `oap audit verify` forever after a
		// delete-and-recreate. See pkg/memory/kinds/auditkey.
		r.witnessAuditKey(ctx, &sess)
	}

	// 4b. LastIdleAt stamping. Stamp once when the session enters phase=Idle;
	// the archive sweep (below) compares against this timestamp to decide
	// when to transition the session to Succeeded.
	switch {
	case sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle && sess.Status.LastIdleAt == nil:
		now := metav1.Now()
		sess.Status.LastIdleAt = &now
	case sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle && sess.Status.LastIdleAt != nil:
		sess.Status.LastIdleAt = nil
		// A session that left Idle is no longer slept. The wake-annotation block
		// clears LastIdleAt as part of its own transition, so this arm is the
		// catch-all for every other path off Idle. Clearing lets a later Idle
		// period sleep again instead of idleSleepDue reading it as already-slept.
		sess.Status.SleptAt = nil
	}

	// Forensic hold: an unreleased SessionHold naming this session forces
	// phase=Held and reaps the session's pods (containment). Must run before
	// the idle-sweep block immediately below: reconcileArchive/reconcileExpiration
	// in that block can transition an Idle session to a terminal phase
	// (Succeeded/Failed), and the lifecycle machine's terminal-sticky guard
	// means a later Held event can never move a terminal session back — so a
	// hold observed only after that block runs is already too late. It also
	// runs before foldLifecycle/derivePhase further down, for the same reason
	// as the credential-update park below: the steady-state phase write would
	// otherwise clobber this override on the very same reconcile pass.
	if res, proceed, err := r.reconcileHold(ctx, &sess); err != nil || !proceed {
		return res, err
	}

	// 4c. Idle-sleep and archive sweep: an Idle channel-attached session past
	// sleepAfter has its pods reaped but stays Idle and wakeable; past
	// archiveAfter it transitions Idle→Succeeded. Sleep runs first, and
	// reconcileArchive still runs afterwards — a slept session must remain
	// sweepable for archive. Skipped for kubectl-driven sessions, which
	// terminate via agent_work_complete writing Succeeded directly.
	//
	// IMPORTANT: skipped when shouldWake is true. A fresh wake annotation means
	// an inbound just arrived and the runner must respawn now; without the guard
	// this block could return RequeueAfter before the wake-annotation block runs,
	// and the runner would never respawn.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle && sess.Spec.InputChannel != nil && !shouldWake(&sess) {
		_, sleepRequeue, err := r.reconcileSleep(ctx, &sess, &ac)
		if err != nil {
			return ctrl.Result{}, err
		}
		// A slept, node-pinned session past the idle-reclaim grace gives its
		// scratch PVCs back while staying Idle and wakeable — bounding the disk a
		// parked, wakeable population holds on node-local storage. Runs after the
		// sleep reaper (which stamps SleptAt) and keys its deadline off it; a
		// no-op on shared classes, not-yet-slept sessions, or when disabled.
		// Non-fatal like the terminal reclaim: the deletes and the marker write
		// are idempotent, so a failure just retries on the next reconcile, and
		// having reaped the pods matters more.
		idleReclaimRequeue, err := r.reconcileIdleStorageReclaim(ctx, &sess)
		if err != nil {
			log.FromContext(ctx).Info("idle storage reclaim failed; retried on the next reconcile",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}
		transitioned, archiveRequeue, err := r.reconcileArchive(ctx, &sess, &ac)
		if err != nil {
			return ctrl.Result{}, err
		}
		if transitioned {
			// Phase has changed; the next reconcile (driven by the watch on the
			// patched status) will pick it up.
			return ctrl.Result{}, nil
		}
		expTransitioned, expRequeue, err := r.reconcileExpiration(ctx, &sess)
		if err != nil {
			return ctrl.Result{}, err
		}
		if expTransitioned {
			// Phase → Failed; the status watch re-reconciles.
			return ctrl.Result{}, nil
		}
		// Requeue at the soonest of the deadlines: a not-yet-due sleep needs
		// re-evaluating even with the archive deadline far out, a just-slept
		// session still needs its archive requeue, an asleep session still needs a
		// wakeup at its wall-clock expiration even when the others are further out
		// or disabled, and a slept session on node-pinned storage needs its
		// idle-reclaim deadline so its scratch is freed on time.
		requeueAfter := archiveRequeue
		if sleepRequeue > 0 && (requeueAfter == 0 || sleepRequeue < requeueAfter) {
			requeueAfter = sleepRequeue
		}
		if expRequeue > 0 && (requeueAfter == 0 || expRequeue < requeueAfter) {
			requeueAfter = expRequeue
		}
		if idleReclaimRequeue > 0 && (requeueAfter == 0 || idleReclaimRequeue < requeueAfter) {
			requeueAfter = idleReclaimRequeue
		}
		// A parked, not-woken, channel-attached Idle session is fully handled by
		// the sweep above and must NOT fall through to the sidecar, detector and
		// runner provisioning steps, which would re-create the pods the sleep
		// reaper just deleted — a partial-sleep leak when archival is disabled and
		// the session is already slept. Persist any LastIdleAt/SleptAt stamp and
		// return; requeueAfter==0 is fine, since the wake-annotation watch
		// re-reconciles on the next inbound.
		if err := r.applyStatus(ctx, &sess); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Token-use authorization: reconcile the session's externaltoken grants in
	// SpiceDB BEFORE the sidecar-materialization loop and BEFORE the runner is
	// dispatched, so the runner's use_token pre-send check and
	// materializeSidecarSecret's pre-handout check both find an up-to-date grant
	// set. Fail-closed — a grant that cannot be written fails the session rather
	// than dispatching a runner, or handing a sidecar a credential, able to
	// present an un-authorized token. Gated on TokenGranter, nil without SpiceDB.
	//
	// This position depends only on the args-hash key being minted, the
	// AgentClass and runtime identity being resolved, and
	// sess.Status.EffectiveSettings being populated — all well above.
	//
	// It MUST NOT be gated on isTerminalPhase/shouldWake. A credential revoke
	// re-enqueues here via sessionsForAgentIdentityChange, and
	// reconcileCredentialGrants' declarative diff is the ONLY thing that deletes
	// the removed credential's authorized_token grant. A parked or terminal
	// session is exactly when a revoke still has to take effect, since the grant
	// must stop working even with no runner dispatching. A working revocation
	// outranks protecting a Failed session's diagnostic message from a transient
	// SpiceDB error.
	//
	// sweepRevokedGrants at the TOP of the reconcile covers the parked phases
	// that return before this line at all — a Valid=False class, or an Idle
	// channel-attached session leaving through the sleep/archive park. Keep both:
	// the sweep cannot mint, and this call site is unreachable from a parked
	// session but is the authoritative one on the boot path, where the grant set
	// must exist before any credential is handed out.
	if r.TokenGranter != nil {
		if err := r.reconcileCredentialGrants(ctx, &sess, &ac, argsHashKeyBytes); err != nil {
			return r.markBootFailed(ctx, &sess, spiceboxv1alpha1.ReasonAgentSessionTokenGrantFailed, err.Error())
		}
	}

	// 4d. Sidecar toolboxes: resolve each referenced SidecarToolbox CR, allocate
	// loopback ports, materialize the per-session cred Secret each in-pod sidecar
	// envFrom's, and snapshot the result into status. Those Secrets MUST be
	// written before RunnerFactory.Start — a pod referencing a missing Secret
	// hangs in ContainerCreating.
	//
	// Separate-pod (secret-gated) sidecars differ: they get
	// RunMode=RunModeSeparatePod, and materializeSidecarSecret is NOT called for
	// them because they do not mount secrets through the runner pod's envFrom.
	// Instead their required secret-output keys are checked against the
	// per-session <session>-secret-outputs Secret; until those land,
	// AwaitingSecret stays true and the reconcile returns a short RequeueAfter.
	// That is NOT a boot failure — the sidecar pod is created once it flips.
	sidecarSecretName := func(ref string) string {
		return cosidecar.CredentialSecretName(sess.Name, ref)
	}
	resolvedSidecars := make([]spiceboxv1alpha1.ResolvedSidecarToolbox, 0, len(ac.Spec.SidecarToolboxes))
	anyAwaitingSecret := false
	// sidecarSecretRequeue, when nonzero, is the requeue interval while a
	// separate-pod sidecar still awaits its secret-output. It does NOT hold the
	// runner — it only re-triggers this reconcile so the sidecar pod is created
	// once the runner has produced the gating secret.
	var sidecarSecretRequeue time.Duration
	// sidecarPodNotReady marks a satisfied separate-pod sidecar whose pod exists
	// but has not reported a Ready PodIP yet, so the reconcile requeues to
	// reflect the IP on a later pass. NOT a boot failure.
	sidecarPodNotReady := false
	// sidecarTerminalFailure marks a separate-pod sidecar wedged unrecoverably
	// while the runner is already live, so the runner surfaces the failure rather
	// than the session failing. It only slows the requeue — a pod that will not
	// recover does not deserve a 2s poll for the rest of the session — and the
	// pod stays watched, so a genuine recovery is still picked up promptly.
	sidecarTerminalFailure := false
	// pendingObservedPins collects per-sidecar ObservedPin records built while the
	// original tb.Spec is still available, before the by-digest rewrite. They are
	// upserted into status.ObservedPins once ResolvedSidecarToolboxes is set.
	var pendingObservedPins []spiceboxv1alpha1.ObservedPin
	if len(ac.Spec.SidecarToolboxes) > 0 {
		sidecarKeys := make([]string, 0, len(ac.Spec.SidecarToolboxes))
		for _, ref := range ac.Spec.SidecarToolboxes {
			var tb spiceboxv1alpha1.SidecarToolbox
			if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &tb); err != nil {
				if errors.IsNotFound(err) {
					// A dangling ref — typo, cascaded delete, not-yet-created CR —
					// is a terminal session-setup failure, surfaced rather than
					// silently skipped. sidecarToolboxes refs are not validated at
					// admission, so this Get is the only gate.
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionSidecarToolboxMissing,
						fmt.Sprintf("SidecarToolbox %q missing in namespace %q", ref.Ref, sess.Namespace))
				}
				return ctrl.Result{}, fmt.Errorf("get SidecarToolbox %q: %w", ref.Ref, err)
			}
			// Resolve the SpiceboxClass for the merged effective network policy.
			// Its Valid=True is gated upstream, so a Get failure is exceptional
			// but recoverable: mergeNetwork falls back to the CR's own values.
			var cls spiceboxv1alpha1.SpiceboxClass
			classErr := r.Client.Get(ctx, types.NamespacedName{Name: tb.Spec.Sandbox.Class}, &cls)
			if classErr != nil && !errors.IsNotFound(classErr) {
				return ctrl.Result{}, fmt.Errorf("get SpiceboxClass %q for sidecar %q: %w", tb.Spec.Sandbox.Class, ref.Ref, classErr)
			}
			effMode, effHosts := mergeNetwork(classErr == nil, cls.Spec.Network, tb.Spec.Sandbox.Network)

			// By-digest launch: once the toolbox controller has recorded a baseline
			// digest in status.pin, rewrite the source image to "<repo>@<digest>"
			// so the pod launches the exact bytes that were probed, regardless of
			// any upstream retag. A per-session snapshot mutation, not a CRD write.
			//
			// tb.Spec is a value copy, so its string fields are independent, but
			// its inner slices alias the informer cache. DeepCopy before mutating:
			// the rewrite touches only strings, but callers that later mutate
			// ResolvedSidecarToolbox.Spec slices must not corrupt the cache.
			tbSpec := *tb.Spec.DeepCopy()
			// By-digest launch rewrite — SKIPPED on local-dev-image clusters, where
			// a repo@digest ref is unpullable (see ResolveSidecarImageRef). The
			// baseline digest is still recorded in the ObservedPin below.
			var pinBaselineDigest string
			if tb.Status.Pin != nil {
				pinBaselineDigest = tb.Status.Pin.Digest
			}
			tbSpec.Source.Image = ResolveSidecarImageRef(tbSpec.Source.Image, pinBaselineDigest, r.UsesLocalDevImages)
			if tbSpec.Source.Inline != nil {
				tbSpec.Source.Inline.BaseImage = ResolveSidecarImageRef(tbSpec.Source.Inline.BaseImage, pinBaselineDigest, r.UsesLocalDevImages)
			}

			// Build the ObservedPin while the ORIGINAL declared image ref is still
			// in tb.Spec: Strength and Version come from that ref. Digest is the
			// recorded baseline (tb.Status.Pin), NOT re-derived from the rewritten
			// snapshot — so provenance still records the observed digest even on a
			// local-dev cluster where the container ref keeps its mutable tag.
			{
				// Original declared source ref (before by-digest rewrite).
				origRef := tb.Spec.Source.Image
				if origRef == "" && tb.Spec.Source.Inline != nil {
					origRef = tb.Spec.Source.Inline.BaseImage
				}
				var pinStrength string
				if pr, parseErr := (&imagepin.Kind{}).ParseRef(origRef); parseErr == nil {
					pinStrength = string(pr.Strength)
				} else if origRef != "" {
					log.FromContext(ctx).Info("observedPins: ParseRef failed (strength recorded as empty)",
						"toolbox", ref.Ref, "err", parseErr.Error())
				}
				_, origTag, _ := imagepin.SplitRef(origRef)
				pinDigest := pinBaselineDigest
				pendingObservedPins = append(pendingObservedPins, spiceboxv1alpha1.ObservedPin{
					Name: ref.Ref,
					Pin: spiceboxv1alpha1.PinRecord{
						Kind:     imagepin.KindName,
						Strength: pinStrength,
						Digest:   pinDigest,
						Version:  origTag,
					},
				})
			}

			sidecarKeys = append(sidecarKeys, ref.Ref)
			resolvedSidecars = append(resolvedSidecars, spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:                  ref.Name,
				Ref:                   ref.Ref,
				Spec:                  tbSpec,
				RunMode:               RunModeFor(tbSpec),
				EffectiveNetworkMode:  effMode,
				EffectiveAllowedHosts: effHosts,
				// Port set below.
			})
		}
		ports := AllocatePorts(sidecarKeys)
		for i := range resolvedSidecars {
			resolvedSidecars[i].Port = ports[resolvedSidecars[i].Ref]
		}

		// Snapshot the PRIOR SecretTokenHash per Ref before the loop overwrites
		// status.ResolvedSidecarToolboxes. A satisfied separate-pod sidecar whose
		// freshly-computed hash differs from its prior one had its token rotated,
		// so its running pod must be replaced.
		priorSidecarHash := make(map[string]string, len(sess.Status.ResolvedSidecarToolboxes))
		for _, prt := range sess.Status.ResolvedSidecarToolboxes {
			priorSidecarHash[prt.Ref] = prt.SecretTokenHash
		}

		// Materialize secrets for in-pod sidecars; gate separate-pod sidecars on
		// their secret-output keys being present in the per-session secret-output
		// Secret. Separate-pod sidecars skip the in-pod secret materialization
		// entirely — BuildRunnerPod already excludes them from runner-pod
		// containers.
		soSecretName := secretoutsrv.SecretOutputSecretName(sess.Name)
		var soSecret corev1.Secret
		soSecretLoaded := false
		// The sidecar identity seam (spec §13): resolved ONCE per reconcile, not
		// per-toolbox — a session has at most one Ready, session-owned Workshop,
		// and wsSidecarRef names the single sidecar it sanctions. Every other
		// separate-pod sidecar gets a nil identity below.
		wsIdentity, wsSidecarRef, wsNamespace := r.workshopIdentityFor(ctx, &sess)
		for i := range resolvedSidecars {
			rt := &resolvedSidecars[i]
			if rt.RunMode == RunModeSeparatePod {
				// This sidecar carries the workshop identity ONLY when it is the one
				// the Ready, session-owned Workshop named (rt.Ref == wsSidecarRef) —
				// every other sidecar, and every sidecar when no Workshop applies, is
				// false. Computed once per sidecar and reused by both the
				// NetworkPolicy (operator + apiserver egress) and the pod build
				// (projected token) below, so the two can never disagree about which
				// sidecar is the workshop one.
				isWorkshopSidecar := wsIdentity != nil && rt.Ref == wsSidecarRef
				// Separate-pod: satisfied exactly when the per-session
				// secret-output Secret's Data map holds every SecretInput's From
				// key.
				if !soSecretLoaded {
					// Written by the secretoutsrv HTTP handler, which stamps the
					// adoption label, so it is read through the guarded SecretReader
					// like every other operator secret read. NotFound is benign — the
					// runner has not written it yet — and counts as not satisfied.
					so, soErr := r.SecretReader.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: soSecretName})
					switch {
					case soErr == nil:
						soSecret = *so
					case errors.IsNotFound(soErr):
						// Benign: the runner has not written it yet. Leaving soSecret
						// empty makes the key check below miss, so the sidecar reads
						// as not-yet-satisfied — handled, not silent.
					default:
						// Anything else fails closed: never proceed on an unknown
						// secret-output read state.
						return ctrl.Result{}, fmt.Errorf("get secret-output Secret %q: %w", soSecretName, soErr)
					}
					soSecretLoaded = true
				}
				satisfied := true
				for _, si := range rt.Spec.SecretInputs {
					if _, ok := soSecret.Data[si.From]; !ok {
						satisfied = false
						break
					}
				}
				if !satisfied {
					rt.AwaitingSecret = true
					anyAwaitingSecret = true
					log.FromContext(ctx).Info("sidecar awaiting secret-output; deferring",
						"session", sess.Namespace+"/"+sess.Name,
						"sidecar", rt.Ref)
					// Defer until the gating secret-output exists. Once satisfied,
					// the upstream-credential Secret is materialized and envFrom'd
					// into the sidecar pod, NOT the runner pod.
					continue
				}
				rt.AwaitingSecret = false
				// Satisfied: idempotently create the separate per-session sidecar
				// pod with each secretInput wired from the secret-output Secret. The
				// pod name is always stamped; the PodIP is reflected once Ready.
				// SecretTokenHash captures the resolved secret bytes, so a change
				// against the prior hash is a token rotation and replaces the pod.
				rt.SidecarPodName = SidecarPodName(&sess, rt.Ref)
				rt.SecretTokenHash = SecretTokenHash(secretInputBytes(&soSecret, rt.Spec.SecretInputs))
				// Stamp the sidecar pod's NetworkPolicy before the pod exists,
				// so an enforcing CNI never sees an unconstrained sidecar.
				if err := r.ensureSidecarNetworkPolicy(ctx, &sess, *rt, isWorkshopSidecar); err != nil {
					return ctrl.Result{}, err
				}
				// Materialize the per-session upstream-credential Secret and envFrom
				// it into the sidecar pod. A separate-pod sidecar must carry its OWN
				// upstream cred — an in-pod sidecar gets it from the runner pod's
				// envFrom — otherwise its tunnel never comes up and cluster tools
				// fail with a DNS or routing error.
				credSecretName := sidecarSecretName(rt.Ref)
				if err := r.materializeSidecarSecret(ctx, &sess, &ac, *rt, credSecretName, argsHashKeyBytes); err != nil {
					return r.markBootFailed(ctx, &sess,
						sidecarMaterializeFailReason(err),
						fmt.Sprintf("materialize cred secret for sidecar %q: %v", rt.Ref, err))
				}
				// This sidecar gets the workshop identity ONLY when isWorkshopSidecar
				// (computed above, and shared with the NetworkPolicy call) — every
				// other sidecar, and every sidecar when no Workshop applies, gets nil.
				var sidecarIdentity *spiceboxv1alpha1.WorkshopSidecarIdentity
				if isWorkshopSidecar {
					sidecarIdentity = wsIdentity
				}
				podState, podErr := r.reconcileSidecarPod(ctx, &sess, *rt, soSecretName, credSecretName, priorSidecarHash[rt.Ref], sidecarIdentity, wsNamespace)
				if podErr != nil {
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
						fmt.Sprintf("reconcile sidecar pod for %q: %v", rt.Ref, podErr))
				}
				podIP, replacing := podState.PodIP, podState.Replacing
				// Record or clear the terminal-failure observation BEFORE the
				// readiness branches, so whichever exit this pass takes persists it.
				// Clearing on recovery matters: a crash loop that resolves itself
				// must not leave a stale failure on status telling the agent its
				// tools are gone when they are not.
				rt.PodFailure = podState.Failure
				if podState.Failure != nil {
					detail := podState.Failure.Reason
					if podState.Failure.Message != "" {
						detail += " — " + podState.Failure.Message
					}
					// Two ways to surface this, and only one is available at a time.
					// Once the runner pod exists there is a live agent to tell, so
					// the session keeps serving and the runner reports the failure
					// off this status record. Before the runner exists nothing can
					// relay it, so the terminal phase — which channelsd relays — is
					// the ONLY way a human learns why the session is stuck;
					// otherwise this pass requeues forever.
					if !podAlreadyStarted(&sess) {
						return r.markBootFailed(ctx, &sess,
							spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
							fmt.Sprintf("sidecar %q pod %q failed to start: %s (image %q)",
								rt.Ref, rt.SidecarPodName, detail, rt.Spec.Source.Image))
					}
					sidecarTerminalFailure = true
					log.FromContext(ctx).Info("sidecar pod terminally failed; recorded for the runner to surface",
						"session", sess.Namespace+"/"+sess.Name,
						"sidecar", rt.Ref, "pod", rt.SidecarPodName, "detail", detail)
				}
				if replacing {
					// Token rotated and the stale pod deleted: clear the now-gone
					// PodIP and persist the NEW hash, so the next pass recreates
					// with the new value instead of re-deleting, then reflects the
					// PodIP once Ready.
					rt.SidecarPodIP = ""
					sidecarPodNotReady = true
					log.FromContext(ctx).Info("sidecar token rotated; replacing pod",
						"session", sess.Namespace+"/"+sess.Name,
						"sidecar", rt.Ref, "pod", rt.SidecarPodName)
				} else if podIP == "" {
					// Pod created but not Ready yet; requeue and reflect the
					// PodIP on a later pass. Not a boot failure.
					sidecarPodNotReady = true
					log.FromContext(ctx).Info("sidecar pod not Ready yet; awaiting PodIP",
						"session", sess.Namespace+"/"+sess.Name,
						"sidecar", rt.Ref, "pod", rt.SidecarPodName)
				} else {
					rt.SidecarPodIP = podIP
				}
				continue
			}
			// In-pod sidecar: materialize the per-session cred Secret.
			if err := r.materializeSidecarSecret(ctx, &sess, &ac, *rt, sidecarSecretName(rt.Ref), argsHashKeyBytes); err != nil {
				return r.markBootFailed(ctx, &sess,
					sidecarMaterializeFailReason(err),
					fmt.Sprintf("materialize cred secret for sidecar %q: %v", rt.Ref, err))
			}
		}

		sess.Status.ResolvedSidecarToolboxes = resolvedSidecars

		// Upsert one ObservedPin per resolved sidecar from the records collected
		// in the first pass. ObservedAt is preserved while the identity
		// (digest+version+strength) is unchanged and advances only when it
		// changes, so a re-reconcile does not churn the timestamp.
		now := metav1.Now()
		for _, op := range pendingObservedPins {
			pin := op.Pin
			// Preserve ObservedAt when identity is unchanged.
			var preserved *metav1.Time
			for _, existing := range sess.Status.ObservedPins {
				if existing.Name == op.Name &&
					existing.Pin.Digest == pin.Digest &&
					existing.Pin.Version == pin.Version &&
					existing.Pin.Strength == pin.Strength {
					preserved = existing.Pin.ObservedAt
					break
				}
			}
			if preserved != nil {
				pin.ObservedAt = preserved
			} else {
				pin.ObservedAt = &now
			}
			sess.Status.ObservedPins = spiceboxv1alpha1.UpsertObservedPin(sess.Status.ObservedPins, op.Name, pin)
		}
	}

	// Reap separate-pod sidecar pods owned by this session whose toolbox ref has
	// left the resolved set, i.e. a toolbox removed from the AgentClass
	// mid-session. Runs unconditionally, even when the AgentClass now has zero
	// sidecars: the ownerref cascade would collect them at session-delete time,
	// but that leaves stale MCP servers running until then.
	{
		currentSeparateRefs := make(map[string]bool, len(resolvedSidecars))
		for _, rt := range resolvedSidecars {
			if rt.RunMode == RunModeSeparatePod {
				currentSeparateRefs[rt.Ref] = true
			}
		}
		// Protect active content-guard detector pods from orphan cleanup: they
		// carry the same sidecartoolbox label, with the inspector ID as its value,
		// so without this guard cleanupOrphanedSidecarPods would delete them every
		// reconcile — resetting status and blocking runner-pod creation forever.
		// Only inspectors implementing DetectorProvider have a pod to protect.
		if eff := sess.Status.EffectiveSettings; eff != nil {
			for _, ci := range eff.ContentInspectors {
				insp, ok := contentguardregistry.Get(ci.ID)
				if !ok {
					continue
				}
				if _, isDP := insp.(contentguard.DetectorProvider); isDP {
					currentSeparateRefs[ci.ID] = true
				}
			}
		}
		if cleanupErr := r.cleanupOrphanedSidecarPods(ctx, &sess, currentSeparateRefs); cleanupErr != nil {
			// A failed delete is worth retrying; the pod keeps running until a
			// later pass. The helper has already logged, so just return to
			// trigger the retry.
			return ctrl.Result{}, cleanupErr
		}
	}

	// A separate-pod sidecar awaiting its secret-output must NOT hold the runner:
	// the gating secret is produced BY the runner, so returning here would
	// DEADLOCK the session in Pending — the secret cannot appear without a
	// running runner, and the runner never starts while this waits. Persist the
	// AwaitingSecret snapshot, so runner boot skips the not-yet-existent sidecar
	// instead of polling its IP to a timeout, note the requeue, and FALL THROUGH
	// to create the runner. The requeue is short because the Secret is written by
	// the secretoutsrv handler, which does not own this session and so may not
	// re-trigger the watch. Contrast the content-guard detectors above, which are
	// runner-independent and legitimately hold the runner until Ready.
	if anyAwaitingSecret {
		if uerr := r.applyStatus(ctx, &sess); uerr != nil {
			return ctrl.Result{}, uerr
		}
		sidecarSecretRequeue = 5 * time.Second
	}
	// A satisfied separate-pod sidecar whose pod exists but is not Ready, or whose
	// stale pod was just deleted for a token rotation: persist the snapshot
	// assigned above (SidecarPodName plus the NEW SecretTokenHash) and requeue.
	// The next pass reflects the PodIP once Ready or recreates the replacement,
	// and the persisted hash keeps the rotation from being re-detected. The runner
	// pod may still be created meanwhile — it tolerates an unresolved
	// SidecarPodIP.
	if sidecarPodNotReady {
		requeue := 2 * time.Second
		if sidecarTerminalFailure {
			requeue = sidecarTerminalRequeue
		}
		return ctrl.Result{RequeueAfter: requeue}, r.applyStatus(ctx, &sess)
	}

	// 4e. Content-guard detector pods: for each enabled inspector implementing
	// contentguard.DetectorProvider, create a SEPARATE per-session pod with a
	// zero-egress NetworkPolicy and reflect its IP into status once Ready.
	// Runner-pod creation is HELD until every detector PodIP is reflected, with a
	// clear AwaitingDetector reason set meanwhile so the wait is never silent.
	// The loop is generic — it never branches on inspector id.
	{
		var resolvedDetectors []spiceboxv1alpha1.ResolvedContentGuardDetector
		var detKeys []string
		// Collect the detector-requiring inspectors and their specs.
		if eff := sess.Status.EffectiveSettings; eff != nil {
			for _, ci := range eff.ContentInspectors {
				insp, ok := contentguardregistry.Get(ci.ID)
				if !ok {
					// Webhook already rejected unknown IDs; defensive skip.
					continue
				}
				dp, ok := insp.(contentguard.DetectorProvider)
				if !ok {
					// Inspector does not need a detector sidecar (e.g. url-allowlist).
					continue
				}
				raw := ci.Config.Raw
				ds, derr := dp.Detector(raw)
				if derr != nil || ds == nil {
					// Fail-closed: a detector-requiring inspector that can't resolve
					// its image must not silently run unscanned.
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
						fmt.Sprintf("content-guard detector %q: %v", ci.ID, derr))
				}
				// Use the image exactly as declared in the inspector config: pinned
				// by digest it launches the exact bytes, by tag it launches by tag.
				// There is no probe path here as there is for SidecarToolbox.
				img := ds.Image
				resolvedDetectors = append(resolvedDetectors, spiceboxv1alpha1.ResolvedContentGuardDetector{
					Inspector:  ci.ID,
					Image:      img,
					HealthPath: ds.HealthPath,
				})
				detKeys = append(detKeys, "det-"+ci.ID)
			}
		}

		if len(resolvedDetectors) > 0 {
			// Allocate ports in the SAME key space as sidecar ports so they never
			// collide. Derive the sidecar key slice from the resolved snapshot.
			sidecarKeySlice := make([]string, 0, len(resolvedSidecars))
			for _, rt := range resolvedSidecars {
				sidecarKeySlice = append(sidecarKeySlice, rt.Ref)
			}
			allKeys := append(append([]string{}, sidecarKeySlice...), detKeys...)
			ports := AllocatePorts(allKeys)
			for i := range resolvedDetectors {
				det := &resolvedDetectors[i]
				det.Port = ports["det-"+det.Inspector]
				det.PodName = "sidecar-" + sess.Name + "-" + det.Inspector
			}

			// Create each detector pod + its deny-all-egress NetworkPolicy, and
			// reflect the pod IP. Hold runner-pod creation until every PodIP is known.
			detectorAllReady := true
			var notReadyInspectors []string
			// notReadyDetectorPods collects the pod names of the not-ready detectors
			// so the AwaitingDetector wait below can surface a pod-unschedulable
			// reason (same fail-safe path as bundles).
			var notReadyDetectorPods []string
			// earliestDetectorStart is the epoch the detector readiness deadline is
			// measured from: the earliest CreationTimestamp among the not-ready
			// detector pods. As with bundles, under userPassthrough these are not
			// created until the AwaitingCredentials gate clears, so this — not
			// sess.CreationTimestamp — is when detector provisioning began.
			var earliestDetectorStart time.Time
			// Detector pods wedged terminally — bad image, unsatisfiable config,
			// crash loop — collected so the fast-fail below can fail closed in
			// seconds with the specific cause instead of waiting out the deadline.
			var terminalDetectorFailures []string
			for i := range resolvedDetectors {
				det := &resolvedDetectors[i]
				spec := cosidecar.Spec{
					Ref: det.Inspector,
					// Pin the NetworkPolicy name to the one
					// cleanupOrphanedSidecarPods reaps by. On cosidecar's default
					// name, removing an inspector mid-session reaps the detector pod
					// by label but orphans its NetworkPolicy until session GC.
					// BuildPod names the pod by Ref, so only the NP name is affected.
					Name:            SidecarNetworkPolicyName(&sess, det.Inspector),
					Image:           det.Image,
					Port:            det.Port,
					Health:          cosidecar.Healthcheck{Path: det.HealthPath},
					ImagePullSecret: r.ImagePullSecret,
					// Zero egress: the detector ingests potentially hostile tool text
					// and must not be able to exfiltrate it to external endpoints.
					Egress: cosidecar.EgressPolicy{Denied: true},
				}
				pod, _, perr := cosidecar.BuildPod(&sess, spec)
				if perr != nil {
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
						fmt.Sprintf("build detector pod %q: %v", det.Inspector, perr))
				}
				if cerr := r.ensureDetectorPod(ctx, &sess, spec, pod); cerr != nil {
					return ctrl.Result{}, cerr
				}
				// Reflect IP once the pod is Ready.
				var livePod corev1.Pod
				if gerr := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: pod.Name}, &livePod); gerr == nil {
					if podReady(&livePod) && livePod.Status.PodIP != "" {
						det.PodIP = livePod.Status.PodIP
					} else if reason, msg, ok := terminalPodWaitReason(&livePod); ok {
						detail := reason
						if msg != "" {
							detail = reason + " — " + msg
						}
						terminalDetectorFailures = append(terminalDetectorFailures,
							fmt.Sprintf("inspector %q pod %q failed to start: %s (image %q)",
								det.Inspector, pod.Name, detail, det.Image))
					}
				} else {
					// The Get itself failed — NOT the common not-Ready-yet path,
					// which is gerr==nil with the pod not ready. Surface it so a
					// persistent failure such as an RBAC pods-get denial is
					// distinguishable from an unscheduled pod; otherwise the
					// AwaitingDetector requeue loops with no diagnostic.
					log.FromContext(ctx).Info("detector pod get failed; still awaiting PodIP",
						"session", sess.Namespace+"/"+sess.Name, "inspector", det.Inspector,
						"pod", pod.Name, "err", gerr.Error())
				}
				if det.PodIP == "" {
					detectorAllReady = false
					notReadyInspectors = append(notReadyInspectors, det.Inspector)
					notReadyDetectorPods = append(notReadyDetectorPods, pod.Name)
					// Measure from this pod's provisioning start, not session
					// creation. A failed Get above leaves livePod zero, contributing
					// a zero time that earlierNonZero ignores — fail-safe.
					earliestDetectorStart = earlierNonZero(earliestDetectorStart, livePod.CreationTimestamp.Time)
				}
			}

			sess.Status.ResolvedContentGuardDetectors = resolvedDetectors

			if !detectorAllReady {
				// Fast fail-closed: a terminally wedged detector pod will never
				// become Ready, so surface the SPECIFIC cause now — markBootFailed
				// sets Phase=Failed, which channelsd relays — rather than sitting
				// silent until the generic deadline. The loud failure comes first.
				if len(terminalDetectorFailures) > 0 {
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
						fmt.Sprintf("content-guard detector pod(s) will not become Ready: %s; failing closed (the session will not run unscanned)",
							strings.Join(terminalDetectorFailures, "; ")))
				}
				// Deadline backstop: a detector never becoming Ready for a
				// non-terminal reason, such as perpetual unschedulability, would
				// requeue forever on AwaitingDetector. Past detectorReadyDeadline —
				// measured from provisioning start, so a userPassthrough
				// credential-link wait never counts — fail closed via
				// ContentGuardHalt, naming the offending inspector(s). The session
				// never runs unscanned.
				if detectorProvisioningTimedOut(earliestDetectorStart, r.now()) {
					return r.markBootFailed(ctx, &sess,
						spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
						fmt.Sprintf("content-guard detector pod(s) for inspector(s) %v never became Ready within %s; failing closed (the session will not run unscanned)",
							notReadyInspectors, detectorReadyDeadline))
				}
				// Set a clear reason while awaiting the detector pod IP, then
				// requeue — never leave a blank phase. A detector stuck
				// Pending-unschedulable surfaces the scheduler's real reason, and
				// fans a one-shot capacity warning, instead of the generic awaiting
				// text; same fail-safe path as bundles.
				detectorMsg := "awaiting content-guard detector pod IP"
				if stall, ok := r.firstSchedulingStall(ctx, sess.Namespace, notReadyDetectorPods); ok {
					detectorMsg = stall.message()
					r.maybeEmitUnschedulable(ctx, &sess, spiceboxv1alpha1.AgentSessionConditionRunnerReady, stall)
				} else {
					// No scheduling stall right now: reflect that a previously-stuck
					// capacity problem has cleared, even though the detector is not
					// Ready yet.
					r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
						spiceboxv1alpha1.ReasonSandboxScheduled, "")
				}
				conditions.SetFalse(&sess, &sess.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionRunnerReady,
					spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector,
					detectorMsg)
				if uerr := r.applyStatus(ctx, &sess); uerr != nil {
					return ctrl.Result{}, uerr
				}
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			// Every detector pod reached Ready this pass, so clear any capacity
			// stall recorded earlier — this pass may have jumped straight from
			// "stalled" to "Ready" with no intermediate not-stalled pass.
			r.setTrueCondition(&sess, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
				spiceboxv1alpha1.ReasonSandboxScheduled, "")
		} else {
			// No detectors configured: clear any slice left by an inspector that
			// has since been removed from settings.
			sess.Status.ResolvedContentGuardDetectors = nil
		}
	}

	// 5a. Wake-annotation detection: transition Idle → Pending so the pod-creation
	// path below spawns a fresh runner. MUST run before the
	// isTerminalForPodLifecycle guard, or a session with a fresh wake annotation
	// is parked prematurely.
	if shouldWake(&sess) {
		// Captured before anything below mutates the Idle condition:
		// ArchivedBySweep reads its current reason, which the SetFalse further
		// down overwrites with WakeRequested.
		wasArchivedBySweep := spiceboxv1alpha1.ArchivedBySweep(&sess)
		// Idle/AwaitingRetry → Pending: record WakeRequested and let the
		// projection put phase back to Pending.
		//
		// On a provider-error RETRY the same turn is replayed, and WakeRequested
		// cannot be finely ordered against that turn's events, so a completed
		// retry raw-folds to Pending — see the same-turn retry note on
		// operatorOrderKey. The steady-state reconcilePhase corrects it, refusing
		// to demote a runner-written Idle, so the CR-visible phase stays Idle. An
		// Idle wake is a new user turn: it bumps memTurnIndex and folds to Running.
		if err := r.applyEvent(ctx, &sess, lifecyclecore.WakeRequested{}); err != nil {
			return ctrl.Result{}, err
		}
		// Don't bump RunnerRestarts: wake-from-Idle is a normal user-driven
		// event, not a crash. The pod's RestartCount, synced by reflectRunnerPod,
		// is the source of truth for real container crashes, and the
		// maxRunnerRestarts kill-switch must fire only on those.
		sess.Status.LastIdleAt = nil
		// Clear SleptAt explicitly, not just via the LastIdleAt catch-all: this
		// path nils LastIdleAt in the same pass that applies WakeRequested, and
		// that catch-all ran earlier in this call while phase was still Idle, so
		// it never observes phase != Idle && LastIdleAt != nil. Without this, a
		// session that slept once would have SleptAt stuck non-nil forever —
		// idleSleepDue reads a non-nil SleptAt as "already slept, not due" — and
		// could never sleep again on a later Idle period.
		sess.Status.SleptAt = nil
		idleWakeMessage := ""
		if wasArchivedBySweep {
			// Un-archive: the sweep stamped a terminal outcome on a session that is
			// resuming. Leaving FinishedAt set would make a live session look
			// completed to the CLI, the admin UI, and session GC.
			sess.Status.FinishedAt = nil
			idleWakeMessage = "resumed from archive by a new inbound"
		}
		conditions.SetFalse(&sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionIdle,
			spiceboxv1alpha1.ReasonAgentSessionWakeRequested, idleWakeMessage)
		conditions.SetFalse(&sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
			spiceboxv1alpha1.ReasonAgentSessionWakeRequested, "")
		now := metav1.Now()
		sess.Status.LastWakeAt = &now
		if err := r.applyStatus(ctx, &sess); err != nil {
			return ctrl.Result{}, err
		}
		// Clear the TTL annotation on successful retry wake so the next
		// ProviderError starts a fresh 30-minute window.
		r.clearRetrySinceAnnotation(ctx, &sess)
		// Tear down the prior runner — a Completed pod or stale goroutine —
		// before spawning the wake's replacement. The pod-create path below uses
		// the fixed name `<sess>-runner`, so without this Start returns
		// AlreadyExists for the Completed pod and the session sits in Pending
		// forever.
		if err := r.RunnerFactory.Stop(ctx, &sess); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop prior runner for wake: %w", err)
		}
		// Re-read so the requeued reconcile (below) observes the persisted
		// Pending phase rather than the stale in-memory Idle copy.
		if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &sess); !cont {
			return ctrl.Result{}, err
		}
		// Stop here rather than falling through to the runner start. The sleep
		// reaper deleted a slept session's bundle SpiceboxSessions while
		// status.BundleSessions still names them, and bundle provisioning runs
		// only when sandboxProvisioningDesired(phase), so it was gated off this
		// pass while phase was still Idle. Starting the runner now would spawn it
		// against bundles that do not exist: it Gets each name at startup and
		// crash-loops. Returning lets the now-Pending phase reach bundle
		// provisioning on the NEXT reconcile, and wait for Ready, before the
		// runner ever starts. It cannot loop — shouldWake requires
		// Idle/AwaitingRetry, and WakeRequested just folded phase to Pending.
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// 5a'. AwaitingRetry TTL and lifecycle reconcile, reached only when shouldWake
	// was false. It reconciles the lifecycle fold with the CR phase — a signed log
	// already showing PhaseFailed, its retry budget exhausted, applies that
	// terminal phase now — and enforces the TTL, emitting RetryTTLExpired after
	// defaultRetryTTL of inactivity so a session cannot strand in AwaitingRetry.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry {
		done, result, err := r.checkRetryTTL(ctx, &sess)
		if err != nil {
			return ctrl.Result{}, err
		}
		if done {
			sess.Status.ObservedGeneration = sess.Generation
			return result, r.applyStatus(ctx, &sess)
		}
	}

	// 5b. Runner Pod. Pod management is skipped while the session is Idle, where
	// channel-attached sessions park between conversations. The wake path above
	// transitions Idle→Pending first, so reaching here still Idle means "stay
	// parked".
	if isTerminalForPodLifecycle(sess.Status.Phase) {
		sess.Status.ObservedGeneration = sess.Generation
		return ctrl.Result{}, r.applyStatus(ctx, &sess)
	}

	// Start is idempotent, returning nil on AlreadyExists; the in-process factory
	// spawns a goroutine and returns at once. The Get below picks up the
	// just-created Pod and drives status reflection, while in-process has no Pod
	// and its NotFound is handled as "newly spawned: set RunnerCreating and exit".
	//
	// StartOpts carries the resolved sidecar snapshot and the per-session Secret
	// naming fn. PodRunnerFactory wires each sidecar container's envFrom to
	// sidecarSecretName(ref), whose Secrets were materialized above, before this
	// call. An empty snapshot simply builds a pod with no sidecars.
	if err := r.RunnerFactory.Start(ctx, &sess, &ac, StartOpts{
		ResolvedSidecars:              resolvedSidecars,
		SidecarSecretName:             sidecarSecretName,
		ResolvedContentGuardDetectors: sess.Status.ResolvedContentGuardDetectors,
	}); err != nil {
		return r.surfaceRunnerRefused(ctx, &sess, err)
	}

	var existingPod corev1.Pod
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: RunnerPodName(&sess),
	}, &existingPod)
	switch {
	case errors.IsNotFound(err):
		// Either the in-process factory spawned a goroutine and there is no Pod,
		// or the pod factory created one the cache has not observed yet. Mark the
		// runner Creating and let the next reconcile reflect it; the factory
		// decides whether this session has a workload name worth recording.
		if name := r.RunnerFactory.ObservedName(&sess); name != "" {
			sess.Status.RunnerPodName = name
		}
		r.setFalseCondition(&sess, spiceboxv1alpha1.AgentSessionConditionRunnerReady,
			spiceboxv1alpha1.ReasonRunnerCreating, "runner spawned")
		return ctrl.Result{}, r.applyStatus(ctx, &sess)
	case err != nil:
		return ctrl.Result{}, err
	}

	// Stale-pod recovery: a terminal pod under a Pending session, typically just
	// after a wake from Idle, is deleted and the reconcile requeued so the
	// idempotent Start creates a fresh one. Without this a wake-respawn hangs
	// permanently on the prior turn's terminal pod.
	if (existingPod.Status.Phase == corev1.PodSucceeded || existingPod.Status.Phase == corev1.PodFailed) &&
		sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending {
		if delErr := r.Client.Delete(ctx, &existingPod); delErr != nil && !errors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete stale runner pod: %w", delErr)
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	sess.Status.RunnerPodName = existingPod.Name

	// 6. Reflect pod phase + count terminal restarts.
	wasRunnerReady := conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	r.reflectRunnerPod(&sess, &existingPod)
	// Authority handoff: on the runner pod's edge to Ready, record RunnerClaimed
	// once. The projection moves to Running and the live region passes to the
	// runner.
	if !wasRunnerReady && conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady) {
		if err := r.applyEvent(ctx, &sess, lifecyclecore.RunnerClaimed{}); err != nil {
			return ctrl.Result{}, err
		}
	}

	crashMsg, crashing := runnerCrashDetail(&existingPod)
	// Surface a crashing runner as Failed. Two triggers, so no user is left
	// staring at a silent hang:
	//   - fast path: down with a CAPTURED error, already restarted at least
	//     once, and crashed while THIS operator process was watching
	//     (shouldFastFailRunner) — such a crash repeats identically and never
	//     recovers, so the real cause surfaces in tens of seconds instead of at
	//     the restart cap, but only once the operator's own absence is ruled out.
	//   - backstop: the hard restart cap, for crashes with no captured message.
	overCap := sess.Status.RunnerRestarts > maxRunnerRestarts
	fastFail := shouldFastFailRunner(&existingPod, sess.Status.RunnerRestarts, crashMsg, crashing, r.witnessing())
	if (overCap || fastFail) && !isTerminalPhase(sess.Status.Phase) {
		// The runner pod has crash-looped: record RunnerCrash. The precise reason
		// and condition are set below.
		if err := r.applyEvent(ctx, &sess, lifecyclecore.RunnerCrash{}); err != nil {
			return ctrl.Result{}, err
		}
		sess.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionRunnerCrash
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		// Prefer the runner's real error over a bare restart count. The kubelet
		// captures it into the terminated-container message — podspec.go sets
		// terminationMessagePolicy=FallbackToLogsOnError — and channelsd relays
		// this message to the user's channel.
		msg := fmt.Sprintf("runner restarted %d times", sess.Status.RunnerRestarts)
		if crashMsg != "" {
			msg = fmt.Sprintf("runner failed to start: %s", crashMsg)
		}
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason:  spiceboxv1alpha1.ReasonAgentSessionRunnerCrash,
			Message: msg,
		})
	}

	// credential_update park/unpark, driven by CredentialUpdateRequest CRs
	// referencing this session rather than by its own lifecycle log: the
	// request_credential_update meta tool BLOCKS the runner in-process while a
	// request is Open, so there is no lifecycle event to fold. An Open request
	// forces AwaitingCredentials and returns early; an unpark clears the marker
	// condition and falls through, letting derivePhase below pick the real
	// steady-state phase.
	if res, proceed, err := r.reconcileCredentialUpdatePark(ctx, &sess); err != nil || !proceed {
		return res, err
	}

	// Derive the canonical phase from the folded lifecycle log, once, at the end
	// of the steady-state computation, so the operator is the single place that
	// decides phase for a live session. Early-return paths emit their event via
	// applyEvent and return before reaching here.
	state, err := r.foldLifecycle(ctx, &sess)
	if err != nil {
		return ctrl.Result{}, err
	}
	// reconcilePhase guards the steady-state write: an empty or incomplete folded
	// log projects to the bootstrap Pending, which must not clobber a
	// more-advanced or terminal phase set directly this reconcile. Legitimate
	// demotions to Pending were already applied via applyEvent above, so the
	// current phase is Pending there and the guard is a no-op.
	sess.Status.Phase = reconcilePhase(derivePhase(state), sess.Status.Phase)
	// A fold to a terminal phase here (a runner-reported terminal event, not one
	// of the explicit failure/success paths above) must still carry a FinishedAt:
	// without it the terminal-pod reaper, CLI, admin UI, and session GC see a
	// session that never "finished". Backfill once, on the edge, only when
	// missing — set-once so a byte-identical re-apply stays a no-op.
	sess.Status.FinishedAt = stampTerminalFinish(sess.Status.Phase, sess.Status.FinishedAt, metav1.NewTime(r.now()))

	// Apply lifecycle-projected surface conditions. The operator is the SINGLE
	// writer of ScopeReviewPending and ToolCallGated; both derive from the folded
	// lifecycle log and no other component writes them.
	view := lifecyclecore.Project(state)
	if view.ScopeReviewPending {
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentSessionConditionScopeReviewPending,
			Status: metav1.ConditionTrue,
			Reason: "Pending",
		})
	} else {
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentSessionConditionScopeReviewPending,
			Status: metav1.ConditionFalse,
			Reason: "Resolved",
		})
	}
	if view.ToolCallGated > 0 {
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentSessionConditionToolCallGated,
			Status:  metav1.ConditionTrue,
			Reason:  "HookDenied",
			Message: fmt.Sprintf("%d tool call(s) denied by hooks without human review", view.ToolCallGated),
		})
	} else {
		conditions.Set(&sess, &sess.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentSessionConditionToolCallGated,
			Status: metav1.ConditionFalse,
			Reason: "NoDenials",
		})
	}

	sess.Status.ObservedGeneration = sess.Generation
	// RequeueAfter is nonzero only while a secret-gated sidecar awaits its secret,
	// so the sidecar pod is created as soon as the runner produces it. The runner
	// is already Running by now, so this blocks nothing.
	return ctrl.Result{RequeueAfter: sidecarSecretRequeue}, r.applyStatus(ctx, &sess)
}

func (r *Reconciler) finalize(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sess, spiceboxv1alpha1.FinalizerAgentSession) {
		return ctrl.Result{}, nil
	}
	// Revoke the session's memory token. NONE of the session's memory is deleted
	// on teardown: the append-only kinds (transcript, audit, authz-decision,
	// tool-session) form a tamper-evident log that is PERMANENT, and a session
	// disappearing must NEVER take its logs with it. memory.Local.DeleteScope
	// structurally refuses append-only kinds regardless of caller, so this holds
	// even if a caller tries. Ephemeral working memory is a separate retention
	// policy's job and is never wiped by session lifecycle.
	key := memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
	r.Tokens.Revoke(key)
	// Tear down the runner via the factory. Owner-ref GC would delete the Pod
	// anyway, but an explicit Stop lets the in-process factory cancel its
	// goroutine deterministically. Best-effort: a failure is logged and the
	// finalize sequence continues, so a stuck Stop never blocks finalization.
	// RunnerFactory is a required collaborator, so there is no nil guard here.
	if err := r.RunnerFactory.Stop(ctx, sess); err != nil {
		log.FromContext(ctx).Info("finalize: RunnerFactory.Stop failed (best-effort)",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	// Reap the per-session toolspec-reader ClusterRoleBinding BEFORE the SpiceDB
	// deletion below, which can return early on error; this reap is best-effort,
	// since a stale CRB only references a now-deleted SA. Being cluster-scoped it
	// has no namespaced owner ref and is NOT collected by owner-ref GC, so the
	// finalizer must delete it by its deterministic name. NotFound is success;
	// any other error is logged and does not block finalization.
	crbName := ToolspecReaderCRBName(sess)
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crbName}}
	if err := r.Client.Delete(ctx, crb); err != nil && !errors.IsNotFound(err) {
		log.FromContext(ctx).Info("finalize: toolspec-reader ClusterRoleBinding delete failed (best-effort)",
			"session", sess.Namespace+"/"+sess.Name,
			"clusterRoleBinding", crbName,
			"err", err.Error())
	}
	// Reap the cross-namespace passthrough Role/RoleBinding: they live in the
	// identities namespace with no AgentSession owner ref, so owner-ref GC will
	// not remove them. NotFound is success; other errors are logged, not fatal.
	passthroughName := PassthroughRoleName(sess)
	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: passthroughName, Namespace: spiceboxv1alpha1.IdentitiesNamespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: passthroughName, Namespace: spiceboxv1alpha1.IdentitiesNamespace}},
	} {
		if err := r.Client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			log.FromContext(ctx).Info("finalize: passthrough RBAC delete failed (best-effort)",
				"session", sess.Namespace+"/"+sess.Name, "object", obj.GetName(), "err", err.Error())
		}
	}
	// Delete this session's SpiceDB relationships before removing the finalizer.
	// A nil deleter skips gracefully. On error, return so the finalizer is NOT
	// removed and the next reconcile retries.
	if r.SpiceDBDeleter != nil {
		// Slot grants first. They are the only relationships that outlive the
		// session's own — they sit on external resources — so if exactly one of
		// these two calls gets to run, it should be this one.
		if err := r.SpiceDBDeleter.DeleteSlotGrants(ctx, sess.Namespace, sess.Name); err != nil {
			log.FromContext(ctx).Error(err, "delete session slot grants",
				"session", sess.Namespace+"/"+sess.Name)
			return ctrl.Result{}, err
		}
		// Data slot grants, for the same reason and by neither of the other
		// sweeps: they sit on pt_tag objects with the session as SUBJECT, so a
		// child handed data would otherwise keep reading it after it ended.
		if err := r.SpiceDBDeleter.DeleteDataSlotGrants(ctx, sess.Namespace, sess.Name); err != nil {
			log.FromContext(ctx).Error(err, "delete session data slot grants",
				"session", sess.Namespace+"/"+sess.Name)
			return ctrl.Result{}, err
		}
		if err := r.SpiceDBDeleter.DeleteAgentSessionRelationships(ctx, sess.Namespace, sess.Name); err != nil {
			log.FromContext(ctx).Error(err, "delete agentsession relationships",
				"session", sess.Namespace+"/"+sess.Name)
			return ctrl.Result{}, err
		}
	} else {
		log.FromContext(ctx).Info("SpiceDBDeleter not configured; skipping SpiceDB relationship cleanup",
			"session", sess.Namespace+"/"+sess.Name)
	}
	// Owner-ref GC handles SA/Role/RoleBinding/Secret/Pod.
	controllerutil.RemoveFinalizer(sess, spiceboxv1alpha1.FinalizerAgentSession)
	if err := r.Client.Update(ctx, sess); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reapSessionPods tears down a terminal session's pods — bundle
// SpiceboxSessions, the runner pod, and any detector/cosidecar pods — while
// KEEPING the AgentSession and its PVCs, Secrets, RBAC and SpiceDB
// relationships, so status/conditions/bundleSessions stay available for
// debugging. Bundle SpiceboxSessions go by the deterministic names recorded on
// status.bundleSessions during provisioning, the runner pod via
// RunnerFactory.Stop. Idempotent: NotFound means "already reaped", so a repeat
// run is a clean no-op. Deleting a bundle SpiceboxSession cascades to its
// sandbox pod through owner-ref GC. Any other delete error is logged and
// returned so the reconcile retries — never silently dropped.
func (r *Reconciler) reapSessionPods(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	reaped := make([]string, 0, len(sess.Status.BundleSessions)+2)
	// 1. Bundle SpiceboxSessions → sandbox pods via owner-ref GC.
	for _, b := range sess.Status.BundleSessions {
		if b.SpiceboxSessionName == "" {
			continue
		}
		ss := &spiceboxv1alpha1.SpiceboxSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: b.SpiceboxSessionName},
		}
		if err := r.Client.Delete(ctx, ss); err != nil {
			if errors.IsNotFound(err) {
				continue // already reaped
			}
			logger.Info("reap: delete bundle SpiceboxSession failed; will retry",
				"session", sess.Namespace+"/"+sess.Name,
				"spiceboxSession", b.SpiceboxSessionName, "err", err.Error())
			return ctrl.Result{}, fmt.Errorf("reap bundle SpiceboxSession %q: %w", b.SpiceboxSessionName, err)
		}
		reaped = append(reaped, "spiceboxsession/"+b.SpiceboxSessionName)
	}
	// 2. Runner pod, via the factory, which handles NotFound internally. A Stop
	// error is logged and returned so the reconcile retries rather than
	// stranding the runner pod.
	if err := r.RunnerFactory.Stop(ctx, sess); err != nil {
		logger.Info("reap: RunnerFactory.Stop failed; will retry",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("reap runner pod: %w", err)
	}
	reaped = append(reaped, "pod/"+RunnerPodName(sess))
	// 3. Detector / cosidecar pods. cleanupOrphanedSidecarPods deletes any pod
	// carrying this session's label whose sidecartoolbox ref is absent from the
	// "current" set, so passing nil treats every such pod — separate-pod
	// sidecars and detector/cosidecar pods alike, they share these labels — as
	// orphaned. Idempotent: already-gone pods are NotFound inside the helper.
	if err := r.cleanupOrphanedSidecarPods(ctx, sess, nil); err != nil {
		logger.Info("reap: cleanup detector/cosidecar pods failed; will retry",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return ctrl.Result{}, fmt.Errorf("reap detector/cosidecar pods: %w", err)
	}
	logger.Info("reaped session pods; AgentSession + PVCs + Secrets kept",
		"session", sess.Namespace+"/"+sess.Name,
		"grace", r.FailedSandboxReapGrace.String(),
		"deleted", strings.Join(reaped, ","))
	return ctrl.Result{}, nil
}

func (r *Reconciler) reflectRunnerPod(sess *spiceboxv1alpha1.AgentSession, pod *corev1.Pod) {
	switch pod.Status.Phase {
	case corev1.PodRunning:
		// The RUNNER container's readiness, not the pod's. In-pod
		// SidecarToolboxes are real containers here and the kubelet calls a pod
		// Running while any one container runs, so a runner that exited 0 beside
		// a live Ready sidecar would report RunnerReady=True and the
		// !wasRunnerReady edge would emit RunnerClaimed for a runner that is
		// gone. A pod with no runner status yet reads as not-ready, which is the
		// pre-start state anyway.
		ready := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == runnerContainerName {
				ready = cs.Ready
				break
			}
		}
		status := metav1.ConditionFalse
		reason := spiceboxv1alpha1.ReasonRunnerCreating
		if ready {
			status = metav1.ConditionTrue
			reason = spiceboxv1alpha1.ReasonRunnerReady
		}
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: status, Reason: reason,
		})
	case corev1.PodSucceeded, corev1.PodFailed:
		// The container restart count below is the canonical crashloop signal.
	default:
		// Pod exists (this is only reached once Get above found it) but the
		// kubelet has not reported a phase past Pending yet, or has reported none
		// at all. Reflect "runner exists, not yet ready" unconditionally.
		//
		// Without this, a RunnerReady condition set BEFORE this pod existed
		// stands forever: a refused Start sets False/RunnerPodRefused and
		// returns before this function ever runs; once the refusal clears and
		// Start succeeds, the very next Get can already find the pod (no cache
		// lag, or enough reconciles have passed for the informer to catch up),
		// so the NotFound branch above — the only other place that writes
		// RunnerCreating — never fires either. Nothing then moves the condition
		// off RunnerPodRefused until the pod reaches Running, telling a person
		// their agent could not start when it was in fact coming up. A fresh
		// session with no prior condition is unaffected: this simply sets the
		// same RunnerCreating a first-time NotFound would have set.
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonRunnerCreating, Message: "runner spawned",
		})
	}
	// Container restart counter as the canonical crashloop signal.
	//
	// Read from THE POD BEING REFLECTED, never kept as an all-time high-water
	// mark. The runner Pod really is replaced — the wake path Stops it, which
	// DELETES the Pod, and the next pass Starts a fresh one at the same fixed
	// name with RestartCount 0. Carried forward instead, one earlier crash arms
	// fastFail VACUOUSLY for the rest of the session: earlyRunnerCrashRestarts is
	// 1, so the first down-window of every later fresh pod, before the kubelet
	// has retried, would terminate the session as RunnerCrash — the inverse of
	// what the constant means. overCap is unaffected either way; it compares
	// against a max, not a sum.
	//
	// A pod publishing no runner container status yet leaves the count untouched,
	// so a fresh read can never zero a live crashloop count the guards are
	// mid-way through evaluating.
	//
	// Scoped to the RUNNER container, matching runnerCrashDetail. In-pod
	// SidecarToolboxes are real containers running images taken verbatim from a
	// user-supplied CR; counting their restarts let a third party's flapping MCP
	// server terminate the session with "runner restarted N times" — overCap is
	// not gated on `crashing` — and arm fastFail against a runner that never
	// restarted.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != runnerContainerName {
			continue
		}
		sess.Status.RunnerRestarts = cs.RestartCount
		break
	}
}

// runnerCrashDetail reports whether the runner container is down — terminated
// non-zero, or crash-looping — and the crash message the kubelet captured from
// the tail of the runner's own logs via
// terminationMessagePolicy=FallbackToLogsOnError. A runner that crashed once and
// RECOVERED returns crashed=false, so a transient blip is never mistaken for a
// terminal failure. This puts the runner's ACTUAL error on the user's channel
// instead of a generic restart count.
func runnerCrashDetail(pod *corev1.Pod) (msg string, crashed bool) {
	c := runnerCrash(pod)
	return c.msg, c.crashed
}

// runnerCrashState is one reading of the runner container's crash: whether it is
// down, the message the kubelet captured, and WHEN that termination finished.
// All three come from a single read so they always describe the SAME
// termination. shouldFastFailRunner decides from the timestamp whether the
// message it is about to surface still describes a live condition, and a
// timestamp read in a different branch than the message would answer a different
// question than the one asked.
type runnerCrashState struct {
	msg        string
	finishedAt time.Time
	crashed    bool
}

// runnerCrash reads the runner container's crash state off the pod. It is the
// single implementation behind runnerCrashDetail; see that function's doc for
// the semantics of "down" and why a recovered container reports no crash.
func runnerCrash(pod *corev1.Pod) runnerCrashState {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != runnerContainerName {
			continue
		}
		if cs.Ready {
			return runnerCrashState{} // recovered — not a terminal crash
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return runnerCrashState{msg: strings.TrimSpace(t.Message), finishedAt: t.FinishedAt.Time, crashed: true}
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 {
				return runnerCrashState{msg: strings.TrimSpace(t.Message), finishedAt: t.FinishedAt.Time, crashed: true}
			}
		}
	}
	return runnerCrashState{}
}

func (r *Reconciler) setTrueCondition(sess *spiceboxv1alpha1.AgentSession, condType, reason, msg string) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionTrue, Reason: reason, Message: msg,
	})
}

func (r *Reconciler) setFalseCondition(sess *spiceboxv1alpha1.AgentSession, condType, reason, msg string) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})
}

// maxRunnerRefusalMessage bounds how much of a refusal's text is copied onto
// the condition and into the signed log. Generous enough for an admission
// verdict's full sentence (a quota names every resource it demands), short
// enough that a pathological message cannot bloat either record.
const maxRunnerRefusalMessage = 800

// surfaceRunnerRefused records a runner that could not be CREATED, then hands
// the error back for retry.
//
// Returning the error alone is not enough, and that is the whole point of this
// function: a start failure that only reaches the operator's log leaves the
// session sitting at Pending with no condition, no message, and nothing durable
// to read afterwards — a person watching sees an agent that never starts and is
// told nothing. Three records, each for a different reader:
//
//   - RunnerReady=False, carrying the refusal's own words. It is a positive
//     readiness gate (agentstatus.notReadyGates), so webd's chat health
//     watcher and channelsd's startup caption both surface it.
//   - The signed lifecycle log, which outlives the CR's status.
//   - The returned error, unchanged, so controller-runtime still retries: a
//     start failure is a verdict on this attempt, not on the session.
//
// The reason is NOT always ReasonRunnerPodRefused. That reason — and its
// "not allowed the resources it needs" phrasing in agentstatus/friendly.go —
// is reserved for a genuine admission verdict (apierrors.IsForbidden /
// IsInvalid: a quota, a policy, a webhook). Any other Start error (a
// transient apiserver error, a client-side pod-build failure) gets the
// neutral ReasonRunnerStartError instead, so a person is never told their
// resources were refused when nothing of the kind happened.
//
// The log append is gated on the condition actually CHANGING. A standing
// refusal re-reconciles for as long as it stands, and this Kind is append-only,
// so an unconditional append would grow the log without bound and bury the
// timeline it exists to be. The condition write is idempotent and always runs.
func (r *Reconciler) surfaceRunnerRefused(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, startErr error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	raw := stringsx.CapRunes(strings.TrimSpace(startErr.Error()), maxRunnerRefusalMessage)

	reason := spiceboxv1alpha1.ReasonRunnerStartError
	msg := "could not be started yet: " + raw
	if errors.IsForbidden(startErr) || errors.IsInvalid(startErr) {
		reason = spiceboxv1alpha1.ReasonRunnerPodRefused
		msg = raw
	}

	prior := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	unchanged := prior != nil &&
		prior.Status == metav1.ConditionFalse &&
		prior.Reason == reason &&
		prior.Message == msg
	if !unchanged {
		if err := r.applyEvent(ctx, sess, lifecyclecore.RunnerPodRefused{Message: msg}); err != nil {
			// Best-effort: the CR-side records below are what a person reads
			// right now, and losing the durable copy must not also lose them.
			logger.Info("agentsession: recording the runner refusal in the lifecycle log failed",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}
	}
	r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionRunnerReady, reason, msg)
	if err := r.applyStatus(ctx, sess); err != nil {
		logger.Info("agentsession: writing the runner refusal onto session status failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	logger.Info("agentsession: the runner could not be created; retrying",
		"session", sess.Namespace+"/"+sess.Name, "reason", reason, "err", msg)
	return ctrl.Result{}, fmt.Errorf("start runner: %w", startErr)
}

// markBootFailed transitions the session to phase=Failed with a Failed=True
// condition carrying reason and msg, then persists status. It is the generic
// session-boot-failed setter, serving both sidecar-toolbox boot failures and
// content-guard detector halts, and mirrors the BundleFail / RunnerCrash
// terminal-failure idiom used elsewhere in Reconcile.
func (r *Reconciler) markBootFailed(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, msg string) (ctrl.Result, error) {
	// Append the failure transition to the signed lifecycle log ONLY when this
	// reconcile is actually moving the session INTO Failed. A persistently failing
	// session re-reconciles every few seconds, so an unconditional applyEvent
	// would re-append the same event forever, growing the log without bound and
	// tainting the tamper-evident timeline. The phase and condition writes below
	// are idempotent and must still run to keep status current.
	if !isTerminalPhase(sess.Status.Phase) {
		// Read BEFORE applyEvent, which projects the new phase onto the CR:
		// podAlreadyStarted reads that same phase, so asking afterwards would see
		// Failed every time and report "no runner ever came up" for every session,
		// including one whose agent had been running for an hour.
		hadLiveRunner := podAlreadyStarted(sess)

		// ProvablyUnschedulable is the coarse "provisioning cannot complete" event;
		// the precise cause rides on the Failed condition and FailureReason.
		if err := r.applyEvent(ctx, sess, lifecyclecore.ProvablyUnschedulable{}); err != nil {
			return ctrl.Result{}, err
		}
		// Answer the trigger that started this session — but ONLY when no runner
		// ever came up. podAlreadyStarted is exactly the "is there a live agent to
		// tell" question the sidecar-pod branch above already turns on: once there
		// is one, reporting is the agent's, and an agent that ran and finished
		// without answering is the `trigger-status-concluded` completion
		// requirement's business, not this one's.
		//
		// Inside the not-yet-terminal guard, so this is the pass that MOVES the
		// session into Failed: a persistently failing session re-reconciles every
		// few seconds, and a report on every pass would talk to the provider
		// forever. After applyEvent, so a transition that could not be recorded
		// does not publish a verdict for a session that is not failing yet.
		if !hadLiveRunner {
			r.concludeTriggerStatusOnBootFailure(ctx, sess, reason)
		}
	}
	sess.Status.FailureReason = reason
	now := metav1.Now()
	sess.Status.FinishedAt = &now
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg,
	})
	return ctrl.Result{}, r.applyStatus(ctx, sess)
}

// sidecarTokenAuthzUnavailableError marks an INDETERMINATE outcome of
// materializeSidecarSecret's pre-handout use_token check — the CheckUseToken RPC
// itself errored — as distinct from a definitive deny. Callers select
// ReasonAgentSessionTokenAuthzUnavailable over the generic
// ReasonAgentSessionSidecarBootFailed, mirroring the ToolCall reconciler's
// deny-vs-indeterminate distinction. Here BOTH outcomes fail the whole session:
// a sidecar's Secret cannot be partially materialized the way a single ToolCall
// can be failed surgically while the session continues.
type sidecarTokenAuthzUnavailableError struct{ err error }

func (e *sidecarTokenAuthzUnavailableError) Error() string { return e.err.Error() }
func (e *sidecarTokenAuthzUnavailableError) Unwrap() error { return e.err }

// sidecarMaterializeFailReason picks the AgentSession Failed reason for a
// materializeSidecarSecret error: TokenAuthzUnavailable when the pre-handout
// use_token check could not be confirmed (fail-closed indeterminate),
// SidecarBootFailed for everything else — credential resolution failure, a
// definitive use_token deny, or a Secret write failure.
func sidecarMaterializeFailReason(err error) string {
	var authzErr *sidecarTokenAuthzUnavailableError
	if stderrors.As(err, &authzErr) {
		return spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable
	}
	return spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed
}

// materializeSidecarSecret resolves the upstream credential for one
// SidecarToolbox and writes the per-session Secret the sidecar container
// envFrom's. Credential resolution is CONTROLLER-side: the token is read here,
// once per pod reconcile, and projected into the declared env-var name. OAuth
// JIT-refresh does NOT apply to a sidecar cred resolved this way — the value is
// frozen at pod-create time.
//
// An empty Secret is written, without error, when the sidecar declares no
// upstream credential need: no envVar, no provider, or no identity to resolve
// against. That path MUST succeed.
//
// Before the value is written, a one-time pre-handout use_token check verifies
// that the operator's OWN authorized_token grant for this credential — written
// earlier in this same reconcile by reconcileCredentialGrants — still allows it.
// There is no per-use recheck: the sidecar freezes the credential at pod-create
// time, so ongoing revocation is pod-recreate.
func (r *Reconciler) materializeSidecarSecret(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
	rt spiceboxv1alpha1.ResolvedSidecarToolbox,
	name string,
	argsHashKey []byte,
) error {
	envVar := rt.Spec.UpstreamAuth.EnvVar

	// resolveSidecarCredential is the SAME identity pick-and-lookup that
	// reconcileCredentialGrants' sidecar enumeration uses, so the
	// CredentialSource — and therefore externaltoken.CredID — computed below for
	// the handout check matches byte-for-byte what was granted.
	ident, cred, src, ok, err := r.resolveSidecarCredential(ctx, sess, ac, rt.Ref, rt.Spec.UpstreamAuth)
	if err != nil {
		return err
	}
	if !ok {
		// No env-var to inject into, no provider declared, or no identity to
		// resolve against: the sidecar needs no upstream credential. Write an
		// empty Secret so its envFrom still resolves to an existing object.
		return r.writeSidecarSecret(ctx, sess, name, nil)
	}
	credName := rt.Ref + "-creds"

	// Classify the credential once, through the credkind registry rather than
	// switching on cred.Type, and reuse the Kind for both the adoption lookup
	// below and the minted/stored branch further down. Fail closed on a type
	// nothing understands rather than silently skipping adoption or falling
	// into the wrong resolution path — a half-configured sidecar secret is
	// worse than a loud reconcile error.
	k, kErr := credkindregistry.Get(cred.Type)
	if kErr != nil {
		return fmt.Errorf("classify credential %q for sidecar %q: %w", credName, rt.Ref, kErr)
	}

	// Adopt the upstream credential Secret so the SecretReader guard permits the
	// read. Skipped when SecretReader is unwired, where r.Client is used directly.
	if r.SecretReader != nil {
		// SecretRef LOCATES the backing Secret only; a nil ref (e.g. a minted
		// type with nothing stored) means there is nothing to adopt.
		if ref := k.SecretRef(*cred); ref != nil && ref.Name != "" {
			credRef := types.NamespacedName{Namespace: ident.Namespace, Name: ref.Name}
			ownerRef := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
			if aErr := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, credRef, ownerRef, "AgentSession"); aErr != nil && !errors.IsNotFound(aErr) {
				return fmt.Errorf("adopt credential Secret %q for sidecar %q: %w", ref.Name, rt.Ref, aErr)
			}
		}
	}

	var value string
	// A Minted credential (federated or githubApp today, any future minted
	// kind besides) is produced FRESH on every resolve, not read from a
	// stored Secret, so this branches ahead of ResolveSecretValue, which does
	// not handle a minted type. Resolve is the SAME kind-dispatched choke
	// point the inproc broker uses (resolveOneSource), so a minted kind's
	// mint logic — federation exchange, an App-installation-token exchange,
	// whatever a future kind adds — lives once, in its own package, not
	// re-derived here.
	if k.Minted() {
		minted, _, mErr := k.Resolve(ctx, credkind.Deps{Client: r.Client, Federation: r.Minter, GitHubApp: r.GitHubApp}, src)
		if mErr != nil {
			return fmt.Errorf("resolve minted credential for sidecar %q: %w", rt.Ref, mErr)
		}
		value = string(minted.AccessToken.UnderlyingValue())
	} else {
		// credresolve.ResolveSecretValue reads the credential's backing Secret and
		// gates oauth on expires_at, returning ErrExpired without exposing token
		// bytes. It is the same resolution the inproc broker uses, and the value
		// stays wrapped until written into the per-session Secret here.
		//
		// Read through the guard's LIVE reader when SecretReader is wired: the
		// upstream cred Secret was adopted just above, and only a live read sees
		// the new label immediately — the label-filtered cache would miss it until
		// the informer syncs.
		var credReader client.Reader = r.Client
		if r.SecretReader != nil {
			credReader = r.SecretReader.Reader
		}
		resolved, rErr := credresolve.ResolveSecretValue(ctx, credReader, ident.Namespace, *cred)
		if rErr != nil {
			return fmt.Errorf("resolve credential %q for sidecar %q: %w", credName, rt.Ref, rErr)
		}
		value = string(resolved.UnderlyingValue())
	}

	// One-time pre-handout use_token check. Gated on TokenChecker: if grants were
	// never written, checking against them would be meaningless.
	if r.TokenChecker != nil {
		credID := externaltoken.CredID(src)
		presented := externaltoken.ValueHash(argsHashKey, value)
		allowed, cErr := r.TokenChecker.CheckUseToken(ctx, sess.Namespace, sess.Name, credID, presented, true)
		if cErr != nil {
			log.FromContext(ctx).Info("sidecar use_token check errored; failing closed",
				"session", sess.Namespace+"/"+sess.Name, "sidecar", rt.Ref, "credID", credID, "err", cErr.Error())
			return &sidecarTokenAuthzUnavailableError{err: fmt.Errorf(
				"token-use authorization check for sidecar %q credID=%s errored: %w", rt.Ref, credID, cErr)}
		}
		if !allowed {
			log.FromContext(ctx).Info("sidecar use_token check denied; not writing credential Secret",
				"session", sess.Namespace+"/"+sess.Name, "sidecar", rt.Ref, "credID", credID)
			return fmt.Errorf("sidecar %q credential has been revoked (credID=%s, no matching authorized_token grant)", rt.Ref, credID)
		}
	}

	return r.writeSidecarSecret(ctx, sess, name, map[string]string{envVar: value})
}

// materializeCatalogToken reads a model-catalog token Secret from its system
// namespace, through the live reader when the SecretReader guard is wired. The
// caller writes the value into the per-session Secret; it never lands in a
// tenant-readable Secret of its own.
func (r *Reconciler) materializeCatalogToken(ctx context.Context, src *spiceboxv1alpha1.NamespacedSecretKeyRef) (string, error) {
	var reader client.Reader = r.Client
	if r.SecretReader != nil {
		reader = r.SecretReader.Reader
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: src.Name}, &sec); err != nil {
		return "", fmt.Errorf("read catalog token %s/%s: %w", src.Namespace, src.Name, err)
	}
	v, ok := sec.Data[src.Key]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("catalog token %s/%s missing key %q", src.Namespace, src.Name, src.Key)
	}
	return string(v), nil
}

// writeSidecarSecret create-or-updates an Opaque per-session Secret owned by the
// AgentSession, so owner-ref GC cascades it away on finalization. Idempotent:
// Create, then on AlreadyExists Get and Update, so a re-reconcile with rotated
// creds rewrites the value.
func (r *Reconciler) writeSidecarSecret(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string, env map[string]string) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       sess.Namespace,
			OwnerReferences: sessionOwnerRef(sess),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: env,
	}
	// Stamp the adoption label so this operator-minted Secret enters the
	// label-filtered cache and passes the SecretReader guard on subsequent reads.
	adoptguard.WithAdoptedLabel(sec)
	if err := r.Client.Create(ctx, sec); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create sidecar secret %q: %w", name, err)
		}
		existing, getErr := r.getSecret(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: name})
		if getErr != nil {
			return fmt.Errorf("get existing sidecar secret %q: %w", name, getErr)
		}
		// Replace wholesale: StringData only adds/overrides keys, so clear Data first or a removed credential key would persist.
		existing.Data = nil
		existing.StringData = env
		if updErr := r.Client.Update(ctx, existing); updErr != nil {
			return fmt.Errorf("update sidecar secret %q: %w", name, updErr)
		}
	}
	return nil
}

// materializePassthroughCredentials projects a userPassthrough session's
// PROJECTABLE credential VALUES into a per-session Secret in the SESSION
// namespace, owned by the AgentSession. Each value is read from the user's
// master Secret in the identities namespace and keyed by credential name. That
// is what closes the cross-namespace ToolCall denial: a sandbox ToolCall's
// credential source then resolves WITHIN its own namespace, satisfying
// ToolCall.ValidateCredentialSourceNamespaces.
//
// Which credentials those are is the caller's Projectable()-driven collector's
// answer (staticPassthroughCredentials), and today it is exactly type=static —
// oauth and federated resolve from the master or IdP-identity Secret directly,
// keeping JIT refresh and ID-JAG minting anchored there. This function must not
// re-assume that: everything it needs per credential is read back through the
// registry, so a second Projectable kind needs no edit here. Idempotent and
// re-projected each reconcile, so a refreshed master propagates.
func (r *Reconciler) materializePassthroughCredentials(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	staticCreds []spiceboxv1alpha1.AgentCredential,
) (map[string]string, error) {
	data := make(map[string]string, len(staticCreds))
	for i := range staticCreds {
		cred := &staticCreds[i]
		// Which Secret holds this credential's value is the credential kind's
		// answer, not this function's. The collector above is Projectable()-
		// driven — any kind that opts in lands here — so reading cred.Static
		// directly was a hardcoded consumer of a registry-dispatched predicate:
		// the first non-static Projectable kind would nil-panic, or, with the
		// guard, fail every session with "has type=static but static block is
		// nil" about a credential that is not static at all.
		k, kErr := credkindregistry.Get(cred.Type)
		if kErr != nil {
			return nil, fmt.Errorf("project passthrough credential %q: %w", cred.Name, kErr)
		}
		ref := k.SecretRef(*cred)
		if ref == nil {
			return nil, fmt.Errorf(
				"passthrough credential %q is type=%s, which the collector reports as projectable, but it names no backing Secret",
				cred.Name, cred.Type)
		}
		// Adopt the master Secret so the SecretReader guard permits the read.
		// NotFound is left to ResolveSecretValue below, which returns
		// ErrSecretMissing with a clear message.
		credReader := client.Reader(r.Client)
		if r.SecretReader != nil {
			credRef := types.NamespacedName{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: ref.Name}
			ownerRef := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
			if aErr := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, credRef, ownerRef, "AgentSession"); aErr != nil && !errors.IsNotFound(aErr) {
				return nil, fmt.Errorf("adopt master credential Secret %q for passthrough credential %q: %w", ref.Name, cred.Name, aErr)
			}
			credReader = r.SecretReader.Reader
		}
		val, err := credresolve.ResolveSecretValue(ctx, credReader, spiceboxv1alpha1.IdentitiesNamespace, *cred)
		if err != nil {
			return nil, fmt.Errorf("project passthrough credential %q: %w", cred.Name, err)
		}
		data[cred.Name] = string(val.UnderlyingValue())
	}
	// writeSidecarSecret is the generic owned-per-session Opaque Secret writer,
	// reused here.
	if err := r.writeSidecarSecret(ctx, sess, spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name), data); err != nil {
		return nil, err
	}
	return credHashes(data), nil
}

// findSidecarCredential returns the credential named name from creds, or nil.
func findSidecarCredential(creds []spiceboxv1alpha1.AgentCredential, name string) *spiceboxv1alpha1.AgentCredential {
	for i := range creds {
		if creds[i].Name == name {
			return &creds[i]
		}
	}
	return nil
}

func classIsValid(ac *spiceboxv1alpha1.AgentClass) bool {
	for _, c := range ac.Status.Conditions {
		if c.Type == spiceboxv1alpha1.AgentClassConditionValid {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// podAlreadyStarted reports whether the runner pod has been created for this
// session (so a newly-fatal settings violation should grandfather it rather
// than refuse). RunnerReady being present (any status) means Start ran.
func podAlreadyStarted(s *spiceboxv1alpha1.AgentSession) bool {
	return conditions.Find(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady) != nil ||
		s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning
}

// imagePinGateViolation reports the first sidecar toolbox whose image pin has
// drifted while the effective pinning mode is "block", meaning the session must
// not start. ("", nil) means nothing gates. The Gets are cheap informer-cache
// reads.
//
// PinVerifyFailed — a probe error or unresolvable digest — deliberately does NOT
// gate: where a baseline exists the sidecar spec is rewritten to launch by the
// recorded digest, so the bytes are pinned even when the probe cannot re-verify.
// Gating on it would add denial without adding safety.
func (r *Reconciler) imagePinGateViolation(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass, eff *spiceboxv1alpha1.EffectiveSettings) (msg string, err error) {
	var clusterPol, namespacePol *spiceboxv1alpha1.PinningPolicy
	if eff != nil && eff.Pinning != nil {
		clusterPol = eff.Pinning.Cluster
		namespacePol = eff.Pinning.Namespace
	}
	for _, ref := range ac.Spec.SidecarToolboxes {
		var tb spiceboxv1alpha1.SidecarToolbox
		if getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &tb); getErr != nil {
			if errors.IsNotFound(getErr) {
				// Missing toolbox is handled as a terminal failure later in the
				// resolve loop; not a block-gate concern.
				continue
			}
			return "", fmt.Errorf("imagePinGateViolation: get SidecarToolbox %q: %w", ref.Ref, getErr)
		}
		driftCond := conditions.Find(tb.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
		if driftCond == nil || driftCond.Status != metav1.ConditionFalse || driftCond.Reason != spiceboxv1alpha1.ReasonPinDrifted {
			continue
		}
		pinReq := settings.PinRequirementFor(clusterPol, namespacePol, "image", ref.Ref)
		if pinReq.Mode == spiceboxv1alpha1.PinModeBlock {
			return fmt.Sprintf("SidecarToolbox %q image has drifted from its pin baseline (%s); update the baseline or re-freeze with an @sha256 ref to unblock", ref.Ref, driftCond.Message), nil
		}
	}
	return "", nil
}

// witnessAuditKey records, in the session's own append-only scope, which key
// this AgentSession instance signs its records with — the operator attesting
// what its status already says, somewhere the status cannot take with it when
// the CR is deleted.
//
// Written on every reconcile that has a key on status, and idempotent by
// construction: the entry ID is the key's content address and its createdAt is
// the session's creationTimestamp, so a re-record is a byte-identical re-put,
// which the append-only facade answers with the stored entry and no write. A
// wall-clock stamp would make every reconcile a conflict instead.
//
// Best-effort and LOGGED. Losing the witness costs the offline verifiability of
// this session's records after its CR is gone; it costs the running session
// nothing, and failing a reconcile over it would take down sessions for a
// forensic aid.
func (r *Reconciler) witnessAuditKey(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) {
	if r.AuditKeyMemory == nil || sess.Status.AuditKeyID == "" || sess.Status.AuditPublicKey == "" {
		return
	}
	err := auditkey.Witness(ctx, r.AuditKeyMemory,
		memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name},
		string(sess.UID), sess.Status.AuditKeyID, sess.Status.AuditPublicKey,
		sess.CreationTimestamp.Time)
	besteffort.Log(log.FromContext(ctx).Info, "audit: witness per-session audit key binding", err,
		"session", sess.Namespace+"/"+sess.Name, "keyID", sess.Status.AuditKeyID)
}

// recordAuditChainHeads computes the per-publisher final chain heads for a
// completed session's scope and stamps them onto status.AuditChainHeads as
// publisher → "seq:lastHash". Called once per session, the first time it reaches
// a terminal phase with no heads recorded; the caller has already checked
// r.AuditChainHeads != nil and status.AuditChainHeads == nil.
func (r *Reconciler) recordAuditChainHeads(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	heads, err := r.AuditChainHeads(ctx, scope)
	if err != nil {
		return fmt.Errorf("compute chain heads: %w", err)
	}
	// Empty heads are not persisted: an empty map round-trips to nil, so writing
	// it would not latch the guard, and skipping avoids per-reconcile churn.
	if len(heads) == 0 {
		return nil
	}
	formatted := make(map[string]string, len(heads))
	for publisher, h := range heads {
		formatted[publisher] = fmt.Sprintf("%d:%s", h.Seq, h.LastHash)
	}
	sess.Status.AuditChainHeads = formatted
	if err := r.applyStatus(ctx, sess); err != nil {
		return fmt.Errorf("persist chain heads: %w", err)
	}
	return nil
}

func isTerminalPhase(p string) bool {
	return p == spiceboxv1alpha1.AgentSessionPhaseSucceeded || p == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// reapAction is the decision for a terminal session's pods: leave them alone,
// requeue until the grace elapses, or reap them now.
type reapAction int

const (
	reapActionNone    reapAction = iota // not eligible / reaping disabled — leave the pods alone
	reapActionRequeue                   // within grace — requeue after the returned remaining
	reapActionReap                      // grace elapsed (or n/a) — tear the pods down
)

// terminalReapAction decides whether a terminal session's pods should be reaped.
// Succeeded (archived) reaps immediately — an archived session is done; the next
// inbound spawns a fresh session, so nothing needs the warm pods. Failed reaps
// after failedGrace (kept briefly for crash debugging). grace<=0 disables the
// Failed path only (Succeeded still reaps; archived leaks are never wanted).
// A Failed session with no finishedAt stamp measures the grace from its creation
// timestamp instead (the same fallback reconcileStorageReclaim uses), so a
// terminal phase reached via the plain lifecycle fold still reaps; Succeeded
// reaps regardless of finishedAt.
func terminalReapAction(phase string, finishedAt *metav1.Time, created metav1.Time, failedGrace time.Duration, now time.Time) (reapAction, time.Duration) {
	switch phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
		return reapActionReap, 0
	case spiceboxv1alpha1.AgentSessionPhaseFailed:
		if failedGrace <= 0 {
			return reapActionNone, 0
		}
		// A Failed phase reached without a FinishedAt stamp still reaps, off the
		// creation timestamp — the same fallback reconcileStorageReclaim uses. The
		// steady-state phase write folds the lifecycle log to Failed WITHOUT
		// stamping FinishedAt (only the explicit markBootFailed / runner-crash /
		// retry-TTL paths do), so a session that reaches Failed that way would
		// otherwise return None here forever and strand its sandbox pods on a node.
		ref := created.Time
		if finishedAt != nil {
			ref = finishedAt.Time
		}
		if remaining := failedGrace - now.Sub(ref); remaining > 0 {
			return reapActionRequeue, remaining
		}
		return reapActionReap, 0
	default:
		return reapActionNone, 0
	}
}

// isTerminalForPodLifecycle returns true for phases where the reconciler
// must NOT create or respawn the runner pod. Idle and AwaitingRetry
// are included because channel-attached sessions park there between
// turns; the wake-annotation path (shouldWake) transitions them out
// before this check runs.
func isTerminalForPodLifecycle(p string) bool {
	return p == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		p == spiceboxv1alpha1.AgentSessionPhaseFailed ||
		p == spiceboxv1alpha1.AgentSessionPhaseIdle ||
		p == spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
}

// collectCredentialSecretNames returns the deduplicated, sorted set of Secret
// names an AgentIdentity's credentials reference, skipping empty ones and any
// credential of an unregistered type (logged: unlike "no backing Secret",
// that is a wiring bug this pin set would otherwise mask by silently
// omitting the credential's Secret). This is the exact pin set for the
// per-session Role's MCP `secrets` get rule.
//
// Dispatches through credkindregistry.SecretNameFor rather than switching on
// cred.Static/OAuth/Federated — see credkind/guard_test.go's Guard 6.
func collectCredentialSecretNames(ctx context.Context, ai *spiceboxv1alpha1.AgentIdentity) []string {
	seen := make(map[string]struct{})
	for _, cred := range ai.Spec.Credentials {
		name, err := credkindregistry.SecretNameFor(cred)
		if err != nil {
			log.FromContext(ctx).Info("collectCredentialSecretNames: credential has an unregistered type; its Secret is excluded from the per-session Role's secrets pin set",
				"agentIdentity", ai.Namespace+"/"+ai.Name, "credential", cred.Name, "type", cred.Type, "err", err.Error())
			continue
		}
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// runnerNATSUserGrant builds the per-session NATS user grant for a runner.
//
// SUBSCRIBE is the whole session subject tree: the runner listens on in.* for
// its wake-up, interrupts, tool-session input and app-tool calls, and on out.*
// for the applied bridge that resumes an approval. It also subscribes to the
// cluster-wide revocation bus so in-flight credential and tool revocations reach
// it. Revocation is subscribe-only — the runner consumes revokes and never emits
// them — so revocation.Subject is deliberately absent from PubAllow. Omitting it
// from SubAllow makes the JWT deny the subscription, surfacing as a NATS
// "Permissions Violation" and silently disabling revocation for that session.
//
// PUBLISH is ENUMERATED, not the subtree. `<prefix>.>` would include
// `<prefix>.in.interaction_decision`, and channelsd's decision pipe takes the
// acting principal from the payload's unauthenticated Decider field. The runner
// knows the owner's external identity, so a subtree grant would let the very
// component the approval gate exists to constrain publish a decision naming the
// owner as decider. Denying it server-side is defense in depth — a runner pod
// also carries the operator's unscoped SpiceDB token today, so forging buys it
// nothing yet — and becomes load-bearing once that token problem is closed.
//
// MonitoringEventSubject is the one CLUSTER-SCOPED subject here. The runner is
// the only component that can observe an agent's definition failing at
// evaluation — a CEL trust rule that compiles but errors on a real argument is
// invisible to every reconcile — and reporting it needs the fanout subject the
// operator's watcher already uses. Publish-only and carrying no authorization
// weight: the worst a misbehaving runner buys is channel noise.
//
// The four IN subjects are the only inbound ones the runner publishes, and
// every one is on its OWN prefix. agent_message_send is the newest and the
// one that shows why the enumeration matters: it asks channelsd to deliver a
// message to ANOTHER session (a delegating parent answering its child), and
// it is published here, on the sender's own subject, precisely because
// granting the runner publish on the destination's subtree would let any
// runner inject inbound traffic into any session in the cluster. channelsd
// corroborates the destination against a real Channel joining the two before
// carrying anything; see channelevents.KindAgentMessageSend.
//
// The OUT side stays a wildcard because the runner publishes many out-kinds,
// and it must be `out.>` rather than `out.*` since kinds like
// "assistant.stream.delta" contain dots and span several tokens. `_INBOX.>`
// stays broad on PUBLISH because the runner is itself a responder: it answers
// a browser widget's app-tool call with m.Respond, publishing into webd's
// inbox, not its own.
//
// The grant NAME comes from apnats.PrincipalName, never concatenation, and the
// error it can return is load-bearing. That name is the sole input to the reply
// inbox on BOTH sides of the wire — the SubAllow entry below and the client's
// CustomInboxPrefix, re-derived from the JWT's Name claim — so two sessions
// yielding one name share an inbox root, and each one's JWT then authorizes
// reading the other's replies, thread and channel history included. Joining on
// '-' did exactly that: '-' is legal in both a DNS-1123 namespace and name, so
// (team-a, bot) and (team, a-bot) both produced "runner-team-a-bot" —
// collision-free-looking, and attacker-selectable by anyone who can name a
// session in a namespace they control.
//
// Refusing such a name also closes a second overlap: session names may contain
// '.', and the subject prefix is dot-joined, so a session "a" would hold an
// `ap.session.<ns>.a.>` subtree grant covering every subject of a session "a.b".
// channelevents.parseSessionSubject already depends on that no-dot assumption
// when routing on the authorized subject, so failing closed here enforces an
// invariant the bus already relies on rather than adding a new restriction.
func runnerNATSUserGrant(namespace, name string) (apnats.UserGrant, error) {
	prefix := subjects.Session(namespace, name)
	grantName, err := apnats.PrincipalName("runner", namespace, name)
	if err != nil {
		return apnats.UserGrant{}, fmt.Errorf("build per-session NATS principal name for %s/%s: %w", namespace, name, err)
	}
	return apnats.UserGrant{
		Name: grantName,
		PubAllow: []string{
			prefix.OutTree(),
			prefix.History(),
			prefix.ChannelHistory(),
			prefix.In(string(channelevents.KindMetaagentRequest)),
			prefix.In(string(channelevents.KindInteractionRequest)),
			prefix.In(string(channelevents.KindInteractionApplied)),
			prefix.In(string(channelevents.KindAgentMessageSend)),
			channelevents.MonitoringEventSubject,
			apnats.InboxRoot + "." + subjects.Tail,
		},
		SubAllow: []string{
			prefix.Tree(),
			apnats.InboxSubjectFor(grantName),
			revocation.Subject,
		},
	}, nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateAuditKeypair mints an Ed25519 keypair, returning the hex seed for the
// Secret and the base64 public key plus keyID for status.
func generateAuditKeypair() (seedHex, pubB64, keyID string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	seed := priv.Seed()
	return hex.EncodeToString(seed), base64.StdEncoding.EncodeToString(pub), provenance.KeyID(pub), nil
}

// syncAuditKeyStatus re-derives status.auditPublicKey/auditKeyID from the
// per-session Secret's signing seed and stamps them when they disagree,
// reporting whether anything changed so the caller writes status only on a
// meaningful change.
//
// It runs on EVERY reconcile of a session whose Secret carries a seed, not only
// when status is empty. The seed is the sole source of truth for this session's
// audit identity and status is a projection of it. A session runner holds
// `patch` on its own agentsessions/status, and the operator registers whatever
// status holds as a trusted verify-on-write key — which `oap audit verify`
// rebuilds its offline registry from. Backfilling only on empty would let a
// runner-written key survive forever: it could sign honest entries under the
// real key, rotate the anchor to one of its own, and leave the genuine prefix
// failing verification while its forgeries verify. Re-deriving makes any such
// write self-healing on the next reconcile, and the admission webhook refuses it
// outright.
//
// The seed is never rotated under a live session, so a legitimate session
// converges on the first pass and this is a no-op afterwards — no status churn.
func syncAuditKeyStatus(sess *spiceboxv1alpha1.AgentSession, seedHex string) (changed bool, err error) {
	pubB64, keyID, err := auditPubFromSeed(seedHex)
	if err != nil {
		return false, err
	}
	if sess.Status.AuditPublicKey == pubB64 && sess.Status.AuditKeyID == keyID {
		return false, nil
	}
	sess.Status.AuditPublicKey = pubB64
	sess.Status.AuditKeyID = keyID
	return true, nil
}

// auditPubFromSeed derives the base64 public key and keyID from a hex-encoded
// Ed25519 seed, backfilling status from a Secret that already carries one.
func auditPubFromSeed(seedHex string) (pubB64, keyID string, err error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return "", "", err
	}
	if len(seed) != ed25519.SeedSize {
		return "", "", fmt.Errorf("audit seed has %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub), provenance.KeyID(pub), nil
}

func isSessionReady(s *spiceboxv1alpha1.SpiceboxSession) bool {
	for _, c := range s.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// upsertResolvedBundle replaces the entry named rb.Name, or appends it,
// preserving order. The retry branch needs it to persist the retry ledger
// mid-loop, before deleting the failed bundle instance.
func upsertResolvedBundle(list []spiceboxv1alpha1.ResolvedBundle, rb spiceboxv1alpha1.ResolvedBundle) []spiceboxv1alpha1.ResolvedBundle {
	for i := range list {
		if list[i].Name == rb.Name {
			list[i] = rb
			return list
		}
	}
	return append(list, rb)
}

func hasFailedSession(s *spiceboxv1alpha1.SpiceboxSession) bool {
	for _, c := range s.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// failedSessionDetail returns "reason: message" (or just the reason) for a bundle
// SpiceboxSession's true Failed condition, so the AgentSession can relay the real
// cause (e.g. an ImagePullBackOff) to the user's channel rather than a generic
// "bundle is Failed". Empty when the session is not failed.
func failedSessionDetail(s *spiceboxv1alpha1.SpiceboxSession) string {
	for _, c := range s.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed && c.Status == metav1.ConditionTrue {
			if c.Message != "" {
				return c.Reason + ": " + c.Message
			}
			return c.Reason
		}
	}
	return ""
}
