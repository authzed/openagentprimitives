// tools_testrun.go implements test_tool, read_test_log and stop_test: the
// builder session's ways to exercise a candidate AgentClass without a human
// running it directly through this sidecar. test_tool delegates a bounded
// single_turn task to it and reads back what the child DID (its tool calls
// and results), never what it said. It creates a SubagentRequest naming
// this builder session (B/X) as parent, in the workshop namespace W, waits
// for the child to reach a terminal phase, and reads its transcript back
// through s.readChildToolResults (transcript.go). The sidecar holds no
// delegate verb of its own beyond this CR create — the SubagentRequest
// controller (pkg/controllers/subagentrequest) validates roster membership
// and budget and creates the actual child session, exactly as it does for
// any other delegating runner.
//
// read_test_log and stop_test act on the PERSON'S OWN test session, not a
// delegated child: tools_trytest.go's test_link/watch_test/test_sessions
// are how that session gets started and found by name; this file only
// reads its transcript and deletes it. The attended, builder-delegated
// run_test tool that used to fill this role is gone — a child running AS
// the person widens the builder's own identity, which the delegation gates
// refuse, so the person now starts their own test themselves (see
// tools_trytest.go's own doc).
package workshopmcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Tool names announced on the MCP surface.
const (
	toolTestTool    = "test_tool"
	toolReadTestLog = "read_test_log"
	toolStopTest    = "stop_test"
)

// testerPreamble is prepended to every test_tool request so the child
// session — a real delegation, not a rehearsal — treats the instruction as
// a bounded test rather than an open-ended conversation, and stops instead
// of asking a clarifying question a single_turn child has no channel to
// ask anyway.
const testerPreamble = "You are being tested. Carry out the following request using your tools, then stop. " +
	"Do not ask questions or take any action beyond what the request names.\n\nRequest: "

// subagentPollInterval governs awaitSubagentTerminal's re-Get cadence. A
// package var, not a const, so a test can shrink it rather than waiting
// out the real interval against a fake client that never advances a
// SubagentRequest's phase on its own — mirrors probePollInterval
// (tools_probe.go).
var subagentPollInterval = 500 * time.Millisecond

// subagentTestTimeout bounds how long test_tool waits for its single_turn
// child to reach a terminal phase. A single_turn child may call several
// tools across a real model turn before it returns, so this is more
// generous than probeHTTPTimeout's plain HTTP round trip — but still
// bounded, so a stuck or wedged child cannot stall the builder turn
// indefinitely.
const subagentTestTimeout = 5 * time.Minute

// registerTests wires the workshop's delegated-test tool (test_tool) and the
// two tools that read/stop the person's OWN test session (read_test_log,
// stop_test) onto mcpSrv. test_link/watch_test/test_sessions — how that
// session gets started and found — are tools_trytest.go's registerTryTest.
func (s *Server) registerTests(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolTestTool,
		Description: "Delegate a bounded, single-turn task to a candidate AgentClass and return what it " +
			"DID — its tool calls and their results — never its prose — together with how it ended: " +
			"`phase`, `failureReason`, `failureMessage` and `auditEntries`. Use this to verify a class you are " +
			"building actually works before declaring it finished. A roster/ceiling denial or a webhook " +
			"refusal surfaces as an error naming the reason; a child that failed to even start still returns " +
			"successfully, with an empty `toolRecords` and its own verdict in the other fields — read those " +
			"before concluding anything from an empty list.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"class": map[string]any{
					"type":        "string",
					"description": "The AgentClass to delegate to — must appear in this builder's own subagents roster.",
				},
				"request": map[string]any{
					"type":        "string",
					"description": "The task to hand the child, appended verbatim after a fixed tester preamble.",
				},
			},
			"required": []any{"class", "request"},
		},
	}, s.handleTestTool)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolReadTestLog,
		Description: "Read a test run's own tool-call/tool-result records — never its prose — together " +
			"with how it ended: `phase`, `failureReason`, `failureMessage` and `auditEntries`. " +
			"session is a name from test_sessions; use this while the person's test is still ongoing " +
			"to see what it has done so far. An empty `toolRecords` does NOT mean nothing happened — " +
			"a run that was stopped before it could call anything records the reason in the same " +
			"result, so read `failureMessage` and `auditEntries` before concluding anything.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "The test session's name, from test_sessions.",
				},
			},
			"required": []any{"session"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleReadTestLog)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolStopTest,
		Description: "Stop the person's test session, from test_sessions; the conversation returns to " +
			"you. This deletes the test conversation and its log, so read the log first (read_test_log). " +
			"Stopping a session that has already ended is not an error.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "The test session's name, from test_sessions.",
				},
			},
			"required": []any{"session"},
		},
	}, s.handleStopTest)
}

// testToolArgs is test_tool's own argument shape.
type testToolArgs struct {
	Class   string `json:"class"`
	Request string `json:"request"`
}

// handleTestTool answers the `test_tool` tool call.
func (s *Server) handleTestTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a testToolArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("test_tool: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Class) == "" || strings.TrimSpace(a.Request) == "" {
		return s.toolErr("test_tool: class and request are both required"), nil
	}

	sessNS, sessName := s.sessionRef()
	sr := &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-tool-",
			Namespace:    s.Identity.Namespace,
		},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent: spiceboxv1alpha1.NamespacedRef{
				Namespace: sessNS,
				Name:      sessName,
			},
			Class: a.Class,
			Task:  testerPreamble + a.Request,
			Mode:  spiceboxv1alpha1.SubagentModeSingleTurn,
		},
	}
	if err := s.K8s.Create(ctx, sr); err != nil {
		if isDeniedErr(err) {
			// A webhook/apiserver refusal on Create itself — e.g. the runner
			// identity check in pkg/controllers/webhooks/subagentrequest.
			// Surfaced verbatim, never re-worded.
			return s.deniedResult(err), nil
		}
		return s.toolErr("test_tool: creating test request: %v", err), nil
	}

	// Create succeeding is NOT proof the child ran: roster membership and
	// budget are validated by the SubagentRequest controller AFTER the
	// object exists, and a Denied phase there is a separate, later signal
	// from a Create-time webhook refusal.
	done, err := s.awaitSubagentTerminal(ctx, sr.Namespace, sr.Name)
	if err != nil {
		return s.toolErr("test_tool: waiting for the test child: %v", err), nil
	}
	if done.Status.Phase == spiceboxv1alpha1.SubagentRequestPhaseDenied {
		return s.toolErr("test_tool: the test was refused: %s", done.Status.FailureReason), nil
	}
	if done.Status.ChildRef == nil {
		return s.toolErr("test_tool: the test child never started"), nil
	}

	log, err := s.readChildToolResults(ctx, done.Status.ChildRef.Name)
	if err != nil {
		return s.toolErr("test_tool: reading the child's tool-results: %v", err), nil
	}
	out := map[string]any{
		"child":        done.Status.ChildRef.Name,
		"toolRecords":  log.Records,
		"auditEntries": log.Audit,
	}
	s.sessionVerdictFields(ctx, done.Status.ChildRef.Namespace, done.Status.ChildRef.Name, out)
	return s.jsonResult(out)
}

// awaitSubagentTerminal polls the SubagentRequest ns/name's own status (by
// re-Get, never watch — a short-lived, single-shot wait inside one tool
// call, not a controller) until status.phase reaches a terminal value
// (Succeeded, Failed or Denied), or subagentTestTimeout elapses — whichever
// comes first. Mirrors awaitProbe's shape (tools_probe.go:281). The sidecar
// never runs the child session itself; the SubagentRequest controller
// (pkg/controllers/subagentrequest) does, and this only watches for it to
// finish.
func (s *Server) awaitSubagentTerminal(ctx context.Context, ns, name string) (*spiceboxv1alpha1.SubagentRequest, error) {
	pollCtx, cancel := context.WithTimeout(ctx, subagentTestTimeout)
	defer cancel()

	key := client.ObjectKey{Namespace: ns, Name: name}
	for {
		var current spiceboxv1alpha1.SubagentRequest
		if err := s.K8s.Get(ctx, key, &current); err != nil {
			return nil, fmt.Errorf("re-reading SubagentRequest %s: %w", key, err)
		}
		// IsTerminal() is the canonical Succeeded/Failed/Denied predicate on the
		// type — reuse it rather than restating the phase set here, where a future
		// terminal phase would silently be missed and poll to a timeout.
		if current.IsTerminal() {
			return &current, nil
		}
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf(
				"timed out waiting for SubagentRequest %s to reach a terminal phase (last observed phase: %q)",
				key, current.Status.Phase)
		case <-time.After(subagentPollInterval):
		}
	}
}

// readTestLogArgs is read_test_log's own argument shape.
type readTestLogArgs struct {
	Session string `json:"session"`
}

// handleReadTestLog answers the `read_test_log` tool call. Unlike
// test_tool's own transcript read, this never waits for a terminal phase —
// session names the person's own test session (from test_sessions), which
// may still be running, and the point of this tool is to let the builder
// watch it AS it runs, not only after it finishes.
//
// The result carries FOUR things, and the last three exist because the first
// one alone is not an account of a run. `toolRecords` is what the session did;
// an empty list is a real and common state — a session refused at start calls
// nothing — and a reader handed only that concludes nothing happened and stops
// the session, destroying the only evidence. So the result also carries the
// session's own verdict (`phase`, `failureReason`, `failureMessage`) and its
// signed transitions (`auditEntries`), which say what happened to it.
//
// A session whose state cannot be read says so under `stateUnreadable` rather
// than omitting the verdict: an absent field reads as "nothing went wrong",
// which is the failure this tool exists to stop making.
func (s *Server) handleReadTestLog(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a readTestLogArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("read_test_log: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Session) == "" {
		return s.toolErr("read_test_log: session is required"), nil
	}
	log, err := s.readChildToolResults(ctx, a.Session)
	if err != nil {
		return s.toolErr("read_test_log: reading the test log: %v", err), nil
	}
	out := map[string]any{
		"session":      a.Session,
		"toolRecords":  log.Records,
		"auditEntries": log.Audit,
	}
	s.sessionVerdictFields(ctx, s.Identity.Namespace, a.Session, out)
	return s.jsonResult(out)
}

// sessionVerdictFields reads namespace/name's own AgentSession status and
// merges its verdict into out: `phase`, and — only when the session actually
// failed — `failureReason` and `failureMessage`. read_test_log and test_tool
// both report this about a run, from the same source, so a run that failed
// before calling anything is never silently indistinguishable from one that
// simply hasn't done anything yet.
//
// A session whose state cannot be read reports a fixed sentence under
// `stateUnreadable` rather than the raw apiserver error: the caller is an MCP
// tool result a builder session may relay to a person, and the apiserver's
// own wording carries infrastructure words (resource groups, API paths) that
// must never reach that person. The raw error is logged instead, so an
// operator can still diagnose it.
func (s *Server) sessionVerdictFields(ctx context.Context, namespace, name string, out map[string]any) {
	var sess spiceboxv1alpha1.AgentSession
	if gerr := s.K8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sess); gerr != nil {
		slog.Default().Info("workshopmcp: a test session's state could not be read",
			"namespace", namespace, "name", name, "err", gerr.Error())
		out["stateUnreadable"] = "the test's state could not be read right now"
		return
	}
	out["phase"] = sess.Status.Phase
	if sess.Status.FailureReason != "" {
		out["failureReason"] = sess.Status.FailureReason
	}
	if msg := failedConditionMessage(&sess); msg != "" {
		out["failureMessage"] = msg
	}
}

// failedConditionMessage returns the human-readable half of a session's
// failure — the Failed condition's message, which is where the runner and the
// operator both put the words that say what to do about it. The status's
// failureReason is only the classification.
func failedConditionMessage(sess *spiceboxv1alpha1.AgentSession) string {
	if c := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed); c != nil {
		return c.Message
	}
	return ""
}

// stopTestArgs is stop_test's own argument shape.
type stopTestArgs struct {
	Session string `json:"session"`
}

// handleStopTest answers the `stop_test` tool call: it deletes the named
// AgentSession — the person's own root test session, found via
// test_sessions — which is what ends the test and returns the conversation
// to the builder. Deleting a session that is already gone — the person
// already ended it, or a previous stop_test call already succeeded — is
// reported as success, not an error: the caller's goal ("this test is not
// running") is already true.
func (s *Server) handleStopTest(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a stopTestArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("stop_test: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Session) == "" {
		return s.toolErr("stop_test: session is required"), nil
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Identity.Namespace,
			Name:      a.Session,
		},
	}
	if err := s.K8s.Delete(ctx, sess); err != nil {
		if apierrors.IsNotFound(err) {
			return s.jsonResult(map[string]any{"session": a.Session, "status": "already stopped"})
		}
		return s.toolErr("stop_test: stopping the test: %v", err), nil
	}
	return s.jsonResult(map[string]any{"session": a.Session, "status": "stopped"})
}
