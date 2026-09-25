// pkg/controllers/relationshipsource/monitoring.go
package relationshipsource

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// Monitoring categories — see channelevents.MonitoringEvent's own doc for
// the closed-ish set ("credential" | "reconcile" | "session" | "transport").
const (
	monitoringCategoryCredential = "credential"
	monitoringCategoryReconcile  = "reconcile"
)

// passIssue is one kind of non-fatal-to-Ready fact a sync pass can report,
// repeatedly, across reconciles. Ready stays True/Synced through every one
// of these (relsync.Pass's own doc: "one scope failing is not a pass
// failure") — so, unlike the credential-resolve/kind-claimed failures
// below, nothing in the CR's persisted status distinguishes "this pass had
// a guard refusal" from "this pass was clean". That is exactly what
// monitoringTracker exists to remember instead, and exactly why each is
// reported once per TRANSITION rather than once per pass: a source with a
// standing guard-refusal bug would otherwise alert on every single
// interval, forever, which is how people learn to filter the channel.
type passIssue string

const (
	// issueGuardRefusal: relsource.CheckWrite/CheckDeleteFilter refused a
	// write this pass attempted — the kind's own declared claims disagree
	// with what it tried to write. A bug in the kind, not a runtime
	// condition, hence Error.
	issueGuardRefusal passIssue = "guard-refusal"
	// issueCASFailure: SpiceDB refused a write because the sentinel
	// precondition no longer matched (an out-of-band write raced this
	// pass's own read-diff-write). Error: the sync's own compare-and-swap
	// invariant was violated.
	issueCASFailure passIssue = "cas-failure"
	// issueRefusedPrune: a complete enumeration reported zero scopes, and
	// step 5 refused to reap rather than trust that as "genuinely empty".
	// Warning: nothing was deleted, so this is a caution, not a fault.
	issueRefusedPrune passIssue = "refused-prune"
	// issueScopeFailure: any other single scope's fetch/read/write error
	// (a transient upstream hiccup, a timeout). Warning: relsync.Pass's own
	// doc is explicit that this is never fatal to the pass as a whole.
	//
	// Classified but NOT emitted — see emittedPassIssues. It remains the
	// default bucket classifyScopeError falls through to, which is what keeps
	// the three buckets above meaning exactly what they say.
	issueScopeFailure passIssue = "scope-failure"
	// issueJoinMisses: PassResult.JoinMisses — upstream members whose
	// identity resolved to no platform user, dropped rather than written.
	// Warning: tracked by COUNT rather than presence, since "17 of 20
	// joined" changing to "12 of 20" is itself worth a fresh report even
	// though misses never cleared to zero.
	issueJoinMisses passIssue = "join-misses"
)

// level is the MonitoringEvent.Level this issue reports at, both on Failed
// and on its own Recovered (channelevents.MonitoringEvent's own doc: "A
// recovery event carries the SAME level as the failure it resolves").
func (i passIssue) level() string {
	switch i {
	case issueGuardRefusal, issueCASFailure:
		return channelevents.MonitoringLevelError
	default:
		return channelevents.MonitoringLevelWarning
	}
}

// condition is the synthetic MonitoringEvent.Condition label for this
// issue. None of these name a real status Condition Type on
// RelationshipSource — Ready stays True throughout every one of them (see
// the passIssue doc) — so, like
// pkg/controllers/useridentity/attested_edge.go's "AttestedIdentity", this
// is a descriptive fact label, not a CRD condition an operator could find
// on `kubectl describe`.
func (i passIssue) condition() string {
	switch i {
	case issueGuardRefusal:
		return "WriteGuard"
	case issueCASFailure:
		return "WritePrecondition"
	case issueRefusedPrune:
		return "Reap"
	case issueJoinMisses:
		return "MembershipJoin"
	default:
		return "ScopeSync"
	}
}

// reason is the MonitoringEvent.Reason for this issue.
func (i passIssue) reason() string {
	switch i {
	case issueGuardRefusal:
		return "RelationOwnedByAnotherSource"
	case issueCASFailure:
		return "PreconditionMismatch"
	case issueRefusedPrune:
		return "ReapRefused"
	case issueJoinMisses:
		return "MembershipJoinMisses"
	default:
		return "ScopeFailed"
	}
}

// label is a short human phrase for this issue, used only to build a
// Recovered summary where there is no longer an error to quote.
func (i passIssue) label() string {
	switch i {
	case issueGuardRefusal:
		return "a write-guard refusal"
	case issueCASFailure:
		return "a write-precondition (CAS) failure"
	case issueRefusedPrune:
		return "a refused prune"
	case issueJoinMisses:
		return "join misses"
	default:
		return "a scope failure"
	}
}

// classifyScopeError buckets one relsync.ScopeError.Err into the passIssue
// it reports as. Every branch is a structured check — a gRPC status code or
// an exported sentinel — never a match against message text, so a future
// rewording of any of these packages' error strings cannot silently break
// this classification (fix round 1: the previous version matched
// "relsource:"/"refusing to reap" substrings, which also mis-tagged
// relsource.ErrClaimTableIncomplete — a binary-wiring bug — as an ordinary
// guard refusal, since its message happens to share the same prefix).
//
//   - codes.FailedPrecondition is checked first: it survives
//     fmt.Errorf("write: %w", …) wrapping via errors.As inside
//     google.golang.org/grpc/status.FromError (confirmed against the
//     go.mod-pinned grpc-go version).
//   - relsource.ErrRefused / relsync.ErrRefusedPrune are checked with
//     errors.Is, which walks the SAME %w chain.
func classifyScopeError(err error) passIssue {
	if status.Code(err) == codes.FailedPrecondition {
		return issueCASFailure
	}
	if errors.Is(err, relsource.ErrRefused) {
		return issueGuardRefusal
	}
	if errors.Is(err, relsync.ErrRefusedPrune) {
		return issueRefusedPrune
	}
	return issueScopeFailure
}

// scopeSet is the set of relsync.ScopeID values currently failing with one
// passIssue. An empty (or nil) set means the issue is not present this
// pass. The refused-prune issue is scope-less (relsync reports it with
// Scope == "" — see ScopeError's own doc), so it only ever populates this
// set with the single key ""; that degenerates correctly to plain
// present/absent tracking for that one issue.
type scopeSet map[relsync.ScopeID]struct{}

// sortScopeIDs sorts ids in place and returns it, for a deterministic
// Summary string.
func sortScopeIDs(ids []relsync.ScopeID) []relsync.ScopeID {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// passIssueState is the per-(namespace,name) bookkeeping monitoringTracker
// keeps: the SET of scope IDs failing with each issue on the last pass this
// process observed (fix round 1 — see observeIssue's doc for why a bare
// bool undercounted), and the last reported JoinMisses count.
type passIssueState struct {
	failing         map[passIssue]scopeSet
	joinMisses      int
	joinMissesKnown bool
}

// monitoringTracker is process-local, in-memory bookkeeping for the
// pass-level issues above — mirrors passPacer's own rationale
// (AGENTS.md's durability lens): losing it on an operator restart
// re-announces an already-active issue at most once more; it decides
// REPORTING cadence, never what was actually written to SpiceDB or the
// CR's own status, neither of which this tracker touches.
type monitoringTracker struct {
	mu    sync.Mutex
	state map[types.NamespacedName]*passIssueState
}

func (t *monitoringTracker) stateFor(key types.NamespacedName) *passIssueState {
	if t.state == nil {
		t.state = map[types.NamespacedName]*passIssueState{}
	}
	s, ok := t.state[key]
	if !ok {
		s = &passIssueState{failing: map[passIssue]scopeSet{}}
		t.state[key] = s
	}
	return s
}

// observeIssue records the SET of scope IDs failing with issue on this
// pass (current may be nil/empty, meaning none) and reports the scopes
// that CHANGED STATE relative to the last pass this process observed,
// split into recoveredIDs (were failing, now aren't) and failedIDs
// (weren't failing, now are) — a scope holding steady, failing or not,
// appears in neither.
//
// Fix round 1: a single bool ("is anything failing") — or a single
// Failed/Recovered verdict for the whole set — under-reported. Probe:
// pass 1, scope A fails → Failed(A), correct. Pass 2, A recovers AND a
// DIFFERENT scope B starts failing IN THE SAME PASS → a bool never
// changes (something was failing before, something is failing now), so a
// boolean tracker emits NOTHING: no Recovered for A, no Failed for B, and
// an operator keeps staring at pass 1's stale alert describing a scope
// that is now fine while a different one silently broke. Even a
// whole-set-level Failed/Recovered verdict gets this wrong: the set
// changed ({A}→{B}), so it would emit ONE Failed(B) — new information
// about B, but still no Recovered for A, so the stale alert about A is
// never retracted. Reporting the two DISJOINT deltas separately is what
// makes both facts visible in the one pass they both happened: a scope
// recovering is not the same fact as a different scope failing, even when
// they land in the same Pass, and both need their own transition.
//
// A cold-start read of an unseen key returns a nil scopeSet (zero value),
// which behaves exactly like any other empty set for the range loops
// below — so a first observation that IS failing produces failedIDs
// (nothing in a nil `was` to range over) and an active issue still
// survives an operator restart's report; a first observation that ISN'T
// failing produces neither list. No separate "known" bookkeeping needed.
func (t *monitoringTracker) observeIssue(key types.NamespacedName, issue passIssue, current scopeSet) (recoveredIDs, failedIDs []relsync.ScopeID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stateFor(key)
	was := s.failing[issue]
	s.failing[issue] = current
	for id := range was {
		if _, stillFailing := current[id]; !stillFailing {
			recoveredIDs = append(recoveredIDs, id)
		}
	}
	for id := range current {
		if _, wasFailing := was[id]; !wasFailing {
			failedIDs = append(failedIDs, id)
		}
	}
	return sortScopeIDs(recoveredIDs), sortScopeIDs(failedIDs)
}

// observeJoinMisses records this pass's JoinMisses count for key and
// reports whether it CHANGED — not whether it is merely nonzero. "17 of 20
// joined" holding steady at 17 must not alert every interval, but 17
// becoming 12 is new information worth a fresh report even though misses
// never cleared to zero; only a transition to/from exactly zero is
// expressed as Failed/Recovered (MonitoringEvent has no third transition
// value), a changed-but-still-nonzero count is reported as Failed again
// with the new count in the summary.
func (t *monitoringTracker) observeJoinMisses(key types.NamespacedName, misses int) (emit bool, transition string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stateFor(key)
	if s.joinMissesKnown && s.joinMisses == misses {
		return false, ""
	}
	prev, prevKnown := s.joinMisses, s.joinMissesKnown
	s.joinMisses, s.joinMissesKnown = misses, true
	if misses == 0 {
		if prevKnown && prev > 0 {
			return true, channelevents.MonitoringTransitionRecovered
		}
		// Cold start at zero, or an already-clean pass whose count simply
		// hadn't been recorded yet: nothing regressed, nothing to report.
		return false, ""
	}
	return true, channelevents.MonitoringTransitionFailed
}

// emittedPassIssues is the subset of passIssue publishPassIssues actually
// reports on the bus, in report order. Every scope error is still CLASSIFIED
// (and still logged, unconditionally, by Reconcile — no-silent-errors does not
// bend for a reporting decision); this list is only about what reaches a
// role=monitoring Channel.
//
// issueScopeFailure is deliberately absent, and it is the only omission. It is
// the bucket an ordinary per-scope fetch failure falls into, and that bucket is
// reported ONCE PER SOURCE by RelationshipSourceConditionPartialFailure through
// pkg/controllers/monitoring's Targets table instead. Two things settled it:
//
//   - VOLUME. These events are per SCOPE, and the failure that motivated the
//     condition was 156 repositories refusing with one root cause: one revoked
//     permission. That is 156 chat messages saying the same thing, which is how
//     people learn to mute the channel. The condition posts one.
//   - DURABILITY, which is NOT a reason to keep both. monitoringTracker is
//     process-local, and observeIssue's cold-start read of an unseen key
//     returns a nil scopeSet, so after an operator restart every currently
//     failing scope reads as newly failing and re-announces — exactly what the
//     condition-driven watcher does from durable status. Keeping both bought
//     no restart coverage, only the duplicate.
//
// The other three stay here rather than moving to a condition, because each is
// a distinct DEFECT rather than a degree of the same one — a guard refusal is a
// bug in a kind's declared claims, a CAS failure is a violated write invariant,
// a refused prune is the reaper declining to trust an empty enumeration — and
// each needs the scope named to be actionable. Folding them into one
// "something was wrong" condition would lose the thing an operator acts on.
var emittedPassIssues = []passIssue{issueGuardRefusal, issueCASFailure, issueRefusedPrune}

// publishPassIssues classifies res's ScopeErrors and JoinMisses into the
// passIssue buckets above, and reports every TRANSITION monitoringTracker
// detects for src. Called once per successful Pass (never on the
// early-return failure paths, where no Pass ran and res is meaningless).
func (r *Reconciler) publishPassIssues(ctx context.Context, src *v1.RelationshipSource, res relsync.PassResult) {
	key := types.NamespacedName{Namespace: src.Namespace, Name: src.Name}

	byIssue := map[passIssue]scopeSet{}
	// errByIssueScope keeps one representative ScopeError per (issue,
	// scope) so a Failed summary can quote real error text for one of the
	// NEWLY failing scopes specifically, not just any error classified
	// into the same issue this pass.
	errByIssueScope := map[passIssue]map[relsync.ScopeID]relsync.ScopeError{}
	for _, se := range res.ScopeErrors {
		issue := classifyScopeError(se.Err)
		if byIssue[issue] == nil {
			byIssue[issue] = scopeSet{}
			errByIssueScope[issue] = map[relsync.ScopeID]relsync.ScopeError{}
		}
		byIssue[issue][se.Scope] = struct{}{}
		errByIssueScope[issue][se.Scope] = se
	}

	for _, issue := range emittedPassIssues {
		recoveredIDs, failedIDs := r.monitoring.observeIssue(key, issue, byIssue[issue])
		// Both can fire from the SAME pass — see observeIssue's doc: a scope
		// recovering and a different scope newly failing are two disjoint
		// facts, not one verdict about the issue as a whole.
		if len(recoveredIDs) > 0 {
			r.publishPassEvent(ctx, src, issue, channelevents.MonitoringTransitionRecovered,
				scopeRecoveredSummary(issue, recoveredIDs))
		}
		if len(failedIDs) > 0 {
			r.publishPassEvent(ctx, src, issue, channelevents.MonitoringTransitionFailed,
				scopeFailedSummary(issue, failedIDs, errByIssueScope[issue]))
		}
	}

	if emit, transition := r.monitoring.observeJoinMisses(key, res.JoinMisses); emit {
		r.publishPassEvent(ctx, src, issueJoinMisses, transition, joinMissesSummary(res.JoinMisses))
	}
}

// scopeRecoveredSummary builds the Summary text for the scopes in ids that
// just stopped failing with issue.
func scopeRecoveredSummary(issue passIssue, ids []relsync.ScopeID) string {
	if issue == issueRefusedPrune {
		// Scope-less (relsync reports this with Scope == ""; ids is always
		// [""] here) — naming a "scope" would be misleading, not informative.
		return fmt.Sprintf("%s cleared: this pass reported none", issue.label())
	}
	return fmt.Sprintf("%s cleared for %d scope(s): %v", issue.label(), len(ids), ids)
}

// scopeFailedSummary builds the Summary text for the scopes in ids that
// newly started failing with issue this pass. errs maps a scope ID to its
// ScopeError, so the message can quote real error text for one of THESE
// scopes specifically — not an arbitrary error the issue happened to also
// have this pass.
//
// Scrubbed like every other quoted error, even though the emitted buckets
// (emittedPassIssues) today hold only errors this codebase generates itself —
// a relsource refusal, a gRPC precondition, relsync's own refused prune. That
// is an accident of which buckets emit, not a property of this function: it is
// handed whatever classifyScopeError put in the bucket, classification is by
// sentinel rather than by origin, and a kind error that happens to wrap one of
// those sentinels lands here quoting an upstream URL. Relying on the accident
// would put the check one refactor away from being wrong, on the path that
// broadcasts to a chat channel.
func scopeFailedSummary(issue passIssue, ids []relsync.ScopeID, errs map[relsync.ScopeID]relsync.ScopeError) string {
	var first string
	if se, ok := errs[ids[0]]; ok {
		first = scrubScopeErrorMessage(se.Error())
	}
	if issue == issueRefusedPrune {
		return fmt.Sprintf("this pass refused to reap: %s", first)
	}
	return fmt.Sprintf("%d new scope(s) failing with %s this pass (%v); first: %s",
		len(ids), issue.label(), ids, first)
}

// joinMissesSummary builds the Summary text for a JoinMisses report.
func joinMissesSummary(misses int) string {
	if misses == 0 {
		return "join misses cleared: every upstream member this pass fetched resolved to a platform user"
	}
	return fmt.Sprintf("%d upstream member(s) this pass fetched resolved to no platform user and were dropped", misses)
}

// publishPassEvent is the shared low-level publish for every passIssue
// event. Always goes through channelevents.PublishMonitoring — never a
// hand-rolled marshal, which would skip MonitoringEvent.Validate and let a
// malformed event look "sent" right up until an operator notices nothing
// ever arrives.
func (r *Reconciler) publishPassEvent(ctx context.Context, src *v1.RelationshipSource, issue passIssue, transition, summary string) {
	logger := log.FromContext(ctx)
	if r.MonitoringPublish == nil {
		logger.V(1).Info("RelationshipSource: pass issue not reported (no monitoring publisher configured)",
			"relationshipsource", src.Name, "namespace", src.Namespace, "issue", string(issue), "transition", transition)
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      issue.level(),
		Category:   monitoringCategoryReconcile,
		Transition: transition,
		Source: channelevents.MonitoringSourceRef{
			Kind: "RelationshipSource", Namespace: src.Namespace, Name: src.Name,
		},
		Condition: issue.condition(),
		Reason:    issue.reason(),
		Summary:   summary,
		Timestamp: time.Now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		logger.Info("publish RelationshipSource pass issue event failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "issue", string(issue), "err", err.Error())
	}
}

// publishCredResolveFailed reports a credential resolution failure: Error
// level, since spec.auth cannot be resolved to a usable token at all (a
// missing AgentIdentity/credential, an unregistered credkind, a Secret
// that cannot be adopted or read) — not a transient sync hiccup.
//
// Checked against src's PRIOR Ready condition (before the caller's
// r.requeue mutates it), exactly like publishKindClaimed: a failure already
// reported on an earlier reconcile is not re-announced on every interval
// for as long as an operator takes to fix spec.auth.
func (r *Reconciler) publishCredResolveFailed(ctx context.Context, src *v1.RelationshipSource, resolveErr error) {
	logger := log.FromContext(ctx)
	if already := conditions.Find(src.Status.Conditions, v1.RelationshipSourceConditionReady); already != nil &&
		already.Status == metav1.ConditionFalse && already.Reason == v1.ReasonRelationshipSourceAuthResolveFailed {
		return
	}
	if r.MonitoringPublish == nil {
		logger.V(1).Info("RelationshipSource: credential resolution failure not reported (no monitoring publisher configured)",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", resolveErr.Error())
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   monitoringCategoryCredential,
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "RelationshipSource", Namespace: src.Namespace, Name: src.Name,
		},
		Condition: v1.RelationshipSourceConditionReady,
		Reason:    v1.ReasonRelationshipSourceAuthResolveFailed,
		// Scrubbed, and this is the one that was actually leaking. credhost.Check
		// quotes the FULL raw destination when it refuses a credential's
		// destination, so a spec.baseURL carrying a token in its query string
		// reached this Summary verbatim and fanned out to every role=monitoring
		// Channel — while the same text, on the same reconcile, was already
		// being scrubbed on its way into the Ready condition. Status was
		// protected and chat was not.
		Summary:   fmt.Sprintf("credential resolution failed: %s", scrubScopeErrorMessage(resolveErr.Error())),
		Hint:      "check spec.auth's AgentIdentity/credential name and the backing Secret",
		Timestamp: time.Now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		logger.Info("publish RelationshipSource credential resolution failure event failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
	}
}

// recoverableReadyReasons is every Ready-condition Reason this controller
// actually PUBLISHES a Failed report for, mapped to that report's level —
// publishKindClaimed's kind-claim conflict (Warning) and
// publishCredResolveFailed's credential-resolution failure (Error).
//
// Fix round 1 (Minor): publishRecovered used to fire for ANY prior-False
// reason, including KindUnregistered and PassFailed — neither of which has
// a matching Failed publisher (see their Reconcile branches: both requeue
// directly, with no publish call), so an operator who never received either
// alert would still see its "recovery". Scoping to this map, rather than
// guessing a default level for the unmapped reasons, means a reason can
// only ever produce a Recovered if it can be seen HERE to have produced a
// Failed — the two are structurally kept in sync at this one declaration
// instead of by convention across four Reconcile branches.
var recoverableReadyReasons = map[string]string{
	v1.ReasonRelationshipSourceKindClaimed:       channelevents.MonitoringLevelWarning,
	v1.ReasonRelationshipSourceAuthResolveFailed: channelevents.MonitoringLevelError,
}

// publishRecovered reports Ready clearing a prior failure. wasFailing/
// priorReason are a snapshot taken by the caller at the TOP of Reconcile,
// before anything on this pass could mutate src.Status.Conditions — the
// same "checked before mutation" contract publishCredResolveFailed and
// publishKindClaimed already follow, just handed in rather than re-derived,
// since by the time this runs the condition has already been overwritten
// to Ready=True/Synced.
func (r *Reconciler) publishRecovered(ctx context.Context, src *v1.RelationshipSource, wasFailing bool, priorReason string) {
	if !wasFailing {
		return
	}
	level, ok := recoverableReadyReasons[priorReason]
	if !ok {
		// KindUnregistered, PassFailed, or anything else with no matching
		// Failed report — nothing to recover FROM, as far as monitoring
		// ever heard.
		return
	}
	logger := log.FromContext(ctx)
	if r.MonitoringPublish == nil {
		logger.V(1).Info("RelationshipSource: recovery not reported (no monitoring publisher configured)",
			"relationshipsource", src.Name, "namespace", src.Namespace, "reason", priorReason)
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      level,
		Category:   monitoringCategoryReconcile,
		Transition: channelevents.MonitoringTransitionRecovered,
		Source: channelevents.MonitoringSourceRef{
			Kind: "RelationshipSource", Namespace: src.Namespace, Name: src.Name,
		},
		Condition: v1.RelationshipSourceConditionReady,
		Reason:    priorReason,
		Summary:   fmt.Sprintf("Ready recovered: %s cleared", priorReason),
		Timestamp: time.Now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		logger.Info("publish RelationshipSource recovery event failed",
			"relationshipsource", src.Name, "namespace", src.Namespace, "err", err.Error())
	}
}
