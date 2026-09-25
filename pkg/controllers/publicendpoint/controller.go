// Package publicendpoint reconciles the PublicEndpoint CR: it opens the
// tunnel that makes this cluster reachable from the public Internet and
// publishes where it is reachable on status.url.
//
// # The tunnel runs IN THIS PROCESS
//
// There is no agent Deployment and no agent image. Every registered provider
// (pkg/web/localtunnel/registry) is a Go client — ngrok's is built on its Go
// agent SDK, where Tunnel.Start opens the tunnel inside the calling process
// — so the operator holds the live localtunnel.Tunnel itself, keyed by CR
// name, and Reconcile is what opens it, keeps it, and tears it down. That
// keeps the credential in exactly one pod (the operator reads the Secret
// directly; no auth token is plumbed into a second workload) and needs no
// image at all.
//
// The consequence is that this reconciler owns a live, process-local resource,
// which imposes four rules the rest of the file is shaped around:
//
//   - ONE tunnel per CR. Reconcile runs repeatedly; a second Start would take
//     a second provider session (ngrok's free tier allows exactly one) and
//     would change the published URL for no reason. Reconciles that find an
//     open tunnel at the CR's current metadata.generation are a cheap no-op.
//     "Unchanged" means generation, so a spec edit reopens and a status write
//     or resync does not.
//   - THE MAP LOCK IS NEVER HELD ACROSS I/O. Start blocks on the network and
//     Stop can too. The lock is taken to claim, commit, or hand back an entry
//     and released before any provider call; a claim marks the entry `opening`
//     so a concurrent reconcile of the same CR waits instead of opening a
//     second session.
//   - DELETION MUST STOP THE TUNNEL. Without a finalizer the CR vanishes, the
//     map entry is never revisited, and the provider session stays open for
//     the operator's lifetime with nothing referencing it. FinalizerPublicEndpoint
//     is what guarantees the teardown reconcile happens.
//   - THE TUNNEL'S CONTEXT IS NOT THE RECONCILE'S. A provider keeps the context
//     handed to Start for the tunnel's WHOLE LIFE (ngrok's forward loop returns
//     on ctx.Done), while controller-runtime's reconcile context is
//     request-scoped: it is cancelled on Reconcile's return whenever the manager
//     sets a ReconciliationTimeout. Passing the reconcile context would arm a
//     silent total failure — one standard hardening knob, set by someone who
//     never read this file, and every tunnel dies the instant it is published
//     while the CR still reports Ready with a URL that forwards nothing and
//     claimOpen short-circuits every later reconcile without consulting the
//     provider. Each tunnel therefore gets its OWN context (context.WithCancel
//     over context.WithoutCancel), cancelled by us when the tunnel is stopped.
//
// Process restart is not a leak: the tunnels die with the process and the next
// reconcile reopens them. Leader election means only the elected operator
// replica runs this controller, so replicas do not each open a session.
//
// # Status
//
// status.url / .phase / .observedAt are OBSERVATIONS, written on the status
// subresource only, and only when they actually change — an unconditional
// write every reconcile churns the object and re-triggers every watcher.
// status.url is non-empty exactly when this process holds an open tunnel.
//
// # This controller is the single writer of webd's external URL
//
// The spicebox-webd-external-url ConfigMap carries the address webd builds
// every redirect, OAuth redirect_uri and artifact deep-link from, and the
// channel planner seeds a new channel's external-base-url from it. It used to
// have two writers, both of which wrote a LOOPBACK address: `oap install`
// seeded it with localhost defaults and `oap desktop` rewrote it to
// http://127.0.0.1:<port> on every boot. Neither knew a tunnel existed, so a
// GitHub App minted from that value was refused outright — "Hook url is not
// supported because it isn't reachable over the public Internet (127.0.0.1)"
// — by a user who never typed a URL at all.
//
// This reconciler now owns it, in BOTH states, because one writer with two
// states is the only shape in which the loopback value cannot outlive the
// tunnel that replaces it:
//
//   - READY publishes status.url.
//
//   - PENDING with no open in flight publishes spec.localURL — the host-side
//     address whoever created the CR knows and the cluster does not. An empty
//     ConfigMap is not a neutral state — it breaks every link webd mints — so
//     the reachable-locally address is the correct answer when nothing public
//     exists yet, and it must be the CREATOR's address: a guess of "localhost"
//     where the truth is "127.0.0.1" (or of :8080 where the truth is a
//     runtime-chosen port) names an address the operator's browser is not on,
//     and every link webd mints from it goes nowhere.
//
//     What this value does NOT decide is whether that local address is served.
//     Once a tunnel is up, both keys name it and webd's links are the tunnel's
//     — while a request arriving on the loopback is still answered, because on
//     a kind whose profile allows a shared origin webui.ServeHTTP treats a
//     loopback host as that shared host (see its dispatch). The desktop's own
//     console reaches webd that way whether or not a tunnel exists.
//
//   - PENDING WHILE AN OPEN IS IN FLIGHT writes nothing. status.url is empty in
//     that window by design and the real address is seconds away, held by the
//     reconcile that owns the `opening` reservation; writing a loopback here
//     would flap a live public URL out and back on every reopen.
//
//   - FAILED writes nothing, leaving the last good value. A stale URL is bad;
//     an empty one is worse.
//
//   - DELETED gives both keys back, before the finalizer is removed — see
//     releaseExternalURL. This is the one state where an absent value beats a
//     stale one: the tunnel is gone for good, so what is left otherwise is a
//     public address forwarding nothing, permanently, on a kind where nothing
//     else writes this ConfigMap.
//
// Two things gate the write shut entirely, both fail-closed:
//
//   - AN ENDPOINT THAT DOES NOT TARGET WEBD never writes it. The CRD's target
//     is an arbitrary Service, and webd's external URL is what the channel
//     planner seeds a channel's external-base-url from — so an endpoint
//     tunnelling, say, a dashboard would have GitHub delivering webhook
//     payloads, signed with that channel's secret, to a third-party service.
//     Two endpoints would also flap the value against each other forever, since
//     one field owner plus ForceOwnership is last-writer-wins, not a conflict.
//   - A CLUSTER KIND WHOSE PublicEndpointPolicy IS `Never` opens no tunnel and
//     writes nothing. Those clusters have real ingress and `oap install` has
//     already pointed webd at a genuine https:// host — or seeded the ConfigMap
//     EMPTY so credential links fail closed until it does. Overwriting either
//     with a loopback address, every 30s under ForceOwnership, is precisely the
//     failure install goes out of its way to prevent.
//
// The write is a server-side apply of the two keys, not a read-modify-write:
// the operator's ConfigMap informer is label-filtered to ADOPTED objects
// (see adoptguard), so a cached read of this fixed-infra ConfigMap would miss,
// and SSA needs no read at all. It is create-or-update in one call, it takes
// only the two keys it names (leaving any other key to its own owner), and an
// apply whose result is identical to what is stored is a no-op at the
// apiserver — no resourceVersion bump, no watch event — which is what keeps a
// per-reconcile apply from churning an object webd polls.
package publicendpoint

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"
	// externalurl is webd's READER for this ConfigMap. Taking the namespace
	// from it rather than re-declaring the string is what keeps the single
	// writer and the reader pointed at the same object by construction.
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

const (
	// ownerKind is the owner label adoptkit stamps on the auth-token Secret.
	ownerKind = "PublicEndpoint"

	// secretRequeue is how often to re-check an unresolvable authTokenRef.
	// Matches ClusterIdentityProvider's cadence, and for the same reason: a
	// PublicEndpoint's Secret lives in an arbitrary namespace, and watching
	// every Secret cluster-wide costs far more than a slow re-check.
	secretRequeue = 30 * time.Second

	// startFailureRequeue is how long to wait before retrying a provider that
	// refused to open the tunnel. Provider outages and quota exhaustion clear
	// on their own; nothing else would re-trigger this CR.
	startFailureRequeue = 30 * time.Second

	// openingRequeue is how long to wait when another reconcile of the SAME CR
	// is already inside the provider's Start. Short, because it resolves as
	// soon as that call returns.
	openingRequeue = 2 * time.Second

	// externalURLReleaseGrace bounds how long a deletion may be held up by a
	// ConfigMap this controller cannot give back.
	//
	// It exists because both unbounded answers are wrong. Retry forever and a
	// permanently unwritable ConfigMap — a revoked RBAC rule, a namespace being
	// torn down — makes the CR undeletable, wedging every `kubectl delete` and
	// every namespace teardown behind it. Give up immediately and one apiserver
	// blip leaves a dead public URL advertised for the life of the cluster,
	// which is the very failure the release exists to prevent. So: retry with
	// backoff for this long, then let the object go and say what was left
	// behind.
	externalURLReleaseGrace = 2 * time.Minute

	// externalURLFieldOwner is the SSA field manager this controller writes
	// webd's external-URL ConfigMap under, so its two keys are visibly owned
	// here and a foreign write to them shows up as a field-manager conflict
	// rather than a silent last-writer-wins race.
	//
	// Defined in pkg/apis/v1alpha1 rather than here: a host-side writer of the
	// same ConfigMap reads it to tell whether this controller has taken the
	// keys yet, so it is part of the contract, not private to this file.
	externalURLFieldOwner = v1.WebdExternalURLFieldOwner
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=publicendpoints,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=publicendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=publicendpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;patch

// This controller is the SINGLE WRITER of webd's external-URL ConfigMap, which
// it server-side-applies. SSA needs `patch`, plus `create` for the first apply
// on a cluster where the ConfigMap does not exist yet.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;patch

// Reconciler reconciles PublicEndpoint objects and owns the tunnels they open.
type Reconciler struct {
	Client client.Client
	// SecretReader is the guarded reader for the auth-token Secret. Reads are
	// gated to Secrets the operator has adopted (carrying AdoptedLabel) or the
	// fixed-infra allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
	// ClusterKind is the resolved AP_CLUSTER_KIND this operator was installed
	// for. Its InstallProfile().PublicEndpointPolicy() decides whether a tunnel
	// may be opened here at all. Must be set before Reconcile is called;
	// declared as the interface (never as a concrete pointer) so an unwired
	// Reconciler holds a genuinely nil interface the guard below can catch,
	// rather than a typed-nil that panics on first use.
	ClusterKind cloud.Strategy
	// Now supplies status.observedAt. Nil means time.Now; tests inject a fixed
	// clock so they can tell the controller's stamp from anything else's.
	Now func() time.Time

	// mu guards tunnels. It is NEVER held across a provider call — see the
	// package comment.
	mu sync.Mutex
	// tunnels is the live tunnel this process holds per PublicEndpoint name
	// (the CR is cluster-scoped, so the name is the whole key).
	tunnels map[string]*heldTunnel
	// stopped is set once stopAll has run. An open still in flight then
	// commits into a map nothing will ever drain again, so commit refuses and
	// the opener stops its own tunnel instead of leaking a live session for
	// the rest of the process's life.
	stopped bool
}

// heldTunnel is one CR's slot in the map. An entry with opening=true is a
// reservation: some reconcile is inside Start for it, and tunnel/cancel are
// still nil.
type heldTunnel struct {
	generation int64
	url        string
	tunnel     localtunnel.Tunnel
	// cancel ends the context this tunnel was OPENED with — its own, not the
	// reconcile's (see the package comment's fourth rule). Calling it is what
	// releases a provider parked on that context; Stop alone would leave a
	// provider whose forward loop only exits on ctx.Done running.
	cancel  context.CancelFunc
	opening bool
}

// stopHeld ends a held tunnel: cancels the context it was opened with, then
// asks the provider to close it.
//
// MUST be called with Reconciler.mu NOT held — Stop can block on the network,
// and this is why claim/take hand entries back to their caller rather than
// stopping them under the lock. cancel goes first because it is non-blocking
// and unparks any provider goroutine waiting on the context, so Stop is not
// racing a live forward loop; both are idempotent.
func stopHeld(held *heldTunnel) error {
	if held == nil {
		return nil
	}
	if held.cancel != nil {
		held.cancel()
	}
	if held.tunnel == nil {
		return nil
	}
	return held.tunnel.Stop()
}

// outcome is one reconcile's judgment: what to publish and how to requeue.
// message empty means the Ready condition is True.
type outcome struct {
	result  ctrl.Result
	url     string
	phase   string
	reason  string
	message string

	// externalURL is what webd's external-URL ConfigMap must hold once this
	// outcome is published; EMPTY means "leave whatever is there alone".
	//
	// It is a separate field from url rather than derived from url/phase at the
	// write site because the three ConfigMap states do not line up with the
	// three phases: a Pending outcome writes the loopback address, but a
	// Pending outcome that is Pending BECAUSE an open is in flight must write
	// nothing at all. Deriving it would put that distinction in a reader's
	// blind spot; carrying it makes every construction site below state its
	// answer out loud.
	externalURL string
}

func (o outcome) ready() bool { return o.phase == v1.PublicEndpointPhaseReady }

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("publicendpoint", req.Name)

	var pe v1.PublicEndpoint
	if err := r.Client.Get(ctx, req.NamespacedName, &pe); err != nil {
		if apierrors.IsNotFound(err) {
			// The CR is gone without our finalizer having run — force-removed
			// finalizers, or the CRD itself deleted. Stop whatever this process
			// still holds for it, or the provider session leaks for the
			// operator's lifetime with nothing left to reference it.
			stopped, busy, err := r.closeTunnel(req.Name)
			switch {
			case busy:
				// An open is still in flight for a CR that no longer exists.
				// Come back for it — returning here would strand the tunnel
				// it is about to finish opening, which is the same reasoning
				// finalize applies to the same situation.
				logger.Info("a deleted PublicEndpoint's tunnel is still opening; will close it on the next pass")
				return ctrl.Result{RequeueAfter: openingRequeue}, nil
			case err != nil:
				logger.Info("stopping the tunnel of an already-deleted PublicEndpoint failed", "err", err.Error())
			case stopped:
				logger.Info("tunnel closed for an already-deleted PublicEndpoint")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get PublicEndpoint %s: %w", req.Name, err)
	}

	// Deletion first: releasing a CR touches no Secret, so a mis-wired
	// SecretReader must not be able to wedge deletions.
	if !pe.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &pe)
	}

	if r.SecretReader == nil {
		// Fail loudly rather than nil-panic deep inside the credential read: a
		// Reconciler wired without its guarded reader is a startup bug.
		return ctrl.Result{}, fmt.Errorf("publicendpoint %s: Reconciler.SecretReader is not wired", pe.Name)
	}
	if r.ClusterKind == nil {
		// Same reasoning, and the consequence is worse: without the cluster
		// kind there is no way to tell a dev cluster from one with real
		// ingress, and guessing "allowed" would let a tunnel overwrite a
		// production webd URL. Loud error, no reconcile.
		return ctrl.Result{}, fmt.Errorf("publicendpoint %s: Reconciler.ClusterKind is not wired", pe.Name)
	}

	// The finalizer goes on BEFORE any tunnel is opened, so there is never a
	// window where a session is live and nothing guarantees its teardown.
	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &pe, v1.FinalizerPublicEndpoint); added || err != nil {
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to PublicEndpoint %s: %w", pe.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	prior := pe.DeepCopy()
	o := r.evaluate(ctx, &pe)

	// status.url is non-empty exactly while a tunnel is open, so any non-Ready
	// outcome also tears down whatever this process was holding. A revoked
	// Secret or a spec pointed at an unregistered provider must not leave a
	// live session behind that nothing publishes.
	if !o.ready() {
		switch stopped, busy, err := r.closeTunnel(pe.Name); {
		case busy:
			// Entirely normal: a concurrent reconcile is mid-open and owns the
			// entry. Logged at debug, NOT as a failure — this path repeats
			// every openingRequeue while an open is in flight, and
			// failure-shaped noise on a loop for a healthy state is how real
			// failures get tuned out.
			logger.V(1).Info("tunnel is still opening; leaving it to the reconcile that owns it",
				"reason", o.reason)
		case err != nil:
			logger.Info("stopping the tunnel of a no-longer-ready PublicEndpoint failed",
				"phase", o.phase, "reason", o.reason, "err", err.Error())
		case stopped:
			logger.Info("tunnel closed", "reason", o.reason)
		}
	}

	// Publish webd's external URL BEFORE the status comparison below, so a
	// reconcile that finds status unchanged still reasserts the ConfigMap. The
	// two objects drift independently: nothing watches the ConfigMap back onto
	// this controller, so a reconcile that skipped the apply whenever status
	// happened to match would leave an out-of-band edit standing until the next
	// spec change.
	//
	// A failure here is NOT fatal to the status write — the CR must still say
	// what this reconcile decided — so it is logged with context now and
	// returned from every exit below, where it becomes a requeue-with-backoff.
	cmErr := r.publishExternalURL(ctx, &pe, o)
	if cmErr != nil {
		logger.Info("publishing webd's external URL failed; the CR's status is still written",
			"phase", o.phase, "reason", o.reason, "externalURL", o.externalURL, "err", cmErr.Error())
		cmErr = fmt.Errorf("publish webd external URL for PublicEndpoint %s: %w", pe.Name, cmErr)
	}

	pe.Status.URL = o.url
	pe.Status.Phase = o.phase
	if o.ready() {
		conditions.SetTrue(&pe, &pe.Status.Conditions, v1.PublicEndpointConditionReady, o.reason)
	} else {
		conditions.SetFalse(&pe, &pe.Status.Conditions, v1.PublicEndpointConditionReady, o.reason, o.message)
	}

	// Compare with observedAt neutralized: it is stamped ON CHANGE, so
	// including a fresh timestamp in the comparison would make every reconcile
	// look changed and write.
	//
	// The assignment is inert as the code stands — nothing above touches
	// observedAt, so it already equals prior's. It is here so that a future
	// edit which DOES stamp it before this point cannot quietly turn every
	// reconcile back into a write.
	settled := *pe.Status.DeepCopy()
	settled.ObservedAt = prior.Status.ObservedAt
	if equality.Semantic.DeepEqual(prior.Status, settled) {
		logger.V(1).Info("status unchanged, skipping write", "phase", o.phase, "reason", o.reason)
		// A non-nil error and a RequeueAfter cannot be returned together:
		// controller-runtime IGNORES the result whenever the error is non-nil
		// and logs a warning saying so, which on the Pending path would both
		// spam the log and silently drop the timed re-check. Return the error
		// alone and let its rate-limited backoff be the retry.
		if cmErr != nil {
			return ctrl.Result{}, cmErr
		}
		return o.result, nil
	}

	pe.Status.ObservedAt = &metav1.Time{Time: r.now()}
	if err := r.Client.Status().Patch(ctx, &pe, client.MergeFrom(prior)); err != nil {
		// cmErr, if any, was already logged with full context above, so it is
		// reported rather than dropped even though the status failure is what
		// this return carries.
		return ctrl.Result{}, fmt.Errorf("write PublicEndpoint %s status: %w", pe.Name, err)
	}
	logger.Info("PublicEndpoint reconciled", "phase", o.phase, "reason", o.reason, "url", o.url)
	// Same rule as the early return above: never both.
	if cmErr != nil {
		return ctrl.Result{}, cmErr
	}
	return o.result, nil
}

// publishExternalURL points webd's external-URL ConfigMap at this outcome's
// address, creating the ConfigMap if a cluster does not have one yet.
//
// An empty outcome.externalURL means this reconcile has no address to publish,
// and the ConfigMap is left exactly as it is — see the package comment for the
// three states and why "leave it alone" is never the same as "blank it".
//
// BOTH origins get the same value. webd splits a trusted auth origin from a
// sandbox content origin, but a single tunnel (or a single loopback port) is
// one host serving both, and webd gates shared-origin serving per request. A
// writer that set only trusted-url would leave artifact content loading from
// whatever stale host the sandbox key still held.
func (r *Reconciler) publishExternalURL(ctx context.Context, pe *v1.PublicEndpoint, o outcome) error {
	if !targetsWebd(pe.Spec.Target) {
		// Not an error and not a failure of this endpoint — its tunnel is fine
		// and status.url reports it. It simply does not get to name the address
		// every channel's external-base-url is seeded from. V(1) rather than
		// Info: a Pending endpoint re-checks every secretRequeue, and
		// failure-shaped noise on a loop is how real failures get tuned out.
		log.FromContext(ctx).V(1).Info(
			"endpoint does not target webd; leaving webd's external URL alone",
			"publicendpoint", pe.Name,
			"target", pe.Spec.Target.Namespace+"/"+pe.Spec.Target.Service)
		return nil
	}
	if o.externalURL == "" {
		return nil
	}
	cm := &corev1.ConfigMap{
		// TypeMeta is REQUIRED for a server-side apply: the payload is sent as
		// an apply patch, which the apiserver rejects without apiVersion+kind.
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: externalurl.Namespace,
			Name:      v1.WebdExternalURLConfigMap,
		},
		Data: map[string]string{
			v1.WebdTrustedURLKey: o.externalURL,
			v1.WebdSandboxURLKey: o.externalURL,
		},
	}
	// ForceOwnership so this controller takes the two keys from whichever
	// manager wrote them first — on an existing cluster that is `oap install`'s
	// localhost seed, which is precisely the stale value this controller exists
	// to replace. Only the two named keys are claimed; anything else in the
	// ConfigMap stays with its own owner.
	if err := r.Client.Patch(ctx, cm, client.Apply,
		client.FieldOwner(externalURLFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply ConfigMap %s/%s: %w",
			externalurl.Namespace, v1.WebdExternalURLConfigMap, err)
	}
	return nil
}

// evaluate resolves the provider, reads the credential, and brings the tunnel
// up — returning what to publish. It never writes status itself.
func (r *Reconciler) evaluate(ctx context.Context, pe *v1.PublicEndpoint) outcome {
	logger := log.FromContext(ctx).WithValues("publicendpoint", pe.Name)

	// Gate 0a: this cluster kind permits a project-managed tunnel at all.
	//
	// FIRST, before the provider or the credential, because this is the one
	// refusal that protects something already working: on a durable cluster
	// webd's external URL names a real https:// host that `oap install`
	// configured, and every other gate below is downstream of a tunnel we must
	// not open here in the first place. Task-ordering aside, an admission
	// webhook cannot be the only guard — it cannot see a CR that was created
	// before the policy existed, and this reconciler can.
	//
	// Dispatched through the policy's own predicate, not `!= Never`, so a
	// future policy value has to be decided at its definition rather than
	// silently falling into whichever branch a comparison happens to pick.
	if policy := r.ClusterKind.InstallProfile().PublicEndpointPolicy(); !policy.AllowsTunnel() {
		// Permanent for the life of this install — AP_CLUSTER_KIND is stamped
		// on the Deployment — so there is nothing to requeue for, and
		// externalURL stays empty so the ConfigMap is never touched.
		return outcome{
			phase:  v1.PublicEndpointPhaseFailed,
			reason: v1.ReasonPublicEndpointClusterKindRefusesTunnels,
			message: fmt.Sprintf(
				"cluster kind %q does not allow a project-managed tunnel (policy %s): this cluster reaches "+
					"the Internet through its own ingress, and webd's external URL is already set to a real "+
					"address. Delete this PublicEndpoint; configure external access with `oap install "+
					"--trusted-hostname` instead.",
				r.ClusterKind.Key(), policy),
		}
	}

	// Gate 0b: spec.localURL is present.
	//
	// The CRD marks it required, so this is reachable only for a CR created
	// against an earlier version of the schema. Refused rather than defaulted:
	// the whole reason the field exists is that no default can be right — webd
	// dispatches on an exact bare-host match, and both the host and the port
	// differ between the flows that create these CRs.
	if pe.Spec.LocalURL == "" {
		return outcome{
			phase:  v1.PublicEndpointPhaseFailed,
			reason: v1.ReasonPublicEndpointLocalURLMissing,
			message: "spec.localURL is empty: set the address the target is reachable at from the host " +
				"while no tunnel is up (scheme, host and port — e.g. \"http://localhost:8080\"). It is not " +
				"defaulted because a guessed host or port makes webd 404 every route.",
		}
	}

	// Gate 1: the provider resolves. Dispatched through the registry — this
	// file never compares spec.provider against a known name, so a new
	// provider is a registration and never an edit here.
	factory, err := registry.Get(pe.Spec.Provider)
	if err != nil {
		// Permanent until the spec is edited, and a spec edit re-triggers the
		// watch, so there is nothing to requeue for.
		// externalURL deliberately empty: a spec naming a provider this build
		// does not have says nothing about what webd's external address is, so
		// whatever the ConfigMap holds is left to its owner.
		return outcome{
			phase:   v1.PublicEndpointPhaseFailed,
			reason:  v1.ReasonPublicEndpointProviderUnknown,
			message: err.Error(),
		}
	}

	// Gate 2: spec.reservedDomain is refused while no provider honors it.
	//
	// Fail-closed on purpose. The field promises a stable hostname that never
	// drifts, but no registered provider wires it through today (ngrok's Go
	// agent SDK support for it is a separate follow-up — see that package's
	// init()). Accepting it and opening a per-session URL anyway would leave
	// the operator believing a promise the system is not keeping, and the
	// URL would silently change on every restart. A field accepted and
	// ignored is worse than one refused.
	if pe.Spec.ReservedDomain != "" {
		// externalURL deliberately empty, as for every other Failed outcome.
		return outcome{
			phase:  v1.PublicEndpointPhaseFailed,
			reason: v1.ReasonPublicEndpointReservedDomainUnsupported,
			message: fmt.Sprintf(
				"spec.reservedDomain (%q) is not supported yet: no registered tunnel provider honors it, "+
					"so the endpoint would silently get a provider-assigned URL that changes on every restart "+
					"instead of the pinned hostname. Clear the field to accept a provider-assigned URL.",
				pe.Spec.ReservedDomain),
		}
	}

	// Gate 3: the auth token resolves.
	token, pending, ok := r.authToken(ctx, pe)
	if !ok {
		return pending
	}

	// Gate 4: claim the right to open this CR's tunnel.
	state, url, stale := r.claim(pe.Name, pe.Generation)
	if stale != nil {
		// A superseded generation's tunnel, handed back by claim precisely so
		// it is stopped out here rather than under the map lock.
		if err := stopHeld(stale); err != nil {
			logger.Info("stopping the superseded tunnel failed; opening the replacement anyway", "err", err.Error())
		}
	}
	switch state {
	case claimOpen:
		return outcome{
			url:         url,
			externalURL: url,
			phase:       v1.PublicEndpointPhaseReady,
			reason:      v1.ReasonPublicEndpointTunnelOpen,
		}
	case claimBusy:
		// Another reconcile of this same CR is inside Start. Opening a second
		// one would take a second provider session; come back when it settles.
		//
		// externalURL deliberately empty even though this is Pending — the ONE
		// Pending state that must not write the loopback address. The reconcile
		// holding the reservation is about to learn the real URL; replacing a
		// live public address with a loopback one for the couple of seconds it
		// takes would flap webd's base URL out and back on every reopen.
		return outcome{
			result:  ctrl.Result{RequeueAfter: openingRequeue},
			phase:   v1.PublicEndpointPhasePending,
			reason:  v1.ReasonPublicEndpointTunnelOpening,
			message: "another reconcile is already opening this endpoint's tunnel",
		}
	}

	// claimOwned: this reconcile opens the tunnel, and every exit below must
	// either commit or release the reservation.
	failed := func(msg string) outcome {
		r.release(pe.Name)
		// externalURL deliberately empty: an open that just failed leaves the
		// ConfigMap on its last good value. A stale URL is bad; an empty one
		// breaks every redirect and deep link webd mints.
		return outcome{
			result:  ctrl.Result{RequeueAfter: startFailureRequeue},
			phase:   v1.PublicEndpointPhaseFailed,
			reason:  v1.ReasonPublicEndpointTunnelFailed,
			message: msg,
		}
	}

	// ReservedDomain is still passed through even though gate 2 has just
	// guaranteed it is empty: the plumbing is what the refusal above is
	// waiting on, so lifting the refusal is a one-line change here and
	// nowhere else.
	tunnel := factory(registry.Options{
		AuthToken:      token,
		ReservedDomain: pe.Spec.ReservedDomain,
	})
	// NOT a typed-nil guard, and deliberately not commented as one: factory
	// returns localtunnel.Tunnel, so a provider that returned a typed-nil
	// *Tunnel would box into a NON-nil interface, sail past this check and
	// panic at Start. Only a genuinely nil interface is caught here. The real
	// protection against the typed-nil shape lives at each provider's
	// registered factory, which constructs a fresh non-nil value (see
	// localtunnel/ngrok and localtunnel/stub).
	if tunnel == nil {
		return failed(fmt.Sprintf("provider %q built no tunnel", pe.Spec.Provider))
	}

	// The tunnel gets its OWN context, not this reconcile's — the package
	// comment's fourth rule. WithoutCancel keeps the reconcile's values (the
	// logger the provider may use) while severing its cancellation, and the
	// WithCancel on top is the handle stored beside the tunnel so stopping it
	// really ends it.
	tunnelCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Ownership of cancel passes to the map on a successful commit; until
	// then this reconcile owns it, and every failure exit must release it.
	// The flag (rather than a cancel() per return) is what keeps a future
	// early-return from silently leaking the context.
	committed := false
	defer func() {
		if !committed {
			cancel()
		}
	}()

	upstream := upstreamAddr(pe.Spec.Target)
	publicURL, err := tunnel.Start(tunnelCtx, upstream)
	if err != nil {
		logger.Info("opening the tunnel failed", "provider", pe.Spec.Provider, "upstream", upstream, "err", err.Error())
		return failed(fmt.Sprintf("open %s tunnel to %s: %v", pe.Spec.Provider, upstream, err))
	}
	if publicURL == "" {
		// A provider that reports success with no address has nothing to
		// publish; treat it as a failure rather than advertising Ready with an
		// empty status.url.
		if stopErr := tunnel.Stop(); stopErr != nil {
			logger.Info("stopping a tunnel that returned no URL failed", "err", stopErr.Error())
		}
		return failed(fmt.Sprintf("provider %q opened a tunnel but returned no public URL", pe.Spec.Provider))
	}

	if !r.commit(pe.Name, pe.Generation, publicURL, tunnel, cancel) {
		// The operator shut down while this tunnel was opening, so stopAll has
		// already drained the map and will never see it. Close it here or it
		// stays open, tracked by nothing, until the process exits.
		logger.Info("operator shut down while the tunnel was opening; closing it",
			"provider", pe.Spec.Provider, "url", publicURL)
		if stopErr := tunnel.Stop(); stopErr != nil {
			logger.Info("closing the late-opened tunnel failed", "err", stopErr.Error())
		}
		// committed stays false, so the deferred cancel ends its context too.
		return failed("operator is shutting down")
	}
	committed = true

	logger.Info("tunnel opened", "provider", pe.Spec.Provider, "upstream", upstream, "url", publicURL)
	return outcome{
		url:         publicURL,
		externalURL: publicURL,
		phase:       v1.PublicEndpointPhaseReady,
		reason:      v1.ReasonPublicEndpointTunnelOpen,
	}
}

// authToken resolves spec.authTokenRef to the provider credential. The second
// return is the outcome to publish when it does not resolve; ok reports which
// of the two to use. The token itself is never logged.
func (r *Reconciler) authToken(ctx context.Context, pe *v1.PublicEndpoint) (token string, pending outcome, ok bool) {
	ref := pe.Spec.AuthTokenRef
	secretRef := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	owner := types.NamespacedName{Name: pe.Name} // cluster-scoped: no namespace

	// A prerequisite that has not arrived yet is a state to report, not a
	// reconcile error to retry forever — Pending plus a timed re-check, so a
	// Secret created after the CR is picked up without intervention.
	notReady := func(msg string) outcome {
		// Nothing public exists and none is being opened, so webd's external
		// URL is the host-side address the CR's creator supplied. This is the
		// state `oap install` and `oap desktop` used to write for themselves,
		// folded into the single writer — with the address still coming from
		// them, because it is the one fact the cluster cannot derive.
		return outcome{
			result:      ctrl.Result{RequeueAfter: secretRequeue},
			externalURL: pe.Spec.LocalURL,
			phase:       v1.PublicEndpointPhasePending,
			reason:      v1.ReasonPublicEndpointAuthTokenMissing,
			message:     msg,
		}
	}

	// Adopt before reading: a metadata-only SSA that stamps AdoptedLabel, so
	// the guarded reader will serve it. The existence probe inside Adopt uses
	// the live reader, since the label-filtered cache holds only already-
	// adopted objects. A NotFound flows back here as "the Secret is missing".
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, owner, ownerKind); err != nil {
		return "", notReady(fmt.Sprintf("adopt Secret %s/%s: %v", ref.Namespace, ref.Name, err)), false
	}

	sec, err := r.SecretReader.Get(ctx, secretRef)
	if err != nil {
		return "", notReady(fmt.Sprintf("get Secret %s/%s: %v", ref.Namespace, ref.Name, err)), false
	}
	val, present := sec.Data[ref.Key]
	if !present || len(val) == 0 {
		return "", notReady(fmt.Sprintf("Secret %s/%s missing or empty key %q", ref.Namespace, ref.Name, ref.Key)), false
	}
	return string(val), outcome{}, true
}

// finalize stops the CR's tunnel and releases the object.
func (r *Reconciler) finalize(ctx context.Context, pe *v1.PublicEndpoint) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("publicendpoint", pe.Name)

	held, busy := r.take(pe.Name)
	if busy {
		// A concurrent reconcile is inside Start for this CR. Removing the
		// finalizer now would let the object vanish while a session is still
		// being opened for it — the exact leak the finalizer exists to prevent.
		logger.Info("PublicEndpoint deleted while its tunnel is still opening; deferring finalizer removal")
		return ctrl.Result{RequeueAfter: openingRequeue}, nil
	}
	if held != nil {
		if err := stopHeld(held); err != nil {
			// Log and still release the object: a provider that cannot be
			// stopped must not make the CR permanently undeletable.
			logger.Info("stopping the tunnel on delete failed; releasing the object anyway", "err", err.Error())
		} else {
			logger.Info("tunnel closed on delete")
		}
	}

	if !controllerutil.ContainsFinalizer(pe, v1.FinalizerPublicEndpoint) {
		return ctrl.Result{}, nil
	}

	// Give webd's external URL back BEFORE the object is released, because
	// after that nothing in this cluster remembers the value was ours.
	//
	// Stopping the tunnel and leaving the address published is this feature's
	// own root-cause bug on the delete path: webd would go on advertising —
	// and every credential link, OAuth redirect_uri and artifact deep-link go
	// on carrying — a public URL that forwards nothing. On `desktop` the
	// host-side writer takes the ConfigMap back at the next boot; on `local`
	// nothing ever does.
	if err := r.releaseExternalURL(ctx, pe); err != nil {
		// Retried with BACKOFF, and bounded. Returning the error rather than a
		// RequeueAfter is deliberate: controller-runtime ignores a result
		// whenever the error is non-nil, and this way the failure reaches its
		// error log too rather than only ours.
		if waited := r.now().Sub(pe.DeletionTimestamp.Time); waited < externalURLReleaseGrace {
			return ctrl.Result{}, fmt.Errorf("release webd's external URL for deleted PublicEndpoint %s: %w", pe.Name, err)
		}
		// Past the bound the object is let go anyway. A CR that can never be
		// deleted is worse than a stale ConfigMap value — it wedges every
		// namespace teardown and every `kubectl delete` behind a failure the
		// operator cannot clear — so the choice is made loudly, naming the
		// object that now needs correcting by hand, rather than by retrying
		// forever in silence.
		logger.Info("could not give webd's external URL back within the grace period; releasing the PublicEndpoint anyway",
			"grace", externalURLReleaseGrace,
			"configmap", externalurl.Namespace+"/"+v1.WebdExternalURLConfigMap,
			"consequence", "webd keeps advertising this tunnel's address, which now forwards nothing, until those keys are corrected",
			"err", err.Error())
	}

	controllerutil.RemoveFinalizer(pe, v1.FinalizerPublicEndpoint)
	if err := r.Client.Update(ctx, pe); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from PublicEndpoint %s: %w", pe.Name, err)
	}
	return ctrl.Result{}, nil
}

// releaseExternalURL gives the two webd external-URL keys back, so a deleted
// endpoint leaves the address absent rather than pointing at a tunnel that no
// longer exists.
//
// It is an APPLY THAT NAMES NO KEY, not a write of empty strings. Server-side
// apply removes whatever the field manager owned and no longer sets, and only
// what no other manager claims — so the keys revert to absent, any key some
// other writer owns is untouched, and an endpoint that never wrote (a cluster
// kind whose policy refuses tunnels) relinquishes nothing because it owned
// nothing.
//
// AN ABSENT ConfigMap IS CREATED EMPTY by the apply, which is the one thing
// this cannot avoid: the operator's ConfigMap informer is label-filtered to
// adopted objects, so there is no read to condition on. The result is
// indistinguishable from the normal one — a ConfigMap carrying neither key,
// which every reader already treats as "no external URL published".
//
// Only an endpoint that TARGETS WEBD gives anything back, for the same reason
// only such an endpoint writes: see publishExternalURL.
func (r *Reconciler) releaseExternalURL(ctx context.Context, pe *v1.PublicEndpoint) error {
	if !targetsWebd(pe.Spec.Target) {
		return nil
	}
	cm := &corev1.ConfigMap{
		// TypeMeta is REQUIRED for a server-side apply — see publishExternalURL.
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: externalurl.Namespace,
			Name:      v1.WebdExternalURLConfigMap,
		},
		// No Data: that IS the release.
	}
	if err := r.Client.Patch(ctx, cm, client.Apply, client.FieldOwner(externalURLFieldOwner)); err != nil {
		return fmt.Errorf("apply ConfigMap %s/%s: %w",
			externalurl.Namespace, v1.WebdExternalURLConfigMap, err)
	}
	return nil
}

// -----------------------------------------------------------------------
// The tunnel map. Every function here takes the lock briefly and returns any
// provider call for its CALLER to make, after the lock is released.
// -----------------------------------------------------------------------

type claimState int

const (
	// claimOwned: the caller reserved the slot and must open the tunnel.
	claimOwned claimState = iota
	// claimOpen: a tunnel is already open at this generation; reuse its URL.
	claimOpen
	// claimBusy: another reconcile is inside Start for this CR.
	claimBusy
)

// claim reserves the right to open name's tunnel at generation gen.
//
// stale is a superseded generation's tunnel that the CALLER must Stop — handed
// back rather than stopped here precisely so the map lock is not held across
// that call.
func (r *Reconciler) claim(name string, gen int64) (state claimState, url string, stale *heldTunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tunnels == nil {
		r.tunnels = map[string]*heldTunnel{}
	}
	switch held := r.tunnels[name]; {
	case held == nil:
		// Nothing held: reserve below.
	case held.opening:
		return claimBusy, "", nil
	case held.generation == gen && held.tunnel != nil:
		return claimOpen, held.url, nil
	default:
		stale = held
	}
	// The reservation. This line is the whole reason "never hold the lock
	// across Start" and "never open two tunnels" are compatible: it publishes
	// the intent to open UNDER the lock, so a concurrent reconcile of this CR
	// sees claimBusy instead of an empty slot while Start runs unlocked.
	// TestReconcile_AConcurrentReconcileWaitsInsteadOfOpeningASecondTunnel is
	// what keeps it from being deleted as apparently-dead code.
	r.tunnels[name] = &heldTunnel{generation: gen, opening: true}
	return claimOwned, "", stale
}

// commit records a successfully opened tunnel against the reservation claim
// made. It reports false when stopAll has already run: the tunnel arrived
// after shutdown drained the map, so nothing would ever stop it and the caller
// must do so itself.
func (r *Reconciler) commit(name string, gen int64, url string, tunnel localtunnel.Tunnel, cancel context.CancelFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	// Lazy-init as claim does. stopAll leaves an empty map rather than a nil
	// one, but an assignment into a nil map panics, and this is exactly the
	// path a shutdown racing an open takes.
	if r.tunnels == nil {
		r.tunnels = map[string]*heldTunnel{}
	}
	r.tunnels[name] = &heldTunnel{generation: gen, url: url, tunnel: tunnel, cancel: cancel}
	return true
}

// release drops a reservation whose open failed, so the next reconcile is free
// to retry rather than seeing a permanent claimBusy.
func (r *Reconciler) release(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if held := r.tunnels[name]; held != nil && held.opening {
		delete(r.tunnels, name)
	}
}

// take removes name's entry and returns it for the caller to stopHeld. busy
// reports that a reconcile is mid-Start: nothing is open yet, and the
// reservation is not the caller's to take.
func (r *Reconciler) take(name string) (held *heldTunnel, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held = r.tunnels[name]
	if held == nil {
		return nil, false
	}
	if held.opening {
		return nil, true
	}
	delete(r.tunnels, name)
	return held, false
}

// closeTunnel stops and forgets name's tunnel.
//
// The three results are three DISTINCT states, not two: stopped (there was a
// tunnel and it is now closed), busy (a concurrent reconcile is mid-open and
// owns the entry), and err (a tunnel existed and the provider refused to close
// it). busy is a normal state on a concurrent open, so it is reported as
// itself rather than folded into err — a caller that logged it as a failure
// would emit failure-shaped noise on a loop for an entirely healthy path.
func (r *Reconciler) closeTunnel(name string) (stopped, busy bool, err error) {
	held, busy := r.take(name)
	if busy {
		return false, true, nil
	}
	if held == nil {
		return false, false, nil
	}
	return true, false, stopHeld(held)
}

// stopAll closes every tunnel this process holds. Called on manager shutdown
// so a graceful stop does not leave provider sessions dangling.
func (r *Reconciler) stopAll(ctx context.Context) {
	logger := log.FromContext(ctx)

	r.mu.Lock()
	held := r.tunnels
	// Leave an EMPTY map, not a nil one, and mark the reconciler stopped. An
	// open still inside Start will return after this and try to commit; nil
	// would panic it, and silently accepting it would leak a live session into
	// a map nothing drains again. commit refuses on stopped and the opener
	// closes what it opened.
	r.tunnels = map[string]*heldTunnel{}
	r.stopped = true
	r.mu.Unlock()

	for name, entry := range held {
		if entry.opening {
			// A reservation, not a tunnel: nothing is open yet. The reconcile
			// inside Start owns it and will close it when commit refuses.
			continue
		}
		if err := stopHeld(entry); err != nil {
			logger.Info("stopping a tunnel during shutdown failed", "publicendpoint", name, "err", err.Error())
		}
	}
}

// targetsWebd reports whether this endpoint's target is webd's own Service —
// the ONLY target whose public address may be published as webd's external URL.
//
// Delegated to cloud.IsWebdTarget rather than compared here, because the same
// question decides whether a host-side writer of that ConfigMap must stand
// down (cmd/oap/internal/desktopcmd). Two copies of the predicate would let
// this reconciler and that writer disagree about which endpoint owns the value
// — which is precisely the two-writer failure this controller exists to end.
func targetsWebd(t v1.PublicEndpointTarget) bool {
	return cloud.IsWebdTarget(t.Namespace, t.Service)
}

// upstreamAddr renders the in-cluster address the tunnel forwards to. The
// tunnel runs inside the operator process, so the Service's cluster DNS name
// is directly reachable — there is no port-forward in the path.
func upstreamAddr(t v1.PublicEndpointTarget) string {
	host := t.Service + "." + t.Namespace + ".svc.cluster.local"
	return "http://" + net.JoinHostPort(host, strconv.Itoa(int(t.Port)))
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	// SecretReader is injected by main.go (operator-wide, flag-controlled guard
	// mode); tests inject their own. No self-construct here.

	// Close every held tunnel when the manager shuts down.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		// The manager's context is already cancelled here; stopAll uses it for
		// logging only, and Tunnel.Stop takes none.
		r.stopAll(ctx)
		return nil
	})); err != nil {
		return fmt.Errorf("register PublicEndpoint tunnel shutdown hook: %w", err)
	}

	// No Secret watch: a PublicEndpoint's authTokenRef may name a Secret in any
	// namespace, and watching every Secret cluster-wide costs far more informer
	// overhead than it saves. Every unresolvable-credential path requeues after
	// secretRequeue instead, so a Secret created after the CR is picked up
	// within 30 seconds — the same trade ClusterIdentityProvider makes.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.PublicEndpoint{}).
		Complete(r)
}
