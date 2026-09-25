// Package workshopthreadsrv exposes the ONE operator route that lets an
// agent-builder workshop's builder session learn who else has already worked
// the conversation thread it was just pulled into (plan 9a, Task 1):
//
//	GET /workshop/agents-in-thread   →   [{"namespace","name","class","role"}, ...]
//
// # Ruling — a narrow tuple-authorized route, not a broadened bearer
//
// (The same ruling workshoptranscriptsrv and workshopdraftsrv document for
// their own routes.) The workshop sidecar's operator bearer is registered
// (plan 2) keyed to the SYNTHETIC {builderSessionNamespace,
// WorkshopName(builderSessionName)} pair — never to the builder session
// {B, X} itself — so LookupInfo alone proves only "this bearer belongs to
// some workshop", nothing about which thread it may enumerate. Every other
// piece of identity this route needs is derived, never accepted from the
// caller: the route takes no path parameter and no request body at all.
//
//  1. authenticate the bearer (LookupInfo) to learn which Workshop CR it
//     belongs to;
//  2. resolve THAT Workshop CR and read spec.session (B/X, the builder) from
//     it — never from the URL, which carries no session identity to begin
//     with;
//  3. require the Workshop to be Ready;
//  4. re-check workshop:<W>#build@agentsession:<B>/<X> against SpiceDB,
//     FullyConsistent, moments before the read — the same tuple
//     workshopdraftsrv re-verifies before a write, reused here as a
//     freshness gate: a torn-down or reassigned workshop must not go on
//     answering "who's in my thread" after it has lost standing over W;
//  5. read X's OWN AgentSession (namespace B) to learn its channel
//     bindings, and derive the shared thread's anchor from them
//     ("<channel_id>:<thread_ts>", mirroring
//     pkg/channels/channelsd/pipeline's threadAnchor);
//  6. List sessions in B scoped to X's own channel identity
//     (MatchingLabels{LabelChannelName, LabelChannelKey} built from
//     whichever of X's own bindings produced the anchor — never a bare
//     namespace List), UNION a second List on LabelOutputChannelKey alone
//     (mirroring pipeline.go's correlateSessions fallback, for a peer bound
//     to the anchor only via its own OUTPUT channel — a cron- or
//     output-anchored session), and keep only the ones that genuinely bind
//     that SAME anchor (thread_owner.go's bindsAnchor ruling: a hashed key
//     indexes the List efficiently, but the anchor — not the key — is what
//     proves shared membership, because a DM key is only meaningful within
//     one Channel);
//  7. for each such thread participant, walk its OWN delegation closure
//     (agentsession_closure.DescendantsOf) to pick up every subagent that
//     participant has EVER delegated to — its whole subtree, all-time, with
//     no anchor or time filter. A participant that also ran ten unrelated
//     tasks contributes all ten: a "descendant" row is therefore WEAKER
//     evidence than a "participant" row — useful for seeing what a
//     participant is capable of delegating to, not proof any of it happened
//     on this thread (step 8's dedup keeps "participant" as the win when a
//     session is both, for exactly this reason). DescendantsOf returns strict
//     descendants only, so the participant itself is added by hand, and it
//     resolves the participant's own tree root first, so it is correct
//     whether the participant is itself a root or a non-root delegated
//     session. A single member's ancestor walk failing (a dangling
//     Spec.Parent — subagentrequest's delegate.go creates no
//     OwnerReference from the SubagentRequest to the PARENT AgentSession,
//     only to the child, so deleting/reaping an intermediate session
//     orphans a child whose Spec.Parent names a session that no longer
//     exists) does not fail the whole request: that participant's
//     descendants are skipped and
//     logged, the participant itself is still emitted, and every other
//     participant's own walk proceeds independently;
//  8. emit {namespace, name, class, role}, deduped across both roles
//     (a participant that is ALSO a descendant of some other participant is
//     emitted once, as a participant — the more specific fact), stopping the
//     walk once MaxAgentsInThread rows have accumulated, EXCLUDING the
//     builder session X itself — X is the one asking, not a participant to
//     reproduce.
//
// Every step denies closed: a missing/invalid bearer, an unresolvable or
// not-Ready Workshop, a nil or erroring build checker, and a false tuple all
// refuse the read. A bearer can therefore never be asked — and can never be
// tricked — into enumerating a thread beyond its own builder session's, and
// never a namespace at large.
//
// # Why spec.session is safe to read even though the sidecar can patch it
//
// The Workshop CR's own sidecar Role grants update/patch on this exact
// object, spec.session included — so spec.session is not trusted-by-
// construction the way status.Namespace is (operator-only, set once at
// admission and never rewritten). A forged spec.session only changes WHICH
// session workshopID's build tuple is asked about; it can never change
// workshopID itself, which the sidecar cannot write. The route therefore
// reduces to "does the session named in the (possibly forged) spec field
// hold workshop:<W>#build for the workshop id the operator itself
// provisioned" — the same shape workshopdraftsrv's own package doc argues for
// its write path, reused here unchanged. As defense-in-depth on top of that,
// ServeHTTP also refuses outright whenever spec.session names a namespace
// other than the Workshop CR's own (ws.Spec.Session.Namespace != ws.Namespace):
// a legitimately-provisioned Workshop never diverges (WorkshopNamespacesFor's
// own doc — a builder session's Workshop always lives in root.Namespace), so
// a patched cross-namespace reference is refused before it ever reaches the
// tuple check, mirroring the same hardening class
// subagentrequest/controller.go already applies to sr.Spec.Parent.Namespace.
//
// # Why this is not a cluster-wide enumeration
//
// Every List call here is BOTH namespace-scoped (to B, the builder's own
// namespace — never cluster-wide) AND label-scoped: the first two Lists key
// on X's own channel Name+Key (or output key alone), the closure walk keys
// on one participant's own delegation-tree root. MaxAgentsInThread stops the
// PARTICIPANT loop from walking further closures once that many rows have
// accumulated — bounding how many participants' closures get walked in one
// request, not the cost of any single participant's own walk already under
// way (see MaxAgentsInThread's own doc for that distinction). The guarantee
// is against a pathological tree compounding ACROSS participants, not a
// bound on total work from any one of them.
//
// # Why no ReadTranscriptChecker-shaped check, and no memory.Memory at all
//
// workshoptranscriptsrv re-checks a PER-CHILD SpiceDB tuple (read_transcript)
// because its payload is message CONTENT — a child's turns — read out of the
// memory data plane under a minted system approval. This route's payload is
// metadata only (namespace/name/class/role, already visible to anyone
// genuinely in the same thread by construction) and touches no memory scope
// at all: every field comes straight off an AgentSession or Workshop CR via
// the operator's own in-process, RBAC-authorized client. There is therefore
// no bearer-scoped memory capability door to cross here, and no
// memory.WithSystemApproval mint — unlike workshoptranscriptsrv.
// pkg/controllers/workshop/controller.go registers the bearer's token with NO
// extras, so its authorized set is {B, ws-<X>}, never {B, X} itself — a claim
// the master spec's §13 got wrong (see the dated addendum this task also
// appends there). This route works with that truth rather than around it: it
// needs no token-registry widening, because it never reaches into X's memory
// scope to begin with. The one live authorization check it DOES make is
// workshopdraftsrv's own WorkshopBuildChecker tuple, re-verified here as a
// freshness gate rather than invented fresh for this route.
package workshopthreadsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// Path is the route this package mounts. No path parameter: every identity
// the handler needs is derived from the bearer alone.
const Path = "/workshop/agents-in-thread"

// MaxAgentsInThread caps how many {participant + descendant} rows one
// request ever returns, and stops WALKING FURTHER PARTICIPANTS once that many
// rows have accumulated — serveHTTP breaks out of the participant loop rather
// than calling DescendantsOf for every remaining participant only to discard
// the rows a moment later.
//
// This bounds the NUMBER OF PARTICIPANTS whose own closures get walked in one
// request, not the cost of any SINGLE participant's walk: DescendantsOf costs
// one List for that participant's whole closure plus one WalkAncestors (up to
// MaxLineageWalk hops each) per member of it, and none of that is
// interruptible mid-participant — a lone participant with an unusually large
// subtree still pays that full cost in one iteration before the next
// participant's cap check ever runs. What the cap actually prevents is a
// pathological tree COMPOUNDING ACROSS participants (fifty participants each
// with their own large subtree), not a guarantee on total work for a single
// request; WorkshopLimits' own per-workshop caps (workshop_types.go's
// MaxObjects et al.) are what keep any one participant's own subtree bounded
// in practice.
//
// The early stop can still leave the very last participant's own closure
// pushing the total past this cap in one jump (one participant with many
// children) — serveHTTP's final truncation is the hard backstop for that
// case, not the primary mechanism.
const MaxAgentsInThread = 50

const (
	// RoleParticipant marks a session that itself shares the builder's
	// conversation thread, found via its own channel binding.
	RoleParticipant = "participant"
	// RoleDescendant marks a session found by walking a thread participant's
	// OWN delegation closure — a subagent that participant has delegated to
	// AT SOME POINT, not necessarily while working this thread, and not
	// itself bound to the thread's channel. Treat it as weaker evidence than
	// RoleParticipant: it shows what a participant is capable of delegating
	// to, not proof any of it happened here.
	RoleDescendant = "descendant"
)

// WorkshopBuildChecker answers workshop:<workshopID>#build for a session —
// satisfied by (*pkg/authz/spicedb.Client).CheckWorkshopBuild. Declared
// locally (mirroring pkg/controllers/workshopprobe.WorkshopBuildChecker,
// pkg/controllers/webhooks/workshop.WorkshopBuildChecker and
// pkg/web/workshopdraftsrv.WorkshopBuildChecker) rather than a
// *spicedb.Client field, for the same two reasons those packages give: this
// package's tests inject a fake with no live SpiceDB, and the handler never
// carries a typed pointer that could be assigned nil into an interface field
// (CLAUDE.md's typed-nil rule) — a genuinely unwired dependency is a true
// nil interface, caught by the nil check in ServeHTTP, not a panic.
type WorkshopBuildChecker interface {
	CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error)
}

// AgentInThread is one row of the 200 response body: a JSON array of these,
// sorted by (namespace, name) for a deterministic reply.
type AgentInThread struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Class     string `json:"class"`
	Role      string `json:"role"`
}

// NewHandler returns the GET /workshop/agents-in-thread handler.
//
//   - c resolves the caller's own Workshop CR (to derive B/X), reads X's own
//     AgentSession to learn its channel bindings, and lists/walks sessions
//     in B. Reads only.
//   - reg authenticates the bearer.
//   - build answers workshop:<W>#build for the derived (B, X); nil is
//     handled at request time (denied, not panicked) per the typed-nil rule
//     on WorkshopBuildChecker.
//   - log records a per-participant delegation-closure walk failure (a
//     dangling Spec.Parent) so the operator can locate the orphaned session
//     without that one broken link taking down the whole lookup. The zero
//     value (logr.Logger{}) is a safe no-op — logr.Logger.Info/Error both
//     check for a nil sink before doing anything — so a caller that omits it
//     gets silent-but-safe logging, never a panic.
func NewHandler(c client.Client, reg *tokens.Registry, build WorkshopBuildChecker, log logr.Logger) http.Handler {
	h := &handler{c: c, reg: reg, build: build, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, h.serveHTTP)
	return mux
}

type handler struct {
	c     client.Client
	reg   *tokens.Registry
	build WorkshopBuildChecker
	log   logr.Logger
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-agents-in-thread"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, prefix)

	res, rerr := ResolveAgentsInThread(r.Context(), h.c, h.reg, h.build, h.log, token)
	if rerr != nil {
		if rerr.Status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-agents-in-thread"`)
		}
		http.Error(w, rerr.Message, rerr.Status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(res.Agents); err != nil {
		// The lookup already succeeded; only the response encode failed (e.g.
		// a client that closed the connection mid-write). Logged per
		// CLAUDE.md's never-silently-drop-an-error rule — WriteHeader has
		// already committed the 200 status, so nothing more can be told to
		// the caller at this point.
		h.log.Info("workshopthreadsrv: encoding agents-in-thread response failed", "err", err.Error())
	}
}

// ResolveError is a denied or failed resolution from ResolveAgentsInThread:
// the HTTP status the caller should reply with, plus a caller-safe message.
// A second route translates it into its own http.Error exactly the way this
// package's own ServeHTTP does above.
type ResolveError struct {
	Status  int
	Message string
}

func (e *ResolveError) Error() string { return e.Message }

// Resolved is what ResolveAgentsInThread computes for one bearer: the
// resolved builder session identity — B/X (SessionNamespace/SessionName)
// and the workshop id W its Workshop CR reports (WorkshopID) — together
// with its own agents-in-thread set.
type Resolved struct {
	WorkshopID       string
	SessionNamespace string
	SessionName      string
	Agents           []AgentInThread
}

// ResolveAgentsInThread runs this package's own steps 1-8 (see the package
// doc above) for a bearer TOKEN already stripped of its "Bearer " prefix:
// authenticate it, resolve and validate the Workshop CR it belongs to,
// re-check workshop:<W>#build, resolve the builder session (B/X), and
// compute its thread's participant/descendant set.
//
// Exported so a second route can ask "is this class reachable from THIS
// builder's own agents-in-thread result" without re-deriving the join
// itself: agent-builder plan 9b, Task 1's reachability rule ("a class is
// projectable iff it appears in THIS builder's own agents-in-thread
// result", pkg/web/workshopprojectsrv) needs EXACTLY this computation, not
// a second copy of it — 9a already carries one deliberate duplication (the
// anchor helpers below, pinned by anchor_parity_test.go) that needed a
// parity test to keep honest, and a second would be worse. ServeHTTP (this
// package's own handler, above) is now a thin wrapper over this: it calls
// ResolveAgentsInThread, translates a *ResolveError into http.Error, and
// JSON-encodes the success case.
func ResolveAgentsInThread(ctx context.Context, c client.Client, reg *tokens.Registry, build WorkshopBuildChecker, log logr.Logger, token string) (*Resolved, *ResolveError) {
	// The bearer resolves to the SYNTHETIC workshop key
	// {builderSessionNamespace, WorkshopName(builderSessionName)} the
	// AgentSession reconciler registered it under (plan 2) — this proves the
	// bearer belongs to SOME workshop, nothing about which thread it may
	// enumerate.
	info, ok := reg.LookupInfo(token)
	if !ok {
		return nil, &ResolveError{Status: http.StatusUnauthorized, Message: "unauthorized"}
	}

	var ws spiceboxv1alpha1.Workshop
	if err := c.Get(ctx, types.NamespacedName{Namespace: info.Session.Namespace, Name: info.Session.Name}, &ws); err != nil {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("no workshop resolves for this bearer: %v", err)}
	}
	if ws.Status.Namespace == "" || ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("workshop %s/%s is not Ready (phase %q)", ws.Namespace, ws.Name, ws.Status.Phase)}
	}
	// Defense-in-depth on top of the tuple re-check below: a legitimately
	// provisioned Workshop's spec.session always names a session in the
	// Workshop's OWN namespace (WorkshopNamespacesFor's own doc). spec.session
	// is bearer-patchable (see the package doc's "why this is safe" section),
	// so a cross-namespace value is refused outright here rather than left to
	// the tuple check alone — the same hardening class
	// subagentrequest/controller.go applies to sr.Spec.Parent.Namespace.
	if ws.Spec.Session.Namespace != ws.Namespace {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("workshop %s/%s names a session in a different namespace (%q); refusing", ws.Namespace, ws.Name, ws.Spec.Session.Namespace)}
	}
	// B/X and W come ENTIRELY from the resolved CR — never the URL, which
	// carries no identity for this route at all.
	sessNS, sessName, workshopID := ws.Spec.Session.Namespace, ws.Spec.Session.Name, ws.Status.Namespace

	// Fail-closed on a nil checker: an unwired dependency must never read as
	// "no tuple to check, so allow" (CLAUDE.md's typed-nil rule; mirrors
	// workshopdraftsrv's h.build == nil guard).
	if build == nil {
		return nil, &ResolveError{Status: http.StatusServiceUnavailable, Message: "no workshop build-authorization backend is wired; refusing to enumerate"}
	}
	// FullyConsistent per CheckWorkshopBuild's own contract: this is a
	// privilege gate asked moments before the read it protects. An error is
	// fail-closed too — an unconfirmable tuple is a no, not a maybe.
	allowed, err := build.CheckWorkshopBuild(ctx, workshopID, sessNS, sessName)
	if err != nil {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("checking workshop:%s#build for session %s/%s: %v", workshopID, sessNS, sessName, err)}
	}
	if !allowed {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("session %s/%s does not hold workshop:%s#build", sessNS, sessName, workshopID)}
	}

	// X's own channel bindings are read directly off its AgentSession spec —
	// never a caller-supplied value — to derive the thread anchor this
	// lookup is scoped to.
	var x spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: sessName}, &x); err != nil {
		return nil, &ResolveError{Status: http.StatusForbidden, Message: fmt.Sprintf("builder session %s/%s not found: %v", sessNS, sessName, err)}
	}

	participants, err := threadParticipants(ctx, c, sessNS, &x)
	if err != nil {
		return nil, &ResolveError{Status: http.StatusInternalServerError, Message: "list thread participants: " + err.Error()}
	}

	// emitted maps (namespace, name) -> its index in out, for every session
	// already placed there, across BOTH roles — so a session that is
	// independently a thread participant AND reachable via some OTHER
	// participant's delegation closure (an attended child bound to the same
	// anchor as its own root, per subagentrequest/controller.go's buildChild)
	// is emitted exactly once. Participant wins when a session is both:
	// independent thread membership is the more specific fact, not merely
	// having been delegated by someone else who also happens to be in the
	// thread.
	//
	// The index (not just a seen bool) is what makes that ordering-proof: a
	// session can be discovered as a DESCENDANT of some earlier participant in
	// this very loop before its own turn as a PARTICIPANT comes up (delegation
	// order and participant-list order are unrelated), and skipping it there
	// would leave it stuck at the lower-specificity role. Finding its existing
	// row and upgrading it in place gets "participant wins" right regardless
	// of which one this loop happens to reach first.
	// isBuilder reports whether (ns, name) IS the builder session X itself —
	// the one asking, never a participant or descendant to reproduce. Used by
	// BOTH loops below (MINOR-10): a check on the participant loop alone once
	// let X slip back in as a DESCENDANT whenever some other participant's own
	// delegation closure happened to include X (X is itself an attended child
	// of a participant) — X was excluded from `participants` directly, but
	// nothing stopped the descendant loop from adding it right back in, and
	// X never entered `emitted` to be caught by the dedup either.
	isBuilder := func(ns, name string) bool { return ns == sessNS && name == sessName }

	emitted := make(map[string]int, len(participants))
	// Initialized non-nil (MINOR-9): an empty result must encode as the JSON
	// array `[]`, matching this route's documented shape and the tool's own
	// contract, never `null` from an untouched nil slice.
	out := []AgentInThread{}
	for i := range participants {
		if len(out) >= MaxAgentsInThread {
			// Stop WALKING FURTHER PARTICIPANTS, not just truncating the reply:
			// a delegation-closure List for every remaining participant would
			// still cost a real List call per participant for rows that get
			// discarded moments later by the truncation below. See
			// MaxAgentsInThread's own doc: the cap bounds how many participants'
			// closures get walked, not the cost of any single participant's own
			// walk already under way.
			break
		}
		p := &participants[i]
		if isBuilder(p.Namespace, p.Name) {
			// The builder session itself is never a participant to
			// reproduce — it is the one asking.
			continue
		}
		pKey := p.Namespace + "/" + p.Name
		if idx, ok := emitted[pKey]; ok {
			out[idx].Role = RoleParticipant // upgrade: independent membership beats "merely delegated"
		} else {
			emitted[pKey] = len(out)
			out = append(out, AgentInThread{Namespace: p.Namespace, Name: p.Name, Class: p.Spec.Class, Role: RoleParticipant})
		}

		// DescendantsOf resolves p's OWN delegation-tree root first
		// (RootNameFor) before listing the closure, so it is correct whether p
		// is itself a root or a non-root delegated session. ListClosure(ns,
		// p.Name) — the prior shape here — silently returned nothing whenever p
		// was itself non-root: a descendant's LabelDelegationRoot names the
		// TREE ROOT, never its immediate parent (RootNameFor's own doc), so
		// filtering on p.Name found no one. p itself is added by hand above,
		// exactly as DescendantsOf's doc instructs callers who want the whole
		// subtree including its own root.
		descendants, err := spiceboxv1alpha1.DescendantsOf(ctx, c, p)
		if err != nil {
			// A dangling Spec.Parent (subagentrequest's delegate.go
			// creates no OwnerReference from the SubagentRequest to the PARENT
			// AgentSession, only to the child, so deleting/reaping an
			// intermediate session orphans a child whose Spec.Parent names a
			// session that no longer exists) must not 500 the WHOLE lookup,
			// permanently, for this thread. One participant's broken closure
			// walk is logged and skipped; the participant itself is already
			// emitted above, and every other participant's own walk proceeds
			// independently.
			log.Info("workshopthreadsrv: delegation closure walk failed for a thread participant; continuing without its descendants",
				"session", pKey, "err", err.Error())
			continue
		}
		for j := range descendants {
			d := &descendants[j]
			if isBuilder(d.Namespace, d.Name) {
				// The builder session must never be reproduced, whichever loop
				// finds it — see isBuilder's own doc.
				continue
			}
			dKey := d.Namespace + "/" + d.Name
			if _, ok := emitted[dKey]; ok {
				continue // already emitted — as a participant, or as another participant's descendant; never demote
			}
			emitted[dKey] = len(out)
			out = append(out, AgentInThread{Namespace: d.Namespace, Name: d.Name, Class: d.Spec.Class, Role: RoleDescendant})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > MaxAgentsInThread {
		out = out[:MaxAgentsInThread]
	}

	return &Resolved{WorkshopID: workshopID, SessionNamespace: sessNS, SessionName: sessName, Agents: out}, nil
}

// threadParticipants returns every AgentSession in namespace ns that
// genuinely shares x's own conversation thread — INCLUDING x itself, which
// the caller (serveHTTP) excludes from the final response. Returns (nil,
// nil), not an error, when x is not on a shared thread at all (a
// kubectl-driven or cron-only builder session): that is an ordinary
// condition, not a failure.
//
// Scoped twice, never a namespace dump: both Lists below are label-scoped to
// x's own channel identity (the shape at
// pkg/channels/channelsd/pipeline/pipeline.go:451-490's correlateSessions),
// and the merged result is further filtered to sessions that genuinely bind
// the SAME thread anchor (pkg/channels/channelsd/pipeline/thread_owner.go:
// 31-49's ruling — a hashed key indexes each List, but the anchor is what
// proves shared membership, because a DM key is only meaningful within one
// Channel).
func threadParticipants(ctx context.Context, c client.Client, ns string, x *spiceboxv1alpha1.AgentSession) ([]spiceboxv1alpha1.AgentSession, error) {
	binding, anchor := resolveThreadAnchor(x)
	if binding == nil || anchor == "" {
		return nil, nil
	}
	keyHash := channelkey.LabelValue(binding.Key)

	var candidates spiceboxv1alpha1.AgentSessionList
	if err := c.List(ctx, &candidates,
		client.InNamespace(ns),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelChannelName: binding.Name,
			spiceboxv1alpha1.LabelChannelKey:  keyHash,
		},
	); err != nil {
		return nil, fmt.Errorf("list sessions for channel %s/%s: %w", ns, binding.Name, err)
	}

	// A peer anchored to the SAME thread purely through its own OUTPUT
	// channel — a cron- or output-anchored session, whose LabelOutputChannelKey
	// is stamped by the outbound relay after its first send
	// (outbound/relay.go:1005-1015's patchOutputChannel) — is invisible to the
	// query above, which only ever keys on LabelChannelKey. Mirror
	// pipeline.go's correlateSessions and also query LabelOutputChannelKey.
	// LabelChannelName is deliberately NOT part of this second query, for the
	// same reason correlateSessions omits it there: such a session's own
	// LabelChannelName (if it carries one at all) names its INPUT channel,
	// which need not be the Channel CR x is bound to.
	var outputCandidates spiceboxv1alpha1.AgentSessionList
	if err := c.List(ctx, &outputCandidates,
		client.InNamespace(ns),
		client.MatchingLabels{spiceboxv1alpha1.LabelOutputChannelKey: keyHash},
	); err != nil {
		return nil, fmt.Errorf("list sessions for output channel key in %s: %w", ns, err)
	}

	seen := make(map[string]bool, len(candidates.Items)+len(outputCandidates.Items))
	var out []spiceboxv1alpha1.AgentSession
	for _, list := range []*spiceboxv1alpha1.AgentSessionList{&candidates, &outputCandidates} {
		for i := range list.Items {
			s := &list.Items[i]
			if !bindsAnchor(s, anchor) {
				continue
			}
			key := s.Namespace + "/" + s.Name
			if seen[key] {
				continue // found via both queries — a session can carry both labels
			}
			seen[key] = true
			out = append(out, *s)
		}
	}
	// Sorted before returning: unsorted, `out` carries whatever
	// order the two List calls' cache happened to return sessions in — cache
	// map order, not a stable one — so serveHTTP's early-break cap
	// (MaxAgentsInThread) would select a different first-N set across two
	// identical requests. Sorting here, before the cap ever sees the slice,
	// makes the selection deterministic; serveHTTP's own final sort re-sorts
	// the merged {participant, descendant} output, which is a different slice
	// entirely and does not make this one redundant.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// resolveThreadAnchor returns x's own conversation anchor
// ("<channel_id>:<thread_ts>") together with the SPECIFIC binding it came
// from, checking x's input binding first and its output binding second — an
// ordinary human-initiated thread lives on the input side, a cron-spawned
// session's on the output side (see
// pkg/channels/channelsd/pipeline/thread_owner.go's threadOwnerLabels doc).
// Returning the SOURCE binding alongside the anchor (not just the anchor
// value) matters: the List in threadParticipants must key off the SAME
// binding the anchor came from, not whichever binding happens to be
// non-nil, or a cron-spawned builder session's non-thread input binding
// would scope the List to the wrong channel entirely.
func resolveThreadAnchor(x *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.ChannelBinding, string) {
	if a := bindingAnchor(x.Spec.InputChannel); a != "" {
		return x.Spec.InputChannel, a
	}
	if a := bindingAnchor(x.Spec.OutputChannel); a != "" {
		return x.Spec.OutputChannel, a
	}
	return nil, ""
}

// bindingAnchor mirrors pkg/channels/channelsd/pipeline's unexported
// threadAnchor (thread_owner.go:22-37) — duplicated rather than imported
// because that function is package-private and this route intentionally
// takes no dependency on channelsd's pipeline package. The two must be kept
// in sync by hand; both encode the same two-field rule for the same stated
// reason: both channel_id and thread_ts are required, and the channel KEY is
// deliberately not used here (a DM key is only meaningful within one
// Channel, so keying off it would conflate two unrelated conversations
// across two different bots).
//
// Deliberately NOT unified with pipeline's pair in this round: thread_owner.go
// is live channelsd pipeline routing code on a hot path, and relocating it
// under a plan whose subject is a builder lookup would mix an unrelated
// refactor into a security-surface review — a mistake there breaks message
// routing, not this route. Left as a decision, not an oversight: drift
// between the two is made LOUD instead, by
// TestAnchorHelpers_MatchPipelinesOwnBehavior (anchor_parity_test.go), which
// asserts this pair and pipeline's own (via pipeline.ThreadAnchorForTest /
// pipeline.BindsAnchorForTest) agree over a shared input table.
func bindingAnchor(b *spiceboxv1alpha1.ChannelBinding) string {
	if b == nil {
		return ""
	}
	channelID, threadTS := b.External["channel_id"], b.External["thread_ts"]
	if channelID == "" || threadTS == "" {
		return ""
	}
	return channelID + ":" + threadTS
}

// bindsAnchor mirrors pkg/channels/channelsd/pipeline's unexported
// bindsAnchor (thread_owner.go:39-49): sess genuinely shares the
// conversation at anchor when either of its own bindings names it — plus an
// explicit empty-anchor guard the original does not have. Without that
// guard, an empty anchor argument would match any binding whose OWN computed
// anchor is also empty ("" == ""), which pipeline's own bindsAnchor does not
// defend against (TestAnchorHelpers_MatchPipelinesOwnBehavior,
// anchor_parity_test.go, documents the divergence directly rather than
// letting the parity check paper over it). No live bug either way: every
// caller here (threadParticipants) already returns early whenever its own
// resolved anchor is "", so bindsAnchor is never reached with one in
// practice.
func bindsAnchor(sess *spiceboxv1alpha1.AgentSession, anchor string) bool {
	if anchor == "" {
		return false
	}
	return bindingAnchor(sess.Spec.InputChannel) == anchor || bindingAnchor(sess.Spec.OutputChannel) == anchor
}
