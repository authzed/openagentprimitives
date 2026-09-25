package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaInspectionPhase is when, relative to an ungated meta tool's execution,
// this path inspects a contentguard point.
type metaInspectionPhase int

const (
	// metaPhaseBeforeExec inspects the call's arguments; a block there means the
	// tool never runs.
	metaPhaseBeforeExec metaInspectionPhase = iota
	// metaPhaseAfterExec inspects the produced result before it feeds the LLM.
	metaPhaseAfterExec
)

// metaInspectionPhases assigns a phase to EVERY point in
// contentguard.InspectionPoints — the closed set an Instance may declare.
//
// Keying off the whole contract, not one named point, is what closes an
// exfiltration hole: inspecting results only lets a url-allowlist inspector
// (points [args, result], defaultAction deny) block a non-allowlisted URL in a
// gated tool's ARGS while passing the same URL in respond_to_user's — the meta
// surface that actually egresses. A point contentguard adds later has no phase
// here; runMetaContentInspection fails closed on that rather than skipping, and
// TestMetaInspectionCoversEveryInspectionPoint catches it in CI first.
var metaInspectionPhases = map[pipeline.Point]metaInspectionPhase{
	pipeline.PreToolCall:  metaPhaseBeforeExec,
	pipeline.PostToolCall: metaPhaseAfterExec,
}

// inspectMetaToolCall wraps one ungated meta-tool execution with the content
// inspection a gated tool gets from the contentguard pipeline adapter
// (pkg/authz/contentguard/hook.go): args before the tool runs, result after.
// Meta tools with a trivial (Stateless/Passthrough) permission bypass the
// containment pipeline entirely, so this is their ONLY content inspection.
//
// The args half deliberately ignores Result.Trusted: that flag describes
// framework-owned RESULT content, while the arguments are model-authored and are
// exactly what a prompt injection steers — honoring it here would leave
// respond_to_user's text uninspected.
func (l *Loop) inspectMetaToolCall(
	ctx context.Context, toolName string, args json.RawMessage, exec func(context.Context) tool.Result,
) tool.Result {
	if withheld, ok := l.runMetaContentInspection(ctx, metaPhaseBeforeExec, toolName, args, tool.Result{}); !ok {
		return withheld
	}
	out := exec(ctx)
	if out.Trusted {
		return out
	}
	return l.inspectUntrustedResult(ctx, toolName, out)
}

// withholdMetaContent is what the model sees in place of blocked content. The
// wording differs by phase because the situations differ: before exec the tool
// never ran at all, after exec it ran and its output was withheld.
func withholdMetaContent(phase metaInspectionPhase, reason string) tool.Result {
	if phase == metaPhaseBeforeExec {
		return tool.Result{Content: "tool call blocked by content guard: " + reason, IsError: true}
	}
	return tool.Result{Content: "content withheld by content guard: " + reason, IsError: true}
}

// inspectUntrustedResult runs the session's content inspectors over an untrusted
// meta-tool result — the after-exec half of inspectMetaToolCall. Meta tools
// bypass the PostToolCall pipeline (leakageGateApplies excludes KindMeta), so
// the caller routes any meta result NOT marked Result.Trusted here instead.
//
// The verdict mapping mirrors the pipeline adapter (pkg/authz/contentguard/hook.go)
// so a meta result is judged exactly as a gated one would be:
//
//   - Block, or an inspector error, fails closed: the result becomes an IsError
//     note so the agent sees it was withheld and why.
//   - Approve raises the SAME content_inspection ApprovalAsk the adapter builds
//     and blocks for a human. It is the prompt-injection inspector's DEFAULT, so
//     downgrading it to Block makes the withhold UNRECOVERABLE — the subject is
//     stored transcript text, so every later recall scores the same and is
//     withheld again, killing memory recall for the session with no human ever
//     asked — while treating it as a pass fails OPEN against an operator who
//     configured "ask a human".
//   - With no reachable approver the result is withheld: fail-closed, but scoped
//     to this one result. The gated path escalates a publish failure to a
//     session Halt; that blast radius is wrong for a memory recall.
//
// The subject is bounded by contentguard.Capped (pkg/authz/contentguard/cap.go),
// as in the adapter, so an oversized meta result cannot buy a warn-mode Pass by
// outrunning the detector's timeout on one path but not the other; truncation is
// stamped onto the Finding's Details and carried into the audit event below.
//
// Safe for concurrent use.
func (l *Loop) inspectUntrustedResult(ctx context.Context, toolName string, res tool.Result) tool.Result {
	// args are not part of the after-exec subject (contentguard.SubjectFor maps
	// PostToolCall onto the result), so nil is the whole truth here.
	if withheld, ok := l.runMetaContentInspection(ctx, metaPhaseAfterExec, toolName, nil, res); !ok {
		return withheld
	}
	return res
}

// runMetaContentInspection runs every content inspector that declares a point
// assigned to phase, over the Subject contentguard.SubjectFor builds for that
// point, and reports whether the call may proceed. ok=false carries the
// replacement IsError result the model must see instead. Inspectors are visited
// in configured order and the first blocking verdict wins, on both phases.
//
// Safe for concurrent use.
func (l *Loop) runMetaContentInspection(
	ctx context.Context, phase metaInspectionPhase, toolName string, args json.RawMessage, res tool.Result,
) (tool.Result, bool) {
	for idx, inst := range l.ContentInspectors {
		id := l.contentInspectorID(idx)
		for _, at := range inst.Points() {
			record := func(action, reason string, details map[string]any) {
				l.recordContentGuardEvent(ctx, contentguard.Event{
					Inspector: id, Action: action, Tool: toolName,
					Point:   string(at),
					Reason:  reason,
					Details: details,
				})
			}

			ph, known := metaInspectionPhases[at]
			if !known {
				// contentguard grew an inspection point this path was never
				// taught. Refusing the call is the fail-closed answer: an admin
				// configured a guard for this point, and proceeding with it
				// unapplied is exactly the silently-inert enforcement this path
				// exists to prevent. Unreachable while metaInspectionPhases keys
				// contentguard.InspectionPoints — a test asserts that.
				reason := "content guard " + id + ": inspection point " + string(at) + " is not handled for ungated tools"
				slog.Default().Info("content guard: unhandled inspection point on the ungated meta path; failing closed",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"tool", toolName, "inspector", id, "point", string(at))
				record("block", reason, nil)
				return withholdMetaContent(phase, reason), false
			}
			if ph != phase {
				continue
			}

			subj, serr := contentguard.SubjectFor(at, toolName, args, res.Content, res.IsError)
			if serr != nil {
				// Same contract backstop as above, from the other side: never
				// hand an inspector an empty Subject, which it would Pass.
				reason := "content guard " + id + ": " + serr.Error()
				slog.Default().Info("content guard: no subject for this point on the ungated meta path; failing closed",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"tool", toolName, "inspector", id, "point", string(at), "err", serr.Error())
				record("block", reason, map[string]any{"error": serr.Error()})
				return withholdMetaContent(phase, reason), false
			}

			f, err := contentguard.Capped(inst).Inspect(ctx, subj)
			if err != nil {
				reason := "content guard " + id + ": inspection failed: " + err.Error()
				record("block", reason, map[string]any{"error": err.Error()})
				return withholdMetaContent(phase, reason), false
			}

			switch f.Action {
			case contentguard.Block:
				record("block", f.Reason, f.Details)
				return withholdMetaContent(phase, f.Reason), false

			case contentguard.Approve:
				// Same audit shape the adapter emits when it raises the ask.
				record("approve", f.Reason, f.Details)
				approved, aerr := l.askContentInspectionApproval(ctx, toolName, id, f)
				if aerr != nil {
					// No human could be asked (approval path unwired, no resolvable
					// approver, publish/await failed, turn context ended). Withhold
					// and say why — never silently pass, never halt the session.
					reason := f.Reason + " (approval unavailable: " + aerr.Error() + ")"
					slog.Default().Info("content guard: content_inspection approval unavailable; withholding meta content",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
						"tool", toolName, "inspector", id, "point", string(at), "err", aerr.Error())
					record("block", reason, f.Details)
					return withholdMetaContent(phase, reason), false
				}
				if !approved {
					reason := f.Reason + " (not approved)"
					record("block", reason, f.Details)
					return withholdMetaContent(phase, reason), false
				}
				// Released by a human: the content stands, exactly as an approved
				// content_inspection lets a gated tool's I/O through.

			default: // Pass
				record("pass", f.Reason, f.Details)
			}
		}
	}
	return res, true
}

// askContentInspectionApproval raises the same content_inspection ApprovalAsk
// the contentguard pipeline adapter builds for an Approve finding
// (pkg/authz/contentguard/hook.go) and blocks for the human decision, so a
// meta-tool result gets the identical human-release valve a gated one gets.
//
// It drives runnerHost directly rather than pipeline.Executor: the executor
// turns an approval publish/await failure into host.Halt, terminating the whole
// session, which one un-askable memory recall must not do — those failures come
// back as an error for the caller to fail closed on locally. The await deadline
// is the same TTL stamped on the card's ExpiresAt, so the wait and the displayed
// expiry cannot disagree.
func (l *Loop) askContentInspectionApproval(ctx context.Context, toolName, inspectorID string, f contentguard.Finding) (bool, error) {
	if l.Approval == nil || l.InteractionRequestPublish == nil {
		return false, fmt.Errorf("no approval surface is wired for this session")
	}
	host := newRunnerHost(l, hostSession{
		Namespace: l.SessionKey.Namespace,
		Name:      l.SessionKey.Name,
		Class:     l.AgentName,
	})
	ask := pipeline.ApprovalAsk{
		Kind:    "content_inspection",
		Summary: f.Reason,
		Payload: map[string]any{
			"inspector": inspectorID,
			"tool":      toolName,
			"details":   f.Details,
		},
		OnTimeout: timeoutPolicyFor("content_inspection"),
	}
	reqID, err := host.PublishApproval(ctx, ask)
	if err != nil {
		return false, err
	}
	approved, by, timedOut, err := host.AwaitDecision(ctx, reqID, host.contentInspectionTTL())
	if err != nil {
		return false, err
	}
	if timedOut {
		// A lapsed deadline is a sticky deny. OnTimeout is stamped on the ask so
		// it stays byte-identical to the adapter's, but only pipeline.Executor
		// reads it; this path deliberately denies rather than halting even if
		// content_inspection's policy ever becomes TimeoutHalt — see the
		// blast-radius note above.
		approved = false
	}
	// Parity with pipeline.Executor: record who resolved the ask. Best-effort;
	// runnerHost.Audit logs its own write failures.
	if aerr := host.Audit(ctx, []pipeline.AuditRecord{{
		Kind: "approval_resolved",
		Fields: map[string]any{
			"approvalKind": ask.Kind,
			"approved":     approved,
			"approver":     by,
		},
	}}); aerr != nil {
		slog.Default().Info("content guard: approval_resolved audit write failed",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"tool", toolName, "inspector", inspectorID, "err", aerr.Error())
	}
	return approved, nil
}
