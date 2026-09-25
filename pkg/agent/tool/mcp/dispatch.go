package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp/labelextract"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/authz/observe/fromcrd"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	probe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	validator "github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

type tcArgs struct {
	// OperationID is the Operation this call is audited against.
	OperationID string `json:"operation_id"`
	// Reason is the LLM's stated justification, recorded in the audit entry.
	Reason string `json:"_reason"`
	// Args is the upstream MCP tool's own argument object, forwarded verbatim.
	Args map[string]any `json:"args"`
}

// resultMeta carries the SEP-1913 _meta.annotations block on a tools/call
// response. Present only when the server emits it.
type resultMeta struct {
	Annotations *resultAnnotations `json:"annotations,omitempty"`
}

// resultAnnotations is the subset of SEP-1913 annotations the dispatcher
// consumes at post-call time.
type resultAnnotations struct {
	// MaliciousActivityHint is the server flagging its own response as suspect;
	// the dispatcher withholds the payload from the model when set.
	MaliciousActivityHint bool `json:"maliciousActivityHint,omitempty"`
	// Attribution names the upstream sources the response drew on.
	Attribution []string `json:"attribution,omitempty"`
	// OpenWorldHint marks the response as reaching beyond the server's own data.
	OpenWorldHint bool `json:"openWorldHint,omitempty"`
}

// Cancel satisfies tool.Cancellable. MCP calls are cancelled by cancelling the
// per-call context the runner passes to Execute → probe.CallTool, which makes
// the go-sdk emit an MCP notifications/cancelled to the server (see
// go-sdk mcp/transport.go). There is no additional client-side teardown, so
// Cancel is a no-op that exists to MARK this tool interruptible in-flight.
func (m *MCPTool) Cancel(ctx context.Context) error { return nil }

// Execute issues JSON-RPC tools/call and maps the response into a tool.Result.
// Pre-call validation runs through pkg/tools/mcp/validator.Check below — the
// tool → allowedFields → deny.effects → constraints pipeline — and
// produces a Decision whose ParsedArgs is already redacted against
// tool.Args.SensitiveFields.
func (m *MCPTool) Execute(ctx context.Context, raw json.RawMessage, sess *agenttool.SessionContext) (agenttool.Result, error) {
	var a tcArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return agenttool.Result{Content: "mcp: invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if a.OperationID == "" || a.Reason == "" {
		return agenttool.Result{Content: "mcp: " + agenttool.MissingOpCtxMessage, IsError: true}, nil
	}
	if sess != nil && sess.Operations != nil {
		if _, ok := sess.Operations.Get(a.OperationID); !ok {
			return agenttool.Result{
				Content: fmt.Sprintf("mcp: operation_id %q is not registered — call new_operation first", a.OperationID),
				IsError: true,
			}, nil
		}
	}

	// Pre-call validation via the structured validator. Decision.ParsedArgs
	// is already redaction-applied, so audit records and surfaced messages
	// never carry the raw sensitive values.
	if m.spec == nil {
		return agenttool.Result{
			Content: "mcp: validator internal: spec back-pointer missing on MCPTool",
			IsError: true,
		}, nil
	}
	dec, verr := validator.Check(m.spec, validator.Invocation{
		ToolName: m.toolName,
		Args:     a.Args,
	})
	if verr != nil {
		return agenttool.Result{
			Content: "mcp: validator internal: " + verr.Error(),
			IsError: true,
		}, nil
	}
	if !dec.Allow {
		if sess != nil && sess.Operations != nil {
			rb, _ := json.Marshal(dec.Parsed.Args) // already redacted
			failedPath := ""
			if dec.FailedOn != nil {
				failedPath = dec.FailedOn.Path
			}
			// This call never dispatches (the function returns immediately
			// below), so it is complete the instant it's recorded — there is
			// no in-flight window to track.
			if idx, ok := sess.Operations.RecordCall(a.OperationID, agenttool.OperationCall{
				Tool:   m.llmName,
				Reason: a.Reason + " [REJECTED: " + failedPath + ": " + dec.Reason + "; args=" + string(rb) + "]",
			}); ok {
				sess.Operations.CompleteCall(a.OperationID, idx)
			}
		}
		return agenttool.Result{Content: dec.Reason, IsError: true}, nil
	}
	// Allow path — record audit with redacted args, then dispatch. Mark the
	// call done on every exit path below (the defer) — the operation-activity
	// live tree only surfaces operations with in-flight (not-yet-completed)
	// calls.
	if sess != nil && sess.Operations != nil {
		rb, _ := json.Marshal(dec.Parsed.Args)
		if idx, ok := sess.Operations.RecordCall(a.OperationID, agenttool.OperationCall{
			Tool:   m.llmName,
			Reason: a.Reason + " [args=" + string(rb) + "]",
		}); ok {
			defer sess.Operations.CompleteCall(a.OperationID, idx)
		}
	}

	callCtx := ctx
	if m.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}

	logger := m.logger
	if logger == nil {
		logger = slog.Default()
	}

	// Dispatch over a spec-compliant MCP session (pkg/tools/mcp/probe, backed by the
	// official Go SDK): it performs the initialize handshake, Mcp-Session-Id
	// tracking, and SSE unwrapping — a bare stateless POST is rejected by
	// spec-compliant servers. Auth + the OAuth reauth-on-401 retry live in the
	// injected transport (below), not here. m.httpClient is the plain client for
	// operator-controlled reach paths (sidecar pod IP / loopback); a nil client
	// defaults to the SSRF-guarded client for agent-supplied MCPServer URLs.
	//
	// reauthPersist wraps m.reauth so a successful 401-triggered refresh is also
	// persisted onto the tool (m.setAuthLocked): reauth once, then reuse the
	// fresh token.
	var reauthPersist func(context.Context) (string, string, error)
	if m.reauth != nil {
		reauthPersist = func(rctx context.Context) (string, string, error) {
			nh, nv, rerr := m.reauth(rctx)
			if rerr != nil {
				logger.Warn("mcp.dispatch.reauth failed; surfacing original 401",
					"tool", m.llmName, "server", m.serverName, "err", rerr.Error())
				return "", "", rerr
			}
			m.setAuthLocked(nh, nv)
			return nh, nv, nil
		}
	}

	// A credential revoked mid-session must be re-resolved BEFORE anything goes
	// upstream — and the call denied if it cannot be. This precedes the call
	// deliberately: the OAuth reauth-on-401 inside CallTool cannot substitute for
	// it, because a revoked credential does not necessarily provoke a 401 (the
	// token stays valid at the upstream until the AgentIdentity credential is
	// actually rotated or deleted), and by then it has already been used.
	header, value, err := m.resolveIfRevoked(callCtx)
	if err != nil {
		logger.Warn("mcp.dispatch: denying call on revoked credential",
			"tool", m.llmName, "server", m.serverName, "err", err.Error())
		return agenttool.Result{Content: "mcp: " + err.Error(), IsError: true}, nil
	}

	// Durable per-call authorization: the operator writes externaltoken
	// authorized_token grants (and revokes them) directly in SpiceDB, so a
	// grant written or revoked mid-session takes effect on the very next
	// call — unlike resolveIfRevoked above, which only reacts to a locally
	// frozen credential being marked stale. Checked immediately before the
	// exact (header, value) about to go upstream, fully consistent so a
	// just-written revoke can never race a stale read.
	if g := m.currentUseTokenGate(); g != nil {
		// failSession invokes g.FailSession if wired; otherwise this is a
		// misconfigured gate (Checker set but FailSession not — see
		// UseTokenGate's doc contract in mcp_tool.go), which we log loudly
		// per AGENTS.md's no-silent-errors rule rather than panic on a nil
		// call. Either way the call itself is still denied below — mirrors
		// the file's `if m.reauth != nil` defensive style.
		failSession := func(reason, msg string) {
			if g.FailSession == nil {
				logger.Error("mcp.dispatch: token-use gate has no FailSession wired; cannot fail session, denying this call only",
					"tool", m.llmName, "credID", g.CredID, "reason", reason)
				return
			}
			g.FailSession(reason, msg)
		}
		if g.Checker == nil { // typed-nil / unconfigured checker → fail closed, loud
			logger.Error("mcp.dispatch: token-use checker not configured; failing session",
				"tool", m.llmName, "credID", g.CredID)
			failSession(spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, "token-use checker not configured")
			return agenttool.Result{Content: "mcp: token authorization unavailable", IsError: true}, nil
		}
		presented := externaltoken.ValueHash(g.HMACKey, value)
		allowed, cerr := g.Checker.CheckUseToken(callCtx, g.SessionNS, g.SessionName, g.CredID, presented, true)
		switch {
		case cerr != nil: // indeterminate → fail the session (loud, per spec)
			logger.Error("mcp.dispatch: token authz indeterminate; failing session",
				"tool", m.llmName, "credID", g.CredID, "err", cerr.Error())
			failSession(spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, cerr.Error())
			return agenttool.Result{Content: "mcp: token authorization unavailable: " + cerr.Error(), IsError: true}, nil
		case !allowed: // definitive deny → surgical, session continues
			logger.Warn("mcp.dispatch: token use denied (revoked)", "tool", m.llmName, "credID", g.CredID)
			return agenttool.Result{Content: "mcp: token revoked; access denied", IsError: true}, nil
		}
	}

	// Route through the per-AgentSession persistent session when the runner wired
	// one: it reuses ONE MCP session per (server URL, credential), so server-side
	// per-session state keyed by the MCP session id survives instead of resetting
	// every call. A nil cache means a fresh session per call.
	//
	// The cache is told WHICH credential this is, not just its current bytes:
	// reauthPersist writes a refreshed value straight back onto the tool, so
	// `value` changes on nothing more than a routine OAuth refresh, and a cache
	// keyed on it would drop the session — and the server state — exactly one
	// call after every refresh.
	var outcome *probe.CallOutcome
	var cerr error
	if m.sessionCache != nil {
		outcome, cerr = m.sessionCache.CallTool(callCtx, m.url, m.httpClient, m.toolName, a.Args, probe.Credential{
			ID:     m.credentialIdentity(value),
			Header: header,
			Value:  value,
			Reauth: reauthPersist,
		})
	} else {
		outcome, cerr = probe.CallTool(callCtx, m.url, m.httpClient, m.toolName, a.Args, header, value, reauthPersist)
	}
	if cerr != nil {
		res := agenttool.Result{Content: "mcp: " + cerr.Error(), IsError: true}
		// Surface the upstream HTTP status out-of-band (Result.HTTPStatus), NOT
		// into Content — Content is unchanged from before this field existed.
		// errors.As, not string-matching: probe.HTTPError is the typed carrier
		// authTransport.httpError produces precisely so callers don't have to
		// parse "HTTP %d" out of the error text (see its doc comment). A
		// non-HTTP failure (transport error, timeout, ctx cancellation) leaves
		// httpErr nil and res.HTTPStatus at its zero value — "nothing observed",
		// never mistaken for a real status (see Result.HTTPStatus's doc).
		var httpErr *probe.HTTPError
		if errors.As(cerr, &httpErr) {
			res.HTTPStatus = httpErr.StatusCode
		}
		// Log the upstream failure here, because otherwise the only operator-
		// visible trace is the CONSEQUENCE — toolguard's breaker opening with a
		// trip count and cool-off but no cause — and diagnosing a 401 from that
		// alone is guesswork.
		//
		// The status and server URL separate the cases an operator must tell
		// apart: a rejected credential, an unreachable host, a tool the server no
		// longer has. INFO, because a failing upstream is operational, not debug.
		// The credential itself is never logged, only whether one was attached —
		// which distinguishes "we sent no auth" from "our auth was refused", two
		// different bugs behind one status code.
		logger.Info("mcp.dispatch: upstream tool call failed",
			"tool", m.llmName, "server", m.serverName, "url", m.url,
			"httpStatus", res.HTTPStatus, "authHeaderSent", header != "",
			"err", cerr.Error())
		return res, nil
	}

	// From here down the exchange has COMPLETED: probe.CallTool returned no error,
	// so the server answered 200 with a well-formed JSON-RPC result — every non-2xx
	// became a *probe.HTTPError above, every transport failure some other error.
	// The credential got past the endpoint's auth layer and the tool ran.
	//
	// So the result every path below returns is built ONCE, here, with the positive
	// corroboration carrier already stamped — rather than at each exit, where a
	// path that refuses the RESPONSE (the maliciousActivityHint withhold) or fails
	// a downstream write (relwrites) would silently ship OriginAuthenticated=false.
	// The recorder reads {HTTPStatus: 0, OriginAuthenticated: false} as neither
	// recording nor retracting, leaving a stale auth-failure observation alive
	// across a call that just proved the credential works — and the agent chooses
	// the arguments that reach those refusals. Each exit below fills in only what
	// it wants the model to see.
	res := agenttool.Result{OriginAuthenticated: true}

	// Re-shape into the env-style locals the SEP-1913 annotation, relwrites and
	// label-extract blocks below read.
	var env struct {
		Result struct {
			Content []probe.ContentBlock
			IsError bool
			Meta    *resultMeta
		}
	}
	env.Result.Content = outcome.Content
	env.Result.IsError = outcome.IsError
	if len(outcome.Meta) > 0 {
		var rm resultMeta
		if json.Unmarshal(outcome.Meta, &rm) == nil {
			env.Result.Meta = &rm
		}
	}

	var content bytes.Buffer
	var uiSpec *agenttool.UIResourceSpec
	for _, b := range env.Result.Content {
		switch b.Type {
		case "text":
			content.WriteString(b.Text)
		case "image":
			fmt.Fprintf(&content, "[image: %s, %d bytes]", b.MIMEType, len(b.Data))
		case "resource":
			if spec, ok := uiResourceFrom(b, m.originName, m.toolName); ok {
				if uiSpec == nil && !env.Result.IsError {
					// Surface this widget: it's the first one and the call succeeded.
					uiSpec = spec
					// Trusted bytes only: m.llmName is the spec-derived tool name,
					// never server response data.
					fmt.Fprintf(&content, "[interactive widget rendered by %s; shown to the user in the session view]", m.llmName)
				} else {
					// A widget we will NOT surface (error result, or an additional
					// widget beyond the first). Say it exists but was not shown —
					// never falsely claim it is displayed.
					fmt.Fprintf(&content, "[interactive widget from %s returned but not shown]", m.llmName)
				}
			} else {
				fmt.Fprintf(&content, "[resource: %s]", b.URI)
			}
		default:
			fmt.Fprintf(&content, "[unknown content type: %s]", b.Type)
		}
		content.WriteString("\n")
	}

	// Shared post-effect snapshot, hoisted above both post-effect blocks so they
	// see one snapshot and the response body is unmarshalled once.
	//
	// argsMap is the already-validated server-arg portion of the envelope; a nil
	// map makes CEL `args.X` yield "no such key" rather than panic.
	// resultPayload is the first text content block parsed as JSON, best-effort:
	// a parse failure leaves it nil and CEL `has(result.X)` short-circuits.
	argsMap := a.Args
	var resultPayload map[string]any
	if len(env.Result.Content) > 0 && env.Result.Content[0].Type == "text" {
		_ = json.Unmarshal([]byte(env.Result.Content[0].Text), &resultPayload)
	}

	// SEP-1913 post-call inspection. The pre-call validator decision sees only
	// static annotations; here we look at response-level _meta.annotations
	// which carry the server's per-call findings.
	var ann *resultAnnotations
	if env.Result.Meta != nil {
		ann = env.Result.Meta.Annotations
	}
	if ann != nil {
		if ann.MaliciousActivityHint {
			// Operator log — full context including attribution. Both content
			// and attribution are server-controlled bytes and could carry
			// prompt-injection payloads, so neither reaches model context.
			logger.Warn("mcp.dispatch.maliciousActivityHint",
				"tool", m.llmName,
				"server", m.serverName,
				"attribution", ann.Attribution,
			)
			// The LLM-facing content contains ONLY hard-coded bytes: m.llmName
			// is a trusted spec-derived name, not server response data.
			res.Content = fmt.Sprintf("[mcp:%s] server flagged maliciousActivityHint=true on this response; content withheld from the model context.", m.llmName)
			res.IsError = true
			return res, nil
		}
		if len(ann.Attribution) > 0 {
			logger.Info("mcp.dispatch.attribution",
				"tool", m.llmName,
				"server", m.serverName,
				"attribution", ann.Attribution,
			)
		}
		// openWorldHint on a response is informational at this layer; the
		// runner's downstream policy may use it later.
	}

	// Write declared relationships from the response. Runs only on a SUCCESSFUL
	// tool call with declared blocks and a wired Writer.
	//
	// Failures here are LOAD-BEARING: these JIT writes seed the links a
	// downstream permission Check relies on, so a failure (CEL evaluation,
	// SpiceDB validation, network) is surfaced as IsError on the tool result
	// where the agent and the user see it. Logging it and returning the tool data
	// anyway produces a mysterious denial several steps later instead.
	if !env.Result.IsError && len(m.writesRelationships) > 0 && m.relWriter != nil {
		blocks := make([]relwrites.Block, 0, len(m.writesRelationships))
		for _, w := range m.writesRelationships {
			blocks = append(blocks, relwrites.Block{
				When:             w.When,
				ForEach:          w.ForEach,
				Exclusive:        w.Exclusive,
				RequireSlotBound: w.RequireSlotBound,
				Tuple: relwrites.Tuple{
					Resource: w.Tuple.Resource,
					Relation: w.Tuple.Relation,
					Subject:  w.Tuple.Subject,
				},
			})
		}
		written, relErr := relwrites.Run(ctx, m.relWriter, blocks, map[string]any{"args": argsMap, "result": resultPayload}, m.slotBoundChecker, func(msg string, kv ...any) {
			logger.Info(msg, append([]any{"tool", m.llmName, "server", m.serverName}, kv...)...)
		})
		// Audit what LANDED before deciding whether to abort. Run is partial by
		// construction: blocks are attempted independently, and a
		// requireSlotBound block's tuples are filtered one by one, so a call can
		// write real SpiceDB tuples AND return a refusal from the same Run.
		// Recording only on the all-clear path made the audit go quiet exactly
		// when the gate acted — and the gate turns partial refusal from a
		// CEL-bug curiosity into a routine outcome, so written-but-unaudited
		// would have become routine with it. relwrites_audit is the
		// tamper-evident answer to "what tuples did this session generate?"; it
		// must name what actually landed, refusal or no refusal.
		//
		// len(written) > 0 keeps an entry meaning "these tuples landed" — see
		// relwritesaudit.RecordWritten's own doc comment for why it also skips
		// an empty write. Shared with the sandbox dispatch path
		// (pkg/agent/tool/sandbox.evaluateWritesRelationships) so the two can
		// never diverge on what "a tuple landed" gets audited as.
		if sess != nil {
			scope := memorypkg.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
			if auditErr := relwritesaudit.RecordWritten(ctx, m.mem, scope, m.llmName, written); auditErr != nil {
				slog.Default().Info("relwritesaudit.Record",
					"session", scope.ID, "err", auditErr.Error())
			}
		}
		if relErr != nil {
			res.Content = fmt.Sprintf("%s: failed to register access-control relationships from the tool response — downstream permission checks would deny, so this call is aborted: %v", m.llmName, relErr)
			res.IsError = true
			return res, nil
		}
	}

	// Observations: what this result ASSERTS, recorded against the subjects the
	// same result named.
	//
	// Runs after the relationship writes and is guarded the same way — an
	// errored call asserts nothing. The failure handling differs from
	// relwrites' in cause but not in loudness. A relwrites failure aborts
	// because a downstream Check would otherwise deny for no visible reason; a
	// failure here means a fact was NOT recorded, which a precondition reads as
	// undetermined and denies with an actionable hint. Either way the agent
	// must see it: a fact that failed to record looks exactly like a fact
	// nobody tried to record, and an append-only conflict means a CONTRADICTION
	// was attempted, which must never be silent.
	if !env.Result.IsError && len(m.observes) > 0 && m.mem != nil && sess != nil {
		// Same expression the relwritesaudit.Record call above uses for the
		// memory scope — copied here rather than shared, because that call's
		// own `scope` local is block-scoped to the writesRelationships `if`
		// above and does not reach this one.
		scope := memorypkg.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

		// observedfact.Record's repeat-observation path reads the stored entry
		// back to tell an ordinary re-observation from a genuine contradiction
		// (RULING T1-A, factcontent.Record), and Local.Query checks ReadMemory
		// unconditionally regardless of Kind (facade.go:516) — unlike the
		// append-only Put itself, which needs no WriteMemory approval at all.
		// The relwritesaudit.Record call above never hits that branch on its
		// own first-ever write, so it never needed one; this call can. Nothing
		// upstream of Execute mints a ReadMemory approval onto ctx today, so
		// this mints its own the same way other in-process callers reach the
		// facade's data plane from outside a request handler (e.g.
		// pkg/authz/plangate/hold/tripper.go, pkg/controllers/toolcall/
		// snapshot.go) — WithSystemApproval only ADDS to ctx's approval set
		// (memory.WithApproval), so it can never revoke one the caller already
		// carried.
		recordCtx := memorypkg.WithSystemApproval(ctx, "mcp:observe")

		// `session` is bound to the same "<ns>/<name>" string the sandbox
		// path's sandboxCELVars produces, and which pkg/authz/observe's package
		// doc advertises as available. Binding it here is what makes an
		// observes block PORTABLE between the two dispatch paths in fact and
		// not merely in the documentation: an unbound key is nil-filled by
		// relwrites rather than rejected, so a block relying on it would fail
		// closed here while working on a sandbox tool — a difference nothing
		// would report. scope.ID above is that same string, reused rather than
		// re-concatenated so the two can never drift.
		vars := map[string]any{"args": argsMap, "result": resultPayload, "session": scope.ID}

		for i, o := range m.observes {
			obs, err := observe.Evaluate(fromcrd.FromCRD(o), vars)
			if err != nil {
				logger.Info("observe: evaluate failed", "tool", m.llmName, "server", m.serverName, "block", i, "err", err.Error())
				res.Content = fmt.Sprintf("%s: could not record what this result asserts (block %d): %v", m.llmName, i, err)
				res.IsError = true
				return res, nil
			}
			for _, one := range obs {
				// ToolUseID copies the relwritesaudit.Record call's own "" a few
				// lines above: Execute has no tool_use_id threaded through it
				// today (see tcArgs — only operation_id and _reason), so neither
				// call can populate one yet. ToolName still lets an auditor join
				// on WHICH tool produced the fact, even without a per-call id.
				one.Source = factcontent.Source{ToolName: m.llmName, ToolUseID: ""}
				if err := observedfact.Record(recordCtx, m.mem, scope, one); err != nil {
					logger.Info("observe: record failed", "tool", m.llmName, "server", m.serverName, "block", i, "err", err.Error())
					res.Content = fmt.Sprintf("%s: could not record what this result asserts (block %d): %v", m.llmName, i, err)
					res.IsError = true
					return res, nil
				}
				// Log the SUCCESS too, not only the failure. A fact recorded
				// about a type no slot declares is otherwise invisible from end
				// to end: the binder never reads that type (it asks only about
				// declared ones, by construction), so nothing downstream can
				// report the subject either. This one line is what makes
				// "I declared observes and the slot is still empty" — a
				// resourceType typo, a slot naming git_commit while the block
				// derives github_pr — diagnosable from logs.
				//
				// Names, never VALUES: a fact's value comes out of a tool
				// result and can carry anything the upstream server returned.
				// The subject and the fact names are what a reader needs to
				// join this against a slot declaration.
				for _, subj := range one.Subjects {
					logger.Info("observe: recorded",
						"tool", m.llmName, "server", m.serverName, "block", i,
						"resourceType", subj.ResourceType, "resourceID", subj.ResourceID,
						"facts", factcontent.FactNames(one.Facts))
				}
			}
		}
	} else if !env.Result.IsError && len(m.observes) > 0 {
		// A declared block that did NOT run: distinguish "skipped" from "nothing
		// declared" in the logs, because the len(m.observes)==0 case above is
		// silent by design (a tool with no observes block has nothing to say)
		// and would otherwise look identical to this one. Production always
		// wires m.mem and sess (internal/cmd/runner's session start), so this is
		// a defensive branch, not an expected path — which is exactly why it
		// must log rather than fall through unremarked.
		logger.Info("observe: declared blocks skipped — memory or session not wired",
			"tool", m.llmName, "server", m.serverName,
			"memWired", m.mem != nil, "sessWired", sess != nil)
	}

	// Slice: approval label resolution — extract (resourceType, id,
	// name) tuples from the tool response for approval-time
	// substitution. Non-fatal: failures log + skip; the tool result
	// is returned unchanged. Labels never reach any LLM.
	if !env.Result.IsError && len(m.labels) > 0 && m.labelSink != nil {
		for i, lb := range m.labels {
			block := labelextract.Block{
				When:    lb.When,
				ForEach: lb.ForEach,
				Tuple: labelextract.Tuple{
					ResourceType: lb.Label.ResourceType,
					ID:           lb.Label.ID,
					Name:         lb.Label.Name,
				},
			}
			extracted, err := labelextract.Evaluate(block, argsMap, resultPayload)
			if err != nil {
				logger.Info("labelextract: evaluate failed",
					"tool", m.llmName, "server", m.serverName,
					"block", i, "err", err.Error())
				continue
			}
			for _, e := range extracted {
				m.labelSink.Put(e.ResourceType, e.ID, e.Name)
			}
		}
	}

	res.Content = strings.TrimRight(content.String(), "\n")
	// env.Result.IsError is the SERVER's opinion of the arguments (a tool-level
	// error inside a 200), not a statement about the credential — which is why
	// the carrier stamped above rides out regardless of it. Without that, an
	// agent that keeps sending arguments it knows will fail keeps a dead
	// auth-failure observation alive.
	res.IsError = env.Result.IsError
	res.UIResource = uiSpec // set only for the first widget on a successful result (gated in the loop)
	return res, nil
}
