// pkg/web/webui/sessions/start_workshopcap.go holds the start route's
// per-starter builder-workshop cap: the refusal that happens BEFORE anything
// is created, and the sentence that names the workshops the person already
// has open.
//
// The operator enforces the same ceiling from the other side
// (pkg/controllers/agentsession's ensureWorkshop), and still must: a session
// can be started by a channel, a trigger, or the CLI, none of which pass
// through here. But by the time that gate runs the AgentSession exists, so
// the person who pressed Start lands on a session that boot-failed and is
// told only that it ended. Refusing here is what turns that into an answer
// they can act on.
//
// Both gates read the SAME facts through the SAME accessors — the cluster
// sanction (SettingsLimits.BuilderClassFor), the class's own sidecar ref
// (AgentClass.ReferencesSidecarToolbox), the ceiling
// (SettingsLimits.MaxWorkshopsPerStarterOrDefault) and the copy
// (v1alpha1.WorkshopLimitBody) — so this route cannot refuse a start the
// operator would have allowed, nor admit one it will immediately kill.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The two states a person reads beside each of their open workshops. Plain
// words: a phase string ("AwaitingDecision") is operational vocabulary, and
// what the person needs to know is only whether the thing is still working.
const (
	workshopStateInProgress = "in progress"
	workshopStateFinished   = "finished"
)

// errEmptyWorkshopCapSubject is the error value returned when the signed-in
// subject canonicalizes to nothing. It mirrors internal/cmd/webd's
// errEmptyWorkshopSubject, and for the same reason: Workshop's
// spec.starterCanonical is optional, so an empty id would match every
// workshop some other bug left it unset on. Counting those against this
// viewer, or starting anyway, are both wrong — the caller answers 503.
var errEmptyWorkshopCapSubject = errors.New("sessions: workshop cap check with an empty canonical subject")

// workshopCapRefusal decides whether this (namespace, class) start must be
// refused because the viewer is already at their builder-workshop ceiling.
//
// Three answers, and the difference between the last two is the whole point:
//
//   - ("", nil): start. Either this class makes no workshop, or the viewer is
//     under the ceiling.
//   - (msg, nil): refuse with msg — the viewer IS at the ceiling, and msg
//     names the workshops they can close to get under it.
//   - ("", err): indeterminate. The caller answers 503, never a refusal and
//     never a start: telling someone with no workshops that they are at a
//     limit is a lie, and starting a session the operator will boot-fail
//     seconds later is the failure this route exists to replace.
//
// The caller has ALREADY proven the viewer holds standing on (ns, class), so
// the AgentClass read below is not a class-existence oracle.
func workshopCapRefusal(ctx context.Context, d Deps, ns, class, subject string) (string, error) {
	canonical, err := identity.Subject(subject).CanonicalUserID()
	if err != nil {
		return "", err
	}
	if canonical.IsZero() {
		return "", errEmptyWorkshopCapSubject
	}

	// The CLUSTER tier alone, exactly as ensureWorkshop reads it: a namespace
	// tenant must not be able to sanction itself for a workshop, so the
	// namespace tier is not consulted here at all.
	var cas spiceboxv1alpha1.ClusterAgentSettings
	if err := d.K8s().Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas); err != nil {
		if apierrors.IsNotFound(err) {
			// No cluster tier means no sanction, and no sanction means the
			// operator creates no workshop for any class — so there is nothing
			// to cap. The same answer ensureWorkshop reaches from a nil spec.
			return "", nil
		}
		return "", fmt.Errorf("workshopCapRefusal: get ClusterAgentSettings %q: %w", spiceboxv1alpha1.ClusterAgentSettingsName, err)
	}

	sanction := cas.Spec.Limits.BuilderClassFor(ns, class)
	if sanction == nil {
		return "", nil // not a builder class: it costs this viewer no workshop
	}

	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: class}, &ac); err != nil {
		if apierrors.IsNotFound(err) {
			// A sanction naming a class that is not there creates no workshop.
			// The start itself is refused moments later by browsersession.Create,
			// with the copy that keeps class existence unenumerable — this
			// function must not pre-empt it with a different answer.
			return "", nil
		}
		return "", fmt.Errorf("workshopCapRefusal: get AgentClass %s/%s: %w", ns, class, err)
	}
	if !ac.ReferencesSidecarToolbox(sanction.SidecarToolbox) {
		// Sanctioned by name, but the class wires no such sidecar in, so the
		// operator would create no workshop either.
		return "", nil
	}

	var list spiceboxv1alpha1.WorkshopList
	if err := d.K8s().List(ctx, &list); err != nil {
		return "", fmt.Errorf("workshopCapRefusal: list workshops: %w", err)
	}
	var open []*spiceboxv1alpha1.Workshop
	for i := range list.Items {
		w := &list.Items[i]
		// LIVE workshops of THIS person only. A workshop being deleted has
		// already stopped counting for the operator, and telling someone to
		// close one that is on its way out is advice they cannot act on.
		if w.Spec.StarterCanonical != canonical.String() || w.DeletionTimestamp != nil {
			continue
		}
		open = append(open, w)
	}
	// Refused once the count REACHES the ceiling, not once it passes: nothing
	// has been created yet, so the session this request would start is the one
	// that would take it over. ensureWorkshop counts to the same ceiling with
	// the session's own workshop excluded, which is this rule seen from the
	// other side of the create.
	if len(open) < int(cas.Spec.Limits.MaxWorkshopsPerStarterOrDefault()) {
		return "", nil
	}

	// One spelling for the whole list, decided by the list (workshopCapName).
	qualify := slices.ContainsFunc(open, func(w *spiceboxv1alpha1.Workshop) bool { return w.Namespace != ns })
	entries := make([]string, 0, len(open))
	for _, w := range open {
		entries = append(entries, workshopCapName(w, qualify)+" ("+workshopStateFor(ctx, d, w)+")")
	}
	// Sorted by what the person reads, so the order does not depend on List's.
	slices.Sort(entries)
	return workshopLimitMessage(entries), nil
}

// workshopCapName is what the refusal calls one workshop, and it is chosen to
// be what workshop_close_others takes back: the builder SESSION's name, with
// the workshop's namespace in front when qualify says the list needs it.
//
// The count behind this refusal is cluster-wide (a person's workshops can sit
// in any sanctioned builder class's namespace), so a bare name is not always
// enough to reach one. A far workshop named without its namespace would send
// the person to close something in their own namespace that is not the
// workshop they were told about, or nothing at all.
//
// qualify is decided over the WHOLE list, not per workshop, because a bare
// name resolves in the namespace of whichever builder the person opens — and
// this sentence is what they choose from, so nothing here knows which that
// will be. All at home: every name stays bare, and every one of them resolves
// wherever they open. One of them elsewhere: every name carries its namespace,
// the ones at home included, so the list reads the same from any of them.
func workshopCapName(w *spiceboxv1alpha1.Workshop, qualify bool) string {
	if !qualify {
		return w.Spec.Session.Name
	}
	return w.Namespace + "/" + w.Spec.Session.Name
}

// workshopStateFor reports whether a workshop's builder session is still
// working, in the two words a person needs to choose which one to close.
//
// It never drops a workshop from the answer. A session that cannot be read is
// reported as still in progress and logged: the list is what the person is
// told to go and close, and an omission there is a workshop they will never
// find while the cap keeps refusing them. A session that is GONE is reported
// finished — there is nothing left of it to be in progress.
func workshopStateFor(ctx context.Context, d Deps, w *spiceboxv1alpha1.Workshop) string {
	var sess spiceboxv1alpha1.AgentSession
	key := client.ObjectKey{Namespace: w.Spec.Session.Namespace, Name: w.Spec.Session.Name}
	if err := d.K8s().Get(ctx, key, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return workshopStateFinished
		}
		d.Logger().Info("sessions start: could not read a workshop's builder session; reporting it as still open",
			"ns", key.Namespace, "name", key.Name, "workshop", w.Namespace+"/"+w.Name, "err", err.Error())
		return workshopStateInProgress
	}
	switch sess.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.AgentSessionPhaseFailed:
		return workshopStateFinished
	default:
		// Running, Idle, Pending, "" and every Awaiting* phase: the session is
		// alive and its workshop is one the person can still open and use.
		return workshopStateInProgress
	}
}

// workshopLimitMessage composes the refusal: the shared limit sentence, the
// workshops that are open right now, and the one action that clears them.
//
// The names are workshopCapName's — the builder session's, every one of them
// namespace-qualified as soon as any workshop in the list sits outside the
// namespace being started in — which is exactly what the
// workshop_close_others tool accepts, so the sentence's instruction can be
// followed verbatim by the agent the person opens.
//
// An empty list degrades to the limit sentence alone rather than emitting
// "Open now: ." — reachable only for a ceiling of zero or less, which the CRD
// does not forbid.
func workshopLimitMessage(entries []string) string {
	if len(entries) == 0 {
		return spiceboxv1alpha1.WorkshopLimitBody
	}
	return spiceboxv1alpha1.WorkshopLimitBody + " Open now: " + strings.Join(entries, ", ") +
		". Open one of them and ask it to close the others."
}
