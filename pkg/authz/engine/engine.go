// Package engine is the top-level runner-facing Engine interface
// aggregator. It composes pkg/authz's Checker + Granter + Lookuper +
// BindingLifecycle into a single dependency consumers (runner,
// channelsd, future authzd-callers) take.
//
// FUTURE-MOVE methods (CheckEntityCanBind, CheckMCPTrust,
// CheckInformationFlow) return ErrNotYetMigrated-flavored Results
// until subsequent PRs land their real implementations. No call site
// in this PR invokes them.
package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Engine is the single interface every authz consumer depends on.
type Engine interface {
	Checker
	Granter
	Lookuper
	BindingLifecycle
}

// Checker covers all Check* methods.
//
// Every method fails CLOSED. The two Result-returning shapes report failure as
// OutcomeDenied plus a Message (there is no error to ignore); the two
// (bool, error) shapes return false alongside any error, and their callers must
// treat a non-nil error as a denial rather than retrying into an allow.
type Checker interface {
	// CheckToolCall decides whether a tool call may dispatch: it runs the
	// session-scope pre-pass, then p's PermissionCheck against SpiceDB for the
	// subject(s) in `in`, falling back to the per-session grant when one is
	// configured. Returns Allowed or Denied-with-Message; an RPC error, a nil
	// client, or an unwired ToolChecker all deny (fail-CLOSED). A Denied Result
	// stamped EnforceAlways must be honored even under permissive toolAuthMode.
	CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result

	// CheckSessionInteract decides whether canonicalID may send a message into
	// this session (agentsession#interact) — channelsd's inbound gate. Callers
	// bind fullyConsistent=true when a just-written started_by/participant tuple
	// must be visible. Returns (false, nil) when no checker is wired; on error
	// returns (false, err) and every caller denies (fail-CLOSED), the Interact
	// hook marking it transient so it reads as infra failure, not "unauthorized".
	CheckSessionInteract(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)

	// CheckSessionInbound is the type-dispatching form of the same gate, and
	// the one channelsd's Interact hook calls: `actor` carries its SpiceDB
	// object type, so a human resolves through agentsession#interact and a
	// delegating or delegated session through agentsession#converse. An actor
	// type with no inbound permission returns authz.ErrActorTypeUnsupported —
	// a DEFINITIVE refusal, distinct from a backend error, which callers must
	// not retry. Returns (false, nil) when no checker is wired.
	CheckSessionInbound(ctx context.Context, scope authz.SessionRef, actor identity.Subject, fullyConsistent bool) (bool, error)

	// CheckSessionGrant decides whether a post-approval per-session grant
	// (agentsession#check_<permName>_<resType>) now satisfies a call that the
	// base permission denied. Returns Allowed only on an affirmative SpiceDB
	// answer; a nil checker or an RPC error return Denied with the cause as the
	// Message (fail-CLOSED).
	CheckSessionGrant(ctx context.Context, scope authz.SessionRef, subject, permName, resType string) authz.Result
	// CheckApproverAuthorized is the click-time gate: with resources present
	// the clicker must be #owner on AT LEAST ONE source resource — quorum is
	// one, because approval is delivered to every owner and any one of them
	// vouches. Session standing is NOT consulted on that branch (the resource
	// owner vouches for their own resource; see the rationale on
	// authz.CheckApproverAuthorized). Empty resources ⇒ session-approve-only
	// gate. Fail-closed: with no affirmative ownership, any error ⇒ not
	// authorized.
	CheckApproverAuthorized(ctx context.Context, scope authz.SessionRef, resources []authz.ApproverResourceRef, canonicalID identity.CanonicalUserID) (bool, error)

	// The three below are declared shape only: the implementation returns Denied
	// carrying ErrNotYetMigrated rather than panicking, so an accidental call
	// fails CLOSED instead of crashing the caller. No production site calls them.

	// CheckEntityCanBind will decide whether a subject may bind an extracted
	// entity into the session scope. Always Denied today.
	CheckEntityCanBind(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result

	// CheckMCPTrust will decide whether an MCP tool's declaration is trusted
	// enough to dispatch with the given args. Always Denied today.
	CheckMCPTrust(ctx context.Context, spec authz.MCPToolRef, args json.RawMessage) authz.Result

	// CheckInformationFlow will decide whether payload may flow from src to dst
	// (the information-leakage axis). Always Denied today.
	CheckInformationFlow(ctx context.Context, src, dst authz.ResourceRef, payload json.RawMessage) authz.Result
}

// Granter covers all Touch* / Grant / Revoke methods.
//
// Every method is a WRITE, so there is no open/closed axis: an error means the
// tuple may not exist and the caller must not proceed as though it does. All of
// them are idempotent TOUCH-shaped writes, safe to repeat on retry. With no
// underlying writer wired (SpiceDB disabled) they no-op and return nil.
type Granter interface {
	// TouchStartedBy writes agentsession#started_by@user:<canonicalID> — the
	// tuple that makes canonicalID the session's originator.
	TouchStartedBy(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error

	// TouchOwner writes agentsession#owner@<subjectRef>, where subjectRef is a
	// concrete subject ("user:abc") or a subject set ("group:eng#member").
	// Ownership confers approve and fork standing; started_by does not.
	TouchOwner(ctx context.Context, scope authz.SessionRef, subjectRef string) error

	// TouchInteractParticipant writes agentsession#participant@<subject> for a
	// subject-SET expression, admitting a whole group to the session.
	TouchInteractParticipant(ctx context.Context, scope authz.SessionRef, subject string) error

	// TouchInteractParticipantUser writes
	// agentsession#participant@user:<canonicalID> for one concrete user.
	TouchInteractParticipantUser(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error

	// TouchDeniedUser writes agentsession#denied@user:<canonicalID>, the
	// blocklist tuple that overrides any interact standing the user would
	// otherwise have — and which a fork must copy forward.
	TouchDeniedUser(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error

	// Grant writes rels verbatim (batched, TOUCH semantics). Empty rels is a
	// no-op returning nil.
	Grant(ctx context.Context, rels []authz.Relation) error

	// Revoke deletes rels verbatim. Empty rels is a no-op returning nil; an
	// error means the relations may still exist, so access may still be live.
	Revoke(ctx context.Context, rels []authz.Relation) error
}

// Lookuper covers all Lookup* methods.
//
// These ENUMERATE rather than decide, so an error is not itself a verdict — but
// every caller in this repo treats a failed enumeration as "no one qualifies"
// and stops, because acting on a partial member list would silently widen or
// narrow who is asked. With no underlying lookuper wired they return empty.
type Lookuper interface {
	// LookupApprovers returns every canonical subject currently included in the
	// subject-set expression (e.g. "agentsession:ns/name#approve"). Empty means
	// nobody holds it — never "unknown".
	LookupApprovers(ctx context.Context, subjectSet string) ([]string, error)

	// LookupInteractParticipants returns every canonical subject currently
	// authorized to message this session. Used to address a broadcast, not to
	// decide one user's access — CheckSessionInteract does that.
	LookupInteractParticipants(ctx context.Context, scope authz.SessionRef) ([]string, error)

	// LookupSubjectIncludes reports whether canonicalID is currently a member of
	// the subject-set expression. Returns (false, err) on failure and callers
	// deny (fail-CLOSED).
	LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error)
	// ResolveApprovers returns eligible approvers — the resource owner-sets
	// (intersected across resources) when present, else the session
	// approve-set — and empty=true when no one is eligible (fail-closed "no
	// one has standing"). See the rationale on authz.ResolveApprovers.
	ResolveApprovers(ctx context.Context, sessionApproveSet string, resourceOwnerSets []string) ([]string, bool, error)
}

// BindingLifecycle covers per-session binding lifecycle operations.
//
// The per-session authz config snapshot is deliberately NOT here. It is written
// by the OPERATOR (pkg/controllers/agentsession's reconcileAuthzSessionConfig,
// via the authz_session_config Kind's own accessor), because authzd derives the
// session's cold-start authorization policy from that record and the session
// must not choose the gates applied to itself. The Kind is ComponentWritten, so
// a per-session bearer is refused it at the memory facade's per-kind write
// door — do not add a write path back onto this interface, which the runner
// holds.
//
// BindClassDefaults and FillToolArgs take the session ref alongside the memory
// scope because a slot grant is keyed on the session, not on a memory
// document: the two coordinates were interchangeable while bindings lived in
// memory and are not once they live in SpiceDB.
type BindingLifecycle interface {
	// BindClassDefaults Checks each entity's declared default resources for
	// subject and writes the ALLOWED ones into the session scope as
	// SourceDefault. Denied defaults are dropped with an INFO log, not an error
	// — a default the user cannot reach is not a failure, it is simply absent
	// (fail-CLOSED). The returned error is a memory read/write failure, which
	// leaves the scope unchanged.
	BindClassDefaults(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string) error

	// PromoteExtractedSlots binds the instances the extractor proposed for one
	// turn, after Checking each for the requester. The authorization half of the
	// query/extract fill source: authzd proposes, this disposes.
	PromoteExtractedSlots(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string, turnIndex int) error

	// PromoteObservedSlots binds the instances a recorded FACT named — a signed
	// delivery's or a tool result's — after Checking each for the requester.
	// The authorization half of the observed fill source: an observation
	// proposes, this disposes.
	//
	// Unlike PromoteExtractedSlots it takes no turn index. Facts are keyed by
	// their SUBJECT, not by the turn that produced them, because a fact
	// legitimately outlives the turn it was observed in: the tool call that
	// records one and the call that needs the binding are normally two
	// dispatches of the same turn, and a later turn's gated call must still
	// find the instance bound.
	PromoteObservedSlots(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string) error

	// FillToolArgs injects already-bound entity resource IDs into a tool call's
	// args where the entity declares an autoFill arg matching toolName. It
	// authorizes nothing — it only replays bindings BindClassDefaults or the
	// extractor already vetted. On error the caller keeps the original args.
	FillToolArgs(ctx context.Context, sess authz.SessionRef, entities []authz.BoundEntitySpec, toolName string, args json.RawMessage) (json.RawMessage, error)

	// WaitForExtraction blocks until the extraction for turn inboxIdx reaches
	// complete or failed, or deadline elapses. Best-effort ordering only, so it
	// returns nil in BOTH cases: a timeout means the caller proceeds without
	// freshly-extracted bindings, never that it proceeds without a Check.
	WaitForExtraction(ctx context.Context, scope memory.Scope, inboxIdx int, deadline time.Duration) error
}

// Dependency interfaces — kept narrow so tests fake individual methods.
// Production passes *spicedb.Client (via adapters) + memory.Memory.

// ToolChecker is the narrow SpiceDB-side dependency for CheckToolCall.
type ToolChecker interface {
	// CheckToolCall answers the tool-call gate. Same contract as
	// Checker.CheckToolCall: no error return, every failure is a Denied Result.
	CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result
}

// SessionInteractChecker is the narrow SpiceDB-side dependency for
// CheckSessionInteract and CheckSessionInbound. It carries BOTH arms of the
// inbound gate, because CheckSessionInbound picks between them by the acting
// subject's type and a dep that supplied only one would leave the other
// permanently unanswerable.
type SessionInteractChecker interface {
	// CheckInteract reports whether canonicalID holds agentsession#interact on
	// ns/name. fullyConsistent=true reads at head, required when the granting
	// tuple may have just been written. (false, err) on failure; callers deny.
	CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// CheckConverse reports whether the session (senderNS, senderName) holds
	// agentsession#converse on ns/name — the agent-to-agent arm. Same
	// fail-closed contract as CheckInteract.
	CheckConverse(ctx context.Context, ns, name, senderNS, senderName string, fullyConsistent bool) (bool, error)
}

// SessionGrantChecker is the narrow SpiceDB-side dependency for CheckSessionGrant.
type SessionGrantChecker interface {
	// CheckSessionGrant reports whether the session carries the post-approval
	// grant satisfying permName on resType for subject. Returns the verdict as a
	// Result plus a transport error; the wrapper denies on either.
	CheckSessionGrant(ctx context.Context, ns, name, subject, permName, resType string) (authz.Result, error)
}

// GranterImpl is the narrow SpiceDB-side dependency for Touch* methods.
// Each method writes exactly the relation its Granter counterpart documents,
// against the (ns, name) session coordinates rather than a SessionRef.
type GranterImpl interface {
	// TouchStartedBy writes agentsession#started_by@user:<canonicalID>.
	TouchStartedBy(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
	// TouchOwner writes agentsession#owner@<subjectRef> (concrete or subject set).
	TouchOwner(ctx context.Context, ns, name, subjectRef string) error
	// TouchInteractParticipant writes agentsession#participant@<subject set>.
	TouchInteractParticipant(ctx context.Context, ns, name, subject string) error
	// TouchInteractParticipantUser writes agentsession#participant@user:<canonicalID>.
	TouchInteractParticipantUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
	// TouchDeniedUser writes agentsession#denied@user:<canonicalID>.
	TouchDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
}

// RelWriter is the narrow SpiceDB-side dependency for Grant / Revoke.
type RelWriter interface {
	// WriteRelationships batch-writes rels with TOUCH semantics (idempotent).
	WriteRelationships(ctx context.Context, rels []authz.Relation) error
	// DeleteRelationships batch-deletes rels. An error means access may still
	// be live, so a revocation caller must surface rather than swallow it.
	DeleteRelationships(ctx context.Context, rels []authz.Relation) error
}

// SlotListerImpl is the narrow SpiceDB-side dependency for reading the
// instances bound into this session's slots. Satisfies authz.SlotLister.
type SlotListerImpl interface {
	ListSlotGrants(ctx context.Context, ns, name string) ([]authz.SlotBinding, error)
}

// LookuperImpl is the narrow SpiceDB-side dependency for Lookup* methods.
type LookuperImpl interface {
	// LookupSubjects expands a subject-set expression to its current members.
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
	// LookupInteractSubjects expands the session's interact set to its members.
	LookupInteractSubjects(ctx context.Context, ns, name string) ([]string, error)
	// LookupSubjectIncludes reports membership of one canonical user in a
	// subject set. (false, err) on failure; callers deny (fail-CLOSED).
	LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error)
}

// ApproverCheckerImpl is the narrow SpiceDB-side dependency for the two-phase
// approver authorization: click-time session-approve gate +
// per-resource owner gate.
type ApproverCheckerImpl interface {
	// CheckApprove reports whether canonicalID holds agentsession#approve — the
	// gate for approvals that name no source resource. (false, err) on failure.
	CheckApprove(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// CheckOwnerOnResource reports whether canonicalID holds #owner on
	// <resType>:<resID>. Retained for callers outside the approver gate.
	CheckOwnerOnResource(ctx context.Context, resType, resID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// CheckOnResource reports whether canonicalID holds <permission> on
	// <resType>:<resID> — the gate for a resource-scoped approval, asked with
	// the permission that resource's type declared as conferring approver
	// standing. (false, err) on failure.
	CheckOnResource(ctx context.Context, resType, resID, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// Deps wires the Engine implementation. Each field may be nil; the
// corresponding Engine method will degrade (return denied / nil / empty).
type Deps struct {
	ToolChecker            ToolChecker
	SessionInteractChecker SessionInteractChecker
	SessionGrantChecker    SessionGrantChecker
	Granter                GranterImpl
	RelWriter              RelWriter
	SlotLister             SlotListerImpl
	Lookuper               LookuperImpl
	ApproverChecker        ApproverCheckerImpl
	Memory                 memory.Memory
	// SessionExpiration is the session's wall-clock lifetime cap
	// (budget.sessionExpiration). Slot grants expire with it. Zero means the
	// session declares no cap, which is NOT "no expiry" — see
	// authz.SlotGrantExpiry.
	SessionExpiration time.Duration
}

// New constructs an Engine backed by the given dependencies.
func New(d Deps) Engine {
	return &impl{d: d}
}

type impl struct{ d Deps }

// --- Checker ---

func (i *impl) CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	if i.d.ToolChecker == nil {
		return authz.Result{Outcome: authz.OutcomeDenied, Message: "engine: no ToolChecker wired"}
	}
	return i.d.ToolChecker.CheckToolCall(ctx, p, in)
}

func (i *impl) CheckSessionInteract(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	return authz.CheckSessionInteract(ctx, i.d.SessionInteractChecker, scope, canonicalID, fc)
}

func (i *impl) CheckSessionInbound(ctx context.Context, scope authz.SessionRef, actor identity.Subject, fc bool) (bool, error) {
	return authz.CheckSessionInbound(ctx, i.d.SessionInteractChecker, scope, actor, fc)
}

func (i *impl) CheckSessionGrant(ctx context.Context, scope authz.SessionRef, subject, permName, resType string) authz.Result {
	return authz.CheckSessionGrant(ctx, i.d.SessionGrantChecker, scope, subject, permName, resType)
}

func (i *impl) CheckApproverAuthorized(ctx context.Context, scope authz.SessionRef, resources []authz.ApproverResourceRef, canonicalID identity.CanonicalUserID) (bool, error) {
	if i.d.ApproverChecker == nil {
		return false, nil
	}
	return authz.CheckApproverAuthorized(ctx, i.d.ApproverChecker, scope.Namespace, scope.Name, resources, canonicalID)
}

// FUTURE-MOVE stubs: return Denied with the ErrNotYetMigrated message.
// They do NOT panic — that would crash the runner on an accidental call.

func (i *impl) CheckEntityCanBind(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	return authz.Result{Outcome: authz.OutcomeDenied, Message: "engine.CheckEntityCanBind: " + authz.ErrNotYetMigrated.Error()}
}

func (i *impl) CheckMCPTrust(_ context.Context, _ authz.MCPToolRef, _ json.RawMessage) authz.Result {
	return authz.Result{Outcome: authz.OutcomeDenied, Message: "engine.CheckMCPTrust: " + authz.ErrNotYetMigrated.Error()}
}

func (i *impl) CheckInformationFlow(_ context.Context, _, _ authz.ResourceRef, _ json.RawMessage) authz.Result {
	return authz.Result{Outcome: authz.OutcomeDenied, Message: "engine.CheckInformationFlow: " + authz.ErrNotYetMigrated.Error()}
}

// --- Granter ---
//
// Each method nil-checks i.d.Granter and delegates directly, rather than
// routing through pkg/authz's Touch* facade funcs (which take the wider
// authz.Granter interface): GranterImpl is deliberately the narrow,
// session-scoped 5-method subset, and authz.Granter has since grown
// class-scoped members (TouchInteractor) that have no ns/name-session
// meaning here. Passing i.d.Granter to an authz.Granter-typed parameter
// would require GranterImpl to mirror every future addition to
// authz.Granter even when Engine's own contract has no use for it.

func (i *impl) TouchStartedBy(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error {
	if i.d.Granter == nil {
		return nil
	}
	return i.d.Granter.TouchStartedBy(ctx, scope.Namespace, scope.Name, canonicalID)
}

func (i *impl) TouchOwner(ctx context.Context, scope authz.SessionRef, subjectRef string) error {
	if i.d.Granter == nil {
		return nil
	}
	return i.d.Granter.TouchOwner(ctx, scope.Namespace, scope.Name, subjectRef)
}

func (i *impl) TouchInteractParticipant(ctx context.Context, scope authz.SessionRef, subject string) error {
	if i.d.Granter == nil {
		return nil
	}
	return i.d.Granter.TouchInteractParticipant(ctx, scope.Namespace, scope.Name, subject)
}

func (i *impl) TouchInteractParticipantUser(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error {
	if i.d.Granter == nil {
		return nil
	}
	return i.d.Granter.TouchInteractParticipantUser(ctx, scope.Namespace, scope.Name, canonicalID)
}

func (i *impl) TouchDeniedUser(ctx context.Context, scope authz.SessionRef, canonicalID identity.CanonicalUserID) error {
	if i.d.Granter == nil {
		return nil
	}
	return i.d.Granter.TouchDeniedUser(ctx, scope.Namespace, scope.Name, canonicalID)
}

func (i *impl) Grant(ctx context.Context, rels []authz.Relation) error {
	return authz.Grant(ctx, i.d.RelWriter, rels)
}

func (i *impl) Revoke(ctx context.Context, rels []authz.Relation) error {
	return authz.Revoke(ctx, i.d.RelWriter, rels)
}

// --- Lookuper ---

func (i *impl) LookupApprovers(ctx context.Context, subjectSet string) ([]string, error) {
	return authz.LookupApprovers(ctx, i.d.Lookuper, subjectSet)
}

func (i *impl) LookupInteractParticipants(ctx context.Context, scope authz.SessionRef) ([]string, error) {
	return authz.LookupInteractParticipants(ctx, i.d.Lookuper, scope)
}

func (i *impl) LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error) {
	return authz.LookupSubjectIncludes(ctx, i.d.Lookuper, subjectRef, canonicalID)
}

func (i *impl) ResolveApprovers(ctx context.Context, sessionApproveSet string, resourceOwnerSets []string) ([]string, bool, error) {
	return authz.ResolveApprovers(ctx, i.d.Lookuper, sessionApproveSet, resourceOwnerSets)
}

// --- BindingLifecycle ---

// BindClassDefaults Checks each BoundEntitySpec.Defaults against SpiceDB and
// writes the allowed subset into the session's scope. A nil Memory or
// ToolChecker makes it a no-op.
func (i *impl) BindClassDefaults(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string) error {
	if i.d.Memory == nil || i.d.ToolChecker == nil {
		return nil
	}
	chk := authz.CheckerFunc(func(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
		return i.d.ToolChecker.CheckToolCall(ctx, p, in)
	})
	return authz.BindClassDefaults(ctx, i.d.Memory, scope, sess, entities, chk, i.d.RelWriter, subject, time.Now, i.d.SessionExpiration)
}

func (i *impl) PromoteExtractedSlots(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string, turnIndex int) error {
	if i.d.Memory == nil || i.d.ToolChecker == nil {
		return nil
	}
	chk := authz.CheckerFunc(func(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
		return i.d.ToolChecker.CheckToolCall(ctx, p, in)
	})
	return authz.PromoteExtractedSlots(ctx, i.d.Memory, scope, sess, entities, chk, i.d.RelWriter, subject, turnIndex, time.Now, i.d.SessionExpiration)
}

func (i *impl) PromoteObservedSlots(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string) error {
	if i.d.Memory == nil || i.d.ToolChecker == nil {
		return nil
	}
	chk := authz.CheckerFunc(func(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
		return i.d.ToolChecker.CheckToolCall(ctx, p, in)
	})
	return authz.PromoteObservedSlots(ctx, i.d.Memory, scope, sess, entities, chk, i.d.RelWriter, subject, time.Now, i.d.SessionExpiration)
}

// FillToolArgs reads the session's slot grants and auto-fills matching tool
// args from them — the same store the per-tool Check consults.
func (i *impl) FillToolArgs(ctx context.Context, sess authz.SessionRef, entities []authz.BoundEntitySpec, toolName string, args json.RawMessage) (json.RawMessage, error) {
	if i.d.SlotLister == nil {
		return args, nil
	}
	return authz.FillToolArgs(ctx, i.d.SlotLister, sess, entities, toolName, args)
}

// WaitForExtraction polls extraction_state until the given turn is
// complete|failed or deadline elapses. Best-effort: always returns nil.
func (i *impl) WaitForExtraction(ctx context.Context, scope memory.Scope, inboxIdx int, deadline time.Duration) error {
	if i.d.Memory == nil {
		return nil
	}
	return authz.WaitForExtraction(ctx, i.d.Memory, scope, inboxIdx, deadline)
}
