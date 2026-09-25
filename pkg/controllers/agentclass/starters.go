package agentclass

import (
	"context"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// StarterLinker makes agentclass:<ns>/<name>#starter hold exactly the given
// subjects. Satisfied by *spicedb.Client. Same contract as PlatformLinker for a
// name SpiceDB cannot express: report spicedb.ErrUnrepresentableObjectID so the
// reconciler stops retrying and says so on the CR.
type StarterLinker interface {
	EnsureAgentClassStarters(ctx context.Context, ns, name string, subjects []string) error
}

// wellFormedStarters returns the entries of allowedStarters that SpiceDB's
// agentclass#starter relation can actually hold, and the first offending entry
// with its error. The write uses the well-formed subset; validation reports the
// rest (validateStartGate, as SpecInvalid).
//
// Dropping a bad entry from the write is not tidiness, it is the whole point.
// EnsureAgentClassStarters sends the set as ONE atomic WriteRelationships:
// SpiceDB rejects the entire batch on the first subject its schema cannot hold,
// and that takes the DELETEs down with it — so a starter REMOVED from the list
// would silently keep its standing while the class still read Valid=True.
func wellFormedStarters(entries []string) (ok []string, bad string, badErr error) {
	for _, s := range entries {
		if err := validateStarterShape(s); err != nil {
			if badErr == nil {
				bad, badErr = s, err
			}
			continue
		}
		ok = append(ok, s)
	}
	return ok, bad, badErr
}

// validateStarterShape admits EXACTLY the two subject shapes the SpiceDB
// schema declares for the relation this list is written to
// (`relation starter: user | group#member`, schema.zed):
//
//   - "user:<id>" with NO "#relation", where <id> is canonical-shaped
//     (identity.IsCanonicalUserIDShape);
//   - "group:<id>#member" — the relation must be exactly "member".
//
// Everything else is refused HERE, before the write, because the write cannot
// refuse it usefully: it is one atomic batch, so a subject the schema cannot
// hold fails the adds AND the deletes together (see wellFormedStarters).
// authz.ValidateSubjectSet alone is far too permissive for this field — it
// accepts "group:eng" with no relation, "group:eng#owner", and
// "user:abc123#member", none of which the starter relation can hold.
//
// The canonical-id rule is the second half, and it is about matching rather
// than shape: subjectRE's id charset admits '@' and '.', so a hand-authored
// "user:alice@example.com" is syntactically fine and can never match anything
// CheckAgentClassStart resolves — that check always runs against an
// identity.CanonicalUserID, always the platform's own base64url encoding — so
// such an entry would be written and then silently never satisfy anyone.
func validateStarterShape(s string) error {
	if err := authz.ValidateSubjectSet(s, authz.SubjectUser, authz.SubjectGroup); err != nil {
		return fmt.Errorf("%w; use exactly %q (no #relation) or %q", err, "user:<canonical id>", "group:<id>#member")
	}
	if id, isUser := strings.CutPrefix(s, "user:"); isUser {
		if strings.Contains(id, "#") {
			return starterShapeError(s)
		}
		if !identity.IsCanonicalUserIDShape(id) {
			return stderrors.New("user ids must be canonical (the base64url form the platform mints), never a raw email or external id")
		}
		return nil
	}
	rest, isGroup := strings.CutPrefix(s, "group:")
	if !isGroup {
		return starterShapeError(s)
	}
	id, relation, hasRelation := strings.Cut(rest, "#")
	if id == "" || !hasRelation || relation != "member" {
		return starterShapeError(s)
	}
	return nil
}

// starterShapeError names the offending entry and both accepted shapes, so an
// operator reading `kubectl describe agentclass` can fix it without reading
// the schema.
func starterShapeError(s string) error {
	return fmt.Errorf("%q is not a shape agentclass#starter can hold; use exactly %q (no #relation) or %q",
		s, "user:<canonical id>", "group:<id>#member")
}

// workshopOwnerStarter returns the "user:<canonical>" subject of the person
// who owns the workshop ac was authored in, or "" when ac's namespace is not
// a workshop. The tie is the namespace's own labels (the Workshop controller
// stamps LabelWorkshopSessionNamespace/Name on the namespace it provisions),
// then the Workshop CR named for that session, whose spec.starterCanonical is
// exactly "the canonical id of the person who started the builder session".
//
// This is what lets a person start the agent they are building, as
// themselves, from the browser chat: agentclass#starter is a tuple, never a
// spec field, so an installed copy of the class never carries it. A workshop
// namespace whose Workshop is gone (teardown in flight) yields "" with a log
// line rather than an error: the class is about to go with the namespace,
// and failing its reconcile would only make that noisier.
//
// Both Gets use the uncached reader (APIReader-or-Client, same idiom as
// controller.go's ConfigMap/Secret adopt reads), never r.Client directly.
// No controller in this operator watches Namespace, so an r.Client.Get here
// would be the first-ever cached read of it — the shared informer cache
// starts a standing, cluster-wide Namespace watch lazily on first use, which
// this one reconcile path has no business provisioning. A stale cache entry
// would also matter here: a namespace whose workshop labels just landed (or
// a Workshop that just appeared) needs to be seen on the very next
// reconcile, not after an informer resync.
func (r *Reconciler) workshopOwnerStarter(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) (string, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: ac.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get namespace %s: %w", ac.Namespace, err)
	}
	sessNS, sessName := ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace], ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName]
	if sessNS == "" || sessName == "" {
		return "", nil
	}
	var ws spiceboxv1alpha1.Workshop
	key := types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := reader.Get(ctx, key, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("agentclass in a workshop namespace whose Workshop is gone; linking no owner starter",
				"agentclass", ac.Namespace+"/"+ac.Name, "workshop", key.String())
			return "", nil
		}
		return "", fmt.Errorf("get workshop %s: %w", key, err)
	}
	if ws.Spec.StarterCanonical == "" {
		return "", nil
	}
	return "user:" + ws.Spec.StarterCanonical, nil
}

// ensureStarterLinks writes the class's starter set, on every reconcile and
// BEFORE the validation gates, for the same reason ensurePlatformLink does: an
// invalid class is exactly the one an admin may need to start to diagnose it.
// An empty set is written too — that is how a removed allowlist loses standing.
//
// The written set is declared allowedStarters UNIONED with the workshop
// owner (workshopOwnerStarter): a class authored inside a workshop namespace
// is startable by the person building it without any spec field naming them,
// so an installed copy of the class never inherits that standing.
func (r *Reconciler) ensureStarterLinks(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) error {
	declared := ac.Spec.GetAuthz().GetSession().AllowedStarters
	subjects, _, _ := wellFormedStarters(declared)
	owner, err := r.workshopOwnerStarter(ctx, ac)
	if err != nil {
		log.FromContext(ctx).Info("resolving the workshop owner for agentclass#starter failed; requeuing",
			"agentclass", ac.Namespace+"/"+ac.Name, "err", err.Error())
		return err
	}
	if owner != "" && !slices.Contains(subjects, owner) {
		subjects = append(subjects, owner)
	}
	if r.StarterLinker == nil {
		if len(subjects) > 0 {
			log.FromContext(ctx).Info("StarterLinker not configured; skipping agentclass#starter — allowedStarters can never be satisfied for this class, so every session of it will be refused",
				"agentclass", ac.Namespace+"/"+ac.Name, "allowedStarters", len(subjects))
		}
		return nil
	}
	err = r.StarterLinker.EnsureAgentClassStarters(ctx, ac.Namespace, ac.Name, subjects)
	switch {
	case err == nil:
		reason := spiceboxv1alpha1.ReasonStartersLinked
		if len(subjects) == 0 {
			reason = spiceboxv1alpha1.ReasonNoAllowedStarters
		}
		conditions.SetTrue(ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionStartersLinked, reason)
		return nil
	case stderrors.Is(err, spicedb.ErrUnrepresentableObjectID):
		log.FromContext(ctx).Info("this AgentClass's name cannot be expressed as a SpiceDB object id, so agentclass#starter can never be written; every session of it will be refused. NOT retrying",
			"agentclass", ac.Namespace+"/"+ac.Name, "err", err.Error())
		conditions.SetFalse(ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionStartersLinked,
			spiceboxv1alpha1.ReasonUnrepresentableClassName, err.Error())
		return nil
	default:
		log.FromContext(ctx).Info("agentclass#starter write failed; requeuing",
			"agentclass", ac.Namespace+"/"+ac.Name, "err", err.Error())
		conditions.SetFalse(ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionStartersLinked,
			spiceboxv1alpha1.ReasonStartersLinkFailed, err.Error())
		return err
	}
}

// validateStartGate checks the start-gate fields. Returns (result, true) when
// the class was marked invalid and the caller must return that result.
//
// It READS ac.Status.UserlessInput, so it must run after the Channel walk that
// derives it — see its call site in Reconcile.
func (r *Reconciler) validateStartGate(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) (ctrl.Result, bool, error) {
	s := ac.Spec.GetAuthz().GetSession()
	if _, bad, err := wellFormedStarters(s.AllowedStarters); err != nil {
		res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
			fmt.Sprintf("spec.authz.session.allowedStarters entry %q: %v", bad, err))
		return res, true, rerr
	}
	// A start gate admits only PEOPLE — every arm of agentclass#start_session
	// resolves a user, and EnforceStartGate refuses structurally when a session
	// has no human starter. An input Channel that carries no person (a webhook,
	// a cron) asserts a service subject, so every session it opens would be
	// failed before any pod, forever, on a class that otherwise reads Valid=True.
	//
	// Caught here rather than left to the session because the class is where the
	// contradiction lives and where an operator can fix it. Note that
	// onlyStartersInteract makes EffectiveSessionInteractPermission() non-empty,
	// so userLessChannelMissingAuthz below no longer refuses this combination —
	// this is the only gate that does.
	if len(s.AllowedStarters) > 0 && ac.Status.UserlessInput {
		res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
			"spec.authz.session.allowedStarters is a start gate that admits only human starters, so this class cannot be bound to an input Channel that carries no person: no session it opens could ever be started. Remove the allowlist, or bind a human-attributed input Channel")
		return res, true, rerr
	}
	if len(s.AllowedStarters) == 0 {
		if s.PlatformAdminsMayStart != nil && !*s.PlatformAdminsMayStart {
			res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
				"spec.authz.session.platformAdminsMayStart: false requires a non-empty allowedStarters — with neither arm nobody could ever start this class")
			return res, true, rerr
		}
		if s.OnlyStartersInteract {
			res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
				"spec.authz.session.onlyStartersInteract requires a non-empty allowedStarters")
			return res, true, rerr
		}
	}
	if s.OnlyStartersInteract {
		if s.InteractPermission != "" {
			res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
				"spec.authz.session.onlyStartersInteract and interactPermission are mutually exclusive: the first fixes the interact set to this class's starters")
			return res, true, rerr
		}
		if _, err := spicedb.AgentClassObjectID(ac.Namespace, ac.Name); err != nil {
			res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonSpecInvalid,
				"spec.authz.session.onlyStartersInteract names this class as a SpiceDB subject-set, which its name cannot be: "+err.Error())
			return res, true, rerr
		}
	}
	return ctrl.Result{}, false, nil
}
