package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferencewrite"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// subjectResolutionUnavailableReason is the fixed Reason a ?user-ref= read
// carries when the reference is well-formed enough to NEED a RelationReader
// (a resource reference, or a trigger-author reference that recurses into
// one) but this deployment has none configured (WithSubjectResolution was
// never passed). Fixed and generic rather than echoing
// subjectresolve.ErrNoRelationReader's own text: that error is an internal
// signal this package translates, not wire-facing wording of its own.
const subjectResolutionUnavailableReason = "subject resolution is not available on this cluster"

// handlePreferencesGet serves GET /memory/_preferences/{ns}/{name}, in one
// of two mutually exclusive modes selected by query parameter:
//
//   - ?turn=<idx> (or neither param): the turn-author-verified preferences
//     snapshot for the session's AgentClass, over the FULL declared schema.
//     The author is NEVER taken from the request — only from the server's
//     own turn record.
//   - ?user-ref=<ref>: a subject-NAMED read for whichever user ref resolves
//     to (see handlePreferencesGetForUserRef) — the class-visible subset
//     ONLY, and always audited.
//
// Both are mutually exclusive (400 if both are given). Class/session
// identity comes from the AgentSession/AgentClass CRs named by the URL,
// never the request body — there is none, this is a GET — in either mode.
//
// Disclosure model for ?user-ref=: a class author opts a preference into
// `visibility: class` on the AgentClass CR; that is the ONLY way a value
// becomes readable for anyone but the user themself. A self-only
// (visibility unset, or explicitly "self") key is filtered out of the
// SCHEMA before this handler reads any user value for it, so a bug
// elsewhere in this function cannot leak one just because a value happens
// to exist — and every subject-named read, whether or not the reference
// resolved, is appended to the session's own tamper-evident chain as a
// preference_access entry (pkg/memory/kinds/preferenceaccess), so "who did
// this agent look up, and what could it see" is always reconstructable.
func (h *handler) handlePreferencesGet(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.prefsReader == nil {
		http.Error(w, "preferences not supported", http.StatusMethodNotAllowed)
		return
	}
	turnParam := r.URL.Query().Get("turn")
	userRef := r.URL.Query().Get("user-ref")
	if turnParam != "" && userRef != "" {
		http.Error(w, "?turn and ?user-ref are mutually exclusive", http.StatusBadRequest)
		return
	}
	ns, name, ok := strings.Cut(scope.ID, "/")
	if !ok {
		// scope.ID is always built as ns+"/"+name by ServeHTTP; unreachable in
		// practice, but a malformed scope must fail loud rather than panic on
		// the CR lookups below.
		log.FromContext(r.Context()).Info("memory: preferences malformed session scope", "scope", scope.ID)
		http.Error(w, "malformed session scope", http.StatusInternalServerError)
		return
	}

	// SystemContext, not r.Context(): the user_preference read below queries
	// the (turn author's or named subject's) USER scope, and Local.Query's
	// capability door (ensureApproval on q.Scope.ID) refuses it — the
	// request's bearer approval names only the session's own "<ns>/<name>".
	// This resolution is the operator's, keyed to a record the caller cannot
	// influence, so it runs under the platform's own approval with the token
	// marks cleared.
	sysCtx := memory.SystemContext(r.Context(), "preferences")

	if userRef != "" {
		h.handlePreferencesGetForUserRef(w, r, scope, sysCtx, ns, name, userRef)
		return
	}

	author, status, err := h.resolvePreferencesTurnAuthor(sysCtx, r, scope)
	if err != nil {
		log.FromContext(r.Context()).Info("memory: preferences turn lookup failed",
			"session", scope.ID, "status", status, "err", err.Error())
		// httpError, not http.Error: a failed memory Query surfaces here with
		// sentinelStatus, and the sentinel header is what lets httpclient
		// rebuild the same error on the other side.
		httpError(w, err, status)
		return
	}

	sess, class, ok := h.lookupSessionAndClass(w, r, scope, ns, name)
	if !ok {
		return
	}
	className := sess.Spec.Class
	if len(class.Spec.UserPreferences) == 0 {
		log.FromContext(r.Context()).Info("memory: preferences requested for a class that declares none",
			"session", scope.ID, "class", className)
		http.Error(w, "class declares no preferences", http.StatusNotFound)
		return
	}

	globals, ok := h.lookupGlobals(w, r, scope, ns, className)
	if !ok {
		return
	}

	var subject string
	userVals := map[string]apiextv1.JSON{}
	if canonical, err := identity.Subject(author).CanonicalUserID(); err == nil {
		subject = canonical.String()
		userScope, uerr := memory.UserScope(subject)
		if uerr != nil {
			log.FromContext(r.Context()).Info("memory: preferences user scope derivation failed",
				"session", scope.ID, "subject", subject, "err", uerr.Error())
			http.Error(w, "user scope error", http.StatusInternalServerError)
			return
		}
		res, qerr := h.mem.Query(sysCtx, memory.Query{Scope: userScope, Kinds: []string{userpreference.KindName}})
		if qerr != nil {
			failRequest(w, r, scope, "preferences.user", qerr)
			return
		}
		for _, e := range res.Entries {
			var p userpreference.Preference
			if derr := json.Unmarshal(e.Content, &p); derr != nil {
				log.FromContext(r.Context()).Info("memory: preferences skipping undecodable user_preference entry",
					"session", scope.ID, "id", e.ID, "err", derr.Error())
				continue
			}
			if p.ClassNamespace != ns || p.ClassName != className {
				continue
			}
			userVals[p.Key] = apiextv1.JSON{Raw: p.Value}
		}
	}
	// Author empty or not "user:"-prefixed (a service subject, or no author at
	// all) falls through here with subject == "" and userVals empty — the
	// resolver then reports Snapshot values from the class default / admin
	// global only, exactly as if no user layer existed.

	snap := preferences.Resolve(class.Spec.UserPreferences, globals, userVals)
	writeJSON(w, r, http.StatusOK, preferences.SnapshotResponse{
		Subject:        subject,
		ClassNamespace: ns,
		ClassName:      className,
		Snapshot:       snap,
	})
}

// maxAuditDetailRunes bounds how much of a fault's error text an audit
// entry's Reason echoes. Audit entries are append-only and undeletable, so
// an unbounded upstream error string (a SpiceDB gRPC dump, a wrapped
// Postgres message) would be immortalized at whatever size it arrived —
// the same reasoning subjectresolve bounds its agent-relayed Reasons.
const maxAuditDetailRunes = 300

// boundedAuditDetail caps a fault message before it lands in an append-only
// audit record, marking the cut with an ellipsis.
func boundedAuditDetail(s string) string {
	runes := []rune(s)
	if len(runes) <= maxAuditDetailRunes {
		return s
	}
	return string(runes[:maxAuditDetailRunes]) + "…"
}

// handlePreferencesGetForUserRef serves ?user-ref=<ref>: a preferences read
// for whichever user ref resolves to, over the class-visible subset of the
// schema ONLY — see handlePreferencesGet's doc for the disclosure model.
// ns/name are the URL-named session/class coordinates already validated by
// the caller; sysCtx is the platform-approved context the user-scope query
// and the audit write both need (see handlePreferencesGet's own comment on
// why r.Context() is not enough).
//
// AUDIT COVERAGE IS TOTAL from the point the session/class are validated
// (and the class declares at least one preference — before that there is no
// disclosure surface and no resolution is attempted): every exit appends
// exactly one preference_access entry, fault paths included, BEFORE its
// response bytes go out. The fault paths matter as much as the successes —
// httpclient retries a 5xx up to eight times, so one unaudited fault branch
// would be one unaudited branch amplified into eight invisible attempts.
// The two halves fail differently, on purpose:
//
//   - On the 200 paths (resolved or unresolved), an audit failure fails the
//     REQUEST — a disclosure the trail does not carry would silently break
//     the guarantee this route is told holds.
//   - On the fault paths, an audit failure is logged alongside the original
//     fault and the ORIGINAL fault is still answered — masking a 502 behind
//     an audit 500 would hide the real failure from the caller, and nothing
//     was disclosed for the trail to miss.
func (h *handler) handlePreferencesGetForUserRef(w http.ResponseWriter, r *http.Request, scope memory.Scope, sysCtx context.Context, ns, name, ref string) {
	if h.prefAudit == nil {
		// Fail closed — see WithPreferenceAudit's doc: an unaudited
		// subject-named disclosure is a different feature, not a degraded
		// version of this one.
		log.FromContext(r.Context()).Info("memory: preferences user-ref read refused: no audit writer configured",
			"session", scope.ID)
		http.Error(w, "subject-named preference reads require the audit writer, which is not configured on this cluster", http.StatusServiceUnavailable)
		return
	}

	sess, class, ok := h.lookupSessionAndClass(w, r, scope, ns, name)
	if !ok {
		return
	}
	className := sess.Spec.Class
	if len(class.Spec.UserPreferences) == 0 {
		log.FromContext(r.Context()).Info("memory: preferences requested for a class that declares none",
			"session", scope.ID, "class", className)
		http.Error(w, "class declares no preferences", http.StatusNotFound)
		return
	}

	// Filter to visibility: class keys BEFORE resolving anything else, so a
	// self-only key is never even a candidate the rest of this function could
	// read a value for. classVisible feeds preferences.Resolve directly (the
	// snapshot's Keys can only ever be these); visibleNames gates both the
	// admin-global map and the user-value map below.
	var classVisible []v1alpha1.UserPreferenceSchema
	visibleNames := map[string]bool{}
	for _, s := range class.Spec.UserPreferences {
		if s.Visibility == "class" {
			classVisible = append(classVisible, s)
			visibleNames[s.Name] = true
		}
	}
	keys := make([]string, 0, len(classVisible))
	for _, s := range classVisible {
		keys = append(keys, s.Name)
	}

	// audit appends THE preference_access record for this attempt. Every
	// exit below calls it exactly once (directly on the 200 paths,
	// via auditFault on the fault paths).
	audit := func(outcome, reason, subject string, auditedKeys []string) error {
		return preferenceaccess.Record(sysCtx, h.prefAudit, scope, preferenceaccess.Content{
			Ref:             ref,
			ResolvedSubject: subject,
			Outcome:         outcome,
			Reason:          reason,
			Keys:            auditedKeys,
		})
	}
	// auditFault records a fault-path attempt: Keys empty (nothing was
	// disclosed), Reason the bounded fault text, subject whatever was known
	// by the time the fault hit. An audit failure here is logged WITH the
	// fault it was recording and the original fault still answers the
	// request — see the function doc for why the two halves fail differently.
	auditFault := func(outcome, subject string, faultErr error) {
		if aerr := audit(outcome, boundedAuditDetail(faultErr.Error()), subject, nil); aerr != nil {
			log.FromContext(r.Context()).Info("memory: preferences user-ref audit write failed on a fault path",
				"session", scope.ID, "outcome", outcome, "fault", faultErr.Error(), "auditErr", aerr.Error())
		}
	}

	globals, gerr := h.readGlobals(r.Context(), ns, className)
	if gerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences AgentSettings lookup failed",
			"session", scope.ID, "err", gerr.Error())
		auditFault(preferenceaccess.OutcomeGlobalsError, "", gerr)
		http.Error(w, "settings lookup failed", http.StatusInternalServerError)
		return
	}
	classVisibleGlobals := map[string]v1alpha1.PreferenceGlobal{}
	for k, v := range globals {
		if visibleNames[k] {
			classVisibleGlobals[k] = v
		}
	}

	env := subjectresolve.Env{
		Relations: h.subjectRelations,
		// sess was already read through h.prefsReader by lookupSessionAndClass
		// above; reusing it here (rather than a second CR Get) is both fewer
		// round trips and the same data, since nothing between here and there
		// can have changed it.
		SessionAnnotations: func(context.Context) (map[string]string, error) {
			return sess.Annotations, nil
		},
	}
	res, rerr := subjectresolve.Resolve(r.Context(), ref, env)
	switch {
	case errors.Is(rerr, subjectresolve.ErrNoRelationReader):
		// A well-formed reference this deployment cannot resolve because no
		// RelationReader is wired — NOT a fault, and not the caller's mistake
		// either: answer exactly as if the resource had no linked user, so an
		// operator that never configured SpiceDB relation reads sees a
		// resolution-shaped outcome, not a 5xx.
		res = subjectresolve.Resolution{Reason: subjectResolutionUnavailableReason}
	case rerr != nil:
		log.FromContext(r.Context()).Info("memory: preferences user-ref resolution failed",
			"session", scope.ID, "err", rerr.Error())
		auditFault(preferenceaccess.OutcomeResolverError, "", rerr)
		httpError(w, fmt.Errorf("resolve user-ref: %w", rerr), http.StatusBadGateway)
		return
	}

	var subject string
	userVals := map[string]apiextv1.JSON{}
	if res.Subject != "" {
		subject = res.Subject
		userScope, uerr := memory.UserScope(subject)
		if uerr != nil {
			log.FromContext(r.Context()).Info("memory: preferences user scope derivation failed",
				"session", scope.ID, "subject", subject, "err", uerr.Error())
			auditFault(preferenceaccess.OutcomeUserScopeError, subject, uerr)
			http.Error(w, "user scope error", http.StatusInternalServerError)
			return
		}
		qres, qerr := h.mem.Query(sysCtx, memory.Query{Scope: userScope, Kinds: []string{userpreference.KindName}})
		if qerr != nil {
			auditFault(preferenceaccess.OutcomeQueryError, subject, qerr)
			failRequest(w, r, scope, "preferences.user", qerr)
			return
		}
		for _, e := range qres.Entries {
			var p userpreference.Preference
			if derr := json.Unmarshal(e.Content, &p); derr != nil {
				log.FromContext(r.Context()).Info("memory: preferences skipping undecodable user_preference entry",
					"session", scope.ID, "id", e.ID, "err", derr.Error())
				continue
			}
			if p.ClassNamespace != ns || p.ClassName != className {
				continue
			}
			if !visibleNames[p.Key] {
				// Self-only key: never enters the user-values map for a
				// named-user read, even though the same row IS read (and
				// used) for the turn-author's own request.
				continue
			}
			userVals[p.Key] = apiextv1.JSON{Raw: p.Value}
		}
	}

	outcome := preferenceaccess.OutcomeOK
	if subject == "" {
		outcome = preferenceaccess.OutcomeUnresolved
	}
	if aerr := audit(outcome, res.Reason, subject, keys); aerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences user-ref audit write failed",
			"session", scope.ID, "err", aerr.Error())
		httpError(w, fmt.Errorf("record preference-access audit: %w", aerr), http.StatusInternalServerError)
		return
	}

	// Never answer an unresolved user-ref with class DEFAULTS. A default
	// snapshot is byte-for-byte identical to "this user saved nothing", so a 200
	// here would let the caller silently act on the wrong policy for a user it
	// could not identify — the webhook-triggered reviewbot bug: pinging on the
	// on_problems default when the author had set `always`. The attempt was
	// audited above (OutcomeUnresolved); the client gets a hard error carrying
	// the resolution reason and decides for itself (reviewbot: fall back to a
	// plain login, never a guessed mention). A RESOLVED user with no stored
	// value for a key still gets that key's default, in the snapshot below.
	if subject == "" {
		reason := res.Reason
		if reason == "" {
			reason = "no linked platform user"
		}
		http.Error(w, fmt.Sprintf("could not resolve user-ref %q to a platform user: %s", ref, reason),
			http.StatusUnprocessableEntity)
		return
	}

	snap := preferences.Resolve(classVisible, classVisibleGlobals, userVals)
	writeJSON(w, r, http.StatusOK, preferences.SnapshotResponse{
		Subject:        subject,
		ClassNamespace: ns,
		ClassName:      className,
		Snapshot:       snap,
		Note:           res.Reason,
	})
}

// handlePreferencesFirstPartyGet serves
// GET /memory/_preferences_firstparty/{ns}/{className}?subject=<bare-canonical>:
// the CLASS-addressed first-party preferences read the App Home (and any
// other first-party UI with no session of its own) uses to render a
// subject's OWN settings for a class. channelsd-ONLY — see
// componentPrincipalFrom's doc: the human-confirm verification that ties a
// request to a real, logged-in subject happens on channelsd's side of the
// conversational/UI surface, and the operator has no way to redo that check
// itself, exactly the same trust boundary handlePreferencesCommit already
// relies on for writes.
//
// {ns}/{className} names the AGENT CLASS directly — there is no
// AgentSession in this flow at all, so unlike every other _preferences*
// route this does NOT go through lookupSessionAndClass. scope.ID is still
// built by ServeHTTP as ns+"/"+name, but here "name" is the class name, not
// a session name.
//
// Returns the FULL declared schema — self-only keys AND class-visible keys
// alike — because the caller here is the subject looking at their OWN data,
// not an agent looking up someone else's (see handlePreferencesGetForUserRef,
// whose whole point is the opposite: a subset an agent may see about another
// person). There is accordingly no disclosure surface here beyond what the
// subject already has: NOT audited, unlike ?user-ref=, which audits every
// attempt because it is a subject-named read ABOUT someone else.
func (h *handler) handlePreferencesFirstPartyGet(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.prefsReader == nil {
		http.Error(w, "preferences not supported", http.StatusMethodNotAllowed)
		return
	}
	if principal, ok := componentPrincipalFrom(r.Context()); !ok || principal != "channelsd" {
		log.FromContext(r.Context()).Info("memory: preferences first-party read refused: caller is not the channelsd component",
			"scope", scope.ID)
		http.Error(w, "forbidden: first-party preferences reads require the channelsd component token", http.StatusForbidden)
		return
	}
	ns, className, ok := strings.Cut(scope.ID, "/")
	if !ok {
		// scope.ID is always built as ns+"/"+name by ServeHTTP; unreachable in
		// practice, but a malformed scope must fail loud rather than panic on
		// the CR lookup below.
		log.FromContext(r.Context()).Info("memory: preferences first-party malformed class scope", "scope", scope.ID)
		http.Error(w, "malformed class scope", http.StatusInternalServerError)
		return
	}

	subject := r.URL.Query().Get("subject")
	userScope, uerr := memory.UserScope(subject)
	if uerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party bad or missing subject",
			"ns", ns, "class", className, "err", uerr.Error())
		http.Error(w, "bad subject: "+uerr.Error(), http.StatusBadRequest)
		return
	}

	var class v1alpha1.AgentClass
	if err := h.prefsReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: className}, &class); err != nil {
		status := http.StatusInternalServerError
		if apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		log.FromContext(r.Context()).Info("memory: preferences first-party AgentClass lookup failed",
			"ns", ns, "class", className, "status", status, "err", err.Error())
		http.Error(w, "class lookup failed", status)
		return
	}
	if len(class.Spec.UserPreferences) == 0 {
		log.FromContext(r.Context()).Info("memory: preferences first-party requested for a class that declares none",
			"ns", ns, "class", className)
		http.Error(w, "class declares no preferences", http.StatusNotFound)
		return
	}

	globals, gerr := h.readGlobals(r.Context(), ns, className)
	if gerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party AgentSettings lookup failed",
			"ns", ns, "class", className, "subject", subject, "err", gerr.Error())
		http.Error(w, "settings lookup failed", http.StatusInternalServerError)
		return
	}

	// SystemContext, not r.Context(): this query targets the SUBJECT's user
	// scope, not any session/caller marks the channelsd token itself carries,
	// and the capability door's approval only ever names the URL's own
	// scope.ID (here, "<ns>/<className>", which is not a user scope at all).
	// Same pattern handlePreferencesGet uses for the same reason.
	sysCtx := memory.SystemContext(r.Context(), "preferences-firstparty")
	userVals := map[string]apiextv1.JSON{}
	res, qerr := h.mem.Query(sysCtx, memory.Query{Scope: userScope, Kinds: []string{userpreference.KindName}})
	if qerr != nil {
		failRequest(w, r, scope, "preferences.firstparty.user", qerr)
		return
	}
	for _, e := range res.Entries {
		var p userpreference.Preference
		if derr := json.Unmarshal(e.Content, &p); derr != nil {
			log.FromContext(r.Context()).Info("memory: preferences first-party skipping undecodable user_preference entry",
				"ns", ns, "class", className, "id", e.ID, "err", derr.Error())
			continue
		}
		if p.ClassNamespace != ns || p.ClassName != className {
			continue
		}
		// No visibility filter: unlike handlePreferencesGetForUserRef, EVERY
		// declared key is a candidate here, self-only included — that is the
		// whole point of this route. class.Spec.UserPreferences (passed to
		// Resolve below) is the full, unfiltered schema.
		userVals[p.Key] = apiextv1.JSON{Raw: p.Value}
	}

	snap := preferences.Resolve(class.Spec.UserPreferences, globals, userVals)
	writeJSON(w, r, http.StatusOK, preferences.SnapshotResponse{
		Subject:        subject,
		ClassNamespace: ns,
		ClassName:      className,
		Snapshot:       snap,
	})
}

// resolvePreferencesTurnAuthor answers "who authored the turn this snapshot
// resolves for", from the server's own transcript ONLY.
//
//   - ?turn=N given: read turn.EntryID(N, "user") in scope; missing → 404
//     "unknown turn".
//   - ?turn= absent: scan the scope's turn entries, and take the AUTHOR of
//     the highest-index Role="user" entry; no user turns at all → empty
//     author (a webhook/cron/bento session with no human turn), which is not
//     an error.
func (h *handler) resolvePreferencesTurnAuthor(ctx context.Context, r *http.Request, scope memory.Scope) (identity.Subject, int, error) {
	if s := r.URL.Query().Get("turn"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", http.StatusBadRequest, fmt.Errorf("bad ?turn: %w", err)
		}
		id := turn.EntryID(n, "user")
		res, err := h.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{turn.KindName}, IDs: []string{id}})
		if err != nil {
			return "", sentinelStatus(err), err
		}
		if len(res.Entries) == 0 {
			return "", http.StatusNotFound, errors.New("unknown turn")
		}
		t, err := turn.EntryToTurn(res.Entries[0])
		if err != nil {
			// The entry exists but will not decode as a turn — same client-visible
			// outcome as a missing one (no author to trust), but the decode
			// detail is worth a log line of its own since the generic failure
			// log above only ever sees "unknown turn".
			log.FromContext(r.Context()).Info("memory: preferences turn entry undecodable",
				"session", scope.ID, "id", res.Entries[0].ID, "err", err.Error())
			return "", http.StatusNotFound, errors.New("unknown turn")
		}
		return t.Author, 0, nil
	}

	res, err := h.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{turn.KindName}})
	if err != nil {
		return "", sentinelStatus(err), err
	}
	var author identity.Subject
	best := -1
	for _, e := range res.Entries {
		t, terr := turn.EntryToTurn(e)
		if terr != nil {
			// Skipped, not fatal — mirrors turn.Appender.ReadAll: one undecodable
			// row must not wedge every preferences read for the session. Logged
			// so a poisoned transcript entry is still discoverable.
			log.FromContext(r.Context()).Info("memory: preferences skipping undecodable turn entry",
				"session", scope.ID, "id", e.ID, "err", terr.Error())
			continue
		}
		if t.Role != "user" {
			continue
		}
		// Tool-result turns are stored with role "user" too (the LLM protocol
		// models a tool result as a user-role message; EntryToTurn derives Role
		// from the "-user" ID suffix) but carry no Author — only a real human
		// message turn does. An empty Author therefore means "not a human turn";
		// skipping it keeps this resolution on the latest HUMAN author. Without
		// it, an active tool-calling loop — where the latest user-role turn is
		// almost always a tool_result — resolves to an empty author, and every
		// preference read falls through to the class default even for a user who
		// just saved a value (set_preference's own readback included, so a save
		// looks like it never took and the agent re-prompts for confirmation).
		if t.Author.Empty() {
			continue
		}
		if t.Index > best {
			best = t.Index
			author = t.Author
		}
	}
	// TODO(session-identity): this transcript-scanning heuristic is a stand-in
	// for real session identity. Inferring "who is this session for" by picking
	// the latest human-authored turn is fragile — it silently broke the moment a
	// role/authorship assumption shifted (the empty-author tool_result bug above),
	// it has no clean representation for a multi-participant session, and the
	// write path already resolves a verified canonical subject (the confirming
	// decider) that this read path re-derives independently and can disagree with.
	// Replace it with an explicit, first-class session-identity/participant record
	// the reader consults directly instead of inferring from turns.
	return author, 0, nil
}

// lookupSessionAndClass resolves the URL-named AgentSession and its
// AgentClass through h.prefsReader — the shared first two lookups every
// preferences route needs. Class comes from the SESSION's own spec, never
// the request. Writes the HTTP error itself (404 on either CR missing, 500
// on any other read fault) and returns ok=false; callers must return
// immediately when ok is false.
func (h *handler) lookupSessionAndClass(w http.ResponseWriter, r *http.Request, scope memory.Scope, ns, name string) (v1alpha1.AgentSession, v1alpha1.AgentClass, bool) {
	var sess v1alpha1.AgentSession
	if err := h.prefsReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
		status := http.StatusInternalServerError
		if apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		log.FromContext(r.Context()).Info("memory: preferences AgentSession lookup failed",
			"session", scope.ID, "status", status, "err", err.Error())
		http.Error(w, "session lookup failed", status)
		return sess, v1alpha1.AgentClass{}, false
	}
	className := sess.Spec.Class

	var class v1alpha1.AgentClass
	if err := h.prefsReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: className}, &class); err != nil {
		status := http.StatusInternalServerError
		if apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		log.FromContext(r.Context()).Info("memory: preferences AgentClass lookup failed",
			"session", scope.ID, "class", className, "status", status, "err", err.Error())
		http.Error(w, "class lookup failed", status)
		return sess, class, false
	}
	return sess, class, true
}

// readGlobals is lookupGlobals' non-writing core: className's admin globals
// from the AgentSettings singleton, or nil when none is configured — an
// absent AgentSettings means "no globals for anything," not a fault, the
// same contract Resolve expects of its globals argument. It returns the read
// fault instead of answering it, for the one caller
// (handlePreferencesGetForUserRef) that must append an audit record BEFORE
// any response bytes go out.
func (h *handler) readGlobals(ctx context.Context, ns, className string) (map[string]v1alpha1.PreferenceGlobal, error) {
	var settings v1alpha1.AgentSettings
	switch err := h.prefsReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: v1alpha1.AgentSettingsName}, &settings); {
	case err == nil:
		return settings.Spec.ClassUserPreferences[className], nil
	case apierrors.IsNotFound(err):
		return nil, nil
	default:
		return nil, err
	}
}

// lookupGlobals wraps readGlobals for the routes with no audit obligation:
// writes the HTTP error itself on a read fault and returns ok=false.
func (h *handler) lookupGlobals(w http.ResponseWriter, r *http.Request, scope memory.Scope, ns, className string) (map[string]v1alpha1.PreferenceGlobal, bool) {
	globals, err := h.readGlobals(r.Context(), ns, className)
	if err != nil {
		log.FromContext(r.Context()).Info("memory: preferences AgentSettings lookup failed",
			"session", scope.ID, "err", err.Error())
		http.Error(w, "settings lookup failed", http.StatusInternalServerError)
		return nil, false
	}
	return globals, true
}

// isNullPreferenceValue reports whether v is absent, or present but carrying
// a JSON null — the two wire shapes CommitRequest's doc names as "clear the
// user layer". Mirrors preferences.isNull's definition (unexported in that
// package) rather than importing behavior across a package boundary for one
// predicate.
func isNullPreferenceValue(v *apiextv1.JSON) bool {
	if v == nil {
		return true
	}
	trimmed := bytes.TrimSpace(v.Raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

// handlePreferencesCommit serves POST /memory/_preferences_commit/{ns}/{name}:
// writes (or clears) one user's saved value for a class-declared preference
// key. channelsd-ONLY — see withComponentPrincipal's doc for why: channelsd
// is where a set-preference reply's human author gets verified, over the
// conversational surface, and the operator has no way to redo that check
// itself. A session bearer can read a session's OWN snapshot but must never
// reach this route, even for its own session: the write targets the
// ComponentWritten user_preference kind in the SUBJECT's user scope, keyed
// off a subject the request claims — trusting a session credential for that
// would let anyone able to talk to the session's own memory API set another
// person's saved preference.
//
// Class is NEVER taken from the request — only from the URL-named
// AgentSession's own spec — so a caller cannot commit a value into a class
// the session does not actually belong to.
func (h *handler) handlePreferencesCommit(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.prefsReader == nil {
		http.Error(w, "preferences not supported", http.StatusMethodNotAllowed)
		return
	}
	if principal, ok := componentPrincipalFrom(r.Context()); !ok || principal != "channelsd" {
		log.FromContext(r.Context()).Info("memory: preferences commit refused: caller is not the channelsd component",
			"session", scope.ID)
		http.Error(w, "forbidden: preferences commit requires the channelsd component token", http.StatusForbidden)
		return
	}
	ns, name, ok := strings.Cut(scope.ID, "/")
	if !ok {
		// scope.ID is always built as ns+"/"+name by ServeHTTP; unreachable in
		// practice, but a malformed scope must fail loud rather than panic on
		// the CR lookups below.
		log.FromContext(r.Context()).Info("memory: preferences commit malformed session scope", "scope", scope.ID)
		http.Error(w, "malformed session scope", http.StatusInternalServerError)
		return
	}

	defer r.Body.Close()
	var req preferences.CommitRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode CommitRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" || req.Subject == "" {
		http.Error(w, "key and subject are required", http.StatusBadRequest)
		return
	}
	userScope, err := memory.UserScope(req.Subject)
	if err != nil {
		http.Error(w, "bad subject: "+err.Error(), http.StatusBadRequest)
		return
	}

	sess, class, ok := h.lookupSessionAndClass(w, r, scope, ns, name)
	if !ok {
		return
	}
	className := sess.Spec.Class

	var schema *v1alpha1.UserPreferenceSchema
	for i := range class.Spec.UserPreferences {
		if class.Spec.UserPreferences[i].Name == req.Key {
			schema = &class.Spec.UserPreferences[i]
			break
		}
	}
	if schema == nil {
		log.FromContext(r.Context()).Info("memory: preferences commit for an undeclared key",
			"session", scope.ID, "class", className, "key", req.Key)
		http.Error(w, fmt.Sprintf("unknown preference key %q", req.Key), http.StatusNotFound)
		return
	}

	globals, ok := h.lookupGlobals(w, r, scope, ns, className)
	if !ok {
		return
	}
	// Precedence rule mirrors preferences.Resolve exactly: a global only
	// locks when it is BOTH marked Lock AND still valid against the current
	// schema. A global that no longer type-checks (the class schema changed
	// out from under it) is a violation Resolve reports and ignores at read
	// time, not a value anyone can be locked to — so it must not block a
	// commit either.
	if g, hasGlobal := globals[req.Key]; hasGlobal && g.Lock {
		if verr := preferences.ValidateValue(*schema, g.Value); verr == nil {
			log.FromContext(r.Context()).Info("memory: preferences commit refused: key is locked by admin policy",
				"session", scope.ID, "subject", req.Subject, "class", className, "key", req.Key)
			http.Error(w, fmt.Sprintf("key %q is locked by admin policy", req.Key), http.StatusConflict)
			return
		}
	}

	var valueBytes json.RawMessage
	if isNullPreferenceValue(req.Value) {
		// The cleared tombstone: written explicitly rather than passing
		// through whatever null-ish bytes the request happened to carry, so
		// every cleared entry's stored bytes are byte-identical.
		valueBytes = json.RawMessage("null")
	} else {
		if verr := preferences.ValidateValue(*schema, *req.Value); verr != nil {
			http.Error(w, "value: "+verr.Error(), http.StatusBadRequest)
			return
		}
		valueBytes = json.RawMessage(req.Value.Raw)
	}

	content, merr := json.Marshal(userpreference.Preference{
		ClassNamespace: ns,
		ClassName:      className,
		Key:            req.Key,
		Value:          valueBytes,
		SetViaSession:  ns + "/" + name,
	})
	if merr != nil {
		// Marshaling our own struct; unreachable in practice, but every error
		// path here must still answer and log rather than proceed on a
		// half-built entry.
		log.FromContext(r.Context()).Info("memory: preferences commit entry encode failed",
			"session", scope.ID, "subject", req.Subject, "class", className, "key", req.Key, "err", merr.Error())
		http.Error(w, "encode preference: "+merr.Error(), http.StatusInternalServerError)
		return
	}

	// SystemContext, not r.Context(): this write targets the SUBJECT's user
	// scope, not the calling channelsd token's own session/caller marks, and
	// user_preference is ComponentWritten — only the operator's own system
	// approval may author it (see the Kind's doc). Same pattern
	// handlePreferencesGet already uses for its own user-scope read.
	sysCtx := memory.SystemContext(r.Context(), "preferences-commit")
	if _, err := h.mem.Put(sysCtx, memory.Entry{
		Scope:   userScope,
		Kind:    userpreference.KindName,
		ID:      userpreference.EntryID(ns, className, req.Key),
		Content: content,
	}); err != nil {
		status := sentinelStatus(err)
		log.FromContext(r.Context()).Info("memory: preferences commit failed",
			"session", scope.ID, "subject", req.Subject, "class", className, "key", req.Key, "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePreferencesFirstPartyCommit serves
// POST /memory/_preferences_firstparty_commit/{ns}/{className}: the
// CLASS-addressed sibling of handlePreferencesCommit, for first-party UI
// surfaces with no AgentSession of their own (the Slack App Home today).
// channelsd-ONLY, for the same reason handlePreferencesCommit is: the
// human-confirm verification for a preference edit happens on channelsd's
// side of the conversational/UI surface, and the operator has no way to
// redo that check itself — it trusts channelsd's word for WHICH subject
// issued this request, exactly like the session-addressed commit trusts it
// for the request body's Subject field.
//
// {ns}/{className} names the AGENT CLASS directly, mirroring
// handlePreferencesFirstPartyGet — there is no AgentSession in this flow at
// all, so this does NOT go through lookupSessionAndClass; class is read
// straight off the URL via h.prefsReader.
//
// Reuses every validation/lock/write step handlePreferencesCommit already
// has, in the same order: key-in-schema (404), the admin-lock refusal (409,
// only when the locking global itself still type-checks against the
// current schema), preferences.ValidateValue (400), null-tombstone
// normalization, and the ComponentWritten user_preference write under
// SystemContext with a deterministic EntryID.
//
// NEW here: once the value lands, a preference_write audit entry is
// appended into the SUBJECT's own user scope (never the class scope) — the
// tamper-evident record of "this human edited this setting through this UI
// surface." h.prefWriteAudit == nil refuses the whole route with 503,
// mirroring WithPreferenceAudit's ?user-ref= refusal: an unaudited
// first-party write is a different, unaudited feature, not a degraded
// version of this one.
//
// Audit-then-success ordering: an audit append failure answers 500 even
// though the user_preference write already landed, because a caller that
// is told 204 has no reason to retry, and an untraceable edit is exactly
// what this audit trail exists to prevent. A retry after a 500 here is
// safe — the value write's ID is deterministic, so it re-lands the exact
// same value, and a second preference_write entry recording a second
// identical attempt is an honest duplicate, not a gap.
func (h *handler) handlePreferencesFirstPartyCommit(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.prefsReader == nil {
		http.Error(w, "preferences not supported", http.StatusMethodNotAllowed)
		return
	}
	if h.prefWriteAudit == nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party commit refused: no audit writer configured",
			"scope", scope.ID)
		http.Error(w, "first-party preferences commits require the audit writer, which is not configured on this cluster", http.StatusServiceUnavailable)
		return
	}
	if principal, ok := componentPrincipalFrom(r.Context()); !ok || principal != "channelsd" {
		log.FromContext(r.Context()).Info("memory: preferences first-party commit refused: caller is not the channelsd component",
			"scope", scope.ID)
		http.Error(w, "forbidden: first-party preferences commit requires the channelsd component token", http.StatusForbidden)
		return
	}
	ns, className, ok := strings.Cut(scope.ID, "/")
	if !ok {
		// scope.ID is always built as ns+"/"+name by ServeHTTP; unreachable in
		// practice, but a malformed scope must fail loud rather than panic on
		// the CR lookup below.
		log.FromContext(r.Context()).Info("memory: preferences first-party commit malformed class scope", "scope", scope.ID)
		http.Error(w, "malformed class scope", http.StatusInternalServerError)
		return
	}

	defer r.Body.Close()
	var req preferences.CommitRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode CommitRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" || req.Subject == "" {
		http.Error(w, "key and subject are required", http.StatusBadRequest)
		return
	}
	userScope, err := memory.UserScope(req.Subject)
	if err != nil {
		http.Error(w, "bad subject: "+err.Error(), http.StatusBadRequest)
		return
	}

	var class v1alpha1.AgentClass
	if err := h.prefsReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: className}, &class); err != nil {
		status := http.StatusInternalServerError
		if apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		log.FromContext(r.Context()).Info("memory: preferences first-party commit AgentClass lookup failed",
			"ns", ns, "class", className, "status", status, "err", err.Error())
		http.Error(w, "class lookup failed", status)
		return
	}

	var schema *v1alpha1.UserPreferenceSchema
	for i := range class.Spec.UserPreferences {
		if class.Spec.UserPreferences[i].Name == req.Key {
			schema = &class.Spec.UserPreferences[i]
			break
		}
	}
	if schema == nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party commit for an undeclared key",
			"ns", ns, "class", className, "key", req.Key)
		http.Error(w, fmt.Sprintf("unknown preference key %q", req.Key), http.StatusNotFound)
		return
	}

	globals, gerr := h.readGlobals(r.Context(), ns, className)
	if gerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party commit AgentSettings lookup failed",
			"ns", ns, "class", className, "err", gerr.Error())
		http.Error(w, "settings lookup failed", http.StatusInternalServerError)
		return
	}
	// Precedence rule mirrors handlePreferencesCommit exactly: a global only
	// locks when it is BOTH marked Lock AND still valid against the current
	// schema — a global that no longer type-checks must not block a commit.
	if g, hasGlobal := globals[req.Key]; hasGlobal && g.Lock {
		if verr := preferences.ValidateValue(*schema, g.Value); verr == nil {
			log.FromContext(r.Context()).Info("memory: preferences first-party commit refused: key is locked by admin policy",
				"ns", ns, "subject", req.Subject, "class", className, "key", req.Key)
			http.Error(w, fmt.Sprintf("key %q is locked by admin policy", req.Key), http.StatusConflict)
			return
		}
	}

	var valueBytes json.RawMessage
	if isNullPreferenceValue(req.Value) {
		// The cleared tombstone: written explicitly rather than passing through
		// whatever null-ish bytes the request happened to carry, so every
		// cleared entry's stored bytes are byte-identical.
		valueBytes = json.RawMessage("null")
	} else {
		if verr := preferences.ValidateValue(*schema, *req.Value); verr != nil {
			http.Error(w, "value: "+verr.Error(), http.StatusBadRequest)
			return
		}
		valueBytes = json.RawMessage(req.Value.Raw)
	}

	content, merr := json.Marshal(userpreference.Preference{
		ClassNamespace: ns,
		ClassName:      className,
		Key:            req.Key,
		Value:          valueBytes,
		// SetViaSession stays empty: this value was set through the class-
		// addressed first-party route, not through any AgentSession.
	})
	if merr != nil {
		// Marshaling our own struct; unreachable in practice, but every error
		// path here must still answer and log rather than proceed on a
		// half-built entry.
		log.FromContext(r.Context()).Info("memory: preferences first-party commit entry encode failed",
			"ns", ns, "subject", req.Subject, "class", className, "key", req.Key, "err", merr.Error())
		http.Error(w, "encode preference: "+merr.Error(), http.StatusInternalServerError)
		return
	}

	// SystemContext, not r.Context(): this write targets the SUBJECT's user
	// scope, not the calling channelsd token's own caller marks, and
	// user_preference is ComponentWritten — only the operator's own system
	// approval may author it. Same pattern handlePreferencesCommit already
	// uses for its own user-scope write.
	sysCtx := memory.SystemContext(r.Context(), "preferences-firstparty-commit")
	if _, err := h.mem.Put(sysCtx, memory.Entry{
		Scope:   userScope,
		Kind:    userpreference.KindName,
		ID:      userpreference.EntryID(ns, className, req.Key),
		Content: content,
	}); err != nil {
		status := sentinelStatus(err)
		log.FromContext(r.Context()).Info("memory: preferences first-party commit failed",
			"ns", ns, "subject", req.Subject, "class", className, "key", req.Key, "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}

	// THE audit append — see the function doc for why a failure here still
	// answers 500 rather than reporting success for a write the trail never
	// recorded, even though the value write above already landed.
	if aerr := preferencewrite.Record(sysCtx, h.prefWriteAudit, userScope, preferencewrite.Content{
		Subject:        req.Subject,
		ClassNamespace: ns,
		ClassName:      className,
		Key:            req.Key,
		Value:          valueBytes,
		Via:            "app-home",
	}); aerr != nil {
		log.FromContext(r.Context()).Info("memory: preferences first-party commit audit write failed after a successful value write",
			"ns", ns, "subject", req.Subject, "class", className, "key", req.Key, "err", aerr.Error())
		httpError(w, fmt.Errorf("record preference-write audit: %w", aerr), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
