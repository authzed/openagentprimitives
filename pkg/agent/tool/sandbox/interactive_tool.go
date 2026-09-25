package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/registry"
)

// InteractiveHooks are supplied by the runner per session. They bridge the
// interactive tool's stdout/stderr to the channel and pull channel input
// back. The sandbox package stays transport-agnostic — the runner owns NATS.
type InteractiveHooks struct {
	// OnOutput publishes one non-terminal output chunk to the channel.
	OnOutput func(toolCallRef, stream string, data []byte)
	// OnTerminal publishes the final delta (Terminal: true) so the channel
	// can mark the session ended. exitReason is one of
	// completed | idle | maxDuration | failed.
	OnTerminal func(toolCallRef, exitReason string, exitCode int32)
	// OnEvent receives one parsed event from a toolkit-declared stream
	// parser, along with the dispatching call's `_reason` and the
	// dispatched tool's name (outerTool, e.g. "claude"). Set by the
	// runner alongside OnOutput; when nil (or the toolkit has no
	// streamFormat) the sandbox tool falls back to raw OnOutput.
	OnEvent func(toolCallRef, reason, outerTool string, ev toolkitstream.Event)
	// Register hands the runner a Feed(data) func keyed by toolCallRef, so
	// the runner's in.tool_session_input subscriber can route human input to
	// this bridge. The returned cancel removes the registration.
	Register func(toolCallRef string, feed func([]byte) error) (cancel func())
	// IdleTimeoutDefault / MaxDurationDefault apply when the call omits them.
	IdleTimeoutDefault time.Duration
	MaxDurationDefault time.Duration
	// BridgeDialOpts are extra gRPC dial options for the gateway connection.
	// Production callers leave this nil. Tests inject a bufconn dialer.
	BridgeDialOpts []grpc.DialOption
}

type interactiveHooksKey struct{}

// WithInteractiveHooks returns a context carrying hooks the interactive
// sandbox tool needs to bridge to the channel. The runner attaches them in
// dispatchToolUses; tests can inject their own.
func WithInteractiveHooks(ctx context.Context, hooks InteractiveHooks) context.Context {
	return context.WithValue(ctx, interactiveHooksKey{}, hooks)
}

// InteractiveHooksFromCtx returns hooks attached by WithInteractiveHooks.
func InteractiveHooksFromCtx(ctx context.Context) (InteractiveHooks, bool) {
	h, ok := ctx.Value(interactiveHooksKey{}).(InteractiveHooks)
	return h, ok
}

// interactiveArgs extends sandboxArgs with the agent-specified session knobs.
type interactiveArgs struct {
	// OperationID is the Operation this call is audited against.
	OperationID string `json:"operation_id"`
	// Reason is the LLM's stated justification, recorded in the audit entry.
	Reason string `json:"_reason"`
	// Args is the toolkit command's argv tail.
	Args []string `json:"args"`
	// IdleTimeout is a Go duration string; empty takes the toolspec default.
	IdleTimeout string `json:"idle_timeout,omitempty"`
	// MaxDuration is a Go duration string capping total run time.
	MaxDuration string `json:"max_duration,omitempty"`
	// PairWithHuman routes the session's stdin to the channel so a human can type.
	PairWithHuman bool `json:"pair_with_human,omitempty"`
}

// executeStreaming runs the streaming-bridge path shared by stream and
// interactive modes: create the ToolCall, poll for the gateway endpoint, run the
// bridge. Approval already happened — the tool declares external stateImpact, so
// dispatchToolUses gated it before Execute was called.
//
// Both modes parse and publish stdout/stderr identically, differing only in:
//   - the ToolCall spec: stream uses spec.timeout as its deadline; interactive
//     uses spec.idleTimeout + spec.maxDuration.
//   - stdin: interactive registers a Feed so the runner can route channel
//     input to the bridge; stream has a closed stdin (one-shot) and skips it.
func (s *SandboxTool) executeStreaming(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext, ids IDs, hooks InteractiveHooks) (tool.Result, error) {
	// Execute already gated out ModeSync, so streamMode() is stream or
	// interactive here.
	mode := s.opts.streamMode()
	interactive := mode == spiceboxv1alpha1.ToolCallModeInteractive

	var a interactiveArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tool.Result{Content: "interactive: invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if a.OperationID == "" {
		return tool.Result{
			Content: "interactive: operation_id is required — call new_operation first and pass its operation_id on every tool call",
			IsError: true,
		}, nil
	}
	if a.Reason == "" {
		return tool.Result{
			Content: "interactive: _reason is required — explain why this specific call is needed",
			IsError: true,
		}, nil
	}
	if sess == nil || sess.K8sClient == nil {
		return tool.Result{}, fmt.Errorf("interactive: SessionContext.K8sClient is nil")
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok {
		return tool.Result{}, fmt.Errorf("interactive: SessionContext.K8sClient does not satisfy controller-runtime client.Client")
	}
	bundleSession, ok := sess.BundleSessions[s.opts.BundleName]
	if !ok || bundleSession == "" {
		return tool.Result{Content: "interactive: no SpiceboxSession registered for bundle " + s.opts.BundleName, IsError: true}, nil
	}

	// Resolve effective timeouts: per-call > hooks defaults. These are only
	// USED in the interactive branch — stream mode's deadline is spec.timeout —
	// but resolving them unconditionally keeps the logic simple.
	idle := hooks.IdleTimeoutDefault
	if d, err := time.ParseDuration(a.IdleTimeout); err == nil && d > 0 {
		idle = d
	}
	maxDur := hooks.MaxDurationDefault
	if d, err := time.ParseDuration(a.MaxDuration); err == nil && d > 0 {
		maxDur = d
	}

	tcName := fmt.Sprintf("%s-%d-%s", sess.Name, ids.TurnIndex, normalizeToolCallNameSegment(ids.ToolUseID))
	tval := true
	ownerRefs := []metav1.OwnerReference{}
	if sess.AgentSessionUID != "" {
		ownerRefs = []metav1.OwnerReference{{
			APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:               "AgentSession",
			Name:               sess.Name,
			UID:                sess.AgentSessionUID,
			Controller:         &tval,
			BlockOwnerDeletion: &tval,
		}}
	}
	// Generate the gateway stream token here, in the runner. We commit ONLY
	// the hash to the ToolCall spec; the raw token preimage stays in this
	// local variable and is handed straight to the bridge below — it never
	// touches the API server. The operator reads spec.streamTokenHash,
	// registers the gateway stream with that hash, and the gateway hashes the
	// token the bridge presents to compare.
	streamToken, streamTokenHash, err := generateStreamTokenAndHash()
	if err != nil {
		return tool.Result{}, fmt.Errorf("interactive: generate stream token: %w", err)
	}

	tcSpec := spiceboxv1alpha1.ToolCallSpec{
		Session:         bundleSession,
		Tool:            s.opts.Suffix,
		Args:            a.Args,
		Mode:            mode,
		StreamTokenHash: streamTokenHash,
		// Static, non-secret env the toolkit declares for its own binary. This
		// is the path the Claude toolkits take — their only subcommand is
		// mode: stream — so the CLI's retry cap arrives here, not via the sync
		// dispatch.
		Env:                 toolkitEnvDefaults(s.opts.Toolkit, nil),
		Credentials:         s.opts.Credentials,
		PreDispatchSnapshot: ids.PreDispatch, // nil when not stateful
	}
	if interactive {
		tcSpec.IdleTimeout = metav1.Duration{Duration: idle}
		tcSpec.MaxDuration = metav1.Duration{Duration: maxDur}
	} else {
		// Stream mode's deadline is spec.timeout (the operator's
		// reconcileStreaming uses it). A one-shot streaming tool has no
		// idle/maxDuration knobs.
		tcSpec.Timeout = metav1.Duration{Duration: s.opts.Timeout}
	}
	tc := &spiceboxv1alpha1.ToolCall{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "ToolCall",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tcName,
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentsession":  sess.Name,
				"agentbundle":   s.opts.BundleName,
				"agenttoolspec": s.opts.ToolspecName,
				"ap.operation":  a.OperationID,
				// Raw tool_use ID — keeps the pre-dispatch snapshot's append-only
				// audit entry ID unique per dispatch (see the sandbox_tool.go
				// builder and LabelToolUseID's doc for why an empty value collides).
				spiceboxv1alpha1.LabelToolUseID: ids.ToolUseID,
			},
			Annotations: map[string]string{
				"ap.operation/reason": a.Reason,
			},
			OwnerReferences: ownerRefs,
		},
		Spec: tcSpec,
	}

	if err := c.Create(ctx, tc); err != nil && !errors.IsAlreadyExists(err) {
		return tool.Result{Content: "interactive: create ToolCall: " + err.Error(), IsError: true}, nil
	}
	callIdx, recorded := sess.Operations.RecordCall(a.OperationID, tool.OperationCall{
		Tool:       s.Name(),
		Reason:     a.Reason,
		ToolCallID: tcName,
	})
	if !recorded {
		return tool.Result{
			Content: fmt.Sprintf("interactive: operation %q disappeared between validation and dispatch", a.OperationID),
			IsError: true,
		}, nil
	}
	// Mark the call done on every exit path below — completion is defined as
	// this dispatch's Execute returning, even though the interactive session
	// itself may stay live via the streaming gateway (see report note).
	defer sess.Operations.CompleteCall(a.OperationID, callIdx)

	// Poll status.streaming for the gateway endpoint. The controller's
	// reconcileStreaming path populates Available + GatewayEndpoint once the
	// exec is registered. The status carries NO token — the raw token is the
	// streamToken local above, held only in this runner's memory.
	endpoint, perr := pollStreamingEndpoint(ctx, c, sess.Namespace, tcName, s.opts.PollInterval)
	if perr != nil {
		return tool.Result{Content: "interactive: " + perr.Error(), IsError: true}, nil
	}

	// Resolve a parser from the toolkit's declared StreamFormat. nil-safe:
	// no toolkit / no streamFormat / no registered factory → no parser → raw path.
	var parser toolkitstream.Parser
	if s.opts.Toolkit != nil && s.opts.Toolkit.StreamFormat != "" {
		if f, ok := registry.ByKind(s.opts.Toolkit.StreamFormat); ok {
			parser = f.New()
		}
		// If StreamFormat is set but unknown, the raw path still works;
		// a future internal/cmd/runner log call can warn about the missing factory.
	}

	// The bridge's idle timer differs by mode. Interactive uses the resolved
	// idle window — a parked-but-live session is normal there. Stream is a
	// one-shot run to completion: it has no legitimate idle state, so the only
	// idle bound is spec.timeout acting as a safety net; the bridge must not
	// idle-park a streaming tool prematurely.
	bridgeIdle := idle
	if !interactive {
		bridgeIdle = s.opts.Timeout
	}

	// Bounded in-memory capture folded into the returned tool.Result. stdout is
	// always captured: the parsed path prefers parser.Outcome().Text and falls
	// back to this tail when the parser yields no caller-facing text. stderr is
	// always captured for failure diagnostics.
	stdoutTail := &tailBuffer{max: maxStdoutTailBytes}
	stderrTail := &tailBuffer{max: maxStderrTailBytes}

	// Build + run the bridge. OnIdle deletes the ToolCall CR; the controller's
	// deletion path (Task 4) calls Registry.CancelAndUnregister, which stops
	// the exec, which unblocks the bridge's stream.Recv loop with Exit/EOF.
	br := NewBridge(BridgeConfig{
		GatewayAddr:  endpoint.GatewayEndpoint,
		Namespace:    sess.Namespace,
		ToolCallName: tcName,
		// The raw token from local memory — NOT from status (status has none).
		Token:       streamToken,
		IdleTimeout: bridgeIdle,
		OnOutput: func(stream string, data []byte) {
			switch stream {
			case "stderr":
				stderrTail.append(data)
			case "stdout":
				// Captured unconditionally: on the parsed path this is the
				// last-resort payload when the parser yields no caller-facing
				// text (see composeStreamResult), so the orchestrator is never
				// left with a bare status line. This only fills a buffer — the
				// routing below is unchanged, so parser-active stdout still
				// reaches OnEvent only, never OnOutput.
				stdoutTail.append(data)
			}
			// Stderr and parser-less stdout go through the raw path. The
			// parser is invoked only for stdout when both a parser AND the
			// OnEvent callback are present.
			if stream != "stdout" || parser == nil || hooks.OnEvent == nil {
				if hooks.OnOutput != nil {
					hooks.OnOutput(tcName, stream, data)
				}
				return
			}
			events, perr := parser.Parse(data)
			if perr != nil {
				// Defensive: parser shouldn't error in v1, but if it does,
				// fall back to raw so the user still sees output.
				if hooks.OnOutput != nil {
					hooks.OnOutput(tcName, stream, data)
				}
				return
			}
			for _, ev := range events {
				hooks.OnEvent(tcName, a.Reason, s.opts.Suffix, ev)
			}
		},
		OnIdle: func() {
			// Best-effort delete; controller deletion path cancels the exec.
			_ = c.Delete(context.Background(), tc)
		},
	})
	br.dialOpts = hooks.BridgeDialOpts
	// Only interactive mode wires the stdin Feed: the runner routes channel
	// input to the bridge. Stream mode is one-shot with a closed stdin (the
	// operator's reconcileStreaming uses streamingStdin), so it skips Register.
	if interactive {
		unregister := hooks.Register(tcName, br.Feed)
		defer unregister()
	}

	res, runErr := br.Run(ctx)
	// Flush parser.Done() before signalling terminal so any final
	// synthesized event (e.g. a result the agent omitted) reaches the
	// channel before "session ended".
	if parser != nil && hooks.OnEvent != nil {
		if evs, derr := parser.Done(); derr == nil {
			for _, ev := range evs {
				hooks.OnEvent(tcName, a.Reason, s.opts.Suffix, ev)
			}
		}
	}
	if runErr != nil {
		if hooks.OnTerminal != nil {
			hooks.OnTerminal(tcName, "failed", -1)
		}
		content := "interactive: bridge: " + runErr.Error()
		if st := stderrTail.Bytes(); len(st) > 0 {
			content += "\n--- stderr (tail) ---\n" + string(st)
		}
		return tool.Result{
			Content:  content,
			IsError:  true,
			Terminal: true,
		}, nil
	}
	if hooks.OnTerminal != nil {
		hooks.OnTerminal(tcName, res.ExitReason, res.ExitCode)
	}

	var oc *toolkitstream.Outcome
	if parser != nil {
		o := parser.Outcome()
		oc = &o
	}
	return composeStreamResult(res, oc, stdoutTail.Bytes(), stderrTail.Bytes()), nil
}

const (
	// maxStdoutTailBytes / maxStderrTailBytes bound the in-memory capture of a
	// streaming tool's output before it is folded into the tool.Result returned
	// to the orchestrator. Belt-and-suspenders: the toolguard data-volume cap
	// still applies to the returned content downstream.
	maxStdoutTailBytes = 16 << 10
	maxStderrTailBytes = 4 << 10
)

// tailBuffer keeps at most max trailing bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) append(p []byte) {
	if t.max <= 0 {
		return
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

// composeStreamResult builds the tool.Result returned to the orchestrator when a
// streaming/interactive tool exits cleanly (not via a bridge/transport error).
//
// The returned Content is the inner tool's OWN output — its final answer
// (outcome.Text) when the parser recognized one, otherwise its raw stdout tail
// (the parser-less path, or a parser that yielded no text) — under a one-line
// status header. On failure the bounded stderr tail is appended so the
// orchestrator learns why (no-silent-errors).
//
// Only "idle" carries Terminal=true (IdleExit parks the session);
// "completed"/"failed"/"maxDuration" return an ordinary tool_result so the agent
// can react to the now-substantive content.
//
// It also stamps the credential-update observation carriers (Result.ExitCode /
// .Stderr / .OriginAuthenticated). A toolkit's authFailure block is declared per
// PROVIDER, not per ToolCall mode, so observing only the sync path would make
// corroboration depend on a mode flag unrelated to credentials.
//
// The carrier gates are deliberately NOT `isErr`:
//
//   - Stderr is carried only when the PROCESS failed (ExitReason "failed").
//     isErr is wider — the stream parser also raises it for a recognized
//     terminal result with OK=false on a process that exited 0 — and gating on
//     it would hand the classifier a cleanly-exited process's stderr, which the
//     sync path refuses via !toolCallSucceeded. "idle" and "maxDuration" are OUR
//     watchdog killing the process, so their stderr is not evidence either.
//   - ExitCode drops a negative value, the bridge's own sentinel (below).
//   - OriginAuthenticated needs a process that ran to completion and exited 0 —
//     narrower than !isErr, because a clean exit whose parser reported OK=false
//     is an error about the WORK, not the credential, and must still retract a
//     stale auth-failure observation.
//   - UnbilledFailure carries the credential-HALT observation, which the tool
//     guard answers by ending the session. It takes the same "the process
//     failed" footing as Stderr and yields to OriginAuthenticated, and its
//     claim comes from the toolkit's own structured terminal event — never from
//     the stdout or stderr tails this function also holds.
func composeStreamResult(res BridgeResult, outcome *toolkitstream.Outcome, stdoutTail, stderrTail []byte) tool.Result {
	isErr := res.ExitReason == "failed"
	var payload string
	if outcome != nil {
		payload = outcome.Text
		if outcome.HasResult && !outcome.OK {
			isErr = true
		}
	}
	// Fall back to the raw stdout tail whenever no caller-facing text is
	// available — either the toolkit declared no stream parser, or the parser
	// recognized no terminal result text. The orchestrator gets the tool's own
	// output rather than a bare status line.
	if payload == "" {
		payload = string(stdoutTail)
	}

	var b strings.Builder
	b.WriteString(statusLine(res, outcome, isErr))
	if payload != "" {
		b.WriteString("\n")
		b.WriteString(payload)
	}
	if isErr && len(stderrTail) > 0 {
		b.WriteString("\n--- stderr (tail) ---\n")
		b.Write(stderrTail)
	}

	out := tool.Result{
		Content: b.String(),
		IsError: isErr,
		// The bridge streamed every byte of this Content to the user as it was
		// produced (the tool-session block in web chat, the per-ToolCallRef
		// message in Slack), and the status line above it repeats the outcome
		// their footer showed. That is the whole claim RenderedLive makes, and
		// this is the only place it can honestly be made: a caller-facing
		// composition of a run the user watched. It is set on the failure path
		// too — a failed run is precisely the one the live surfaces keep
		// expanded, so hiding it on reload would lose the most-wanted output.
		RenderedLive: true,
		Terminal:     res.ExitReason == "idle",
		IdleExit:     res.ExitReason == "idle",
	}
	// A negative code is the BRIDGE's sentinel for "no Exit message arrived"
	// (BridgeResult is initialized to -1 and only an Exit frame overwrites it);
	// the gateway sources a real Exit.Code from the process wait status, which
	// is never negative. Leaving it unobserved rather than carrying -1 keeps
	// "nothing was seen" distinguishable from a genuine exit code.
	if res.ExitCode >= 0 {
		exit := res.ExitCode
		out.ExitCode = &exit
	}
	// Both halves are required: "completed" alone is the bridge's INITIAL
	// state (it ships with ExitCode -1 and only an Exit frame overwrites it),
	// so a run whose exit was never observed would otherwise claim the
	// credential worked.
	if res.ExitReason == "completed" && res.ExitCode == 0 {
		out.OriginAuthenticated = true
	}
	if res.ExitReason == "failed" {
		out.Stderr = string(stderrTail)
	}
	// The credential-halt observation, gated on the same "the PROCESS failed"
	// footing as Stderr and against the positive OriginAuthenticated above:
	//
	//   - ExitReason "failed" only. "idle" and "maxDuration" are OUR watchdog
	//     ending the run, so whatever the toolkit had reported by then is not
	//     the provider's verdict on a credential; "completed" means the process
	//     ran to its own end, which a run refused at the auth layer does not.
	//   - Never when OriginAuthenticated was concluded. That is positive
	//     evidence the credential worked and outranks a shape, the same
	//     precedence credupdate.IsAuthShaped applies — the two carriers must
	//     not contradict each other on one result.
	//
	// The claim itself is Outcome.UnbilledFailure's, computed from the
	// toolkit's structured terminal event alone; the stdout and stderr tails in
	// scope here are deliberately not consulted.
	if res.ExitReason == "failed" && !out.OriginAuthenticated && outcome != nil {
		out.UnbilledFailure = outcome.UnbilledFailure()
	}
	return out
}

// statusWord maps the bridge outcome to a human status token.
func statusWord(res BridgeResult, isErr bool) string {
	if isErr {
		return "failed"
	}
	if res.ExitReason == "completed" {
		return "success"
	}
	return res.ExitReason // idle, maxDuration
}

// statusLine renders the one-line header. It prefers the parsed metadata
// (duration/cost) when the tool reported a terminal result; otherwise it falls
// back to the process exit code.
func statusLine(res BridgeResult, outcome *toolkitstream.Outcome, isErr bool) string {
	word := statusWord(res, isErr)
	if outcome != nil && outcome.HasResult {
		dur := time.Duration(outcome.DurationMs) * time.Millisecond
		return fmt.Sprintf("status: %s (%s, $%.2f)", word, dur, outcome.CostUSD)
	}
	return fmt.Sprintf("status: %s (exit %d)", word, res.ExitCode)
}

// pollStreamingEndpoint waits for tc.Status.Streaming to populate. Returns the
// endpoint once Available=true, or an error if ctx expires. The status carries
// no token — the runner holds the raw token in memory; only GatewayEndpoint is
// read here.
func pollStreamingEndpoint(ctx context.Context, c client.Client, ns, name string, interval time.Duration) (*spiceboxv1alpha1.StreamingEndpoint, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var fresh spiceboxv1alpha1.ToolCall
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &fresh); err == nil {
			if fresh.Status.Streaming != nil && fresh.Status.Streaming.Available {
				return fresh.Status.Streaming, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for status.streaming: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// generateStreamTokenAndHash mints a gateway stream token. It returns the raw
// token (base64url of 32 random bytes — kept in runner memory and handed to
// the bridge) and its hex-encoded SHA-256 commitment (stamped onto
// ToolCall.spec.streamTokenHash; the operator and gateway only ever see this).
func generateStreamTokenAndHash() (token, tokenHash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}
