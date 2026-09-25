package identityrefresh

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/backoff"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// MinRequeueFloor prevents pathologically tight reconcile loops when an
// expires_at is very near or a backoff delay drops below this. Picked to be
// above typical informer-cache convergence latency.
const MinRequeueFloor = 15 * time.Second

// Core is the shared refresh policy, constructed once per controller. The
// Backoff is per-Core, so the two reconcilers keep independent failure
// counters exactly as the two copies did.
type Core struct {
	Client       client.Client
	SecretReader *adoptguard.SecretReader
	// DefaultThreshold is the operator-wide fallback for identities that
	// do not set spec.refreshThreshold.
	DefaultThreshold time.Duration
	Backoff          *backoff.Backoff
}

// New builds a Core. The backoff cap is the default threshold, matching what
// each controller's SetupWithManager used to do.
func New(c client.Client, sr *adoptguard.SecretReader, defaultThreshold time.Duration) *Core {
	return &Core{
		Client:           c,
		SecretReader:     sr,
		DefaultThreshold: defaultThreshold,
		Backoff:          backoff.New(defaultThreshold),
	}
}

// ThresholdFor returns the per-identity refresh threshold, falling back to
// the operator-wide default when spec.refreshThreshold is unset.
func (co *Core) ThresholdFor(obj Identity) time.Duration {
	if o := obj.ThresholdOverride(); o != nil && o.Duration > 0 {
		return o.Duration
	}
	return co.DefaultThreshold
}

// credClass classifies one oauth credential at reconcile time.
//
// credSkipNotOAuth     — credential's type does not need refresh, per
//
//	credkind.Kind.NeedsRefresh (caller filtered, defensive); or the type is
//	unregistered (logged and skipped).
//
// credSkipNoExpiry     — three reachable conditions, all observably equivalent:
//
//	  (1) Secret missing (validity reconciler will mark Valid=False),
//	  (2) Secret has no expires_at key ("never expires" per RFC 6749),
//	  (3) expires_at is malformed (logged + treated as no-expiry).
//	None schedule, none attempt, none retry.
//
// credSkipNoRefreshToken — Secret missing refresh_token; we can never refresh this cred.
// credSchedule          — expires_at > threshold; not yet due, schedule next reconcile.
// credAttempt           — expires_at within threshold; call performRefresh now.
type credClass int

const (
	credSkipNotOAuth credClass = iota
	credSkipNoExpiry
	credSkipNoRefreshToken
	credSchedule
	credAttempt
)

// attemptOutcome records what a credAttempt plan's refresh actually did.
// computeRequeueAfter needs it because only a SUCCEEDED attempt invalidates
// the pre-refresh expiresAt the plan was computed against; a failed one
// leaves the Secret — and therefore the already-inside-the-threshold
// expiresAt — exactly as it was.
type attemptOutcome int

const (
	attemptNotRun attemptOutcome = iota // every class other than credAttempt
	attemptSucceeded
	attemptFailed
	// attemptDeferred: the credential is due, but its backoff from an
	// earlier failure has not elapsed, so no round trip was made. Distinct
	// from attemptFailed because nothing new was learned — the failure
	// count must not escalate and the status condition must not be rewritten.
	attemptDeferred
)

type credPlan struct {
	cred      spiceboxv1alpha1.AgentCredential
	class     credClass
	expiresAt time.Time      // valid for credSchedule + credAttempt
	outcome   attemptOutcome // set by the attempt loop, for credAttempt only
}

// classify reads one oauth credential's Secret and produces a credPlan.
// Returns an error only for unrecoverable read failures (caller re-queues
// with the error).
func (co *Core) classify(ctx context.Context, obj Identity,
	cred spiceboxv1alpha1.AgentCredential, threshold time.Duration) (credPlan, error) {

	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		// The Reconcile loop below already filters to NeedsRefresh credentials
		// before calling classify, so this is unreachable in practice — but it
		// must still fail closed (skip) rather than panic on a nil Kind.
		log.FromContext(ctx).Info("identityrefresh: skipping credential of unknown type",
			"credential", cred.Name, "type", cred.Type, "err", err.Error())
		return credPlan{cred: cred, class: credSkipNotOAuth}, nil
	}
	if !k.NeedsRefresh() {
		return credPlan{cred: cred, class: credSkipNotOAuth}, nil
	}
	// WHERE the refreshable token is stored is the kind's answer, not this
	// package's. This used to read `|| cred.OAuth == nil` — a hardcoded
	// consumer of the NeedsRefresh predicate, and a SILENT one: a second
	// refreshable kind storing anywhere else would classify as "nothing to
	// do" on every reconcile, never be refreshed, and expire with no log, no
	// condition and no event. That breaks the no-silent-errors rule and the
	// registry rule in the same line.
	ref := k.SecretRef(cred)
	if ref == nil {
		log.FromContext(ctx).Info(
			"identityrefresh: credential declares NeedsRefresh but names no backing Secret; nothing to refresh",
			"credential", cred.Name, "type", cred.Type, "identity", obj.Object().GetName())
		return credPlan{cred: cred, class: credSkipNotOAuth}, nil
	}
	ns := obj.SecretNamespace()
	secretRef := types.NamespacedName{Namespace: ns, Name: ref.Name}
	// The owner ref is the identity itself. For the cluster-scoped
	// UserIdentity that is a bare name; ObjectKeyFromObject yields both.
	ownerRef := client.ObjectKeyFromObject(obj.Object())
	// Adopt the referenced Secret — metadata-only SSA that stamps AdoptedLabel
	// so the SecretReader is permitted to access it. Adopt's existence check uses
	// the live reader (co.SecretReader.Reader), so a not-yet-adopted Secret (absent
	// from the label-filtered cache) is seen, and a NotFound means skip-refresh
	// (the validity reconciler marks Valid=False / SecretMissing) rather than
	// silently minting an empty Secret via the SSA apply.
	if err := adoptkit.AdoptSecret(ctx, co.SecretReader.Reader, co.Client, secretRef, ownerRef, obj.Kind()); err != nil {
		if errors.IsNotFound(err) {
			// Validity reconciler will mark Valid=False / SecretMissing;
			// refresh is a no-op here.
			return credPlan{cred: cred, class: credSkipNoExpiry}, nil
		}
		// Non-fatal: the validity reconciler will also adopt + mark Invalid;
		// treat as a transient error and requeue.
		return credPlan{}, fmt.Errorf("adopt Secret %s/%s: %w", ns, ref.Name, err)
	}
	sec, err := co.SecretReader.Get(ctx, secretRef)
	if err != nil {
		if errors.IsNotFound(err) {
			// Validity reconciler will mark Valid=False / SecretMissing;
			// refresh is a no-op here.
			return credPlan{cred: cred, class: credSkipNoExpiry}, nil
		}
		return credPlan{}, err
	}
	if len(sec.Data["refresh_token"]) == 0 {
		return credPlan{cred: cred, class: credSkipNoRefreshToken}, nil
	}
	expStr, ok := sec.Data["expires_at"]
	if !ok || len(expStr) == 0 {
		// "Never expires" per RFC 6749 — same semantics as refresh.Run.
		return credPlan{cred: cred, class: credSkipNoExpiry}, nil
	}
	t, perr := time.Parse(time.RFC3339, string(expStr))
	if perr != nil {
		// Treat malformed timestamp as "treat like no-expiry"; we don't
		// keep retrying tight-loop on a bad value. The validity
		// reconciler doesn't validate timestamp format either. Logging
		// is required (AGENTS.md "never silently drop errors") so an
		// operator can debug a Secret that suddenly has a bad value.
		log.FromContext(ctx).Info("refresh: malformed expires_at, treating as no-expiry",
			"credential", cred.Name,
			"secret", ref.Name,
			"value", string(expStr),
			"err", perr.Error())
		return credPlan{cred: cred, class: credSkipNoExpiry}, nil
	}
	if time.Until(t) > threshold {
		return credPlan{cred: cred, class: credSchedule, expiresAt: t}, nil
	}
	return credPlan{cred: cred, class: credAttempt, expiresAt: t}, nil
}

// performRefresh runs an RFC 6749 refresh-token grant via the shared
// pkg/platform/identity/refresh package, mapping its returned error into a
// status reason. Returns (success, reason, message).
func (co *Core) performRefresh(ctx context.Context, ns string,
	cred spiceboxv1alpha1.AgentCredential) (bool, string, string) {
	err := refresh.Run(ctx, co.Client, ns, cred)
	if err == nil {
		return true, "", ""
	}
	if stderrors.Is(err, refresh.ErrDecodeResponse) {
		return false, spiceboxv1alpha1.ReasonRefreshResponseInvalid, err.Error()
	}
	return false, spiceboxv1alpha1.ReasonTokenEndpointError, err.Error()
}

// failureEntry carries per-credential failure context through the
// reconcile loop so the final status-write can pick the first
// failure's reason cleanly.
type failureEntry struct {
	credName string
	reason   string
	message  string
}

// Reconcile is the shared entrypoint, called by each controller's Reconcile
// with an empty CR of its own kind wrapped in an adapter. See the spec for
// the full state machine.
func (co *Core) Reconcile(ctx context.Context, key types.NamespacedName, obj Identity) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues(strings.ToLower(obj.Kind()), key)

	if cont, err := apreconcile.LoadInto(ctx, co.Client, key, obj.Object()); !cont {
		return ctrl.Result{}, err
	}
	if obj.Object().GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	// Snapshot pre-mutation status for MergeFrom patching. Sibling validity
	// reconciler patches non-overlapping status fields concurrently; using
	// Update would 409-loop under Secret-watch retrigger.
	prior := obj.DeepCopyIdentity()

	threshold := co.ThresholdFor(obj)

	// Filter to credentials whose type NEEDS REFRESH (oauth today).
	var oauths []spiceboxv1alpha1.AgentCredential
	for _, c := range obj.Credentials() {
		k, err := credkindregistry.Get(c.Type)
		if err != nil {
			// Unknown type: a wiring bug that already surfaces loudly in the
			// identity controller's SpecInvalid condition. Refusing to refresh
			// it here is the fail-closed answer.
			logger.Info("identityrefresh: skipping credential of unknown type",
				"credential", c.Name, "type", c.Type, "err", err.Error())
			continue
		}
		if k.NeedsRefresh() {
			oauths = append(oauths, c)
		}
	}
	if len(oauths) == 0 {
		conditions.SetTrue(obj.Object(), obj.StatusConditions(),
			obj.ConditionType(), spiceboxv1alpha1.ReasonNoOAuthCredentials)
		return ctrl.Result{}, co.Client.Status().Patch(ctx, obj.Object(), client.MergeFrom(prior.Object()))
	}

	// Classify each oauth credential.
	plans := make([]credPlan, 0, len(oauths))
	for _, c := range oauths {
		p, err := co.classify(ctx, obj, c, threshold)
		if err != nil {
			return ctrl.Result{}, err
		}
		plans = append(plans, p)
	}

	// Run attempts, accumulate outcomes.
	var (
		failures  []failureEntry
		succeeded int
		deferred  int      // credentials due but still inside their backoff
		noRefresh []string // credential names skipped due to missing refresh_token
	)
	now := time.Now()
	// Indexed, not ranged: the loop records each attempt's outcome back onto
	// its plan, which computeRequeueAfter reads below.
	for i := range plans {
		p := &plans[i]
		switch p.class {
		case credSkipNoRefreshToken:
			noRefresh = append(noRefresh, p.cred.Name)
		case credAttempt:
			bkey := BackoffKey(obj.Object().GetNamespace(), obj.Object().GetName(), p.cred.Name)
			// The backoff is enforced HERE, on the attempt, not on the
			// RequeueAfter this reconcile returns. A reconcile arrives from
			// whatever fires first — most often a SIBLING credential's
			// post-refresh Secret write re-firing the Secret watch, which owes
			// this credential's backoff nothing. A credential whose
			// refresh_token the provider revoked stays credAttempt forever (a
			// failed refresh never advances its expires_at), so without this
			// gate every such trigger is another round trip to the provider.
			if remaining := co.Backoff.Remaining(bkey); remaining > 0 {
				p.outcome = attemptDeferred
				deferred++
				logger.Info("refresh deferred: backoff has not elapsed",
					"credential", p.cred.Name, "remaining", remaining.String())
				continue
			}
			ok, reason, msg := co.performRefresh(ctx, obj.SecretNamespace(), p.cred)
			if ok {
				p.outcome = attemptSucceeded
				co.Backoff.RecordSuccess(bkey)
				succeeded++
				logger.Info("refresh succeeded", "credential", p.cred.Name)
			} else {
				p.outcome = attemptFailed
				nextDelay := co.Backoff.RecordFailure(bkey)
				failures = append(failures, failureEntry{credName: p.cred.Name, reason: reason, message: msg})
				logger.Info("refresh failed", "credential", p.cred.Name,
					"reason", reason, "err", msg, "nextDelay", nextDelay.String())
			}
		}
	}

	// Write the Refresh condition + lastRefreshAt.
	switch {
	case len(failures) > 0:
		var combined []string
		for _, f := range failures {
			combined = append(combined, fmt.Sprintf("%s: %s: %s", f.credName, f.reason, f.message))
		}
		conditions.SetFalse(obj.Object(), obj.StatusConditions(),
			obj.ConditionType(), failures[0].reason, strings.Join(combined, "; "))
	case deferred > 0:
		// A deferred credential is a credential still known to be broken; this
		// pass simply declined to re-ask. Leave the condition exactly as the
		// last real attempt left it (False, with that attempt's reason) rather
		// than reporting RefreshSucceeded off a healthy sibling or AllOAuthFresh
		// off nothing — either would tell an operator the identity recovered
		// while it is still failing. With nothing else to record this pass is a
		// status no-op, so it does not churn the object.
		if succeeded > 0 {
			// A sibling did refresh, and lastRefreshAt means "when a refresh
			// last succeeded" — recording it does not contradict the failing
			// condition, and losing it would misreport the identity as staler
			// than it is.
			obj.SetLastRefreshAt(metav1.NewTime(now))
		}
	case succeeded > 0:
		conditions.SetTrue(obj.Object(), obj.StatusConditions(),
			obj.ConditionType(), spiceboxv1alpha1.ReasonRefreshSucceeded)
		obj.SetLastRefreshAt(metav1.NewTime(now))
	case len(noRefresh) > 0:
		conditions.SetTrue(obj.Object(), obj.StatusConditions(),
			obj.ConditionType(), spiceboxv1alpha1.ReasonNoRefreshToken)
	default:
		conditions.SetTrue(obj.Object(), obj.StatusConditions(),
			obj.ConditionType(), spiceboxv1alpha1.ReasonAllOAuthFresh)
	}
	if !equality.Semantic.DeepEqual(prior.StatusSnapshot(), obj.StatusSnapshot()) {
		if err := co.Client.Status().Patch(ctx, obj.Object(), client.MergeFrom(prior.Object())); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: co.computeRequeueAfter(plans, threshold, obj)}, nil
}

// computeRequeueAfter returns the min delay across schedule and backoff
// candidates. Zero means "no future RequeueAfter needed" (no oauth cred
// can ever schedule — e.g. all SkipNoRefreshToken / SkipNoExpiry).
//
// Every candidate is > 0 by construction. That invariant is the whole point:
// expiresAt-threshold is ≤ 0 for ANY credAttempt (classify only produces one
// when expires_at is already inside the threshold), so a candidate derived
// from it always wins the min and clamps the whole result to MinRequeueFloor —
// silently discarding a sibling credential's backoff. The schedule is
// advisory in either case (the attempt loop enforces the backoff itself), but
// a schedule that ignores it wakes the operator 20× more often than the
// steady state it computed.
func (co *Core) computeRequeueAfter(plans []credPlan, threshold time.Duration, obj Identity) time.Duration {
	var candidates []time.Duration
	for _, p := range plans {
		switch p.class {
		case credSchedule:
			// > 0 by construction: classify emits credSchedule only when
			// time.Until(expiresAt) > threshold.
			candidates = append(candidates, time.Until(p.expiresAt)-threshold)
		case credAttempt:
			if p.outcome == attemptFailed || p.outcome == attemptDeferred {
				// Failed or deferred: the Secret is untouched either way, so
				// its expiresAt is still inside the threshold and the backoff
				// remainder is the only honest candidate. Remaining, not
				// NextDelay: a deferred credential is partway through a delay
				// already running, and asking for the full delay again would
				// push its retry out on every unrelated trigger.
				bkey := BackoffKey(obj.Object().GetNamespace(), obj.Object().GetName(), p.cred.Name)
				d := co.Backoff.Remaining(bkey)
				if d <= 0 {
					// Reachable two ways: a tracker built with a zero cap
					// reports no delay, and a deferred credential's remainder
					// can round to zero between the gate and here. Retry at the
					// floor rather than contribute nothing — an empty candidate
					// set returns 0, which would drop a failing credential out
					// of the schedule entirely.
					d = MinRequeueFloor
				}
				candidates = append(candidates, d)
				continue
			}
			// Successful attempt: refresh.Run has written a new expires_at to
			// the Secret, but plans were computed BEFORE that write, so this
			// plan's expiresAt is stale and its expiresAt-threshold is the ≤ 0
			// value described above. The Secret watch re-triggers reconcile
			// when the new value lands and recomputes the real schedule; this
			// candidate is only the backstop for that watch not firing, so it
			// is the floor, spelled out. (Numerically identical to the stale
			// arithmetic once the final clamp applies — stated as a constant so
			// no ≤ 0 value can enter the min and poison a sibling's backoff.)
			candidates = append(candidates, MinRequeueFloor)
		}
	}
	if len(candidates) == 0 {
		return 0
	}
	min := candidates[0]
	for _, c := range candidates[1:] {
		if c < min {
			min = c
		}
	}
	if min < MinRequeueFloor {
		min = MinRequeueFloor
	}
	return min
}

// BackoffKey is the per-credential backoff key. The namespace segment is the
// identity's own namespace, empty for a cluster-scoped kind.
func BackoffKey(ns, identityName, credName string) string {
	return ns + "/" + identityName + "/" + credName
}
