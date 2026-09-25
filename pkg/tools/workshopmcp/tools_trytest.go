// tools_trytest.go implements `test_link`, `watch_test` and `test_sessions`:
// the person tests the agent they are building by starting it THEMSELVES,
// as a root session in the workshop namespace, from the browser chat —
// never as a delegated child of this builder (a child running as the person
// widens the builder's identity, which the delegation gates refuse). The
// builder paints the link (ap:agentlink from test_link's result), asks the
// Workshop controller to watch (watch_test → spec.testWatch; the controller
// wakes the builder as the test starts, pauses, ends or times out), and
// finds the test session by name (test_sessions) to read its log.
package workshopmcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Tool names announced on the MCP surface for this file's three tools.
const (
	toolTestLink     = "test_link"
	toolWatchTest    = "watch_test"
	toolTestSessions = "test_sessions"

	// testWatchDefault/testWatchCeiling bound watch_test's timeoutMinutes:
	// unset defaults to 30 minutes, and anything larger is capped at 120 —
	// the builder's own tool call returns immediately either way (the watch
	// itself lives on the Workshop CR, run by the controller), so this is a
	// ceiling on how long the controller keeps polling, not on how long the
	// builder's turn blocks.
	testWatchDefault = 30 * time.Minute
	testWatchCeiling = 120 * time.Minute

	// testWatchDefaultMinutes/testWatchCeilingMinutes are the same two bounds,
	// as raw minutes, for clamping the caller's int arg BEFORE it is ever
	// converted to a time.Duration — see handleWatchTest.
	testWatchDefaultMinutes = int(testWatchDefault / time.Minute)
	testWatchCeilingMinutes = int(testWatchCeiling / time.Minute)

	// The two labels test_link hands the browser for its "Try it" button,
	// keyed on whether the class under test acts as the person or as itself.
	tryLabelAsYourself = "Try it as yourself"
	tryLabel           = "Try it"
)

// registerTryTest wires test_link, watch_test and test_sessions onto mcpSrv.
func (s *Server) registerTryTest(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolTestLink,
		Description: "The props for an ap:agentlink that opens the browser chat's new-session dialog with the " +
			"agent under construction preselected and prompt prefilled, so the person tests it as themselves. " +
			"Paint the result as {component:\"ap:agentlink\", props:<this result>} in the testRun hook. The label " +
			"says 'Try it as yourself' when the agent acts as each person, 'Try it' otherwise.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"class":  map[string]any{"type": "string", "description": "The agent (an AgentClass in this workshop) to try."},
			"prompt": map[string]any{"type": "string", "description": "The opening request to prefill — the brief's first example."},
		}, "required": []any{"class", "prompt"}},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleTestLink)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolWatchTest,
		Description: "Ask to be told when the person's own test of an agent starts, pauses, resumes, ends, or times out. " +
			"Call it right after painting the test link, then wait with await_user_message: each event " +
			"arrives as a message in this conversation. timeoutMinutes defaults to 30 and is capped at 120; " +
			"tell the person how long you will watch. Calling it again while the person's test is still " +
			"running keeps watching THAT test and only extends the deadline; otherwise it starts a new " +
			"watch. The result's `keptWatching` says which happened.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"class":          map[string]any{"type": "string"},
			"timeoutMinutes": map[string]any{"type": "integer"},
		}, "required": []any{"class"}},
	}, s.handleWatchTest)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolTestSessions,
		Description: "The person's own test sessions of an agent in this workshop, newest last: name, ref, phase, " +
			"when it started, when it was last active. Use the name with read_test_log and stop_test, and the " +
			"ref as an ap:chat's sessionRef.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"class": map[string]any{"type": "string"},
		}, "required": []any{"class"}},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleTestSessions)
}

// testLinkArgs is test_link's own argument shape.
type testLinkArgs struct {
	Class  string `json:"class"`
	Prompt string `json:"prompt"`
}

// workshopClass looks up the named AgentClass in this workshop's own
// namespace. test_link (to read identityMode) and watch_test (only to
// confirm the class exists before recording a watch on it) both need
// exactly this Get; this is the one place that does it.
func (s *Server) workshopClass(ctx context.Context, name string) (*spiceboxv1alpha1.AgentClass, error) {
	var ac spiceboxv1alpha1.AgentClass
	if err := s.K8s.Get(ctx, client.ObjectKey{Namespace: s.Identity.Namespace, Name: name}, &ac); err != nil {
		return nil, err
	}
	return &ac, nil
}

// handleTestLink answers the `test_link` tool call: it looks up the named
// AgentClass in this workshop (purely to read its identityMode; test_link
// creates nothing) and returns the ap:agentlink props the builder paints so
// the person can start their OWN session of it. A class not yet authored in
// this workshop is a tool error naming it, never a link to nowhere.
func (s *Server) handleTestLink(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a testLinkArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("test_link: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Class) == "" || strings.TrimSpace(a.Prompt) == "" {
		return s.toolErr("test_link: class and prompt are both required"), nil
	}

	ac, err := s.workshopClass(ctx, a.Class)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return s.toolErr("test_link: no agent %q in this workshop", a.Class), nil
		}
		return s.toolErr("test_link: reading the agent: %v", err), nil
	}

	label := tryLabel
	if ac.Spec.IdentityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		label = tryLabelAsYourself
	}
	return s.jsonResult(map[string]any{
		"namespace":  s.Identity.Namespace,
		"agentClass": a.Class,
		"prompt":     a.Prompt,
		"label":      label,
	})
}

// watchTestArgs is watch_test's own argument shape.
type watchTestArgs struct {
	Class          string `json:"class"`
	TimeoutMinutes int    `json:"timeoutMinutes"`
}

// handleWatchTest answers the `watch_test` tool call: it records the watch
// on THIS builder session's own Workshop CR (spec.testWatch), which the
// Workshop controller (pkg/controllers/workshop/testwatch.go) picks up on
// its next reconcile and turns into wake-ups on this session's transcript
// as the person's test starts, pauses, resumes, ends, or times out.
//
// The call is one of two things, and watchStillCoversItsTest decides which.
// A KEPT watch — "Keep watching" on a test the person is still running —
// holds the watch's (class, startedAt) identity and moves only the deadline,
// so the controller goes on following the same test. Anything else is a new
// watch with a fresh startedAt, which the controller reads as a new record
// rather than inheriting the previous watch's deliveries. The result says
// which in `keptWatching`, which is named for the LEASE it reports and for
// nothing else: the watcher delivers "The person resumed the test." as its
// own separate event, about a person coming back to a paused test, and the
// builder reads both — so this field's name must not be one it could take
// for that.
func (s *Server) handleWatchTest(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a watchTestArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("watch_test: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Class) == "" {
		return s.toolErr("watch_test: class is required"), nil
	}

	if _, err := s.workshopClass(ctx, a.Class); err != nil {
		if apierrors.IsNotFound(err) {
			return s.toolErr("watch_test: no agent %q in this workshop", a.Class), nil
		}
		return s.toolErr("watch_test: reading the agent: %v", err), nil
	}

	// Clamp the raw minutes BEFORE converting to a Duration: multiplying an
	// unclamped, caller-supplied int by time.Minute can overflow a
	// time.Duration (an int64 of nanoseconds) for a sufficiently large
	// TimeoutMinutes, wrapping to a negative duration that would slip past a
	// post-conversion min() clamp entirely.
	minutes := a.TimeoutMinutes
	if minutes < 1 {
		minutes = testWatchDefaultMinutes
	} else if minutes > testWatchCeilingMinutes {
		minutes = testWatchCeilingMinutes
	}
	timeout := time.Duration(minutes) * time.Minute
	now := time.Now().UTC()
	deadline := now.Add(timeout)

	sessNS, sessName := s.sessionRef()
	key := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	keptWatching := false
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Reset per attempt: a conflict retry re-reads the workshop, and the
		// answer it gives may differ from the one the losing attempt got.
		keptWatching = false
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		startedAt := metav1.NewTime(now)
		keep, err := s.watchStillCoversItsTest(ctx, &ws, a.Class)
		if err != nil {
			return err
		}
		if keep {
			startedAt = ws.Spec.TestWatch.StartedAt
			keptWatching = true
		}
		ws.Spec.TestWatch = &spiceboxv1alpha1.WorkshopTestWatch{
			Class:     a.Class,
			StartedAt: startedAt,
			Deadline:  metav1.NewTime(deadline),
		}
		return s.K8s.Update(ctx, &ws)
	}); err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("watch_test: recording the watch: %v", err), nil
	}
	return s.jsonResult(map[string]any{
		"class":    a.Class,
		"deadline": deadline.Format(time.RFC3339),
		"status":   "watching",
		// keptWatching tells the builder which of the two this was: true means
		// the person's existing test is still being watched (so the page keeps
		// the chat and the running state it already shows), false means a new
		// watch that has yet to see any test.
		"keptWatching": keptWatching,
	})
}

// watchStillCoversItsTest answers whether a watch_test call for this class
// should KEEP the watch already on the workshop rather than begin a new one.
//
// The controller identifies a watch by (class, startedAt) and ignores every
// session created before startedAt (qualifyingTestSession,
// pkg/controllers/workshop/testwatch.go), so stamping the current time onto a
// re-arm — which is exactly what "Keep watching" after a timeout is — arms a
// watch that can never see the test the person is still running. Keeping the
// identity is right precisely when the recorded test session is still there
// and has not finished: the rest of THAT test — its pauses, its ending — is
// still ahead.
//
// Everything else is a new watch: no watch on this class, no session recorded
// against it yet, the session gone (workshop_stop_test deletes it), or a
// session that already reached the terminal set the watcher treats as the
// test being over.
func (s *Server) watchStillCoversItsTest(ctx context.Context, ws *spiceboxv1alpha1.Workshop, class string) (bool, error) {
	want := ws.Spec.TestWatch
	if want == nil || want.Class != class {
		return false, nil
	}
	st := ws.Status.TestWatch
	if st == nil || st.Class != class || !st.StartedAt.Time.Equal(want.StartedAt.Time) || st.Session == "" {
		return false, nil
	}

	var sess spiceboxv1alpha1.AgentSession
	sessKey := client.ObjectKey{Namespace: s.Identity.Namespace, Name: st.Session}
	if err := s.K8s.Get(ctx, sessKey, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		// Returned, never swallowed into "start fresh": a read that failed
		// says nothing about whether the test is running, and guessing would
		// silently re-arm the broken watch this whole function exists to fix.
		return false, fmt.Errorf("reading the recorded test session %s: %w", sessKey, err)
	}
	switch sess.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.AgentSessionPhaseFailed:
		return false, nil
	}
	return true, nil
}

// testSessionsArgs is test_sessions's own argument shape.
type testSessionsArgs struct {
	Class string `json:"class"`
}

// handleTestSessions answers the `test_sessions` tool call: every AgentSession
// in the workshop namespace of the named class, started by the SAME person who
// started this builder (this workshop's own spec.starterCanonical), oldest
// first — the same (class, starter) filter
// pkg/controllers/workshop/testwatch.go's qualifyingTestSession applies,
// minus that function's additional "created after the watch began" bound,
// since this tool is a general lookup for read_test_log/stop_test, not
// scoped to one particular watch.
//
// spec.starterCanonical is +optional, and StartedByCanonical returns "" for
// a session with no started-by annotation. An empty starter must never
// compare equal to an empty/absent canonical — that would let a session
// NOBODY is attributed to look like the person's own test — so an empty
// starter on this workshop fails closed to an empty list rather than
// matching every unattributed session (mirrors testwatch.go's own guard).
func (s *Server) handleTestSessions(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a testSessionsArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("test_sessions: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Class) == "" {
		return s.toolErr("test_sessions: class is required"), nil
	}

	sessNS, sessName := s.sessionRef()
	var ws spiceboxv1alpha1.Workshop
	if err := s.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, &ws); err != nil {
		return s.toolErr("test_sessions: reading this workshop: %v", err), nil
	}

	out := []map[string]any{}
	if ws.Spec.StarterCanonical == "" {
		return s.jsonResult(map[string]any{"sessions": out})
	}

	var list spiceboxv1alpha1.AgentSessionList
	if err := s.K8s.List(ctx, &list, client.InNamespace(s.Identity.Namespace)); err != nil {
		return s.toolErr("test_sessions: listing sessions: %v", err), nil
	}

	var matched []*spiceboxv1alpha1.AgentSession
	for i := range list.Items {
		sess := &list.Items[i]
		if sess.Spec.Class != a.Class {
			continue
		}
		if spiceboxv1alpha1.StartedByCanonical(sess).String() != ws.Spec.StarterCanonical {
			continue
		}
		matched = append(matched, sess)
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].CreationTimestamp.Time.Before(matched[j].CreationTimestamp.Time)
	})

	for _, sess := range matched {
		entry := map[string]any{
			"name": sess.Name,
			// The builder paints an ap:chat for this session, and ChatProps'
			// sessionRef is "<namespace>/<name>" — a bare name cannot address
			// a session — so the namespace travels beside the name, and ref
			// is the two already joined.
			//
			// ref exists rather than leaving the join to the builder because
			// the skill that consumes it may not name a namespace at all
			// (TestSkillBodiesAvoidPlatformVocabulary bans the word from every
			// skill body), so a procedure written in the only vocabulary
			// allowed there could not say which two fields to join.
			"namespace": sess.Namespace,
			"ref":       sess.Namespace + "/" + sess.Name,
			"phase":     sess.Status.Phase,
			"started":   sess.CreationTimestamp.Time.Format(time.RFC3339),
		}
		// lastActivity is status.lastWakeAt: the last time the session was
		// woken for a turn, not literal last activity. It is the closest
		// durable marker of activity the session carries, so it is the
		// acceptable proxy here rather than something finer-grained.
		if sess.Status.LastWakeAt != nil {
			entry["lastActivity"] = sess.Status.LastWakeAt.Time.Format(time.RFC3339)
		}
		// turnCount is the MODEL turns of the session's CURRENT runner pod,
		// from the per-pod Progress snapshot — nil until that pod has
		// completed a turn, and reset when the pod is replaced (a paused test
		// is exactly a pod exit). It is not a count of what the person said,
		// and must not be reported as one: the log read by read_test_log is
		// the only place that answer comes from.
		if sess.Status.Progress != nil {
			entry["turnCount"] = sess.Status.Progress.TurnCount
		}
		out = append(out, entry)
	}
	return s.jsonResult(map[string]any{"sessions": out})
}
