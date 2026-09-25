package agentsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// classLabelKey is stamped onto every SandboxTemplate/SandboxWarmPool
// ReconcilePool creates, so SweepOrphanedPools can reclaim them with one
// cross-namespace List — the only mechanism reaching a namespace the
// per-namespace loop no longer iterates toward. A label rather than tracked
// state (e.g. a previous-namespace list in status) survives an operator
// restart with nothing to rehydrate or drift, so the sweep also self-heals
// changes made while the operator was down.
const classLabelKey = "agentprimitives.authzed.com/spicebox-class-hash"

// classLabelValueHexLen is how many hex characters of classLabelKey's
// SHA-256 value are kept. 128 bits makes a collision between two distinct
// class names astronomically unlikely.
const classLabelValueHexLen = 32

// classLabelValue hashes className into a label-value-safe identifier. Label
// values cap at 63 characters — far shorter than a class name may be — so
// this ALWAYS hashes, unlike poolNameFor/templateNameFor, which stay readable
// under the 253-character DNS-1123 subdomain budget and hash only on
// truncation. Hashing sometimes would work until a long class name arrived,
// and readability buys nothing: only SweepOrphanedPools's List reads it.
func classLabelValue(className string) string {
	return hashHex([]byte(className), classLabelValueHexLen)
}

// templateHashHexLen is how many hex characters of the rendered PodSpec's
// SHA-256 are appended to the class name. 48 bits keeps the object name short
// while making a collision between two pod shapes of one class negligible.
const templateHashHexLen = 12

// hashHex returns the first n hex characters of data's SHA-256 sum.
func hashHex(data []byte, n int) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:n]
}

// templateNameFor names the SandboxTemplate for one class's rendered PodSpec.
// A PURE function of (className, spec) — no wall clock, no randomness, no
// counter — so a byte-identical re-render re-applies as a no-op, while any
// change to the PodSpec mints a NEW name instead of mutating in place. That
// is what makes stale-sandbox replacement work: ReconcilePool re-points the
// warm pool at the new name and the pool's Recreate strategy tears down
// sandboxes built from the old one. Mutating in place would look unchanged to
// that comparison and serve stale sandboxes to sessions forever.
//
// A json.Marshal failure propagates rather than falling back to another
// encoding. %#v is the tempting fallback and is unusable: it prints POINTER
// ADDRESSES for PodSpec's many *bool/*string/*Quantity fields, so two
// identical spec values would yield different names — a new template and a
// full pool re-point on every reconcile, the exact unbounded-orphan failure
// this function prevents. No fallback encoding is provably as pure.
func templateNameFor(className string, spec corev1.PodSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal rendered pod spec: %w", err)
	}
	return dnsSafeName(className, "-"+hashHex(b, templateHashHexLen)), nil
}

// poolNameFor names the class's SandboxWarmPool. Unlike templateNameFor, it
// depends on className ALONE, so the pool is stable across class edits and
// ReconcilePool re-points the same object. A hash-named pool would mint a
// fresh one per edit that nothing would ever delete.
func poolNameFor(className string) string {
	return dnsSafeName(className, "-pool")
}

// truncationDisambiguatorHexLen is how many hex characters of a hash of the
// FULL (pre-truncation) base dnsSafeName inserts when it truncates.
// Truncation alone is NOT injective — two bases sharing a long common prefix
// collapse to one string — and for poolNameFor that means two distinct
// SpiceboxClasses computing the same pool name and each re-pointing the
// other's warm pool. 32 bits makes that collision negligible.
const truncationDisambiguatorHexLen = 8

// dnsSafeName appends suffix to base, truncating base to fit the DNS-1123
// subdomain length limit. Truncation inserts a hash of the full base to keep
// the mapping injective (see truncationDisambiguatorHexLen), after trimming
// any trailing '-' or '.' the cut left behind so the disambiguator never
// lands on an invalid trailing character.
func dnsSafeName(base, suffix string) string {
	max := validation.DNS1123SubdomainMaxLength
	if len(base)+len(suffix) <= max {
		return base + suffix
	}
	disambiguator := "-" + hashHex([]byte(base), truncationDisambiguatorHexLen)
	keep := max - len(suffix) - len(disambiguator)
	if keep < 0 {
		keep = 0
	}
	return strings.TrimRight(base[:keep], "-.") + disambiguator + suffix
}

// ReconcilePool brings the SandboxTemplate + SandboxWarmPool for one class in
// line with req.Replicas. Idempotent — safe to call on every class reconcile.
//
// req.ClassUID is required: it becomes the ownerReference on every object
// created here, so deleting the SpiceboxClass garbage-collects the pool. An
// empty ClassUID is refused rather than silently creating an unowned pool.
func (r *Runtime) ReconcilePool(ctx context.Context, req sandboxkinds.PoolRequest) error {
	if req.ClassUID == "" {
		return fmt.Errorf(
			"reconcile warm pool for %s/%s: ClassUID is required (an unowned pool would "+
				"never be garbage collected when the class is deleted)", req.Namespace, req.ClassName)
	}

	// prewarmAvailable is resolved once at NewRuntime from the RESTMapper: the
	// extensions.agents.x-k8s.io CRDs (SandboxClaim/SandboxWarmPool) ship
	// separately from the base Sandbox CRD, so a cluster can serve one and not
	// the other. The spiceboxclass controller's type assertion only proves the
	// Runtime implements Prewarmer, not that pre-warming is usable now;
	// reporting the shared sentinel here is what lets it stamp a validation
	// condition instead of requeuing forever on a transient-looking failure.
	if !r.prewarmAvailable {
		if req.Replicas == 0 {
			// No pool can exist without the extensions CRDs, so there is
			// nothing to tear down. Erroring here would make every pass over
			// a class that never asked to pre-warm a reconcile failure.
			return nil
		}
		return fmt.Errorf("agent-sandbox: %w (the extensions.agents.x-k8s.io CRDs — "+
			"SandboxClaim/SandboxWarmPool — are not installed in this cluster)",
			sandboxkinds.ErrPrewarmingUnavailable)
	}

	poolName := poolNameFor(req.ClassName)

	if req.Replicas == 0 {
		// Leave the template: cheap, immutable, bounded at one per class shape.
		// Deleting it only churns the next non-zero reconcile.
		return r.deleteWarmPool(ctx, req.Namespace, poolName)
	}

	spec, err := podspec.BuildClassSpec(req.Class, req.Toolchains)
	if err != nil {
		return fmt.Errorf("render class pod spec for %s/%s: %w", req.Namespace, req.ClassName, err)
	}
	templateName, err := templateNameFor(req.ClassName, spec)
	if err != nil {
		return fmt.Errorf("compute template name for %s/%s: %w", req.Namespace, req.ClassName, err)
	}
	owner := classOwnerRef(req.ClassName, req.ClassUID)

	if err := r.ensureTemplate(ctx, req.Namespace, templateName, req.ClassName, spec, owner); err != nil {
		return err
	}

	oldTemplateName, err := r.ensureWarmPool(ctx, req.Namespace, poolName, req.ClassName, templateName, req.Replicas, owner)
	if err != nil {
		return err
	}

	// The pool pointed at a different template, which is now unreferenced and,
	// being hash-named, will never be reused. O(1) — the old ref came off the
	// pool object ensureWarmPool already had in hand.
	//
	// Accepted limitation: a delete failing with anything but NotFound is NOT
	// retried. ensureWarmPool has already written the new TemplateRef, so the
	// next reconcile sees a converged pool and short-circuits past this block.
	// The blast radius is one sandbox-less SandboxTemplate CR; reordering the
	// Update and Delete to close the window would instead leave the pool
	// briefly pointing at a template about to vanish, which is worse.
	if oldTemplateName != "" && oldTemplateName != templateName {
		if err := r.deleteTemplate(ctx, req.Namespace, oldTemplateName); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphanedPools deletes every SandboxWarmPool and SandboxTemplate this
// backend has ever created for className whose namespace is not in
// desiredNamespaces. ReconcilePool's per-namespace loop only ever acts on the
// current desired set, so a namespace dropped from it is never revisited.
//
// Driven by classLabelKey — one label-scoped List across all namespaces — not
// by tracked state such as a previous-namespace list in status: nothing
// durable to drift, and it self-heals a namespace-list edit made while the
// operator was down, which diffing against a last-observed list could not.
//
// Ownership is re-checked with hasOwnerRef before every delete. The label is
// a fast index, not the authority: a classLabelValue collision is
// astronomically unlikely, but a destructive cross-namespace operation should
// not rest on that alone.
func (r *Runtime) SweepOrphanedPools(ctx context.Context, className string, classUID types.UID, desiredNamespaces []string) error {
	if !r.prewarmAvailable {
		// Without the extensions CRDs nothing could have been created — same
		// reasoning as ReconcilePool's prewarmAvailable guard.
		return nil
	}

	desired := make(map[string]bool, len(desiredNamespaces))
	for _, ns := range desiredNamespaces {
		desired[ns] = true
	}
	owner := classOwnerRef(className, classUID)
	sel := client.MatchingLabels{classLabelKey: classLabelValue(className)}

	var pools sandboxextv1beta1.SandboxWarmPoolList
	if err := r.deps.Client.List(ctx, &pools, sel); err != nil {
		return fmt.Errorf("list warm pools for class %s: %w", className, err)
	}
	for i := range pools.Items {
		p := &pools.Items[i]
		if desired[p.Namespace] || !hasOwnerRef(p.OwnerReferences, owner) {
			continue
		}
		if err := r.deps.Client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphaned warm pool %s/%s: %w", p.Namespace, p.Name, err)
		}
	}

	var templates sandboxextv1beta1.SandboxTemplateList
	if err := r.deps.Client.List(ctx, &templates, sel); err != nil {
		return fmt.Errorf("list sandbox templates for class %s: %w", className, err)
	}
	for i := range templates.Items {
		t := &templates.Items[i]
		if desired[t.Namespace] || !hasOwnerRef(t.OwnerReferences, owner) {
			continue
		}
		if err := r.deps.Client.Delete(ctx, t); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphaned sandbox template %s/%s: %w", t.Namespace, t.Name, err)
		}
	}
	return nil
}

// tryAdopt claims a pre-warmed sandbox for this session when it may safely
// take one, reporting whether it did. p is the session's already-rendered pod
// (podspec.Build) and claimName is the name Ensure resolves the handle to.
//
// A false return with a nil error is an ORDINARY COLD START, not a failure —
// no prewarming CRDs, no pool for the class, or a session whose rendered
// PodSpec is not the class's. Each still gets a correct sandbox, just not an
// instant one, so none sets a Failed condition or logs at error level.
func (r *Runtime) tryAdopt(
	ctx context.Context, req sandboxkinds.EnsureRequest, p *corev1.Pod, claimName string,
) (bool, error) {
	ns := req.Session.Namespace
	className := req.Session.Spec.Class
	if !r.prewarmAvailable || className == "" {
		// A pool is named for its class, so a session with no class name has
		// nothing to claim against. Not an error — the class spec still
		// rendered, so a cold sandbox is the right answer.
		return false, nil
	}

	classSpec, err := podspec.BuildClassSpec(req.Class, req.Session.Status.ResolvedToolchains)
	if err != nil {
		return false, fmt.Errorf("render class pod spec for %s/%s: %w", ns, className, err)
	}
	// ELIGIBILITY IS TWO EQUALITIES, derived — never a hand-written list of
	// today's session-specific inputs, so a new one makes such sessions
	// ineligible automatically. FIRST: does this session add anything to the
	// class shape, given its own frozen toolchains? A pooled sandbox is built
	// from BuildClassSpec alone, so a session whose rendered PodSpec differs at
	// all would silently receive a pod missing that difference — no workspace
	// mount, no skill bundles.
	//
	// Not sufficient alone: both sides derive from req.Class, the snapshot
	// frozen onto status.resolvedClass at first bind, so this can only answer
	// "does the session add anything", never "is the pool still built from the
	// frozen shape". The template-name check below is the second equality, and
	// the only one that can catch a toolchain mismatch — both sides here render
	// from the session's own ResolvedToolchains.
	//
	// equality.Semantic, not reflect.DeepEqual, so equivalent resource.Quantity
	// values ("1" and "1000m") do not read as a difference.
	if !equality.Semantic.DeepEqual(p.Spec, classSpec) {
		return false, nil
	}

	poolName := poolNameFor(className)
	var pool sandboxextv1beta1.SandboxWarmPool
	if err := r.deps.Client.Get(ctx,
		types.NamespacedName{Namespace: ns, Name: poolName}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			// The class has not opted into pre-warming (or its pool is being
			// torn down). Cold start.
			return false, nil
		}
		return false, fmt.Errorf("get sandbox warm pool %s/%s: %w", ns, poolName, err)
	}

	// THE SECOND EQUALITY: the pool must currently hold the SHAPE this session
	// was frozen at. The first is blind to a class edit — req.Class is
	// status.resolvedClass, frozen at first bind, while ReconcilePool renders
	// the pool's template from the class CR's CURRENT spec and current
	// toolchain resolution, and freezing and adoption are always separate
	// reconcile passes. So an admin bumping the class image, or the catalog
	// resolving a new digest for a mount the class already declares, re-points
	// the pool and Recreate rebuilds every warm sandbox at the new shape while
	// this session still renders the old one — and would adopt a sandbox with
	// the wrong image, limits, mounts or toolchain overlay while status claimed
	// otherwise.
	//
	// Comparing the pool's TemplateRef against the name this session's frozen
	// spec produces reuses the same mechanism ReconcilePool uses to replace
	// stale sandboxes, and errs the right way: a mismatch costs one cold start
	// while the edit propagates, never a wrong sandbox.
	wantTemplate, err := templateNameFor(className, classSpec)
	if err != nil {
		return false, fmt.Errorf("compute template name for %s/%s: %w", ns, className, err)
	}
	if pool.Spec.TemplateRef.Name != wantTemplate {
		return false, nil
	}

	claim := &sandboxextv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      claimName,
			Labels:    p.Labels,
			// The SpiceboxSession, reused verbatim from podspec.Build: deleting
			// the session reclaims the claim — and, by cascade, the Sandbox the
			// claim owns — even if Teardown never runs.
			OwnerReferences: p.OwnerReferences,
		},
		Spec: sandboxextv1beta1.SandboxClaimSpec{
			// A SandboxClaim references a WARM POOL. There is no template ref
			// on a claim at all, which is the whole reason poolNameFor is a
			// pure function of the class name and stays stable across class
			// edits while template names are hash-derived.
			WarmPoolRef: sandboxextv1beta1.SandboxWarmPoolRef{Name: poolName},

			// SECURITY, not cosmetics. AP's per-session sandbox NetworkPolicy
			// selects the pod by agentprimitives.authzed.com/session
			// (pkg/controllers/agentsession/netpol.go), BuildClassSpec returns a
			// bare PodSpec with no ObjectMeta, and the pool's template is
			// deliberately NetworkPolicyManagement: Unmanaged. Without these
			// labels the adopted pod is selected by NO NetworkPolicy at all,
			// strictly less isolated than a cold sandbox. additionalPodMetadata
			// is the one field upstream lets a claim inject, and it propagates
			// onto the already-running pod on adoption.
			//
			// OPERATOR PREREQUISITE: the agent-sandbox controller checks these
			// label keys against the domain allowlist in
			// /etc/sandbox-config/allowed-label-domains, which defaults to
			// "sandbox.users.io" alone; a cluster that pre-warms AP sandboxes
			// MUST add "agentprimitives.authzed.com". Otherwise the claim is
			// refused (Ready=False/InvalidMetadata) and no sandbox is created —
			// fail-closed, never an unpoliced pod — with the claim's own message
			// surfaced through Status.
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
				Labels:      p.Labels,
				Annotations: p.Annotations,
			},

			// Env and VolumeClaimTemplates stay unset: upstream documents both
			// as FORCING a cold start, and the template's Disallowed injection
			// policies would reject the claim anyway. Lifecycle stays unset for
			// the reason the direct Sandbox path leaves ShutdownTime unset — AP's
			// TTL controller owns expiry, its idle-sleep/reap controller owns
			// suspension — so the claim never expires and ShutdownPolicy is never
			// consulted.
		},
	}
	if err := r.deps.Client.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create sandbox claim %s/%s: %w", ns, claimName, err)
	}
	// AlreadyExists means a concurrent Ensure won the race: the claim this
	// session needs exists either way, so this is adoption, not a failure.
	return true, nil
}

// claimBinding is what a single Get of a session's SandboxClaim tells the
// verbs that must reach the Sandbox behind it.
type claimBinding struct {
	// found is false when the claim itself no longer exists.
	found bool
	// sandboxName is the pool-generated Sandbox the claim resolved to. Empty
	// until the agent-sandbox controller binds one — a normal transient state,
	// which is why this is a field rather than an error.
	sandboxName string
	// notReady is the claim's own Ready condition when it reports a problem,
	// nil otherwise.
	notReady *metav1.Condition
	// wantPodLabels is spec.additionalPodMetadata.labels — what the claim asked
	// be injected onto the pod. Carried out of the Get Status already performs
	// so verifying the injection landed costs no extra read; see
	// Runtime.adoptedPodLabelsLanded for why that check is a security control.
	wantPodLabels map[string]string
}

// getClaimBinding reads the claim a pre-warmed handle names.
//
// Handle.Ref names the CLAIM for an adopted session, because the Sandbox
// behind it is minted by the pool under a name this backend cannot predict.
// claim.status.sandbox.name is the only link between the two.
func (r *Runtime) getClaimBinding(ctx context.Context, ns, name string) (claimBinding, error) {
	var claim sandboxextv1beta1.SandboxClaim
	if err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return claimBinding{}, nil
		}
		return claimBinding{}, fmt.Errorf("get sandbox claim %s/%s: %w", ns, name, err)
	}
	b := claimBinding{
		found:         true,
		sandboxName:   claim.Status.SandboxStatus.Name,
		wantPodLabels: claim.Spec.AdditionalPodMetadata.Labels,
	}
	if c := meta.FindStatusCondition(claim.Status.Conditions,
		string(sandboxv1beta1.SandboxConditionReady)); c != nil && c.Status != metav1.ConditionTrue {
		b.notReady = c
	}
	return b, nil
}

// claimRefusedReason is the condition reason upstream sets when it rejects a
// claim's own spec. Keyed on the machine reason, never message text, which is
// free-form prose a version bump may reword.
//
// It is the one refusal AP can provoke: the agent-sandbox controller checks
// additionalPodMetadata label keys against a domain allowlist AP (a
// bring-your-own consumer) cannot set, and AP's session label lives under
// agentprimitives.authzed.com, so a stock install refuses every AP claim. Its
// sibling EnvVarsInjectionRejected is unreachable — AP never populates
// SandboxClaimSpec.Env, and the template's Disallowed policy would refuse it.
const claimRefusedReason = "InvalidMetadata"

// claimRefused reports whether the claim has been REFUSED — upstream rejected
// the claim's own spec, so re-submitting identical bytes can never succeed.
// Distinct from "not ready yet", which is transient and must be waited on.
func claimRefused(b claimBinding) bool {
	return b.notReady != nil && b.notReady.Reason == claimRefusedReason
}

// discardRefusedClaim deletes a refused claim so the caller can fall through
// to an ordinary cold start, and tells the cluster operator why.
//
// PRE-WARMING MUST DEGRADE, NEVER BREAK. The refusal is a cluster
// misconfiguration AP cannot fix from inside, and leaving the claim in place
// would wedge the session at Pending forever over an optimisation it does not
// need. The delete tolerates IsNotFound (a concurrent deleter) and
// IsNoMatchError (the extensions CRDs uninstalled underneath us): neither
// leaves anything to reclaim, and erroring would restore the wedge.
func (r *Runtime) discardRefusedClaim(
	ctx context.Context, sess *v1alpha1.SpiceboxSession, ns, name string, b claimBinding,
) error {
	claim := &sandboxextv1beta1.SandboxClaim{}
	claim.Namespace, claim.Name = ns, name
	if err := r.deps.Client.Delete(ctx, claim); err != nil &&
		!apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete refused sandbox claim %s/%s: %w", ns, name, err)
	}

	// Logged unconditionally, not only when monitoring is wired: a cluster
	// with no bus must still be able to find out why its pre-warming never
	// fires, and the operator log is the floor under every other surface.
	r.deps.Logger.Info(
		"pre-warming refused by the agent-sandbox controller; discarded the claim and starting a cold sandbox",
		"session", ns+"/"+name, "reason", b.notReady.Reason, "message", b.notReady.Message)

	r.emitPrewarmRefused(sess, ns, name, b)
	return nil
}

// emitPrewarmRefused fans the refusal out to every role=monitoring Channel.
//
// The audience is the CLUSTER OPERATOR, not a session participant — the fix
// is a flag on someone else's controller — so this is a monitoring event, not
// a session notice. Nil-guarded: an unconfigured bus must degrade the warning,
// never the degradation itself.
//
// Emitted on the TRANSITION only. Its one call site is the pass that finds a
// refused claim and deletes it; later passes run the same lookup, find
// nothing, and fall through to a direct Sandbox. So a misconfigured cluster
// produces one event per session attempting adoption, not one per reconcile —
// this repo has a whole defect class of notices reposting every pass. Not
// exactly-once, though: a stale informer read can surface the just-deleted
// claim and emit a second event, self-correcting once the delete propagates.
//
// A publish failure is logged, not returned: the session is already on the
// cold path, and failing Ensure over an undelivered warning would turn a
// degradation into the outage it just avoided.
func (r *Runtime) emitPrewarmRefused(sess *v1alpha1.SpiceboxSession, ns, name string, b claimBinding) {
	if r.deps.MonitoringPublish == nil {
		return
	}
	// The condition's own transition time, so the event dates the refusal
	// rather than the reconcile that noticed it.
	when := b.notReady.LastTransitionTime.Time
	if when.IsZero() {
		when = time.Now()
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "reconcile",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "SpiceboxSession", Namespace: sess.Namespace, Name: sess.Name,
		},
		Condition: string(sandboxv1beta1.SandboxConditionReady),
		Reason:    b.notReady.Reason,
		Summary: fmt.Sprintf(
			"pre-warming is disabled for this cluster: the agent-sandbox controller refused "+
				"SandboxClaim %s/%s (%s: %s). Session %s is starting a cold sandbox instead — "+
				"slower, but correct and fully network-policied.",
			ns, name, b.notReady.Reason, b.notReady.Message, sess.Name),
		Hint: "add \"agentprimitives.authzed.com\" to the agent-sandbox controller's allowed " +
			"label domains (the file it reads at /etc/sandbox-config/allowed-label-domains, " +
			"which defaults to \"sandbox.users.io\" alone). AP stamps that label so its " +
			"per-session NetworkPolicy can select the sandbox pod, so a claim without it is " +
			"refused by design rather than adopted unpoliced. Until then every session falls " +
			"back to a cold start.",
		Timestamp: when,
	}
	if err := channelevents.PublishMonitoring(r.deps.MonitoringPublish, ev); err != nil {
		r.deps.Logger.Info("publish pre-warming-refused monitoring event failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
}

// unboundClaimStatus describes a claim that has not been given a sandbox yet.
// PhasePending, never PhaseGone and never an error: the claim exists and the
// agent-sandbox controller is still working on it.
//
// The claim's Ready condition is surfaced verbatim when it reports a problem.
// Otherwise an upstream refusal (see claimRefusedReason) reaches the operator
// as an indefinite, reasonless Pending — the silent hang no-silent-errors
// exists to prevent.
func unboundClaimStatus(ns, name string, b claimBinding) sandboxkinds.Status {
	if b.notReady != nil && b.notReady.Reason != "" {
		return sandboxkinds.Status{
			Phase:   sandboxkinds.PhasePending,
			Reason:  b.notReady.Reason,
			Message: conditionMessage(b.notReady, fmt.Sprintf("sandbox claim %s/%s is not ready", ns, name)),
		}
	}
	return sandboxkinds.Status{
		Phase:   sandboxkinds.PhasePending,
		Reason:  sandboxkinds.ReasonCreating,
		Message: fmt.Sprintf("sandbox claim %s/%s has not been given a sandbox yet", ns, name),
	}
}

// classOwnerRef builds the ownerReference AP stamps onto every object a
// class's warm pool owns. Mirrors the SpiceboxSession ownerRef shape in
// podspec.Build (pkg/platform/podspec/builder.go).
func classOwnerRef(className string, classUID types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.SchemeGroupVersion.String(),
		Kind:               "SpiceboxClass",
		Name:               className,
		UID:                classUID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// hasOwnerRef reports whether refs contains owner, compared on the fields
// identifying a specific owning object and its GC semantics (APIVersion,
// Kind, Name, UID).
func hasOwnerRef(refs []metav1.OwnerReference, owner metav1.OwnerReference) bool {
	for _, r := range refs {
		if r.APIVersion == owner.APIVersion && r.Kind == owner.Kind &&
			r.Name == owner.Name && r.UID == owner.UID {
			return true
		}
	}
	return false
}

// ensureTemplate creates the SandboxTemplate if absent. Templates are
// hash-named (templateNameFor), so an existing template with this exact name
// already has this exact spec — there is nothing to reconcile on a hit, only
// on absence.
func (r *Runtime) ensureTemplate(
	ctx context.Context, namespace, name, className string, spec corev1.PodSpec, owner metav1.OwnerReference,
) error {
	var existing sandboxextv1beta1.SandboxTemplate
	err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get sandbox template %s/%s: %w", namespace, name, err)
	}

	tmpl := &sandboxextv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			Labels:          map[string]string{classLabelKey: classLabelValue(className)},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: sandboxextv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{Spec: spec},
			},
			// Unmanaged, deliberately. Upstream's default is Managed, under which
			// the agent-sandbox controller creates its own per-template
			// NetworkPolicy allowing egress to the public internet. Kubernetes
			// NetworkPolicies are ADDITIVE — the union of every policy selecting
			// a pod — so an adopted sandbox would get that unioned with AP's
			// per-session policy (pkg/controllers/agentsession/netpol.go), and a
			// session set to networkMode: none would silently regain public
			// egress: a pre-warmed sandbox strictly less isolated than a cold
			// one. AP's per-session policy is the sole authority.
			//
			// Exactly one window has no NetworkPolicy, and it is accepted: an
			// idle, UNADOPTED pool pod, which only runs `sleep infinity` and
			// holds no session's credentials, workspace or tools. An ADOPTED pod
			// is covered because Status withholds PhaseReady for a pre-warmed
			// handle until it has read the pod and seen the labels land, so no
			// tool call can be dispatched into an unpoliced pod — see
			// adoptedPodLabelsLanded.
			//
			// EnvVarsInjectionPolicy and VolumeClaimTemplatesPolicy stay unset:
			// their Disallowed defaults structurally stop a claim from mutating
			// the pod shape, which is what keeps tryAdopt's
			// eligibility-by-equality check honest.
			NetworkPolicyManagement: sandboxextv1beta1.NetworkPolicyManagementUnmanaged,
		},
	}
	if err := r.deps.Client.Create(ctx, tmpl); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create sandbox template %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureWarmPool creates or re-points the class's (stably-named) warm pool at
// templateName with the requested replica count, returning the templateRef it
// held BEFORE this call (empty on first creation) so the caller can clean up
// an old template that is no longer referenced.
func (r *Runtime) ensureWarmPool(
	ctx context.Context, namespace, name, className, templateName string, replicas int32, owner metav1.OwnerReference,
) (oldTemplateName string, err error) {
	wantLabel := classLabelValue(className)
	var existing sandboxextv1beta1.SandboxWarmPool
	getErr := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	switch {
	case getErr == nil:
		oldTemplateName = existing.Spec.TemplateRef.Name
		if oldTemplateName == templateName &&
			existing.Spec.Replicas != nil && *existing.Spec.Replicas == replicas &&
			existing.Spec.UpdateStrategy != nil &&
			existing.Spec.UpdateStrategy.Type == sandboxextv1beta1.RecreateSandboxWarmPoolUpdateStrategyType &&
			hasOwnerRef(existing.OwnerReferences, owner) &&
			existing.Labels[classLabelKey] == wantLabel {
			// Already converged: skip the write rather than issue a no-op Update
			// every reconcile. OwnerReferences and the class label count as part
			// of convergence, not just Spec — a pool lacking either would
			// otherwise never gain them, leaking exactly the way req.ClassUID
			// and classLabelKey exist to prevent.
			return oldTemplateName, nil
		}
		existing.Spec.TemplateRef = sandboxextv1beta1.SandboxTemplateRef{Name: templateName}
		existing.Spec.Replicas = ptr.To(replicas)
		existing.Spec.UpdateStrategy = recreateUpdateStrategy()
		if !hasOwnerRef(existing.OwnerReferences, owner) {
			existing.OwnerReferences = append(existing.OwnerReferences, owner)
		}
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[classLabelKey] = wantLabel
		if err := r.deps.Client.Update(ctx, &existing); err != nil {
			return "", fmt.Errorf("update sandbox warm pool %s/%s: %w", namespace, name, err)
		}
		return oldTemplateName, nil

	case apierrors.IsNotFound(getErr):
		pool := &sandboxextv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       namespace,
				Name:            name,
				Labels:          map[string]string{classLabelKey: wantLabel},
				OwnerReferences: []metav1.OwnerReference{owner},
			},
			Spec: sandboxextv1beta1.SandboxWarmPoolSpec{
				Replicas:       ptr.To(replicas),
				TemplateRef:    sandboxextv1beta1.SandboxTemplateRef{Name: templateName},
				UpdateStrategy: recreateUpdateStrategy(),
			},
		}
		if err := r.deps.Client.Create(ctx, pool); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create sandbox warm pool %s/%s: %w", namespace, name, err)
		}
		// No old ref on first creation (or on a create race this process lost;
		// either way there is nothing THIS call knows to clean up).
		return "", nil

	default:
		return "", fmt.Errorf("get sandbox warm pool %s/%s: %w", namespace, name, getErr)
	}
}

// recreateUpdateStrategy is REQUIRED, not optional. Upstream's zero value
// (OnReplenish) keeps stale sandboxes in the pool until a claim adopts one, so
// a session could adopt a sandbox built from the previous class shape and
// silently receive the old image. Do not "simplify" it back to the zero value.
//
// It guards only the STALE-POD direction (a pool sandbox older than the
// class). The mirror direction — a session frozen at a shape the pool has
// moved past — is guarded by tryAdopt's template-name comparison, since
// nothing about the pool's own contents can detect it.
func recreateUpdateStrategy() *sandboxextv1beta1.SandboxWarmPoolUpdateStrategy {
	return &sandboxextv1beta1.SandboxWarmPoolUpdateStrategy{
		Type: sandboxextv1beta1.RecreateSandboxWarmPoolUpdateStrategyType,
	}
}

// deleteWarmPool deletes the class's warm pool. Idempotent: an already-absent
// pool is success, matching Runtime.Teardown's convention for the same
// reason — this runs from a reconcile loop that re-enters until it succeeds.
func (r *Runtime) deleteWarmPool(ctx context.Context, namespace, name string) error {
	pool := &sandboxextv1beta1.SandboxWarmPool{}
	pool.Namespace, pool.Name = namespace, name
	if err := r.deps.Client.Delete(ctx, pool); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete sandbox warm pool %s/%s: %w", namespace, name, err)
	}
	return nil
}

// deleteTemplate deletes a template the pool no longer references.
// Idempotent, tolerating a concurrent deleter.
func (r *Runtime) deleteTemplate(ctx context.Context, namespace, name string) error {
	tmpl := &sandboxextv1beta1.SandboxTemplate{}
	tmpl.Namespace, tmpl.Name = namespace, name
	if err := r.deps.Client.Delete(ctx, tmpl); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale sandbox template %s/%s: %w", namespace, name, err)
	}
	return nil
}

var _ sandboxkinds.Prewarmer = (*Runtime)(nil)
