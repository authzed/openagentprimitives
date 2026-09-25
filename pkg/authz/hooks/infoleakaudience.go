package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/leakage"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// ToolWritesDecl declares that a tool writes into a resource memory pool
// named by one of its arguments.
//
// Function-free and tiny on purpose: unlike the read side, nothing here has
// to parse a result format, so the declaration is just "which argument holds
// the destination". It is still supplied by built-in wiring only, never from
// a CRD — naming a write destination decides where data LANDS, which is a
// stronger authority than naming what a result's audience is.
type ToolWritesDecl struct {
	// DestinationArg names the top-level argument carrying "<type>:<id>".
	DestinationArg string
}

// InfoLeakAudienceDeps is the dependency struct for the InfoLeakAudience hook.
type InfoLeakAudienceDeps struct {
	Mode string // "enforcing" | "logging" | "disabled"
	// LookupReads resolves the ToolReadsDecl for a tool name, used at PostToolCall
	// to determine the resource ID. May be nil when only PreResponse is needed.
	LookupReads func(toolName string) *ToolReadsDecl
	// LookupWrites resolves the ToolWritesDecl for a tool name, used at
	// PreToolCall to find the pool a call is about to write into. nil leaves
	// the pool-write gate inert, which is the shape of a host that offers no
	// pool-writing tool.
	LookupWrites func(toolName string) *ToolWritesDecl
	// ResolveAudience returns the channel audience + capability for the current session.
	ResolveAudience func(ctx context.Context) (subjects []string, cap channelkinds.Capability, err error)
	// LookupSubjects returns the canonical subjects permitted on resource#permission.
	LookupSubjects func(ctx context.Context, resource, permission string) ([]string, error)
	// TaintList returns all taint records for the session. Used at PreResponse.
	TaintList func(ctx context.Context) ([]infoleakagetaint.TaintRecord, error)
	// BuildApprovalAsk constructs the ApprovalAsk for a leakage_share gate.
	// May be nil in tests that don't exercise the approval path.
	BuildApprovalAsk func(ctx context.Context, leakedTo []string, taint []infoleakagetaint.TaintRecord, proposedText string) (*pipeline.ApprovalAsk, error)
	// OnUnsupportedChannel governs behavior when the channel kind cannot resolve audience.
	// Values: "blockBinding" | "logOnly" | "bypass". Defaults to "bypass".
	OnUnsupportedChannel string
	// SingleUserBypass, when true, bypasses leakage checks for single-user channels.
	SingleUserBypass bool

	// FineGrained enables per-datum checking. Nil disables it entirely, which
	// is the default and today's behaviour.
	FineGrained *FineGrainedDeps

	// RecordDecision / IsApproved / IsDenied back the per-session leakage
	// approve/deny set in durable memory (the infoleakage_decision kind), so a
	// decision survives a runner restart. The runner wires these to
	// pkg/memory/kinds/infoleakagedecision over the session scope. decision is
	// infoleakagedecision.DecisionApproved | DecisionDenied. nil closures make
	// the corresponding read/write a no-op (tests that don't exercise the set).
	RecordDecision func(ctx context.Context, resourceType, resourceID, decision string) error
	IsApproved     func(ctx context.Context, resourceType, resourceID string) (bool, error)
	IsDenied       func(ctx context.Context, resourceType, resourceID string) (bool, error)

	// NoticeToRequester gates the read-only FYI the "logging" case delivers to
	// the requester on every would-block event (informationLeakage.
	// loggingNoticeToRequester, default false / opt-in — see
	// InformationLeakagePolicy.ResolvedLoggingNoticeToRequester). false
	// suppresses the notice; PublishNotice performs the actual publish.
	NoticeToRequester bool
	// PublishNotice builds + publishes the info_leakage_notice interaction
	// (category categories.InfoLeakageNotice) to the requester. nil
	// makes the notice a no-op regardless of NoticeToRequester (tests that
	// don't exercise the notice path; kubectl-driven sessions with no channel
	// to notify on).
	//
	// requester is the PER-CALL principal (pipeline.Input.Requester) whose read
	// caused the would-block event — the widget viewer on a proxy-exec, the
	// session subject on the LLM path. Passing it (rather than having the
	// publisher resolve it from session state) addresses the notice to the
	// actual reader and keeps an off-loop proxy-exec from reading loop-mutated
	// state; see InfoLeakReadDeps.Requester.
	PublishNotice func(ctx context.Context, requester identity.CanonicalUserID, leakedTo []string, taint []infoleakagetaint.TaintRecord) error

	Logger *slog.Logger
}

// InfoLeakAudience is the PostToolCall + PreResponse hook that enforces the
// audience-side information-leakage policy. Ports enforceAudienceForTaint,
// EnforceAudienceGateForToolResult, and LeakageGateForRespond from leakage.go.
//
// The approved/denied decision set is backed by the durable infoleakage_decision
// memory kind (wired via InfoLeakAudienceDeps.RecordDecision/IsApproved/IsDenied),
// so a denial survives a runner restart and is shared across hook instances over
// the same session scope.
type InfoLeakAudience struct {
	d InfoLeakAudienceDeps
}

// NewInfoLeakAudience creates an InfoLeakAudience hook.
func NewInfoLeakAudience(d InfoLeakAudienceDeps) *InfoLeakAudience {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.OnUnsupportedChannel == "" {
		d.OnUnsupportedChannel = "bypass"
	}
	return &InfoLeakAudience{d: d}
}

func (h *InfoLeakAudience) Name() string { return "info_leak_audience" }

// Points includes PreToolCall for two reasons. Per-datum tracking made the
// general question answerable: a tool call carries data OUT of the session
// exactly as a reply does — its audience is whoever can read the destination —
// but under a session-wide taint set it had no useful answer, so the
// checkpoint did not exist; that half is inert unless FineGrained is wired and
// the capability is granted. A tool that DECLARES its write destination
// (ToolWritesDecl) needs no tagging to be answerable, and its half runs
// whenever such a declaration is wired.
func (h *InfoLeakAudience) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall, pipeline.PreResponse}
}

func (h *InfoLeakAudience) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if h.d.Mode == "disabled" {
		return pipeline.Decision{}
	}

	switch in.Point {
	case pipeline.PreToolCall:
		// Two independent gates share this point, and neither subsumes the
		// other. evalPoolWrite compares the SESSION'S accumulated reads against
		// the audience of the pool a call names; evalPreToolCall measures the
		// outbound payload per-datum and is inert unless FineGrained is wired.
		// Running the pool gate second — or only when the per-datum gate
		// declined to answer — would make the cross-write refusal contingent on
		// per-datum tagging being switched on, which it must never be.
		//
		// A refusal from the pool gate ends the point; on an allow its audit is
		// carried onto whatever the per-datum gate decides, so neither leg's
		// record is dropped. The Approval arm of that condition is a guard, not
		// a live branch — a pool destination answers MayAskAnApprover false and
		// so can never raise a card — but a leg that silently dropped an ask
		// would be a human removed from a decision with nothing in the logs.
		write := h.evalPoolWrite(ctx, in)
		if write.Verdict != pipeline.Allow || write.Approval != nil {
			return write
		}
		fine := h.evalPreToolCall(ctx, in)
		fine.Audit = append(write.Audit, fine.Audit...)
		return fine
	case pipeline.PostToolCall:
		return h.evalPostToolCall(ctx, in)
	case pipeline.PreResponse:
		return h.evalPreResponse(ctx, in)
	}
	return pipeline.Decision{}
}

// evalPoolWrite refuses a write into a pool whose audience is wider than what
// this session's accumulated taint permits.
//
// PRE, not post: the point is that the write must not happen. Everything the
// gate needs is known before dispatch — the destination is an argument, and
// the taint is what the session has already read.
//
// The destination is a POOL, never the channel: the subjects who would newly
// see this data are whoever holds view_memory on the resource being written
// to, which has nothing to do with who is in the conversation. That is the
// whole reason enforceAudienceForTaint takes a Destination.
//
// It REFUSES rather than prompting, and that is the pool destination's own
// answer to MayAskAnApprover rather than anything this leg decides — see
// there for why an approval would grant the wrong thing entirely.
func (h *InfoLeakAudience) evalPoolWrite(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil || h.d.LookupWrites == nil {
		return pipeline.Decision{}
	}
	decl := h.d.LookupWrites(in.Tool.Name)
	if decl == nil || decl.DestinationArg == "" {
		return pipeline.Decision{}
	}
	// resolveToolArgAsExecuted, NOT ExtractToolIDArg: the gate has to read the
	// destination the TOOL will read, and the two disagree on a payload a model
	// can compose for itself. See there — reading it the other way lets a
	// stray sibling `args` object aim this gate at one pool while the write
	// goes to another.
	raw, err := resolveToolArgAsExecuted(in.Tool.Args, decl.DestinationArg)
	if err != nil {
		return h.handleParseError(in.Tool.Name, decl.DestinationArg, fmt.Errorf("parse input: %w", err))
	}
	if raw == "" {
		// A declared write whose destination cannot be read is not a write to
		// nowhere — it is the one shape that would walk past this gate
		// untouched. Route it through the same enforcing-denies /
		// logging-audits split a malformed arg takes. Note the explicit error:
		// handleParseError dereferences its cause, so a nil here would panic
		// the gate instead of closing it.
		return h.handleParseError(in.Tool.Name, decl.DestinationArg,
			fmt.Errorf("write destination argument %q is absent or not a string", decl.DestinationArg))
	}
	// memory.ParseResourceRef, not a hand-rolled Cut: it is the SAME parser
	// record_observation's Execute and httpsrv's destinationFor use for this
	// exact argument, so a ref this gate accepts is a ref every other reader
	// of it accepts too. Before this, evalPoolWrite's own check (found, both
	// halves non-empty) was looser than ResourceScope's — "dossier:a:b" was a
	// resolvable destination here and a refusal at the tool that actually
	// writes. Safe-direction (the write still refused), but a crack in the
	// exact agreement this gate exists to hold, and it let a malformed object
	// ref reach LookupSubjects below.
	destScope, err := memory.ParseResourceRef(raw)
	if err != nil {
		// Mode-aware, like both of its neighbours: the absent-argument case
		// above and handleAudienceResolutionFailure below both route through
		// the enforcing-denies / logging-audits split. The destination string
		// is model-authored, so this is the likeliest of the three to fire, and
		// a cluster running "logging" through a rollout must not get a blocked
		// tool call out of a checkpoint that is meant to be transparent.
		return h.handleParseError(in.Tool.Name, decl.DestinationArg,
			fmt.Errorf("write destination %q is not <type>:<id>: %w", raw, err))
	}
	// ParseResourceRef only ever returns a Scope ResourceScope built, so this
	// split-back can never fail; objType/objID are exactly what evalPoolWrite
	// used to compute itself.
	objType, objID, _ := memory.ResourceRef(destScope)

	// An unwired taint reader is a REFUSAL, not an allow, and the asymmetry it
	// replaces was the one nil-dep in this leg that opened rather than closed.
	//
	// The two inputs this gate compares are the session's reads and the
	// destination's audience. poolDestination.Audience already refuses when its
	// subject lookup is unwired ("the audience of %s is unknown"); an unwired
	// TaintList is the same fact about the other half — what this session has
	// read is unknown, not empty — and "unknown" answered as "nothing" allows
	// every cross-resource write the gate exists to stop. The runner wires
	// TaintMemoryList unconditionally, so this is unreachable there; that makes
	// it a misconfiguration, which is exactly the class the neighbouring
	// handlers (handleParseError, handleAudienceResolutionFailure) already
	// refuse rather than wave through.
	//
	// Routed through the same enforcing-denies / logging-audits split as those
	// neighbours, so a cluster running "logging" through a rollout still does
	// not get a blocked tool call out of a transparent checkpoint.
	if h.d.TaintList == nil {
		return h.handleParseError(in.Tool.Name, decl.DestinationArg,
			fmt.Errorf("no taint reader wired; what this session has already read is unknown, so a write to %s cannot be judged", destScope.ID))
	}
	taint, err := h.d.TaintList(ctx)
	if err != nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: taint list: %v", err),
		}
	}
	// NO filterApproved here, and that is the difference between this leg and
	// every channel one.
	//
	// The decision set is keyed by (resourceType, resourceID) alone — it
	// carries no destination — and filterApproved DROPS an approved record from
	// the comparison rather than re-resolving it. On the channel leg that is
	// fine: the approval was about disclosing that resource to an audience, and
	// the same audience is being measured again. Here it would mean an owner's
	// "yes, tell the channel about dossier:d-1" also cleared a write of
	// dossier-derived data into an unrelated resource's pool, which nobody
	// asked and nobody granted — the infoleakage_grant an approval writes is
	// not consulted by view_memory, so the pool's live audience would not have
	// admitted those subjects either. It is a one-way leak of authority, since
	// a pool write can never ADD to the approved set (it never prompts), and it
	// would be SILENT: allowed with no audit at all.
	//
	// The spec accepts the cost this creates in as many words: "in a session
	// touching two customers, writes after the first cross-read are refused,
	// including innocent ones."

	// A session that has read nothing constrains nothing. Short-circuit
	// BEFORE computePermitted, exactly as evalPreResponse does: an empty
	// taint set must not be read as an empty permitted set, which would make
	// leakedTo the whole audience and refuse every first write.
	if len(taint) == 0 {
		return pipeline.Decision{}
	}

	return h.enforceAudienceForTaint(ctx, in.Requester, taint, "",
		PoolDestination(h.d.LookupSubjects, objType, objID))
}

// evalPostToolCall mirrors EnforceAudienceGateForToolResult: resolve the
// resource ID, build a synthetic taint record for the just-executed tool, and
// run the audience gate.
func (h *InfoLeakAudience) evalPostToolCall(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil || h.d.LookupReads == nil {
		return pipeline.Decision{}
	}

	// Errored results carry no resource to scope-check / taint / gate — and
	// (post toolguard) the post pipeline now runs for failures too. Skip.
	if in.Point == pipeline.PostToolCall && in.Tool != nil && in.Tool.IsError {
		return pipeline.Decision{}
	}

	decl := h.d.LookupReads(in.Tool.Name)
	if decl == nil {
		return pipeline.Decision{}
	}

	// A plural declaration names the resources the RESULT came from — one
	// memory search reads the session's own scope plus every resource pool it
	// reached through a slot grant. Gate the result against each pool's own
	// audience; a single ResourceType could not name them, and skipping them
	// here would leave this gate blind to exactly the tools info_leak_read is
	// tagging.
	if decl.ResultResources != nil {
		return h.evalResultResources(ctx, in, decl)
	}

	if decl.ResourceType == "" {
		return pipeline.Decision{}
	}

	var resID string
	switch {
	case decl.IDArg != "":
		v, err := ExtractToolIDArg(in.Tool.Args, decl.IDArg)
		if err != nil {
			// Malformed (LLM-supplied) args must not silently skip the
			// audience taint record. In enforcing mode this is a hard deny
			// (mirror infoleakread.handleMissingArg); a nil-err empty value
			// below stays a legit skip.
			return h.handleParseError(in.Tool.Name, decl.IDArg, fmt.Errorf("parse input: %w", err))
		}
		if v == "" {
			return pipeline.Decision{}
		}
		resID = v
	case decl.ResultIDField != "":
		v, err := ExtractToolResultID(in.Tool.Result, decl.ResultIDField)
		if err != nil {
			return h.handleParseError(in.Tool.Name, decl.ResultIDField, fmt.Errorf("parse result: %w", err))
		}
		if v == "" {
			return pipeline.Decision{}
		}
		resID = v
	default:
		return pipeline.Decision{}
	}

	rec := infoleakagetaint.TaintRecord{
		ToolUseID:    in.Tool.UseID,
		AccessedAt:   time.Now().UTC(),
		ResourceType: decl.ResourceType,
		ResourceID:   resID,
		Permission:   decl.Permission,
		ToolName:     in.Tool.Name,
	}
	// An Allow here is NOT an approval and must not be recorded as one. Allow is
	// the zero Verdict, so it also covers "no member of the audience I resolved a
	// moment ago lacked permission", a capability bypass, and logging mode's
	// auto-allowed would-block — none of which any approver decided. Recording
	// those into the durable decision set would durably exempt the resource from
	// the respond-time gate (filterApproved drops it), so an audience that GROWS
	// between the read and the reply — the exact leak this gate exists to catch —
	// would reach a new member unprompted. The respond-time gate re-resolves the
	// live audience instead; a genuine approval is recorded by the host via
	// RecordApproved once the executor's ask resolves.
	return h.enforceAudienceForChannel(ctx, in.Requester, []infoleakagetaint.TaintRecord{rec}, "")
}

// evalResultResources runs the same-turn audience gate over a result that came
// from SEVERAL resources, one synthetic taint record each.
//
// It shares resolveResultReads with info_leak_read rather than parsing the
// result itself, so the hook that TAGS a call and the hook that GATES it cannot
// come to different conclusions about what the call read.
//
// A result the declaration cannot attribute routes through the same
// enforcing-denies / logging-audits split a malformed id field takes: an
// unreadable result must not be a silent skip of the audience gate, which is
// the one thing standing between a pool's entries and a channel member outside
// that pool's audience.
func (h *InfoLeakAudience) evalResultResources(ctx context.Context, in pipeline.Input, decl *ToolReadsDecl) pipeline.Decision {
	refs, err := resolveResultReads(decl, in.Tool.Result)
	if err != nil {
		return h.handleParseError(in.Tool.Name, "(result resources)", err)
	}
	if len(refs) == 0 {
		// Nothing resource-scoped in this result; the session's own entries are
		// the coarse floor's business, and it runs at respond time.
		return pipeline.Decision{}
	}
	now := time.Now().UTC()
	recs := make([]infoleakagetaint.TaintRecord, 0, len(refs))
	for _, r := range refs {
		recs = append(recs, infoleakagetaint.TaintRecord{
			ToolUseID:    in.Tool.UseID,
			AccessedAt:   now,
			ResourceType: r.Type,
			ResourceID:   r.ID,
			Permission:   r.Permission,
			ToolName:     in.Tool.Name,
		})
	}
	// Synthetic, exactly as the single-resource path's record is: these gate
	// THIS result and are never persisted. info_leak_read owns the durable
	// taint, and it recorded the same resources a moment ago.
	return h.enforceAudienceForChannel(ctx, in.Requester, recs, "")
}

// handleParseError is this hook's answer to a field it needed and could not
// read. It serves BOTH points:
//
//   - at PostToolCall, a malformed-JSON failure resolving the resource id;
//   - at PreToolCall, every way the pool-write leg can fail to establish what
//     it is judging — an unreadable or absent destination argument, a
//     destination that is not "<type>:<id>", and an unwired taint reader.
//
// Mirrors the enforcing-vs-logging distinction in
// infoleakread.handleMissingArg: in enforcing mode an unparseable
// (prompt-injectable) arg/result — or a dependency that cannot answer — is a
// hard deny so the audience comparison can't be silently skipped; in logging
// mode it is audited and allowed through.
func (h *InfoLeakAudience) handleParseError(toolName, field string, cause error) pipeline.Decision {
	switch h.d.Mode {
	case "enforcing":
		h.d.Logger.Info("info-leakage: tool field unparseable; failing closed",
			"tool", toolName, "field", field, "err", cause.Error())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: tool %q: field %q unparseable; refusing to share: %v", toolName, field, cause),
		}
	case "logging":
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "audience_taint_skipped_unparseable",
				Fields: map[string]any{"tool": toolName, "field": field, "err": cause.Error()},
			}},
		}
	}
	return pipeline.Decision{}
}

// evalPreResponse mirrors LeakageGateForRespond: list all taint, filter
// already-approved records, then run the audience gate over the remainder.
func (h *InfoLeakAudience) evalPreResponse(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if h.d.TaintList == nil {
		// No taint list: can still handle capability-bypass audits but nothing to gate.
		return h.capabilityBypassCheck(ctx, nil)
	}
	taint, err := h.d.TaintList(ctx)
	if err != nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: taint list: %v", err),
		}
	}

	// Filter approved taint so the respond-time gate doesn't re-prompt.
	taint = h.filterApproved(ctx, taint)

	if len(taint) == 0 {
		return h.capabilityBypassCheck(ctx, nil)
	}

	var proposedText string
	if in.Response != nil {
		proposedText = in.Response.Text
	}

	// Per-datum first, and only when the payload is FULLY tag-covered. It can
	// permit a reply the coarse set would have blocked — that is the whole
	// point — but never the reverse: anything it declines to answer falls
	// through to the session-wide comparison below, unchanged.
	//
	// "Fully covered" is a claim about the TEXT, and it is only meaningful
	// when the text is all that is being sent. A reply carrying attachments
	// sends bytes this check never looks at — and nothing else looks at them
	// either, since the artifact-preparation path has no leakage hook — so a
	// fully-tagged sentence would otherwise license an untagged file
	// alongside it. Skip the refinement and let the coarse floor decide,
	// which is exactly what happens in coarse mode.
	attachmentsUnmeasured := in.Response != nil && in.Response.HasAttachments
	if !attachmentsUnmeasured {
		if dec, outcome := h.checkFineGrainedResponse(ctx, proposedText); outcome != fineGrainedNotApplicable {
			return dec
		}
	}

	return h.enforceAudienceForChannel(ctx, in.Requester, taint, proposedText)
}

// capabilityBypassCheck handles the zero-taint case: still resolve audience for
// capability-bypass audits, then emit respond_no_leak.
func (h *InfoLeakAudience) capabilityBypassCheck(ctx context.Context, taint []infoleakagetaint.TaintRecord) pipeline.Decision {
	if h.d.ResolveAudience == nil {
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "respond_no_leak",
				Fields: map[string]any{"taintCount": "0", "permittedCount": "n/a"},
			}},
		}
	}
	_, capability, aerr := h.d.ResolveAudience(ctx)
	if aerr != nil {
		return h.handleAudienceResolutionFailure(ctx, aerr)
	}
	bypassAudit := h.capabilityBypassAudit(capability)
	if bypassAudit != nil {
		return pipeline.Decision{Audit: []pipeline.AuditRecord{*bypassAudit}}
	}
	return pipeline.Decision{
		Audit: []pipeline.AuditRecord{{
			Kind:   "respond_no_leak",
			Fields: map[string]any{"taintCount": "0", "permittedCount": "n/a"},
		}},
	}
}

// capabilityBypassAudit returns an AuditRecord if the capability warrants a
// bypass, or nil if the capability does not trigger a bypass.
func (h *InfoLeakAudience) capabilityBypassAudit(capability channelkinds.Capability) *pipeline.AuditRecord {
	switch capability {
	case channelkinds.CapabilityUnsupported:
		switch h.d.OnUnsupportedChannel {
		case "logOnly":
			return &pipeline.AuditRecord{Kind: "unsupported_channel_logOnly"}
		case "bypass":
			return &pipeline.AuditRecord{
				Kind:   "respond_capability_bypass",
				Fields: map[string]any{"capability": "unsupported", "disposition": "bypass"},
			}
		}
	case channelkinds.CapabilitySingleUser:
		if h.d.SingleUserBypass {
			return &pipeline.AuditRecord{
				Kind:   "respond_capability_bypass",
				Fields: map[string]any{"capability": "singleUser", "disposition": "bypass"},
			}
		}
	}
	return nil
}

// channelDest is the destination every channel-bound leg uses. Keeping it a
// method rather than a field means each evaluation gets a fresh destination,
// so the memoized audience never outlives one gate run. With no resolver
// wired it yields a destination that REFUSES — the audience is unknown, not
// empty — which is why enforceAudienceForChannel, not this, carries the
// no-resolver bypass.
func (h *InfoLeakAudience) channelDest() Destination {
	return &channelDestination{resolve: h.d.ResolveAudience}
}

// enforceAudienceForChannel runs the audience gate against the session's
// channel, and owns the no-resolver bypass.
//
// That bypass ("no audience resolver — no gate possible; allow") belongs to
// the CHANNEL and to nothing else, which is why it sits here rather than one
// level down in the shared gate. In the shared gate it would short-circuit a
// POOL destination too, so every session with no channel binding — every
// kubectl-driven one — would have its cross-pool write gate silently skipped:
// fail-open in the gate carrying the whole cross-contamination refusal.
func (h *InfoLeakAudience) enforceAudienceForChannel(ctx context.Context, requester identity.CanonicalUserID, taint []infoleakagetaint.TaintRecord, proposedText string) pipeline.Decision {
	if h.d.ResolveAudience == nil {
		return pipeline.Decision{}
	}
	return h.enforceAudienceForTaint(ctx, requester, taint, proposedText, h.channelDest())
}

// enforceAudienceForTaint is the core audience gate, ported from leakage.go.
//
// requester is the PER-CALL principal (pipeline.Input.Requester) — the widget
// VIEWER on a proxy-exec, the session subject on the LLM path. It is threaded
// in (rather than resolved from session state inside PublishNotice) so the
// logging-mode notice addresses whoever actually made the read, and so a
// proxy-exec — which runs off the loop goroutine — never reads loop-mutated
// state. See InfoLeakReadDeps.Requester for the same rule on the read gate.
//
// It deliberately carries NO no-resolver bypass of its own: see
// enforceAudienceForChannel, which owns that one, for why leaving it here
// would fail open on every pool destination.
func (h *InfoLeakAudience) enforceAudienceForTaint(ctx context.Context, requester identity.CanonicalUserID, taint []infoleakagetaint.TaintRecord, proposedText string, dest Destination) pipeline.Decision {
	audience, err := dest.Audience(ctx)
	if err != nil {
		return h.handleAudienceResolutionFailure(ctx, err)
	}
	capability := dest.Capability()

	// Capability-bypass check.
	if bypassAudit := h.capabilityBypassAudit(capability); bypassAudit != nil {
		switch capability {
		case channelkinds.CapabilityUnsupported:
			switch h.d.OnUnsupportedChannel {
			case "blockBinding":
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					Reason:  "info-leakage: channel kind does not support audience resolution",
				}
			}
			return pipeline.Decision{Audit: []pipeline.AuditRecord{*bypassAudit}}
		case channelkinds.CapabilitySingleUser:
			if h.d.SingleUserBypass {
				return pipeline.Decision{Audit: []pipeline.AuditRecord{*bypassAudit}}
			}
			// Falls through to permitted check.
		}
	}

	// Compute permitted.
	if h.d.LookupSubjects == nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "info-leakage: LookupSubjects not wired",
		}
	}
	permitted, err := h.computePermitted(ctx, taint)
	if err != nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: compute permitted: %v", err),
		}
	}

	leakedTo := subtractSubjects(audience, permitted)
	if len(leakedTo) == 0 {
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind: "respond_no_leak",
				Fields: map[string]any{
					"taintCount":     fmt.Sprintf("%d", len(taint)),
					"permittedCount": fmt.Sprintf("%d", len(permitted)),
				},
			}},
		}
	}

	// Filter causative taint.
	causativeTaint, filterErr := h.filterTaintCausedByAudience(ctx, taint, leakedTo)
	if filterErr != nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: filter causative taint: %v", filterErr),
		}
	}
	if len(causativeTaint) == 0 {
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind: "respond_no_leak",
				Fields: map[string]any{
					"taintCount":     fmt.Sprintf("%d", len(taint)),
					"permittedCount": fmt.Sprintf("%d", len(permitted)),
				},
			}},
		}
	}

	switch h.d.Mode {
	case "enforcing":
		// A destination no approval can open is refused here, before the
		// decision store is touched and before any card is built — see
		// Destination.MayAskAnApprover. FIRST, not as a fallback when
		// BuildApprovalAsk happens to be nil: a pool write must deny on a
		// cluster where the approval path is fully wired, which is every real
		// one. Nothing below could change the verdict for such a destination,
		// only the wording, and the re-prompt bookkeeping is meaningless for a
		// gate that never prompts.
		if !dest.MayAskAnApprover() {
			return refuseTheShare(dest, leakedTo, causativeTaint,
				"this destination cannot be opened to new subjects by an approval")
		}
		// If any causative resource was already denied this session, do NOT re-prompt.
		for _, t := range causativeTaint {
			denied, derr := h.isDenied(ctx, t.ResourceType, t.ResourceID)
			if derr != nil {
				// Fail closed: the decision store is unreadable; refuse to share.
				h.d.Logger.Info("info-leakage: decision store read failed; failing closed",
					"resource", t.ResourceType+":"+t.ResourceID, "err", derr.Error())
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					Reason:  fmt.Sprintf("info-leakage: decision store unavailable; refusing to share %s:%s", t.ResourceType, t.ResourceID),
				}
			}
			if denied {
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					// The "ErrShareDenied" literal is a stable sentinel: the runner's
					// PreResponse adapter finds it by strings.Contains to map this Deny
					// back to leakage.ErrShareDenied (→ Terminal/IdleExit in
					// respond_to_user). Do NOT change this string without updating that
					// adapter.
					Reason: fmt.Sprintf(
						"info-leakage: sharing this data was already denied for this audience; not re-requesting approval [ErrShareDenied]: %s",
						leakage.ErrShareDenied.Error(),
					),
					Audit: []pipeline.AuditRecord{{
						Kind: "leakage_denied",
						Fields: map[string]any{
							"leakedTo":  leakedTo,
							"resources": taintToResourceRefs(causativeTaint),
							"note":      "previously denied this session; not re-prompting",
						},
					}},
				}
			}
		}
		// Request approval.
		if h.d.BuildApprovalAsk != nil {
			ask, aerr := h.d.BuildApprovalAsk(ctx, leakedTo, causativeTaint, proposedText)
			if aerr != nil {
				return pipeline.Decision{Verdict: pipeline.Deny, Reason: aerr.Error()}
			}
			return pipeline.Decision{
				Approval: ask,
				Audit: []pipeline.AuditRecord{{
					Kind: "leakage_detected",
					Fields: map[string]any{
						"leakedTo":  leakedTo,
						"resources": taintToResourceRefs(causativeTaint),
					},
				}},
			}
		}
		// BuildApprovalAsk not wired — fail closed.
		return refuseTheShare(dest, leakedTo, causativeTaint, "approval not wired")
	case "logging":
		// Notice-only: logging mode never blocks — the leak is auto-allowed —
		// so instead of pausing for an owner's approval this delivers a
		// read-only FYI to the requester. Best-effort: a publish failure must
		// not turn an already-allowed response into a denial, but IS logged,
		// since the requester is expecting it.
		if h.d.NoticeToRequester && h.d.PublishNotice != nil {
			if perr := h.d.PublishNotice(ctx, requester, leakedTo, causativeTaint); perr != nil {
				h.d.Logger.Info("info-leakage: notice publish failed",
					"leakedTo", leakedTo, "err", perr.Error())
			}
		}
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "would_block_leakage",
				Fields: map[string]any{"leakedTo": leakedTo},
			}},
		}
	}
	return pipeline.Decision{}
}

// RecordDenied marks a (resourceType, resourceID) tuple as denied for this
// session so subsequent audience gates don't re-prompt the approver. Persisted
// in the durable infoleakage_decision memory kind. Called by the runner host's
// AwaitDecision on approval denial. The public signature stays ctx-free to keep
// the host's on_denied callback wiring stable; a background context drives the
// store write and any error is logged (no-silent-errors).
func (h *InfoLeakAudience) RecordDenied(resourceType, resourceID string) {
	h.recordDecisionCtx(context.Background(), resourceType, resourceID, infoleakagedecision.DecisionDenied)
}

// RecordApproved marks a (resourceType, resourceID) tuple as approved for
// this session. Called after a successful leakage_share approval.
func (h *InfoLeakAudience) RecordApproved(resourceType, resourceID string) {
	h.recordDecisionCtx(context.Background(), resourceType, resourceID, infoleakagedecision.DecisionApproved)
}

// RecordDeniedForTest is a test-only helper to seed the denied set.
func (h *InfoLeakAudience) RecordDeniedForTest(resourceType, resourceID string) {
	h.RecordDenied(resourceType, resourceID)
}

// recordDecisionCtx persists one decision via the store closure. A nil store is
// a no-op; a store error is logged (no-silent-errors) — the caller already has
// a fail-closed default for the read side.
func (h *InfoLeakAudience) recordDecisionCtx(ctx context.Context, resourceType, resourceID, decision string) {
	if h.d.RecordDecision == nil {
		return
	}
	if err := h.d.RecordDecision(ctx, resourceType, resourceID, decision); err != nil {
		h.d.Logger.Info("info-leakage: record decision failed",
			"resource", resourceType+":"+resourceID, "decision", decision, "err", err.Error())
	}
}

// isDenied reports whether a resource was denied this session. A nil store
// reader returns (false, nil).
func (h *InfoLeakAudience) isDenied(ctx context.Context, resourceType, resourceID string) (bool, error) {
	if h.d.IsDenied == nil {
		return false, nil
	}
	return h.d.IsDenied(ctx, resourceType, resourceID)
}

// isApproved reports whether a resource was approved this session. A nil store
// reader returns (false, nil).
func (h *InfoLeakAudience) isApproved(ctx context.Context, resourceType, resourceID string) (bool, error) {
	if h.d.IsApproved == nil {
		return false, nil
	}
	return h.d.IsApproved(ctx, resourceType, resourceID)
}

// filterApproved removes taint records an approver has already granted this
// session (recorded via RecordApproved when the executor's leakage_share ask
// resolves), so the respond-time gate does not re-prompt for them. Only a real
// approval belongs in that set — see evalPostToolCall on why an Allow verdict
// must not be recorded as one. A store read error is logged and the record is
// KEPT (fail-closed: an unreadable store must not silently drop the gate).
func (h *InfoLeakAudience) filterApproved(ctx context.Context, taint []infoleakagetaint.TaintRecord) []infoleakagetaint.TaintRecord {
	if len(taint) == 0 {
		return taint
	}
	out := make([]infoleakagetaint.TaintRecord, 0, len(taint))
	for _, t := range taint {
		approved, err := h.isApproved(ctx, t.ResourceType, t.ResourceID)
		if err != nil {
			h.d.Logger.Info("info-leakage: decision store read failed in filterApproved; keeping record (fail-closed)",
				"resource", t.ResourceType+":"+t.ResourceID, "err", err.Error())
			out = append(out, t)
			continue
		}
		if approved {
			continue
		}
		out = append(out, t)
	}
	return out
}

// computePermitted returns the intersection of LookupSubjects results across
// all taint records. Ported from leakage.go.
func (h *InfoLeakAudience) computePermitted(ctx context.Context, taint []infoleakagetaint.TaintRecord) ([]string, error) {
	var permitted map[string]struct{}
	for _, t := range taint {
		resource := t.ResourceType + ":" + t.ResourceID
		subjects, err := h.d.LookupSubjects(ctx, resource, t.Permission)
		if err != nil {
			return nil, fmt.Errorf("info-leakage: lookup %s#%s: %w", resource, t.Permission, err)
		}
		set := make(map[string]struct{}, len(subjects))
		for _, s := range subjects {
			set[s] = struct{}{}
		}
		if permitted == nil {
			permitted = set
			continue
		}
		for k := range permitted {
			if _, ok := set[k]; !ok {
				delete(permitted, k)
			}
		}
	}
	out := make([]string, 0, len(permitted))
	for k := range permitted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// filterTaintCausedByAudience returns taint records for which at least one
// leakedTo member is not yet permitted on that specific resource. Ported from
// leakage.go.
func (h *InfoLeakAudience) filterTaintCausedByAudience(ctx context.Context, taint []infoleakagetaint.TaintRecord, leakedTo []string) ([]infoleakagetaint.TaintRecord, error) {
	if len(leakedTo) == 0 || h.d.LookupSubjects == nil {
		return taint, nil
	}
	leakedSet := make(map[string]struct{}, len(leakedTo))
	for _, s := range leakedTo {
		leakedSet[s] = struct{}{}
	}
	out := make([]infoleakagetaint.TaintRecord, 0, len(taint))
	for _, t := range taint {
		resource := t.ResourceType + ":" + t.ResourceID
		subjects, err := h.d.LookupSubjects(ctx, resource, t.Permission)
		if err != nil {
			return nil, fmt.Errorf("info-leakage: lookup %s#%s: %w", resource, t.Permission, err)
		}
		permittedSet := make(map[string]struct{}, len(subjects))
		for _, s := range subjects {
			permittedSet[s] = struct{}{}
		}
		causative := false
		for leaked := range leakedSet {
			if _, ok := permittedSet[leaked]; !ok {
				causative = true
				break
			}
		}
		if causative {
			out = append(out, t)
		}
	}
	return out, nil
}

// refuseTheShare is the enforcing-mode refusal that does NOT raise a card:
// either the destination never escalates, or nothing is wired to escalate to.
//
// The reason names the destination and every subject that would newly see the
// data, because it is the whole of what the caller gets. "cannot enforce"
// alone says neither what was being sent nor where it was going, which is a
// refusal an operator cannot act on. It emits the same leakage_detected record
// the approval path does — a leak WAS detected either way, and the Verdict is
// what distinguishes refusing from asking.
func refuseTheShare(dest Destination, leakedTo []string, causativeTaint []infoleakagetaint.TaintRecord, why string) pipeline.Decision {
	return pipeline.Decision{
		Verdict: pipeline.Deny,
		Reason: fmt.Sprintf(
			"info-leakage: %s; refusing to send to %s, which would newly expose this data to %s",
			why, dest.Describe(), strings.Join(leakedTo, ", ")),
		Audit: []pipeline.AuditRecord{{
			Kind: "leakage_detected",
			Fields: map[string]any{
				"leakedTo":  leakedTo,
				"resources": taintToResourceRefs(causativeTaint),
			},
		}},
	}
}

// subtractSubjects returns audience members not in permitted.
func subtractSubjects(audience, permitted []string) []string {
	p := make(map[string]struct{}, len(permitted))
	for _, s := range permitted {
		p[s] = struct{}{}
	}
	out := make([]string, 0, len(audience))
	for _, s := range audience {
		if _, ok := p[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// taintToResourceRefs converts taint records to ResourceRefs for audit records.
func taintToResourceRefs(taint []infoleakagetaint.TaintRecord) []infoleakageaudit.ResourceRef {
	out := make([]infoleakageaudit.ResourceRef, 0, len(taint))
	for _, t := range taint {
		out = append(out, infoleakageaudit.ResourceRef{Type: t.ResourceType, ID: t.ResourceID})
	}
	return out
}

func (h *InfoLeakAudience) handleAudienceResolutionFailure(_ context.Context, cause error) pipeline.Decision {
	switch h.d.Mode {
	case "enforcing":
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: audience resolution: %v", cause),
		}
	case "logging":
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "audience_resolution_failed",
				Fields: map[string]any{"err": cause.Error()},
			}},
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*InfoLeakAudience)(nil)
