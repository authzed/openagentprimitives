// Package capability registers the meta-tool capabilities an AgentClass can
// grant. Each Capability decides — from the request (class grant / default-on)
// combined with availability (bound channel, backends in RunnerEnv) — whether
// it is active and which meta tools it contributes. Assemble is the single seam
// internal/cmd/runner and the e2e factory both drive.
package capability

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/utils/clock"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// Config is a capability's parsed per-capability sub-config. Each capability
// type-asserts its own concrete type inside Offer.
type Config any

// SkipReason records why an active capability WITHHELD a tool at runtime —
// usually every tool it would have contributed (granted, but the
// channel/backend could not satisfy it), and sometimes one of several, when the
// session's own shape makes that one tool the wrong thing to offer. Offer
// returns the tools it is still contributing alongside it; the two are
// independent, and Assemble appends whatever came back either way.
//
// Never silent: the caller logs it.
type SkipReason struct {
	Capability string
	Reason     string
}

// RunnerEnv carries the runner-side wiring capabilities need to construct their
// tools. Populated by internal/cmd/runner and, identically, by the e2e in-process
// factory. Fields are added by the tasks that need them; unused fields are the
// zero value (capabilities that don't need a field ignore it).
type RunnerEnv struct {
	GoalsCaller meta.GoalsCaller

	// AllToolsSoFar is set by Assemble immediately before invoking the
	// introspection capability's Offer, so introspection can resolve over the
	// full merged tool list (meta + non-meta) assembled so far. Every other
	// capability sees this as the zero value (nil).
	AllToolsSoFar []tool.Tool

	// NATSPublish publishes a payload to a NATS subject. nil for kubectl-driven
	// (non-channel-attached) sessions; capabilities that use it must tolerate
	// nil (mirrors internal/cmd/runner's pubFn today).
	NATSPublish func(ctx context.Context, subject string, payload []byte) error

	// EnvelopeSigner signs every envelope a meta tool publishes with the
	// session's identity key. Nil in contexts that publish unsigned (none in
	// production; the zero value keeps test fixtures compiling).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// ChannelAttached is true when the session has a bound channel (Binding is
	// non-nil), letting a capability choose a channel-aware tool variant.
	ChannelAttached bool

	// MemoryAvailable is true when a memory backend is wired for this session,
	// letting the memory capability contribute query_memory.
	MemoryAvailable bool

	// SearchAvailable is true when a search provider is present, letting the
	// memory capability additionally contribute search_memory.
	SearchAvailable bool

	// KGAvailable is true when a knowledge-graph endpoint is configured,
	// letting the knowledge capability contribute query_knowledge.
	KGAvailable bool

	// UserPreferences is the class's declared per-user preference schema
	// (AgentClass.spec.userPreferences). Empty means the class offers none,
	// letting the preferences capability withhold its tools entirely rather
	// than offer a save/read surface with nothing to save or read.
	UserPreferences []spiceboxv1alpha1.UserPreferenceSchema

	// PreferencesReader backs the preferences capability's read tool: the
	// resolved (schema + globals + this user's saved values) snapshot for the
	// CURRENT turn's author. Nil disables the capability the same way a nil
	// backend disables memory/search above.
	PreferencesReader meta.PreferencesReader

	// PreferenceSaver backs the preferences capability's save tool: publishes
	// a preference_save confirm addressed to the current turn's author and
	// blocks for the decision. Nil disables the capability.
	PreferenceSaver meta.PreferenceSaver

	// Client is the controller-runtime client used to resolve artifact
	// handles passed to respond_to_user's `attached` field. Mirrors
	// internal/cmd/runner's `c` local.
	Client client.Client

	// Artifacts resolves logical/tag attachment handles to ArtifactRender CR
	// names for respond_to_user. Mirrors internal/cmd/runner's `artifactSvc` local.
	Artifacts *artifacts.Service

	// SubjectPrefix is the NATS subject prefix for this session's channel
	// binding (ChannelBinding.NATSSubjectPrefix). Mirrors internal/cmd/runner's
	// `subjectPrefix` local.
	SubjectPrefix string

	// RenderFetch fetches a source revision's rendered bytes from the operator
	// (body + Content-Type). Backs artifact_offer_view's BundledOnly preview.
	// Mirrors internal/cmd/runner's fetchRenderBytes closure over its memory URL + token.
	RenderFetch func(ctx context.Context, ns, sess, render string) ([]byte, string, error)

	// MarkupGen is the secondary-LLM markup generator passed to the css
	// PreviewHTML path (nil-safe). Mirrors internal/cmd/runner's
	// markup.NewAnthropic(apiKey).Generate.
	MarkupGen channelassets.MarkupGenerator

	// InboundCh receives a struct{} per inbound NATS message; the
	// await_user_message tool blocks on it. Mirrors internal/cmd/runner's
	// `natsRT.inboundCh` local.
	InboundCh <-chan struct{}

	// PresenceCh receives a struct{} per ui_presence heartbeat — a viewer is
	// looking at this session's agent-defined UI. Forwarded to
	// await_user_message, which restarts its idle timer on each rather than
	// waking the loop; see meta.AwaitConfig.PresenceCh for why a runner's
	// lifetime has to answer to viewers and not only to conversation.
	PresenceCh <-chan struct{}

	// IdleTTL is how long await_user_message waits for an inbound message
	// before returning a terminal IdleExit result. Mirrors internal/cmd/runner's
	// `cfg.idleTTL` local.
	IdleTTL time.Duration

	// Clock is injectable for deterministic await_user_message tests. Nil →
	// the meta package falls back to clock.RealClock{}.
	Clock clock.Clock

	// OnAwaitYield is invoked immediately before await_user_message blocks,
	// signaling the runner has yielded to the user. Mirrors the OnYield
	// closure over internal/cmd/runner's `loopRef`.
	OnAwaitYield func(ctx context.Context)

	// OnAwaitResume is invoked only when InboundCh wakes await_user_message
	// (a real user reply). Mirrors the OnResume closure over internal/cmd/runner's
	// `loopRef`.
	OnAwaitResume func(ctx context.Context)

	// LeakageGate, when non-nil, is called before respond_to_user publishes
	// the outbound channel envelope. Mirrors internal/cmd/runner's late-bound
	// `leakageGateFn` closure (set only after the Loop exists).
	LeakageGate func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error

	// AppendSystemNote records a delivered-marker system note after
	// respond_to_user publishes. Mirrors internal/cmd/runner's `apdNote` local, built by
	// runner.AppendSystemNoteFunc.
	AppendSystemNote func(ctx context.Context, content map[string]any) error

	// NATSRequest performs a NATS request/reply: publishes payload on
	// subject and returns the reply bytes. Backs read_thread_history and
	// read_channel_history. nil for kubectl-driven sessions; mirrors
	// internal/cmd/runner's natsRequestFunc(natsRT.conn).
	NATSRequest func(ctx context.Context, subject string, payload []byte) ([]byte, error)

	// PlanGateActive reports whether the plan gate is on for this session.
	// select_phase is only offered when it is: with the gate off there is no
	// frozen plan to index into, so the tool would be a guaranteed error.
	PlanGateActive bool

	// ActivePlan returns the frozen approved plan and whether one is in force.
	// Backs select_phase's bounds check. Nil ⇒ no plan.
	ActivePlan func(ctx context.Context) (plangate.Plan, bool)

	// RecordPhaseSelection appends a phase-selection record to the
	// append-only log. The agent supplies WHICH phase; this is the runtime
	// attesting THAT the switch happened, which is what the fold replays.
	RecordPhaseSelection func(ctx context.Context, index int) error

	// RecordPhaseCompletion appends the agent's declaration that a phase
	// finished. Nil leaves complete_phase unable to persist, which it reports.
	RecordPhaseCompletion func(ctx context.Context, index int, outcome string) error

	// PhaseCompletion and PhaseEntered are folded from the log: which phases are
	// finished, and which the runtime has seen entered. Nil on either leaves the
	// corresponding check inert rather than blocking.
	PhaseCompletion func(ctx context.Context) map[int]bool
	PhaseEntered    func(ctx context.Context) map[int]bool

	// PhaseEntries is the COUNT behind PhaseEntered, which select_phase needs
	// to enforce a phase's declared max.count. Nil leaves the entry limit
	// unenforced rather than blocking.
	PhaseEntries func(ctx context.Context) map[int]int

	// FreezePhases fires when the agent declares phases: the runtime freezes
	// the ordered list, prices it, and records it. Nil when the gate is off.
	//
	// The []string is the notices for anything DROPPED — a handle that is not
	// a valid handle, or not on this session's surface. They travel back to the
	// agent on the update_plan result, because the freeze silently narrows the
	// phase and the agent is the only party that can re-declare it.
	FreezePhases func(ctx context.Context, phases []plans.Phase) ([]string, error)

	// ResolvedChannel, ResolvedSecret, ResolvedKind are the one-time
	// resolve.ForSession(rootCtx, c, &sess) result for this session's bound
	// Channel, computed once by internal/cmd/runner and shared by every
	// channel-sourced capability that needs it (mention_lookup,
	// channel_history). nil/zero when not channel-attached or when
	// ResolveErr is non-nil.
	ResolvedChannel *spiceboxv1alpha1.Channel
	ResolvedSecret  *corev1.Secret
	ResolvedKind    channelkinds.Kind

	// ResolveErr is the error from the one-time resolve.ForSession call,
	// non-nil when the bound Channel/Secret/Kind could not be loaded.
	// mention_lookup turns this into a SkipReason; channel_history quietly
	// declines (the warning is already logged once by mention_lookup's
	// Offer for the same err).
	ResolveErr error

	// SkillBodies maps each opted-in skill's canonical name to its SKILL.md
	// body, resolved once at runner startup (internal/cmd/runner's resolveSkills). The
	// skills capability injects load_skill only when this is non-empty; an
	// empty/nil map means the agent opted into no resolvable skills.
	SkillBodies map[string]string

	// ModelCapabilities is the resolved model's capability set
	// (provider.Capabilities(sess.Status.EffectiveSettings.Model.Name)),
	// letting the artifacts capability decide which modality-contributed file
	// tools/tiers apply for this turn. Zero value is an empty CapabilitySet
	// (Has always false), matching Provider.Capabilities' unknown-model
	// behavior.
	ModelCapabilities llm.CapabilitySet

	// ArtifactReader is the Tier-1 read-by-reference backend for the files
	// modality's fetch_artifact tool. nil means Tier-1 is unavailable, so no
	// Tier-1 file tool is offered (never a silent no-op tool).
	ArtifactReader modality.ArtifactReader

	// PinAttachment backs show_attachment: it marks one of this session's
	// attachments to be rendered again on the next request, exempting it from
	// the newest-N window. Late-bound through the loop reference like
	// LeakageGate, since the Loop owns the pin set and is built after tools
	// are assembled. Nil disables the tool rather than offering one that
	// cannot work.
	PinAttachment func(ctx context.Context, handle string) error

	// ViewerCanInteract reports whether the participant the agent is currently
	// answering may interact with this session. Backs show_agent_ui's call-time
	// refusal: a link the recipient cannot open is worse than no link, because
	// the agent has already told them it works.
	//
	// A func, late-bound over the Loop like LeakageGate and PinAttachment: the
	// Loop owns "who is speaking now" and is built after tools are assembled, and
	// a func-typed nil is honest where an interface field holding a typed-nil
	// pointer would not be. Nil ⇒ agent_ui_handoff declines to offer the tool,
	// with a logged skip, rather than offering one that cannot check.
	ViewerCanInteract func(ctx context.Context) (bool, error)

	// FileBridge is the provider-native bridge moving bytes between artifactstore
	// and a provider code-execution container. Always nil today, which is what
	// keeps mount_artifact from being offered (see tool.Result.ContainerUpload).
	// Only files-in would use it; files-out inlines bytes via FileDownloader,
	// because the runner has no artifactstore write access.
	FileBridge modality.Bridge

	// FileDownloader downloads a provider code-execution file by id so
	// artifact_prepare(source=container_file) can inline its bytes into the
	// ArtifactRender CR. nil unless native file handling is active; a
	// container_file request with a nil FileDownloader fails closed.
	FileDownloader meta.FileDownloader

	// NativeFileOptIn is the resolved settings opt-in for Tier-2 (provider-
	// native) file handling: sess.Status.EffectiveSettings.NativeFileHandling,
	// wired live by the runner. Default false; Tier-2 tools/instructions only
	// apply when this is true AND ModelCapabilities advertises the matching
	// capability (modality.Env.NativeActive).
	NativeFileOptIn bool

	// WorkspaceSource, when non-nil, describes the session's bound + overlaid
	// WorkspaceSource so the workspace capability can offer sync_workspace /
	// apply_workspace. Nil when no source is bound or the overlay isn't cut.
	WorkspaceSource *WorkspaceSourceRuntime

	// UIView is the agent-UI view-model handle backing update_view and
	// read_view. A CONCRETE POINTER, so a nil check means what it says —
	// nil for every session whose AgentClass references no AgentUI, which
	// is the common case. Both capabilities SKIP (logged) rather than
	// offering a tool that cannot work.
	UIView *uiview.Runtime

	// CredentialUpdateClient is the controller-runtime client the
	// credential_update capability uses to create and poll
	// CredentialUpdateRequest CRs. nil ⇒ the capability contributes no tool
	// and reports a SkipReason — never a tool that would silently fail.
	CredentialUpdateClient client.Client

	// SubagentCreate writes a new SubagentRequest CR; the delegate tool's write
	// half of its create-then-poll round trip. nil (alongside SubagentPoll) ⇒
	// the subagents capability contributes no tool and reports a SkipReason —
	// never a tool that would silently fail.
	SubagentCreate func(ctx context.Context, sr *spiceboxv1alpha1.SubagentRequest) error

	// SubagentPoll reads back a SubagentRequest CR by name; the delegate
	// tool's read half. See SubagentCreate.
	SubagentPoll func(ctx context.Context, name string) (*spiceboxv1alpha1.SubagentRequest, error)

	// SubagentSend delivers one message from this session to a delegated
	// child, on the agent-to-agent inbound path. It is reply_to_subagent's
	// write half; nil ⇒ this session has no bus (a kubectl-driven parent), and
	// the subagents capability offers reply_to_subagent anyway and lets Execute
	// refuse the call. See subagents.go's Offer for why the refusal beats a
	// SkipReason here: it reaches the model rather than only the log.
	SubagentSend func(ctx context.Context, childNS, childName, text string) error

	// SubagentResolveDataTag maps a tool_use_id from this session to the pt-tag
	// minted for that call, so `delegate`'s `inputs` can fill a child's data
	// slot without a model ever handling a tag id. See
	// meta.DelegateConfig.ResolveDataTag for why the resolution is server-side.
	//
	// nil ⇒ this session cannot hand over data; a delegation that asks to is
	// refused by name rather than sent with its slots dropped.
	SubagentResolveDataTag func(ctx context.Context, toolUseID string) (string, error)

	// SubagentBindDataSlot appends one data slot to a live delegation's SPEC,
	// backing send_input — the parent's answer to a child's request_input.
	//
	// The spec rather than a grant: this records what the parent OFFERS, and
	// the controller re-checks attenuation and grading against live state
	// before anything binds. nil ⇒ this session cannot send data onward, and
	// send_input refuses by name.
	SubagentBindDataSlot func(ctx context.Context, requestName, slot, tagID string) error

	// SubagentTimeout bounds how long ONE delegate or reply_to_subagent call
	// waits on a SubagentRequest before giving up. A zero value leaves
	// meta.NewDelegateTool to fall back to its own internal default rather
	// than polling forever.
	SubagentTimeout time.Duration

	// AskParent records a delegated child's question on its own AgentSession
	// status, returning the exchange number it landed as. It is the write
	// half of ask_parent; nil ⇒ the child has no way to reach the agent that
	// delegated to it, and the subagent_conversation capability contributes
	// nothing rather than a tool that would park the session on a question
	// nobody can see.
	AskParent func(ctx context.Context, question string) (int64, error)

	// RequestInput routes a delegated child's mid-flight request for DATA to
	// the agent that delegated to it, as a plan amendment. It is the write
	// half of request_input; nil ⇒ the tool is not offered.
	//
	// Distinct from AskParent in what it carries and in what it costs. A
	// question parks the child on an answer; this does not park at all, so it
	// is offered to a single_turn child too — one with no channel, which
	// AskParent's own reachability gate would rightly refuse.
	RequestInput func(ctx context.Context, slot, why string) error

	// MintDerivedTag mints a DERIVED pt_tag from the given source ids (its
	// readers are their intersection) STORING content under it, and returns its
	// id. Storing the content is what lets egress content-binding accept the
	// pasted region later. It backs derive_tag; nil ⇒ the tool is not offered.
	MintDerivedTag func(ctx context.Context, derivedFrom []string, content string) (string, error)

	// ResolveTagContents returns the stored content for each id (a DeriveSource
	// per id), so the Offer can hand the derivation validator the real source
	// bytes to judge the derived content against. nil disables derive_tag along
	// with MintDerivedTag/DeriveValidator.
	ResolveTagContents func(ctx context.Context, ids []string) ([]DeriveSource, error)

	// DeriveValidator judges whether a derive_tag call's derived content is a
	// valid transformation of its named sources, introducing no information not
	// present in them. It is REQUIRED for derive_tag: without it a model could
	// cite a wide source and wrap sensitive-derived content, and the platform
	// could not tell — so a nil DeriveValidator means derive_tag is NOT offered
	// (fail-closed; no unvalidated derivation). It must run as its own component
	// with a clean context, isolated from the possibly-injected session — like
	// the prompt-injection detector — so the same injection cannot steer it.
	DeriveValidator DeriveValidator

	// TagAccessCheck reports whether THIS session may hold the named tag
	// (pt_tag#access = session + ancestors + granted_to). It gates every
	// derive_tag input; nil skips the check (test/standalone construction).
	TagAccessCheck func(ctx context.Context, tagID string) (bool, error)

	// CompletionRequirements are the requirement keys this session's AgentClass
	// declared (spec.completionRequirements). Empty leaves this session's
	// terminal tool — agent_work_complete, or return_result for a delegated
	// child — ungated, exactly as it behaved before the gate existed.
	CompletionRequirements []string

	// RecordCompletionBypass records an agent's decision to finish with one of
	// those requirements unmet — onto session status and onto the channel, so a
	// person learns of it. Nil makes the gate REFUSE a bypass rather than grant
	// an unrecorded one; see meta.CompletionConfig.RecordBypass.
	RecordCompletionBypass func(ctx context.Context, b completion.Bypass) error

	// TriggerStatusAPIBaseURL overrides the provider API host the trigger-status
	// tools talk to. Empty means each kind's real default.
	//
	// The only seam that lets a test redirect those outbound calls, since the
	// kinds behind them default to the real provider. Production leaves it
	// empty. It is deliberately provider-agnostic in name but shares the
	// honest smell of pkg/controllers/channel's GitHubAPIBaseURL: one override
	// for whichever kind this session's trigger belongs to, kept because a
	// per-kind override abstraction would have exactly one consumer today.
	TriggerStatusAPIBaseURL string

	// WebdBaseURL returns webd's externally reachable base URL, or "" when
	// this cluster has none yet. It is what lets conclude_trigger_status fill
	// in its own details link — the durable, subject-independent artifact URL —
	// instead of asking a model to assemble one.
	//
	// A getter rather than a string because the value can land after the pod
	// started. Nil in a process with no webd at all (a kubectl-driven session,
	// a test fixture): the trigger is still concluded, without a link.
	WebdBaseURL func() string

	// ToolLookup resolves a tool by its LLM-facing name over the FINAL merged
	// tool table. Deliberately NOT AllToolsSoFar: Ordered() sorts
	// credential_update mid-alphabet while AllToolsSoFar is filled only just
	// before the last-sorted introspection capability's Offer, so it would be nil
	// at Offer time regardless.
	//
	// Wiring sets this to a wrapper closure forwarding to a variable populated
	// AFTER Assemble returns, the same late-binding as LeakageGate: the wrapper
	// is never nil once wired, so a capability capturing the field during Offer
	// still calls a live func at Execute time. Nil in a test-constructed
	// RunnerEnv{}, which the capability and its tool must tolerate.
	ToolLookup func(name string) (tool.Tool, bool)
}

// WorkspaceSourceRuntime is the runner-side view of a session's bound workspace
// source (populated from AgentSession.status.resolvedWorkspaceSource).
type WorkspaceSourceRuntime struct {
	Kind       string // driver kind (registry key), e.g. "git"
	Locator    string
	Revision   string // git ref/branch; "" = driver default
	WorkDir    string // overlay mount path inside the reconcile Job (/workspace)
	OverlayPVC string // the session workspace PVC the overlay was cut into
	CredSecret string // per-session Secret envFrom'd for apply (may be absent at run time)
	// ReconcileImage/ReconcileServiceAccount are the operator-resolved git image
	// and ServiceAccount the sync/apply Job runs as (from status). Empty ⇒ the
	// meta tool falls back to its built-in defaults.
	ReconcileImage          string
	ReconcileServiceAccount string
}

// ModalityEnv builds the per-turn modality.Env from the runner-supplied
// capability facts, so tool assembly (artifacts.Offer) and prompt-instruction
// assembly (internal/cmd/runner) derive it from one place.
func (re RunnerEnv) ModalityEnv() modality.Env {
	return modality.Env{
		ModelCaps:   re.ModelCapabilities,
		NativeOptIn: re.NativeFileOptIn,
		Reader:      re.ArtifactReader,
		Bridge:      re.FileBridge,
	}
}

// DeriveSource is one input to a derive_tag validation: a source tag's id and
// the content the platform stored for it.
type DeriveSource struct {
	TagID   string
	Content string
}

// DeriveValidator judges whether derived content is a faithful transformation of
// its sources — introducing no information not present in them. Implemented by a
// dedicated component (its own model + credential), separate from the session's
// LLM, so a prompt injection in the agent's context cannot also steer the judge.
type DeriveValidator interface {
	// ValidateDerivation returns whether `derived` is a valid transformation of
	// `sources`. A false verdict (or an error, which is fail-closed) causes
	// derive_tag to refuse rather than mint a too-wide tag.
	ValidateDerivation(ctx context.Context, sources []DeriveSource, derived string) (valid bool, reason string, err error)
}

// OfferContext is the input to Capability.Offer.
type OfferContext struct {
	Ctx     context.Context
	Granted bool   // capabilities map has this key
	Enabled bool   // parsed common {enabled} (default true)
	Config  Config // parsed per-capability config
	Class   *spiceboxv1alpha1.AgentClass
	Session *spiceboxv1alpha1.AgentSession
	Binding *spiceboxv1alpha1.ChannelBinding // nil when not channel-attached

	// OutBinding is the binding whose kind will RENDER this session's outbound:
	// spec.outputChannel when the session has one, otherwise Binding. Nil
	// exactly when Binding is.
	//
	// It exists because the two are NOT the same channel for a webhook- or
	// cron-spawned session, and every question about a reply has to be asked of
	// the transport that will show it. github advertises no capabilities and has
	// no live-view surface, so shaping respond_to_user or artifact_offer_view
	// from the input binding of a github→slack session describes a channel
	// nothing is ever rendered on: `attached` vanishes from the schema and the
	// live-view offer is suppressed, on a session whose reader is on Slack.
	//
	// Binding stays the INPUT binding, and capabilities that mean the input keep
	// reading it — trigger-status resolves the system that raised the work, which
	// is the input's kind always. Only outbound-shaping capabilities read this.
	//
	// Read it through OutboundBinding(), never directly: an OfferContext built
	// by hand may leave it unset, and unset means "the same channel", not "no
	// channel".
	OutBinding *spiceboxv1alpha1.ChannelBinding

	Env RunnerEnv
}

// OutboundBinding is the binding whose kind will render this session's
// outbound. It is the accessor every outbound-shaping capability reads.
//
// An unset OutBinding falls back to Binding, which is what a session with no
// separate output channel means — the same channel both ways. That fallback is
// here rather than at each call site because the alternative is a nil
// dereference in a capability, and a context assembled by hand (a test, a
// future caller) legitimately sets only the binding it cares about.
func (o OfferContext) OutboundBinding() *spiceboxv1alpha1.ChannelBinding {
	if o.OutBinding != nil {
		return o.OutBinding
	}
	return o.Binding
}

// Capability is one registered unit of meta-tool functionality.
type Capability interface {
	Name() string                                    // registry key == AgentClass capabilities map key
	DefaultOn() bool                                 // active even when the class does not list it
	Infrastructural() bool                           // always-on, cannot be disabled (core, introspection)
	ParseConfig(raw json.RawMessage) (Config, error) // validate sub-config; pure (no RunnerEnv)
	Offer(OfferContext) ([]tool.Tool, *SkipReason)   // active decided by caller; returns tools or a skip
}

// PromptSection is one titled block of system-prompt text a capability
// contributes ALONGSIDE its tools. It is emitted from the same Offer decision
// that injects the tools (see SectionOfferer), so a capability that skips
// contributes no text either — the two can never disagree about whether the
// agent has the thing the text describes. runner.ComposeSystem renders each
// one as "## Title" followed by Body, verbatim: what a section says lives with
// the capability that gates the tools it describes, never in prompt.go.
type PromptSection struct {
	Title string
	Body  string
}

// SectionOfferer is the optional widening of Capability for a capability
// whose tools need standing prompt text beside them.
//
// Assemble dispatches a capability implementing it through OfferWithSections
// INSTEAD of Offer, so one call decides tools and text together. Offer must
// still exist (the interface requires it) and should delegate to the same
// decision, dropping the sections, so a caller holding only a Capability
// sees exactly the tools Assemble would inject.
type SectionOfferer interface {
	Capability
	OfferWithSections(OfferContext) ([]tool.Tool, []PromptSection, *SkipReason)
}
