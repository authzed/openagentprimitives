package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The per-datum half of the audience check.
//
// The coarse check measures a payload against the union of everything the
// SESSION ever read, so it can only blanket-block or blanket-approve. The case
// it cannot express is the common safe one: an agent using sensitive data
// internally without telling anyone it should not. Per-datum makes that
// expressible, and makes a second egress point worth having — a tool call
// carries data OUT just as a channel message does.
//
// Everything here is additive. With FineGrained nil, or the capability off, or
// the payload not fully tag-covered, the caller falls through to the coarse
// path exactly as before. Tags may only ever REFINE an answer the coarse model
// would have blocked; they never authorize a flow it refused for a reason
// other than audience.

// FineGrainedDeps enables per-datum checking. Nil disables it entirely, which
// is the default and today's behaviour.
type FineGrainedDeps struct {
	// Enabled reports whether this session's class granted
	// fine_grained_info_leakage. Checked FIRST, before coverage: with the
	// capability off nothing mints, so coverage is "no" by construction, and
	// asking it first would make the branch depend on minting behaviour rather
	// than on the operator's choice.
	Enabled func(ctx context.Context) bool

	// TagReaders expands one pt_tag's `reader` permission to concrete
	// subjects. The intersection over a derivation tree happens IN SpiceDB, so
	// this returns a tag's fully-resolved audience, not its direct readers.
	TagReaders func(ctx context.Context, tagID string) ([]string, error)

	// DestinationAudience returns who can read the destination a tool call
	// sends data to, and whether that destination could be resolved AT ALL.
	//
	// The bool is load-bearing and separate from the error: a destination that
	// cannot be resolved has an UNKNOWN audience, which is not an empty one.
	// An empty audience is vacuously safe — nobody to leak to — so returning
	// one for "I don't know" would read as permission to send anywhere.
	DestinationAudience func(ctx context.Context, toolName string, args map[string]any) (audience []string, resolved bool, err error)

	// ParsePayloadTags returns the pt-tag ids covering a payload and whether the
	// whole payload is covered by well-formed, nonce-matched pt-untrusted
	// regions WHOSE CONTENT MATCHES what the platform stored for that id (content
	// binding). A region whose content does not match its claimed id is dropped
	// and coverage becomes false — that is what stops a model pairing a witnessed
	// wide id with fabricated content. Injected from the runner (which owns the
	// envelope format and can read the content store), so this authz-layer hook
	// depends on neither. fullyCovered=false → the coarse floor.
	ParsePayloadTags func(ctx context.Context, payload string) (ids []string, fullyCovered bool)

	// OutboundContent extracts the DATA an egress tool call sends — the string
	// args the model filled, MINUS the routing args the tool's own Check already
	// consumes (the destination id) — from the raw tool args, so the coverage
	// check runs against the content and nothing else.
	//
	// It exists because in.Tool.Args is the wire-form JSON: an MCP/sandbox tool
	// wraps its input in the {operation_id,_reason,args:{…}} envelope, and any
	// pt-untrusted markup the model carried into a content field is JSON-ESCAPED
	// there (`<pt-untrusted`), so scanning the raw args finds no regions and
	// every enveloped egress tool falls to the coarse floor. Unwrapping + field
	// selection needs the tool's schema and the envelope format, both of which
	// live in the runner, so it is injected — this authz-layer hook stays free of
	// both. Nil means "scan the raw args as-is" (the response path, and any
	// non-tool payload). Excluding the destination arg is what lets a routing
	// param like board_id stay untagged while any OTHER untagged string still
	// breaks coverage and drops to the floor — untagged data out is never a
	// silent per-datum pass.
	OutboundContent func(ctx context.Context, toolName string, rawArgs []byte) string
}

// fineGrainedOutcome is what the per-datum path decided.
type fineGrainedOutcome int

const (
	// fineGrainedNotApplicable: fall through to the coarse check. Not a pass.
	fineGrainedNotApplicable fineGrainedOutcome = iota
	fineGrainedAllow
	fineGrainedDeny
)

// payloadAudience resolves the audience a tagged payload may be shown to: the
// INTERSECTION of its covering tags' readers.
//
// Returns notApplicable when the payload is not fully tag-covered. That is the
// property the whole port rests on — a stripped or partial tag degrades to the
// coarse session-wide comparison, which over-blocks, rather than to no check.
func (h *InfoLeakAudience) payloadAudience(ctx context.Context, payload string) ([]string, fineGrainedOutcome, error) {
	if h.d.FineGrained == nil || h.d.FineGrained.TagReaders == nil || h.d.FineGrained.ParsePayloadTags == nil {
		return nil, fineGrainedNotApplicable, nil
	}
	if h.d.FineGrained.Enabled == nil || !h.d.FineGrained.Enabled(ctx) {
		return nil, fineGrainedNotApplicable, nil
	}
	// The payload is covered only by well-formed, nonce-matched pt-untrusted
	// regions — injected content cannot forge a boundary, so a tag id here is
	// one the platform really minted for the bytes it covers. Not fully covered
	// → the coarse floor.
	tags, covered := h.d.FineGrained.ParsePayloadTags(ctx, payload)
	if !covered || len(tags) == 0 {
		return nil, fineGrainedNotApplicable, nil
	}
	sets := make([][]string, 0, len(tags))
	for _, id := range tags {
		readers, rerr := h.d.FineGrained.TagReaders(ctx, id)
		if rerr != nil {
			// A tag we cannot resolve makes the payload's audience unknown.
			// Falling through to the coarse check is the safe answer; treating
			// it as an empty reader set would refuse everything, and treating
			// it as absent would drop a source from the intersection and WIDEN
			// the result.
			return nil, fineGrainedNotApplicable, fmt.Errorf("resolving tag %q: %w", id, rerr)
		}
		sets = append(sets, readers)
	}
	return provenance.IntersectAudiences(sets), fineGrainedAllow, nil
}

// checkFineGrainedResponse measures a fully-tagged reply against the channel
// audience. Returns notApplicable to fall through.
func (h *InfoLeakAudience) checkFineGrainedResponse(ctx context.Context, payload string) (pipeline.Decision, fineGrainedOutcome) {
	readers, outcome, err := h.payloadAudience(ctx, payload)
	if err != nil || outcome == fineGrainedNotApplicable {
		return pipeline.Decision{}, fineGrainedNotApplicable
	}
	if h.d.ResolveAudience == nil {
		return pipeline.Decision{}, fineGrainedNotApplicable
	}
	audience, _, aerr := h.d.ResolveAudience(ctx)
	if aerr != nil {
		// Unresolvable channel audience is the coarse path's own case, and it
		// has policy attached (OnUnsupportedChannel). Do not answer it here.
		return pipeline.Decision{}, fineGrainedNotApplicable
	}
	unauthorized := provenance.Unauthorized(audience, readers)
	if len(unauthorized) == 0 {
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind: "respond_no_leak_per_datum",
				Fields: map[string]any{
					"tagged": "true", "audienceCount": fmt.Sprint(len(audience)),
				},
			}},
		}, fineGrainedAllow
	}
	// A per-datum reply leak: the channel audience includes someone this datum's
	// readers do not. ASK APPROVAL, never hard-deny — coarse prompts on a reply
	// leak, so per-datum must not be stricter (the invariant). Mode-aware and
	// fail-closed-if-unwired via leakApproval. taint feeds the approval card.
	rec := pipeline.AuditRecord{
		Kind:   "respond_leak_per_datum",
		Fields: map[string]any{"unauthorized": strings.Join(unauthorized, ",")},
	}
	var taint []infoleakagetaint.TaintRecord
	if h.d.TaintList != nil {
		taint, _ = h.d.TaintList(ctx) // best-effort: the approval card's resource list
	}
	// fineGrainedDeny here means "the per-datum path RESOLVED this egress"
	// (the caller returns the decision instead of falling through) — the
	// resolution is now an approval request, not necessarily a denial.
	// The reply's destination is the channel, which can be asked.
	return h.leakApproval(ctx, unauthorized, taint, payload, rec, h.channelDest().MayAskAnApprover()), fineGrainedDeny
}

// evalPreToolCall is the checkpoint the coarse model could not host.
//
// A tool call carries data OUT of the session just as a reply does, and its
// audience is whoever can read the destination — once the data is there, they
// can. Under a session-wide taint set this question had no useful answer;
// with per-datum tags it does, and it is what lets an agent use a document
// internally that it may not repeat on its channel.
func (h *InfoLeakAudience) evalPreToolCall(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if h.d.FineGrained == nil || h.d.FineGrained.DestinationAudience == nil || in.Tool == nil {
		return pipeline.Decision{}
	}
	if h.d.FineGrained.Enabled == nil || !h.d.FineGrained.Enabled(ctx) {
		return pipeline.Decision{}
	}
	// The payload for coverage is the OUTBOUND CONTENT — the string args the
	// model filled, minus the routing args — not the raw wire JSON, whose
	// enveloping and escaping would hide the pt-untrusted markup and force every
	// enveloped tool to the coarse floor. OutboundContent (runner-injected) does
	// the unwrap + field selection; nil falls back to the raw args.
	payload := string(in.Tool.Args)
	if h.d.FineGrained.OutboundContent != nil {
		payload = h.d.FineGrained.OutboundContent(ctx, in.Tool.Name, in.Tool.Args)
	}
	readers, outcome, perr := h.payloadAudience(ctx, payload)
	untagged := perr != nil || outcome == fineGrainedNotApplicable

	// The destination is resolved FROM THE ARGS — the same args the tool's own
	// authorization check resolves its resource id from — so they have to be
	// parsed and handed over, not skipped.
	var args map[string]any
	if len(in.Tool.Args) > 0 {
		if err := json.Unmarshal(in.Tool.Args, &args); err != nil {
			// Args that will not parse cannot yield a destination. Unknown,
			// not empty: fall to the same refusal an unresolved destination gets.
			args = nil
		}
	}
	audience, resolved, derr := h.d.FineGrained.DestinationAudience(ctx, in.Tool.Name, args)

	if untagged {
		// A tool with NO destination carries no data OUT — a read, or a
		// Stateless/meta tool (derive_tag, respond_to_user, agent_work_complete,
		// …). This is how meta tools stay exempt without an explicit list: they
		// resolve to no destination. `derr == nil && !resolved` is exactly "not
		// an egress tool".
		if derr == nil && !resolved {
			return pipeline.Decision{}
		}
		// An egress tool sending data the model did NOT wrap in a tag: fall back
		// to the COARSE floor — the destination's audience must be able to read
		// everything the session has read, or this is a potential UNTRACKED leak.
		// This is the guardian's model: tagged → per-datum (below); untagged data
		// out of an egress tool → the session-wide floor, never a silent pass. A
		// resolution ERROR is unknown-not-empty, handled inside the floor.
		return h.coarseToolCallFloor(ctx, in.Tool.Name, audience, resolved, derr)
	}

	// Tagged data to an UNKNOWN destination is not tagged data to an empty one.
	if derr != nil || !resolved {
		rec := pipeline.AuditRecord{
			Kind:   "tool_call_destination_unresolved",
			Fields: map[string]any{"tool": in.Tool.Name, "mode": h.d.Mode},
		}
		if h.d.Mode != "enforcing" {
			return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
		}
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason: fmt.Sprintf("info-leakage: %s carries tagged data to a destination whose audience could not be determined",
				in.Tool.Name),
			Audit: []pipeline.AuditRecord{rec},
		}
	}
	if unauthorized := provenance.Unauthorized(audience, readers); len(unauthorized) > 0 {
		// A KNOWN per-datum leak: the destination can be read by someone not in
		// this datum's reader set. Ask approval, do NOT hard-deny — coarse would
		// also prompt here (coarse-permitted ⊆ this tag's readers), so a hard
		// deny would be MORE restrictive than coarse, violating the invariant.
		var taint []infoleakagetaint.TaintRecord
		if h.d.TaintList != nil {
			taint, _ = h.d.TaintList(ctx) // best-effort: feeds the approval card's resource list
		}
		rec := pipeline.AuditRecord{
			Kind:   "tool_call_leak_per_datum",
			Fields: map[string]any{"tool": in.Tool.Name, "unauthorized": strings.Join(unauthorized, ",")},
		}
		return h.leakApproval(ctx, unauthorized, taint, "tool call: "+in.Tool.Name, rec,
			h.toolCallMayAskAnApprover(in.Tool.Name))
	}
	return pipeline.Decision{
		Audit: []pipeline.AuditRecord{{
			Kind:   "tool_call_no_leak_per_datum",
			Fields: map[string]any{"tool": in.Tool.Name},
		}},
	}
}

// leakApproval turns an ACTUAL egress leak (leakedTo non-empty) — a reply or a
// tool call — into an APPROVAL request rather than a hard deny. This is the
// invariant: per-datum is never more restrictive than the coarse floor, which
// prompts on a leak. In logging mode it records only (never denies). If the
// approval builder is not wired it fails closed (an operator misconfiguration,
// never a silent allow). taint feeds the approval card's "which resources" list
// and may be nil (best-effort). proposedText is the reply text or a tool-call
// description, shown to the approver.
// mayAsk is the DESTINATION's answer to whether a leak here can be put to a
// human at all (Destination.MayAskAnApprover). false refuses instead, and that
// does not break the never-stricter-than-coarse invariant: a destination
// answering false is refused by the coarse floor too — enforceAudienceForTaint
// checks the same thing first — so both halves deny and per-datum stays no
// stricter than the floor it refines.
func (h *InfoLeakAudience) leakApproval(ctx context.Context, leakedTo []string, taint []infoleakagetaint.TaintRecord, proposedText string, rec pipeline.AuditRecord, mayAsk bool) pipeline.Decision {
	if h.d.Mode != "enforcing" {
		return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
	}
	if !mayAsk {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason: fmt.Sprintf(
				"info-leakage: this destination cannot be opened to new subjects by an approval; refusing to send, which would newly expose this data to %s",
				strings.Join(leakedTo, ", ")),
			Audit: []pipeline.AuditRecord{rec},
		}
	}
	if h.d.BuildApprovalAsk == nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: egress would leak to %s and approval is not wired", strings.Join(leakedTo, ", ")),
			Audit:   []pipeline.AuditRecord{rec},
		}
	}
	ask, aerr := h.d.BuildApprovalAsk(ctx, leakedTo, taint, proposedText)
	if aerr != nil {
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: aerr.Error(), Audit: []pipeline.AuditRecord{rec}}
	}
	return pipeline.Decision{Approval: ask, Audit: []pipeline.AuditRecord{rec}}
}

// coarseToolCallFloor is the fallback for an egress tool call whose data the
// model did NOT tag: the destination's audience must be able to read everything
// the session has read (the taint intersection), or the untagged data going out
// is a potential untracked leak. It mirrors the response-side coarse floor, on
// the tool-call destination rather than the channel.
//
// A resolution ERROR (or unresolvable destination) on an egress tool is unknown,
// not empty — deny in enforcing, record in logging — the same as the tagged path.
func (h *InfoLeakAudience) coarseToolCallFloor(ctx context.Context, toolName string, audience []string, resolved bool, derr error) pipeline.Decision {
	if derr != nil || !resolved {
		rec := pipeline.AuditRecord{
			Kind:   "tool_call_destination_unresolved",
			Fields: map[string]any{"tool": toolName, "mode": h.d.Mode},
		}
		if h.d.Mode != "enforcing" {
			return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
		}
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: %s sends data to a destination whose audience could not be determined", toolName),
			Audit:   []pipeline.AuditRecord{rec},
		}
	}
	if h.d.TaintList == nil {
		return pipeline.Decision{} // no taint tracking wired — nothing to floor against
	}
	taint, err := h.d.TaintList(ctx)
	if err != nil {
		// Fail-closed: if we cannot tell what the session read, we cannot clear
		// an untagged egress.
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: fmt.Sprintf("info-leakage: %s: could not read the session taint set: %v", toolName, err)}
	}
	if len(taint) == 0 {
		return pipeline.Decision{} // the session has read nothing — nothing to leak
	}
	permitted, err := h.computePermitted(ctx, taint)
	if err != nil {
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: fmt.Sprintf("info-leakage: %s: compute permitted: %v", toolName, err)}
	}
	unauthorized := provenance.Unauthorized(audience, permitted)
	if len(unauthorized) == 0 {
		return pipeline.Decision{Audit: []pipeline.AuditRecord{{Kind: "tool_call_no_leak_coarse", Fields: map[string]any{"tool": toolName}}}}
	}
	rec := pipeline.AuditRecord{
		Kind:   "tool_call_leak_coarse",
		Fields: map[string]any{"tool": toolName, "unauthorized": strings.Join(unauthorized, ",")},
	}
	// Untagged egress that would actually leak: ASK APPROVAL, never hard-deny
	// (the invariant — coarse prompts on a leak, so per-datum must not be
	// stricter) — unless the destination is one no approval can open, which
	// the coarse floor refuses too. taint is already in scope from TaintList.
	return h.leakApproval(ctx, unauthorized, taint, "tool call: "+toolName, rec,
		h.toolCallMayAskAnApprover(toolName))
}

// toolCallMayAskAnApprover answers Destination.MayAskAnApprover for the place a
// TOOL CALL sends data, for the legs that have a tool name and no Destination
// to ask — the per-datum path resolves an audience through a closure
// (FineGrainedDeps.DestinationAudience) rather than through the interface.
//
// A pool-write declaration is the signal, and it is the same signal the coarse
// leg dispatches on: a tool carrying one sends into a resource's memory pool,
// and a pool is refused rather than carded. Everything else — a wiki post, a
// channel message, any other egress tool — keeps today's behaviour, which is
// that a leak is a question for a human.
//
// Why it matters here and not only in the coarse leg: approving a leakage_share
// runs LeakageGrantWriter over the TAINT's resources, so a card raised on this
// path would widen who may read the resource the session read FROM — the exact
// outcome the coarse refusal prevents, arriving through the one door it did not
// cover.
func (h *InfoLeakAudience) toolCallMayAskAnApprover(toolName string) bool {
	if h.d.LookupWrites == nil {
		return true
	}
	if decl := h.d.LookupWrites(toolName); decl != nil && decl.DestinationArg != "" {
		return poolMayAskAnApprover
	}
	return true
}
