package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubagentRequest is a runner's request to delegate a task to a child session.
//
// The runner creates this CR and polls it; the OPERATOR validates roster
// membership, identity monotonicity and budget, then creates the child
// AgentSession and writes the lineage tuples. The runner deliberately cannot
// create an AgentSession directly: the party whose behaviour delegation
// constrains must not also be the party that authorizes it — the same reasoning
// that puts forensic-hold trippers in the operator.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=subreq
// +kubebuilder:printcolumn:name="Parent",type=string,JSONPath=`.spec.parent.name`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.class`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SubagentRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubagentRequestSpec   `json:"spec,omitempty"`
	Status SubagentRequestStatus `json:"status,omitempty"`
}

type SubagentRequestSpec struct {
	// Parent is the delegating session, set by the runner from its own
	// identity.
	//
	// The claim is verified at admission: the validating webhook at
	// pkg/controllers/webhooks/subagentrequest compares this field against the
	// authenticated principal, which for a per-session runner is
	// `system:serviceaccount:<ns>:<session>-runner-sa` and therefore names the
	// one session it may delegate as. A runner claiming a different session —
	// to reach a larger pooled budget, a wider roster, or a userPassthrough
	// parent it could then inherit from — is refused before the object is
	// persisted.
	//
	// The webhook is failurePolicy: Fail and bounded by matchConditions to
	// runner ServiceAccounts. Principals outside that set are governed by
	// ordinary RBAC: only the per-session runner Role grants an unrestricted
	// namespace-scoped create to an untrusted principal. The operator's own
	// aggregate ClusterRole also carries the verb, cluster-wide — but only so
	// it can grant it onto each runner Role in turn (Kubernetes' RBAC
	// escalation prevention requires a granter to already hold what it
	// grants); the operator is trusted infrastructure, is structurally exempt
	// from the webhook via matchConditions, and never creates one of these
	// itself.
	Parent NamespacedRef `json:"parent"`

	// Class is the AgentClass to delegate to. Must appear in the parent class's
	// spec.subagents roster.
	// +kubebuilder:validation:MinLength=1
	Class string `json:"class"`

	// Task is the parent's own instruction, used verbatim as the child's
	// spec.prompt. In this track it is the whole payload; data slots (tag
	// references) arrive in Track 2.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=8192
	Task string `json:"task"`

	// Mode declares how much surface the child gets, and is an ATTACK-SURFACE
	// declaration rather than a UX preference. A single_turn child fed
	// laundered untrusted content does one bounded thing and returns; a
	// conversational child can be driven by that content across many turns,
	// and has a two-way channel to be driven through. Widen it deliberately.
	//
	// The surface is agent-to-agent. A conversational child is bound to a
	// Channel of kind "agent" whose counterparty is its PARENT session — no
	// person is on the other end of it, and nobody outside the delegation tree
	// can address the child at all. What still reaches a human is a
	// human-directed prompt — an approval, a credential request, an identity
	// choice — which is routed past every binding no human reads, up the
	// lineage, to the nearest ancestor channel someone is actually watching.
	//
	//   single_turn — no channel at all: spec.prompt in, status.result out.
	//                 The default, and the only mode that parallelizes, since
	//                 nothing can arrive mid-flight.
	//   task        — a channel for clarification: the child asks its parent a
	//                 bounded question and continues.
	//   chat        — a channel for a full back-and-forth with the parent.
	//   attended    — a channel for a full back-and-forth with a HUMAN rather
	//                 than the parent agent. Registered here as a legal value;
	//                 the routing, binding and provisioning that give it
	//                 behavior distinct from chat land in a later change.
	//
	// task and chat get identical PROVISIONING — the same `agent` Channel, built
	// by the same branch — and differ in two enforced ways, each read from this
	// field at the point the question is asked and nowhere else:
	//
	//   - the exchange budget. A task child's clarification is bounded to
	//     ExchangeBudget() questions; a chat child's is unbounded. The
	//     SubagentRequest controller counts it down on status.exchangesRemaining
	//     as it honours each one, and refuses the exchange after that.
	//   - the initiative gate. A task child may ASK but may not be DRIVEN: a
	//     human cannot open a turn into one (channelsd's inbound pipeline
	//     refuses it), while a chat child may be addressed freely. The parent's
	//     own reply is not a human opening a turn and is always admitted.
	//
	// Empty means single_turn. The default is opt-in-shaped on purpose,
	// matching every other capability here that widens what untrusted content
	// can drive.
	//
	// This field is a REQUEST, not a grant. The delegating agent names a mode
	// in its delegate call and it lands here; the ceiling is the PARENT
	// class's AgentClassSpec.SubagentModes entry for spec.class, and the
	// SubagentRequest controller denies anything wider than that entry allows.
	// A denied mode is never downgraded to single_turn and run anyway: a
	// silent downgrade would make the feature look broken rather than refused
	// and would hide the roster misconfiguration behind it indefinitely.
	// +kubebuilder:validation:Enum=single_turn;task;chat;attended
	// +optional
	Mode string `json:"mode,omitempty"`

	// DataSlots hands the child specific information BY REFERENCE: each entry
	// names one of the child's declared slots and the pt-tag whose content it
	// may read.
	//
	// By reference is the design, not an encoding choice. There is nowhere in
	// a slot to put content, so a parent structurally cannot launder data into
	// one — the architectural separation of instruction and data channels the
	// handoff rests on. Task carries the parent's own WORDS; this carries
	// references to data it is not repeating.
	//
	// A REQUEST, like Mode. The controller binds only the tags the parent can
	// itself read (`pt_tag:<T>#access@agentsession:<parent>`), so the parent's
	// judgment selects within an envelope it cannot widen; anything else is
	// refused and reported rather than silently dropped, because a child
	// waiting on data that will never arrive is worse than one told it did not
	// get it.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	DataSlots []DataSlotRequest `json:"dataSlots,omitempty"`
}

// DataSlotRequest binds one pt-tag into one of the child's declared data slots.
type DataSlotRequest struct {
	// Slot is the slot name the child's class declared ("diff", "logs"). The
	// child's vocabulary: the same tag in two slots is two entries.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Slot string `json:"slot"`

	// TagID is the pt_tag object id whose content the child may read.
	//
	// The WHOLE payload. A field here able to carry the datum itself would
	// defeat the by-reference property, so this deliberately names a tag and
	// nothing more.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	TagID string `json:"tagID"`
}

// DataSlotRefusalReason says why one requested slot did not arrive.
//
// Three causes, kept apart because they point a reader at three different
// actions and a single "refused" cannot. The controller already logged them
// separately; carrying the distinction on status is what lets the PARENT —
// which reads status, not logs — act on it.
// +kubebuilder:validation:Enum=NotDelegable;WouldDisclose;GradingUnavailable
type DataSlotRefusalReason string

const (
	// DataSlotNotDelegable: the parent cannot itself read the tag, so it had
	// no standing to hand it on. A mistake in what was ASKED FOR — the parent
	// should re-ask for something it holds, and retrying this is pointless.
	DataSlotNotDelegable DataSlotRefusalReason = "NotDelegable"

	// DataSlotWouldDisclose: the parent CAN read it, and handing it to this
	// child would disclose it to someone not among its readers, or the datum
	// carries untrusted content. A fact about the CHILD, not the parent, and
	// a decision no human was asked to make. This is the reason a routing
	// path would act on; today it is simply a refusal.
	DataSlotWouldDisclose DataSlotRefusalReason = "WouldDisclose"

	// DataSlotGradingUnavailable: the grade could not be computed at all.
	// TRANSIENT, and distinct from a decision on purpose — an unanswerable
	// check recorded as "would disclose" reads as a judgement nobody made,
	// and would send a parent to re-ask differently when retrying is the
	// right move.
	DataSlotGradingUnavailable DataSlotRefusalReason = "GradingUnavailable"
)

// RefusedDataSlot is one slot the parent asked for and did not get.
type RefusedDataSlot struct {
	// Slot is the child-side slot name that stayed empty.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Slot string `json:"slot"`

	// TagID is the tag that was not bound.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	TagID string `json:"tagID"`

	// Reason is why. Machine-readable so a parent can branch on it rather
	// than parse prose.
	Reason DataSlotRefusalReason `json:"reason"`
}

type SubagentRequestStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is Pending, Running, AwaitingParent, Succeeded, Failed or Denied.
	//
	// AwaitingParent is the one NON-terminal phase a caller must act on rather
	// than keep waiting through: the child has asked its delegating parent a
	// question (Message) and has parked until the answer arrives. It is what
	// makes a conversational delegation resumable — the parent's delegate call
	// returns there instead of blocking to a terminal phase, answers on its
	// next turn, and the child stays alive across the gap.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ChildRef names the created child session, empty until it exists.
	// +optional
	ChildRef *NamespacedRef `json:"childRef,omitempty"`

	// BoundDataSlots are the slots actually bound onto the child — the subset
	// of spec.dataSlots the parent had standing to delegate.
	// +optional
	BoundDataSlots []DataSlotRequest `json:"boundDataSlots,omitempty"`

	// RefusedDataSlots are the slots the parent asked for and did not get,
	// each carrying WHY.
	//
	// Recorded rather than dropped, and this is the whole reason the field
	// exists: a child waiting on data that will never arrive, with nothing
	// anywhere saying why, is a far worse failure than a refusal it can see.
	// The parent reads this back and can say so, or re-ask for something it
	// does hold.
	//
	// The reason is carried per slot because the causes point a reader at
	// different actions, and a bare list cannot tell them apart. See
	// DataSlotRefusalReason.
	// +optional
	RefusedDataSlots []RefusedDataSlot `json:"refusedDataSlots,omitempty"`

	// PendingDataSlots are slots whose binding would disclose, and for which a
	// human who can speak for the datum is being asked. Written by the
	// controller when it parks at AwaitingDisclosure; cleared once every one
	// is decided.
	//
	// Distinct from RefusedDataSlots because the states are opposite in what
	// they ask of a reader: a refusal is over, a pending slot is a question
	// still open. Collapsing them would have a parent report a denial for a
	// decision nobody has made yet.
	// +optional
	PendingDataSlots []DataSlotRequest `json:"pendingDataSlots,omitempty"`

	// InputRequest mirrors the child's outstanding mid-flight ask for data
	// (its status.inputRequest), so the parent — which polls THIS object and
	// cannot read the child's — can see what was asked for and why.
	//
	// Mirrored rather than read through, for the reason parentExchange is: the
	// parent's runner has no permission on the child's AgentSession, and RBAC
	// cannot scope a status read to one field.
	// +optional
	InputRequest *InputRequest `json:"inputRequest,omitempty"`

	// DisclosureRequestedAt is when the wait for a disclosure decision began.
	// It bounds that wait: a question nobody answers must end as a stated
	// failure rather than a delegation that hangs until the parent's own tool
	// timeout and reports nothing about why.
	// +optional
	DisclosureRequestedAt *metav1.Time `json:"disclosureRequestedAt,omitempty"`

	// ApprovedDataSlots are the slots a human cleared for disclosure to THIS
	// child. Written by channelsd when an approval lands, and consumed by the
	// controller on its next pass.
	//
	// PER REQUEST, never a standing grant, and that is the point of recording
	// it here rather than in a decision store keyed by tag: consent was given
	// to disclose this datum to this child for this delegation. The same tag
	// handed to a different child is a different disclosure and gets asked
	// again.
	// +optional
	ApprovedDataSlots []DataSlotRequest `json:"approvedDataSlots,omitempty"`

	// DeniedDataSlots are the slots a human REFUSED to disclose to this child.
	// Written by channelsd alongside ApprovedDataSlots.
	//
	// Recorded as explicitly as an approval, and that is the point of writing
	// it at all: without it the controller cannot tell "someone said no" from
	// "nobody has answered yet", so a refused disclosure would sit until the
	// wait window elapsed and then report a timeout — telling the parent
	// nobody decided when in fact someone did.
	// +optional
	DeniedDataSlots []DataSlotRequest `json:"deniedDataSlots,omitempty"`

	// Result is the child's final answer, copied from the child's
	// status.result once it reaches Succeeded.
	// +optional
	Result string `json:"result,omitempty"`

	// Artifacts are the durable outputs the child handed back alongside its
	// Result, copied from the child's status.result.artifacts when it reaches
	// Succeeded. Without this field a child could accept artifact handles into
	// return_result and nothing would ever carry them across the delegation
	// boundary: the parent could not see that an artifact existed, could not
	// attach it, and could not even decide to ask the child for it again.
	//
	// Handing an artifact to the delegating agent is delivery TO THE PARENT,
	// which is a different act from delivery to a user. Nothing here reaches a
	// person; the parent decides whether to deliver what it was given, ask for
	// something else, or produce its own.
	//
	// The CHILD's own claim, and UNTRUSTED exactly as Result and Message are.
	// Each ID is copied verbatim — this controller does not resolve it, and
	// its presence here is not evidence that any bytes exist. The parent's
	// respond_to_user is what verifies it, at the moment the parent tries to
	// attach it: that call re-reads this list, requires the render to be owned
	// by the child this request names and to be Ready, and refuses legibly
	// otherwise. Verifying HERE instead would put the check a reconcile before
	// the use, where a render that is later revised, GC'd or never was would
	// still read as verified.
	//
	// BOUNDED on copy (count and per-entry description length) rather than
	// mirrored wholesale: status is not a transport for an arbitrary payload,
	// and a child's list is as unbounded as its summary was before
	// truncateResult existed. An overflowing list is cut and said so in
	// Determination, never silently.
	// +optional
	// +listType=atomic
	Artifacts []ResultArtifact `json:"artifacts,omitempty"`

	// Message is the child's outstanding question to its parent, copied from
	// the child's status.parentExchange.question while phase is
	// AwaitingParent. It is the CHILD's own words, exactly as Result is, and
	// every consumer must treat it as untrusted content — the delegate tool
	// returns it to the parent's model as an uninspected-by-default tool
	// result, i.e. one that runs through the same content-guard inspection any
	// untrusted meta-tool result gets.
	//
	// Retained after the exchange is answered, as a record of what was last
	// asked; Exchange, not the presence of this field, says whether a question
	// is outstanding.
	// +optional
	Message string `json:"message,omitempty"`

	// Exchange is the count of questions the child has asked its parent so
	// far, mirrored from the child's status.parentExchange.exchange. It is
	// monotonic and never decreases.
	//
	// It exists so a parent can tell a NEW question from the one it just
	// answered. Without it, a reply followed by a re-poll that lands before
	// the child has woken reads the same outstanding question a second time
	// and answers itself in a loop.
	//
	// MIRRORED, therefore never a ceiling. The child writes the number this
	// copies, so it can restate it — which is exactly why the exchange budget
	// is measured against ExchangesRemaining below and never against this.
	// +optional
	Exchange int64 `json:"exchange,omitempty"`

	// ExchangesRemaining is how many FURTHER questions this delegation will
	// carry from its child to its parent. It is the exchange budget
	// SubagentRequestSpec.Mode declares, resolved once (ExchangeBudget) at the
	// point an exchange is honoured and counted down from there.
	//
	// Controller-owned and monotonically decreasing. That is the whole point:
	// the child writes its own status.parentExchange — including the exchange
	// NUMBER — so a budget measured against anything the child authors could be
	// widened by restating it, and the party a ceiling constrains must not be
	// able to widen it. Nothing outside this controller writes this field; the
	// per-session runner Role grants no write on subagentrequests/status at all.
	//
	// Nil means "no bound is in force", and covers two states that need no
	// distinguishing because neither refuses anything: a mode whose budget is
	// unbounded (chat), and a bounded delegation that has not yet had a first
	// exchange honoured. Zero means the budget is spent and the next question
	// will be refused.
	//
	// Read by the CHILD as well as by the controller: the child's ask_parent
	// checks it before recording a question, so a question that would be
	// refused comes back to the child as a legible tool result in the same turn
	// instead of parking it on an answer that will never arrive. That read is
	// legibility, not enforcement — the controller refuses regardless of what
	// the child does with it.
	// +optional
	ExchangesRemaining *int64 `json:"exchangesRemaining,omitempty"`

	// AwaitingParentSince is when the CURRENT unanswered question was first
	// observed. Controller-owned (never copied from the child) so a child
	// cannot extend its own parking bound by restamping it: it is the clock
	// the SubagentRequest controller measures a silent parent against, and a
	// child that answered to nobody must not be able to hold a session, its
	// Channel and its share of the tree's agent ceiling open forever.
	//
	// Cleared when the exchange is answered, so the bound is per-exchange
	// rather than a budget the whole conversation shares.
	// +optional
	AwaitingParentSince *metav1.Time `json:"awaitingParentSince,omitempty"`

	// CompletionTime is when this request reached a terminal phase — Succeeded,
	// Failed or Denied alike, so it is the clock for "how long has this been
	// resolved", not a success timestamp.
	//
	// Controller-owned, written in the same status update that sets the
	// terminal phase, and never rewritten afterwards. It exists for one
	// consumer: the reclaim pass that deletes a resolved delegation once it
	// has been kept long enough (SubagentRequest reconciler,
	// DefaultTerminalRetention). A delegation whose parent is still alive is
	// reclaimed by nothing else — the owner-reference cascade only runs when
	// the PARENT is deleted — so without this clock a long-lived conversational
	// parent accrues one request, one Channel and one child session per
	// delegation for its entire lifetime.
	//
	// Nil on a request that has not resolved, and on one resolved before this
	// field existed; the reclaim pass stamps the latter on its next reconcile
	// rather than treating "no clock" as "expired", so an upgrade cannot sweep
	// away every historical delegation at once.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// FailureReason is a stable machine-readable reason on Failed or Denied.
	// A DENIAL is not a Failure: keeping the phases distinct is what lets the
	// delegate meta-tool (pkg/agent/tool/meta/delegate.go) tell the parent's
	// model, in the imperative result text it returns on Denied, not to
	// re-attempt the same work through another child -- while a Failed
	// result is presented as retryable. Nothing in this type or the
	// controller binds or enforces that instruction; it is prose at the
	// tool/model boundary, not a system property.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// Determination is human-readable text explaining the phase, surfaced
	// rather than logged so a refusal is never silent.
	// +optional
	Determination string `json:"determination,omitempty"`
}

const (
	SubagentRequestPhasePending = "Pending"
	SubagentRequestPhaseRunning = "Running"
	// SubagentRequestPhaseAwaitingParent is NON-terminal: the child asked its
	// parent a question and parked. IsTerminal deliberately excludes it, so a
	// reconcile still revisits the request and the child stays alive; what
	// bounds it is the controller's own per-exchange parent-reply timeout, not
	// a phase transition the child can reach on its own.
	SubagentRequestPhaseAwaitingParent = "AwaitingParent"
	// SubagentRequestPhaseAwaitingDisclosure is NON-terminal: a requested data
	// slot would disclose its datum to this child, so a human who can speak
	// for that datum is being asked.
	//
	// THE CHILD IS NOT CREATED WHILE THIS HOLDS. Starting it with the
	// undecided slot omitted would hand it a partial handoff, which it cannot
	// distinguish from a parent that chose to send less — so it would proceed
	// on the shorter set as though that were the task. The delegation waits
	// whole or fails whole.
	SubagentRequestPhaseAwaitingDisclosure = "AwaitingDisclosure"
	SubagentRequestPhaseSucceeded          = "Succeeded"
	SubagentRequestPhaseFailed             = "Failed"
	SubagentRequestPhaseDenied             = "Denied"
)

const (
	SubagentModeSingleTurn = "single_turn"
	SubagentModeTask       = "task"
	SubagentModeChat       = "chat"
	// SubagentModeAttended is a channel for a full back-and-forth with a HUMAN
	// rather than the parent agent. Registered here as a legal Mode value; the
	// routing, respond_to_user binding and provisioning that give it behavior
	// distinct from chat are added in a later change.
	SubagentModeAttended = "attended"
)

// IsTerminal reports whether the request has reached a final phase.
func (r *SubagentRequest) IsTerminal() bool {
	switch r.Status.Phase {
	case SubagentRequestPhaseSucceeded, SubagentRequestPhaseFailed, SubagentRequestPhaseDenied:
		return true
	}
	return false
}

// EffectiveMode returns the declared mode, resolving empty to single_turn.
// Callers must use this rather than reading Spec.Mode directly: an empty
// string reaching a switch that only knows the three constants is how a
// default becomes a panic or, worse, a silent widening.
func (r *SubagentRequest) EffectiveMode() string {
	if r.Spec.Mode == "" {
		return SubagentModeSingleTurn
	}
	return r.Spec.Mode
}

// ExchangeBudget reports how many questions a child delegated in this mode may
// ask its parent, and whether the mode bounds them at all. It is one half of
// what Spec.Mode DECLARES, and this method is the only place that half is
// interpreted — the SubagentRequest controller resolves it once, where an
// exchange is honoured, and records the answer on
// SubagentRequestStatus.ExchangesRemaining. No other component reads Spec.Mode
// to answer this question; encoding it a second time (in provisioning, in the
// runner, in a settings tier) would put one attack-surface decision in two
// places that can disagree.
//
//	single_turn -> (0, true)  — headless: no channel, so no question to carry.
//	                            reconcileParentExchange never reaches the
//	                            budget for one, because it discards a
//	                            single_turn child's parentExchange outright.
//	task        -> (1, true)  — "a bounded question", per Spec.Mode's own doc.
//	chat        -> (0, false) — a full back-and-forth; nothing here bounds it.
//	attended    -> (0, false) — also a full back-and-forth, but with a human
//	                            rather than the parent agent; nothing here
//	                            bounds that conversation either.
//
// An unrecognized mode is bounded at ZERO, not unbounded. Reconcile denies one
// long before this is reached (SubagentModeUnrecognized), so this is the
// second, fail-closed answer for a stored object that reached etcd another way.
func (r *SubagentRequest) ExchangeBudget() (limit int64, bounded bool) {
	switch r.EffectiveMode() {
	case SubagentModeChat, SubagentModeAttended:
		return 0, false
	case SubagentModeTask:
		return 1, true
	default:
		return 0, true
	}
}

// PermitsHumanInitiative reports whether a PERSON may open a turn into a child
// delegated in this mode. It is the other half of what Spec.Mode declares, and
// like ExchangeBudget it is the only place that half is interpreted.
//
// chat and attended say yes. A task child "may ask; it may not be driven" —
// its whole declaration is that untrusted content moves it exactly one
// bounded step — so admitting a human-authored turn would hand it the very
// surface the mode exists to withhold. single_turn is headless and has no
// surface to be addressed on at all; it answers no here too, so a channel
// that somehow resolved to one refuses rather than falling through. attended
// is human-directed by definition — its whole point is a person driving the
// child's conversation — so it answers yes alongside chat.
//
// It says nothing about the child's PARENT. A parent answering its child is
// not a person opening a turn, and the inbound path admits that reply without
// reaching this method — see the pipeline's initiative gate.
func (r *SubagentRequest) PermitsHumanInitiative() bool {
	switch r.EffectiveMode() {
	case SubagentModeChat, SubagentModeAttended:
		return true
	default:
		return false
	}
}

// SubagentModesAll lists the four declared modes, narrowest first: single_turn
// (no channel), task (one bounded parent question), then the two unbounded
// back-and-forth modes ordered by who is on the other end — chat (the parent
// agent) before attended (a human), since a human-directed conversation is
// the widest surface of the four. A function returning a fresh slice rather
// than an exported package-level var, so no caller can mutate the canonical
// list out from under the others.
func SubagentModesAll() []string {
	return []string{SubagentModeSingleTurn, SubagentModeTask, SubagentModeChat, SubagentModeAttended}
}

// IsSubagentMode reports whether m is one of the four declared modes.
//
// The empty string is NOT one of them. Empty is the absent-field default that
// EffectiveMode resolves; a caller validating a SUPPLIED value has to tell
// "unset" apart from "misspelled", because reading a misspelling as the
// default turns an operator's or an agent's mistake into a silently different
// delegation.
func IsSubagentMode(m string) bool {
	return slices.Contains(SubagentModesAll(), m)
}

// +kubebuilder:object:root=true
type SubagentRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SubagentRequest `json:"items"`
}

func init() { SchemeBuilder.Register(&SubagentRequest{}, &SubagentRequestList{}) }
