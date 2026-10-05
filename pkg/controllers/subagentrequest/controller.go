// Package subagentrequest hosts the SubagentRequest controller: the
// operator-side component that authorizes and performs delegation.
//
// The runner's delegate tool creates a SubagentRequest and polls it; it
// deliberately cannot create an AgentSession directly. The party whose
// behaviour delegation constrains must not also be the party that
// authorizes it — the same reasoning that puts forensic-hold trippers in
// the operator rather than the runner.
package subagentrequest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
)

// maxCopiedResultLen bounds how much of a child session's own text is copied
// onto SubagentRequestStatus — Result for a final answer, Message for an
// outstanding question. Neither field has a MaxLength of its own (a known gap
// from Task 6's review), so an unbounded child answer copied verbatim into
// status would be an etcd object-size problem — status is not a transport for
// arbitrary payloads.
// disclosurePollInterval paces re-reconciles while a data slot awaits a
// human's decision.
//
// A poll rather than a watch on the approval: the answer lands as a status
// patch from channelsd, which already re-triggers this controller through its
// own For(&SubagentRequest{}) — the interval is the backstop for a patch that
// is delivered while this controller is down, not the primary signal. Kept
// well short of the delegate tool's own Timeout so a decision made promptly is
// acted on promptly.
const disclosurePollInterval = 15 * time.Second

// disclosureWaitWindow bounds how long a delegation waits for a human to rule
// on a slot's disclosure.
//
// Shorter than the delegate tool's own DefaultTimeout (30m) on purpose, so the
// request reaches a STATED failure before the parent's call gives up on its
// own. A tool that times out says only "the delegation did not finish"; a
// request that fails here says a decision was needed and never came, which is
// the sentence a user needs.
const disclosureWaitWindow = 20 * time.Minute

const maxCopiedResultLen = 4096

// truncationMarker is appended (within the maxCopiedResultLen budget) when a
// child's text is cut short. The doc comment above explains WHY the bound
// exists to a developer reading this file; it is never seen by whoever
// actually reads the field -- the parent agent polling the request, or a
// human inspecting status -- so a silent byte-slice would read as a
// complete answer that just happens to stop mid-sentence. The marker makes
// the clipping visible to that reader instead.
const truncationMarker = "\n\n[truncated: full result exceeded %d bytes]"

// maxCopiedArtifacts bounds how many of a child's returned artifact handles
// are carried onto SubagentRequestStatus.Artifacts. It exists for the reason
// maxCopiedResultLen does — a child's list is its own to write and status is
// not a transport for an arbitrary payload — and it is deliberately the same
// number respond_to_user caps one delivery at (meta.defaultMaxAttachments):
// carrying more handles than the parent could name in a single reply would
// bound nothing a parent could act on.
const maxCopiedArtifacts = 10

// maxCopiedArtifactDescLen bounds one artifact's child-authored description.
// Far shorter than maxCopiedResultLen because this field is a label on a
// handle, not the answer: the answer travels in Result.
const maxCopiedArtifactDescLen = 256

// maxCopiedArtifactIDLen is the longest artifact handle copied. It is the
// apiserver's own limit on an object name, because the handle's only use is
// to name an ArtifactRender the parent then Gets: a longer string cannot name
// one, and truncating it would silently produce a DIFFERENT handle that names
// something else or nothing at all.
const maxCopiedArtifactIDLen = 253

// ReasonWorkshopBuildDenied is the terminal reason on a SubagentRequest whose
// parent named a namespace other than its own, where that namespace IS a
// workshop provisioned for exactly the claimed parent (its own labels prove
// it — see admitCrossNamespaceParent), but the workshop:<W>#build SpiceDB
// tuple did not hold for it: an unwired WorkshopBuild checker, a check
// error, or an explicit false all land here. A cross-namespace parent whose
// target namespace is not a workshop labeled for it at all is a different,
// pre-existing refusal — "ParentCrossNamespace" — because that shape was
// never a workshop delegation attempt to begin with; this reason exists so
// the two are told apart in status.failureReason.
const ReasonWorkshopBuildDenied = "WorkshopBuildDenied"

// ReasonAttendedChildInProgress is the terminal reason on an `attended`
// SubagentRequest denied because the tree already has a LIVE attended child
// (spec §lifecycle: "one attended child at a time"). It is a policy refusal,
// not a retryable failure -- deny(), not fail() -- because a second attended
// child is never admissible while the first is still running; the parent
// must stop the existing one (delete its SubagentRequest) before asking for
// another.
const ReasonAttendedChildInProgress = "AttendedChildInProgress"

// ReasonAttendedRootNotChannelAttached is the terminal reason on an
// `attended` SubagentRequest whose delegation tree ROOT carries no channel
// binding at all (a kubectl-driven root, or one whose InputChannel/
// OutputChannel are both nil). Attended mode's whole point is a child that
// talks to the HUMAN on the root's channel (spec §4); a root with no channel
// has no human to hand the child, and building one anyway would silently
// produce a child that can neither be reached nor reply -- exactly the
// "never silently drop" failure CLAUDE.md rules out. deny(), not fail():
// this is permanent for the root as it stands, not a transient condition a
// retry could clear.
const ReasonAttendedRootNotChannelAttached = "AttendedRootNotChannelAttached"

// WorkshopBuildChecker answers workshop:<workshopID>#build for a session —
// satisfied by *pkg/authz/spicedb.Client.CheckWorkshopBuild. Declared as a
// local interface (same name/shape as pkg/controllers/workshopprobe and
// pkg/controllers/webhooks/workshop, deliberately not shared — three
// independently wired admission gates over one small method, not a
// dependency that would couple them) rather than folded into LineageWriter:
// this is the ONE relaxation of the same-namespace rule, worth its own
// nil-checked seam distinct from the lineage-writing half of SpiceDB this
// controller already depends on.
type WorkshopBuildChecker interface {
	CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error)
}

// LineageWriter is the SpiceDB half this controller needs. An interface so the
// unit tests need no SpiceDB, and so a typed-nil *spicedb.Client can never be
// assigned into it — declare the field as the interface, not the pointer.
type LineageWriter interface {
	TouchLineage(ctx context.Context, childNS, childName, parentNS, parentName string) error

	// CheckDelegationBlocked is the operator-side revocation lever:
	// agentsession:<parent>#delegation_blocked. true means an operator has
	// revoked this parent's delegation at runtime (a wildcard delegation_revoked
	// tuple), and this reconcile refuses the spawn — narrower than a `hold`,
	// which stops the session entirely. Checked here, in the operator, not in the
	// runner: the runner already asked (its delegate call created the request);
	// the platform decides whether to honour it.
	CheckDelegationBlocked(ctx context.Context, parentNS, parentName string) (bool, error)

	// SessionHasOnResource answers the data-slot attenuation question:
	// `pt_tag:<T>#access@agentsession:<parent>`. A SESSION subject, because
	// pt_tag's `access = session + session->ancestor + granted_to` is a fact
	// about the session a tag was minted in rather than about whoever started
	// it — no user-subject check can express it.
	SessionHasOnResource(ctx context.Context, resourceType, resourceID, permission string, session authz.SessionRef) (bool, error)

	// GrantDataSlots binds tags onto the CHILD. Called only with the subset
	// FilterDelegableDataSlots cleared.
	GrantDataSlots(ctx context.Context, ns, name string, bindings []authz.DataSlotBinding, expiresAt time.Time) error
}

// `subagentrequests` carries `delete` on top of the read verbs for one write:
// reclaimTerminal deletes a resolved request once it has been kept for
// DefaultTerminalRetention, which is what reclaims the child session and the
// per-request `agent` Channel through the owner-reference cascade. Without it
// a live conversational parent's finished delegations accumulate for its whole
// lifetime, because the cascade only ever runs from the parent's own deletion.
// `subagentrequests` carries `update` on top of the read/delete verbs above
// for one write: the finalizer prologue (EnsureFinalizer on the way in,
// RemoveFinalizer + a RetryOnConflict-wrapped Update in finalizeAttended on
// the way out) that gives an `attended` request a chance to notify its
// watching parent before deletion cascades its child away — finalizers live
// in metadata, so this is a base-resource write, not a status one. Every
// non-attended request still never carries the finalizer at all, so this is a
// no-op write it never reaches.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=subagentrequests,verbs=get;list;watch;delete;update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=subagentrequests/status,verbs=get;update;patch
// `agentsessions` carries `patch` on top of the read/create/delete verbs
// above for one write: notifyAttendedParent stamps the wake-requested-at
// annotation on the watching PARENT session (the same annotation
// channelsd's own applyWake writes), which also lives in metadata, not
// status.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch;create;delete;patch
// `agentsessions/status` carries exactly `update`, for one write:
// refuseExchange clears status.parentExchange.pending on a child whose question
// this delegation's mode has no budget left to carry. Nothing else here writes
// a child's status — completion propagation READS it and writes only the
// request's own. A marker is this controller's statement of need, not a copy of
// the operator's union, so the verb is claimed where the write is.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/status,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// `channels` carries exactly create AND delete, the two verbs this reconciler
// uses: buildAgentChannel creates the `agent` Channel a conversational child
// talks over, and rollback deletes it when a later step of the same delegation
// fails. Without delete the rollback path would leave the Channel behind on
// every partially-applied pass. It reads no Channel and watches none — the
// only Watch is on AgentSession (SetupWithManager) — so get/list/watch are
// deliberately NOT claimed here. The operator's Role holds them anyway, from
// the AgentSession and Channel reconcilers that do read Channels; a marker is
// this controller's own statement of need, not a copy of the union.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=channels,verbs=create;delete
// `namespaces` carries exactly `get`, the one verb admitCrossNamespaceParent
// uses: recovering the workshop-session labels a cross-namespace parent must
// be checked against from sr.Namespace's OWN namespace object, never from
// the (caller-supplied, untrusted) SubagentRequest's own fields. No list or
// watch — this controller never enumerates namespaces and reacts to no
// namespace event.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get

type Reconciler struct {
	client.Client
	// APIReader bypasses the informer cache for the one read where a stale
	// answer changes the outcome: notifyAttendedParent's Get of the watching
	// parent, which decides whether that parent is still live enough to be
	// told its child ended. A cache that still shows Running for a parent
	// that finished seconds ago would put the line in a transcript nobody
	// will ever read. Falls back to Client when nil (unit-test convenience,
	// mirroring pkg/controllers/workshop's APIReader).
	APIReader          client.Reader
	Scheme             *runtime.Scheme
	Authz              LineageWriter
	MaxDelegationDepth int

	// WorkshopBuild is the ONE relaxation of step 0's same-namespace rule: a
	// SubagentRequest whose parent is in a different namespace is admitted
	// only when sr.Namespace is a workshop provisioned for exactly that
	// parent (its own labels prove it) AND the parent still holds that
	// workshop's workshop:<W>#build SpiceDB tuple (spec §2.6). Declared as
	// the WorkshopBuildChecker interface, never *spicedb.Client — CLAUDE.md's
	// typed-nil rule — so an unwired dependency is a true nil interface,
	// checked explicitly in admitCrossNamespaceParent rather than a panic
	// controller-runtime's recovery would otherwise hide. Nil denies every
	// cross-namespace parent exactly as before this field existed: fail
	// closed, not fail open.
	WorkshopBuild WorkshopBuildChecker

	// Grader decides whether binding one tag into the child DISCLOSES anything
	// and whether the datum can be trusted — the half attenuation does not
	// answer. Attenuation bounds what the parent MAY delegate; grading asks
	// what delegating it would mean for the child's readers.
	//
	// Nil leaves attenuation as the only check, which is what shipped before
	// per-datum provenance existed. It is a function rather than an interface
	// because the caller supplies handoff.GradeRequest closed over its own
	// SpiceDB lookups, and this package must not grow a dependency on how those
	// are resolved.
	Grader func(ctx context.Context, child authz.SessionRef, tagID string) (handoff.Grade, string, error)

	// DeriveChildPlanRoot reads the PARENT's plan-gate log, folds it, and writes
	// the CHILD a projected plan-gate root — the phase the parent was in when it
	// delegated, as the child's own approved ceiling. nil when the operator did
	// not wire it (the child then starts ungated, the pre-Plan-1c behavior).
	// Injected rather than done inline so this package keeps no dependency on
	// how a plan-gate log is folded, exactly like Grader for the SpiceDB
	// lookups.
	DeriveChildPlanRoot func(ctx context.Context, parent, child authz.SessionRef) error

	// PublishInteraction publishes an envelope on a session's IN subject,
	// where channelsd parks the session and re-emits for the renderer (or, for
	// a KindUserMessage envelope, simply nudges a live runner). Two envelope
	// kinds ride this: an interaction_request, to actually ASK a human to rule
	// on a data slot's disclosure; and, from notifyAttendedParent, a
	// KindUserMessage, to publish the SAME NATS wakeup channelsd's own
	// applyWake/publishWakeup publishes for an ordinary human turn — reusing
	// the wake contract rather than inventing a second one.
	//
	// Nil means nobody is asked, and an attended parent's forced wake is
	// skipped (best-effort; the durable Memory.Append half still lands, so a
	// person who checks back still sees the fixed line even if the nudge
	// never fired). The routing still runs — a disclosing slot still parks and
	// still fails at the wait window with a stated reason — because a
	// controller that silently bound what it could not get a decision on
	// would be the one outcome worse than not asking.
	PublishInteraction func(ctx context.Context, ns, name string, env channelevents.Envelope) error

	// ParentMemory is the operator's in-process memory facade, used ONLY by
	// notifyAttendedParent to append the fixed "the test was stopped" / "the
	// test finished" line to an attended child's watching parent when the
	// child reaches a terminal phase or is deleted early. The turn Kind is
	// append-only (the transcript, per memory/kinds/turn), so this MUST be
	// wired to the operator's SIGNING facade (main.go's opSigned =
	// provenance.NewSigningMemory(memLocal, opSigner)), never to raw memLocal
	// directly — an unsigned append-only Put is rejected at write time (see
	// agentsession.Reconciler.LifecycleMemory's own doc, wired the same way
	// for the identical reason). Nil is a valid, best-effort wiring state: no
	// attended request is ever created without a live operator, but a
	// controller wired without it (a stripped-down test, an incremental
	// rollout) simply skips the notification rather than panicking — the
	// parent misses the line, not the whole reconcile.
	ParentMemory memory.Memory

	// TagSources returns the "type:id" objects a tag was minted from, whose
	// owners are the people entitled to rule on disclosing it. `pt_tag` has no
	// owner relation of its own, so this is the only route to a decider.
	//
	// Nil, or an empty result, means the card names no resources and the
	// category's DecideResourceOwners policy finds nobody — so it falls back
	// to the session approve-set rather than reaching an empty decider set.
	TagSources func(ctx context.Context, parent authz.SessionRef, tagID string) ([]string, error)

	// ResourceOwners expands "<type>:<id>#owner" to the canonical user ids who
	// may rule on that object. It turns TagSources' answer into the people a
	// card is actually delivered to.
	//
	// Nil, or an empty result, leaves the card with no approvers — which the
	// envelope contract REFUSES for an approvers-scope prompt, so the card is
	// not published and the request expires with its stated reason. That is
	// the honest outcome: a disclosure nobody can rule on is not one to
	// silently allow.
	ResourceOwners func(ctx context.Context, resourceType, resourceID string) ([]string, error)

	// DefaultMaxDelegatedAgents bounds a delegation tree when nothing in the
	// settings tiers sets a ceiling. It exists because an unset ceiling must
	// not read as "unlimited": zero is the right reading of an unset DURATION
	// (MaxDuration means "no time cap") and the wrong reading of an unset
	// COUNT, where it would license the runaway fan-out the ceiling exists to
	// prevent. An explicit tier value always wins.
	DefaultMaxDelegatedAgents int

	// MaxAwaitingParent bounds how long ONE unanswered question may park a
	// conversational child before this controller ends the delegation. Zero
	// falls back to DefaultMaxAwaitingParent — never to "no bound"; see
	// maxAwaitingParent.
	MaxAwaitingParent time.Duration

	// TerminalRetention is how long a RESOLVED request is kept before the
	// reclaim pass deletes it and, by owner-reference cascade, its child
	// session and their `agent` Channel. Zero falls back to
	// DefaultTerminalRetention — never to "reclaim immediately"; see
	// terminalRetention.
	TerminalRetention time.Duration

	// Now is the time source for that bound, injectable so the timeout is
	// testable without sleeping. Nil is time.Now.
	Now func() time.Time
}

// reader returns the uncached APIReader when wired, else the cached Client.
// See the APIReader field for the one read routed through it.
func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sr v1.SubagentRequest
	if err := r.Get(ctx, req.NamespacedName, &sr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion first, exactly like every other finalizer-using reconciler in
	// this repo (see publicendpoint.Reconciler): a deletion the finalizer
	// prologue below stamped must be routed here on every subsequent pass,
	// including reclaimTerminal's OWN r.Delete once a request has already
	// resolved — finalizeAttended tells the two apart (see its own doc).
	if !sr.DeletionTimestamp.IsZero() {
		return r.finalizeAttended(ctx, &sr)
	}

	// The finalizer goes on BEFORE the child can ever be created, so there is
	// never a window where a human could be talking to a live attended child
	// and a raw delete of this request skips straight past
	// finalizeAttended's chance to tell it. Scoped to `attended` alone —
	// every other mode never carries this finalizer, so its own deletion path
	// is completely unaffected (deny/fail/resolve outcomes, and
	// reclaimTerminal's own cascade, are byte-identical for every other
	// mode).
	//
	// Falls through to the rest of this pass rather than requeueing:
	// EnsureFinalizer's own Update already durably persisted the finalizer
	// before returning added=true, so there is no unsafe window left to wait
	// out — unlike a reconciler that would otherwise open some OTHER resource
	// after adding its finalizer, nothing below this point is unsafe to do in
	// the same pass.
	if sr.EffectiveMode() == v1.SubagentModeAttended {
		if _, err := apreconcile.EnsureFinalizer(ctx, r.Client, &sr, v1.FinalizerSubagentRequest); err != nil {
			return ctrl.Result{}, fmt.Errorf("add attended finalizer to SubagentRequest %s/%s: %w", sr.Namespace, sr.Name, err)
		}
	}

	if sr.IsTerminal() {
		return r.reclaimTerminal(ctx, &sr)
	}
	if sr.Status.ChildRef != nil {
		// The child already exists (this request already ran the create path
		// below on a prior reconcile). What's left is watching it to
		// completion, not re-creating it.
		return r.reconcileChild(ctx, &sr)
	}

	// 0. Same-namespace, with ONE relaxation. buildChild always places the
	// child in sr.Namespace (it has to: OwnerReferences are namespace-local,
	// so the child MUST live where its owning SubagentRequest does), while
	// sr.Spec.Parent.Namespace is caller-supplied and could name a different
	// one. A cross-namespace parent is refused UNLESS sr.Namespace is a
	// workshop namespace provisioned for exactly that parent AND the parent
	// still holds that workshop's SpiceDB workshop:<W>#build permission
	// (spec §2.6) — the one delegation-track exception the design allows: a
	// workshop's own builder session may run a child of the class it is
	// building, in the workshop namespace, and nowhere else.
	workshopCrossNs := false
	if sr.Spec.Parent.Namespace != sr.Namespace {
		admitted, reason, msg := r.admitCrossNamespaceParent(ctx, &sr)
		if !admitted {
			if reason == "" {
				reason = "ParentCrossNamespace"
				msg = fmt.Sprintf("parent %s/%s is in a different namespace than this request (%s); cross-namespace delegation is not supported",
					sr.Spec.Parent.Namespace, sr.Spec.Parent.Name, sr.Namespace)
			}
			return r.deny(ctx, &sr, reason, msg)
		}
		// Admitted: sr.Namespace is a workshop truly provisioned for this
		// parent, and the parent holds standing to build in it. Fall through —
		// every check below this point (roster, mode, graph, identity,
		// ceiling) still applies exactly as it does for a same-namespace
		// request; only the namespace-equality gate is relaxed. workshopCrossNs
		// records that this admission held, for the roster check below (R1) —
		// the ONE further gate this specific admission also relaxes.
		workshopCrossNs = true
	}

	parent, parentClass, err := r.loadParent(ctx, &sr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if parent == nil {
		return r.deny(ctx, &sr, "ParentMissing",
			fmt.Sprintf("the delegating session %q no longer exists", sr.Spec.Parent.Name))
	}
	if parent.Spec.GoalExecution != nil {
		return r.deny(ctx, &sr, "GoalExecutionDelegationDenied", "A bounded goal session cannot delegate; request a separately reviewed execution instead.")
	}
	if parentClass == nil {
		return r.deny(ctx, &sr, "ParentClassMissing",
			fmt.Sprintf("the delegating session %q's AgentClass %q no longer exists", parent.Name, parent.Spec.Class))
	}

	// 1. Roster membership. A class not on the roster is DENIED, not failed:
	// the parent must not re-attempt it through a differently-shaped request.
	// workshopChild records an R1 admission for the mode ceiling below, which
	// R1 also has to relax: a class no roster names has no roster-declared
	// modes either.
	workshopChild := false
	if !onRoster(parentClass, sr.Spec.Class) {
		// R1: a workshop child's class is authored inside W at build time, so it can
		// never be a literal member of the builder's fixed roster. Admit it iff
		// the request was admitted as a workshop cross-namespace request AND the class
		// genuinely lives in W (sr.Namespace) — the namespace the workshop#build tuple
		// already licensed this parent to build in. Any class not resolvable in W still
		// falls through to OffRoster, so this never widens the roster beyond W.
		//
		// An ordinary (non-workshop) off-roster request is denied immediately, before
		// the class Get: the bypass needs workshopCrossNs anyway, so gating the Get on
		// it keeps the common off-roster path byte-identical to the pre-R1 code — no
		// extra read, and a transient read error can never turn an off-roster denial
		// into a requeue for a request that was never eligible to bypass.
		inWorkshop := false
		if workshopCrossNs {
			var err error
			inWorkshop, err = r.childClassInWorkshop(ctx, &sr)
			if err != nil {
				return ctrl.Result{}, err // a transient Get error is retried, never a silent pass
			}
		}
		if !inWorkshop {
			return r.deny(ctx, &sr, "OffRoster",
				fmt.Sprintf("%q is not in %q's subagents roster", sr.Spec.Class, parentClass.Name))
		}
		workshopChild = true
	}

	// 1a. RequireSubagentDigestPins settings enforcement. Resolved (and pins
	// read) here, before Task 7's own pin check below, so both share the one
	// RosterPins() computation rather than reading the roster twice. This is a
	// REQUIREMENT, not a grant — settings.Resolve folds it as a ratchet any
	// tier may set and none may relax — checked against the same roster
	// membership already established at step 1.
	clusterSpec, nsSpec, err := settingswiring.FetchTiers(ctx, r.Client, sr.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetch settings tiers: %w", err)
	}
	// Named tierEff, not eff -- step 6 below binds `eff` to the delegation
	// tree ROOT's settings (a different resolution, for a different check);
	// reusing the name here would either shadow it pointlessly or, since both
	// sit in the same function scope, collide outright.
	tierEff, _ := settings.Resolve(settings.Inputs{Cluster: clusterSpec, Namespace: nsSpec})
	pins := parentClass.Spec.RosterPins()[sr.Spec.Class]
	if tierEff.RequireSubagentDigestPins && len(pins) == 0 {
		return r.deny(ctx, &sr, "DigestPinRequired",
			fmt.Sprintf("settings require digest-pinned delegation, but %q is unpinned on %q's roster", sr.Spec.Class, parentClass.Name))
	}

	// 1b. Digest pin. A pinned roster entry binds the delegation target to the
	// exact installed bundle digest — the "agent card" check. Missing install
	// record and mismatch both refuse: an unpinned INSTALL cannot satisfy a
	// pinned ENTRY, fail closed.
	if len(pins) > 0 {
		if len(pins) > 1 {
			return r.deny(ctx, &sr, "DigestPinConflict",
				fmt.Sprintf("%q appears on %q's roster with %d different digest pins; a contradictory roster pins nothing",
					sr.Spec.Class, parentClass.Name, len(pins)))
		}
		var target v1.AgentClass
		if err := r.Get(ctx, types.NamespacedName{Namespace: sr.Namespace, Name: sr.Spec.Class}, &target); err != nil {
			if apierrors.IsNotFound(err) {
				return r.deny(ctx, &sr, "ClassMissing",
					fmt.Sprintf("digest-pinned subagent class %q does not exist", sr.Spec.Class))
			}
			return ctrl.Result{}, err
		}
		if target.Status.OapInstall == nil || target.Status.OapInstall.Digest == "" {
			return r.deny(ctx, &sr, "DigestUnrecorded",
				fmt.Sprintf("%q is digest-pinned on %q's roster but has no recorded .oap install digest — install it from a bundle (oap agent install) or drop the pin",
					sr.Spec.Class, parentClass.Name))
		}
		if target.Status.OapInstall.Digest != pins[0] {
			return r.deny(ctx, &sr, "DigestPinMismatch",
				fmt.Sprintf("%q's installed digest %s does not match the roster pin %s on %q",
					sr.Spec.Class, target.Status.OapInstall.Digest, pins[0], parentClass.Name))
		}
	}

	// 2. The delegation-mode ceiling, checked HERE and beside membership on
	// purpose. spec.mode arrives from the delegating agent's own `delegate`
	// call, so it is a REQUEST; the grant is the PARENT class's
	// spec.subagentModes entry for this child, and this controller is the only
	// thing that reads the two together. Putting the check in the tool would
	// put it in the runner — the party the delegation constrains — and a
	// SubagentRequest can in any case be created by anything holding the CRD's
	// RBAC, which the tool never sees.
	//
	// It runs before the graph, identity and budget checks below because a
	// refused mode is a refusal of THIS request outright: none of those answers
	// changes it, and reaching them first would only widen the window in which
	// a mode nobody granted is on its way to provisioning a Channel.
	//
	// An unrecognized mode string is DENIED rather than read as empty. Empty
	// means single_turn (EffectiveMode), and collapsing a misspelling into the
	// default would run a delegation the agent did not ask for while telling
	// nobody. The CRD's own Enum refuses these at the apiserver on the tool's
	// path; this is the same answer for a request that reached etcd another
	// way, and for a stored object written before the enum existed.
	if sr.Spec.Mode != "" && !v1.IsSubagentMode(sr.Spec.Mode) {
		return r.deny(ctx, &sr, "SubagentModeUnrecognized",
			fmt.Sprintf("%q is not a delegation mode; valid modes are %v", sr.Spec.Mode, v1.SubagentModesAll()))
	}
	// Never downgraded to single_turn and run anyway: a silent downgrade makes
	// the feature look broken rather than refused, and hides the roster
	// misconfiguration behind it indefinitely. The message names exactly three
	// things — the mode asked for, the subagent it was asked for, and what the
	// roster actually allows — because the delegate tool hands Determination
	// verbatim to the parent's model, and that text is everything the agent
	// gets to learn from a refusal.
	permitted := parentClass.Spec.PermittedSubagentModes(sr.Spec.Class)
	if workshopChild {
		// R1, continued. The class was authored in W at build time, so no
		// roster names it and PermittedSubagentModes answers nil — which
		// refused every mode, attended and single_turn alike, and with them the
		// one workshop tool that creates these requests (test_tool): a
		// finished, valid build could not be rehearsed at all (observed live
		// on oap-desktop, 2026-09-13). A workshop child's ceiling is the
		// workshop's own testing surface, and nothing wider: single_turn is
		// what test_tool asks for; attended stays on the ceiling as headroom
		// for a delegation mode a workshop child would use, though no
		// workshop tool requests it today; task and chat stay refused,
		// because the workshop rehearses a build, it does not run it.
		permitted = workshopChildModes()
	}
	if !slices.Contains(permitted, sr.EffectiveMode()) {
		return r.deny(ctx, &sr, "SubagentModeNotPermitted",
			fmt.Sprintf("mode %q is not permitted for subagent %q by %q's roster, which allows %v",
				sr.EffectiveMode(), sr.Spec.Class, parentClass.Name, permitted))
	}

	// 2b. The operator-side revocation lever. delegation_blocked is a default-off
	// kill switch: an operator writes a wildcard `delegation_revoked` tuple onto a
	// parent session to stop it spawning ANY further children at runtime, without
	// holding the whole session (a `hold` stops the parent working entirely; this
	// stops only its delegation). Checked HERE, in the operator, and not in the
	// runner: the runner already asked — its delegate call is what created this
	// request — and the platform is the party that decides whether to honour it.
	//
	// FullyConsistent inside CheckDelegationBlocked: a revoke written moments
	// before a delegation must be seen by that delegation, or the lever races the
	// spawn it exists to stop. It runs before the graph, identity and budget
	// checks below for the same reason the mode ceiling does — a revoked
	// delegation is a refusal of THIS request outright, and reaching the
	// provisioning steps first would only widen the window in which a child
	// nobody may spawn is on its way to existing.
	if blocked, err := r.Authz.CheckDelegationBlocked(ctx, parent.Namespace, parent.Name); err != nil {
		// Fail closed but requeue: a SpiceDB read error is not a grant. Returning
		// the error (rather than deny()) leaves the request Pending and retries,
		// so a transient outage delays the delegation instead of terminally
		// refusing one that was never actually revoked.
		return ctrl.Result{}, fmt.Errorf("check delegation revocation for %s/%s: %w", parent.Namespace, parent.Name, err)
	} else if blocked {
		return r.deny(ctx, &sr, "DelegationRevoked",
			fmt.Sprintf("delegation from %q has been revoked by an operator", parent.Name))
	}

	// 3. Graph validity, re-checked here rather than trusted from the class's
	// Valid condition: a roster edited between admission and delegation would
	// otherwise be enforced against a stale answer.
	var classes v1.AgentClassList
	if err := r.List(ctx, &classes, client.InNamespace(sr.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list agentclasses: %w", err)
	}
	rosters := make(map[string][]string, len(classes.Items))
	var childClass *v1.AgentClass
	for i := range classes.Items {
		rosters[classes.Items[i].Name] = classes.Items[i].Spec.RosterNames()
		if classes.Items[i].Name == sr.Spec.Class {
			childClass = &classes.Items[i]
		}
	}
	// A cross-namespace admitted request (step 0's workshop relaxation):
	// parentClass lives in sr.Spec.Parent.Namespace, not sr.Namespace, so the
	// List above — scoped to sr.Namespace — never saw it, and the walk below,
	// rooted at parentClass.Name, would otherwise fail closed on "not an
	// AgentClass in this namespace" (or, worse, silently resolve against an
	// unrelated class that happens to share the name in sr.Namespace). Added
	// from the object loadParent already resolved from the PARENT's own
	// namespace — not a second List — so this entry always names the real
	// parentClass, and always wins over any same-named collision the first
	// loop populated. When the parent is same-namespace (the ordinary case,
	// parentClass.Namespace == sr.Namespace), the List above already carries
	// its own fresh entry and this is a no-op by construction.
	if parentClass.Namespace != sr.Namespace {
		rosters[parentClass.Name] = parentClass.Spec.RosterNames()
	}
	if childClass == nil {
		return r.deny(ctx, &sr, "ClassMissing",
			fmt.Sprintf("AgentClass %q does not exist", sr.Spec.Class))
	}
	if err := agentclass.ValidateRoster(parentClass.Name, rosters, r.MaxDelegationDepth); err != nil {
		return r.deny(ctx, &sr, v1.ReasonRosterInvalid, err.Error())
	}

	// 4. An ask/dynamic child class is refused delegation IN EVERY MODE,
	// including the conversational ones that now have a channel of their own.
	//
	// The refusal is about the monotonicity check at step 5, not about whether
	// anyone can see the prompt. That check runs exactly ONCE, here, against
	// the mode the child class resolves to at delegation time — and an
	// ask/dynamic class resolves to a PROVISIONAL "agent"
	// (ResolveEffectiveIdentityMode's documented behavior before a choice
	// lands), which passes trivially. The real mode is decided later, by a
	// person, and lands on the child's status.effectiveIdentityMode via the
	// identity-choice gate (pkg/controllers/agentsession/identitychoice.go).
	// Nothing re-runs CheckMonotonicIdentity when it does. So an ask child of
	// an agent-identity parent, admitted here, could be answered
	// "userPassthrough" half a minute later and run as a human its parent
	// never had the authority to act as — the exact laundering monotonic.go
	// exists to make unrepresentable.
	//
	// Admitting these would therefore mean building the re-check, on the
	// operator side that applies the choice, before the choice is applied.
	// That is a real design, not a flag flip, and it is not this change.
	//
	// This reads the child class's RAW spec.identityMode deliberately — every
	// other place in this plan that does so is a bug, but here the question is
	// "which flow would this class enter", not "what does it resolve to
	// today", and only the raw field answers that. ResolveEffectiveIdentityMode
	// would collapse ask/dynamic down to the same provisional "agent" this
	// check exists to catch, hiding the case entirely.
	if childClass.Spec.IdentityMode == v1.IdentityModeAsk || childClass.Spec.IdentityMode == v1.IdentityModeDynamic {
		return r.deny(ctx, &sr, "ChildIdentityModeUnsupported",
			fmt.Sprintf("AgentClass %q uses identityMode %q, so its identity is chosen after the session starts; a delegated child's identity is checked against its parent's only once, at delegation, and nothing re-checks it when a later choice lands",
				childClass.Name, childClass.Spec.IdentityMode))
	}

	// 5. Identity monotonicity.
	parentMode := agentsession.ResolveEffectiveIdentityMode(parent, parentClass)
	childMode := agentsession.ResolveEffectiveIdentityMode(nil, childClass)
	// ResolveStartedByCanonical is a PACKAGE-LEVEL function, not a method — and
	// it is the one to use. Its doc: "Use this, never a hand-rolled
	// derivation. The two encodings are NOT interchangeable" — the stamped
	// canonical keys on the channel-verified email, while a bare derivation
	// can only produce the synthetic base64url(kind:teamScope:externalID), a
	// DIFFERENT subject for the same person, and the substitution is silent
	// because an email-less subject is ordinary for a guest.
	//
	// The child INHERITS the parent's starter (set in buildChild), so
	// equality holds by construction on this path. Both are passed anyway
	// rather than collapsing the signature: the parameter is what stops a
	// future caller that derives a child starter differently from skipping
	// the check silently.
	starter := v1.ResolveStartedByCanonical(parent).String()
	if err := CheckMonotonicIdentity(parentMode, childMode, starter, starter); err != nil {
		return r.deny(ctx, &sr, "IdentityWidens", err.Error())
	}

	// 6. Pooled tree ceiling. MaxDelegatedAgents is a property of the whole
	// delegation TREE, not of any one session (BudgetConfig.MaxDelegatedAgents's
	// doc comment), so it is resolved from the ROOT's effective settings —
	// never the parent's or the child's own class — and checked against the
	// root's own closure.
	//
	// None of the lookups below silently skips the check -- the root, its
	// class, its effective settings, its own-namespace closure, its own
	// workshop namespace (Task 9, spec §13: a builder root's workshop
	// children pool into this SAME ceiling, so the ROOT's Workshop CR is
	// resolved here too), and that workshop namespace's own closure. Each
	// classifies its error the same way loadParent and the roster List above
	// it already do -- one idiom for the whole function, not a bolted-on
	// exception per lookup: IsNotFound or IsForbidden means the root, its
	// class, or a closure is permanently gone or the controller lacks the
	// RBAC to see it, and no amount of retrying fixes that, so it resolves
	// the request Failed via fail(). Any other error (a transient Get/List
	// failure) falls through to a wrapped return and is retried like every
	// other genuine infrastructure error in this function. v1.ErrLineageCycle
	// joins that same permanent
	// bucket below: admission DAG-validates rosters so a cycle should never
	// reach here, but IF one does, it is a defect in stored state, not a
	// transient read -- no amount of retrying un-loops a parent chain, so
	// treating it as retryable would hang this request (and the parent's
	// delegate call, up to its poll deadline) forever instead of surfacing
	// the corruption.
	root, err := v1.ResolveRoot(ctx, r.Client, parent)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || errors.Is(err, v1.ErrLineageCycle) {
			return r.fail(ctx, &sr, "RootUnresolvable",
				fmt.Sprintf("could not resolve the delegation tree's root to check its agent ceiling: %s", err))
		}
		return ctrl.Result{}, fmt.Errorf("resolve delegation root: %w", err)
	}
	var rootClass v1.AgentClass
	if err := r.Get(ctx, types.NamespacedName{Namespace: root.Namespace, Name: root.Spec.Class}, &rootClass); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return r.fail(ctx, &sr, "RootClassMissing",
				fmt.Sprintf("could not load the delegation tree root %q's AgentClass %q to check its agent ceiling: %s",
					root.Name, root.Spec.Class, err))
		}
		return ctrl.Result{}, fmt.Errorf("get root session's AgentClass: %w", err)
	}
	eff, _, err := settingswiring.ResolveForSession(ctx, r.Client, &rootClass, root)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return r.fail(ctx, &sr, "RootSettingsUnresolvable",
				fmt.Sprintf("could not resolve the delegation tree root %q's effective settings to check its agent ceiling: %s",
					root.Name, err))
		}
		return ctrl.Result{}, fmt.Errorf("resolve root session's effective settings: %w", err)
	}
	ceiling := int(eff.Budget.MaxDelegatedAgents)
	if ceiling <= 0 {
		// Nothing in any settings tier set a ceiling. Zero means UNSET, not
		// unlimited — an unset COUNT must not read as "no cap" (that reading is
		// right for a DURATION, wrong here), so fall back to the built-in bound
		// rather than skipping the check. Only when the fallback is ALSO <= 0
		// has an operator explicitly disabled the bound, and only then is the
		// check genuinely skipped.
		ceiling = r.DefaultMaxDelegatedAgents
	}
	if ceiling > 0 {
		// This is List-then-Create, not atomic: two SubagentRequests delegating
		// concurrently off the same tree (two different members firing at once,
		// or a harness issuing parallel tool calls from one turn -- one parent
		// alone cannot race itself, since delegate is a synchronous
		// create-and-poll call) can both ListClosure before either Creates, both
		// observe a count under the ceiling, and both proceed, landing the tree
		// over its stated bound. The overshoot is bounded by the number of
		// concurrent requests and visible after the fact in the label-selector
		// count, so it is accepted rather than built out: the spec calls this
		// ceiling a runtime backstop, which may be best-effort, and a
		// compare-and-swap or leased counter to close the race is materially
		// more design than this check. What is not acceptable is
		// implying a hardness this mechanism does not have, hence this comment.
		closure, err := v1.ListClosure(ctx, r.Client, root.Namespace, root.Name)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				return r.fail(ctx, &sr, "RootClosureUnresolvable",
					fmt.Sprintf("could not list the delegation tree rooted at %q to check its agent ceiling: %s", root.Name, err))
			}
			return ctrl.Result{}, fmt.Errorf("list delegation closure: %w", err)
		}
		// Task 9 (spec §13): a builder root's own workshop children pool into
		// this SAME ceiling -- they carry the root's own LabelDelegationRoot
		// (buildChild stamps it regardless of which namespace the child lands
		// in), but they live in the workshop namespace W, never root.Namespace,
		// so the ListClosure call above -- scoped to root.Namespace alone --
		// never sees them. WorkshopNamespacesFor resolves W from root's OWN
		// Workshop CR only (never a cluster-wide scan), and is nil-safe: a
		// non-workshop root gets back (nil, nil), workshopNamespaces stays
		// empty, and every loop below it is a no-op -- so a non-workshop tree's
		// count is byte-identical to what it was before this task.
		workshopNamespaces, err := v1.WorkshopNamespacesFor(ctx, r.Client, root)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				return r.fail(ctx, &sr, "RootClosureUnresolvable",
					fmt.Sprintf("could not resolve the delegation tree root %q's own workshop namespace to check its agent ceiling: %s", root.Name, err))
			}
			return ctrl.Result{}, fmt.Errorf("resolve root's workshop namespaces: %w", err)
		}
		var workshopMembers []v1.AgentSession
		for _, wsNS := range workshopNamespaces {
			// Same ListClosure this whole check already uses, just pointed at W
			// instead of root.Namespace -- the label selector (LabelDelegationRoot
			// == root.Name) is identical, only the namespace argument differs, so
			// ListClosure itself needed no change at all.
			members, lerr := v1.ListClosure(ctx, r.Client, wsNS, root.Name)
			if lerr != nil {
				if apierrors.IsNotFound(lerr) || apierrors.IsForbidden(lerr) {
					return r.fail(ctx, &sr, "RootClosureUnresolvable",
						fmt.Sprintf("could not list the delegation tree rooted at %q in its workshop namespace %q to check its agent ceiling: %s", root.Name, wsNS, lerr))
				}
				return ctrl.Result{}, fmt.Errorf("list delegation closure in workshop namespace %q: %w", wsNS, lerr)
			}
			workshopMembers = append(workshopMembers, members...)
		}
		// +1 for the root itself: ListClosure returns DESCENDANTS ONLY -- the
		// root carries no LabelDelegationRoot naming itself, so it is never a
		// member of its own closure (see ListClosure's doc comment). Omitting
		// this would undercount every tree by exactly one.
		//
		// A child THIS SAME REQUEST already created on an earlier pass is
		// excluded from the count. childName derives it from the request
		// rather than minting it, which is what makes the exclusion exact
		// rather than a guess. Reconcile only reaches this check with
		// sr.Status.ChildRef == nil -- true on a genuine first pass, but ALSO
		// true on a retry whose final status write (the one that persists
		// ChildRef, at the bottom of this function) failed after the child and
		// its lineage tuples were already committed. Without this exclusion,
		// that retry re-enters this exact check, counts the child it already
		// created as one MORE agent, and can deny a delegation whose child is
		// already running: the request goes terminal Denied with ChildRef nil
		// while the child keeps its slot in the tree, unobserved and no longer
		// counted against it. This is the first stateful check placed in front
		// of a non-idempotent action whose own effect it counts -- before the
		// ceiling existed, the identical retry was safe (Create returned
		// AlreadyExists and was ignored).
		count := 1 // the root itself; ListClosure returns descendants only
		ownChildName := childName(&sr)
		for i := range closure {
			if closure[i].Namespace == sr.Namespace && closure[i].Name == ownChildName {
				continue
			}
			count++
		}
		// Workshop members are counted only while LIVE (non-terminal). A
		// workshop test child churns far more than an ordinary delegated child
		// (spec §13) -- counting every one the workshop has EVER run, the way
		// the same-namespace half above counts every descendant ever created,
		// would make a workshop-heavy tree's ceiling far stricter than the
		// pooled budget is meant to be. The same-namespace half is deliberately
		// left as-is; only this new half is phase-filtered.
		//
		// The identical own-child-from-a-retry exclusion above applies here
		// too, keyed off the same (sr.Namespace, ownChildName) pair: a
		// cross-namespace SubagentRequest's child is always created IN
		// sr.Namespace (buildChild has to -- OwnerReferences are
		// namespace-local), which for a workshop delegation IS the workshop
		// namespace, so a retry whose final status write failed after the
		// child was already committed must not count that same child against
		// itself here any more than it may in the root's own namespace.
		for i := range workshopMembers {
			if workshopMembers[i].Namespace == sr.Namespace && workshopMembers[i].Name == ownChildName {
				continue
			}
			if isTerminalAgentSessionPhase(workshopMembers[i].Status.Phase) {
				continue
			}
			count++
		}
		if count >= ceiling {
			return r.deny(ctx, &sr, "TreeCeilingExceeded",
				fmt.Sprintf("delegation tree rooted at %q already has %d of %d allowed agents; creating another %q would exceed the ceiling",
					root.Name, count, ceiling, sr.Spec.Class))
		}
	}

	// 6.5. attended's own two preconditions, checked only for that mode and
	// only after every check above has already passed -- neither widens what a
	// non-attended request may do, so there is no reason to pay for either on
	// a path that can never reach them.
	if sr.EffectiveMode() == v1.SubagentModeAttended {
		// A root with no channel binding at all has no human to hand the
		// child: see ReasonAttendedRootNotChannelAttached's doc for why this
		// is a denial (permanent for the root as it stands) rather than a
		// failure (retryable).
		if v1.OutboundBinding(root) == nil {
			return r.deny(ctx, &sr, ReasonAttendedRootNotChannelAttached,
				fmt.Sprintf("the delegation tree rooted at %q has no channel binding to hand an attended child; "+
					"attended mode requires a human on the other end", root.Name))
		}

		// One LIVE attended child at a time across the WHOLE tree (spec
		// §lifecycle), checked against the root's closure rather than any one
		// member's own subtree: whichever session in the tree asks, the
		// person on the other end is the SAME root binding, so two live
		// attended children would put two concurrent conversations in front
		// of the one human, each unaware of the other. Uses the same
		// ListClosure the pooled ceiling above does, requeried here rather
		// than shared with it: that List runs only when ceiling > 0, and this
		// check must run regardless of whether any ceiling is configured.
		closure, err := v1.ListClosure(ctx, r.Client, root.Namespace, root.Name)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				return r.fail(ctx, &sr, "RootClosureUnresolvable",
					fmt.Sprintf("could not list the delegation tree rooted at %q to check for a live attended child: %s", root.Name, err))
			}
			return ctrl.Result{}, fmt.Errorf("list delegation closure for attended-child check: %w", err)
		}
		// This same request's own child, from a retry after a failed final
		// status write -- the identical shape the tree-ceiling exclusion
		// above guards against, and for the same reason: it must not deny
		// itself against a child it already created.
		ownChildName := childName(&sr)
		for i := range closure {
			member := &closure[i]
			if member.Namespace == sr.Namespace && member.Name == ownChildName {
				continue
			}
			if member.Labels[v1.LabelAttendedParentName] == "" {
				continue // not an attended child
			}
			if isTerminalAgentSessionPhase(member.Status.Phase) {
				continue // resolved; no longer "in progress"
			}
			return r.deny(ctx, &sr, ReasonAttendedChildInProgress, "a test is already running; stop it first")
		}
	}

	// 7. Create the child, then its Channel (conversational modes only), then
	// write lineage. Order matters in the other direction than it looks: a
	// tuple naming a session that does not exist is inert, and so is a Channel
	// naming one, whereas a session with no tuple has standing that resolves to
	// NOBODY — its approvals would reach no one and its transcript would be
	// unreadable, and a conversational child with no Channel can neither be
	// spoken to nor answer. So create, then link. If a later step fails, roll
	// back what was already created rather than leaving a half-provisioned
	// child: the compensating delete means a retry rebuilds from scratch
	// instead of depending on a future reconcile to notice and repair an
	// orphan.
	// Data slots are settled BEFORE any child exists.
	//
	// The obvious order — create, grade, roll back if a slot needs a decision
	// — is racy in a way that surfaces as a lie to the parent. The child's
	// name is deterministic, so the delete and the next pass's create contend
	// over one object, and a parent polling its delegation can observe the
	// gap: it reported ChildVanished, i.e. "the delegation failed", for a
	// delegation that was merely waiting on a person.
	//
	// Grading a child that does not exist is sound because its audience is
	// KNOWN in advance and is exactly its parent's. read_transcript is
	// `interact + parent->read_transcript`, interact is
	// `owner + started_by + participant`, and buildChild writes NONE of those
	// three onto the child — only Class, Parent, Prompt and InputChannel. So
	// the child contributes an empty own-audience and inherits the parent's
	// whole one, which makes the parent's audience the same set rather than a
	// proxy for it.
	//
	// If buildChild ever grants the child a subject of its own, that subject
	// has to be added here, or a datum could be cleared against an audience
	// narrower than the one that will actually hold it.
	if done, res, err := r.settleDataSlotsBeforeChild(ctx, &sr, parent); err != nil || !done {
		return res, err
	}

	child := r.buildChild(&sr, parent, root)

	// The child inherits its PORTION of the parent's plan as its own plan-gate
	// root (the phase the parent was in when it delegated), projected to the
	// child's scope. Written BEFORE Create, deliberately: the child's runner
	// reads its plan-gate log ONCE at startup, and it starts as soon as the
	// child AgentSession CR exists — so a write after Create races the runner
	// and loses, leaving a plan-gated child with no root that fails every
	// permissioned call with "requires an approved plan" and cannot recover
	// (a child has no update_plan). Writing to the child's memory scope before
	// the CR exists is safe: the scope is a key, not a live object, and an
	// orphan root from a failed Create below is never read. The func is
	// idempotent, so an AlreadyExists retry does not append a second root.
	// Injected (mirroring Grader) so this package keeps no fold dependency. A
	// parent that ran no plan gate leaves the log empty and nothing is written.
	// NOT fatal: a missing root fails the child's gate CLOSED, the safe
	// direction; logged, never swallowed.
	if r.DeriveChildPlanRoot != nil {
		if err := r.DeriveChildPlanRoot(ctx,
			authz.SessionRef{Namespace: parent.Namespace, Name: parent.Name},
			authz.SessionRef{Namespace: child.Namespace, Name: child.Name}); err != nil {
			log.FromContext(ctx).Info("subagentrequest: derive child plan-gate root failed; "+
				"the child starts without an inherited ceiling and its gate fails closed until it re-plans",
				"parent", parent.Namespace+"/"+parent.Name, "child", child.Namespace+"/"+child.Name,
				"err", err.Error())
		}
	}

	if err := r.Create(ctx, child); err != nil && !apierrors.IsAlreadyExists(err) {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			// Same hang shape loadParent's ParentClassMissing case exists to
			// fix, one CRD later: wrapping and returning ANY Create error here
			// backs off exponentially and retries forever, but neither of
			// these is something a retry can fix. IsInvalid is a permanently
			// malformed object -- e.g. sr.Name+"-child" exceeding the
			// 253-character name limit for a long session name; IsForbidden is
			// an RBAC denial of the controller's own identity, not a
			// transient API hiccup. Route both through fail() so the request
			// reaches a phase (and the parent's delegate tool call returns)
			// within the poll deadline instead of hanging for the full
			// timeout. A genuinely transient error (anything else) still
			// falls through to the wrapped return below and gets retried.
			return r.fail(ctx, &sr, "ChildCreateRejected",
				fmt.Sprintf("creating the child session was rejected: %s", err))
		}
		return ctrl.Result{}, fmt.Errorf("create child session: %w", err)
	}
	// The `agent` Channel a conversational child talks over. nil for
	// single_turn, which stays headless (buildAgentChannel's doc).
	channel := buildAgentChannel(&sr, parent)
	if channel != nil {
		if err := r.Create(ctx, channel); err != nil && !apierrors.IsAlreadyExists(err) {
			// The Channel is handed to rollback even though this IS its failed
			// Create: a Create whose write landed but whose response was lost
			// returns something other than AlreadyExists, so the object can
			// exist despite the error. rollback treats NotFound as success, so
			// naming it costs nothing when it genuinely was not created — and
			// on the terminal IsInvalid/IsForbidden branch below, passing nil
			// would strand that Channel indefinitely, with no later reconcile
			// coming to notice it.
			if rbErr := r.rollback(ctx, child, channel); rbErr != nil {
				return ctrl.Result{}, fmt.Errorf("create child channel: %w (%v)", err, rbErr)
			}
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
				// Same permanent-vs-transient split the child Create above
				// makes, for the same reason: neither an over-long generated
				// name nor an RBAC denial of this controller's own identity is
				// fixed by retrying, and hanging here would hang the parent's
				// delegate call to its poll deadline.
				return r.fail(ctx, &sr, "ChildChannelCreateRejected",
					fmt.Sprintf("creating the child's %s channel was rejected: %s", agent.KindName, err))
			}
			return ctrl.Result{}, fmt.Errorf("create child channel: %w", err)
		}
	}
	if err := r.Authz.TouchLineage(ctx, child.Namespace, child.Name, parent.Namespace, parent.Name); err != nil {
		if rbErr := r.rollback(ctx, child, channel); rbErr != nil {
			return ctrl.Result{}, fmt.Errorf("write lineage tuples: %w (%v)", err, rbErr)
		}
		return ctrl.Result{}, fmt.Errorf("write lineage tuples: %w", err)
	}

	// Data slots, AFTER lineage: the child exists and its tree edges are
	// written, so a bound tag is reachable and a failure here rolls back a
	// child that is already accounted for rather than one half-built.
	bound, refused, pending, err := r.bindDataSlots(ctx, &sr, child, parent)
	if err != nil {
		if rbErr := r.rollback(ctx, child, channel); rbErr != nil {
			return ctrl.Result{}, fmt.Errorf("bind data slots: %w (%v)", err, rbErr)
		}
		return ctrl.Result{}, fmt.Errorf("bind data slots: %w", err)
	}

	// Nothing can be pending here: settleDataSlotsBeforeChild decided every
	// slot before this child was built, so anything still undecided returned
	// long before the Create above. A pending slot at this point would mean
	// that gate was bypassed, and binding it would start a child on a datum
	// nobody cleared.
	if len(pending) > 0 {
		if rbErr := r.rollback(ctx, child, channel); rbErr != nil {
			return ctrl.Result{}, fmt.Errorf("unsettled data slots after the pre-create gate: %w", rbErr)
		}
		return ctrl.Result{}, fmt.Errorf(
			"unsettled data slots reached child creation for %s/%s (%d pending); "+
				"settleDataSlotsBeforeChild should have returned first",
			sr.Namespace, sr.Name, len(pending))
	}

	sr.Status.ChildRef = &v1.NamespacedRef{Namespace: child.Namespace, Name: child.Name}
	sr.Status.BoundDataSlots = bound
	// APPENDED, never assigned: the pre-create gate records its own refusals
	// (a slot whose grade could not be computed) on this same object, and
	// overwriting here would drop them — leaving a parent told nothing about a
	// slot that will never arrive, which is the exact failure the refused list
	// exists to prevent.
	sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, refused...)
	// Cleared: every slot that was pending has now been decided, and a stale
	// list here would have channelsd re-publish a card for a settled question.
	sr.Status.PendingDataSlots = nil
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Status.Determination = fmt.Sprintf("delegated to %s", child.Name)
	return ctrl.Result{}, r.Status().Update(ctx, &sr)
}

func (r *Reconciler) buildChild(sr *v1.SubagentRequest, parent, root *v1.AgentSession) *v1.AgentSession {
	// The child's tree root is the parent's root if the parent carries one,
	// and the parent's own name otherwise (a parent without the label IS a
	// root). Computed once here, from the parent alone -- no API call, no walk
	// -- so the closure (pooled ceiling, hold cascade, denial streak) is one
	// indexed List rather than a walk per member.
	labels := map[string]string{v1.LabelDelegationRoot: v1.RootNameFor(parent)}

	// A task/chat child additionally gets the `agent` Channel built by
	// buildAgentChannel, bound here as its INPUT channel. That binding is what
	// makes the child the BOUND END of the conversation: channelsd resolves an
	// agent message's Channel from the (target, sender) pair, and this is the
	// K8s-witnessed half of it — the Channel's own spec.authzSubject names the
	// other end (pkg/channels/channelsd/pipeline's resolvePairChannel).
	//
	// The three channel-correlation labels are stamped alongside it because
	// channelsd's session sweeps select on them (its startup-status and
	// credential-request passes list by LabelChannelName). They do NOT route
	// an agent message: HandleAgentMessageSend names the target session
	// explicitly, since a child->parent message rides a Channel whose
	// correlation labels are on the child.
	//
	// An attended child is provisioned differently again (below): it gets NO
	// agent Channel and no correlation labels at all -- its counterparty is a
	// human on the ROOT's own channel, not the parent, and the label-selector
	// sweeps those correlation labels feed must keep seeing exactly one
	// session per (channel, key): the root. Routing the root's inbound turns
	// to the attended child while it runs is Tasks 6-7's own job, keyed off
	// LabelAttendedParentNamespace/Name below, not off a shared
	// channel-correlation label.
	//
	// A single_turn child gets neither: it is headless by construction, its
	// prompt goes in through spec.prompt and its answer comes back through
	// status.result, and Track 1a is built on that shape.
	var inputChannel, outputChannel *v1.ChannelBinding
	switch sr.EffectiveMode() {
	case v1.SubagentModeAttended:
		// The child's conversation is with the HUMAN on the ROOT session's own
		// channel (spec §4, Ruling 1 of this plan's ledger), never with the
		// parent that delegated it -- so both bindings are stamped from the
		// ROOT's own outbound binding (root.Spec.OutputChannel, falling back to
		// root.Spec.InputChannel: the surface the person is actually on), not
		// the parent's.
		//
		// A nil root binding (a kubectl-driven root, or one with no channel
		// attachment at all) leaves both fields nil -- Reconcile refuses the
		// request before reaching here (ReasonAttendedRootNotChannelAttached)
		// rather than build a child that could never be attended in fact.
		if rb := v1.OutboundBinding(root); rb != nil {
			prefix := channelevents.SubjectPrefix(sr.Namespace, childName(sr))
			inputChannel = attendedChildBinding(rb, prefix)
			outputChannel = attendedChildBinding(rb, prefix)
		}
		// The watch relationship Tasks 6-7 read to route the root's inbound
		// turns to this child and append them to the root's own inbox: sr's
		// OWN parent, not necessarily the root -- an attended child two hops
		// down a tree is still watched by the session that actually asked for
		// it. Set whenever the request IS attended, independent of whether a
		// root binding resolved above: the labels name a watch relationship,
		// not a channel binding, and a human still needs a way to see why an
		// attended child was created mute. Two labels, not one combined value
		// -- see LabelAttendedParentNamespace/Name's doc for why a single
		// "<namespace>/<name>" label value is invalid on a real apiserver.
		labels[v1.LabelAttendedParentNamespace] = sr.Spec.Parent.Namespace
		labels[v1.LabelAttendedParentName] = sr.Spec.Parent.Name
	default:
		if channelName := agentChannelNameFor(sr); channelName != "" {
			key := agentChannelKey(childName(sr))
			inputChannel = &v1.ChannelBinding{
				Name: channelName,
				Kind: agent.KindName,
				Key:  key,
				// Straight off the kind rather than a literal list, which is what
				// keeps a binding's declared format capabilities in step with what
				// the kind actually renders. They shape no tool schema for THIS
				// child: respond_to_user is withheld from a session whose binding
				// reaches another session (pkg/agent/tool/meta/capability's
				// respondToUserSkip), and it is the only tool that reads them.
				Capabilities:      agent.Kind{}.Capabilities(),
				NATSSubjectPrefix: channelevents.SubjectPrefix(sr.Namespace, childName(sr)),
			}
			labels[v1.LabelChannelName] = channelName
			labels[v1.LabelChannelKind] = agent.KindName
			labels[v1.LabelChannelKey] = channelkey.LabelValue(key)
		}
	}

	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sr.Namespace,
			Name:      childName(sr),
			Labels:    labels,
			// The child inherits the parent's starter verbatim. A passthrough
			// child MUST act as the same human (CheckMonotonicIdentity), and the
			// owner resolver reads this annotation to decide ownership; deriving
			// it any other way would let the two disagree.
			Annotations: startedByAnnotations(parent),
			// Owner-ref to the REQUEST, not the parent session: the request is
			// what this child answers, and GC'ing the child with the request
			// keeps the lifetimes aligned.
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sr,
				v1.SchemeGroupVersion.WithKind("SubagentRequest"))},
		},
		Spec: v1.AgentSessionSpec{
			Class:         sr.Spec.Class,
			Parent:        &v1.NamespacedRef{Namespace: parent.Namespace, Name: parent.Name},
			Prompt:        v1.PromptSource{Inline: sr.Spec.Task},
			InputChannel:  inputChannel,
			OutputChannel: outputChannel,
		},
	}
}

// attendedChildBinding copies rb (the root's own outbound binding) for use as
// an attended child's channel binding, preserving the DESTINATION —
// Name/Kind/Key/Capabilities/External — while overriding NATSSubjectPrefix to
// subjectPrefix, the CHILD's own.
//
// The destination fields are copied wholesale on purpose: Name is the Channel
// CR the resolver actually loads to deliver
// (pkg/channels/channelkinds/resolve.ForSession prefers OutputChannel), and
// External carries the destination routing metadata (e.g. a Slack
// thread_ts/channel_id) that lands the child's replies in the SAME thread the
// person is already reading, not a new one.
//
// NATSSubjectPrefix is the one field that must NOT be copied verbatim.
// internal/cmd/runner reads InputChannel.NATSSubjectPrefix to initialize ITS
// OWN NATS connection (main.go's `subjectPrefix = binding.NATSSubjectPrefix`);
// copying the root's prefix onto the child would have the child's runner
// connect on the ROOT's own session subjects -- two live runners sharing one
// session's inbound/outbound NATS traffic, not a channel choice. Every
// caller must derive subjectPrefix the same way buildChild always has for a
// channel-bound child: channelevents.SubjectPrefix(sr.Namespace,
// childName(sr)).
//
// External is cloned, not aliased, so a future in-place mutation of the
// copy (there is none today -- every consumer reassigns the field rather
// than mutating the map, see outbound/relay.go's patchOutputChannel) can
// never reach back into the root's own live AgentSession object.
func attendedChildBinding(rb *v1.ChannelBinding, subjectPrefix string) *v1.ChannelBinding {
	cp := *rb
	cp.External = maps.Clone(rb.External)
	cp.NATSSubjectPrefix = subjectPrefix
	return &cp
}

// childName is the delegated child's AgentSession name. Derived from the
// request, never minted, so a retry after a partially-applied pass re-Creates
// the SAME object and gets AlreadyExists instead of a second child -- the
// property the tree-ceiling exclusion above depends on to be exact rather than
// approximate.
func childName(sr *v1.SubagentRequest) string { return sr.Name + "-child" }

// agentChannelNameFor is the name of the `agent` Channel a conversational
// child talks over, and "" for a mode that gets no Channel at all. One
// function answers BOTH questions on purpose: the name and the existence of
// the Channel must never be decided by two switches that can disagree, and
// every caller here needs them together.
//
// Deterministic from the request, for the same reason childName is: Create is
// not idempotent, this controller re-enters the create path on any retry whose
// final status write failed, and a minted name would leave a second orphan
// Channel on every such retry.
//
// The "-inbox" suffix is a historical name, kept only because renaming it
// would orphan the Channel of every delegation already running. The Channel is
// not an inbox: it is the conversation between the parent and the child, both
// directions ride it, and channelsd resolves it from the (target, sender) pair
// rather than from either end's own binding.
//
// task and chat resolve to the SAME provisioning, and this switch is the only
// place spec.mode drives the `agent` Channel's PROVISIONING: it decides
// whether ONE exists at all, nothing more. attended also falls through to the
// default "" here -- correctly, since an attended child gets no `agent`
// Channel either -- but its OWN bindings are a distinct decision made
// directly in buildChild (a copy of the ROOT's own channel, not a new
// Channel object), which this function knows nothing about. (Reconcile reads
// spec.mode once more, at step 2, to decide whether the mode is permitted at
// all — an authorization question, not a provisioning one, and it never
// reaches here when the answer is no.)
//
// The declared difference between task and chat IS enforced, and deliberately
// not here: the exchange budget in reconcileParentExchange (via
// SubagentRequest.ExchangeBudget), and the initiative gate on the inbound path
// (via SubagentRequest.PermitsHumanInitiative). Both read spec.mode at the
// point the question arises. Do not encode either as a provisioning difference
// here, which would put an attack-surface decision in two places.
func agentChannelNameFor(sr *v1.SubagentRequest) string {
	switch sr.EffectiveMode() {
	case v1.SubagentModeTask, v1.SubagentModeChat:
		return sr.Name + "-inbox"
	default:
		return ""
	}
}

// agentChannelKey is the channelKey the child's binding is correlated on. An
// `agent` Channel is dedicated to exactly one conversation for its whole
// lifetime (the kind's DefaultSessionScope is "singleton"), so the child's own
// name is the only axis there is to key on.
//
// It does not route agent messages. Those resolve their Channel from the
// (target, sender) pair and name the target session explicitly, precisely
// because these correlation labels sit on the CHILD and so would resolve a
// child -> parent message back to its own sender. What the key is still for is
// the channelsd session sweeps that select on the labels stamped beside it.
func agentChannelKey(sessionName string) string { return agent.KindName + ":" + sessionName }

// buildAgentChannel returns the `agent` Channel a task/chat child is bound to,
// or nil for a single_turn child (which is headless and gets none).
//
// spec.authzSubject names the PARENT, the far end of the conversation this
// Channel IS: the child is the end BOUND to it (spec.inputChannel above), so
// naming the parent here is what identifies the pair. channelsd matches the
// two together — target and claimed sender against bound end and counterparty
// — before it will carry a message either way, so naming the child here
// instead would leave the Channel joining the child to itself and every
// message between the pair unroutable.
//
// It is NOT the acting subject: an arriving message is attributed to the
// session the NATS subject it arrived on authorized, never to this field
// (pkg/channels/channelsd/pipeline.HandleAgentMessageSend).
//
// Owner-ref'd to the REQUEST, exactly as the child session is, so the two are
// garbage-collected together and a Channel can never outlive the delegation it
// exists to serve.
func buildAgentChannel(sr *v1.SubagentRequest, parent *v1.AgentSession) *v1.Channel {
	name := agentChannelNameFor(sr)
	if name == "" {
		return nil
	}
	return &v1.Channel{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sr.Namespace,
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sr,
				v1.SchemeGroupVersion.WithKind("SubagentRequest"))},
		},
		Spec: v1.ChannelSpec{
			Kind:         agent.KindName,
			Role:         v1.ChannelRoleBoth,
			AuthzSubject: "agentsession:" + parent.Namespace + "/" + parent.Name,
			AgentClass:   sr.Spec.Class,
			SessionScope: agent.Kind{}.DefaultSessionScope(),
			// No credentials: the counterparty is in-cluster, so this kind
			// declares no required Secret keys and the Channel controller skips
			// the Secret checks entirely on an empty secretName.
		},
	}
}

// rollback removes a delegation's objects: the child session always, and its
// Channel whenever one exists for this request. Both deletes are attempted
// even when the first fails, so one failure cannot hide the other, and every
// failure is reported to the caller rather than logged and swallowed -- the
// caller turns a non-nil return into a retry.
//
// Two callers, one job. The create path undoes a PARTIALLY-APPLIED pass --
// including from the branch where the Channel's own Create is what failed,
// because a Create whose write landed and whose response was lost returns an
// error while the object exists. reconcileParentExchange ends a delegation
// whose parent stopped answering, where both objects were fully created and
// are simply no longer wanted.
//
// A NotFound is success: the object is gone, which is the state this is trying
// to reach. That is what makes naming an object that may never have been
// created free.
func (r *Reconciler) rollback(ctx context.Context, child *v1.AgentSession, channel *v1.Channel) error {
	var failures []string
	if child != nil {
		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Sprintf("child session %s rollback failed: %v", child.Name, err))
		}
	}
	if channel != nil {
		if err := r.Delete(ctx, channel); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Sprintf("child channel %s rollback failed: %v", channel.Name, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// deny records a terminal refusal. It returns a nil error deliberately: a
// policy refusal is a RESULT, not a controller failure, and requeueing it
// would spin forever.
func (r *Reconciler) deny(ctx context.Context, sr *v1.SubagentRequest, reason, msg string) (ctrl.Result, error) {
	return r.resolve(ctx, sr, v1.SubagentRequestPhaseDenied, reason, msg)
}

// resolve is the ONE place a request reaches a terminal phase. Every terminal
// write goes through it — deny, fail, and the Succeeded arm of reconcileChild —
// so the completion clock the reclaim pass measures against cannot be forgotten
// by a fourth one added later. It is the same reasoning that keeps the mode
// ceiling in one place: a rule enforced at three call sites is a rule with
// three chances to be missed.
//
// It is also the ONE place an `attended` request's watching parent is told
// its child just ended, for the same reason: gating on
// sr.Status.ChildRef != nil (rather than on which of deny/fail/resolve's
// three call sites reached here) is what makes the notification correct
// automatically — every deny() before a child exists (off-roster, tree
// ceiling, ReasonAttendedChildInProgress, ReasonAttendedRootNotChannelAttached)
// leaves ChildRef nil and is silently skipped, while every fail() reached
// AFTER a child exists (ChildVanished, ChildFailed, ParentUnanswered) is
// covered without having to name each one here.
//
// The returned RequeueAfter is what schedules the reclaim: nothing else would
// revisit a resolved request, since the only Watch is on AgentSession and a
// finished child stops producing events. On an operator restart the informer's
// initial sync re-reconciles every request and this same path re-schedules from
// the stored clock, so a restart costs at most the remainder of one retention
// window, never the reclaim itself.
func (r *Reconciler) resolve(
	ctx context.Context, sr *v1.SubagentRequest, phase, reason, msg string,
) (ctrl.Result, error) {
	// The attended parent is told BEFORE the terminal write, because this is
	// the only pass that ever tells it: once the phase is terminal, every
	// later reconcile takes reclaimTerminal and never reaches here again.
	// Ordering the notify first trades the failure mode in the direction
	// finalizeAttended's own doc argues for: a crash between the notice and
	// the persist costs a duplicate line, not a silently missed one. The
	// resolution itself never waits on the notice — it lands whatever the
	// parent's loop is doing — so a failure is reported and the request
	// resolves.
	if sr.EffectiveMode() == v1.SubagentModeAttended && sr.Status.ChildRef != nil {
		if err := r.notifyAttendedParent(ctx, sr, attendedLineFor(phase)); err != nil {
			log.FromContext(ctx).Info("attended completion: "+attendedNotifyOutcome(err)+"; the request resolves anyway",
				"request", sr.Namespace+"/"+sr.Name,
				"parent", sr.Spec.Parent.Namespace+"/"+sr.Spec.Parent.Name, "phase", phase, "err", err.Error())
		}
	}
	sr.Status.Phase = phase
	sr.Status.FailureReason = reason
	sr.Status.Determination = msg
	now := metav1.NewTime(r.clock())
	sr.Status.CompletionTime = &now
	if err := r.Status().Update(ctx, sr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.terminalRetention()}, nil
}

// fail records a terminal, RETRYABLE failure — deliberately distinct from
// deny. A denial is a policy refusal that the delegate meta-tool presents to
// the parent's model as non-retryable (the parent must not re-attempt the
// work elsewhere); a failure is not — a crashed or vanished child is not a
// policy decision about the work, and conflating the two would let a
// transient crash permanently block a legitimate retry. Nothing here binds
// or enforces the parent's non-retry; that instruction is prose in the
// tool's result text (pkg/agent/tool/meta/delegate.go), not a controller
// guarantee. Also returns a nil error for the same reason deny does: this is
// a resolved RESULT, not a controller error to requeue.
func (r *Reconciler) fail(ctx context.Context, sr *v1.SubagentRequest, reason, msg string) (ctrl.Result, error) {
	return r.resolve(ctx, sr, v1.SubagentRequestPhaseFailed, reason, msg)
}

// reconcileChild resolves a request whose child session has already been
// created, by reading that child's own status. This is the
// completion-propagation half of the controller: it is the only place
// anything in the tree writes SubagentRequestPhaseSucceeded, and the only
// place anything writes SubagentRequestPhaseAwaitingParent.
//
//   - Child Succeeded -> request Succeeded, result copied (bounded).
//   - Child Failed -> request Failed via fail(), never deny(): a crashed
//     child is retryable.
//   - Child not found -> request Failed via fail(), never deny(): a
//     vanished child is not a policy refusal either.
//   - Anything else (Running, or any other non-terminal phase) -> hand off to
//     reconcileParentExchange, which decides between AwaitingParent and
//     Running from the child's own status.parentExchange. Resolving here
//     would hand the parent an empty result as though the child had actually
//     finished.
func (r *Reconciler) reconcileChild(ctx context.Context, sr *v1.SubagentRequest) (ctrl.Result, error) {
	var child v1.AgentSession
	err := r.Get(ctx, types.NamespacedName{Namespace: sr.Status.ChildRef.Namespace, Name: sr.Status.ChildRef.Name}, &child)
	if apierrors.IsNotFound(err) {
		return r.fail(ctx, sr, "ChildVanished",
			fmt.Sprintf("child session %s/%s no longer exists", sr.Status.ChildRef.Namespace, sr.Status.ChildRef.Name))
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get child session: %w", err)
	}

	switch child.Status.Phase {
	case v1.AgentSessionPhaseSucceeded:
		determination := fmt.Sprintf("child %s succeeded", child.Name)
		if child.Status.Result != nil {
			sr.Status.Result = truncateResult(child.Status.Result.Summary)
			artifacts, dropped := copyReturnedArtifacts(child.Status.Result.Artifacts)
			sr.Status.Artifacts = artifacts
			if dropped > 0 {
				// Surfaced AND logged, never silently cut: the parent is about
				// to be shown the handles that did survive and has no way to
				// tell a child that returned three from one that returned
				// thirteen.
				determination = fmt.Sprintf("%s (returned %d artifacts; the first %d were carried, %d dropped)",
					determination, len(child.Status.Result.Artifacts), len(artifacts), dropped)
				log.FromContext(ctx).Info("subagentrequest: dropping artifact handles a child returned beyond the copy bound",
					"namespace", sr.Namespace, "request", sr.Name, "child", child.Name,
					"returned", len(child.Status.Result.Artifacts), "carried", len(artifacts), "dropped", dropped)
			}
		}
		// Through resolve() like every other terminal write, so the result is
		// copied and the completion clock stamped in ONE status update: a
		// success recorded without the clock would be a delegation nothing ever
		// reclaims. No FailureReason — this is the one terminal phase that is
		// not a refusal.
		return r.resolve(ctx, sr, v1.SubagentRequestPhaseSucceeded, "", determination)
	case v1.AgentSessionPhaseFailed:
		reason := child.Status.FailureReason
		msg := fmt.Sprintf("child %s failed", child.Name)
		if reason != "" {
			msg = fmt.Sprintf("%s: %s", msg, reason)
		}
		return r.fail(ctx, sr, "ChildFailed", msg)
	default:
		// Running, or parked in an intermediate phase (Idle, one of the
		// Awaiting* states) — not yet resolved. Whether the child is WORKING
		// or waiting on its parent is not a phase question: it parks at
		// Running while its pod lives and at Idle once the pod is reaped, and
		// both are ordinary states for a session that is simply busy. Only
		// status.parentExchange distinguishes them.
		// The child's ask for DATA rides the same pass but is a separate
		// axis: it does not park the child and does not change the phase, so
		// it must not be folded into the exchange state machine below.
		// Mirrored FIRST so a request and a question raised in the same turn
		// both land, rather than the phase-changing one winning.
		if err := r.mirrorInputRequest(ctx, sr, &child); err != nil {
			return ctrl.Result{}, err
		}
		// Slots the parent offered AFTER the handoff (send_input). Graded and
		// bound against the LIVE child, which is simpler than the create path
		// rather than harder: the child already exists, so there is nothing to
		// roll back — a slot arriving later is the normal case here, not a
		// half-built delegation.
		if done, err := r.bindOfferedDataSlots(ctx, sr, &child); err != nil || !done {
			return ctrl.Result{RequeueAfter: disclosurePollInterval}, err
		}
		return r.reconcileParentExchange(ctx, sr, &child)
	}
}

// mirrorInputRequest copies a live child's outstanding data request onto the
// request its parent polls.
//
// The parent's runner has no permission on the child's AgentSession, so
// without this the ask is written somewhere nobody who could act on it can
// read — the same reason parentExchange is mirrored rather than read through.
//
// No phase change. Asking for data explicitly does not park the child, so a
// request must not move the request out of Running; a delegation that stopped
// because its child asked a question it was told would not block would be the
// hang the tool's contract rules out.
//
// Unlike parentExchange, a single_turn child's request is NOT ignored. That
// mode's restriction is on CONVERSATION — the child must not be able to
// promote its delegation into one the roster never granted — and a data
// request grants nothing: the parent may only bind a tag it can itself read,
// and a disclosing one still routes to a human. Refusing the ask here would
// withhold a capability the mode never restricted.
func (r *Reconciler) mirrorInputRequest(ctx context.Context, sr *v1.SubagentRequest, child *v1.AgentSession) error {
	ir := child.Status.InputRequest
	switch {
	case ir == nil || !ir.Pending:
		if sr.Status.InputRequest == nil {
			return nil
		}
		// The ask was answered or withdrawn; stop showing it as outstanding.
		sr.Status.InputRequest = nil
	case sr.Status.InputRequest != nil && sr.Status.InputRequest.Exchange == ir.Exchange:
		return nil // already mirrored; comparing the NUMBER, not the text
	default:
		mirrored := *ir
		sr.Status.InputRequest = &mirrored
		log.FromContext(ctx).Info("subagentrequest: child asked for data",
			"request", sr.Namespace+"/"+sr.Name, "child", child.Name,
			"slot", ir.Slot, "exchange", ir.Exchange)
	}
	return r.Status().Update(ctx, sr)
}

// reconcileParentExchange mirrors a live child's conversation state onto the
// request, and enforces the two ceilings that keep a resumable delegation from
// becoming an abandoned or an unbounded one: the parent-reply bound, and the
// exchange budget spec.mode declares.
//
// Four dispositions, from the child's own status.parentExchange:
//
//   - A NEW question is outstanding and the budget allows it -> AwaitingParent,
//     with the child's words and the exchange number copied across, the
//     parent's clock started, and one exchange spent. The parent's delegate /
//     reply_to_subagent call is polling for exactly this.
//   - A NEW question is outstanding and the budget is spent -> refused
//     (refuseExchange), the delegation left running.
//   - The same question is still outstanding -> nothing to write; either
//     requeue for the remainder of the parent's time, or, once that is spent,
//     end the delegation (see below).
//   - Nothing is outstanding -> back to Running if the request was awaiting.
//     The child answered and resumed; leaving the phase behind would keep a
//     working child measured against a bound meant for a parked one.
//
// This is the ONE place spec.mode is read for the budget — through
// SubagentRequest.ExchangeBudget, resolved here where an exchange is honoured
// and recorded on status.exchangesRemaining. Encoding the same decision as a
// provisioning difference would put one attack-surface answer in two places.
//
// The bound is the answer to "a parent that never replies must not park the
// child forever", and it has to live HERE rather than in either party: the
// parent's own poll ceiling bounds only the parent's tool call (a parent that
// gives up, crashes, or is never woken again leaves the child exactly as
// parked as before), and the child cannot bound itself — its whole design is
// to survive its pod being reaped, and parked time deliberately does not burn
// its run-duration budget. The operator is the only party that outlives both.
//
// It is measured PER EXCHANGE, from when each question was first observed,
// not across the conversation: a chat delegation whose every question is
// answered promptly is working exactly as intended and must not be killed for
// having lasted a while. Active work stays bounded by the child's own
// maxDuration budget, and fan-out by the tree's agent ceiling — this bounds
// only the third thing, silence.
func (r *Reconciler) reconcileParentExchange(ctx context.Context, sr *v1.SubagentRequest, child *v1.AgentSession) (ctrl.Result, error) {
	pe := child.Status.ParentExchange
	// A single_turn delegation never becomes a conversation, whatever its
	// child's status says. The child writes status.parentExchange itself (its
	// runner Role grants patch on its own agentsessions/status), so honouring
	// it unconditionally would let a headless child promote its own delegation
	// to one the parent's roster never granted — the mode is an ATTACK-SURFACE
	// declaration, and the party it constrains must not be able to widen it.
	//
	// Ignored LOUDLY: a headless child is offered no ask_parent
	// (subagent_conversation is inactive without a session-counterparty
	// binding), so a value here is a defect or an attempt, and neither should
	// be silent.
	if pe != nil && sr.EffectiveMode() == v1.SubagentModeSingleTurn {
		log.FromContext(ctx).Info("subagentrequest: ignoring a single_turn child's parentExchange; the mode grants no conversation",
			"request", sr.Namespace+"/"+sr.Name, "child", child.Name, "exchange", pe.Exchange, "pending", pe.Pending)
		pe = nil
	}
	if pe == nil || !pe.Pending {
		if sr.Status.Phase != v1.SubagentRequestPhaseAwaitingParent {
			return ctrl.Result{}, nil // nothing observed, nothing to correct
		}
		sr.Status.Phase = v1.SubagentRequestPhaseRunning
		sr.Status.AwaitingParentSince = nil
		sr.Status.Determination = fmt.Sprintf("child %s resumed after its question was answered", child.Name)
		// Status.Message and Status.Exchange are deliberately KEPT: the
		// message is the record of what was last asked, and the exchange
		// number is what a parent's next reply_to_subagent call waits to see
		// advance. Clearing either would make the request forget the
		// conversation it is in the middle of.
		return ctrl.Result{}, r.Status().Update(ctx, sr)
	}

	// A question is outstanding. Record it if this is the first time this
	// exchange has been seen — comparing the NUMBER, not the text, so a child
	// that asks the same thing twice is still two exchanges and gets a fresh
	// clock for the second.
	if sr.Status.Phase != v1.SubagentRequestPhaseAwaitingParent || sr.Status.Exchange != pe.Exchange {
		// The exchange budget the mode declares. Measured against
		// ExchangesRemaining — controller-owned — and never against pe.Exchange
		// or its mirror: the child authors that number, so a budget keyed on it
		// is widened by restating it, exactly the way a single_turn child would
		// promote itself above.
		if rem := sr.Status.ExchangesRemaining; rem != nil && *rem <= 0 {
			return r.refuseExchange(ctx, sr, child, pe)
		}
		now := metav1.NewTime(r.clock())
		sr.Status.Phase = v1.SubagentRequestPhaseAwaitingParent
		sr.Status.Exchange = pe.Exchange
		sr.Status.ExchangesRemaining = spendOneExchange(sr)
		// Bounded exactly as a child's final result is, by the same helper and
		// for the same reason: status is not a transport for arbitrary
		// payloads, and a cut value must say it was cut rather than read as a
		// question that merely stops mid-sentence.
		sr.Status.Message = truncateResult(pe.Question)
		sr.Status.AwaitingParentSince = &now
		sr.Status.Determination = fmt.Sprintf("child %s asked its parent a question (exchange %d) and is waiting", child.Name, pe.Exchange)
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("record awaiting-parent: %w", err)
		}
		return ctrl.Result{RequeueAfter: r.maxAwaitingParent()}, nil
	}

	// Already recorded. AwaitingParentSince is written in the same update that
	// sets the phase, so a missing one means a status written by something
	// other than the branch above; restamp rather than treat "no clock" as
	// "no bound", which would park the child forever.
	if sr.Status.AwaitingParentSince == nil {
		now := metav1.NewTime(r.clock())
		sr.Status.AwaitingParentSince = &now
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("restamp awaiting-parent clock: %w", err)
		}
		return ctrl.Result{RequeueAfter: r.maxAwaitingParent()}, nil
	}
	bound := r.maxAwaitingParent()
	waited := r.clock().Sub(sr.Status.AwaitingParentSince.Time)
	if waited < bound {
		return ctrl.Result{RequeueAfter: bound - waited}, nil
	}

	// Out of time. Tear the delegation down rather than leaving a child parked
	// on an answer that is not coming: it holds a session, a Channel and a
	// share of the tree's agent ceiling, and nothing else would ever reclaim
	// them — the child is waiting by design, so no watchdog, budget or reaper
	// treats it as stuck, and reclaimTerminal only ever looks at a request that
	// has already RESOLVED, which is what this branch makes happen.
	//
	// Teardown BEFORE the status write, so a failed delete leaves the request
	// AwaitingParent and this same branch retries it. Marking the request
	// resolved first would strand whatever the delete failed to remove.
	if err := r.rollback(ctx, child, agentChannelStub(sr)); err != nil {
		return ctrl.Result{}, fmt.Errorf("end an unanswered delegation: %w", err)
	}
	return r.fail(ctx, sr, "ParentUnanswered",
		fmt.Sprintf("child %s asked a question %s ago and %s never answered; the delegation was ended and the child stopped",
			child.Name, bound, sr.Spec.Parent.Name))
}

// spendOneExchange returns the request's exchange budget with the exchange
// being honoured right now deducted, or nil when the mode bounds none.
//
// SubagentRequest.ExchangeBudget is read here and in refuseExchange's message,
// and nowhere else in the tree — this honour/refuse path is the one place
// spec.mode decides how many questions a delegation carries. The count is
// seeded from the mode on the FIRST honoured exchange and decremented from its
// own stored value thereafter, so the stored value IS the count — no second
// field mirrors it, and nothing the child writes takes part.
//
// It floors at zero rather than going negative: the caller refuses at <= 0, so
// a negative would say the same thing less clearly to whoever reads the status.
func spendOneExchange(sr *v1.SubagentRequest) *int64 {
	limit, bounded := sr.ExchangeBudget()
	if !bounded {
		return nil
	}
	next := limit - 1
	if rem := sr.Status.ExchangesRemaining; rem != nil {
		next = *rem - 1
	}
	if next < 0 {
		next = 0
	}
	return new(next)
}

// refuseExchange declines to carry a question the delegation's mode has no
// budget left for, WITHOUT ending the delegation.
//
// That is deliberately neither of the two obvious answers. Refusing silently
// would park the child on an answer that never comes; tearing the delegation
// down would throw away everything the child has already done. The delegation
// stays Running, the child stays alive, and the refusal is legible — the child
// is normally told before it ever gets here, because its own ask_parent reads
// status.exchangesRemaining first and returns the refusal as a tool result in
// the same turn. This branch is the ENFORCING half: a runner that skipped that
// check, or wrote status.parentExchange directly, still gets no exchange.
//
// The child's Pending flag is cleared as part of the refusal, and what that
// protects is the RE-REFUSE loop, not the child. Status.Exchange is left at the
// last HONOURED exchange, so a still-pending flag would fail the
// `sr.Status.Exchange != pe.Exchange` test on every pass — refusing, logging
// and writing status forever — and the request would go on advertising a wait
// that nothing will ever satisfy.
//
// It does NOT leave the child bounded, and it cannot. If this arm is reached at
// all the runner-side guard was bypassed (ask_parent reads
// status.exchangesRemaining first and refuses in the same turn), so the child
// really is parked in ask_parent's wait — and after this write there is no
// parent-reply clock on it: the AwaitingParentSince bound only runs while the
// request is AwaitingParent, and the very next reconcile would drop it anyway,
// because pending=false takes the "nothing outstanding" arm above, which sets
// Running and nils AwaitingParentSince. Keeping the clock HERE would survive
// exactly one pass. What does bound the child is its own idle TTL: the
// ask_parent park exits to phase=Idle and its pod is reaped, and the delegation
// ends when the parent's own per-call poll ceiling expires.
//
// Status.Message and Status.Exchange are left alone — they are the record of
// what was last HONOURED, and a refused question was never carried to the
// parent.
func (r *Reconciler) refuseExchange(
	ctx context.Context, sr *v1.SubagentRequest, child *v1.AgentSession, pe *v1.ParentExchange,
) (ctrl.Result, error) {
	log.FromContext(ctx).Info("subagentrequest: refusing an exchange; the delegation's mode has no clarification budget left",
		"request", sr.Namespace+"/"+sr.Name, "child", child.Name, "mode", sr.EffectiveMode(),
		"exchange", pe.Exchange)

	// The child first, then the request. The other order would mark the
	// exchange refused while the child still advertises it as pending, so a
	// failed clear would leave a question nothing ever revisits.
	child.Status.ParentExchange.Pending = false
	if err := r.Status().Update(ctx, child); err != nil {
		return ctrl.Result{}, fmt.Errorf("clear a refused exchange on child %s: %w", child.Name, err)
	}

	// The LIMIT, not what is left — which is zero by construction here.
	// Determination is handed verbatim to the parent's model by the delegate
	// tool, and "carries 0 questions" would describe the wrong delegation.
	limit, _ := sr.ExchangeBudget()
	sr.Status.Phase = v1.SubagentRequestPhaseRunning
	sr.Status.AwaitingParentSince = nil
	sr.Status.Determination = fmt.Sprintf(
		"child %s asked its parent a question (exchange %d), which was refused: a %q delegation carries %d, and it has spent them. The child continues with what it already has.",
		child.Name, pe.Exchange, sr.EffectiveMode(), limit)
	return ctrl.Result{}, r.Status().Update(ctx, sr)
}

// DefaultMaxAwaitingParent bounds how long one unanswered question may park a
// delegated child before the controller ends the delegation.
//
// Sized generously on purpose: answering can legitimately take the parent a
// while — it may have to finish its own turn, ask its human, or wait out a
// tool call of its own — and killing a child that was about to be answered
// wastes everything it has done. The failure this exists to stop is a parent
// that never answers AT ALL, which no shorter value detects any better.
//
// It is independent of the delegate tool's own poll ceiling and deliberately
// not derived from it: they bound different parties (that one bounds the
// parent's tool call, this one bounds the child's park) and only the operator
// can enforce this one, since a parent that has already given up is by
// definition not running any code that could.
const DefaultMaxAwaitingParent = 30 * time.Minute

// maxAwaitingParent is the configured bound, or the built-in default when
// nothing set one. A zero MaxAwaitingParent must never read as "no bound":
// that is the un-wired reconciler (a test, a binary that forgot the field),
// and reading it as unlimited would silently restore the exact
// parked-forever behaviour the bound exists to remove.
func (r *Reconciler) maxAwaitingParent() time.Duration {
	if r.MaxAwaitingParent > 0 {
		return r.MaxAwaitingParent
	}
	return DefaultMaxAwaitingParent
}

// DefaultTerminalRetention is how long a RESOLVED SubagentRequest is kept
// before the reclaim pass deletes it and, through the owner-reference cascade,
// its child session and the `agent` Channel that was their conversation.
//
// It exists because the cascade only runs the other way. The request is
// owner-ref'd to the PARENT session, so a whole lineage is reclaimed when its
// parent is deleted — but a LIVE parent's finished delegations are reclaimed by
// nothing. That is precisely the shape `chat` mode exists for: a long-lived
// conversational parent accrues one request, one Channel and one terminal child
// per delegation for its entire lifetime, and every one of those Channels stays
// in its class's status.boundChannels, which the AgentClass reconciler walks
// with a Get per entry on every pass.
//
// NOT zero, deliberately. Reclaiming at the moment of completion would delete
// the record of what was delegated and what came back while somebody may still
// be reading it — `kubectl get subagentrequests` after a run is the ordinary
// way a human sees what an agent farmed out, and it is the only place
// status.result is legible without opening the parent's transcript. An hour is
// long enough for that and short enough to bound the accumulation to what one
// parent produces in an hour.
//
// What it does NOT discard is the audit trail. The child's transcript and its
// signed audit entries are append-only memory, and the AgentSession finalizer
// deletes none of a session's memory (pkg/controllers/agentsession's
// ScopeDeleter doc) — so reclaiming the CRs reclaims the Kubernetes objects,
// never the record of what the agents did.
const DefaultTerminalRetention = time.Hour

// terminalRetention is the configured retention, or the built-in default when
// nothing set one. Zero reads as "unset", never as "reclaim immediately" — the
// same fail-safe reading maxAwaitingParent gives its own zero, and for a
// sharper reason: a zero read as "now" would make an un-wired reconciler delete
// every delegation the instant it finished.
func (r *Reconciler) terminalRetention() time.Duration {
	if r.TerminalRetention > 0 {
		return r.TerminalRetention
	}
	return DefaultTerminalRetention
}

// reclaimTerminal is the reconcile arm for an already-resolved request: it
// decides whether the delegation has been kept long enough to delete.
//
// Deleting the request is the whole mechanism — the child session and the
// per-request `agent` Channel are both owner-ref'd to it (buildChild,
// buildAgentChannel), so Kubernetes garbage collection reclaims them without
// this controller naming either one. A terminal request always implies a
// terminal child: the Succeeded and ChildFailed arms are reached FROM the
// child's own phase, a Denied request never created one, and the
// parent-unanswered timeout tears the child down before it resolves.
func (r *Reconciler) reclaimTerminal(ctx context.Context, sr *v1.SubagentRequest) (ctrl.Result, error) {
	retention := r.terminalRetention()

	// A request resolved before status.completionTime existed carries no
	// clock. Start one now rather than reading "no clock" as "expired": an
	// upgrade must not sweep away every historical delegation in the cluster
	// on the operator's first reconcile pass.
	if sr.Status.CompletionTime == nil {
		now := metav1.NewTime(r.clock())
		sr.Status.CompletionTime = &now
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("start the retention clock on an already-resolved delegation: %w", err)
		}
		return ctrl.Result{RequeueAfter: retention}, nil
	}

	if kept := r.clock().Sub(sr.Status.CompletionTime.Time); kept < retention {
		return ctrl.Result{RequeueAfter: retention - kept}, nil
	}

	// NotFound is success: something else already reclaimed it (the parent was
	// deleted, a human ran kubectl delete). Any other failure is returned so
	// the request is retried — dropping it would leave the delegation with no
	// scheduled reconcile and therefore no second chance at reclamation.
	if err := r.Delete(ctx, sr); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("reclaim resolved delegation %s/%s: %w", sr.Namespace, sr.Name, err)
	}
	log.FromContext(ctx).Info("reclaimed a resolved delegation and its child",
		"request", sr.Namespace+"/"+sr.Name, "phase", sr.Status.Phase,
		"child", childRefText(sr), "keptFor", retention.String())
	return ctrl.Result{}, nil
}

// childRefText renders the reclaimed child for the log line, or "none" for a
// request that never got one (every Denied request, and any that failed before
// the create).
func childRefText(sr *v1.SubagentRequest) string {
	if sr.Status.ChildRef == nil {
		return "none"
	}
	return sr.Status.ChildRef.Namespace + "/" + sr.Status.ChildRef.Name
}

// clock is the reconciler's time source, injectable so the parent-reply bound
// is testable without sleeping. Nil (production) is time.Now.
func (r *Reconciler) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// agentChannelStub names the `agent` Channel of a conversational delegation,
// as the bare object reference a Delete needs — nil for a mode that has none.
// Only the namespace and name are populated: rollback deletes by reference
// and treats a NotFound as success, so re-deriving the full spec would be
// work whose result is discarded.
func agentChannelStub(sr *v1.SubagentRequest) *v1.Channel {
	name := agentChannelNameFor(sr)
	if name == "" {
		return nil
	}
	return &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: sr.Namespace, Name: name}}
}

// truncateResult bounds a child's own text to maxCopiedResultLen before it
// lands on SubagentRequestStatus -- Result for the final answer,  Message for
// an outstanding question. See the consts' docs for why the bound exists and
// why a cut value carries truncationMarker.
//
// The marker's length is reserved from the budget FIRST, so the returned
// value (payload + marker) never exceeds maxCopiedResultLen -- a marker that
// pushed the total over the bound would defeat the point of having one.
//
// The payload is cut with strings.ToValidUTF8 rather than a bare byte slice:
// byte-slicing alone can land mid-codepoint on a multi-byte UTF-8 rune,
// which would otherwise round-trip through JSON as a stray U+FFFD.
// ToValidUTF8 drops that dangling partial rune instead of leaving it
// (library over hand-rolled rune-boundary scanning, per AGENTS.md).
func truncateResult(s string) string { return truncateTo(s, maxCopiedResultLen) }

// truncateTo is the bound itself, taking its limit as an argument so a child's
// answer and a child's per-artifact description are cut by ONE implementation
// carrying ONE marker. Two copies of this would drift, and the copy that
// drifted would be the one on the shorter field nobody reads closely.
func truncateTo(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	marker := fmt.Sprintf(truncationMarker, maxLen)
	budget := maxLen - len(marker)
	if budget < 0 {
		budget = 0
	}
	return strings.ToValidUTF8(s[:budget], "") + marker
}

// copyReturnedArtifacts bounds the artifact handles a child returned before
// they land on SubagentRequestStatus.Artifacts, and reports how many entries
// were dropped so the caller can say so rather than cut in silence.
//
// Three separate bounds, because they fail differently:
//
//   - The COUNT is capped at maxCopiedArtifacts and the excess dropped, since
//     a partial list of usable handles is worth more to a parent than none.
//   - A DESCRIPTION over maxCopiedArtifactDescLen is truncated with the same
//     marker a cut Result carries. Cutting it costs the reader some prose and
//     nothing else, so the entry is kept.
//   - An ID over maxCopiedArtifactIDLen — longer than any object name the
//     apiserver will accept — is DROPPED, not truncated. A handle's only use
//     is to name an ArtifactRender the parent then Gets, and a truncated one
//     is a different handle: it would name some other object, or none, while
//     reading to the parent as the one the child returned.
//
// An entry with an empty ID is dropped too: it names nothing the parent could
// ever attach, and carrying it would put a handle in front of the parent's
// model that can only fail.
func copyReturnedArtifacts(in []v1.ResultArtifact) (out []v1.ResultArtifact, dropped int) {
	for _, a := range in {
		if len(out) >= maxCopiedArtifacts || a.ID == "" || len(a.ID) > maxCopiedArtifactIDLen {
			dropped++
			continue
		}
		out = append(out, v1.ResultArtifact{
			ID:          a.ID,
			Description: truncateTo(a.Description, maxCopiedArtifactDescLen),
		})
	}
	return out, dropped
}

// admitCrossNamespaceParent decides whether a parent in a namespace other
// than sr.Namespace may still be admitted — the one relaxation of step 0's
// same-namespace rule (spec §2.6). It is deliberately narrow: sr.Namespace
// must be a workshop namespace truly provisioned for exactly the CLAIMED
// parent, and that parent must currently hold the workshop's SpiceDB
// workshop:<W>#build tuple.
//
// The proof of "provisioned for exactly this parent" is the NAMESPACE
// object's own labels (LabelWorkshopSessionNamespace/LabelWorkshopSessionName,
// stamped once by the Workshop controller at provisioning) — never
// sr.Spec.Parent alone, which is a field on the very object under
// reconciliation and so is exactly as trustworthy as whatever created it.
// Reading the labels first and consulting SpiceDB only once they match is
// also what lets a caller-side spy distinguish "never a workshop attempt"
// (checker not consulted) from "a workshop attempt that failed the tuple
// check" (checker consulted, admitted false).
//
// Every failure mode returns admitted=false: an unreadable namespace, a
// namespace whose labels don't match, a nil WorkshopBuild checker
// (CLAUDE.md's typed-nil rule — an unwired dependency is a true nil
// interface, checked explicitly here rather than a panic controller-runtime's
// recovery would hide), a check error, and an explicit false all deny. reason
// and msg are set (non-empty) only for the two failure modes that occurred
// AFTER the namespace was confirmed to genuinely name this workshop for this
// parent — an unwired checker, a check error, or an explicit false — so the
// caller can tell "not a workshop request at all" (generic
// ParentCrossNamespace, reason=="") from "a workshop request whose standing
// did not hold" (ReasonWorkshopBuildDenied).
func (r *Reconciler) admitCrossNamespaceParent(ctx context.Context, sr *v1.SubagentRequest) (admitted bool, reason, msg string) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: sr.Namespace}, &ns); err != nil {
		// An unreadable namespace (NotFound, or any other error) is treated the
		// same as "not a workshop": sr.Namespace is the SubagentRequest's own
		// containing namespace, which necessarily exists for this object to be
		// reconciled at all, so a Get failure here is not something a retry can
		// resolve into a different answer about whether THIS is a workshop
		// delegation — deny now rather than requeue a check that will keep
		// failing the same way.
		return false, "", ""
	}
	if ns.Labels[v1.LabelWorkshopSessionNamespace] != sr.Spec.Parent.Namespace ||
		ns.Labels[v1.LabelWorkshopSessionName] != sr.Spec.Parent.Name {
		// sr.Namespace is not a workshop provisioned for the CLAIMED parent —
		// either it is not a workshop at all (no labels), or it is a workshop
		// provisioned for some OTHER session. Either way this was never a
		// workshop delegation attempt for this parent, so the checker is not
		// consulted at all and the caller falls back to the generic refusal.
		return false, "", ""
	}
	if r.WorkshopBuild == nil {
		return false, ReasonWorkshopBuildDenied,
			"no WorkshopBuildChecker is wired -- refusing a cross-namespace delegation with no authorization backend to check it against"
	}
	allowed, err := r.WorkshopBuild.CheckWorkshopBuild(ctx, sr.Namespace, sr.Spec.Parent.Namespace, sr.Spec.Parent.Name)
	if err != nil {
		return false, ReasonWorkshopBuildDenied,
			fmt.Sprintf("checking workshop:%s#build for session %s/%s: %v", sr.Namespace, sr.Spec.Parent.Namespace, sr.Spec.Parent.Name, err)
	}
	if !allowed {
		return false, ReasonWorkshopBuildDenied,
			fmt.Sprintf("session %s/%s does not hold workshop:%s#build; cross-namespace delegation is not permitted",
				sr.Spec.Parent.Namespace, sr.Spec.Parent.Name, sr.Namespace)
	}
	return true, "", ""
}

// loadParent loads the delegating session and its AgentClass. Either can have
// been deleted between the parent starting and this reconcile running, and
// the two are told apart by which return is nil:
//
//   - parent == nil: the session itself is gone. deny(ParentMissing).
//   - parent != nil, parentClass == nil: the session is still there, but its
//     AgentClass was deleted out from under it. deny(ParentClassMissing).
//
// Without the second case, a deleted AgentClass wedged the request forever:
// the class Get's NotFound used to be wrapped and returned like any other
// error, controller-runtime requeued with exponential backoff, and a deleted
// AgentClass never comes back on its own — the request sat in Pending
// indefinitely instead of ever resolving. A real
// infrastructure error (anything but NotFound) still returns as an error, so
// it gets retried rather than denied.
func (r *Reconciler) loadParent(ctx context.Context, sr *v1.SubagentRequest) (*v1.AgentSession, *v1.AgentClass, error) {
	var parent v1.AgentSession
	err := r.Get(ctx, types.NamespacedName{Namespace: sr.Spec.Parent.Namespace, Name: sr.Spec.Parent.Name}, &parent)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get parent session: %w", err)
	}
	var pc v1.AgentClass
	err = r.Get(ctx, types.NamespacedName{Namespace: parent.Namespace, Name: parent.Spec.Class}, &pc)
	if apierrors.IsNotFound(err) {
		return &parent, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get parent class: %w", err)
	}
	return &parent, &pc, nil
}

func onRoster(pc *v1.AgentClass, class string) bool {
	for _, n := range pc.Spec.RosterNames() {
		if n == class {
			return true
		}
	}
	return false
}

// childClassInWorkshop reports whether sr.Spec.Class resolves to an AgentClass in
// sr's own namespace (the workshop namespace W for a workshop cross-namespace
// request). It is the roster-membership proof for a workshop child (R1): the class
// exists in the sandbox the workshop#build tuple licensed, and every class there
// arrived through workshop_apply. A NotFound is a definitive "no" (still OffRoster);
// any other error propagates so the reconcile retries rather than admitting on a
// read failure.
func (r *Reconciler) childClassInWorkshop(ctx context.Context, sr *v1.SubagentRequest) (bool, error) {
	var ac v1.AgentClass
	err := r.Get(ctx, types.NamespacedName{Namespace: sr.Namespace, Name: sr.Spec.Class}, &ac)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get workshop child class %s/%s: %w", sr.Namespace, sr.Spec.Class, err)
	}
	return true, nil
}

// isTerminalAgentSessionPhase reports whether an AgentSession has reached a
// resolved phase. Mirrors reconcileChild's own switch (Succeeded/Failed
// resolve the request; everything else -- Running, Idle, any Awaiting* park
// -- is still "in progress"), restated here as a predicate because the
// attended-child-in-progress check needs the same answer about a session it
// is not itself reconciling.
func isTerminalAgentSessionPhase(phase string) bool {
	return phase == v1.AgentSessionPhaseSucceeded || phase == v1.AgentSessionPhaseFailed
}

// startedByAnnotations copies the parent's starter identity onto the child.
// The three annotations are a MATCHED SET — the canonical id is stored
// prefixed ("user:<canonical>") while the others are raw — so they are
// copied together or not at all. A cron/service parent has none, and a
// child of one correctly gets none either: agentsession#started_by is
// user-only in the schema.
func startedByAnnotations(parent *v1.AgentSession) map[string]string {
	out := map[string]string{}
	for _, k := range []string{
		v1.AnnotationStartedByCanonicalID,
		v1.AnnotationStartedByExternalID,
		v1.AnnotationStartedByEmail,
	} {
		if v := parent.Annotations[k]; v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.SubagentRequest{}).
		Watches(&v1.AgentSession{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToRequest)).
		Complete(r)
}

// mapChildToRequest re-enqueues the SubagentRequest that owns the changed
// AgentSession, when that session is a delegated child. Without this watch,
// nothing ever re-reconciles a request after its child finishes: the
// request would sit in Running until some unrelated event happened to
// requeue it, and the runner's delegate tool would poll until its own
// timeout instead of observing completion promptly.
//
// The child carries a controller ownerRef to its SubagentRequest (buildChild,
// via metav1.NewControllerRef), so the mapping is a metadata read — no List
// required. A session with no such ownerRef (not a delegated child at all,
// or owned by something else) maps to nothing.
func (r *Reconciler) mapChildToRequest(_ context.Context, o client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(o)
	if owner == nil || owner.Kind != "SubagentRequest" || owner.APIVersion != v1.SchemeGroupVersion.String() {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: owner.Name}}}
}

// workshopChildModes is the delegation-mode ceiling for a SubagentRequest
// admitted under R1 — a child whose class was authored inside the workshop
// namespace at build time and so appears on no roster. It is the workshop's
// testing surface exactly: single_turn is what workshop test_tool asks for.
// attended is the delegation mode a workshop child would use; no workshop
// tool requests it today (pkg/tools/workshopmcp), and it stays on the
// ceiling as headroom rather than being narrowed away. task and chat are
// deliberately absent — the workshop rehearses a build, it does not run it —
// and a mode listed here is still subject to every later gate (graph,
// identity, ceiling) like any other request.
func workshopChildModes() []string {
	return []string{v1.SubagentModeSingleTurn, v1.SubagentModeAttended}
}
