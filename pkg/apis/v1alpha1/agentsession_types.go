package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=agses
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.class"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Turns",type="integer",JSONPath=".status.progress.turnCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// AgentSession is one running instance of an AgentClass: a conversation with
// its own runner Pod, budget, channel bindings and lifecycle phase. Sessions
// are disposable; the class they name is the durable definition.
//
// Namespaced. Reconciled by pkg/controllers/agentsession, which gates on the
// class being Valid, provisions the per-session ServiceAccount, RBAC and
// Secrets, and owns the runner Pod. Status is written by more than one actor,
// so every writer goes through pkg/controllers/agentstatus rather than a bare
// status update.
type AgentSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSessionSpec   `json:"spec,omitempty"`
	Status AgentSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSession `json:"items"`
}

// The rule pins parent on UPDATE only. It must compare, not merely test presence:
// a rule that only refuses REMOVING parent still lets it be repointed at a
// different session, which silently moves a running session's approval and
// transcript-read standing to a different set of humans.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.parent) || (has(self.parent) && self.parent == oldSelf.parent)",message="spec.parent is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(self.openingSummary) || size(self.openingSummary) == 0 || (has(self.prompt.inline) && size(self.prompt.inline) > 0)",message="openingSummary requires inline instructions"
type AgentSessionSpec struct {
	// GoalExecution binds an operator-created root session to a durable claim.
	// +optional
	GoalExecution *GoalExecutionReference `json:"goalExecution,omitempty"`

	// Class names the AgentClass in the same namespace.
	Class string `json:"class"`

	// Prompt is the initial user message. Immutable after the session
	// transitions out of Pending.
	Prompt PromptSource `json:"prompt"`

	// OpeningSummary optionally replaces the initial instruction bubble with a
	// readable summary and a control to inspect the exact inline instructions.
	// Intended for asynchronously created sessions. Render as plain text.
	// +optional
	// +kubebuilder:validation:MaxLength=2000
	OpeningSummary string `json:"openingSummary,omitempty"`

	// AgentIdentity overrides AgentClass.spec.agentIdentity; a per-bundle
	// agentIdentity still wins over this.
	// +optional
	AgentIdentity string `json:"agentIdentity,omitempty"`

	// Budget optionally narrows the AgentClass budget. Each dimension must
	// be <= the AgentClass cap.
	// +optional
	Budget *BudgetConfig `json:"budget,omitempty"`

	// InputChannel records the channel-driven origin — the Channel
	// whose listener (or scheduler, for bento) produced the inbound
	// message that created this session. Set by channelsd at session
	// creation; nil for kubectl-driven sessions.
	// +optional
	InputChannel *ChannelBinding `json:"inputChannel,omitempty"`

	// OutputChannel records the Channel the session's outbound
	// messages route through. Nil means "same as InputChannel" —
	// the existing single-Channel-per-session case (Slack role=both).
	// +optional
	OutputChannel *ChannelBinding `json:"outputChannel,omitempty"`

	// ForkedFrom names the prior AgentSession (same namespace) this
	// session was forked from via Restart-from-here. Immutable.
	// Distinct from ChannelBinding.InheritFrom (set on auto-inherit
	// after archive); ForkedFrom is set on explicit user-triggered
	// restart.
	// +optional
	ForkedFrom string `json:"forkedFrom,omitempty"`

	// ForkedAtTurn is the index of the last turn copied from
	// ForkedFrom (inclusive). The new user text became turn
	// ForkedAtTurn+1.
	// +optional
	ForkedAtTurn *int32 `json:"forkedAtTurn,omitempty"`

	// Parent is the session that delegated this one. Set ONLY by the
	// SubagentRequest controller; a child is never self-declared.
	//
	// Distinct from ForkedFrom: a fork REPLACES its parent and outlives it, so
	// it gets fresh SpiceDB tuples. A delegated child is SUBORDINATE to a live
	// parent, so its standing resolves through a live `agentsession#parent`
	// arrow and dies when the parent's does.
	//
	// Immutable: re-parenting would silently move a running session's approval
	// and transcript-read standing to a different set of humans.
	// +optional
	Parent *NamespacedRef `json:"parent,omitempty"`
}

type AgentSessionStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the lifecycle phase: Pending, Running, Idle, one of the
	// Awaiting* park states, Succeeded, or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// StartedAt is when the session first left Pending.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the session reached a terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// RunDuration is the session's cumulative ACTIVE run-time (excludes time
	// parked waiting on a human), the accumulator behind budget.maxDuration. The
	// runner seeds a new pod from this and flushes it back before the pod can
	// sleep, so run-time survives the sleep/resume boundary. Unlike
	// status.progress (which resets per pod), this is monotonic across the
	// whole session.
	// +optional
	RunDuration *metav1.Duration `json:"runDuration,omitempty"`

	// LastWakeAt records the most-recent wake-requested-at the operator has
	// acted on. Compared against the metadata annotation to detect new
	// wake-up requests.
	// +optional
	LastWakeAt *metav1.Time `json:"lastWakeAt,omitempty"`

	// LastUIServeAt records the most-recent ui-serve-requested-at the operator
	// has acted on, as LastWakeAt does for a conversation wake.
	//
	// Deliberately a SEPARATE marker: a conversation wake resumes the agent
	// loop while a UI-serve spawn never does, so sharing one would let a
	// dashboard request consume a pending message wake — the user's message
	// silently never delivered — or a page load be answered by an agent turn
	// nobody asked for.
	// +optional
	LastUIServeAt *metav1.Time `json:"lastUIServeAt,omitempty"`

	// LastIdleAt records when the session most recently entered phase=Idle.
	// Stamped by the AgentSession reconciler on Idle entry; cleared on
	// Idle → Running. Drives the operator's archive sweep deadline.
	// +optional
	LastIdleAt *metav1.Time `json:"lastIdleAt,omitempty"`

	// SleptAt is set when an Idle session's pods have been reaped (scaled to
	// zero) and cleared when the session is woken. Distinct from LastIdleAt
	// (which marks Idle entry): SleptAt marks that the compute footprint is
	// gone. Diagnostic + drives the reap decision's "already slept" guard.
	// +optional
	SleptAt *metav1.Time `json:"sleptAt,omitempty"`

	// RunnerPodName is the runner Pod currently backing the session; empty
	// once the pod has been reaped.
	// +optional
	RunnerPodName string `json:"runnerPodName,omitempty"`
	// RunnerRestarts counts container restarts observed on the runner Pod.
	// +optional
	RunnerRestarts int32 `json:"runnerRestarts,omitempty"`

	// RetryAttempts counts the number of times the runner has entered
	// AwaitingRetry (provider-error). Bumped by the runner's
	// WriteAwaitingRetry; never reset. Used by channelsd's session_watcher
	// as a dedup key for posting the Retry button (uid+RetryAttempts) so
	// each re-entry posts exactly one button.
	// +optional
	RetryAttempts int32 `json:"retryAttempts,omitempty"`

	// BundleSessions is one entry per tool bundle the AgentSession reconciler
	// provisioned a SpiceboxSession for. Empty when the class declares none.
	// +optional
	BundleSessions []ResolvedBundle `json:"bundleSessions,omitempty"`

	// ResolvedSidecarToolboxes snapshots each referenced SidecarToolbox at
	// session start, frozen for the session lifetime so a mid-flight CR edit
	// cannot change an in-flight session's runtime contract. Empty when
	// AgentClass.spec.sidecarToolboxes is empty.
	// +optional
	ResolvedSidecarToolboxes []ResolvedSidecarToolbox `json:"resolvedSidecarToolboxes,omitempty"`

	// ResolvedWorkspaceSource snapshots the bound WorkspaceSource for a running
	// session: the ref, the base PVC the overlay was cut from, and whether the
	// cut completed. Frozen at session start (like ResolvedSidecarToolboxes).
	// +optional
	ResolvedWorkspaceSource *ResolvedWorkspaceSource `json:"resolvedWorkspaceSource,omitempty"`

	// SidecarReachability is the runner's per-session live-probe result for each
	// sidecar it has probed (keyed by Name). Runner-owned; kept separate from
	// ResolvedSidecarToolboxes (operator-owned, rebuilt each reconcile) so a
	// merge write never wipes it. Empty until the runner probes a sidecar.
	// +optional
	SidecarReachability []SidecarReachability `json:"sidecarReachability,omitempty"`

	// ResolvedContentGuardDetectors snapshots the detector sidecars injected for
	// content-guard inspectors (e.g. prompt-injection). Empty ⇒ none.
	// +optional
	ResolvedContentGuardDetectors []ResolvedContentGuardDetector `json:"resolvedContentGuardDetectors,omitempty"`

	// ResolvedSkillBundles lists the skills staged onto disk in the sandbox pod
	// for this session -- every AgentSkill whose Target is sandbox/both AND
	// that some ToolBundle's StageSkills names (directly or via "*"). A
	// staged entry always carries a composed SKILL.md; supporting files ride
	// along when the skill also has a bundle. A skill that is not staged
	// (agent-targeted, or not named by any StageSkills) does not appear here
	// at all -- it still reaches the agent via load_skill.
	// +optional
	ResolvedSkillBundles []ResolvedSkillBundle `json:"resolvedSkillBundles,omitempty"`

	// ObservedPins records, per dependency the session actually used, the
	// identity observed at session start (one unified list across kinds;
	// Pin.Kind discriminates). Drift = observed != the definition CR's
	// baseline pin.
	// +optional
	ObservedPins []ObservedPin `json:"observedPins,omitempty"`

	// SlotPins is a DISPLAY-ONLY observation, one entry per single-occupancy
	// resource type, mirroring the instance SpiceDB's slot_pin relation
	// currently pins this session to. SpiceDB is the enforcement source of
	// truth for single-occupancy pinning; nothing that authorizes a bind or a
	// tool call reads this field. See SlotPin's doc comment for what writes
	// it and what MovedBy/MovedAt mean.
	// +optional
	// +listType=map
	// +listMapKey=resourceType
	SlotPins []SlotPin `json:"slotPins,omitempty"`

	// CredentialAuthFailures records, per tool ORIGIN, that the platform ITSELF
	// observed an auth-shaped failure there — the independent corroboration a
	// CredentialUpdateRequest needs when the provider cannot be re-probed to
	// confirm the agent's claim. Runner-owned (only the runner sees a tool
	// call's upstream outcome); the operator reads and never writes it.
	//
	// An entry is REMOVED the moment a call to the same origin succeeds: a
	// stale entry would corroborate a healthy credential forever and help talk
	// a human into re-entering a working token.
	// +optional
	CredentialAuthFailures []CredentialAuthFailure `json:"credentialAuthFailures,omitempty"`

	// Progress accumulates turn, token and tool counts for the CURRENT pod;
	// unlike RunDuration it resets when the pod is replaced.
	// +optional
	Progress *AgentSessionProgress `json:"progress,omitempty"`
	// Result is the agent's final summary and artifacts, set on success.
	// +optional
	Result *AgentResult `json:"result,omitempty"`
	// PinnedMessage is the runner's projection of the triggered session's
	// opening message; channelsd renders it onto the anchored opening line.
	// +optional
	PinnedMessage *PinnedMessageStatus `json:"pinnedMessage,omitempty"`
	// CompletionBypasses records every time this session declared its work
	// complete while a requirement its AgentClass declared was unmet.
	//
	// An OBSERVATION the runner writes, never an applied field: it carries a
	// wall-clock stamp and accumulates as the session runs, so it belongs in
	// controller-owned status rather than anywhere a client server-side-applies.
	// Append-only within a session — a later bypass never rewrites an earlier
	// one, because "it happened twice" is the fact an operator most wants.
	// +optional
	// +listType=atomic
	CompletionBypasses []CompletionBypass `json:"completionBypasses,omitempty"`

	// FailureReason is the human-readable reason phase became Failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
	// RunnerNotes are timestamped diagnostics the runner appended for operators.
	// +optional
	RunnerNotes []RunnerNote `json:"runnerNotes,omitempty"`

	// AppliedInteractPermission is the SessionInteractPermission value
	// captured from AgentClass at session creation. Frozen for the
	// session's lifetime; admins can compare against the class's
	// current spec to see drift.
	// +optional
	AppliedInteractPermission string `json:"appliedInteractPermission,omitempty"`

	// AppliedInteractPermissionAt is when the snapshot was captured.
	// +optional
	AppliedInteractPermissionAt *metav1.Time `json:"appliedInteractPermissionAt,omitempty"`

	// PendingRequesters tracks ad-hoc permission requests that have
	// been delivered to the original requester but not yet decided.
	// Channel-agnostic; populated and cleared by channelsd's pipeline.
	// +optional
	PendingRequesters []PendingRequester `json:"pendingRequesters,omitempty"`

	// PendingInteractions is the single list of interaction prompts awaiting a
	// user decision — tool_approval, info_leakage and content_inspection all
	// park here, keyed by Category. Written by channelsd's
	// HandleInteractionRequest. Each entry is self-contained so consumers
	// (oap session approve, status-watchdog, webd, admind) can display and
	// drive the decision without reading the memory layer. It does NOT drive
	// phase: phase=AwaitingDecision derives from the runner's signed lifecycle
	// projection.
	// +optional
	// +listType=map
	// +listMapKey=requestID
	PendingInteractions []PendingInteraction `json:"pendingInteractions,omitempty"`

	// SupersededBy names the AgentSession that forked from this one
	// via Restart-from-here. Set when phase transitions to Succeeded
	// due to a fork (rather than natural completion). Diagnostic only.
	// +optional
	SupersededBy string `json:"supersededBy,omitempty"`

	// PendingRestart is set by channelsd when a RestartTrigger is
	// published, and cleared by the controller once the fork is
	// complete (SupersededBy populated). Crash-recovery marker; the
	// controller's reconciler retries an unfinished restart on resume.
	// +optional
	PendingRestart *PendingRestart `json:"pendingRestart,omitempty"`

	// StartFailure is set by channelsd when it detects a terminal pre-start
	// failure (e.g. an authz-write failure on session creation). The operator
	// reads it and produces the Failed state — channelsd never writes
	// phase/failureReason/finishedAt itself.
	// +optional
	StartFailure *AgentSessionStartFailure `json:"startFailure,omitempty"`

	// SatisfiedSecretOutputs records secret-output handles the runner has
	// captured and published to the per-session secret-output Secret (via the
	// operator-mediated endpoint). Observability only: the value is never here.
	// +optional
	// +listType=map
	// +listMapKey=handle
	SatisfiedSecretOutputs []SecretOutputCompletion `json:"satisfiedSecretOutputs,omitempty"`

	// AuditPublicKey is the base64 (std) Ed25519 public key whose
	// private half (per-session Secret key "audit-signing-key") signs
	// this session's append-only audit entries. The K8s-witnessed trust
	// anchor for offline verification (`oap audit verify`).
	// +optional
	AuditPublicKey string `json:"auditPublicKey,omitempty"`
	// AuditKeyID is the key fingerprint (hex SHA-256 prefix) — matches
	// the keyId in each signed entry's provenance.
	// +optional
	AuditKeyID string `json:"auditKeyID,omitempty"`
	// AuditChainHeads records each publisher's final chain head
	// (publisher → "seq:lastHash"), stamped once the session reaches a
	// terminal phase. It anchors tail-truncation detection for ended
	// sessions; `oap audit verify` reads it.
	// +optional
	AuditChainHeads map[string]string `json:"auditChainHeads,omitempty"`

	// EffectiveSettings is the resolved 4-tier settings snapshot
	// (cluster → namespace → class → session). Stamped by the AgentSession
	// reconciler before the runner pod is created; the runner reads it.
	// +optional
	EffectiveSettings *EffectiveSettings `json:"effectiveSettings,omitempty"`

	// EffectiveIdentityMode is the RESOLVED identity mode (agent|userPassthrough).
	// For static modes it mirrors spec.identityMode. For ask|dynamic it is unset
	// until the initiating user chooses, then set from the signed choice event.
	// All runtime identity logic reads THIS, not spec.identityMode.
	// +optional
	EffectiveIdentityMode string `json:"effectiveIdentityMode,omitempty"`

	// IdentityChoiceParkedAt is when the session entered AwaitingIdentityChoice.
	// The operator measures IdentityChoiceTimeout from here.
	// +optional
	IdentityChoiceParkedAt *metav1.Time `json:"identityChoiceParkedAt,omitempty"`

	// StartApprovalParkedAt is when the session entered AwaitingStartApproval
	// (parked on AnnotationStartApprovalRequestRef). The operator measures the
	// start-approval deadline from here.
	// +optional
	StartApprovalParkedAt *metav1.Time `json:"startApprovalParkedAt,omitempty"`

	// EstimatedCost is the best-effort end-of-session USD cost estimate.
	// Runner-owned; written at SessionEnd when reportSessionCost is on.
	// +optional
	EstimatedCost *EstimatedSessionCost `json:"estimatedCost,omitempty"`

	// AwaitingUserInputSince is set when the agent yields via
	// await_user_message and cleared on resume or terminal. It is the durable
	// floor under the at-most-once turn_activity yield event: a non-nil value
	// counts as a visible wait, so the silence watchdog never warns while the
	// agent is correctly waiting for a reply. A scalar rather than a condition,
	// so it composes with the owner-partitioned condition merge in
	// pkg/controllers/agentstatus.
	// +optional
	AwaitingUserInputSince *metav1.Time `json:"awaitingUserInputSince,omitempty"`

	// ParentExchange is a delegated child's side of the conversation with the
	// agent that delegated to it. Written by the child's own runner (the
	// ask_parent meta tool sets it; the loop clears Pending on resume) and read
	// by the SubagentRequest controller, which mirrors it onto the request the
	// child answers so the parent's delegate call can return with the question
	// instead of blocking to a terminal phase.
	//
	// Set only on a conversational (task/chat) child. A single_turn child is
	// headless, has no parent to ask, and leaves this nil for its whole life.
	// +optional
	ParentExchange *ParentExchange `json:"parentExchange,omitempty"`

	// InputRequest is this child's outstanding mid-flight ask for data. The
	// SubagentRequest controller mirrors it onto the request its parent is
	// polling, the same route ParentExchange takes.
	// +optional
	InputRequest *InputRequest `json:"inputRequest,omitempty"`

	// AgentWakeCredit is how many further AGENT-DRIVEN wakes this session will
	// accept before a human speaks again. Refilled to the class's WakeBudget by
	// every human turn, counted down by each agent-driven wake.
	//
	// CHANNELSD-OWNED, and that ownership is the control. The sessions it
	// bounds are the ones talking to each other, so a budget either of them
	// could write would be widened by the loop it exists to stop — the same
	// reasoning that makes SubagentRequest's ExchangesRemaining
	// controller-owned rather than mirrored from the child.
	//
	// Nil means no cross-agent wake has been considered yet. Zero means the
	// budget is spent: further agent messages still APPEND — the session sees
	// the whole conversation — but none of them wakes it until a person
	// speaks.
	// +optional
	AgentWakeCredit *int `json:"agentWakeCredit,omitempty"`

	// ClosureDenied reports that some member of this session's DELEGATION
	// CLOSURE has had an authorization decision denied. The trifecta dispatch
	// gate refuses in every mode when it is true — a denied closure is a
	// structural fact ("this delegation has already been judged to have gone
	// wrong"), not a policy judgement about the call in front of the gate.
	//
	// OPERATOR-DERIVED, and it has to be. Answering it means reading every
	// session in the tree, and the runner's Role pins agentsessions to its own
	// name with no `list` at all — a runner-side closure walk is Forbidden at
	// the first hop in production while passing every test on an admin client.
	// So the operator computes the closure fact and the runner reads it from
	// the one object it is allowed to Get: its own.
	//
	// A denial ANYWHERE in the closure sets it, because that is what makes the
	// signal useful: a parent that cannot get approval routes around the
	// refusal by delegating to a different child, and a per-session flag would
	// let it. Nil means not yet evaluated; false means evaluated and clean.
	// +optional
	ClosureDenied *bool `json:"closureDenied,omitempty"`

	// ToolGuard reflects live tool-guard enforcement: currently-open circuit
	// breakers. Patched by the runner on breaker transitions.
	// +optional
	ToolGuard *ToolGuardStatus `json:"toolGuard,omitempty"`

	// ActiveWidgets records the most recent MCP-UI interactive widgets
	// persisted for this session, so a user opening the browser session-view
	// page minutes after a widget was produced can still fetch and render it.
	// Runner-owned; capped to the most recent entries to bound growth over a
	// long session.
	// +optional
	ActiveWidgets []WidgetRef `json:"activeWidgets,omitempty"`

	// PermissionSurface is what this session's tools can actually reach:
	// every permission handle the runner enumerated from the live tool
	// envelope, with the tools that reach it.
	//
	// It answers "what can this agent do?" for the CLI and admin panel from
	// the ONE producer that can answer it exactly — the runner, which holds
	// the real []tool.Tool. Any second derivation (walking CRs, guessing at
	// MCP tool names) would drift from what dispatch actually checks, and the
	// surface≡dispatch equivalence is the whole point of enumerating it.
	//
	// An OBSERVATION, never authority. Nothing may gate on this field: it is
	// a snapshot of a resolution, and a resolution read back later could be
	// stale. Durable authority is carried by handles in frozen plans,
	// approval records and the audit log — which is what permsurface's
	// "only Handle persists" rule is protecting. Writing it here rather than
	// into memory keeps that line visible: status is where this repo puts
	// things a controller observed, and status is definitionally not a grant.
	//
	// A pure function of the tool envelope — sorted, deduplicated, no
	// timestamps — so re-writing it on an unchanged session is a no-op diff.
	// +optional
	PermissionSurface []PermissionSurfaceEntry `json:"permissionSurface,omitempty"`

	// PassthroughCredHashes maps each projected passthrough credential name to
	// the SHA-256 hex of its value as of the last reconcile. A change on the
	// next reconcile means the credential was re-linked or removed, and the
	// operator emits a per-session `credential` invalidation on the
	// ap.revocation bus so a running session picks it up on its next tool call.
	// Operator-owned observation, set on change — never an SSA-applied field.
	// +optional
	PassthroughCredHashes map[string]string `json:"passthroughCredHashes,omitempty"`

	// InteractorTuplesWritten lists the subjects for which this session has
	// already written an agentclass#interactor tuple, so a long multi-turn
	// session does not re-issue the idempotent SpiceDB touch every reconcile.
	// Controller-owned; set-once per subject.
	// +optional
	InteractorTuplesWritten []string `json:"interactorTuplesWritten,omitempty"`

	// Conditions are the session's status conditions, partitioned by owner and
	// merged through pkg/controllers/agentstatus.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// PendingInteraction is one generic interaction awaiting a user decision.
// Written by HandleInteractionRequest on park, cleared by
// HandleInteractionDecision (decision) / HandleInteractionApplied (timeout).
// The field set is a modest union of the approval families' display fields;
// tool_approval, info_leakage, and content_inspection each populate the subset
// they need (this is the single pending-approval list for all three).
type PendingInteraction struct {
	// RequestID is the interaction RequestRef — the map key + the handle
	// echoed on the eventual interaction_applied.
	RequestID string `json:"requestID"`

	// Category is the registered interaction category ("content_inspection").
	// status-watchdog switches wait-visibility on it; oap session approve
	// selects the decision envelope from it.
	Category string `json:"category"`

	// ApproverSubject is the SpiceDB subject-set ref that has standing to
	// decide (session-set policies: "agentsession:<ns>/<name>#approve").
	// oap session approve uses it for the client-side pre-check; the server
	// re-checks authoritatively. Empty for policies with no fixed subject.
	// +optional
	ApproverSubject string `json:"approverSubject,omitempty"`

	// RequestRef is the channel-kind-opaque round-trip handle. For the generic
	// model this equals RequestID; kept distinct for parity with the typed
	// lists and future kinds.
	// +optional
	RequestRef string `json:"requestRef,omitempty"`

	// RequestedAt is the park timestamp; used by display + the timeout watcher.
	RequestedAt metav1.Time `json:"requestedAt"`

	// AgentDisplayName is the operator-configured AgentClass label, forwarded
	// from the request payload for restart-resilient display.
	// +optional
	AgentDisplayName string `json:"agentDisplayName,omitempty"`

	// Summary is the publisher-authored one-line human description (the
	// interaction_request Lead) — the self-contained display string consumers
	// render without reading the memory layer. Category-agnostic.
	// +optional
	Summary string `json:"summary,omitempty"`
}

// PendingRequester captures one ad-hoc permission request that has
// been delivered to the original requester but not yet decided.
// Channel-agnostic; populated and cleared by channelsd's pipeline.
// RequestRef is opaque to the pipeline (the channel kind defines its
// format) and round-tripped to the eventual decision.
type PendingRequester struct {
	// Kind is the channel kind ("slack", future "discord", ...).
	Kind string `json:"kind"`
	// TeamScope is the per-kind workspace/server scope (Slack team ID, ...).
	// +optional
	TeamScope string `json:"teamScope,omitempty"`
	// ExternalID is the per-kind user id (Slack U-id, ...).
	ExternalID string `json:"externalId"`
	// Email is the user's email when the kind has it.
	// +optional
	Email string `json:"email,omitempty"`

	// RequestRef is the kind-defined opaque handle the channel kind
	// uses to find its user-facing artifacts (rejection message, DM,
	// thread refs) when the decision arrives.
	RequestRef string `json:"requestRef"`

	// Category is the interaction category this entry was raised under —
	// "start_approval" for a parked guest session start; empty means the
	// original session-join request ("permission_request"). The decision
	// pipe's category witness reads it so a click cannot resolve a pending
	// request under a weaker standing policy than it was raised behind.
	// +optional
	Category string `json:"category,omitempty"`

	// RequestedAt is when the request was delivered to the original
	// requester.
	RequestedAt metav1.Time `json:"requestedAt"`

	// MessageText is the body of the inbound that triggered the
	// permission request. Replayed verbatim through the inbound
	// pipeline when the original requester clicks Approve, so the
	// user does not have to re-send their message after approval.
	// Truncated to a reasonable bound by the pipeline before storage.
	// +optional
	MessageText string `json:"messageText,omitempty"`

	// ChannelKey is the per-channel routing key (e.g.
	// "thread:C0:1.2") that the inbound carried. Replayed alongside
	// MessageText so the resubmit lands on the same session/thread.
	// +optional
	ChannelKey string `json:"channelKey,omitempty"`
}

// ResolvedBundle records the SpiceboxSession the operator provisioned for one
// of the class's tool bundles.
type ResolvedBundle struct {
	// Name is the bundle name from AgentClass.spec.toolBundles.
	Name string `json:"name"`
	// SpiceboxSessionName is the sandbox session provisioned for the bundle.
	SpiceboxSessionName string `json:"spiceboxSessionName"`
	// AgentIdentity is the identity resolved for this bundle's sandbox.
	AgentIdentity string `json:"agentIdentity"`
	// Restarts counts how many times the operator deleted + recreated this
	// bundle's SpiceboxSession after it reported Failed. One retry is allowed
	// per session; a second failure is terminal (BundleFailed).
	// +optional
	Restarts int32 `json:"restarts,omitempty"`
	// RetriedSessionUID is the UID of the failed SpiceboxSession instance
	// the retry deleted. The terminal check ignores Failed observations
	// carrying this UID: they are stale cache reads of the deleted
	// instance, not the replacement failing.
	// +optional
	RetriedSessionUID string `json:"retriedSessionUID,omitempty"`
}

// ResolvedSkillBundle records one skill the operator staged onto disk in the
// sandbox pod for a session. A skill that is not staged at all (agent-
// targeted, or not named by any ToolBundle's StageSkills) does not appear
// here.
type ResolvedSkillBundle struct {
	// CanonicalName is the skill's canonical name (the AgentClass opt-in key).
	CanonicalName string `json:"canonicalName"`
	// LocalName is the AgentClass's AgentSkill.Name for this skill -- the
	// name a disk-based consumer (Claude Code) actually discovers, because
	// the sandbox pod builder mounts the staged bundle's CONTENT at
	// /skills/<LocalName>/ (see pkg/controllers/agentsession/bundles.go's
	// BuildBundleSession). It is deliberately NOT the same value as
	// MountName below: LocalName only has to be unique within one
	// AgentClass (enforced by validateSkillsSpec) and match the skill's own
	// SKILL.md frontmatter name (enforced in resolveAndStageSkillBundles),
	// whereas MountName has to be safe and unique as a Kubernetes object
	// name/volume name across every repo and version a cluster ever sees --
	// two different scopes, two different values.
	LocalName string `json:"localName"`
	// MountName is the sanitized, collision-free ConfigMap/volume name the
	// staged bundle's ConfigMap and the pod's internal shared-unpack
	// subPath use. It is NOT the sandbox-visible directory name a disk-based
	// consumer discovers the skill under -- see LocalName for that.
	MountName string `json:"mountName"`
	// ConfigMapName is the per-session ConfigMap holding the bundle tarball.
	ConfigMapName string `json:"configMapName"`
	// Digest is a cheap, no-I/O change-detection fingerprint over this
	// skill's staging inputs (the composed SKILL.md content, plus the
	// underlying Skill's bundle digest when one exists). It lets the
	// operator skip re-reading the bundle store on an unchanged reconcile
	// pass; it is NOT the hash of the staged archive's actual bytes -- see
	// ArchiveDigest for that.
	Digest string `json:"digest"`
	// ArchiveDigest is the sha256 content digest of the actual staged
	// archive bytes (the ConfigMap's bundle.tar.gz key). The sandbox pod's
	// mount-unpack init container verifies content against it before
	// extracting, so a ConfigMap that drifted between staging and use fails
	// the pod rather than silently running altered content.
	// +optional
	ArchiveDigest string `json:"archiveDigest,omitempty"`
}

// ResolvedContentGuardDetector snapshots one content-guard detector that the
// operator runs as a SEPARATE per-session pod (own NetworkPolicy, egress denied)
// for the inspector that needs it. The runner reaches it at
// http://<PodIP>:<Port> (set into the runner's CONTENTGUARD_DETECTOR_ENDPOINT
// once the detector pod is Ready). Runner-pod creation is gated on PodIP being
// reflected here — mirrors separate-pod SidecarToolbox.
type ResolvedContentGuardDetector struct {
	// Inspector is the content-guard inspector this detector serves.
	Inspector string `json:"inspector"`
	// Image is the detector container image.
	Image string `json:"image"`
	// Port is the detector's HTTP port on its pod.
	Port int32 `json:"port"`
	// HealthPath is the readiness path; empty means the pod has no probe path.
	HealthPath string `json:"healthPath,omitempty"`
	// PodName is the detector pod's name.
	PodName string `json:"podName,omitempty"`
	// PodIP is the detector pod's IP, reflected once Ready. Empty ⇒ not ready;
	// the operator requeues and holds runner-pod creation.
	PodIP string `json:"podIP,omitempty"`
}

// ResolvedWorkspaceSource snapshots a session's bound WorkspaceSource.
type ResolvedWorkspaceSource struct {
	// Ref is the bound WorkspaceSource CR name.
	Ref string `json:"ref"`
	// BaseClaimName is the shared base PVC the session overlay was cut from.
	BaseClaimName string `json:"baseClaimName"`
	// OverlayCut is true once the reflink cut of base -> the session workspace
	// PVC has completed successfully.
	// +optional
	OverlayCut bool `json:"overlayCut,omitempty"`

	// Kind/Locator/Revision snapshot the bound source's driver + locator + git
	// revision at session start, so the runner can build sync/apply commands.
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Locator string `json:"locator,omitempty"`
	// +optional
	Revision string `json:"revision,omitempty"`

	// ReconcileImage and ReconcileServiceAccount snapshot the git image and
	// ServiceAccount the runner's sync_workspace/apply_workspace Job runs as.
	// The operator resolves them from its own config (--materialize-image,
	// --snapshot-service-account), so a cluster that digest-pins the
	// base-materialize Job gets the same pinned image here without a separate
	// flag. Empty ⇒ the runner falls back to its built-in defaults.
	// +optional
	ReconcileImage string `json:"reconcileImage,omitempty"`
	// +optional
	ReconcileServiceAccount string `json:"reconcileServiceAccount,omitempty"`
}

// ResolvedSidecarToolbox snapshots one SidecarToolbox CR for a running
// AgentSession. Includes the LLM-facing prefix and the operator-allocated
// loopback port the runner uses to reach the sidecar.
type ResolvedSidecarToolbox struct {
	// Name is the LLM-facing prefix (the `name` field from
	// AgentClassSidecarToolboxRef).
	Name string `json:"name"`

	// Ref is the SidecarToolbox CR name (the `ref` field from
	// AgentClassSidecarToolboxRef).
	Ref string `json:"ref"`

	// Port is the runner-side loopback port (operator-allocated).
	// Reachable at 127.0.0.1:<Port>.
	Port int32 `json:"port"`

	// Spec is the SidecarToolbox spec snapshot at session-start time.
	Spec SidecarToolboxSpec `json:"spec"`

	// EffectiveNetworkMode is the merged network mode the sidecar runs under:
	// the SpiceboxClass's mode, upgraded to `allowlist` when the class says
	// `none` but the SidecarToolbox adds allowedHosts. For separate-pod
	// sidecars the operator stamps an L3/L4 NetworkPolicy from it (none → full
	// egress deny; allowlist → DNS + coarse TCP 443/80).
	// +optional
	EffectiveNetworkMode NetworkMode `json:"effectiveNetworkMode,omitempty"`

	// EffectiveAllowedHosts is the sorted, deduped union of the SpiceboxClass's
	// network.allowedHosts and the SidecarToolbox's sandbox.network.allowedHosts.
	// RECORDED, not enforced: hostnames are not expressible in stock
	// NetworkPolicy, so this is the source of truth a DNS-aware policy
	// controller consumes to narrow beyond the coarse L3/L4 policy.
	// +optional
	EffectiveAllowedHosts []string `json:"effectiveAllowedHosts,omitempty"`

	// RunMode is "in-pod" (default, container in the agent pod) or
	// "separate-pod" (secret-gated: its own per-session pod).
	// +optional
	RunMode string `json:"runMode,omitempty"`
	// SidecarPodName is the separate sidecar pod's name (RunMode=="separate-pod").
	// +optional
	SidecarPodName string `json:"sidecarPodName,omitempty"`
	// SidecarPodIP is the separate sidecar pod's IP, reflected once Ready; the
	// runner reaches it at http://<SidecarPodIP>:<port>.
	// +optional
	SidecarPodIP string `json:"sidecarPodIP,omitempty"`
	// AwaitingSecret is true when a secret-gated toolbox's bound secret-output
	// is not yet available; the operator holds pod creation until it is.
	// +optional
	AwaitingSecret bool `json:"awaitingSecret,omitempty"`
	// SecretTokenHash is a hash of the resolved secret value; a change triggers
	// pod replacement.
	// +optional
	SecretTokenHash string `json:"secretTokenHash,omitempty"`

	// PodFailure records a TERMINAL container state on a separate sidecar pod
	// (CrashLoopBackOff, ImagePullBackOff) — one the pod will not recover from
	// on its own. Nil while the pod is healthy or still starting; cleared once
	// it goes Ready.
	//
	// OPERATOR-owned, deliberately separate from the runner-owned
	// SidecarReachability: reachability answers "the runner probed a running
	// server and it did/didn't answer", PodFailure answers "the pod never got
	// far enough to be probed." A crash-looping sidecar never receives a PodIP,
	// so the prober never runs and reachability would stay silently empty —
	// exactly the silent degradation this field exists to end.
	// +optional
	PodFailure *SidecarPodFailure `json:"podFailure,omitempty"`
}

// SidecarPodFailure is the operator's record of why a separate-pod sidecar
// will not become Ready. The runner reads it to tell the agent and the user
// that a promised toolset is unavailable, and why, instead of proceeding
// tool-less and silently.
//
// It carries no timestamp on purpose: AgentSession status is written under
// only-changed-writes semantics, so a wall-clock field would rewrite status on
// every reconcile and churn field ownership. The Pod itself remains the source
// of truth for timing.
type SidecarPodFailure struct {
	// Reason is the kubelet's terminal container Waiting reason, e.g.
	// "CrashLoopBackOff" or "ImagePullBackOff".
	Reason string `json:"reason"`

	// Message is the most actionable text available: the container's last
	// terminated message when present — the crashing container's log tail,
	// thanks to the pod's FallbackToLogsOnError policy — otherwise the
	// kubelet's waiting message. Preferring the log tail is what makes the
	// field worth surfacing; the waiting message alone is content-free for a
	// crash loop ("back-off Ns restarting failed container=…").
	// +optional
	Message string `json:"message,omitempty"`

	// ExitCode is the container's last terminated exit code. Nil when the
	// container never ran (e.g. the image could not be pulled).
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
}

// SidecarReachability is the runner's live-probe result for one sidecar, keyed
// by Name. Written EXCLUSIVELY by the runner, which is co-located, already
// permitted runner→sidecar by the per-session NetworkPolicy, and probes a
// secret-gated sidecar's REAL pod with the REAL delivered secret. It is its
// own list rather than fields on ResolvedSidecarToolbox because the operator
// rebuilds that array each reconcile and a JSON-merge write would wipe
// per-element runner fields.
//
// No entry = not yet probed (AwaitingSecret, or pod not Ready). Reachable=true
// = tools/list succeeded and the allowlist is satisfied. Reachable=false = up
// but unreachable or drifted, with Unreachable carrying the reason, so a
// mid-session sidecar failure is never silent.
type SidecarReachability struct {
	// Name is the LLM-facing prefix (ResolvedSidecarToolbox.Name).
	Name string `json:"name"`
	// Reachable is true when the last live tools/list probe succeeded and the
	// declared allowlist was satisfied.
	Reachable bool `json:"reachable"`
	// ObservedTools is the tool names the live probe reported (sorted). The
	// runtime source of truth, distinct from the SidecarToolbox CR's
	// admission-time status.observedTools (which is not read at runtime).
	// +optional
	ObservedTools []string `json:"observedTools,omitempty"`
	// Unreachable is the probe/allowlist error when Reachable is false; empty
	// when Reachable is true.
	// +optional
	Unreachable string `json:"unreachable,omitempty"`
	// ObservedAt is when the runner last probed this sidecar.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// UpsertSidecarReachability replaces (by Name) or appends the given per-session
// sidecar reachability observation, mirroring UpsertObservedPin. Runner-owned;
// the operator never writes this list.
func UpsertSidecarReachability(list []SidecarReachability, entry SidecarReachability) []SidecarReachability {
	for i := range list {
		if list[i].Name == entry.Name {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}

// CredentialAuthFailure is one origin's auth-shaped-failure observation — the
// platform's OWN evidence that a credential stopped authenticating, as distinct
// from the agent's (untrusted) claim in a CredentialUpdateRequest.
//
// Written EXCLUSIVELY by the runner, the only component that sees a tool call's
// upstream outcome, and only on the TRANSITION into the failing state: the
// counter lives in runner memory, so a long run of failures is one status
// write, not one per call.
//
// Presence is the whole signal, and there is deliberately no "cleared"
// tombstone — a success REMOVES the entry (RemoveCredentialAuthFailure),
// because an observation outliving the failure it describes would corroborate
// a credential that demonstrably still works.
type CredentialAuthFailure struct {
	// Origin is the failing tool's tool.OriginTool.Origin() value, e.g.
	// "mcpserver/github" — the same vocabulary CredentialUpdateRequestSpec.Origin
	// uses, so the reconciler can match an observation to a request without a
	// second resolution step.
	Origin string `json:"origin"`

	// Count is how many consecutive auth-shaped failures the runner had seen
	// at this origin when the observation was recorded. It is NOT kept live:
	// later failures increment only the runner's in-memory counter, so this
	// stays at its transition-time value until a success clears the entry and
	// a fresh failure records a new one.
	// +optional
	Count int32 `json:"count,omitempty"`

	// ObservedAt is when the runner recorded this observation.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// UpsertCredentialAuthFailure replaces (by Origin) or appends one auth-failure
// observation, mirroring UpsertSidecarReachability. Runner-owned; the operator
// never writes this list.
func UpsertCredentialAuthFailure(list []CredentialAuthFailure, entry CredentialAuthFailure) []CredentialAuthFailure {
	for i := range list {
		if list[i].Origin == entry.Origin {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}

// FindCredentialAuthFailure returns origin's observation, or nil when there is
// none — the operator's READ half of the runner-written list, used by the
// CredentialUpdateRequest reconciler to decide whether the platform has its own
// evidence backing the agent's claim (credupdate.Input.Corroborated).
//
// Presence IS the signal, so nothing about Count or ObservedAt enters the
// verdict; the entry is returned rather than a bool only so a caller can log
// WHY it corroborated.
//
// An EMPTY origin never matches: such a request resolves to no credential
// anyway, and an empty key must not collide with a malformed entry and
// manufacture corroboration out of a list this function does not own.
func FindCredentialAuthFailure(list []CredentialAuthFailure, origin string) *CredentialAuthFailure {
	if origin == "" {
		return nil
	}
	for i := range list {
		if list[i].Origin == origin {
			return &list[i]
		}
	}
	return nil
}

// RemoveCredentialAuthFailure drops origin's observation, if present — the
// clear-on-success half of CredentialAuthFailure's invariant: a credential that
// just worked must stop corroborating a replacement request.
//
// Removing the last entry returns nil, not an empty slice, so `omitempty` drops
// the field and the merge patch carries a deletion rather than an empty array,
// matching the other runner-owned observation lists.
func RemoveCredentialAuthFailure(list []CredentialAuthFailure, origin string) []CredentialAuthFailure {
	out := make([]CredentialAuthFailure, 0, len(list))
	for i := range list {
		if list[i].Origin == origin {
			continue
		}
		out = append(out, list[i])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AgentSessionProgress accumulates usage counters for the CURRENT runner pod;
// they reset when the pod is replaced.
type AgentSessionProgress struct {
	// TurnCount is how many agent turns have completed.
	// +optional
	TurnCount int32 `json:"turnCount,omitempty"`
	// InputTokens is cumulative prompt tokens sent to the provider.
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens is cumulative completion tokens returned by the provider.
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheCreationTokens is cumulative cache-write tokens (priced ~1.25x input).
	// +optional
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
	// CacheReadTokens is cumulative cache-read tokens (priced ~0.1x input).
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// ToolCallCount is how many tool calls the agent has issued.
	// +optional
	ToolCallCount int32 `json:"toolCallCount,omitempty"`
}

// EstimatedSessionCost is a best-effort USD estimate of a session's LLM spend,
// stamped at SessionEnd from cumulative token usage × the provider's per-model
// pricing. Money is stored as integer micro-USD (1e-6 USD) to avoid float drift.
// PricingKnown=false means the model had no price; AmountMicroUSD is then 0 and
// must not be shown as a real cost.
type EstimatedSessionCost struct {
	// AmountMicroUSD is the session total in micro-USD (1e-6 USD); 0 and
	// meaningless when PricingKnown is false.
	// +optional
	AmountMicroUSD int64 `json:"amountMicroUSD,omitempty"`
	// Currency is the ISO code the amount is denominated in ("USD").
	// +optional
	Currency string `json:"currency,omitempty"`
	// Model is the configured model the session ran under.
	// +optional
	Model string `json:"model,omitempty"`
	// PricingKnown is false when the model had no price; the amount is then 0
	// and must not be shown as a real cost.
	// +optional
	PricingKnown bool `json:"pricingKnown,omitempty"`
	// AsOf is when the estimate was computed.
	// +optional
	AsOf metav1.Time `json:"asOf,omitempty"`

	// ByModel breaks the total down per actually-served model, so a session
	// that fell back or auto-routed across models shows where the spend went
	// instead of one blended total under the configured model. Empty when the
	// runner reported no per-model usage; AmountMicroUSD and PricingKnown are
	// then the sole source of truth.
	// +optional
	// +listType=atomic
	ByModel []ModelCostBucket `json:"byModel,omitempty"`
}

// ModelCostBucket is one served model's slice of a session's cost estimate.
type ModelCostBucket struct {
	// Model is the served-model display id verbatim (provider/model, or a
	// provider-prefixed served model under routing) — never re-parsed here.
	Model string `json:"model"`
	// InputTokens is this model's share of prompt tokens.
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens is this model's share of completion tokens.
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// AmountMicroUSD is this bucket's share of the session cost, in
	// micro-USD (1e-6 USD): the provider-reported cost when available,
	// else tokens × the resolved per-model pricing table rate.
	// +optional
	AmountMicroUSD int64 `json:"amountMicroUSD,omitempty"`
	// PricingKnown is true when AmountMicroUSD is a real cost — either the
	// provider reported it directly, or the pricing table had a rate for
	// this model. False means AmountMicroUSD is 0 and must not be shown as
	// real spend (same no-fabrication contract as EstimatedSessionCost).
	// +optional
	PricingKnown bool `json:"pricingKnown,omitempty"`
}

// ParentExchange is a delegated child's record of asking the agent that
// delegated to it a question, and of whether that question is still
// outstanding.
//
// It is the child→parent half of a resumable delegation, and it is carried
// here rather than written straight onto the SubagentRequest on purpose:
// RBAC cannot scope a status write to one field, so a runner holding
// `update` on subagentrequests/status could mark its own delegation Denied
// or Succeeded — fabricating a policy refusal the parent is told not to
// retry, or an answer the child never produced. The child writes only its
// OWN status; the SubagentRequest controller stays the single writer of the
// request's, and mirrors this across.
// InputRequest is a delegated child's mid-flight ask for DATA it was not
// given, naming one of its declared input slots.
//
// Sibling of ParentExchange, and separate from it on purpose: that one carries
// a QUESTION whose answer is the parent's words, and this one asks for a
// DATUM whose delivery is a slot binding, graded and possibly routed to a
// human. Folding them together would make "the parent replied" and "the data
// arrived" the same event, when the first can happen without the second.
//
// It does NOT pause the child. The design's own words: asking cannot widen
// anything, and the data may never arrive.
type InputRequest struct {
	// Slot is the child-side slot name it wants filled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Slot string `json:"slot"`

	// Why is the child's stated reason, in its own words. It is what the
	// parent — and any person ruling on the disclosure — actually reads.
	// +kubebuilder:validation:MaxLength=2048
	Why string `json:"why"`

	// Exchange counts this child's data requests, starting at 1. Monotonic
	// for the same reason ParentExchange's is: it is how a parent tells a new
	// request from the one it just answered, and it must survive the child's
	// pod being reaped mid-wait.
	Exchange int64 `json:"exchange"`

	// Pending is true while request number Exchange has not been answered
	// either way — filled, or refused.
	// +optional
	Pending bool `json:"pending,omitempty"`
}

type ParentExchange struct {
	// Exchange counts the questions this child has asked, starting at 1.
	// Monotonic: it is read back from this same field and incremented, so it
	// survives the pod being reaped mid-wait and is never reused. A parent
	// tells a new question from the one it just answered by this number
	// alone.
	Exchange int64 `json:"exchange"`

	// Pending is true while question number Exchange is unanswered — the one
	// fact that says the child is parked on its parent rather than working.
	//
	// Cleared wherever awaitingUserInputSince is cleared on a RESUME (the
	// loop's own resume hook, and the start of every Run, which is what makes
	// it self-healing across a reap→wake cycle), and deliberately NOT cleared
	// on the idle path: a child that parked, timed out to Idle and had its
	// pod reaped is still waiting, and that is precisely the state the whole
	// design keeps resumable.
	// +optional
	Pending bool `json:"pending,omitempty"`

	// Question is the text of question number Exchange, in the child's own
	// words. Bounded because status is not a transport for arbitrary
	// payloads; the child's runner refuses a longer one rather than
	// truncating, so what the parent reads is never a silently altered
	// question.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Question string `json:"question,omitempty"`
}

// AgentResult is what the agent reported on finishing successfully.
type AgentResult struct {
	// Summary is the agent's own account of what it did.
	Summary string `json:"summary"`
	// Artifacts are the durable outputs the agent produced.
	// +optional
	Artifacts []ResultArtifact `json:"artifacts,omitempty"`
}

type ResultArtifact struct {
	// ID is the handle an earlier tool call returned for the artifact, written
	// by the terminal tool that ended the round.
	//
	// Which FORM of handle depends on who reads it back, and the two terminal
	// tools say so in their own schemas. agent_work_complete's list is an audit
	// note nothing resolves, so any handle the agent holds is legible there.
	// return_result's is not: the SubagentRequest controller copies it to the
	// delegating parent, which attaches it by asking the API server for the
	// ArtifactRender it names — so only the `ar-…` render handle works, and an
	// artifact-store id would fail at the moment the parent tried to deliver
	// it.
	ID string `json:"id"`
	// Description is the agent's one-line account of the artifact.
	Description string `json:"description"`
}

// OpeningBadge is the status marker rendered on a triggered session's opening
// message. The concluded-outcome values equal channelkinds.TriggerOutcome so a
// verdict folds directly; in_progress/unfinished/done are framework-derived.
// +kubebuilder:validation:Enum=in_progress;clean;problems_found;could_not_finish;unfinished;done
type OpeningBadge string

const (
	OpeningBadgeInProgress     OpeningBadge = "in_progress"
	OpeningBadgeClean          OpeningBadge = "clean"
	OpeningBadgeProblemsFound  OpeningBadge = "problems_found"
	OpeningBadgeCouldNotFinish OpeningBadge = "could_not_finish"
	OpeningBadgeUnfinished     OpeningBadge = "unfinished"
	OpeningBadgeDone           OpeningBadge = "done"
)

// PinnedMessageStatus is the runner's projection of a triggered session's
// opening-message content, consumed by channelsd to edit that message in place.
// The opening TEXT is not here — it stays in AnnotationSessionOpening.
type PinnedMessageStatus struct {
	// Badge is the current status marker. The runner writes in_progress and, on
	// conclusion, the outcome; channelsd derives the terminal unfinished/done.
	// +optional
	Badge OpeningBadge `json:"badge,omitempty"`
	// Body is the agent-authored enrichment, last-write-wins; may be empty.
	// +optional
	Body string `json:"body,omitempty"`
	// Link is the delivered-result view link, set on conclusion; may be empty.
	// +optional
	Link string `json:"link,omitempty"`
}

// CompletionBypass is one recorded override of this class's completion
// requirements: what was outstanding, why the agent finished anyway, and when.
type CompletionBypass struct {
	// Time is when the bypass was recorded.
	Time metav1.Time `json:"time"`
	// Reason is the agent's stated justification. AGENT-AUTHORED and
	// UNTRUSTED — a surface showing it to a person renders it inert.
	Reason string `json:"reason"`
	// Requirements are the requirement keys that were unmet, so an operator can
	// tell "the report never went out" from "the plan was left half-finished"
	// without reading prose.
	// +optional
	// +listType=atomic
	Requirements []string `json:"requirements,omitempty"`
	// Details is the per-requirement account of what was missing, in the same
	// order as Requirements.
	// +optional
	// +listType=atomic
	Details []string `json:"details,omitempty"`
}

// RunnerNote is one timestamped diagnostic the runner recorded for operators.
type RunnerNote struct {
	Time    metav1.Time `json:"time"`
	Message string      `json:"message"`
}

// WidgetRef identifies one persisted MCP-UI interactive widget artifact,
// recorded onto AgentSessionStatus.ActiveWidgets by the runner's
// applyUIResource hook.
type WidgetRef struct {
	// ArtifactID is the artifacts.Service head ID the widget's HTML was
	// finalized under — the durable handle the browser session-view page
	// fetches by.
	ArtifactID string `json:"artifactID"`
	// Tool is the MCP tool name (tool.UIResourceSpec.Tool) that produced the
	// widget.
	// +optional
	Tool string `json:"tool,omitempty"`
	// RendererKind is the channelassets.Renderer kind that rendered the
	// widget — always "mcpui" today; carried explicitly so a future second
	// widget-renderer kind doesn't need a status-shape change.
	// +optional
	RendererKind string `json:"rendererKind,omitempty"`
	// Origin is the MCPServer CR name whose ui:// resource produced this
	// widget ("mcpserver/<name>", the string tool.OriginTool.Origin returns).
	//
	// It is the widget's IDENTITY, and it is recorded here because this is
	// where the browser can be told about it authoritatively: webd resolves a
	// shell-supplied artifact id against this list and stamps the answer onto
	// the app-tool call, so the runner can refuse a widget that reaches for a
	// DIFFERENT server's tool. The app-tool registry is one flat map across
	// every origin, so without this the tool name alone decided.
	//
	// Empty on a widget persisted before this field existed, which the pin
	// reads as "unknown origin" and therefore leaves unpinned — the same
	// posture those widgets already had.
	// +optional
	Origin string `json:"origin,omitempty"`
}

// SecretOutputCompletion maps a secret-output handle to the per-session
// Secret key (name) its value was written under. No value is stored here.
type SecretOutputCompletion struct {
	// Handle is the secret-output handle the runner captured.
	Handle string `json:"handle"`
	// SecretName is the per-session Secret key the value was written under.
	SecretName string `json:"secretName"`
	// WrittenAt is when the value landed in the Secret.
	WrittenAt *metav1.Time `json:"writtenAt,omitempty"`

	// Name is the secret-output logical name (the toolspec's
	// secretOutput.name). Used by the runner to enforce write-once
	// per name without Secret-read RBAC.
	Name string `json:"name,omitempty"`
}

// PendingRestartMode selects how the fork reconciler seeds the child's
// transcript. Empty is the default restart-from-here behavior.
const (
	// PendingRestartModeRestart (the empty-string default) is
	// restart-from-here: turns 0..CutTurnIndex are copied and the edited
	// message becomes turn CutTurnIndex+1 in the child.
	PendingRestartModeRestart = ""
	// PendingRestartModeInherit is continuation-inherit: the FULL parent
	// transcript is copied (the reconciler recomputes the cut as the
	// parent's last turn index) and the new message is appended after it.
	// Written by channelsd when a new inbound arrives for a terminal
	// session that a fresh clean retry can continue from.
	PendingRestartModeInherit = "inherit"
	// PendingRestartModeTakeover is a DIFFERENT-user continuation: someone who
	// is not the parent's started_by continues a terminal predecessor in the
	// same thread and becomes the child's owner. InheritHistory selects whether
	// the parent transcript carries over (ordinary terminal) or the child
	// starts fresh (policy/security halt). Authorization is the channelsd choke
	// point — the requester sent a legitimate app-mention into the thread, the
	// same basis on which they could start their own session — so
	// ReconcileRestart does NOT run the agentsession#fork gate for this mode.
	// The inherit and restart modes keep it.
	PendingRestartModeTakeover = "takeover"
)

// PendingRestart records an in-flight Restart-from-here trigger.
// Set on the prior (parent) AgentSession by channelsd when the
// RestartTrigger envelope is published; cleared by the AgentSession
// controller's restart reconciler once SupersededBy is populated.
type PendingRestart struct {
	// CutTurnIndex is the index of the last turn copied to the child
	// (inclusive). The user's edited message becomes turn
	// CutTurnIndex+1 in the child session. Ignored when Mode is
	// "inherit" — the reconciler recomputes the cut as the parent's
	// last turn so the whole transcript carries forward.
	CutTurnIndex int32 `json:"cutTurnIndex"`

	// Mode selects the transcript-seeding behavior: "" (restart-from-here,
	// cut at CutTurnIndex) or "inherit" (full-transcript continuation). Both
	// modes are gated on agentsession#fork and copy the parent's denied
	// tuples to the child.
	// +optional
	Mode string `json:"mode,omitempty"`

	// NewUserText is the edited message text the user submitted
	// through the channel-kind modal. Becomes the new session's
	// inbox turn at CutTurnIndex+1.
	NewUserText string `json:"newUserText"`

	// TriggeredBy is the canonical subject of the user who clicked
	// Restart (e.g., "user:alice"). Used for SpiceDB interact-perm
	// re-check by the reconciler.
	TriggeredBy identity.Subject `json:"triggeredBy"`

	// RequestedAt is when channelsd published the trigger.
	RequestedAt metav1.Time `json:"requestedAt"`

	// TargetSessionName is the deterministic name of the child
	// AgentSession the reconciler will create. Stamped here so a
	// crash-and-retry produces the same name.
	TargetSessionName string `json:"targetSessionName"`

	// NewOwnerExternalID is the channel external-id of the taking-over user
	// (takeover mode only). Stamped onto the child's
	// AnnotationStartedByExternalID so the child is owned by the new user, not
	// the parent's owner. Empty for restart/inherit modes.
	// +optional
	NewOwnerExternalID string `json:"newOwnerExternalID,omitempty"`

	// InheritHistory selects the takeover-mode transcript seeding: true copies
	// the full parent transcript (ordinary terminal), false seeds only the new
	// user's message (policy/security-halt carve-out — the halted transcript is
	// not handed to a new owner). Ignored for restart/inherit modes.
	// +optional
	InheritHistory bool `json:"inheritHistory,omitempty"`

	// Signature is channelsd's Ed25519 attestation over this marker, bound to
	// the parent session's namespace, name and UID. The operator's restart
	// reconciler verifies it before acting and refuses the restart when it is
	// absent or does not verify.
	//
	// It exists because the marker is an authorization input the session's own
	// runner can write: the runner Role grants patch on agentsessions/status,
	// Kubernetes RBAC has no field-level granularity, and takeover mode skips
	// the SpiceDB fork gate. Without an author binding, a compromised runner
	// could name any victim in triggeredBy and the operator would stamp that
	// identity onto the child session, projecting the victim's credentials into
	// the attacker's pod. See pkg/agent/restartmarker.
	// +optional
	Signature *PendingRestartSignature `json:"signature,omitempty"`
}

// PendingRestartSignature is the connector's attestation over a PendingRestart
// marker: an Ed25519 signature by a registered component publisher key, over a
// canonical digest of the marker bound to its parent session. Verified by the
// operator against the same publisher-key registry that backs append-only
// memory provenance (pkg/memory/publisherkeys).
type PendingRestartSignature struct {
	// Publisher is the provenance publisher identity that signed the marker.
	Publisher string `json:"publisher"`

	// KeyID is the content address of the signing key's public half. The
	// registry refuses to bind a keyID to a key that does not hash to it, so a
	// tampered registration cannot make an attacker's key resolve here.
	KeyID string `json:"keyId"`

	// Sig is the raw Ed25519 signature over the canonical marker digest.
	Sig []byte `json:"sig"`
}

// AgentSessionStartFailure carries the channelsd-owned start-failure signal.
// Set by channelsd when a terminal pre-start failure is detected (e.g. an
// authz-write failure on session creation). The operator reads it and drives
// the Failed state; channelsd never writes phase/failureReason/finishedAt.
type AgentSessionStartFailure struct {
	// Reason is the machine-readable failure code (mirrors the constants used
	// for AgentSession status conditions, e.g. ReasonAgentSessionAuthzWriteFail).
	Reason string `json:"reason"`
	// Message is the optional human-readable description of the failure.
	// +optional
	Message string `json:"message,omitempty"`
}

func init() {
	SchemeBuilder.Register(&AgentSession{}, &AgentSessionList{})
}

// ChannelBinding records a Channel reference attached to an AgentSession.
// The InputChannel binding records the channel-driven origin (Set once by
// channelsd at session creation; immutable thereafter; nil for
// kubectl-driven sessions). The OutputChannel binding records the outbound
// routing target when it differs from the input.
type ChannelBinding struct {
	// Name is the Channel CR name (same namespace as the AgentSession).
	Name string `json:"name"`

	// Kind denormalizes Channel.spec.kind for fast filtering without a
	// secondary lookup.
	Kind string `json:"kind"`

	// Key is the channelKey computed by the kind impl (e.g.,
	// "thread:<channel_id>:<thread_ts>" or "dm:<user_id>"). Free-form;
	// the SHA-256-hashed form is on the metadata.labels for selector lookup.
	Key string `json:"key"`

	// Capabilities lists the format capabilities advertised by the kind
	// (e.g., {"text","markdown"}). The runner consumes these to shape
	// the respond_to_user tool's JSON schema.
	Capabilities []string `json:"capabilities"`

	// NATSSubjectPrefix is the full prefix shared by all of this session's
	// NATS subjects (without trailing dot, e.g., "ap.session.default.foo").
	// Denormalized so the runner doesn't have to construct it.
	NATSSubjectPrefix string `json:"natsSubjectPrefix"`

	// External carries kind-specific opaque routing metadata that the kind's
	// Sender consumes to render outbound (e.g., Slack channel_id, thread_ts).
	// Treated as opaque by the rest of the system.
	// +optional
	External map[string]string `json:"external,omitempty"`

	// InheritFrom names the prior AgentSession (same namespace) whose memory
	// channelsd copied into this session at creation time. Set only when this
	// session was spawned because the prior session for the same Channel + key
	// was archived (Succeeded) or Failed. Diagnostic / introspection only.
	// +optional
	InheritFrom string `json:"inheritFrom,omitempty"`

	// RoutingMode controls which inbound messages reach the agent. "" (the
	// default, and all a bot-originated thread ever uses) routes every message
	// in the conversation. "mention_only" routes only @-mention events and is
	// set when the bot was summoned into a pre-existing thread: the listener
	// drops non-mention replies and the sender appends an "@-mention to reply"
	// hint.
	// +kubebuilder:validation:Enum="";mention_only
	// +optional
	RoutingMode string `json:"routingMode,omitempty"`
}

// OutboundBinding returns the ChannelBinding anything user-facing must be
// addressed to: OutputChannel when the session has one (cron / split-channel),
// otherwise InputChannel (the ordinary case, where origin and destination are
// the same Channel). Nil only for a session with no channel attachment at all.
//
// Every outbound surface must resolve through this rather than hand-rolling
// the fallback. Omitting it once cleared a status indicator against a bento
// input binding, which carries no channel_id, so the slack sender refused the
// clear and a cron session's "Working on the current step…" caption stuck
// forever. The failure mode is always that shape: correct for every
// same-channel session, broken only for cron.
func OutboundBinding(sess *AgentSession) *ChannelBinding {
	if sess == nil {
		return nil
	}
	if sess.Spec.OutputChannel != nil {
		return sess.Spec.OutputChannel
	}
	return sess.Spec.InputChannel
}

// SameChannelAs reports whether other addresses the same underlying channel as
// b. A nil other means "same as input" (OutputChannel nil defaults to the
// InputChannel). When both carry a kind-native channel id in
// External["channel_id"], that comparison wins; otherwise it falls back to
// Channel CR name equality. Used to gate channel-history reads when
// info-leakage is off (read allowed only when output == input channel).
func (b *ChannelBinding) SameChannelAs(other *ChannelBinding) bool {
	if other == nil {
		return true
	}
	if bID, oID := b.External["channel_id"], other.External["channel_id"]; bID != "" && oID != "" {
		return bID == oID
	}
	return b.Name == other.Name
}

// PermissionSurfaceEntry is one permission class this session's tools can
// reach, and the tools that reach it.
//
// Handle is the wire form permsurface mints — "perm:write:github_repo" or
// "tool:apply_workspace". It is carried as a string rather than a parsed
// struct because that is the form a human reads on an approval card and in an
// audit record, and the form a plan declares.
type PermissionSurfaceEntry struct {
	// Handle is the durable, opaque reference to the permission class.
	Handle string `json:"handle"`

	// StateImpact is the MAX severity across every tool reaching this handle.
	// Max, not per-tool: a handle one tool reads and another writes is a write
	// handle, and reporting the milder value would understate the reach.
	// +kubebuilder:validation:Enum=readonly;readwrite;external
	StateImpact string `json:"stateImpact"`

	// Tools are the LLM-facing names that reach this handle, sorted and
	// deduplicated. A tool reaching one handle through both its base
	// permission and a variant appears once — it is one route, not two.
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// GoalExecutionReference is immutable and may be created only by the operator.
type GoalExecutionReference struct {
	GoalID        string `json:"goalID"`
	OccurrenceID  string `json:"occurrenceID"`
	GoalRevision  int64  `json:"goalRevision"`
	ConsentDigest string `json:"consentDigest"`
}
