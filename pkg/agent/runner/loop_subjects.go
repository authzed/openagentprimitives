package runner

import (
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ResolveAuthSubjects populates l.authSubject / l.authSubjects from the
// AgentSession annotations and AgentClass.spec.toolAuthSubject. Called by
// internal/cmd/runner/main.go after the session and class are fetched, before Run.
// No-op when AuthzCli is nil (authz layer disabled).
//
// LastInboundCanonicalIDAnnotationKey (slack) is already canonical;
// AnnotationStartedByExternalID is the raw external ID from session creation
// and must be re-canonicalized using the channel kind, producing (absent an
// email) the synthetic base64(kind:teamScope:externalID) canonical.
//
// Neither annotation exists on a session whose inbound carried no human at
// all — a webhook delivery, a cron tick — so the last word goes to the service
// subject its input Channel declared for exactly this purpose (see
// serviceSubjectFor).
//
// kubectl-driven sessions have none of the three and fall through to the empty
// string, so readonly/readwrite/external tool calls deny at the dispatch site
// (toolcheck refuses to build a check with no acting subject, in words). The
// only sanctioned bypass is AgentClass.spec.authz.toolCalls.mode="disabled".
func (l *Loop) ResolveAuthSubjects(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) {
	if l.AuthzCli == nil {
		return
	}
	mode := class.Spec.GetAuthz().GetToolCalls().Subject
	if mode == "" {
		mode = "currentRequester"
	}

	// currentRequester: set by channelsd's Slack listener on every inbound.
	currentReq := sess.Annotations[slack.LastInboundCanonicalIDAnnotationKey]

	// startedBy: the pipeline-stamped canonical when there is one, else a
	// synthetic derived from the external id. ResolveStartedByCanonical owns
	// that precedence for every consumer — see its doc for why the two
	// encodings must not be substituted for one another.
	startedByCanonical := spiceboxv1alpha1.ResolveStartedByCanonical(sess)

	l.authSubjectMode = mode
	switch mode {
	case "currentRequester":
		// The current requester annotation, written by the operator from the
		// inbound the channel resolved.
		l.authSubject = identity.CanonicalFromTrusted(currentReq,
			"operator-written current-requester annotation")
		if l.authSubject.IsZero() {
			// Fallback: kubectl-driven sessions have no inbound annotation.
			l.authSubject = startedByCanonical
		}
	case "startedBy":
		l.authSubject = startedByCanonical
	case "both":
		l.authSubjectStartedBy = startedByCanonical
		l.setBothSubjects(currentReq)
	}

	// Final fallback, after every mode has had its say: a session with no human
	// on any leg acts as the non-human subject its input Channel declared.
	//
	// It stamps only the singular ACTING principal, never the "both"-mode
	// subject SET. That set's elements are human legs and the check requires
	// every one of them to allow; appending a service subject would claim a
	// human standing nobody granted. With the set empty — which is exactly when
	// this branch runs, since setBothSubjects drops empties — toolcheck reads
	// the singular Subject, so the fallback still reaches the check.
	//
	// advanceRequester never has to undo this: it no-ops on an empty Author,
	// which is every turn of a session with no human in it, and a human turn
	// that does arrive SHOULD take the subject over.
	if l.authSubject.IsZero() {
		l.authSubject = serviceSubjectFor(sess, class)
	}
}

// serviceSubjectFor returns the non-human subject this session acts as, or ""
// when it has none.
//
// THE SESSION IS THE DEFAULT; a declared service is the OVERRIDE. When a
// userless session's input Channel names no `service:<id>`, the session acts
// as ITSELF — `agentsession:<ns>/<name>`.
//
// The question a non-human principal has to answer is what the thing being
// granted TO actually is. A declared service is a name an operator typed into
// a wizard: no lifecycle, nothing that scopes it to the work it authorizes,
// and nothing that reaps it. The session has all three already. Making it the
// principal is what turns session-only standing from a rule somebody enforces
// into a STRUCTURAL fact — the principal cannot outlive the session because it
// IS the session, and revoking it is deleting it.
//
// It also makes the principal grantable at all. `definition service {}` is
// deliberately relation-less: every check against a service subject resolves
// to no permission, so a userless session could be REFUSED precisely and never
// ALLOWED anything. An agentsession subject is already admitted where a
// non-human principal should be able to hold authority — resource
// `slot_grant_<perm>` relations are agentsession-typed (authz.SlotGrantRelation)
// — so the existing grant mechanism starts working for these sessions with no
// new schema.
//
// It grants nothing by itself. Standing still requires somebody to write a
// slot grant naming this session, which is the same deliberate act the service
// definition's own comment describes; what changes is that the principal named
// by that grant now expires with the work it was issued for.
//
// WHY THE OVERRIDE STAYS. A declared service is not a legacy path being
// retired. Several sessions of one class sharing one durable principal is a
// real thing to want — a fleet of webhook-triggered runs holding one standing
// grant — and it is exactly what a per-session principal cannot express. The
// default changes; the choice does not go away.
//
// Both halves below must still agree, and each answers a different question:
//
//   - AgentClass.status.userlessInput — "can a session of this class be BORN
//     with no human on it?" The AgentClass reconciler derives it once over the
//     bound Channels (registry.IsUserlessInput AND SpawnsSessionOnInbound, in
//     agentclass.isUserlessSessionSource); nothing re-derives it. A class whose
//     input IS attributable must never substitute a service identity for a
//     requester that went missing: there, an absent requester means attribution
//     was LOST, and losing attribution has to deny.
//   - the session's own AnnotationAuthzServiceSubject — "was THIS session's
//     inbound the userless one?" A class with both a webhook input and a Slack
//     input has the derived bool set for every session it spawns, including the
//     Slack ones; only the sessions channelsd actually created from a userless
//     inbound carry the annotation.
//
// Requiring both is what keeps a class-level fact from answering a
// per-session question. Either alone fails open in one direction.
func serviceSubjectFor(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) identity.CanonicalUserID {
	if class == nil || !class.Status.UserlessInput {
		return identity.CanonicalUserID{}
	}
	if declared := spiceboxv1alpha1.AuthzServiceSubject(sess); !declared.IsZero() {
		return declared
	}
	return sessionSelfSubject(sess)
}

// sessionSelfSubject is the session acting in its own right.
//
// The object id is "<ns>/<name>" because that is what authz.SlotGrantRelation
// already writes as the subject id of a per-resource grant. Deriving it a
// second way here would produce a principal that looks right in a log and
// matches no tuple anybody ever wrote — the failure mode is a silent, total
// deny that reads like a policy decision.
//
// Returned as a fully-qualified "agentsession:<id>" reference, not a bare
// canonical. identity.Subject treats any "<type>:<rest>" value as a reference
// of its own type, and the tool-call gate's subjectObject splits on that first
// colon — so this needs no new plumbing, and must NOT be prefixed with "user:"
// anywhere downstream (which is the trap that comment in check_tool_call.go
// exists to prevent).
func sessionSelfSubject(sess *spiceboxv1alpha1.AgentSession) identity.CanonicalUserID {
	if sess == nil || sess.Namespace == "" || sess.Name == "" {
		// A session that cannot name itself must not fall back to some other
		// principal. Empty means no acting subject, which denies at dispatch.
		return identity.CanonicalUserID{}
	}
	// The platform derives this from the session object itself, so it is as
	// established as any id gets — nobody asserted it.
	return identity.CanonicalFromTrusted("agentsession:"+sess.Namespace+"/"+sess.Name,
		"derived by the platform from the session's own namespace/name")
}

// setBothSubjects rebuilds l.authSubjects from the given current-requester
// canonical plus the frozen l.authSubjectStartedBy, dropping empty elements,
// and stamps the acting principal onto l.authSubject. Called at session start
// (ResolveAuthSubjects) and again by advanceRequester on each later inbound.
//
// "both" mode needs BOTH fields, which pkg/platform/pipeline carries separately
// (see the Input.Requester/Subjects contract in pkg/platform/pipeline/types.go).
// authSubjects is the authorization SET, the only input the tool-call check
// consults: authz.check prefers Inputs.Subjects over Inputs.Subject whenever it
// is non-empty, which is exactly when this mode is active. authSubject is the
// singular ACTING principal, threaded by dispatchToolUses as
// pipeline.Input.Requester — the leakage read gate, the leakage notice's
// addressee, the authz-decision ledger and BindClassDefaults each need one
// actor and have no set-shaped answer. Leaving it unset gives every LLM-path
// tool call an empty Requester, which the read gate treats as unevaluable and,
// under enforcing leakage, fails closed on; setting it cannot widen
// authorization, since the check ignores Subject while Subjects is populated.
//
// Assigning the acting principal HERE rather than in ResolveAuthSubjects is what
// keeps it advancing: advanceRequester re-enters through this function on every
// drained turn, so attribution follows the live sender instead of freezing on
// the initiator (still the fallback until an inbound annotation exists).
func (l *Loop) setBothSubjects(currentReq string) {
	raw := []string{currentReq, l.authSubjectStartedBy.String()}
	filtered := raw[:0]
	for _, s := range raw {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	l.authSubjects = filtered

	l.authSubject = identity.CanonicalFromTrusted(currentReq,
		"operator-written current-requester annotation")
	if l.authSubject.IsZero() {
		l.authSubject = l.authSubjectStartedBy
	}
}

// advanceRequester moves the current-requester component of the resolved
// tool-call auth subject(s) to author's canonical, so tool calls dispatched
// after this point authorize as the CURRENT requester rather than whichever
// subject was frozen at session start. Called by drainInbox for every
// drained inbound turn that carries a non-empty Author.
//
// One runner pod serves an entire multiplayer channel/thread across many
// await_user_message parks; without this advance the subject stays pinned to
// the session's original initiator for its whole lifetime, so a
// lower-privileged participant's tool calls would silently authorize against
// the initiator's grants — a privilege escalation. See the comment on the
// Loop.authSubject field. Limitation: when several inbox turns drain in one
// batch, only the LAST author becomes the subject for the whole batch; there
// is no per-tool-call attribution back to the turn that produced each call.
//
// No-op when AuthzCli is nil (authz disabled), author is empty (a system- or
// agent-authored turn: keep the prior subject rather than clearing it), or
// authSubjectMode is "startedBy" (the frozen initiator is that mode's subject).
func (l *Loop) advanceRequester(author identity.Subject) {
	if l.AuthzCli == nil || author.Empty() {
		return
	}
	canonical := strings.TrimPrefix(author.String(), "user:")
	switch l.authSubjectMode {
	case "currentRequester":
		// Derived from the turn Author the channel resolved server-side.
		l.authSubject = identity.CanonicalFromTrusted(canonical,
			"per-turn author resolved by the channel")
	case "both":
		l.setBothSubjects(canonical)
	}
}

// AuthSubject returns the canonical SpiceDB subject ID of the acting principal
// resolved by ResolveAuthSubjects, in every subject mode. Used by internal/cmd/runner
// to feed BindDefaults at session start.
func (l *Loop) AuthSubject() string { return l.authSubject.String() }

// authorForStarter returns the per-turn Author subject for the session's
// initial-prompt (index-0) turn. StartedByCanonical is stored BARE (no "user:"
// prefix — see internal/cmd/runner/main.go's startedByCanonical()), so this prepends the
// scheme to match identity.Subject's format. Empty when there is no human
// starter (kubectl/bento-driven sessions), leaving the turn's Author unset.
func (l *Loop) authorForStarter() identity.Subject {
	if l.StartedByCanonical.IsZero() {
		return ""
	}
	return l.StartedByCanonical.Subject()
}

// setCurrentUserTurnIndex records the transcript Index of the most recently
// drained user turn. Stored as (i + 1) so the underlying atomic's zero value
// — a bare Loop, or a Run that has not yet drained any user turn — reads back
// as -1 via CurrentUserTurnIndex, never a false turn 0.
//
// Called from two places, both documented on advanceRequester/drainInbox:
// Run's start-of-session scan (highestUserTurnIndex over the transcript it
// already reads at start) and drainInbox beside every advanceRequester call
// (the turn it just placed). Both answer the same question — "what is the
// most recent turn a human actually sent?" — at the two moments that fact can
// change.
func (l *Loop) setCurrentUserTurnIndex(i int) {
	l.currentUserTurnIndex.Store(int64(i) + 1)
}

// CurrentUserTurnIndex returns the transcript Index of the most recently
// drained user turn, or -1 when none is known yet (a bare Loop, or a session
// whose first user turn has not been drained). preferencesReader captures
// this as its turnIndex func so a preference read scopes to what the CURRENT
// turn's author has seen, not to whichever turn happened to be current when
// the tool table was assembled.
func (l *Loop) CurrentUserTurnIndex() int {
	v := l.currentUserTurnIndex.Load()
	if v == 0 {
		return -1
	}
	return int(v - 1)
}

// highestUserTurnIndex scans a transcript (the same []memory.Turn Run reads
// via l.Memory.ReadAll at start) for the highest Index among turns a HUMAN
// authored — role "user" AND a non-empty Author. Used to seed
// currentUserTurnIndex on a resumed session, so a runner restart does not
// report "unknown" for a conversation that has plainly had a human turn
// already. Returns -1 when prior carries no human turn (a genuinely cold
// session, before turn 0 lands, or one driven with no human author).
//
// The Author test is load-bearing: tool-result turns are stored role "user"
// too (the LLM protocol models a tool result as a user-role message) but carry
// no Author. This seed feeds only the preference read (via CurrentUserTurnIndex
// → preferencesReader), which resolves the CURRENT human author's saved
// values. Counting a tool_result here would, on any resume of a session mid
// tool-loop, seed ?turn= to a tool_result index whose author is empty, and
// every preference read would then fall through to the class default — the same
// class of bug resolvePreferencesTurnAuthor guards against on the no-?turn path.
//
// TODO(session-identity): both this seed and the operator-side turn-author
// resolution are transcript-scanning heuristics standing in for real session
// identity. The write path already resolves a verified canonical subject; the
// read path should consult a first-class session-identity/participant record
// rather than re-deriving "who is this for" by scanning turns.
func highestUserTurnIndex(prior []memory.Turn) int {
	idx := -1
	for _, t := range prior {
		if t.Role == "user" && !t.Author.Empty() && t.Index > idx {
			idx = t.Index
		}
	}
	return idx
}
