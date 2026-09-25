package workshop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The messages a person can read back through the close_others tool. Plain
// words, nothing internal: they are rendered verbatim to whoever asked, the
// same contract expiredNoticeBody keeps.
const (
	closeRefusedNotYours  = "not yours to close"
	closeRefusedSelf      = "the workshop you are in"
	closeNoOtherWorkshops = "no other workshops of yours"
	closeNothingNew       = "nothing new to close: the rest were answered by an earlier ask"
	closeDeadlineMessage  = "not closed: it was still being set up when the time ran out"
)

// closePendingRequeue is how soon a pass that left a close request undecided
// comes back for it.
//
// An undecided request has NO trigger of its own. Nothing watches the thing it
// is waiting on (a target finishing its provisioning), and the requester's own
// Workshop is otherwise reconciled only by the generic requeue at the end of
// Reconcile — up to an hour out. The tool that asked waits thirty seconds and
// then reports what settled, so an hour is the difference between a person
// being told their workshops closed and a person being told to ask again about
// something that had already been decided minutes later.
//
// Five seconds is short against that wait and cheap against the work: a pass
// with nothing left undecided does not shorten anything, so this is bounded to
// the workshops that actually have a request outstanding. How LONG one can be
// outstanding is bounded too — closeDecisionDeadline refuses a target that
// never becomes decidable — so this loop runs for minutes at worst rather than
// for the life of the requester.
const closePendingRequeue = 5 * time.Second

// closeDecisionDeadline is how long a close request may sit undecided before
// its target is refused: ten minutes from the moment the tool asked
// (spec.closeRequests[].requestedAt), which every target a wildcard expands to
// inherits from that ask's own entry.
//
// The undecided window exists because a target that has not written its tuples
// cannot be checked truthfully (see decideCloseTarget), and a set-once refusal
// written inside that window could never be revisited. But a target can stay
// there forever — a namespace still Terminating, an RBAC refusal, anything its
// own reconcile keeps retrying — and that cost the requester a full reconcile
// every closePendingRequeue for the life of the workshop, while the person who
// asked was told only that their answer was still being decided.
//
// Ten minutes is long against provisioning (seconds) and short against a person
// waiting. What it writes is a decision like any other: set once and never
// revisited, for the same reason every other close decision is — a request
// re-decided later could reach through to a DIFFERENT session of the same name.
const closeDecisionDeadline = 10 * time.Minute

// parseCloseTarget reads one spec.closeRequests target into the Workshop it
// names, or reports it as the wildcard.
//
// Three spellings, and the CRD pattern on WorkshopCloseRequest.Target admits
// only these:
//
//   - "*" (and, for a later ask, "*:<n>") is the wildcard fulfilCloseWildcard
//     expands; no key is returned for it.
//   - "namespace/name" is a Workshop in THAT namespace — a cluster running a
//     second sanctioned builder class puts a person's workshops outside the
//     one asking, and the start route's refusal names them that way.
//   - anything else is a bare Workshop name in requesterNS, which is what the
//     tool writes for a workshop in the builder's own namespace.
//
// The key it returns is the identity of the target: "x" asked from namespace
// n and "n/x" asked from anywhere are the same workshop, and two workshops of
// one name in two namespaces are two. Callers key their decided map on
// key.String() for exactly that reason.
func parseCloseTarget(requesterNS, target string) (key types.NamespacedName, wildcard bool) {
	// The wildcard grammar is the apis package's, the same one the apiserver
	// admitted this target under and the same one close_others writes its next
	// ask with.
	if spiceboxv1alpha1.IsWorkshopCloseWildcard(target) {
		return types.NamespacedName{}, true
	}
	if ns, name, ok := strings.Cut(target, "/"); ok {
		return types.NamespacedName{Namespace: ns, Name: name}, false
	}
	return types.NamespacedName{Namespace: requesterNS, Name: target}, false
}

// closeDecidedKey is the key one target's decision is remembered under: the
// canonical namespace/name for a workshop, and the ask's own spelling for a
// wildcard (which names no workshop, and whose summary is its own answer).
func closeDecidedKey(requesterNS, target string) string {
	key, wildcard := parseCloseTarget(requesterNS, target)
	if wildcard {
		return target
	}
	return key.String()
}

// fulfilCloseRequests decides every spec.closeRequests entry that has no
// decision yet, and closes the ones SpiceDB allows.
//
// The decision is the person's, not the session's: the subject is this
// workshop's own starter (spec.starterCanonical), so a builder asking to close
// another workshop asks as the human it acts for. workshop#close resolves to
// that person being the target's starter, or a platform admin — see the
// schema fragment in pkg/authz/spicedb/schema.
//
// Each decision is written ONCE per CANONICAL target and never revised. That is
// load-bearing rather than tidy: closing deletes the target's builder
// AgentSession, so a request re-decided later could reach through to a
// DIFFERENT session that happens to carry the same name — a person's next
// build, killed by a request they made about their last one. A request that
// spells an already-decided target differently ("x", then "<this
// namespace>/x") is answered by COPYING that decision under its own spelling:
// the tool reads its answer back by the target it wrote, so it needs an entry
// of its own, and re-deciding would close a second session.
//
// A target is a Workshop — bare in this namespace, namespace/name elsewhere
// (parseCloseTarget) — or a wildcard ask (WorkshopCloseTargetAll, or the
// sequenced "*:<n>" a person's later ask carries), which fulfilCloseWildcard
// resolves to the person's other live workshops and answers with a summary of
// what that ask decided. Self is refused first, before any lookup or
// expansion: a builder must never close the workshop it is in, and that is true
// whether or not the check would allow it (it would — the starter is the same
// person).
//
// changed reports whether status.closeRequests was mutated, so the caller can
// persist those decisions even on the error return — a target whose session is
// already deleted must not be decided twice.
//
// res carries a short RequeueAfter whenever a request was left undecided, which
// the caller folds into Reconcile's own result (shorter wins), the same way
// reconcileTestWatch's result is folded. See closePendingRequeue for why an
// undecided request needs one at all.
func (r *Reconciler) fulfilCloseRequests(ctx context.Context, ws *spiceboxv1alpha1.Workshop) (res ctrl.Result, changed bool, err error) {
	if len(ws.Spec.CloseRequests) == 0 {
		return ctrl.Result{}, false, nil
	}
	// Two views of what has already been decided, because two questions get
	// asked of it. decided is keyed CANONICALLY and holds the whole entry: a
	// wildcard expansion reads it to skip a target some request already settled
	// — including one THIS ask settled on an earlier held pass, which it must
	// not decide (and so re-delete) twice — while a request in a second
	// spelling needs its message too, to copy. spelled is the literal targets
	// status already carries, which is what says whether a request has an entry
	// of its OWN.
	//
	// decided is SET ONCE per canonical target, in status order, which is the
	// order the decisions were made in. Two entries can share a canonical
	// target — a wildcard ask's decision and the copy a second spelling of it
	// was answered with — and the one that DECIDED the target is the first of
	// them. Letting the copy (which carries no ask) replace it would make the
	// ask that decided the target read it back as somebody else's.
	decided := make(map[string]spiceboxv1alpha1.WorkshopCloseStatus, len(ws.Status.CloseRequests))
	spelled := make(map[string]struct{}, len(ws.Status.CloseRequests))
	remember := func(entry spiceboxv1alpha1.WorkshopCloseStatus) {
		key := closeDecidedKey(ws.Namespace, entry.Target)
		if _, done := decided[key]; !done {
			decided[key] = entry
		}
		spelled[entry.Target] = struct{}{}
	}
	for _, s := range ws.Status.CloseRequests {
		remember(s)
	}

	logger := log.FromContext(ctx)
	// spec.starterCanonical is the bare canonical the AgentSession reconciler
	// recorded at creation; reading it back is not a fresh verification, which
	// is what CanonicalFromTrusted names.
	starter := identity.CanonicalFromTrusted(ws.Spec.StarterCanonical,
		"operator-written spec.starterCanonical on the REQUESTING Workshop")

	// write appends one decision and remembers it under both views. The log
	// line is the caller's: a decision made here and one copied from another
	// spelling are different events, and an operator reading the log needs to
	// see which happened.
	//
	// ask is the wildcard request a decision was made FOR, and is what lets a
	// LATER ask tell the targets it decided from the ones an earlier one did.
	// It is empty for a named request, and empty for a wildcard's own summary:
	// the summary IS its ask, its target says so, and stamping it would put the
	// ask itself among the workshops the tool reports.
	write := func(target, phase, message, ask string) {
		now := metav1.NewTime(r.now())
		entry := spiceboxv1alpha1.WorkshopCloseStatus{
			Target:    target,
			Phase:     phase,
			Message:   message,
			Ask:       ask,
			DecidedAt: &now,
		}
		ws.Status.CloseRequests = append(ws.Status.CloseRequests, entry)
		remember(entry)
		changed = true
	}

	record := func(target, phase, message, ask string) {
		write(target, phase, message, ask)
		logger.Info("workshop close request decided",
			"workshop", ws.Namespace+"/"+ws.Name, "target", target, "phase", phase, "why", message, "ask", ask)
	}

	for _, req := range ws.Spec.CloseRequests {
		key, wildcard := parseCloseTarget(ws.Namespace, req.Target)
		decidedKey := req.Target
		if !wildcard {
			decidedKey = key.String()
		}
		if prior, done := decided[decidedKey]; done {
			if _, own := spelled[req.Target]; own {
				continue
			}
			// The same workshop, asked about in a spelling that has no entry
			// yet. Copy the answer rather than make a second one: nothing is
			// checked, nothing is deleted, and the tool waiting on THIS
			// spelling is answered.
			logger.Info("workshop close request answered from the decision already recorded under another spelling",
				"workshop", ws.Namespace+"/"+ws.Name, "target", req.Target,
				"decidedAs", prior.Target, "phase", prior.Phase)
			// Only a NAMED request reaches here (a wildcard's decided key IS
			// its own spelling), and the entry it gets is that request's own —
			// so it carries no ask, whichever request first decided the target.
			write(req.Target, prior.Phase, prior.Message, "")
			continue
		}

		if !wildcard && key == client.ObjectKeyFromObject(ws) {
			record(req.Target, spiceboxv1alpha1.WorkshopClosePhaseRefused, closeRefusedSelf, "")
			continue
		}

		// An unattributed workshop (spec.starterCanonical empty — a session
		// with no started-by) has no person to ask as. Refuse rather than
		// check: an empty subject id is not a subject, and asking SpiceDB
		// about one would be an error the pass would then retry forever. This
		// is also what stops a wildcard from EXPANDING on nobody's behalf: the
		// expansion matches on starterCanonical, and an empty one would match
		// every other unattributed workshop in the cluster.
		if starter.IsZero() {
			// A wildcard refused here is answered in its own entry, like every
			// other answer to an ask, so it carries no ask of its own either.
			record(req.Target, spiceboxv1alpha1.WorkshopClosePhaseRefused, closeRefusedNotYours, "")
			continue
		}

		if wildcard {
			if werr := r.fulfilCloseWildcard(ctx, ws, starter, req.Target, req.RequestedAt, decided, record); werr != nil {
				return ctrl.Result{}, changed, werr
			}
			continue
		}

		phase, message, decidedNow, derr := r.decideCloseTarget(ctx, ws, starter, key)
		if derr != nil {
			return ctrl.Result{}, changed, derr
		}
		if !decidedNow {
			// Undecided is not free: nothing watches what this request waits
			// on, so the requester is re-reconciled every closePendingRequeue
			// until it resolves. The deadline is what ends that for a target
			// that never will.
			if r.closeDeadlinePassed(ctx, ws, req.Target, req.RequestedAt) {
				record(req.Target, spiceboxv1alpha1.WorkshopClosePhaseRefused, closeDeadlineMessage, "")
			}
			continue
		}
		record(req.Target, phase, message, "")
	}

	// Anything still undecided is waiting on something nothing watches, so this
	// pass asks for its own next look. One line per pass, naming the targets, so
	// an operator reading logs can see WHAT is outstanding rather than only that
	// the workshop is reconciling often.
	var pending []string
	for _, req := range ws.Spec.CloseRequests {
		if _, own := spelled[req.Target]; !own {
			pending = append(pending, req.Target)
		}
	}
	if len(pending) > 0 {
		logger.Info("workshop close requests still undecided; re-checking shortly",
			"workshop", ws.Namespace+"/"+ws.Name, "pending", pending, "in", closePendingRequeue.String())
		return ctrl.Result{RequeueAfter: closePendingRequeue}, changed, nil
	}
	return ctrl.Result{}, changed, nil
}

// fulfilCloseWildcard resolves the one target the sidecar can name without
// being able to list anything — the wildcard, ask — into the person's other
// live workshops, decides the ones no request has settled yet, and then answers
// ask itself with a summary of what IT decided.
//
// Every ask expands AFRESH, which is what makes "close the others" something a
// person may say twice: the second ask sees the workshops they opened since the
// first one. What it must not do is answer for them twice. A target some other
// request already settled — an earlier ask's, or a named request's — is skipped
// whole: not re-checked, not re-deleted, and not counted into this ask's
// summary, which would tell the person their fresh ask closed something it
// never looked at. Decisions are set once per canonical target and never
// revised, so a workshop refused by an earlier ask stays refused for this
// requester; they close it from another builder, or it expires.
//
// An ask left with nothing it may decide is answered NotFound, in the words
// that say WHICH nothing: the person has no other workshops at all
// (closeNoOtherWorkshops), or every one of them was answered already and no
// decision is ever revised (closeNothingNew). The second is not a dead end —
// the answer they want is the one they already have — so it must not read like
// the first.
//
// The summary is HELD until every target THIS ask is deciding has a decision.
// Holding it is what keeps a set-once entry honest: a count frozen while one
// target was still provisioning would both misreport what happened and strand
// that target forever, because this ask is never expanded again. A target this
// same ask decided on an earlier pass is skipped rather than re-made, and the
// counts are read back from the decisions themselves, so a held pass neither
// re-checks nor loses what it already settled.
//
// Each target is recorded under the spelling the person could say back: its
// bare name when it lives in the requester's own namespace, namespace/name when
// it does not, and stamped with ask, which is how the tool reads back the
// answer to its OWN call. The lookup is canonical either way, so a workshop a
// NAMED request already settled is skipped here whichever spelling that request
// used.
//
// The summary lands under ask's OWN spelling rather than a hardcoded "*":
// status.closeRequests is keyed by target, so a sequenced ask ("*:2") answered
// under "*" would collide with the first ask's entry and be refused as a
// duplicate key.
//
// decided is fulfilCloseRequests' canonically-keyed view of what status already
// holds; record is its recorder, passed in so every entry this writes lands on
// the same status list and the same changed flag.
func (r *Reconciler) fulfilCloseWildcard(
	ctx context.Context,
	ws *spiceboxv1alpha1.Workshop,
	starter identity.CanonicalUserID,
	ask string,
	requestedAt metav1.Time,
	decided map[string]spiceboxv1alpha1.WorkshopCloseStatus,
	record func(target, phase, message, ask string),
) error {
	targets, lerr := r.startersOtherWorkshops(ctx, ws)
	if lerr != nil {
		return lerr
	}

	var undecided int
	var skipped []string
	for _, key := range targets {
		if prior, known := decided[key.String()]; known {
			// Already answered. A decision THIS ask made on an earlier held
			// pass is left exactly as it is — the summary below reads it back
			// from status — and one some other request made is somebody else's
			// answer, which this ask neither re-decides nor claims.
			if prior.Ask != ask {
				skipped = append(skipped, prior.Target)
			}
			continue
		}
		// The spelling a person could say back to the tool, which reaches
		// the same workshop this expansion just decided.
		target := key.Name
		if key.Namespace != ws.Namespace {
			target = key.String()
		}
		phase, message, decidedNow, derr := r.decideCloseTarget(ctx, ws, starter, key)
		if derr != nil {
			return derr
		}
		if !decidedNow {
			// Every target this ask expands to inherits the ask's own asking
			// time: the person asked ONCE, so one deadline covers every
			// workshop that ask resolved to. A target that runs out of it is
			// refused like a named one, and counted as refused below, which is
			// what finally lets the held summary be written.
			if !r.closeDeadlinePassed(ctx, ws, target, requestedAt) {
				undecided++
				continue
			}
			phase, message = spiceboxv1alpha1.WorkshopClosePhaseRefused, closeDeadlineMessage
		}
		record(target, phase, message, ask)
	}
	// One line per pass, naming what this ask left alone, so an operator can see
	// WHY a fresh "close everything" answered with a count smaller than the
	// workshops the person still has open.
	if len(skipped) > 0 {
		log.FromContext(ctx).Info("workshop wildcard close skipped targets an earlier request had already decided",
			"workshop", ws.Namespace+"/"+ws.Name, "ask", ask, "skipped", skipped, "expanded", len(targets))
	}
	if undecided > 0 {
		log.FromContext(ctx).Info("workshop wildcard close held: a workshop it expanded to has not finished provisioning, so its answer is not yet knowable",
			"workshop", ws.Namespace+"/"+ws.Name, "ask", ask, "undecided", undecided, "expanded", len(targets))
		return nil
	}

	// The summary counts what this ask DECIDED, read back from the decisions
	// themselves, rather than what its expansion holds now. Closing a target
	// deletes its builder session, and its Workshop carries that session's
	// owner reference, so the workshop this ask closed on an earlier held pass
	// is garbage-collected and gone from the expansion by the time the summary
	// can be written. Counting the current expansion would under-report the
	// ask's own work — "1 closed" beside a result list of two.
	closed, refused, mine := 0, 0, 0
	for _, s := range ws.Status.CloseRequests {
		if s.Ask != ask {
			continue
		}
		mine++
		switch s.Phase {
		case spiceboxv1alpha1.WorkshopClosePhaseClosed:
			closed++
		case spiceboxv1alpha1.WorkshopClosePhaseRefused:
			refused++
		}
	}
	if mine == 0 {
		// Two kinds of nothing, and the person can act on only one of them:
		// nothing else is open at all, or everything that is open was already
		// answered — by an earlier ask or by a named request — and a decision
		// is never revised, so asking again cannot produce a new one.
		nothing := closeNoOtherWorkshops
		if len(skipped) > 0 {
			nothing = closeNothingNew
		}
		record(ask, spiceboxv1alpha1.WorkshopClosePhaseNotFound, nothing, "")
		return nil
	}
	record(ask, spiceboxv1alpha1.WorkshopClosePhaseClosed,
		fmt.Sprintf("%d closed, %d refused", closed, refused), "")
	return nil
}

// closeDeadlinePassed reports whether an undecided close request has run out of
// time, and says so once in the log when it has — the pass that gets true here
// records the refusal, and a set-once decision is never asked about again.
//
// It is only ever asked about a request THIS pass could not decide: a target
// whose tuples are standing is answered by SpiceDB however long it took to get
// there, so the deadline can refuse a workshop that is still being set up and
// nothing else.
//
// requestedAt is the request's own asking time, which the targets a wildcard
// expands to inherit from that ask's entry — the deadline belongs to the ask,
// not to each workshop the ask resolved to, and a later ask brings its own.
//
// A zero requestedAt is already past it. The tool always stamps one, so only a
// hand-written request lacks it, and a deadline that cannot be measured has to
// fail in the direction that can only refuse and cannot loop.
func (r *Reconciler) closeDeadlinePassed(ctx context.Context, ws *spiceboxv1alpha1.Workshop, target string, requestedAt metav1.Time) bool {
	logger := log.FromContext(ctx)
	if requestedAt.IsZero() {
		logger.Info("workshop close request refused: it carries no requestedAt, so the wait for its target could not be bounded",
			"workshop", ws.Namespace+"/"+ws.Name, "target", target)
		return true
	}
	if r.now().Before(requestedAt.Add(closeDecisionDeadline)) {
		return false
	}
	logger.Info("workshop close request refused: its target was still being set up when the decision deadline ran out",
		"workshop", ws.Namespace+"/"+ws.Name, "target", target,
		"requestedAt", requestedAt.UTC().Format(time.RFC3339), "deadline", closeDecisionDeadline.String())
	return true
}

// startersOtherWorkshops lists the live Workshops ACROSS EVERY NAMESPACE that
// belong to the same person as ws — every one the wildcard resolves to. The
// requester is excluded (a builder never closes the workshop it is in) and so is
// anything already being torn down, which would otherwise be counted as a close
// this pass did not perform.
//
// The cluster, not the namespace, because that is the population the person is
// measured against: both halves of the cap — the start route's refusal
// (pkg/web/webui/sessions' workshopCapRefusal) and the operator's own
// workshopCapExceeded — count a starter's live workshops cluster-wide. Bounding
// the expansion to one namespace would let the refusal name workshops the only
// tool the person has could never reach, on any cluster running a second
// sanctioned builder class. Reaching them costs no authority: each expanded
// target is still decided by its own SpiceDB check, and each is ended through
// its own spec.session reference.
//
// A NAMED request reaches just as far, and says so: its target may carry the
// namespace its workshop lives in (parseCloseTarget), so the person can close
// by name any workshop this expansion could have reached.
func (r *Reconciler) startersOtherWorkshops(ctx context.Context, ws *spiceboxv1alpha1.Workshop) ([]types.NamespacedName, error) {
	var list spiceboxv1alpha1.WorkshopList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("close request from Workshop %s/%s: list workshops to expand %q: %w",
			ws.Namespace, ws.Name, spiceboxv1alpha1.WorkshopCloseTargetAll, err)
	}
	var keys []types.NamespacedName
	for i := range list.Items {
		other := &list.Items[i]
		// Namespace AND name: across the cluster, a workshop sharing this
		// one's name in another namespace is a different workshop, not this
		// one seen twice.
		if (other.Namespace == ws.Namespace && other.Name == ws.Name) || other.DeletionTimestamp != nil {
			continue
		}
		if other.Spec.StarterCanonical != ws.Spec.StarterCanonical {
			continue
		}
		keys = append(keys, client.ObjectKeyFromObject(other))
	}
	// Sorted so the decisions a pass records, and the counts it summarizes, do
	// not depend on list order.
	slices.SortFunc(keys, func(a, b types.NamespacedName) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return keys, nil
}

// decideCloseTarget answers one close target: whether the person may end it
// and, when they may, ending it. decided=false means the answer is not knowable
// yet and nothing was recorded — the target has not finished provisioning, so
// the tuples a check reads do not exist.
//
// Ordering, on purpose:
//
//   - a missing target is NotFound, not Refused. It is already gone; saying
//     "not yours" about someone else's absent workshop would leak more than
//     it explains.
//   - a check ERROR is never a refusal. It returns, so the pass retries: an
//     unreachable SpiceDB recorded as Refused would permanently answer "not
//     yours to close" to a person about their own workshop.
func (r *Reconciler) decideCloseTarget(ctx context.Context, ws *spiceboxv1alpha1.Workshop, starter identity.CanonicalUserID, key types.NamespacedName) (phase, message string, decided bool, err error) {
	var found spiceboxv1alpha1.Workshop
	if gerr := r.Client.Get(ctx, key, &found); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return spiceboxv1alpha1.WorkshopClosePhaseNotFound, "", true, nil
		}
		return "", "", false, fmt.Errorf("close request from Workshop %s/%s: get target %s: %w", ws.Namespace, ws.Name, key, gerr)
	}

	// The target's OWN tuples are what a close check reads, and
	// WorkshopConditionTupleWritten is the only thing that says they are
	// standing: Reconcile anchors status.namespace at step 3 and does not write
	// the tuples until step 7, so in that window the target HAS a namespace and
	// has no #starter tuple, and a check would deny a person their own
	// workshop. Leave it undecided until both are true — the tool reports what
	// settled and says the rest is still being decided, which is true — rather
	// than writing a set-once refusal that could never be revisited. The id is
	// re-read beside the condition because it is what the check names.
	//
	// The wait this opens is bounded by the CALLER, not here: a target still in
	// this window at closeDecisionDeadline is refused in plain words
	// (closeDeadlinePassed), because a workshop can stay here forever.
	workshopID := found.Status.Namespace
	if workshopID == "" || !apimeta.IsStatusConditionTrue(found.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten) {
		log.FromContext(ctx).Info("workshop close request left undecided: the target's close tuples are not standing yet",
			"workshop", ws.Namespace+"/"+ws.Name, "target", key.String())
		return "", "", false, nil
	}

	// A nil Tuples dependency denies nothing and allows nothing: it means
	// this process cannot answer, which is the error case, not a refusal.
	// (Reconcile's step 7 already fails closed on it before this pass
	// runs; the guard is here so a direct caller cannot skip it.)
	if r.Tuples == nil {
		return "", "", false, fmt.Errorf("close request from Workshop %s/%s: Reconciler.Tuples is not wired; cannot decide who may close %s", ws.Namespace, ws.Name, key)
	}
	allowed, cerr := r.Tuples.CheckWorkshopClose(ctx, workshopID, starter)
	if cerr != nil {
		return "", "", false, fmt.Errorf("close request from Workshop %s/%s: check close on %s: %w", ws.Namespace, ws.Name, key, cerr)
	}
	if !allowed {
		return spiceboxv1alpha1.WorkshopClosePhaseRefused, closeRefusedNotYours, true, nil
	}

	// Deleting the target's builder session is what ends the target
	// workshop: the Workshop carries that session's owner reference, so
	// the cascade deletes the CR and its finalizer runs the ordinary
	// teardown. Nothing here touches the target's namespace directly.
	sessKey := types.NamespacedName{Namespace: found.Spec.Session.Namespace, Name: found.Spec.Session.Name}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name}}
	if derr := r.Client.Delete(ctx, sess); derr != nil && !apierrors.IsNotFound(derr) {
		return "", "", false, fmt.Errorf("close request from Workshop %s/%s: delete target session %s: %w", ws.Namespace, ws.Name, sessKey, derr)
	}
	return spiceboxv1alpha1.WorkshopClosePhaseClosed, "", true, nil
}

// releaseIfFinished ends a workshop whose builder session is over: the person
// is done, so the build space stops standing (and stops counting against their
// maxWorkshopsPerStarter cap).
//
// Terminal is Succeeded or Failed — Idle is not, because a channel-attached
// session parks there between turns and wakes again.
//
// An install request the builder made HOLDS the release until it is decided:
// admind installs from this workshop's namespace, so tearing it down while an
// admin still has the card in front of them would fail the install with
// nothing to point at. A workshop that requested nothing (hand-off, or a
// person who just stopped) has nothing to wait for.
//
// Releasing mirrors sweepExpiry (teardown.go): stamp the standing reason on
// the workshop, persist it, then Delete the CR so the existing finalizer
// teardown reverses every layer. handled=true means the caller returns
// (res, err) immediately; handled=false means "not finished — carry on
// provisioning".
//
// sess is the builder session Reconcile just read. A session that is GONE is
// finished too, but it does not arrive here: Reconcile's step 1 returns first
// for a ghost workshop, whose CR owner-ref GC is already removing — the same
// end state this produces, reached by the cascade instead.
func (r *Reconciler) releaseIfFinished(ctx context.Context, ws *spiceboxv1alpha1.Workshop, sess *spiceboxv1alpha1.AgentSession) (handled bool, res ctrl.Result, err error) {
	if sess == nil || !sessionFinished(sess.Status.Phase) {
		return false, ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	// status.install is non-nil inside this branch: installDecided answers true
	// for a workshop that never requested an install.
	if !installDecided(ws.Status.Install) {
		logger.Info("workshop release held: the builder session finished but its install request is still open",
			"workshop", ws.Namespace+"/"+ws.Name, "session", ws.Spec.Session.Namespace+"/"+ws.Spec.Session.Name,
			"installPhase", ws.Status.Install.Phase)
		return false, ctrl.Result{}, nil
	}

	msg := fmt.Sprintf("builder session %s/%s finished (%s)", ws.Spec.Session.Namespace, ws.Spec.Session.Name, sess.Status.Phase)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReleased
	ws.Status.ObservedGeneration = ws.Generation
	conditions.SetFalse(ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionNamespaceReady, spiceboxv1alpha1.ReasonWorkshopSessionFinished, msg)
	if uerr := r.Client.Status().Update(ctx, ws); uerr != nil {
		return true, ctrl.Result{}, fmt.Errorf("persist released Workshop %s/%s status: %w", ws.Namespace, ws.Name, uerr)
	}
	logger.Info("workshop released", "workshop", ws.Namespace+"/"+ws.Name,
		"session", ws.Spec.Session.Namespace+"/"+ws.Spec.Session.Name, "why", msg)
	if derr := r.Client.Delete(ctx, ws); derr != nil && !apierrors.IsNotFound(derr) {
		return true, ctrl.Result{}, fmt.Errorf("delete released Workshop %s/%s: %w", ws.Namespace, ws.Name, derr)
	}
	return true, ctrl.Result{}, nil
}

// sessionFinished reports whether a builder session has reached a phase it
// never comes back from.
func sessionFinished(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// installDecided reports whether an install request (if one was ever made) has
// reached a phase nobody is waiting on. Requested and Approved are still open:
// Approved means an admin said yes and the install has not landed yet.
func installDecided(in *spiceboxv1alpha1.WorkshopInstallStatus) bool {
	if in == nil {
		return true
	}
	switch in.Phase {
	case spiceboxv1alpha1.WorkshopInstallPhaseInstalled,
		spiceboxv1alpha1.WorkshopInstallPhaseDeclined,
		spiceboxv1alpha1.WorkshopInstallPhaseFailed:
		return true
	default:
		return false
	}
}
