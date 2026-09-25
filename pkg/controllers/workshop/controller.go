package workshop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/inboxwake"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// FieldOwner is the SSA field manager for every object this
// controller server-side-applies (the deny-all NetworkPolicy, the
// ResourceQuota, and the workshop-namespace/workshops-CR RBAC). A dedicated
// owner, distinct from every other controller's, so a second controller can
// never silently steal a field this one applies.
const FieldOwner = "workshop-controller"

// workshopSecretTokenKey is the bearer Secret's data key.
const workshopSecretTokenKey = "token"

// WorkshopTuples is the layer-1.3 half of provisioning: the SpiceDB tuples
// binding a workshop to the one session allowed to act as it and to the people
// allowed to close it, and their reversal at teardown. Satisfied by
// *spicedb.Client (pkg/authz/spicedb); see EnsureWorkshopSubjects /
// DeleteWorkshopRelationships there for the exact tuple shapes. A nil Tuples
// must never let provisioning reach Ready — see the TupleWritten step in
// Reconcile.
type WorkshopTuples interface {
	// EnsureWorkshopSubjects TOUCHes the workshop's #session, #starter and
	// #platform tuples in one request — idempotent, safe to call every
	// reconcile. starter may be the zero value (spec.starterCanonical is
	// optional), in which case no #starter tuple is written and the workshop
	// is closable by a platform admin alone.
	EnsureWorkshopSubjects(ctx context.Context, workshopID, sessNS, sessName string, starter identity.CanonicalUserID) error
	// CheckWorkshopClose answers workshop:<workshopID>#close for the PERSON
	// user:<canonical> — the starter of the workshop whose builder asked, so
	// a close request is decided as the human behind it and never as the
	// session. Consumed by the close pass (close.go). An error is never a
	// refusal: the pass retries rather than recording one.
	CheckWorkshopClose(ctx context.Context, workshopID string, canonical identity.CanonicalUserID) (bool, error)
	// DeleteWorkshopRelationships removes every relation on the workshop
	// object. Consumed by teardown (teardown.go); declared on the interface
	// alongside EnsureWorkshopSubjects so both halves of the tuples' lifecycle
	// are visible at the seam.
	DeleteWorkshopRelationships(ctx context.Context, workshopID string) error
}

// Reconciler implements the Workshop controller: lockdown layers 1.2 (the
// closed RBAC kind set) and 1.3 (the SpiceDB tuple).
//
// Tuples and Tokens are declared as interface/pointer fields assigned only
// from a real, non-nil value in production (internal/cmd/operator/main.go) —
// see CLAUDE.md's typed-nil rule. Both are nil-checked at their use site
// rather than trusted, so a misconfigured test fixture or an operator
// started without SpiceDB fails a reconcile loudly (TupleWritten=False /
// TokensReady=False) instead of panicking or silently issuing a workshop
// with no enforceable boundary.
type Reconciler struct {
	Client client.Client
	// APIReader bypasses the informer cache for the namespace-collision
	// check — the one read in this reconciler where a stale cache entry
	// would matter: it decides whether an existing namespace of the derived
	// name is this workshop's or a conflict. Falls back to Client when nil
	// (unit-test convenience, mirroring pkg/controllers/toolcall's APIReader).
	APIReader client.Reader
	// Tuples writes and (via teardown, teardown.go) removes the layer-1.3
	// tuple. nil fails every reconcile closed at the TupleWritten step.
	Tuples WorkshopTuples
	// Tokens is the operator's in-process bearer-token registry. nil fails
	// every reconcile closed at the TokensReady step, after the Secret is
	// safely created (a Secret with no live registration is inert, not
	// unsafe — see the Bearer step's doc).
	Tokens *tokens.Registry
	// ExpiredNoticePublish delivers the sweeper's (sweepExpiry, teardown.go)
	// expiry notice to the requester over the same out.metaagent_notice
	// channel every other operator-originated session notice uses. nil (NATS
	// unreachable at startup) degrades to a log line — the expiry and cleanup
	// still happen.
	ExpiredNoticePublish func(ctx context.Context, ns, name, requesterCanonical, body string) error
	// Now is the time source for ProvisionedAt and the max-age sweeper
	// (sweepExpiry, teardown.go). Nil defaults to time.Now(); tests inject a
	// fixed clock.
	Now func() time.Time
	// BrowserServiceAccount / BrowserServiceAccountNamespace name webd's
	// ServiceAccount, so each workshop namespace can grant it the session
	// writes a person's own test needs (BuildWorkshopBrowserRBAC). Empty
	// means no browser may start sessions in workshops; the reconcile says
	// so once per provisioning and grants nothing.
	BrowserServiceAccount          string
	BrowserServiceAccountNamespace string
	// ParentMemory and PublishInteraction are what inboxwake.Notify needs to
	// tell the builder session about its test (spec.testWatch, testwatch.go):
	// the operator's signing memory facade and its NATS publisher, the same
	// two values the SubagentRequest controller uses to wake an attended
	// child's parent. Nil is tolerated and logged by inboxwake: the watch
	// still records what it observed, the builder simply is not woken.
	ParentMemory       memory.Memory
	PublishInteraction inboxwake.Publish
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshops/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=create;get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=create;get;list;watch;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=create;get;list;watch;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;get;list;watch;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=create;get;list;watch;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;get;list;watch;delete
//
// secrets:delete is this controller's OWN grant-only widening, distinct from
// the kind-set widening below: BuildWorkshopBrowserRBAC hands the browser SA
// delete on Secrets (the creds Secret a person's own test session writes),
// and nothing in this reconciler itself ever deletes a Secret — the bearer
// token Secret this package writes is set-once and outlives the workshop
// (teardown.go never deletes it, only the namespace/tuple/CRBs). Held so the
// RoleBinding can grant it without RBAC escalation refusing the apply.
//
// agentsessions:delete is used DIRECTLY, and it is the one verb here that
// ends something outside this workshop: the close pass (close.go) deletes the
// TARGET workshop's builder session once SpiceDB's workshop#close allows it,
// and that session's owner reference plus the Workshop finalizer tear the
// target down. Every call is gated by that check — never by this grant, which
// is cluster-wide and so proves nothing about who asked.
//
// WIDENING (deliberate, spec §1.2 — Kubernetes escalation prevention): a
// RoleBinding/Role can only grant a verb the GRANTING identity itself
// already holds, so before this controller can hand the workshop SA CRUD on
// the closed kind set, the operator's OWN ClusterRole must hold CRUD on
// every one of those kinds — plus spiceboxtoolspecs/spiceboxtoolkits, which
// the static spicebox-workshop-toolwriter ClusterRole
// (config/manager/workshop-toolwriter.yaml) grants instead of a Role this
// controller builds, but which the operator must still HOLD to bind. This is
// a real, additive widening of what the operator process itself can do to
// every namespace in the cluster, accepted because the alternative —
// granting a verb the operator cannot prove it holds — is what Kubernetes'
// own RBAC escalation check would refuse at apply time anyway. The RBAC
// sufficiency envtest (rbac_sufficiency_integration_test.go) asserts the
// workshop SA gets EXACTLY these kinds and the runner SA gets NONE of them.
//
// The other cluster-scoped CRB this controller creates — readerCRB, binding
// the static spicebox-workshop-reader ClusterRole
// (config/manager/workshop-reader.yaml) — needs NO additional marker here:
// the operator already independently holds get;list;watch on
// clusteragentsettings (pkg/controllers/settings), clusterskills
// (pkg/controllers/clusterskill), and spiceboxclasses
// (pkg/controllers/spiceboxclass) via those controllers' own markers, a
// strict superset of the get;list the reader ClusterRole grants — so the
// escalation check already passes without this controller claiming any of
// the three itself.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses;mcpservers;sidecartoolboxes;agentidentities;skills;agentuis;subagentrequests;spiceboxtoolspecs;spiceboxtoolkits,verbs=create;get;list;watch;update;patch;delete

// reader returns the uncached APIReader when wired, else the cached Client.
// Reads that must see writes the operator's own cache cannot are routed
// through it: the namespace-collision anchor; the workshop namespace's own
// sessions and the builder session's phase, which the test watch reads to
// decide what to deliver and whether the builder is still there to read it;
// and the bearer Secret — which the operator's Secret informer is
// label-filtered to EXCLUDE (internal/cmd/operator/main.go ByObject), so a
// cached Get of it structurally always misses.
func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Reconcile provisions one Workshop's isolated namespace, deny-all
// NetworkPolicy, ResourceQuota, exactly-scoped RBAC, SpiceDB tuple and
// bearer token — each layer stamping its own condition, and any failure
// short of the last step returning an error (never Ready) so the next
// reconcile retries the layer that failed rather than the whole workshop
// silently sitting half-provisioned.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws spiceboxv1alpha1.Workshop
	if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !ws.DeletionTimestamp.IsZero() {
		// teardown (teardown.go) reverses every layer below — standing first —
		// before releasing the finalizer, so deletion never silently releases
		// standing this package cannot prove reversed: the tuple, the bearer
		// registration and the toolwriter ClusterRoleBinding all outlive a
		// namespace delete alone.
		return r.teardown(ctx, &ws)
	}

	// The max-age sweeper: a Ready workshop past spec.limits.maxAge is expired
	// and deleted here, before any provisioning logic runs — sweepExpiry
	// (teardown.go) only acts on Phase==Ready, so this can never race a
	// workshop still mid-provision.
	if handled, res, err := r.sweepExpiry(ctx, &ws); handled {
		return res, err
	}

	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &ws, spiceboxv1alpha1.FinalizerWorkshop); added || err != nil {
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to Workshop %s/%s: %w", ws.Namespace, ws.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 1. The session this workshop belongs to. A ghost Workshop (its session
	// already gone) is left alone: owner-ref GC will delete this CR, and
	// provisioning for a session that no longer exists would only recreate
	// what GC is about to remove.
	var sess spiceboxv1alpha1.AgentSession
	sessKey := types.NamespacedName{Namespace: ws.Spec.Session.Namespace, Name: ws.Spec.Session.Name}
	if err := r.Client.Get(ctx, sessKey, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get session %s for Workshop %s/%s: %w", sessKey, ws.Namespace, ws.Name, err)
	}

	// 1b. Release on finish (close.go), HERE: it needs the session's phase, so
	// it cannot run beside sweepExpiry above, and it must run before anything
	// below provisions — a workshop whose builder session is over must never
	// re-create the namespace, RBAC, tuples and token the teardown it is about
	// to trigger will remove.
	if handled, res, err := r.releaseIfFinished(ctx, &ws, &sess); handled {
		return res, err
	}

	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)

	// 2. Resolve the workshop namespace FIRST — a present-but-mismatched
	// namespace is a name COLLISION, and it must be refused BEFORE
	// status.Namespace is anchored. Present-but-unlabeled (or labeled for a
	// different session) is never an adoption — that namespace belongs to
	// someone else, and relabeling it would silently hand this workshop's RBAC
	// and NetworkPolicy into a namespace it does not own. Refusing before the
	// anchor is load-bearing for teardown: teardown deletes status.Namespace BY
	// NAME (teardown.go), so a collision that anchored the foreign name would
	// let teardown destroy a namespace this workshop does not own. On the
	// collision branch we never anchor and never create; on the not-found
	// branch we anchor (step 3) THEN create (step 4) — so the anchor still
	// precedes any external write.
	reader := r.reader()
	var ns corev1.Namespace
	nsErr := reader.Get(ctx, types.NamespacedName{Name: nsName}, &ns)
	switch {
	case nsErr == nil:
		if ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace] != sess.Namespace ||
			ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName] != sess.Name {
			return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
				fmt.Errorf("namespace %s already exists and is not labeled for this workshop's session %s/%s — refusing to adopt another namespace",
					nsName, sess.Namespace, sess.Name))
		}
		// Ours already (idempotent re-entry) — fall through to anchor + proceed.
	case apierrors.IsNotFound(nsErr):
		// Does not exist yet — safe to anchor (step 3) then create (step 4).
	default:
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
			fmt.Errorf("get workshop namespace %s: %w", nsName, nsErr))
	}

	// 3. DURABLE ANCHOR: persist status.Namespace (+ Phase=Provisioning)
	// BEFORE any external durable state — the namespace, the RBAC objects,
	// the SpiceDB tuple, the bearer Secret — exists. A collision has just been
	// ruled out (step 2), so this only ever names a namespace that is ours or
	// that we are about to create. Set-once (only writes when status.Namespace
	// is still empty), so this converges exactly like every other status write
	// here: a workshop already past this point never re-enters the branch.
	//
	// This is the fix for a real orphan: teardown (teardown.go) reads
	// status.Namespace ALONE, with no session fallback, because on the
	// ordinary owner-ref GC cascade the session is reaped before the
	// Workshop it owns — a session Get from teardown would 404. Before this
	// anchor existed, a crash/restart or a nil-Tokens failure landing between
	// the tuple write (step 7 below) and the single end-of-provisioning
	// status persist (step 9) left status.Namespace empty forever for a
	// tuple that WAS written — teardown would then see "nothing to reverse"
	// and release the finalizer over a live tuple and a live namespace, with
	// no log. Persisting the anchor here, before the tuple can ever be
	// written, closes that gap: by the time anything external exists,
	// status.Namespace already durably names it.
	if ws.Status.Namespace == "" {
		ws.Status.Namespace = nsName
		ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
		ws.Status.ObservedGeneration = ws.Generation
		if err := r.Client.Status().Update(ctx, &ws); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist Workshop %s/%s namespace anchor: %w", ws.Namespace, ws.Name, err)
		}
	}

	// 4. Create the workshop namespace if step 2's Get said NotFound.
	if apierrors.IsNotFound(nsErr) {
		want := BuildWorkshopNamespace(&ws, &sess)
		if err := r.Client.Create(ctx, want); err != nil {
			return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
				fmt.Errorf("create workshop namespace %s: %w", nsName, err))
		}
	}

	// 5. Deny-all NetworkPolicy + ResourceQuota + LimitRange — pure functions of
	// nsName, so a byte-identical re-apply is an SSA no-op.
	if err := r.Client.Patch(ctx, BuildWorkshopDenyAll(nsName),
		client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner),
	); err != nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
			fmt.Errorf("apply deny-all NetworkPolicy in %s: %w", nsName, err))
	}
	if err := r.Client.Patch(ctx, BuildWorkshopQuota(nsName),
		client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner),
	); err != nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
			fmt.Errorf("apply ResourceQuota in %s: %w", nsName, err))
	}
	// The quota above makes requests+limits mandatory on every container here;
	// this supplies them, so a pod created by something that has nowhere to put
	// them — the session reconciler's runner, a probe — is admitted rather than
	// refused. See BuildWorkshopLimitRange.
	if err := r.Client.Patch(ctx, BuildWorkshopLimitRange(nsName),
		client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner),
	); err != nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionNamespaceReady,
			fmt.Errorf("apply LimitRange in %s: %w", nsName, err))
	}
	conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionNamespaceReady, spiceboxv1alpha1.ReasonWorkshopProvisioned)

	// 6. RBAC: the closed kind set (layer 1.2). SA/Role/RoleBindings are
	// SSA'd (pure functions of ws+sess); the cluster-scoped toolwriter CRB is
	// created explicitly and reaped by its deterministic name in teardown —
	// it cannot carry a namespaced owner ref.
	sa, role, wsBinding, crRole, crBinding, toolCRB, readerCRB := BuildWorkshopRBAC(&ws, &sess)
	for _, obj := range []client.Object{sa, role, wsBinding, crRole, crBinding} {
		if err := r.Client.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
			return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionRBACReady,
				fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err))
		}
	}

	// The browser (webd) needs its own start-a-session grant in the workshop
	// namespace — a person testing their build creates a root session there,
	// the same three writes webd's static start namespaces already grant
	// (config/webd/role-builder.yaml). Nothing is wired when either half of
	// BrowserServiceAccount(Namespace) is empty: an operator that never told
	// this reconciler who the browser is must not silently grant an empty
	// RoleBinding subject, so this logs and skips rather than applying one.
	if r.BrowserServiceAccount == "" || r.BrowserServiceAccountNamespace == "" {
		log.FromContext(ctx).Info("no browser service account wired; a person cannot start their own test session in this workshop",
			"workshop", ws.Namespace+"/"+ws.Name)
	} else {
		bRole, bBinding := BuildWorkshopBrowserRBAC(&ws, &sess, r.BrowserServiceAccount, r.BrowserServiceAccountNamespace)
		for _, obj := range []client.Object{bRole, bBinding} {
			if err := r.Client.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
				return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionRBACReady,
					fmt.Errorf("apply browser %T %s: %w", obj, obj.GetName(), err))
			}
		}
	}

	if err := r.Client.Create(ctx, toolCRB); err != nil && !apierrors.IsAlreadyExists(err) {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionRBACReady,
			fmt.Errorf("create toolwriter ClusterRoleBinding %s: %w", toolCRB.Name, err))
	}
	if err := r.Client.Create(ctx, readerCRB); err != nil && !apierrors.IsAlreadyExists(err) {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionRBACReady,
			fmt.Errorf("create reader ClusterRoleBinding %s: %w", readerCRB.Name, err))
	}
	conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionRBACReady, spiceboxv1alpha1.ReasonWorkshopProvisioned)

	// 7. Tuples (layer 1.3). nil Tuples fails closed: without a way to write
	// (and later prove reversed) the standing tuples, this workshop must never
	// reach Ready.
	if r.Tuples == nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTupleWritten,
			fmt.Errorf("Reconciler.Tuples is not wired"))
	}
	// spec.starterCanonical is the bare canonical the AgentSession reconciler
	// recorded at creation; reading it back is not a fresh verification, which
	// is what CanonicalFromTrusted names. It is +optional — an empty value
	// yields the zero id, which the write below turns into "no #starter tuple"
	// rather than a subject nobody is.
	starter := identity.CanonicalFromTrusted(ws.Spec.StarterCanonical,
		"operator-written spec.starterCanonical on this Workshop")
	if err := r.Tuples.EnsureWorkshopSubjects(ctx, nsName, ws.Namespace, sess.Name, starter); err != nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTupleWritten,
			fmt.Errorf("ensure workshop tuples for %s: %w", nsName, err))
	}
	conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten, spiceboxv1alpha1.ReasonWorkshopProvisioned)

	// 7b. Close requests (close.go): decide every spec.closeRequests entry
	// this pass has not already answered. It runs HERE, once this workshop's
	// own tuples are standing, so the workshop doing the asking is fully
	// provisioned before it may end anyone else's — and its decisions ride
	// the single status write at step 9 below rather than opening a second
	// conflict window on the same object.
	closeRes, closeChanged, closeErr := r.fulfilCloseRequests(ctx, &ws)
	if closeErr != nil {
		// A decision already made in this pass names a target whose session
		// is ALREADY deleted. Persist it before returning, or the retry
		// re-decides it and could reach a later session of the same name.
		if closeChanged {
			if perr := r.Client.Status().Update(ctx, &ws); perr != nil {
				log.FromContext(ctx).Info("workshop: persisting a close decision before returning its error failed; the undecided targets are retried",
					"workshop", ws.Namespace+"/"+ws.Name, "persistErr", perr.Error(), "cause", closeErr.Error())
			}
		}
		return ctrl.Result{}, closeErr
	}

	// 8. Bearer token. The Secret is set-once (never rewritten once it
	// exists) and carries the SESSION's owner ref — the same cascade
	// convention as every other per-session Secret (BuildRunnerRBAC). The
	// registry check comes AFTER the Secret exists: a nil Tokens registry
	// leaves a durable, unregistered Secret behind (harmless — nothing
	// serves it until a registry re-registers it, the same restart-rehydrate
	// shape as reregisterMemoryToken), rather than losing the mint because
	// registration failed.
	secretName := spiceboxv1alpha1.WorkshopTokenSecretName(sess.Name)
	secretKey := types.NamespacedName{Namespace: ws.Namespace, Name: secretName}
	var sec corev1.Secret
	// Read through the uncached reader: the operator's Secret informer is
	// label-filtered (main.go ByObject), so this Secret — which carries no
	// such label — is never cached and a cached Get always reports NotFound,
	// which wedged provisioning at "already exists" on every reconcile after
	// the first.
	if err := r.reader().Get(ctx, secretKey, &sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTokensReady,
				fmt.Errorf("get bearer Secret %s: %w", secretKey, err))
		}
		tok, genErr := generateWorkshopToken()
		if genErr != nil {
			return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTokensReady,
				fmt.Errorf("generate bearer token: %w", genErr))
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            secretName,
				Namespace:       ws.Namespace,
				OwnerReferences: cosidecar.OwnerRef(&sess),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{workshopSecretTokenKey: []byte(tok)},
		}
		if err := r.Client.Create(ctx, &sec); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTokensReady,
					fmt.Errorf("create bearer Secret %s: %w", secretKey, err))
			}
			// Idempotent recovery: the Secret exists (a prior pass created it,
			// or a concurrent reconcile won the race) but the cached Get could
			// not see it. Read the REAL one back through the uncached reader —
			// the freshly-generated token is discarded, the durable one wins,
			// so the registry below serves the value the sidecar actually
			// mounts.
			if getErr := r.reader().Get(ctx, secretKey, &sec); getErr != nil {
				return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTokensReady,
					fmt.Errorf("bearer Secret %s exists but could not be read back: %w", secretKey, getErr))
			}
		}
	}

	if r.Tokens == nil {
		return r.failLayer(ctx, &ws, spiceboxv1alpha1.WorkshopConditionTokensReady,
			fmt.Errorf("Reconciler.Tokens is not wired"))
	}
	tokenKey := memory.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}
	if !r.Tokens.Registered(tokenKey) {
		r.Tokens.Set(tokenKey, string(sec.Data[workshopSecretTokenKey]), "" /* no human caller */)
	}
	conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTokensReady, spiceboxv1alpha1.ReasonWorkshopProvisioned)

	// 9. Every layer is up: stamp Ready. ProvisionedAt is set-once — an
	// applied/observed timestamp restamped on every reconcile would defeat
	// both SSA idempotency and the max-age sweeper (sweepExpiry, teardown.go),
	// which measures from the FIRST successful provision, not the most recent
	// reconcile.
	ws.Status.Namespace = nsName
	ws.Status.SidecarIdentity = &spiceboxv1alpha1.WorkshopSidecarIdentity{
		ServiceAccount: spiceboxv1alpha1.WorkshopServiceAccountName(sess.Name),
		TokenSecret:    secretName,
	}
	if ws.Status.ProvisionedAt == nil {
		now := metav1.NewTime(r.now())
		ws.Status.ProvisionedAt = &now
	}
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	ws.Status.ObservedGeneration = ws.Generation

	// The test watch (testwatch.go) runs HERE, on the last line of the Ready
	// path and before the one status write below, for three reasons. Reconcile
	// has no already-Ready short-circuit — the whole provisioning path is
	// idempotent and re-runs every time — so this is reached on every
	// reconcile of a Ready workshop, which is what a watch needs. It runs
	// after the namespace is provisioned and anchored, because
	// status.Namespace is where it looks for the person's test. And folding
	// its status mutations into the write that was already going to happen
	// keeps this one write per reconcile, rather than opening a second
	// conflict window on the same object.
	watchRes, watchChanged, watchErr := r.reconcileTestWatch(ctx, &ws)
	if watchErr != nil {
		// watchChanged means the watch mutated status.testWatch — a fresh
		// record for a replaced watch, an observed phase, or an event it just
		// delivered. Persist it before returning the error, because of that
		// last case: a line that reached the builder but was never written
		// down is delivered again on the retry, and the builder cannot tell a
		// repeat from a second pause.
		if watchChanged {
			if perr := r.Client.Status().Update(ctx, &ws); perr != nil {
				log.FromContext(ctx).Info("workshop: persisting the test watch before returning its error failed; a delivered line may repeat on the retry",
					"workshop", ws.Namespace+"/"+ws.Name, "persistErr", perr.Error(), "cause", watchErr.Error())
			}
		}
		return ctrl.Result{}, watchErr
	}

	// Accepted hazard: if this Update loses a conflict (a concurrent status
	// writer — e.g. the sweeper, or another reconcile of this same key — landed
	// first), the retry re-runs reconcileTestWatch above and can re-deliver a
	// line it already delivered on the losing attempt. At-most-once delivery is
	// not guaranteed here, on purpose: the alternative would be a whole-object
	// retry-on-conflict that re-applies every OTHER writer's fields too, not
	// just testWatch's, to avoid clobbering them — not worth it for a rare,
	// cosmetic duplicate wake line.
	//
	// A close decision made at step 7b rides this same write, so a lost
	// conflict re-decides that target too. The re-decision reads the target
	// Workshop again — by then deleted or terminating — so it records
	// NotFound or re-issues a delete the apiserver answers NotFound. Only a
	// session recreated under the target's exact name inside that window
	// would be reached, which is why the decision is written here rather than
	// left for a later pass to make from scratch.
	if err := r.Client.Status().Update(ctx, &ws); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist Workshop %s/%s status: %w", ws.Namespace, ws.Name, err)
	}

	// 10. Requeue so sweepExpiry (teardown.go, reads Phase+ProvisionedAt to
	// actually expire) gets a chance to run without another trigger: come back
	// at whichever is sooner, the age limit or an hour.
	requeueAfter := time.Hour
	if remaining := ws.Status.ProvisionedAt.Add(ws.Spec.Limits.MaxAge.Duration).Sub(r.now()); remaining < requeueAfter {
		requeueAfter = remaining
	}
	if requeueAfter < 0 {
		requeueAfter = 0
	}
	// A live test watch wants to be back at its poll interval (or its deadline,
	// when that is nearer); the sweeper wants to be back at the age limit.
	// Whichever is sooner wins — taking the watch's value outright would push
	// the sweeper past an expiry it is the only thing enforcing.
	if watchRes.RequeueAfter > 0 && watchRes.RequeueAfter < requeueAfter {
		requeueAfter = watchRes.RequeueAfter
	}
	// An undecided close request (step 7b) wants to be back sooner still, for
	// the same reason and on the same terms: nothing watches what it is waiting
	// on, and the tool that asked is only waiting thirty seconds. Shorter wins,
	// and a pass that decided everything asks for nothing, so this never
	// shortens the requeue of a workshop with no request outstanding.
	if closeRes.RequeueAfter > 0 && closeRes.RequeueAfter < requeueAfter {
		requeueAfter = closeRes.RequeueAfter
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// failLayer stamps condType False/ProvisionFailed with cause's message,
// persists status, and ALWAYS returns cause — even when persisting itself
// errors (logged, never swallowed) — so a half-provisioned workshop is
// requeued and retried on every path, never marked Ready.
func (r *Reconciler) failLayer(ctx context.Context, ws *spiceboxv1alpha1.Workshop, condType string, cause error) (ctrl.Result, error) {
	ws.Status.ObservedGeneration = ws.Generation
	conditions.SetFalse(ws, &ws.Status.Conditions, condType, spiceboxv1alpha1.ReasonWorkshopProvisionFailed, cause.Error())
	if err := r.Client.Status().Update(ctx, ws); err != nil {
		log.FromContext(ctx).Info("workshop: persisting a failed provisioning layer's status errored; the next reconcile retries both the layer and the persist",
			"workshop", ws.Namespace+"/"+ws.Name, "condition", condType, "persistErr", err.Error(), "cause", cause.Error())
	}
	return ctrl.Result{}, cause
}

// generateWorkshopToken mints 32 random bytes, hex-encoded — the same shape
// as every other per-session bearer this repo mints (see agentsession's
// generateToken).
func generateWorkshopToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetupWithManager wires the controller. No Owns: the namespace this
// controller creates is cluster-scoped and cannot be owned by a namespaced
// Workshop, and the requeue arm (step 10 above) is what catches drift in the
// namespace-scoped objects a namespace-delete-and-recreate could disturb.
// The AgentSession watch enqueues this workshop's deterministic name the
// moment its session starts terminating, rather than waiting on that
// session's own (possibly slow) finalizer chain to finish before owner-ref
// GC would otherwise reach this CR.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.Workshop{}).
		Watches(&spiceboxv1alpha1.AgentSession{}, handler.EnqueueRequestsFromMapFunc(r.MapSessionToWorkshop)).
		Complete(r)
}

// workshopNamespacePrefix is what WorkshopNamespaceName emits before the
// session-uid hex, DERIVED from that function rather than spelled out, so a
// change to the naming scheme carries here on its own. It is only ever a
// cheap pre-filter (see MapSessionToWorkshop); the authoritative tie between
// a namespace and its Workshop is always the namespace's labels.
var workshopNamespacePrefix = spiceboxv1alpha1.WorkshopNamespaceName("")

// MapSessionToWorkshop is the AgentSession watch's mapper. Two shapes of
// session reach it and they resolve to a Workshop in two different ways:
//
//   - The BUILDER session, in an ordinary namespace, whose own name derives
//     the Workshop's (WorkshopName). This is the mapping that gets a
//     terminating session's Workshop reconciled promptly, rather than waiting
//     on that session's finalizer chain and owner-ref GC.
//   - A TEST session, inside a workshop namespace — a person trying the agent
//     they just built (spec.testWatch, testwatch.go). Its name derives
//     nothing, so the owning Workshop is read off the namespace's labels.
//     This branch is what makes the watch react to a pause or an ending at
//     once; without it the watch still converges, but only at
//     testWatchPollInterval.
//
// The two branches are exclusive rather than additive because a session in a
// workshop namespace can never own a Workshop of its own: the sanction that
// is the only path to one (agentsession's ensureWorkshop) requires a cluster
// admin to have named that exact (namespace, class) in ClusterAgentSettings,
// and no admin can pre-name a namespace whose hex is derived from a session
// uid that did not exist yet.
//
// Exported so the package's external test can assert both branches, the same
// reason the Build* helpers here are exported.
//
// The Namespace Get is uncached (APIReader-or-Client, the idiom used by
// pkg/controllers/agentclass's workshopOwnerStarter for the identical read):
// no controller in this operator watches Namespace, so a cached Get here
// would lazily start a standing, cluster-wide Namespace watch just to answer
// a label lookup. It is reached only for sessions whose namespace name could
// be a workshop's, so an ordinary session event costs no API call at all. A
// failed Get logs and maps nothing rather than guessing at the workshop-named
// fallback, which for a session in a workshop namespace never names a real CR.
func (r *Reconciler) MapSessionToWorkshop(ctx context.Context, o client.Object) []reconcile.Request {
	sess, ok := o.(*spiceboxv1alpha1.AgentSession)
	if !ok {
		return nil
	}
	if strings.HasPrefix(sess.Namespace, workshopNamespacePrefix) {
		var ns corev1.Namespace
		if err := r.reader().Get(ctx, types.NamespacedName{Name: sess.Namespace}, &ns); err != nil {
			log.FromContext(ctx).Info("workshop: reading the namespace of a session that may be a workshop test failed; its workshop is not being reconciled for this event",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			return nil
		}
		sessNS, sessName := ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace], ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName]
		if sessNS != "" && sessName != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{
				Namespace: sessNS,
				Name:      spiceboxv1alpha1.WorkshopName(sessName),
			}}}
		}
		// A namespace that merely shares the prefix and carries no workshop
		// labels is someone else's; fall through to the name-derived mapping.
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: sess.Namespace,
		Name:      spiceboxv1alpha1.WorkshopName(sess.Name),
	}}}
}
