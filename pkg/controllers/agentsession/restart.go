package agentsession

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/memcopy"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// restorePollInterval is how often ReconcileRestart re-checks an
// in-flight snapshot/restore Job. The Jobs complete asynchronously (a
// pod must run to completion) and the reconciler must not block waiting
// on them, so it requeues and polls the Done probes instead.
const restorePollInterval = 5 * time.Second

// EnsureChildSession creates the deterministic child AgentSession
// from BuildChildSession. Idempotent: on AlreadyExists, the existing
// child is re-fetched and returned. Used by the restart reconciler.
func EnsureChildSession(ctx context.Context, c client.Client, parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) (*spiceboxv1alpha1.AgentSession, error) {
	if parent.Spec.GoalExecution != nil {
		return nil, fmt.Errorf("goal execution cannot create a continuation")
	}
	desired := BuildChildSession(parent, pr)
	if err := c.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("EnsureChildSession: create: %w", err)
		}
	}
	var fetched spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, types.NamespacedName{Namespace: parent.Namespace, Name: pr.TargetSessionName}, &fetched); err != nil {
		return nil, fmt.Errorf("EnsureChildSession: get after create: %w", err)
	}
	return &fetched, nil
}

// findChildSession returns the child AgentSession, or (nil, nil) when it does
// not exist yet. A present child means the fork's seeding phase already
// committed, so ReconcileRestart must resume at the idempotent tail rather than
// replaying writes into a scope the child's runner now owns.
func findChildSession(ctx context.Context, c client.Client, ns, name string) (*spiceboxv1alpha1.AgentSession, error) {
	var child spiceboxv1alpha1.AgentSession
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &child)
	switch {
	case err == nil:
		return &child, nil
	case apierrors.IsNotFound(err):
		return nil, nil
	default:
		return nil, err
	}
}

// SupersedeParent patches the parent AgentSession's status to mark
// it superseded by childName: SupersededBy populated, Phase →
// Succeeded, and SupersededByRestart condition True. Idempotent.
func SupersedeParent(ctx context.Context, c client.Client, parent *spiceboxv1alpha1.AgentSession, childName string) error {
	base := parent.DeepCopy()
	patched := parent.DeepCopy()
	patched.Status.SupersededBy = childName
	// A restart-from-here parent (Idle/Succeeded) settles to Succeeded. A
	// continuation-inherit parent may be terminal-Failed (a transient boot
	// failure the child recovers from) — leave that Failed so the parent's
	// record still reflects why it ended; the SupersededByRestart condition is
	// the authoritative "continued elsewhere" signal.
	if parent.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseFailed {
		patched.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	}
	conditions.SetTrue(patched, &patched.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionSupersededByRestart,
		spiceboxv1alpha1.ReasonReplacedByForkedSession)
	if err := agentstatus.WriteOwned(ctx, c, patched, base, spiceboxv1alpha1.OwnerOperator); err != nil {
		return fmt.Errorf("SupersedeParent: patch: %w", err)
	}
	return nil
}

// lastParentTurnIndex returns the highest turn Index in scope, or -1 when
// the scope has no turns. Used by inherit mode to cut at the end of the
// transcript so the whole thing carries forward and the new message lands
// contiguously at lastTurn+1.
func lastParentTurnIndex(ctx context.Context, m memory.Memory, scope memory.Scope) (int, error) {
	turns, err := turn.ReadAll(ctx, m, scope)
	if err != nil {
		return 0, fmt.Errorf("lastParentTurnIndex: read turns: %w", err)
	}
	last := -1
	for _, t := range turns {
		if t.Index > last {
			last = t.Index
		}
	}
	return last, nil
}

// CopyMemoryPrefix copies turns 0..cutTurn (and turn-anchored
// entries) from src to dst, then appends an inbox turn at
// cutTurn+1 with newUserText. The prefix copy is idempotent; the inbox
// append is NOT (its CreatedAt is time.Now(), so a replay at the same
// cut conflicts with ErrIndexConflict). Callers must run it at most
// once per child scope — ReconcileRestart guarantees this by skipping
// the seed when the child CR already exists.
func CopyMemoryPrefix(ctx context.Context, m memory.Memory, src, dst memory.Scope, cutTurn int, newUserText string, forkMode plangate.ForkMode) error {
	if err := memcopy.CopyPrefix(ctx, m, src, dst, cutTurn); err != nil {
		return fmt.Errorf("CopyMemoryPrefix: prefix copy: %w", err)
	}
	if err := derivePlanGateRoot(ctx, m, src, dst, forkMode); err != nil {
		return fmt.Errorf("CopyMemoryPrefix: derive plan-gate root: %w", err)
	}
	appender := turn.NewAppender(m, dst)
	if err := appender.Append(ctx, memory.Turn{
		Index: cutTurn + 1, Role: "inbox",
		Content:   []memory.ContentBlock{{Type: "text", Text: newUserText}},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("CopyMemoryPrefix: append inbox: %w", err)
	}
	return nil
}

// RecordRestartLineage writes lineage edges marking the fork
// relationship between parent and child sessions at cutTurn. reason
// distinguishes a restart-from-here cut (lineage.ReasonRestart) from a
// full-transcript continuation (lineage.ReasonContinuation). Idempotent.
func RecordRestartLineage(ctx context.Context, m memory.Memory, parentScope memory.Scope, parentName string, childScope memory.Scope, childName string, cutTurn int, reason string) error {
	return lineage.RecordFork(ctx, m, parentScope, parentName, childScope, childName, cutTurn, reason)
}

// WriteSpiceDBParticipants writes the child session's per-session
// SpiceDB relationships. The child's started-by subject is read from
// the CHILD's own AnnotationStartedByCanonicalID — BuildChildSession
// carries the parent's forward for restart/inherit and stamps the NEW
// owner's for a takeover, so this one read is correct for every mode
// (and a takeover never leaks interact to the prior owner). interact
// participants are the class-level policy carried from the parent's
// Status.AppliedInteractPermission. Idempotent (Granter touches are
// last-writer-wins). Skips writes when the data is absent (defensive
// against partial state).
//
// AnnotationStartedByCanonicalID stores the full subject string as
// "user:<canonical>" (the pipeline writes it that way), but
// TouchStartedBy wraps its own user: ObjectType, so we strip the
// "user:" prefix before calling it — matching the pattern in
// pkg/channels/channelkinds/slack/restart.go and pkg/channels/channelsd/pipeline/decision.go.
func WriteSpiceDBParticipants(ctx context.Context, g authz.Granter, parent, child *spiceboxv1alpha1.AgentSession) error {
	childRef := authz.SessionRef{Namespace: child.Namespace, Name: child.Name}
	if started := spiceboxv1alpha1.StartedBySubject(child); !started.Empty() {
		// The child acts as this subject, so validate it before stamping —
		// defense in depth against a malformed or impersonating annotation
		// on the child. Only a concrete user: subject is valid here
		// (TouchStartedBy writes ObjectType "user"); a subject-set or any
		// other type aborts the fork rather than minting a started_by we
		// can't vouch for.
		if err := authz.ValidateSubject(started.String(), authz.SubjectUser); err != nil {
			return fmt.Errorf("WriteSpiceDBParticipants: child started_by subject: %w", err)
		}
		// The annotation stores "user:<canonical>" but TouchStartedBy's
		// SpiceDB write already uses ObjectType "user" and expects only the
		// bare canonical ID as ObjectId.
		if err := authz.TouchStartedBy(ctx, g, childRef, spiceboxv1alpha1.StartedByCanonical(child)); err != nil {
			return fmt.Errorf("WriteSpiceDBParticipants: TouchStartedBy: %w", err)
		}
	}
	if perm := parent.Status.AppliedInteractPermission; perm != "" {
		// Validate before granting, for the same reason the started_by subject
		// above is validated: this value is read off the PARENT's status and
		// stamped as a participant on the CHILD, which on a takeover belongs to
		// a different human — and `interact = owner + started_by + participant
		// - denied` (schema.zed) makes it read access to that person's session,
		// including the transcript this fork just inherited.
		if err := validateInteractPermission(perm); err != nil {
			return fmt.Errorf("WriteSpiceDBParticipants: parent interact permission: %w", err)
		}
		if err := authz.TouchInteractParticipant(ctx, g, childRef, perm); err != nil {
			return fmt.Errorf("WriteSpiceDBParticipants: TouchInteractParticipant: %w", err)
		}
	}
	return nil
}

// interactPermissionRE is the shape of a snapshotted class interact policy:
// "<type>:<id>#<relation>", e.g. "group:engineering#member".
//
// It deliberately mirrors sessionInteractPermissionRE, which the AgentClass
// controller applies to the AUTHORED value in
// AgentClass.spec.authz.session.interactPermission. The two guard different
// trust boundaries — an admin-authored spec field there, a status field a
// compromised runner holds `patch` on here — which is why the check is
// repeated rather than assumed from the class side.
var interactPermissionRE = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-zA-Z0-9_/-]+#[a-z][a-z0-9_]*$`)

// participantSubjectTypes are the SpiceDB object types the COMPOSED schema
// admits as an agentsession#participant subject. Anything else cannot be a
// legitimate grant, so it is refused here rather than left for SpiceDB to reject
// mid-fork.
//
// Derived from the two sources the composed schema itself is built from, never
// listed:
//
//   - the scaffold (pkg/authz/spicedb/schema/schema.zed) contributes the base
//     vocabulary, `user | group#member`. It belongs to no channel kind and is
//     admissible on a build with none registered.
//   - each registered channel kind contributes its own link types via
//     SessionRelationLinker, and the guardian composer unions exactly those into
//     the live relation line — slack_channel#member and slack_usergroup#member
//     for Slack today.
//
// Deriving is what keeps this gate and the schema it guards from disagreeing. A
// literal list here held `user` and `group` alone and refused a legitimate
// slack_channel:<id>#member interact policy at every fork and restart, while the
// live schema had admitted it since the day the kind registered the link — and
// an AgentClass whose interact permission is DERIVED from its output channel's
// membership makes that the common shape rather than a hand-patched one.
//
// Still fail-closed, and that is the property to preserve: an object type
// neither the scaffold nor a registered kind names is refused, so widening this
// to "any well-formed subject" would be a security regression. `interact = owner
// + started_by + participant - denied`, so a bogus participant is read access to
// the transcript the fork just inherited.
//
// An error means the question could not be answered — the schema no longer
// declares the relation — and callers must treat it as a refusal, not as an
// empty set.
func participantSubjectTypes() (map[authz.SubjectType]bool, error) {
	types, err := guardianschema.SubjectTypesFor(
		authzschema.Schema, participantRelation, chregistry.SessionRelationLinks()...)
	if err != nil {
		return nil, fmt.Errorf("admissible participant subject types: %w", err)
	}
	admissible := make(map[authz.SubjectType]bool, len(types))
	for _, t := range types {
		admissible[authz.SubjectType(t)] = true
	}
	return admissible, nil
}

// participantRelation is the agentsession relation an interact policy becomes a
// grant on — the one authz.TouchInteractParticipant writes, and therefore the
// one whose admissible subject types this file validates against.
const participantRelation = "participant"

// validateInteractPermission reports whether perm is a well-formed subject-set
// expression this fork may turn into an agentsession#participant grant.
//
// A bare "user:<id>" is refused on purpose: spicedb.Client.TouchInteractParticipant
// parses its argument with ParseSubject, which REQUIRES the "#relation"
// segment, so a concrete subject reaches SpiceDB as an error anyway — the
// per-user grant path is TouchInteractParticipantUser, which this function's
// caller does not use.
func validateInteractPermission(perm string) error {
	// ParseSubject first — it is the SAME parser the SpiceDB writer uses, so
	// its verdict on the shape is the one that matters. The regex then adds
	// the charset bound ParseSubject does not have (it splits on ':' and '#'
	// and accepts anything between).
	objType, _, _, err := spicedb.ParseSubject(perm)
	if err != nil {
		return fmt.Errorf("interact permission %q: %w", perm, err)
	}
	if !interactPermissionRE.MatchString(perm) {
		return fmt.Errorf("interact permission %q: must be of the form <type>:<id>#<relation>", perm)
	}
	admissible, err := participantSubjectTypes()
	if err != nil {
		return fmt.Errorf("interact permission %q: %w", perm, err)
	}
	if !admissible[authz.SubjectType(objType)] {
		return fmt.Errorf("interact permission %q: subject type %q may not be an agentsession participant", perm, objType)
	}
	return nil
}

// clearPriorRestartDenial sets RestartDenied=False when a prior attempt left it
// True, and mirrors the result onto sess so the caller's later writes build on
// the cleared slice (a denial in this same reconcile is then False→True, which
// re-stamps LastTransitionTime).
//
// A no-op when the condition is absent or already False, so the common path —
// a session that has never been denied — costs one map lookup and no write.
// The condition is set False rather than removed: its history is the audit
// trail of what the fork gate decided and when.
func (r *Reconciler) clearPriorRestartDenial(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	cond := conditions.Find(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRestartDenied)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return nil
	}
	base := sess.DeepCopy()
	patched := sess.DeepCopy()
	conditions.Set(patched, &patched.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionConditionRestartDenied,
		Status:  metav1.ConditionFalse,
		Reason:  spiceboxv1alpha1.ReasonRestartAttemptPending,
		Message: "a new continuation attempt is being evaluated",
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); err != nil {
		return fmt.Errorf("ReconcileRestart: clear prior RestartDenied: %w", err)
	}
	sess.Status.Conditions = patched.Status.Conditions
	return nil
}

// ReconcileRestart drives the fork sequence when sess has a
// PendingRestart marker. Returns (proceed=false, ...) when restart
// is in progress or just finished — the outer Reconcile should
// return immediately. Returns (true, _, nil) when no restart was
// in flight and the outer Reconcile should continue normally.
func (r *Reconciler) ReconcileRestart(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (proceed bool, res ctrl.Result, err error) {
	pr := sess.Status.PendingRestart
	if pr != nil && sess.Spec.GoalExecution != nil {
		return false, ctrl.Result{}, fmt.Errorf("goal execution cannot fork or automatically restart")
	}
	if pr == nil {
		return true, ctrl.Result{}, nil
	}
	// Missing-dependency guard. PublisherKeys belongs HERE and not in the
	// verification step below: an unwired key lookup is an operator
	// misconfiguration, and erroring (retryable, marker preserved) is the right
	// answer, whereas routing it through the deny path would permanently burn a
	// legitimate user's continuation because of a bug on our side.
	if r.RestartMemory == nil || r.AuthzGranter == nil || r.Snapshotter == nil || r.ForkChecker == nil || r.DeniedLister == nil || r.PublisherKeys == nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: missing dependency (Memory=%v Authz=%v Snapshotter=%v ForkChecker=%v DeniedLister=%v PublisherKeys=%v)",
			r.RestartMemory == nil, r.AuthzGranter == nil, r.Snapshotter == nil, r.ForkChecker == nil, r.DeniedLister == nil, r.PublisherKeys == nil)
	}

	// This attempt is about to be adjudicated, so any verdict from a PREVIOUS
	// attempt is stale — clear it before either gate runs.
	//
	// RestartDenied is otherwise written True here and cleared by nothing, which
	// breaks it in two ways. It stops describing current state (a session whose
	// later continuation succeeded still reads as denied, and a monitoring Rule
	// row watching it would re-announce every historical denial on each operator
	// restart — the AgentSession/Failed problem again). And, because
	// meta.SetStatusCondition only moves LastTransitionTime when the STATUS
	// changes, a second denial is True→True and leaves the stamp frozen at the
	// first one — so channelsd's relay, which dedups on that stamp, silently
	// swallows the user's retry after acking it. Clearing here makes every
	// denial a real False→True transition.
	//
	// It runs before the marker-verification gate as well as the fork gate:
	// both end in a RestartDenied verdict, so both need the prior one cleared or
	// a repeat refusal is invisible to the user.
	if cerr := r.clearPriorRestartDenial(ctx, sess); cerr != nil {
		return false, ctrl.Result{}, cerr
	}

	// Step 0: authenticate the marker's AUTHOR before acting on a single field
	// of it. status.pendingRestart is an authorization input the session's own
	// runner can write — the runner Role grants patch on agentsessions/status
	// and Kubernetes RBAC has no field granularity — and takeover mode
	// deliberately skips the SpiceDB fork gate below. Nothing downstream
	// re-derives TriggeredBy, so an unauthenticated marker lets a compromised
	// runner name any victim and have the child stamped with their identity.
	if verr := restartmarker.Verify(r.PublisherKeys, sess, pr); verr != nil {
		return r.denyUnverifiedRestart(ctx, sess, pr, verr)
	}

	// Already complete; clear the marker and signal "no further work". This is
	// crash recovery for the window between SupersedeParent and the marker
	// clear, and re-adjudicating an ATTRIBUTABLE leftover would only manufacture
	// a denial notice for a completed continuation — hence the quiet drop.
	//
	// It runs AFTER Step 0, not before. status.supersededBy is a status field,
	// and a status field is reachable by anything holding `patch` on
	// agentsessions/status — which the session's own runner does. Checking it
	// first turned one forged value into a mute button: every later marker,
	// including a different human's channelsd-authored takeover (the path that
	// deliberately skips the SpiceDB fork gate below, and the recovery mechanism
	// against a runner behaving exactly like that), fell into
	// clearPendingRestart with no condition, no notice and no log. The
	// AgentSession webhook now pins supersededBy, so a runner cannot write it;
	// ordering the checks this way is the second half — a marker nobody can
	// attribute is refused visibly no matter what else the status claims.
	if sess.Status.SupersededBy != "" {
		return r.clearPendingRestart(ctx, sess)
	}

	inherit := pr.Mode == spiceboxv1alpha1.PendingRestartModeInherit
	takeover := pr.Mode == spiceboxv1alpha1.PendingRestartModeTakeover

	// Step 1: phase check. Restart-from-here forks a live/parked session
	// (Idle/Succeeded). Continuation-inherit and different-user takeover
	// additionally fork a terminal Failed session (inherit only for the
	// recoverable reasons the disposition classifier routes here; takeover for
	// any terminal state, since channelsd already decided a new user may
	// continue the thread).
	phaseOK := sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle ||
		sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded
	if inherit || takeover {
		phaseOK = phaseOK || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed
	}
	if !phaseOK {
		return r.clearPendingRestart(ctx, sess)
	}

	// The parent's memory scope. Defined up front because inherit mode reads
	// it to compute the effective cut (the parent's last turn), so the whole
	// transcript carries forward.
	parentScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

	// Effective cut point + lineage reason. Restart cuts at the user-chosen
	// CutTurnIndex; inherit carries the full transcript, so the cut is the
	// parent's last turn (the new message then lands contiguously at
	// lastTurn+1). A read failure aborts before any child materialization.
	effectiveCut := int(pr.CutTurnIndex)
	lineageReason := lineage.ReasonRestart
	switch {
	case takeover:
		lineageReason = lineage.ReasonTakeover
		if pr.InheritHistory {
			// Ordinary terminal: carry the whole transcript, like inherit.
			last, lerr := lastParentTurnIndex(ctx, r.RestartMemory, parentScope)
			if lerr != nil {
				return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: read parent last turn for takeover: %w", lerr)
			}
			effectiveCut = last
		} else {
			// Policy/security-halt carve-out: seed NO prior turns. cut = -1
			// copies only the non-turn-anchored session-level state; the new
			// user's message then lands at turn 0. The halted transcript is not
			// handed to the new owner.
			effectiveCut = -1
		}
	case inherit:
		last, lerr := lastParentTurnIndex(ctx, r.RestartMemory, parentScope)
		if lerr != nil {
			return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: read parent last turn for inherit: %w", lerr)
		}
		effectiveCut = last
		lineageReason = lineage.ReasonContinuation
	}

	// Step 1.4: class start gate — runs before the fork gate below and before
	// ANY materialization, for EVERY mode including takeover. Takeover skips
	// the SessionFork gate on the premise that channelsd's app-mention is the
	// same basis on which the requester could start their own session; a
	// class's allowedStarters is exactly what invalidates that premise for
	// someone not on the list, so it is checked here regardless of mode. See
	// restartStarterAllowed (start_gate.go) for the fail-closed rules.
	if ok, gres, gerr := r.restartStarterAllowed(ctx, sess, pr.TriggeredBy); gerr != nil || !ok {
		return false, gres, gerr
	}

	// Step 1.5: SessionFork authz gate — runs before ANY materialization. A
	// non-owner forker (or a checker error) aborts fail-closed: no child, no
	// memory copy, RestartDenied condition, requester notice, PendingRestart
	// cleared (so the denied fork does not reconcile-loop).
	//
	// Takeover SKIPS this gate: channelsd is the authorization choke point (the
	// new user sent a legitimate app-mention into the thread — the same basis on
	// which they could start their own session), and the parent's
	// agentsession#fork relation neither holds nor should for a different user.
	// inherit/restart keep the fail-closed gate unchanged.
	if !takeover {
		host := newForkHost(forkHostDeps{
			ParentNs:      sess.Namespace,
			ParentName:    sess.Name,
			Forker:        pr.TriggeredBy.String(),
			NoticePublish: r.ForkNoticePublish,
		})
		// pr.TriggeredBy stores the full "user:<canonical>" subject
		// (PendingRestart.TriggeredBy, mirrored from
		// AnnotationStartedByCanonicalID — see restart_decide.go); strip the
		// prefix for pipeline.Input.Requester, which is documented bare.
		// pkg/platform/pipeline imports no domain types (ForkInfo.Forker is a plain
		// string), so .String() is the genuine boundary here, not a marker.
		forkOut, ferr := r.forkExecutor().Run(ctx, pipeline.SessionFork, pipeline.Input{
			Session: pipeline.SessionRef{Namespace: sess.Namespace, Name: sess.Name, Class: sess.Spec.Class},
			Requester: identity.CanonicalFromTrusted(strings.TrimPrefix(pr.TriggeredBy.String(), "user:"),
				"TriggeredBy recorded on the restart request by the platform"),
			Fork: &pipeline.ForkInfo{
				ParentRef: sess.Namespace + "/" + sess.Name,
				ChildRef:  sess.Namespace + "/" + pr.TargetSessionName,
				Forker:    pr.TriggeredBy.String(),
				CutTurn:   effectiveCut,
			},
		}, host)
		if ferr != nil {
			return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: fork gate: %w", ferr)
		}
		if forkOut.Verdict != pipeline.Allow {
			log.FromContext(ctx).Info("ReconcileRestart: fork denied; aborting",
				"session", sess.Namespace+"/"+sess.Name, "forker", pr.TriggeredBy,
				"reason", forkOut.Reason, "firedHook", forkOut.FiredHook)
			base := sess.DeepCopy()
			patched := sess.DeepCopy()
			// Persist the requester-facing wording on the condition. channelsd's
			// session watcher relays this Message to the thread, which is what
			// actually reaches the user: the out.metaagent_notice publish above is
			// best-effort and was, in practice, silently discarded. Fall back to
			// the executor's Reason so the condition is never messageless — a
			// denial the user cannot see is the failure mode this closes.
			denyMsg := host.requesterNotice
			if denyMsg == "" {
				denyMsg = forkOut.Reason
			}
			conditions.Set(patched, &patched.Status.Conditions, metav1.Condition{
				Type:    spiceboxv1alpha1.AgentSessionConditionRestartDenied,
				Status:  metav1.ConditionTrue,
				Reason:  spiceboxv1alpha1.ReasonForkNotAuthorized,
				Message: denyMsg,
			})
			patched.Status.PendingRestart = nil
			if perr := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); perr != nil {
				return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: patch fork-denied: %w", perr)
			}
			return false, ctrl.Result{}, nil
		}
	}

	// Step 2: read post-cut audit, compute affected bundles. For inherit
	// (cut == last turn) there are no post-cut dispatches, so this yields an
	// empty snapshot map and Step 7 takes the CLEAN clone path.
	entries, err := tool_dispatch_snapshot.ForTurnRange(ctx, r.RestartMemory, parentScope, effectiveCut, 1<<30)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: read audit: %w", err)
	}
	_, snapshots := AnalyzePostCut(entries, effectiveCut)

	// The child's memory scope is addressable by name alone — it does NOT
	// require the child AgentSession CR to exist. We seed it (Steps 3-4)
	// BEFORE creating the CR (Step 5) on purpose: the CR is what lets a
	// runner start, and a runner that boots against an unseeded scope reads
	// empty memory and places its own turn-0 from Spec.Prompt, which then
	// collides with the prefix copy on the append-only turn ID
	// (ErrAppendOnlyConflict) and aborts the fork — leaving the child with
	// no inbox turn and no lineage. Seeding first means the runner's
	// ReadAll observes the copied prefix + inbox turn (hadInitialPrompt) and
	// continues from there instead of racing us. Pod-boot latency hides this
	// in production; the zero-latency in-process e2e runner does not. See
	// restart_ordering_test.go.
	childScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + pr.TargetSessionName}

	// Steps 3-5: seed the child's memory, then materialize its CR.
	//
	// The CR is the commit point. Seeding precedes it (see childScope above), so
	// an existing child proves the seed already completed — and the seed must NOT
	// be replayed, because the child's runner now owns that scope. A replayed
	// inbox append lands on a turn index the runner has moved past and fails with
	// memory.ErrIndexConflict, which aborts the reconcile before Steps 8-9. The
	// parent then keeps an immortal PendingRestart, and channelsd's
	// writeInheritForkTrigger short-circuits on it forever: every later reply is
	// acked with "I'm picking it up in a new session" and the session never arrives.
	//
	// The tail (Steps 6-9) is replay-safe on its own: the SpiceDB touches are
	// last-writer-wins, and Snapshot/Restore no-op on AlreadyExists via
	// deterministic Job names. So skipping only the seed lets a retry converge.
	child, err := findChildSession(ctx, r.Client, sess.Namespace, pr.TargetSessionName)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: look up child: %w", err)
	}
	if child == nil {
		// Step 3: copy memory prefix + new inbox turn into the child scope.
		if err := CopyMemoryPrefix(ctx, r.RestartMemory, parentScope, childScope, effectiveCut, pr.NewUserText, forkModeFor(takeover)); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: copy memory: %w", err)
		}

		// Step 4: lineage edges (also child-scope memory — seed before the CR).
		if err := RecordRestartLineage(ctx, r.RestartMemory, parentScope, sess.Name, childScope, pr.TargetSessionName, effectiveCut, lineageReason); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: lineage: %w", err)
		}

		// Step 5: ensure child session exists. Created AFTER its memory is
		// seeded so a freshly-started runner cannot race the copy above.
		child, err = EnsureChildSession(ctx, r.Client, sess, pr)
		if err != nil {
			return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: ensure child: %w", err)
		}
	}

	// Step 6: carry the parent's denied blocklist onto the child BEFORE any
	// interact grant lands. Ordering is security-critical: interact resolves as
	// owner + participant − denied. With a broad interact policy (a group/org
	// subject-set), granting the participant relation first opens a window where
	// interact evaluates against an EMPTY denied set — so a parent-denied member
	// briefly resolves interact=true and can read the child's inherited
	// transcript (memory_entry#read = session→interact). Copying denied first
	// closes that window: the blocklist is in place before the broad grant is
	// ever visible. The child CR + transcript already exist by here, so denied
	// MUST precede the grant. Fail-closed: an error here (nil lister, list
	// failure, touch failure) aborts the fork before the parent is superseded.
	if err := authz.CopyDeniedUsers(ctx, r.DeniedLister, r.AuthzGranter,
		authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		authz.SessionRef{Namespace: child.Namespace, Name: child.Name}); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: copy denied: %w", err)
	}

	// Step 6b: SpiceDB participant writes (the broad interact grant). Runs AFTER
	// the denied-copy above so interact is never granted against an empty
	// blocklist.
	if err := WriteSpiceDBParticipants(ctx, r.AuthzGranter, sess, child); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: spicedb: %w", err)
	}

	// Step 6c: carry the parent's slot grants onto the child — EXCEPT on
	// takeover.
	//
	// A slot grant names the session as its subject, so a child inherits none of
	// them on its own: without this a continuation silently loses every instance
	// a human approved and starts re-asking for values the user already granted.
	//
	// TAKEOVER IS THE CARVE-OUT, and it is the security-relevant half. A takeover
	// is a DIFFERENT user continuing a terminal session, and they become the
	// child's owner — copying the grants would resolve slot_grant->interact +
	// owner for that user on resources they never had standing on, handing them
	// the previous owner's human-approved instance authority. The
	// agentsession#fork gate does not even run for this mode, so nothing else
	// would stop it.
	//
	// Runs after 6a/6b for the reason those are ordered: a slot grant resolves
	// through slot_grant->interact, so it must not go live ahead of the
	// membership (and blocklist) it resolves against.
	//
	// Best-effort: a failure here costs the child a re-approval, so it must not
	// abort a fork whose memory and participants are already in place.
	if !takeover && r.SlotGrantCopier != nil {
		var sessionExpiration time.Duration
		if child.Status.EffectiveSettings != nil {
			sessionExpiration = child.Status.EffectiveSettings.Budget.SessionExpiration.Duration
		}
		copied, cerr := authz.CopySlotGrants(ctx, r.SlotGrantCopier,
			authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
			authz.SessionRef{Namespace: child.Namespace, Name: child.Name},
			authz.SlotGrantExpiry(time.Now(), sessionExpiration))
		if cerr != nil {
			log.FromContext(ctx).Info("ReconcileRestart: slot grants not carried to the child; it will re-ask",
				"session", sess.Namespace+"/"+sess.Name, "child", child.Name, "err", cerr.Error())
		} else if copied > 0 {
			log.FromContext(ctx).Info("ReconcileRestart: carried slot grants to the child",
				"session", sess.Namespace+"/"+sess.Name, "child", child.Name, "grants", copied)
		}
	}

	// Step 7: PVC restore for affected bundles (IMPACTFUL path) or
	// ad-hoc clone of parent's workspace (CLEAN path). The snapshot and
	// restore Jobs complete asynchronously, so this step can come back
	// pending — the one legitimate in-progress hold: PendingRestart
	// stays set and we requeue, because SupersedeParent (Step 8) must
	// NOT run until the child's workspace is actually populated.
	pending, rerr := RestoreOrCloneBundlePVCs(ctx, r.Snapshotter, sess, child, snapshots)
	if rerr != nil {
		if errors.Is(rerr, workspace.ErrSnapshotNotFound) ||
			errors.Is(rerr, workspace.ErrSnapshotJobFailed) ||
			isPermanentSnapshotError(rerr) {
			// A required JIT snapshot is missing, the apiserver
			// permanently rejected the snapshot/restore Job (Invalid /
			// BadRequest / Forbidden), or the Job itself failed
			// terminally (backoff exhausted) — retrying cannot succeed.
			// Either way, mark the parent WorkspaceSnapshotFailed=True and
			// clear PendingRestart so the restart does not retry forever:
			// an immortal PendingRestart wedges the whole thread
			// (channelsd short-circuits every later reply onto it). The
			// operator can inspect the condition via `kubectl describe`.
			log.FromContext(ctx).Info("ReconcileRestart: workspace snapshot failed permanently; clearing PendingRestart",
				"session", sess.Namespace+"/"+sess.Name, "child", pr.TargetSessionName, "err", rerr.Error())
			base := sess.DeepCopy()
			patched := sess.DeepCopy()
			conditions.SetTrue(patched, &patched.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionWorkspaceSnapshotFailed,
				spiceboxv1alpha1.ReasonSnapshotJobFailed)
			patched.Status.PendingRestart = nil
			if perr := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); perr != nil {
				return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: patch snapshot-failed: %w", perr)
			}
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: restore PVCs: %w", rerr)
	}
	if pending {
		log.FromContext(ctx).V(1).Info("ReconcileRestart: workspace snapshot/restore in progress; requeueing",
			"session", sess.Namespace+"/"+sess.Name, "child", pr.TargetSessionName,
			"requeueAfter", restorePollInterval)
		return false, ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	// Step 8: supersede parent.
	if err := SupersedeParent(ctx, r.Client, sess, child.Name); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ReconcileRestart: supersede parent: %w", err)
	}

	// Step 9: clear PendingRestart marker.
	return r.clearPendingRestart(ctx, sess)
}

// forkExecutor builds the one-hook SessionFork pipeline executor. The CheckFork
// closure binds authz.CheckSessionFork fully-consistent; subject arrives as the
// bare canonical (the pipeline.Input.Requester built below is already stripped
// of the "user:" prefix).
func (r *Reconciler) forkExecutor() *pipeline.Executor {
	reg := pipeline.NewRegistry()
	reg.Register(hooks.NewSessionFork(hooks.SessionForkDeps{
		CheckFork: func(ctx context.Context, ns, name string, subject identity.CanonicalUserID) (bool, error) {
			return authz.CheckSessionFork(ctx, r.ForkChecker,
				authz.SessionRef{Namespace: ns, Name: name}, subject, true)
		},
	}), hooks.OrderSessionFork)
	return pipeline.NewExecutor(reg)
}

// ReasonRestartMarkerUnsigned is the reason for
// AgentSessionConditionRestartDenied=True when status.pendingRestart carried no
// attestation at all — the shape a channelsd predating the marker signer
// writes, and the shape of any marker already sitting on an AgentSession when
// the operator is upgraded ahead of it.
//
// Deliberately distinct from ReasonRestartMarkerUnverified, which now means
// only that an attestation was PRESENT and did not hold up. `oap install` rolls
// the operator and channelsd as independent Deployments with no ordering
// between them, so an unsigned marker is the expected shape for the length of
// every upgrade — and an alert keyed on "someone forged a marker" must not fire
// on a routine rollout. The refusal itself is identical: fail-closed, no child,
// marker cleared.
//
// Its siblings live in pkg/apis/v1alpha1 (conditions.go); moving this one to
// join them is a pure relocation that changes nothing about its value.
const ReasonRestartMarkerUnsigned = "RestartMarkerUnsigned"

// unverifiedRestartNotice is what the user sees in-thread when their
// continuation is refused because the marker carried an attestation that did
// not hold up. channelsd's session watcher relays the RestartDenied condition
// Message, so this string reaches a human: it says what happened and what to
// do, and names no CRD, field, key or subject (AGENTS.md: no internal ops
// vocabulary in user messages). The diagnostic detail goes to the log line
// below instead.
const unverifiedRestartNotice = "This couldn't be continued: the request to carry the conversation forward couldn't be verified as coming from the connector. Please send your message again."

// unsignedRestartNotice is the same refusal for the marker that carried NO
// attestation — the upgrade-window shape (see ReasonRestartMarkerUnsigned). A
// user whose thread continuation lands mid-rollout gets told the platform is
// catching up and to re-send, not that they weren't believed.
//
// It renders as the "Reason" excerpt under channelsd's "Couldn't continue this
// conversation" lead, so it reads as a reason plus what to do next, in the voice
// of its neighbours ("Try again in a moment. If it keeps happening, ask an
// operator…"). "usually" is doing real work: an absent attestation is
// overwhelmingly a rollout, but a compromised runner writing a bare marker also
// lands here, and this text must not assert something false about that case.
// The log line below records which it actually was.
const unsignedRestartNotice = "This couldn't be carried forward yet — usually because the connector that relays this conversation is still finishing an update. Send your message again in a moment; if it keeps happening, ask an operator to check that the update completed."

// restartDenialFor maps a marker-verification failure onto the condition reason
// and requester-facing message that fit the fact it carries. Both classes are
// equally terminal and equally fail-closed; they differ only in what the
// operator's dashboards and the user's thread are told.
func restartDenialFor(verr error) (reason, message string) {
	if errors.Is(verr, restartmarker.ErrUnsigned) {
		return ReasonRestartMarkerUnsigned, unsignedRestartNotice
	}
	return spiceboxv1alpha1.ReasonRestartMarkerUnverified, unverifiedRestartNotice
}

// denyUnverifiedRestart refuses a marker that failed author verification: it
// records RestartDenied=True with a requester-facing message, clears the
// marker, and logs the verdict with enough context to tell a forgery from a
// rollout or a misconfiguration.
//
// Every verification failure is TERMINAL — deny and clear, never requeue —
// including the ones that look transient (an unresolvable key, or the
// upgrade-window unsigned marker). That is deliberate, and a requeue would be
// strictly worse rather than gentler:
//
//   - Nothing ever re-signs a marker that is already on the object.
//     channelsd's fork triggers are first-writer-wins — an existing
//     PendingRestart is left intact and the inbound is not even acked — so a
//     retained unsigned marker stays unsigned however long we wait, and any
//     bounded retry could only expire into this same refusal.
//   - While it is retained it wedges the whole thread, because that same
//     short-circuit swallows every later reply. Requeueing would trade one
//     honest refusal for a silence the user's re-sends cannot break.
//
// Clearing is what makes the user's own re-send the retry: unbounded, and
// self-healing the moment channelsd is new, because channelsd then mints and
// signs a fresh marker with its current key. That covers the marker already
// sitting on an AgentSession at upgrade time too — refused once on the first
// reconcile after the operator rolls, cleared, and the next message works.
func (r *Reconciler) denyUnverifiedRestart(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart, verr error) (bool, ctrl.Result, error) {
	publisher, keyID := "", ""
	if pr.Signature != nil {
		publisher, keyID = pr.Signature.Publisher, pr.Signature.KeyID
	}
	reason, message := restartDenialFor(verr)
	log.FromContext(ctx).Info("ReconcileRestart: refusing an unverified restart marker",
		"session", sess.Namespace+"/"+sess.Name,
		"mode", pr.Mode,
		"triggeredBy", pr.TriggeredBy.String(),
		"target", pr.TargetSessionName,
		"signaturePublisher", publisher,
		"signatureKeyID", keyID,
		"reason", reason,
		"err", verr.Error())

	base := sess.DeepCopy()
	patched := sess.DeepCopy()
	conditions.Set(patched, &patched.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionConditionRestartDenied,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	patched.Status.PendingRestart = nil
	if perr := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); perr != nil {
		return false, ctrl.Result{}, fmt.Errorf("denyUnverifiedRestart: patch: %w", perr)
	}
	sess.Status.Conditions = patched.Status.Conditions
	sess.Status.PendingRestart = nil
	return false, ctrl.Result{}, nil
}

func (r *Reconciler) clearPendingRestart(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, ctrl.Result, error) {
	if sess.Status.PendingRestart == nil {
		return true, ctrl.Result{}, nil
	}
	base := sess.DeepCopy()
	patched := sess.DeepCopy()
	patched.Status.PendingRestart = nil
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("clearPendingRestart: %w", err)
	}
	return false, ctrl.Result{}, nil
}

// isPermanentSnapshotError reports whether a snapshot/restore error is a
// permanent apiserver rejection that no retry can fix — an Invalid or
// BadRequest Job (malformed metadata) or a Forbidden create (RBAC). Transient
// errors (conflicts, timeouts, 5xx) stay retryable.
func isPermanentSnapshotError(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err)
}

// derivePlanGateRoot writes the child's single plan-gate root record.
//
// The parent's plan-gate records are deliberately NOT copied — the kind sets
// Retention.NeverForkCopy, because they are a chain whose meaning depends on
// order and on the per-(scope, publisher) provenance sequence. Copying them
// would give the child a history it does not have and could replay an approval
// and its revocation backwards, resurrecting authority a human revoked.
//
// Instead the parent is folded to its terminal state and that OUTCOME becomes
// one derived record. Ordering is then irrelevant because there is nothing left
// to order.
//
// A parent with no plan-gate history produces no root, which is the ordinary
// case for every session that never enabled the gate.
func derivePlanGateRoot(ctx context.Context, m memory.Memory, src, dst memory.Scope, mode plangate.ForkMode) error {
	records, err := plangateaudit.List(ctx, m, src)
	if err != nil {
		// Reading the parent's authorization history failed. Refusing the fork
		// is the fail-closed direction: proceeding would produce a child that
		// silently inherits nothing, which for an `inherit` fork looks like a
		// working session that quietly lost a human's decisions.
		return fmt.Errorf("read parent plan-gate log: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	plan, ok := plangate.PlanFromRecords(records)
	if !ok {
		// History exists but no plan was ever approved in it — nothing to
		// inherit, and nothing wrong.
		return nil
	}

	root, err := plangate.DeriveForFork(plangate.ForkInput{
		Mode:    mode,
		Parent:  src.ID,
		Plan:    plan,
		Records: records,
	})
	if err != nil {
		return err
	}

	root.Mode = "forked"
	root.At = time.Now().UTC()
	return plangateaudit.Record(ctx, m, dst, root)
}

// forkModeFor maps the restart mode onto the plan gate's inheritance rule.
//
// takeover is the only mode that inherits nothing. It is a DIFFERENT user
// continuing a terminal session and becoming the child's owner, and
// ReconcileRestart deliberately does not run the agentsession#fork gate for it
// — the reasoning being that the requester could have started their own session
// anyway. That is correct for a TRANSCRIPT and wrong for AUTHORITY: starting
// your own session gets you no approvals and no grants.
func forkModeFor(takeover bool) plangate.ForkMode {
	if takeover {
		return plangate.ForkTakeover
	}
	return plangate.ForkInherit
}
