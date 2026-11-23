// pkg/web/webui/sessions/list.go joins the sessions a subject may interact with
// (SpiceDB's answer, via LookupInteractableSessions) against their Kubernetes
// objects (class, phase, title, start time) to build the sidebar list GET
// /sessions and GET /sessions/api/sessions both render. buildSessionList's doc
// gives the join direction and why it is not symmetric.
package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// maxListedSessions bounds the lookup and therefore the Get fan-out. webd's
// Kubernetes client is DIRECT, not cache-backed, so every joined row is one
// API-server round trip on a browser page load. Reaching this cap is reported
// (listNotices.Truncated), never silently applied.
const maxListedSessions = 200

// listGetConcurrency bounds the parallel Gets, keeping a full page load to a
// small constant number of concurrent API-server requests rather than up to
// maxListedSessions of them; the join is latency-bound, not CPU-bound.
const listGetConcurrency = 8

// sessionRow is one session as the sidebar renders it. Every field is read
// from the AgentSession the lookup authorized — nothing here is derived from a
// browser-supplied value.
type sessionRow struct {
	Ns   string `json:"ns"`
	Name string `json:"name"`
	// Class is spec.class — carried rather than only a display name because
	// startableClassesFor derives the start path's authorized set from it.
	Class string `json:"class"`
	// Title is the AgentClass's displayName when set, else the class name.
	// Never the session name: the sidebar shows what the agent IS.
	Title string `json:"title"`
	// Phase is human copy, not status.phase. The raw phase is control-plane
	// vocabulary and must not reach the browser.
	Phase string `json:"phase"`
	// AwaitingHuman is true for the four Awaiting* phases. It drives the row's
	// marker AND the shell's cross-session approval notice.
	AwaitingHuman bool `json:"awaitingHuman"`
	// Ended is true for Succeeded/Failed — this row reports PHASE only, never
	// the wake ladder. agentui.ResolveSession applies the same phase test but
	// only after consulting WakeEligible, so the two diverge for an
	// archive-swept session (Succeeded, yet wakeable): badged "Ended" here,
	// resolved as asleep there.
	//
	// The divergence is accepted, because the SELECTION is authoritative and
	// this row is not: viewFor (view.go) re-runs agentui.ResolveSession over
	// the live object on every selection and never reads this field, so such a
	// session still opens as resumable. A stale badge misleads nobody about
	// what they can act on; an extra per-row WakeEligible check would cost
	// every row a condition to serve a value nothing downstream reads.
	Ended bool `json:"ended"`
	// StartedAt is status.startedAt, else creationTimestamp; UTC; nil if unset.
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	UID            string     `json:"uid,omitempty"`
	GoalCreated    bool       `json:"goalCreated,omitempty"`
	OpeningSummary string     `json:"openingSummary,omitempty"`
}

// listNotices is every way the list is incomplete. Each field is rendered as
// its own line; none is silently swallowed. Zero values mean "complete", and a
// complete list renders no notice at all.
type listNotices struct {
	// Unavailable counts sessions the viewer holds interact on that could not
	// be loaded: a Kubernetes object that is gone, a Get that failed, or an
	// object id that is not "<ns>/<name>". One count rather than three,
	// because the viewer's situation is identical in all three; the operator
	// log distinguishes them.
	Unavailable int `json:"unavailable"`
	// Truncated reports the list is the first maxListedSessions, not all.
	Truncated bool `json:"truncated"`
	// BootstrapUnavailable reports that the start set's SECOND arm — the
	// platform#start_session enumeration — could not be evaluated, so the
	// startable classes are the derived ones ALONE and may be missing entries
	// the viewer is entitled to.
	//
	// Carried for the reason Unavailable and Truncated are: it makes an
	// incomplete set distinguishable from a complete empty one, which is what
	// stops startHandler turning a SpiceDB blip into "you may not start this
	// agent". Never a reason to ALLOW anything — it only forbids reading an
	// absent class as a denial.
	BootstrapUnavailable bool `json:"bootstrapUnavailable"`
}

// maxListedClasses bounds the startable-class lookup. Unlike maxListedSessions
// it bounds no Kubernetes fan-out — the classes are read with one List per
// startable namespace regardless — so it exists purely to keep a pathological
// tuple set from becoming an unbounded response. Reaching it is reported as an
// incomplete set, never silently applied: a short agent picker that reads as
// complete is how a viewer concludes an agent does not exist.
const maxListedClasses = 500

// startableClass is one (namespace, class) pair the viewer may start a session
// for. Two arms produce it, and BOTH are authorization rather than a display
// filter over it:
//
//   - The derived arm: a class the viewer already holds interact on a session
//     of. The derivation itself is the authorization — holding standing on a
//     session of that class is what makes offering another instance safe.
//   - The bootstrap arm (bootstrapStartableClasses): an AgentClass enumeration
//     bounded to this webd's startable namespaces, for a viewer holding
//     platform#start_session. It exists solely because the derived arm is
//     empty for everyone on a cluster with no sessions yet, which would make
//     the browser start control permanently unreachable.
//
// The arms are unioned, never substituted: holding the permission never
// narrows what a viewer's own sessions already entitle them to.
type startableClass struct {
	Ns    string `json:"ns"`
	Class string `json:"class"`
	// Title is the class's displayName when set, else the class name.
	Title string `json:"title"`
	// Startable reports whether THIS webd can create a session in this pair's
	// namespace (browserstart.StartableIn): this server's static start
	// namespaces (StartableNamespaces) UNION the viewer's own Ready workshops
	// (WorkshopNamespacesFor) — a fact about the server, not the viewer, so it
	// never narrows the standing that put the pair in the list.
	//
	// The entry is KEPT when false, not dropped: a viewer's standing usually
	// comes from a session started in a channel's namespace, so unreachable
	// pairs are the common case on a shared cluster, and silently hiding
	// agents the viewer demonstrably has access to trades a dead-end control
	// for an unexplained absence. The shell marks these entries, refuses to
	// submit one, and says why (NewSessionDialog).
	Startable bool `json:"startable"`
	// OffersUI reports that this class declares an agent-defined view
	// (spec.agentUI), so starting it should open that view rather than a
	// transcript, and the picker should not demand an opening message.
	//
	// A third independent fact alongside standing and reachability: it is
	// about what the AGENT is. Without it, a viewer discovers a dashboard
	// agent's shape by starting a session and being handed a message box and
	// an empty transcript instead of the thing they came for.
	//
	// Best-effort: a class this server cannot read reports false and gets the
	// transcript-shaped flow, which works for every agent. Failing the whole
	// list over one unreadable class would be a far worse trade.
	OffersUI bool `json:"offersUI"`
}

// errEmptySubject reports that the authenticated subject decoded to an empty
// canonical id. Unreachable in production traffic — both callers of
// buildSessionList run behind webui.Server's auth middleware, which has
// already put a non-empty subject into context — but the sentinel lets them
// render a diagnosable 401 rather than panicking on a nil canonical or folding
// this into the generic 500 a real SpiceDB/Kubernetes failure gets.
var errEmptySubject = errors.New("sessions: empty canonical subject")

// buildSessionList joins the sessions the subject may interact with against
// their Kubernetes objects.
//
// The lookup is authoritative: a row exists only for an id the lookup
// returned. Joining the other way — listing AgentSessions and filtering —
// would turn a filter bug into an authorization leak, and would need a
// cluster-wide list grant webd deliberately does not hold. There is
// deliberately no client.Client.List call anywhere in this file.
//
// include widens the list by exactly one session past the (MinimizeLatency)
// lookup's answer, for the case where the lookup has not yet caught up with a
// just-written started_by. buildSessionList does NOT authorize include: the
// CALLER MUST have already confirmed the viewer may interact with it via a
// fully-consistent CheckInteract. It is the one parameter that can widen the
// list past the per-subject lookup filter, so passing a browser-supplied id
// without that check first turns this into the authorization-leak direction
// buildSessionList otherwise refuses.
//
// EXACTLY ONE caller supplies one: shellPageBuild (page.go), for the
// `?session=` selection it has already gated, from the same (ns, name) the
// gate was asked about. sessionsAPIHandler passes nil. A third caller may only
// be added with its own gate in front of it; there is no ambient
// authorization here to inherit.
//
// fullyConsistent selects the lookup's consistency: the SIDEBAR passes false
// (see the call comment below), the START GATE true, because a stale positive
// on a gate would admit a subject whose access was just revoked. A parameter
// rather than two functions, so the set the gate authorizes against and the
// set the sidebar renders can never be computed two different ways.
//
// The reverse anomaly — an AgentSession whose started_by write never landed,
// so the viewer holds no interact on it — is NOT detectable from a
// subject-filtered read: absence from the lookup is indistinguishable from
// "not authorized". Detecting it needs a cluster-wide AgentSession list and
// the RBAC change that would require.
func buildSessionList(ctx context.Context, d Deps, subject string,
	include *spicedb.SessionRef, fullyConsistent bool,
) ([]sessionRow, listNotices, error) {
	// CanonicalUserID fails closed on a subject that is not "user:"-prefixed
	// (ErrNonUserSubject) rather than passing a non-user subject through as if
	// it were a canonical id. It cannot catch one case: a bare "user:" decodes
	// to a valid, EMPTY canonical with no error — hence the explicit empty
	// check alongside it.
	canonical, cerr := identity.Subject(subject).CanonicalUserID()
	if cerr != nil || canonical.IsZero() {
		return nil, listNotices{}, errEmptySubject
	}

	// The sidebar's callers pass fullyConsistent=false (MinimizeLatency): a row
	// one refresh stale is cosmetic, because every door this list links to
	// re-checks interact fully-consistently before serving anything. The one
	// user-visible staleness — a session the viewer just started — is closed
	// by the include union below, not by paying full consistency on every
	// poll. The start gate is the one caller that passes true.
	res, err := d.LookupInteractableSessions(ctx, canonical, maxListedSessions, fullyConsistent)
	if err != nil {
		// Any lookup error is an error page, never an empty list — an empty
		// result is indistinguishable from "you have no sessions", the exact
		// silent-failure shape this repo forbids. The caller (shellPageBuild /
		// sessionsAPIHandler) turns this into a *webui.PageError.
		return nil, listNotices{}, fmt.Errorf("sessions: lookup interactable sessions: %w", err)
	}

	notices := listNotices{
		Unavailable: len(res.Unrepresentable) + len(res.Conditional),
		Truncated:   res.Truncated,
	}
	for _, id := range res.Unrepresentable {
		d.Logger().Info("sessions: lookup returned an object id that does not split as \"<ns>/<name>\"; dropping from the list",
			"id", id, "subject", subject)
	}
	for _, id := range res.Conditional {
		d.Logger().Info("sessions: lookup returned a conditional (undetermined) permission; treating as unavailable rather than a grant",
			"id", id, "subject", subject)
	}

	refs := append([]spicedb.SessionRef(nil), res.Refs...)
	if include != nil {
		already := false
		for _, r := range refs {
			if r == *include {
				already = true
				break
			}
		}
		if !already {
			refs = append(refs, *include)
		}
	}

	rows := make([]*sessionRow, len(refs))
	var mu sync.Mutex // guards notices.Unavailable and classTitleCache below
	classTitleCache := make(map[classKey]string)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listGetConcurrency)
	for i, r := range refs {
		g.Go(func() error {
			var sess spiceboxv1alpha1.AgentSession
			if err := d.K8s().Get(gctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, &sess); err != nil {
				mu.Lock()
				notices.Unavailable++
				mu.Unlock()
				if apierrors.IsNotFound(err) {
					d.Logger().Info("sessions: lookup-authorized session has no Kubernetes object; dropping from the list",
						"ns", r.Namespace, "name", r.Name, "subject", subject)
				} else {
					d.Logger().Info("sessions: get AgentSession failed; dropping from the list",
						"ns", r.Namespace, "name", r.Name, "subject", subject, "err", err.Error())
				}
				// Never abort the group: one unreadable session must not blank
				// the rest of the dashboard.
				return nil
			}

			// A DELEGATED CHILD is machinery, not a conversation, and never
			// belongs in a human's session list. It is here only because the
			// lookup is honest: a child inherits its parent's started_by (see
			// startedByAnnotations in the subagentrequest controller), so the
			// person who started the root really does hold interact on every
			// descendant. That grant is deliberate — it is what makes the tree
			// auditable and stoppable by the person accountable for it — so the
			// fix is presentational, NOT a narrowing of the permission.
			//
			// Left unfiltered, one delegating turn buries the session the human
			// is actually in under a row per child. Nothing the child renders is
			// addressed to a person either: its surface is an `agent` Channel
			// reaching its parent, and anything genuinely human-directed climbs
			// the lineage to the root instead of stopping here.
			if sess.Spec.Parent != nil {
				return nil
			}

			title := resolveClassTitle(gctx, d, &mu, classTitleCache, sess.Namespace, sess.Spec.Class)
			label, awaitingHuman, ended := phaseLabel(sess.Status.Phase)

			// .UTC() is not cosmetic: metav1.Time's UnmarshalJSON converts to
			// the process's LOCAL zone, so leaving the zone as read would bake
			// the API-server host's TZ into a browser-facing timestamp — a
			// value that differs across replicas or a re-deploy elsewhere.
			var startedAt *time.Time
			switch {
			case sess.Status.StartedAt != nil:
				t := sess.Status.StartedAt.Time.UTC()
				startedAt = &t
			case !sess.CreationTimestamp.IsZero():
				t := sess.CreationTimestamp.Time.UTC()
				startedAt = &t
			}

			rows[i] = &sessionRow{
				Ns: sess.Namespace, Name: sess.Name, Class: sess.Spec.Class, Title: title,
				Phase: label, AwaitingHuman: awaitingHuman, Ended: ended, StartedAt: startedAt,
				UID: string(sess.UID), GoalCreated: sess.Spec.GoalExecution != nil,
			}
			if sess.Spec.GoalExecution != nil {
				rows[i].OpeningSummary = sess.Spec.OpeningSummary
			}
			return nil
		})
	}
	_ = g.Wait() // every goroutine records its own failure; none returns a non-nil error

	out := make([]sessionRow, 0, len(refs))
	for _, r := range rows {
		if r != nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := time.Time{}, time.Time{}
		if out[i].StartedAt != nil {
			ti = *out[i].StartedAt
		}
		if out[j].StartedAt != nil {
			tj = *out[j].StartedAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].Ns+"/"+out[i].Name < out[j].Ns+"/"+out[j].Name
	})

	return out, notices, nil
}

// classKey caches resolveClassTitle's Get by (namespace, class) rather than
// per session, so N sessions of one class cost one AgentClass Get.
type classKey struct{ ns, class string }

// resolveClassTitle resolves one row's Title. A failed or missing AgentClass
// Get is NOT counted into notices.Unavailable: the session is still fully
// usable with its class name as a fallback title, unlike a session whose OWN
// Get failed.
//
// The cache is intentionally not single-flight: two goroutines racing on the
// same (ns, class) miss can both issue a Get. That is bounded (at most
// listGetConcurrency extra Gets, never proportional to the session count) and
// harmless — but the WRITE below must never let a failed Get's fallback
// clobber an already-resolved displayName, or every later row of that class
// downgrades to the fallback too, and startableClassesFor then disagrees with
// the sidebar about the same class's title depending on which row sorted
// first. The cache only ever remembers a GOOD answer.
func resolveClassTitle(ctx context.Context, d Deps, mu *sync.Mutex, cache map[classKey]string, ns, class string) string {
	key := classKey{ns: ns, class: class}

	mu.Lock()
	if title, ok := cache[key]; ok {
		mu.Unlock()
		return title
	}
	mu.Unlock()

	title := class
	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: class}, &ac); err != nil {
		d.Logger().Info("sessions: get AgentClass failed; falling back to the class name as the row title",
			"ns", ns, "class", class, "err", err.Error())
	} else if ac.Spec.DisplayName != "" {
		title = ac.Spec.DisplayName
	}

	mu.Lock()
	cur, ok := cache[key]
	if shouldWriteClassTitle(cur, ok, class) {
		cache[key] = title
	}
	mu.Unlock()
	return title
}

// shouldWriteClassTitle decides whether a resolveClassTitle call may write its
// title into the shared cache, given what is already there for the same key.
// A pure function so the decision is directly unit-testable: resolveClassTitle's
// cache-hit fast path makes this write unreachable on a second call once a
// value is cached, so a black-box test that calls it twice cannot exercise the
// decision at all (TestShouldWriteClassTitle).
//
// Only cache a fallback when nothing better is already there — a sibling
// goroutine's successful resolution must never be overwritten by this one's
// failure. A later successful resolution may still upgrade a fallback.
func shouldWriteClassTitle(alreadyCached string, ok bool, class string) bool {
	return !ok || alreadyCached == class
}

// phaseLabel maps status.phase to the row's user-facing copy — the raw phase
// is control-plane vocabulary and must never reach the browser.
// awaitingHuman is true for exactly the four Awaiting* phases; ended for
// exactly Succeeded/Failed. Idle and WakeEligible are different facts: this
// reports the phase only (Idle -> "Asleep"), while the selected session's own
// origin disclosure reports the wake ladder's branch.
func phaseLabel(phase string) (label string, awaitingHuman, ended bool) {
	switch phase {
	case "", spiceboxv1alpha1.AgentSessionPhasePending, spiceboxv1alpha1.AgentSessionPhaseRunning:
		return "Active", false, false
	case spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice:
		return "Waiting for you", true, false
	case spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry:
		return "Retrying", false, false
	case spiceboxv1alpha1.AgentSessionPhaseIdle:
		return "Asleep", false, false
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.AgentSessionPhaseFailed:
		return "Ended", false, true
	default:
		// An unrecognized phase — a future addition this mapping has not been
		// updated for — must still not leak the raw control-plane string.
		return "Unknown", false, false
	}
}

// authorizedStartableClasses is the START GATE's derivation of the
// (namespace, class) pairs the viewer may begin a session for. It re-runs the
// SAME join the sidebar does — so the set the gate authorizes against and the
// set the shell offers can never be computed two different ways — but FULLY
// CONSISTENTLY, because a stale positive on a gate would admit a subject whose
// access was just revoked.
//
// The list the browser was offered is NOT an input. Whatever class the request
// names is checked against this freshly-recomputed set, so a browser that
// invents a pair, replays an old one, or edits the one it was given gets the
// same answer as one naming a class it never had standing on.
//
// The notices are returned, NOT discarded, and the caller MUST consult them
// before turning an absent pair into a refusal: buildSessionList drops any row
// whose AgentSession Get failed and reports the count rather than the cause,
// so an incomplete authorized set is indistinguishable by content from a
// complete one. Reading the set alone turns a transient Kubernetes fault (or a
// truncated lookup) into a definitive "you may not start this" — the same
// fail-into-denial the SpiceDB half of this join already refuses one layer up.
// See startHandler's incomplete-list branch.
func authorizedStartableClasses(ctx context.Context, d Deps, subject string) ([]startableClass, listNotices, error) {
	rows, notices, err := buildSessionList(ctx, d, subject, nil, true /* fullyConsistent: this is a gate */)
	if err != nil {
		return nil, listNotices{}, err
	}
	classes, notices := startableClassesFor(ctx, d, subject, rows, notices, true /* gate */)
	return classes, notices, nil
}

// classOffersUI reports whether an AgentClass declares an agent-defined view.
//
// It answers about the DECLARATION only, never about whether that view is
// currently resolvable: a class whose AgentUI is missing or Valid=False still
// reports true, because the honest outcome is to open the agent view and show
// why it is unavailable (the disclosure selectedView.OffersAgentUI already
// makes for an open session). Reporting false would route the viewer to a
// transcript and quietly drop the fact that a view was meant to exist.
//
// A read failure is false, logged rather than returned: this is a hint that
// shapes a dialog, and the transcript flow it falls back to works for every
// agent. Propagating the error would fail an entire session list over one
// unreadable class.
func classOffersUI(ctx context.Context, d Deps, ns, class string) bool {
	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: class}, &ac); err != nil {
		if !apierrors.IsNotFound(err) {
			d.Logger().Info("sessions: could not read an agent class to tell whether it declares a UI; offering the transcript flow",
				"ns", ns, "class", class, "err", err.Error())
		}
		return false
	}
	return ac.Spec.AgentUI != nil
}

// startableClassesFor derives the distinct (Ns, Class) pairs over rows — the
// sessions the viewer is ALREADY authorized to interact with — never from an
// AgentClass enumeration. That derivation IS the authorization for starting a
// new session: a class appearing only on a Kubernetes session the lookup never
// named is correctly absent here too.
//
// d supplies the SECOND, independent fact each pair carries: whether this webd
// can create in that namespace at all (startableClass.Startable). Two facts,
// not one — standing is the viewer's, reachability is the server's — and
// folding them together would either hide agents the viewer holds standing on
// or let the start gate inherit a server-capability answer as an
// authorization one.
func startableClassesFor(ctx context.Context, d Deps, subject string, rows []sessionRow,
	in listNotices, fullyConsistent bool,
) ([]startableClass, listNotices) {
	static := d.StartableNamespaces()
	// Resolved ONCE per call, here, rather than left to browserstart.StartableIn
	// to re-resolve per row: WorkshopNamespacesFor is a cluster-wide Workshop
	// List, and `add` below runs once per distinct (ns, class) pair, which
	// over several sessions in several non-static namespaces would otherwise
	// mean one List per pair — up to N+1 per poll per open tab. A lookup
	// error is logged HERE, once, and treated as an empty dynamic set: the
	// static arm alone still answers correctly for every row, just narrower.
	owned, ownedErr := d.WorkshopNamespacesFor(ctx, subject)
	if ownedErr != nil {
		d.Logger().Info("sessions: could not resolve the viewer's own workshops; the startable set uses only the static namespaces",
			"subject", subject, "err", ownedErr.Error())
		owned = nil
	}

	seen := make(map[classKey]bool, len(rows))
	out := make([]startableClass, 0, len(rows))
	add := func(ns, class, title string) {
		key := classKey{ns: ns, class: class}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, startableClass{
			Ns: ns, Class: class, Title: title,
			// StartableInSet, not StartableIn: static/owned are already
			// resolved above, so this is a pure slice check with no lookup —
			// see this function's own doc for why re-resolving per row would
			// be wrong here even though it is exactly right for Start's
			// single-namespace door.
			Startable: browserstart.StartableInSet(static, owned, ns),
			OffersUI:  classOffersUI(ctx, d, ns, class),
		})
	}
	for _, r := range rows {
		add(r.Ns, r.Class, r.Title)
	}

	// The bootstrap arm is unioned on TOP of the derived one, never replacing
	// it: a viewer holding start_session still sees every class their own
	// sessions entitle them to, including in namespaces this webd cannot
	// create in (which the enumeration below deliberately does not scan).
	notices := in
	boot, err := bootstrapStartableClasses(ctx, d, subject, fullyConsistent, static, owned)
	if err != nil {
		// NOT a denial and NOT a silent narrowing. The set really is missing
		// these entries, so it is returned as-is with the flag set, and
		// startHandler reads the flag to answer 503 rather than "you may not
		// start this" — the same rule the Unavailable/Truncated arms follow.
		notices.BootstrapUnavailable = true
		d.Logger().Error(err, "sessions: could not evaluate the platform start_session arm; "+
			"the startable set is the derived classes alone and may be incomplete",
			"subject", subject)
	}
	for _, c := range boot {
		add(c.Ns, c.Class, c.Title)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Ns != out[j].Ns {
			return out[i].Ns < out[j].Ns
		}
		return out[i].Class < out[j].Class
	})
	return out, notices
}

// bootstrapStartableClasses is the start set's SECOND arm: the AgentClasses
// this viewer holds agentclass#start_session on, intersected with the classes
// that actually exist in a namespace this webd can create in.
//
// It exists because the derived arm cannot bootstrap: standing on an existing
// session is self-authorizing, but on a cluster whose first session has never
// been started that derivation is empty for EVERY viewer, leaving the browser
// start control permanently unreachable — a cycle no amount of per-session
// authorization can break.
//
// The join runs SpiceDB-first, Kubernetes-second, the direction
// buildSessionList uses and for the same reason: SpiceDB answers what the
// viewer may do, Kubernetes what exists and what to call it. Neither answer
// alone is the set — a tuple whose class was deleted must not appear, and
// neither must a class the viewer cannot start.
//
// Two deliberate bounds:
//
//   - The permission asked is agentclass#start_session, never
//     platform#start_session directly, even though the platform arm is the
//     only populated one today. It resolves through `platform->start_session`,
//     so granting one team one agent later (agentclass#starter) is a
//     relationship write this code already honours. Asking the platform
//     question directly would make the two separate gates that drift apart.
//   - The result is bounded to static UNIONED with owned — the caller's
//     already-resolved StartableNamespaces() and WorkshopNamespacesFor(subject),
//     the same two arms StartableInSet checks. An entry outside both could
//     never be started by this webd, so listing one would add a control
//     whose every press fails at the apiserver — the dead end
//     startableClass.Startable exists to MARK for the derived arm, which
//     cannot narrow itself the same way because a viewer's standing is not
//     this server's to filter.
//
// static and owned are PASSED IN rather than derived here: both are resolved
// once by startableClassesFor (its own doc explains why — a cluster-wide
// Workshop List must not be re-issued per distinct namespace), and this is
// the other of the two places that resolution feeds. A lookup error is
// already logged by that caller; this function never sees it, only the
// (possibly narrowed-to-static-only) result.
//
// A viewer holding nothing gets (nil, nil): the ordinary case, not an error —
// the derived arm alone is the correct answer for them.
func bootstrapStartableClasses(ctx context.Context, d Deps, subject string,
	fullyConsistent bool, static, owned []string,
) ([]startableClass, error) {
	// Union, deduplicated, static namespaces first: a workshop namespace that
	// happens to already be in the static set must not be scanned twice.
	allNS := append([]string(nil), static...)
	inNS := make(map[string]bool, len(static))
	for _, ns := range static {
		inNS[ns] = true
	}
	for _, ns := range owned {
		if !inNS[ns] {
			inNS[ns] = true
			allNS = append(allNS, ns)
		}
	}
	if len(allNS) == 0 {
		// Nowhere to create, so nothing to enumerate — and no reason to spend
		// a SpiceDB round trip proving it.
		return nil, nil
	}
	// Same fail-closed decode buildSessionList uses: a non-user subject must
	// never reach SpiceDB as if it were a canonical user id, and the empty
	// check stays alongside it because a bare "user:" decodes to a valid,
	// EMPTY canonical with no error.
	canonical, cerr := identity.Subject(subject).CanonicalUserID()
	if cerr != nil || canonical.IsZero() {
		return nil, errEmptySubject
	}
	res, err := d.LookupStartableClasses(ctx, canonical, maxListedClasses, fullyConsistent)
	if err != nil {
		return nil, fmt.Errorf("lookup startable classes: %w", err)
	}
	if res.Truncated {
		// Reported as an error so the caller flags the set incomplete rather
		// than serving a short picker that reads as complete — the same rule
		// the session list's own Truncated notice follows.
		return nil, fmt.Errorf("%w: the startable-class lookup returned its first %d results",
			errIncompleteStandingList, maxListedClasses)
	}
	// Carried out of the lookup rather than dropped: agentclass ids ARE
	// validated on the write side, so either of these means a tuple this
	// operator did not write, or a schema that changed under the lookup.
	if len(res.Unrepresentable) > 0 || len(res.Conditional) > 0 {
		d.Logger().Info("sessions: the startable-class lookup returned ids it could not use; those agents are absent from the picker",
			"subject", subject, "unrepresentable", len(res.Unrepresentable), "conditional", len(res.Conditional))
	}

	// allNS already IS the allowed-namespace set (inNS, built above); reused
	// directly rather than rebuilt, since it is exactly "the namespaces this
	// enumeration is bounded to."
	allowed := make(map[classKey]bool, len(res.Refs))
	for _, ref := range res.Refs {
		if inNS[ref.Namespace] {
			allowed[classKey{ns: ref.Namespace, class: ref.Name}] = true
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}

	var out []startableClass
	for _, ns := range allNS {
		var list spiceboxv1alpha1.AgentClassList
		if err := d.K8s().List(ctx, &list, client.InNamespace(ns)); err != nil {
			// One namespace failing must not silently shrink the set: the
			// viewer IS entitled to these classes, so this is the incomplete
			// case, reported to the caller rather than swallowed.
			return nil, fmt.Errorf("list AgentClasses in %q: %w", ns, err)
		}
		for i := range list.Items {
			ac := &list.Items[i]
			if !allowed[classKey{ns: ac.Namespace, class: ac.Name}] {
				continue
			}
			title := ac.Name
			if ac.Spec.DisplayName != "" {
				title = ac.Spec.DisplayName
			}
			// OffersUI reads straight off the object already in hand — no
			// second Get, unlike the derived arm which only has (ns, class).
			out = append(out, startableClass{
				Ns: ac.Namespace, Class: ac.Name, Title: title,
				OffersUI: ac.Spec.AgentUI != nil,
			})
		}
	}
	return out, nil
}

// sessionsAPIResponse is GET /sessions/api/sessions' whole body — built by the
// SAME buildSessionList + startableClassesFor call shellPageBuild's first paint
// makes (page.go), so the initial render and every poll agree on what "the
// list" means.
type sessionsAPIResponse struct {
	// Sessions is the sidebar list, newest start first.
	Sessions []sessionRow `json:"sessions"`
	// Notices is how the list is incomplete; zero values mean complete.
	Notices listNotices `json:"notices"`
	// StartableClasses is what the viewer may start; empty means no control.
	StartableClasses []startableClass `json:"startableClasses"`
}

// sessionsAPIHandler serves GET /sessions/api/sessions: the viewer's own
// session list, as JSON. Mounted under AuthAuthenticated (sessions.go's
// Routes), which guarantees webui.SubjectFromContext is non-empty here — same
// reachability note as errEmptySubject above.
func sessionsAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subject := webui.SubjectFromContext(r.Context())
		rows, notices, err := buildSessionList(r.Context(), d, subject, nil, false /* MinimizeLatency: a sidebar read, not a gate */)
		if err != nil {
			d.Logger().Error(err, "sessions: list API failed to build the session list; returning 500",
				"subject", subject)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			if encErr := json.NewEncoder(w).Encode(map[string]string{
				"error": "Could not load your sessions. Try again in a moment.",
			}); encErr != nil {
				d.Logger().Info("sessions: list API error-body encode failed", "subject", subject, "err", encErr.Error())
			}
			return
		}

		classes, notices := startableClassesFor(r.Context(), d, subject, rows, notices, false /* poll */)

		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(sessionsAPIResponse{
			Sessions: rows, Notices: notices, StartableClasses: classes,
		}); encErr != nil {
			// Headers are already sent (200 + Content-Type) by the time
			// Encode can fail mid-stream, so there is no status left to
			// correct — log the cause rather than drop it silently.
			d.Logger().Info("sessions: list API response encode failed", "subject", subject, "err", encErr.Error())
		}
	}
}
