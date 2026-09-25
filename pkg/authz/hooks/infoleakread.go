package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// InfoLeakReadDeps is the dependency struct for the InfoLeakRead hook.
type InfoLeakReadDeps struct {
	Mode string // "enforcing" | "logging" | "disabled"
	// SessionRef is "<namespace>/<name>" for the current session. Used to build
	// the coarse-floor taint for an UNDECLARED non-meta tool, whose audience is
	// the session's own participants (agentsession#unknown_provenance). Empty
	// means the floor cannot be built: handleUnmappedTool then fails closed in
	// enforcing rather than silently letting undeclared data out.
	SessionRef  string
	LookupReads func(toolName string) *ToolReadsDecl
	// Requester maps the PER-CALL principal (pipeline.Input.Requester, passed
	// as perCall) to the SpiceDB subject reference the view check runs against.
	// It takes the per-call value rather than reading session/loop state for
	// two reasons, both load-bearing:
	//
	//   - Attribution: on a proxy-exec (an MCP-UI widget's app-tool call) the
	//     acting principal is the widget VIEWER, not the session's current
	//     requester (D-D1). Resolving from loop state authorized a viewer's
	//     read against whoever last messaged the session.
	//   - Concurrency: a proxy-exec runs on the NATS-handler goroutine (readonly
	//     auto-run) or a detached approval goroutine, both concurrent with the
	//     loop goroutine's advanceRequester writes. Reading loop state here was
	//     an unsynchronized read of a field the loop mutates between turns.
	//
	// nil ⇒ no requester resolver wired; the view check is skipped and the read
	// is audited as read_unchecked.
	Requester   func(ctx context.Context, perCall identity.CanonicalUserID) (string, error)
	Check       func(ctx context.Context, subject, permission, resource string) (bool, error)
	AppendTaint func(ctx context.Context, rec infoleakagetaint.TaintRecord) error

	// MintPtTag records the PER-DATUM provenance tag for this read, beside the
	// session-wide taint record above. Nil disables per-datum provenance, which
	// is every deployment that has not enabled the mint route.
	//
	// This is the boundary the design names: minting happens at PostToolCall,
	// from the resources a call accessed — the same inputs already gathered for
	// TaintRecord — and NOT from the entry-appended signal, where a hook
	// writing a memory entry in reaction to a memory entry deadlocks on
	// SigningMemory's non-reentrant per-scope mutex.
	//
	// The request names RESOURCES and never an audience. The reader set is
	// derived server-side by the operator's minter, because a tag's
	// direct_reader GRANTS disclosure and a session able to author one could
	// name an audience its source never authorized.
	MintPtTag func(ctx context.Context, req memory.PtTagMintRequest) (string, error)

	// UntrustedSource answers the INTEGRITY axis of the tag this call mints:
	// may the result carry content the agent must not treat as instructions?
	//
	// Read from the tool's own SEP-1913 `returnMetadata.source` declaration,
	// which the MCP validator already consumes for `deny.trust`. The two are
	// deliberately independent: deny.trust is an opt-in REFUSAL a spec chooses,
	// while this is a FACT about the datum, recorded whether or not anyone
	// chose to deny on it. A tag that omitted the mark because no spec denied
	// would launder untrusted data into a handoff never allowed to carry it.
	//
	// Nil means "nothing declares this", which is NOT the same as trusted —
	// it is the honest answer for a deployment with no SEP-1913 metadata, and
	// it leaves the trifecta's leg A false exactly as it was before.
	UntrustedSource func(toolName string) bool

	Logger *slog.Logger
}

// InfoLeakRead is the PostToolCall hook that enforces the read-side info-leakage
// policy: requester view check and taint record. Ports PostExecuteLeakageHookWithResult
// from leakage.go.
//
// Enforcing: requester lacks view ⇒ Deny. Logging: audit + taint + Allow.
// The same split governs a view check that cannot be evaluated at all — an
// unresolvable requester, no principal on the call, a SpiceDB error: enforcing
// denies (fail closed), logging audits it as read_unchecked and allows, because
// logging mode's whole contract is that it changes nothing.
// Resource-id resolution uses ExtractToolIDArg (idArg wins) or
// ExtractToolResultID (resultIDField fallback).
type InfoLeakRead struct{ d InfoLeakReadDeps }

// NewInfoLeakRead creates an InfoLeakRead hook.
func NewInfoLeakRead(d InfoLeakReadDeps) *InfoLeakRead {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &InfoLeakRead{d: d}
}

func (h *InfoLeakRead) Name() string             { return "info_leak_read" }
func (h *InfoLeakRead) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }

func (h *InfoLeakRead) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	mode := h.d.Mode
	if mode == "disabled" || in.Tool == nil {
		return pipeline.Decision{}
	}

	// A PlanGateGoverned meta tool (delegate) entered the pipeline only for the
	// plan gate; it reaches no external resource and has no toolResourceMap
	// entry, so this gate — which denies an unmapped tool fail-closed — must
	// skip it, exactly as it never sees any other meta tool (they take the
	// ungated path). Without this, forcing delegate through the pipeline makes
	// the read gate refuse every delegation.
	if in.Tool.GovernedMeta {
		return pipeline.Decision{}
	}

	// An errored result USUALLY carries no resource to scope-check / taint /
	// gate, and (post toolguard) the post pipeline runs for failures too. But
	// the skip cannot key on the FLAG: isError is set by the upstream, so an
	// MCP server returning a full issue body under isError:true had its content
	// delivered to the model with no view check and no taint recorded. A gate
	// whose off-switch is a boolean the other side writes is not a gate.
	//
	// So the skip moved from the flag to the FACT, below: an error result runs
	// the normal path, and one whose resource cannot be RESOLVED is treated as
	// nothing-to-gate (errorResultUnresolvable) rather than as a misconfigured
	// declaration. A genuine failure resolves nothing and passes cleanly —
	// which is the behaviour this skip existed to protect — while a failure
	// carrying real content is checked like any other read.
	errored := in.Point == pipeline.PostToolCall && in.Tool.IsError

	if h.d.LookupReads == nil {
		// Gate enabled but no mapping resolver — treat as misconfiguration.
		return h.handleUnmappedTool(ctx, mode, in.Tool.Name, in.Tool.UseID)
	}

	decl := h.d.LookupReads(in.Tool.Name)
	if decl == nil {
		return h.handleUnmappedTool(ctx, mode, in.Tool.Name, in.Tool.UseID)
	}
	// A plural declaration reports the resources from the RESULT — several, of
	// different types, each with its own slice. It is checked FIRST, before
	// every other branch, because it satisfies none of them: there is no single
	// type to name and no single id to extract.
	//
	// Ahead of the NoTaint-equivalent return below on purpose. The plural path
	// performs no requester view check, so a future plural declaration has
	// every reason to also set BypassRequesterCheck — and in the other order
	// that decl would match the NoTaint return and opt out of tagging and
	// tainting entirely, silently, with the resources sitting in the result. A
	// declaration that names resources is tracked; opting out is for a
	// declaration that names none.
	if decl.ResultResources != nil {
		return h.evalResultResources(ctx, mode, in, decl, errored)
	}
	if decl.BypassRequesterCheck && decl.IDArg == "" && decl.ResultIDField == "" {
		// NoTaint equivalent: tool opted entirely out of leakage tracking.
		return pipeline.Decision{}
	}

	if decl.ResourceType == "" {
		// A read declaration naming no resource type: there is nothing to mint
		// a taint record against, so the read gate has no work. (The plural
		// ResultResources form, which carries its own per-pool types, has
		// already been handled above.)
		return pipeline.Decision{}
	}

	// Determine the resource ID: idArg wins (pre-execute path); resultIDField
	// is the response-side fallback.
	var resID, src string
	switch {
	case decl.IDArg != "":
		v, err := ExtractToolIDArg(in.Tool.Args, decl.IDArg)
		if err != nil {
			if errored {
				return h.errorResultUnresolvable(in.Tool.Name, decl.IDArg, err)
			}
			return h.handleMissingArg(ctx, mode, in.Tool.Name, decl.IDArg, fmt.Errorf("parse input: %w", err))
		}
		resID, src = v, decl.IDArg
	case decl.ResultIDField != "":
		v, err := ExtractToolResultID(in.Tool.Result, decl.ResultIDField)
		if err != nil {
			if errored {
				return h.errorResultUnresolvable(in.Tool.Name, decl.ResultIDField, err)
			}
			return h.handleMissingArg(ctx, mode, in.Tool.Name, decl.ResultIDField, fmt.Errorf("parse result: %w", err))
		}
		resID, src = v, decl.ResultIDField
	default:
		// Both empty: schema error; fail closed in enforcing, log in logging.
		// Not exempted for an error result: a declaration naming no id source
		// is broken for every call, and a failed one does not make it less so.
		return h.handleMissingArg(ctx, mode, in.Tool.Name, "(neither idArg nor resultIDField set)", nil)
	}
	if resID == "" {
		if errored {
			return h.errorResultUnresolvable(in.Tool.Name, src, nil)
		}
		return h.handleMissingArg(ctx, mode, in.Tool.Name, src, nil)
	}

	resource := decl.ResourceType + ":" + resID

	bypass := decl.BypassRequesterCheck
	checkPerformed := false
	checkPassed := false
	// uncheckedReason explains an absent verdict in the read_unchecked audit
	// record. It starts at the "nothing was wired" case and is overwritten by
	// whichever step could not produce one.
	uncheckedReason := "no_checker_wired"
	if bypass {
		uncheckedReason = "bypass"
	}
	var requester string
	var auditRecs []pipeline.AuditRecord

	if !bypass && h.d.Check != nil && h.d.Requester != nil {
		// unevaluated names why the view check produced no verdict, "" once it
		// has one. Every non-empty value routes through the same mode switch
		// the "requester lacks view" verdict and the sibling misconfiguration
		// handlers use — never an unconditional Deny, which would make logging
		// mode withhold tool results in the one mode an operator turns on
		// precisely so that nothing changes.
		var unevaluated string
		var cause error

		r, rerr := h.d.Requester(ctx, in.Requester)
		switch {
		case rerr != nil:
			unevaluated, cause = "requester_resolution_failed", rerr
		case r == "":
			// No principal was carried on this call — a kubectl-driven session
			// has no channel identity to resolve. Short-circuit rather than
			// handing "" to the checker: SpiceDB's subject parser rejects it,
			// which would report a missing identity as a malformed-object
			// error and point the operator at the wrong subsystem.
			unevaluated = "no_requester"
		default:
			requester = r
			ok, cerr := h.d.Check(ctx, requester, decl.Permission, resource)
			if cerr != nil {
				unevaluated, cause = "check_failed", cerr
			} else {
				checkPerformed = true
				checkPassed = ok
			}
		}

		switch {
		case unevaluated != "":
			// Never silent, in either mode: an operator must be able to see
			// that the read gate could not decide, and why.
			fields := []any{"tool", in.Tool.Name, "resource", resource, "mode", mode, "reason", unevaluated}
			if cause != nil {
				fields = append(fields, "err", cause.Error())
			}
			h.d.Logger.Info("info-leakage: read view check could not be evaluated", fields...)

			if mode == "enforcing" {
				// Fail closed: enforcing mode admits a result only once the
				// requester is known to hold the permission, and an
				// unevaluated check never establishes that. Do NOT record
				// taint — same as an outright denial.
				detail := unevaluated
				if cause != nil {
					detail = fmt.Sprintf("%s: %v", unevaluated, cause)
				}
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					Reason:  fmt.Sprintf("info-leakage: read denied: could not evaluate %s on %s (%s)", decl.Permission, resource, detail),
				}
			}
			// Logging (and any non-enforcing mode): fall through to taint, and
			// let the read_unchecked audit below carry the reason.
			uncheckedReason = unevaluated

		case !checkPassed:
			switch mode {
			case "enforcing":
				// Denied read in enforcing mode: do NOT record taint.
				return pipeline.Decision{
					Verdict: pipeline.Deny,
					Reason:  fmt.Sprintf("info-leakage: read denied: requester lacks %s on %s", decl.Permission, resource),
				}
			case "logging":
				// Logging mode: audit + fall through to taint.
				auditRecs = append(auditRecs, pipeline.AuditRecord{
					Kind: "read_denied",
					Fields: map[string]any{
						"tool":      in.Tool.Name,
						"resource":  resource,
						"requester": requester,
					},
				})
			}
		}
	}

	// Record taint (allowed check or logging mode deny or bypass/unchecked).
	//
	// A failure here is logged, not fail-closed. The persisted taint feeds the
	// respond-time PreResponse audience gate (accumulated cross-turn taint). A
	// missed append degrades that gate for THIS resource — but the immediate
	// PostToolCall audience gate (info_leak_audience, order 50, runs right
	// after this hook at 40) builds its OWN synthetic taint record for the same
	// tool and gates this single result independently, so a one-off append
	// failure does not open an unguarded same-turn leak. We deliberately do NOT
	// hard-deny an already-executed read on a transient store hiccup; the log
	// (with resource + err) is the operator's signal. If the PreResponse-only
	// path ever becomes load-bearing alone, revisit toward fail-closed.
	if h.d.AppendTaint != nil {
		if err := h.d.AppendTaint(ctx, infoleakagetaint.TaintRecord{
			ToolUseID:    in.Tool.UseID,
			AccessedAt:   time.Now().UTC(),
			ResourceType: decl.ResourceType,
			ResourceID:   resID,
			Permission:   decl.Permission,
			ToolName:     in.Tool.Name,
		}); err != nil {
			h.d.Logger.Info("info-leakage: taint append failed; respond-time gate degraded for this resource (PostToolCall gate still applies)",
				"tool", in.Tool.Name, "resource", resource, "mode", mode, "err", err.Error())
		}
	}

	// Per-datum provenance, alongside the session-wide taint above. The two
	// compose deliberately: taint is the FLOOR — the answer whenever a payload
	// is not fully tag-covered — and tags are what let a flow be judged on the
	// data actually in it rather than on everything the session ever read.
	//
	// A failure here is logged and not fatal, and unlike the taint append that
	// is not merely pragmatic: without a tag this datum falls back to the
	// session-wide taint set, which can only blanket-block or blanket-approve.
	// That is OVER-blocking, which is the degradation the design requires — "a
	// stripped tag must degrade to over-blocking, never to unchecked". Denying
	// an already-executed read because its provenance could not be recorded
	// would trade a safe degradation for a broken tool call.
	if h.d.MintPtTag != nil {
		// The integrity mark rides the same mint as the audience, because both
		// are properties of THIS datum and a tag written without one of them
		// is a tag a later gate would read as clean on that axis.
		untrusted := h.d.UntrustedSource != nil && h.d.UntrustedSource(in.Tool.Name)
		// A read tool's output IS the resource it read, so it mints a LEAF over
		// that resource — ALWAYS, even when the call's args happened to carry
		// tagged content. Deriving the output from arg-tags would launder: a
		// call reading a narrow resource whose args carry a wide tag would mint a
		// wide tag over narrow content (content {A}, tag {A,B,C}), violating
		// "floor stays a floor". The args do not flow into a read output; they
		// were already stripped before the tool ran.
		tagID, err := h.d.MintPtTag(ctx, memory.PtTagMintRequest{
			ToolUseID: in.Tool.UseID,
			Resources: []memory.PtTagResourceRef{{
				Type:       decl.ResourceType,
				ID:         resID,
				Permission: decl.Permission,
			}},
			UntrustedOrigin: untrusted,
			// The result itself, stored behind the tag so the platform can
			// place it in a child's context when this datum is bound into a
			// data slot. It lands in pt_tag_content, which the agent cannot
			// read back — being able to hand a datum onward is not the same
			// as being able to ask for it again.
			Content: in.Tool.Result,
			MIME:    "application/json",
		})
		if err != nil {
			h.d.Logger.Info("info-leakage: pt-tag mint failed; this datum falls back to the session-wide taint set (over-blocks, never under-blocks)",
				"tool", in.Tool.Name, "resource", resource, "mode", mode, "err", err.Error())
		} else if led := provenance.TagLedgerFrom(ctx); led != nil {
			// The emitter (loop.go) reads this to wrap the result the model
			// sees in its pt-untrusted envelope. No ledger (a non-tagging path)
			// simply means no wrap — the result reaches the model unmarked.
			led.RecordMinted(in.Tool.UseID, tagID)
		}
	}

	// Positive-path audit.
	resources := []infoleakageaudit.ResourceRef{{Type: decl.ResourceType, ID: resID}}
	switch {
	case checkPerformed && checkPassed:
		auditRecs = append(auditRecs, pipeline.AuditRecord{
			Kind: "read_permitted",
			Fields: map[string]any{
				"tool":       in.Tool.Name,
				"resources":  resources,
				"requester":  requester,
				"permission": decl.Permission,
			},
		})
	case !checkPerformed:
		auditRecs = append(auditRecs, pipeline.AuditRecord{
			Kind: "read_unchecked",
			Fields: map[string]any{
				"tool":       in.Tool.Name,
				"resources":  resources,
				"permission": decl.Permission,
				"reason":     uncheckedReason,
			},
		})
	}

	return pipeline.Decision{Audit: auditRecs}
}

// evalResultResources is the read gate for a call whose result came from
// SEVERAL resources at once — a memory search spanning the session's own scope
// plus the resource pools it reached through slot grants.
//
// Per resource it does both of the things the single-resource path does once:
//
//   - one taint record, which is what the respond-time floor sees. A pool with
//     a tag and no taint leaves that gate blind to it, because a tag refines
//     only a payload that is FULLY tag-covered and every other payload falls
//     back to the floor.
//   - one MINT CALL, naming that resource alone. Not one call naming them all:
//     the minter INTERSECTS a request's resources, so a single call would
//     produce a tag readable only by whoever can view every pool at once — safe
//     (it under-discloses) and wrong (a reader of one pool could not be handed
//     that pool's own datum), and nothing downstream would report it.
//
// Each mint carries only ITS resource's slice of the result. The content lands
// in pt_tag_content, which the platform places into a child's context when the
// datum is bound into a data slot, so a tag for one pool carrying the whole
// result would disclose every other pool's entries to this pool's audience.
//
// It performs no requester view check, and that is a deliberate difference from
// the single-resource path rather than an omission. The check answers "may this
// requester cause this read?" against ONE object; here a verdict is per pool,
// and the only lever a PostToolCall hook has is denying the whole result — so
// one pool outside the requester's reach would withhold every other pool's
// entries too. What a pool's entries must never do is REACH someone outside the
// pool's audience, and that is gated per pool, the same turn, by
// info_leak_audience over these same resources, and again at respond time. The
// reads are audited as unchecked so the absent verdict is visible rather than
// implied.
func (h *InfoLeakRead) evalResultResources(ctx context.Context, mode string, in pipeline.Input, decl *ToolReadsDecl, errored bool) pipeline.Decision {
	refs, err := resolveResultReads(decl, in.Tool.Result)
	if err != nil {
		// Same fact-not-flag rule as the single-resource path, and the reason
		// the two error kinds are distinguishable at all: a call that FAILED
		// and whose result is not the tool's envelope names nothing, and there
		// is genuinely nothing to gate in that. A REFUSAL is different — the
		// declaration read the result and would not attribute it — and isError
		// is written by the upstream, so letting the flag excuse a refusal
		// would put this gate's off-switch in the other side's hands. See
		// ErrUnattributableResource.
		if errored && !errors.Is(err, ErrUnattributableResource) {
			return h.errorResultUnresolvable(in.Tool.Name, "(result resources)", err)
		}
		return h.handleUnattributableResult(mode, in.Tool.Name, err)
	}
	if len(refs) == 0 {
		// The call read nothing pool-scoped — a search that matched only this
		// session's own entries. Their audience IS the session, which the
		// session-wide floor already covers; minting here would claim a
		// per-datum audience for data that has none of its own.
		return pipeline.Decision{}
	}

	// One integrity answer for the call, as on the single-resource path: it is
	// a property of the tool's source, not of which resource an entry came from.
	untrusted := h.d.UntrustedSource != nil && h.d.UntrustedSource(in.Tool.Name)
	accessedAt := time.Now().UTC()

	resources := make([]infoleakageaudit.ResourceRef, 0, len(refs))
	// The same refs as a STRING, because the audit sink keeps only
	// string-valued fields (runnerHost.writeAudit copies tool/requester/
	// leakedTo/audience and then every remaining string): the structured slice
	// above is dropped there, as it already is for read_permitted. This record
	// is the only place the absent view verdict is visible, so what it names
	// has to survive the write.
	//
	// Per-ref "type:id#permission" rather than one flattened permission field:
	// the declaration allows a different permission per resource, so a single
	// field would be a lie the moment two pools differ, and a reader could not
	// tell which one it was true of.
	refNames := make([]string, 0, len(refs))
	for _, r := range refs {
		resource := r.Type + ":" + r.ID
		resources = append(resources, infoleakageaudit.ResourceRef{Type: r.Type, ID: r.ID})
		refNames = append(refNames, resource+"#"+r.Permission)

		if h.d.AppendTaint != nil {
			if err := h.d.AppendTaint(ctx, infoleakagetaint.TaintRecord{
				ToolUseID:    in.Tool.UseID,
				AccessedAt:   accessedAt,
				ResourceType: r.Type,
				ResourceID:   r.ID,
				Permission:   r.Permission,
				ToolName:     in.Tool.Name,
			}); err != nil {
				h.d.Logger.Info("info-leakage: taint append failed; respond-time gate degraded for this resource (PostToolCall gate still applies)",
					"tool", in.Tool.Name, "resource", resource, "mode", mode, "err", err.Error())
			}
		}

		if h.d.MintPtTag == nil {
			continue
		}
		// Logged and not fatal, as on the single-resource path: without a tag
		// this pool's entries fall back to the session-wide taint set, which
		// over-blocks — the degradation direction the design requires.
		//
		// Nothing is recorded on the tag ledger. The ledger carries one tag id
		// per tool_use and the emitter wraps the WHOLE result in it, but no
		// single pool's tag governs the whole result — each one's stored
		// content is its own slice. Claiming one over the lot would assert
		// something the operator's content binding then refuses, dropping the
		// payload to the coarse floor it would have reached anyway.
		if _, err := h.d.MintPtTag(ctx, memory.PtTagMintRequest{
			ToolUseID: in.Tool.UseID,
			Resources: []memory.PtTagResourceRef{{
				Type:       r.Type,
				ID:         r.ID,
				Permission: r.Permission,
			}},
			UntrustedOrigin: untrusted,
			Content:         r.Content,
			MIME:            "application/json",
		}); err != nil {
			h.d.Logger.Info("info-leakage: pt-tag mint failed; this pool's entries fall back to the session-wide taint set (over-blocks, never under-blocks)",
				"tool", in.Tool.Name, "resource", resource, "mode", mode, "err", err.Error())
		}
	}

	return pipeline.Decision{Audit: []pipeline.AuditRecord{{
		Kind: "read_unchecked",
		Fields: map[string]any{
			"tool":         in.Tool.Name,
			"resources":    resources,
			"resourceRefs": strings.Join(refNames, ", "),
			"reason":       "result_resources",
		},
	}}}
}

// handleUnattributableResult governs a result a plural declaration could not
// read into the resources it came from.
//
// It is NOT the same as a result naming no resources: that one is a search that
// matched only this session's own entries, and is allowed with nothing
// recorded. This is a result whose provenance is unknown, so enforcing withholds
// it rather than handing the model entries nothing can later reason about.
// Logging audits and allows, because logging mode's whole contract is that it
// changes nothing.
func (h *InfoLeakRead) handleUnattributableResult(mode, toolName string, cause error) pipeline.Decision {
	h.d.Logger.Info("info-leakage: tool result could not be attributed to the resources it read",
		"tool", toolName, "mode", mode, "err", cause.Error())
	switch mode {
	case "enforcing":
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: tool %q: result could not be attributed to the resources it read: %v", toolName, cause),
		}
	case "logging":
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "unattributable_result",
				Fields: map[string]any{"tool": toolName, "err": cause.Error()},
			}},
		}
	}
	return pipeline.Decision{}
}

// handleUnmappedTool governs a non-meta tool with no toolResourceMap entry (an
// undeclared sidecar/CLI/MCP tool). It does NOT hard-deny: the invariant is that
// undeclared data is no more restricted than the coarse floor. It records a
// coarse-floor taint whose audience is the session's own participants
// (agentsession#unknown_provenance), so the respond-time gate lets the data flow
// WITHIN the conversation without a prompt and asks for approval only if a reply
// would widen it beyond the session. Enforcing and logging behave the same here
// (both taint + allow); the only fail-closed case is a MISSING session ref,
// where the floor cannot be built and enforcing must not silently let undeclared
// data out. mode is guaranteed non-"disabled" (Eval short-circuits it).
func (h *InfoLeakRead) handleUnmappedTool(ctx context.Context, mode, toolName, toolUseID string) pipeline.Decision {
	if h.d.SessionRef == "" {
		if mode == "enforcing" {
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  fmt.Sprintf("info-leakage: tool %q is undeclared and no session floor is available to gate its egress", toolName),
			}
		}
		return pipeline.Decision{}
	}
	if h.d.AppendTaint != nil {
		if err := h.d.AppendTaint(ctx, infoleakagetaint.TaintRecord{
			ToolUseID:    toolUseID,
			AccessedAt:   time.Now().UTC(),
			ResourceType: "agentsession",
			ResourceID:   h.d.SessionRef,
			Permission:   "unknown_provenance",
			ToolName:     toolName,
		}); err != nil && h.d.Logger != nil {
			h.d.Logger.Info("info-leakage: floor taint append failed for undeclared tool; respond-time egress gate degraded for this datum",
				"tool", toolName, "mode", mode, "err", err.Error())
		}
	}
	return pipeline.Decision{
		Audit: []pipeline.AuditRecord{{
			Kind:   "unmapped_tool_floored",
			Fields: map[string]any{"tool": toolName},
		}},
	}
}

// errorResultUnresolvable is the "nothing to gate" answer for a FAILED tool
// call whose resource cannot be resolved — the case the old blanket isError
// skip existed to protect.
//
// It allows, in both modes, and deliberately does NOT route through
// handleMissingArg: a connection refused is not a broken declaration, and
// treating it as one would deny every failed tool call in an enforcing session.
// The distinction is narrow on purpose — the call must have failed AND have no
// resolvable resource. A failed call that DOES name one is gated normally,
// which is the whole point: isError is written by the upstream and cannot be
// what opens the gate.
//
// Logged at DEBUG: routine on a healthy session, and the one thing an operator
// would want if a resource they expected to see tainted never was.
func (h *InfoLeakRead) errorResultUnresolvable(toolName, src string, cause error) pipeline.Decision {
	fields := []any{"tool", toolName, "idSource", src}
	if cause != nil {
		fields = append(fields, "err", cause.Error())
	}
	h.d.Logger.Debug("info-leakage: failed tool call names no resolvable resource; nothing to gate", fields...)
	return pipeline.Decision{}
}

func (h *InfoLeakRead) handleMissingArg(ctx context.Context, mode, toolName, argName string, cause error) pipeline.Decision {
	switch mode {
	case "enforcing":
		if cause != nil {
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  fmt.Sprintf("info-leakage: tool %q: idArg %q: %v", toolName, argName, cause),
			}
		}
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("info-leakage: tool %q: idArg %q not present in input", toolName, argName),
		}
	case "logging":
		return pipeline.Decision{
			Audit: []pipeline.AuditRecord{{
				Kind:   "unmapped_arg",
				Fields: map[string]any{"tool": toolName, "idArg": argName},
			}},
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*InfoLeakRead)(nil)
