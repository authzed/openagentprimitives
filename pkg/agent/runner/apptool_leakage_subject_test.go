package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingLeakCheck records the subject the info-leakage read gate hands to
// SpiceDB, so a test can assert WHICH principal the view check ran against.
type recordingLeakCheck struct {
	mu         sync.Mutex
	calls      int
	gotSubject string
	allow      bool
}

func (r *recordingLeakCheck) check(_ context.Context, subject, _, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.gotSubject = subject
	return r.allow, nil
}

func (r *recordingLeakCheck) subject() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotSubject
}

func (r *recordingLeakCheck) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// loopWithInfoLeakRead wires the REAL InfoLeakRead hook (with the real
// infoLeakReadDeps closures — the code under test) into an injected executor,
// with a recording SpiceDB check standing in for the cluster. Enforcing mode,
// one mapped tool that reads issue:<id> from the "id" arg.
func loopWithInfoLeakRead(t *testing.T, l *Loop, rec *recordingLeakCheck) {
	t.Helper()
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
	l.LookupToolMapping = func(string) *spiceboxv1alpha1.ToolResourceMapping {
		return &spiceboxv1alpha1.ToolResourceMapping{
			Tool:  "srv",
			Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "issue", Permission: "view", IDArg: "id"},
		}
	}
	l.SpiceDBCheck = rec.check
	// Production wiring shape (internal/cmd/runner): the leakage requester resolver
	// converts a canonical to the "user:"-prefixed SpiceDB subject ref.
	l.RequesterCanonicalID = func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
		return perCall.SubjectRef(), nil
	}
	// TaintMemoryAppend left nil: a nil AppendTaint is a documented no-op and
	// keeps the test off the memory layer.
	reg := pipeline.NewRegistry()
	reg.Register(hooks.NewInfoLeakRead(l.infoLeakReadDeps()), hooks.OrderInfoLeakRead)
	loopWithInjectedExecutor(t, l, reg)
}

// leakArgs is the wrapped args envelope carrying the mapped IDArg.
func leakArgs() json.RawMessage { return json.RawMessage(`{"args":{"id":"ENG-1"}}`) }

// TestInfoLeakRead_PerCallPrincipal pins the read-side info-leakage gate to the
// PER-CALL principal (pipeline.Input.Requester) — the same contract
// pkg/platform/pipeline/types.go documents for Subjects/Requester:
//
//   - LLM path: the check subject is the loop's session subject (unchanged).
//   - app-tool (proxy-exec) path: the check subject is the widget VIEWER (D-D1).
//
// Before the fix the hook resolved the requester from a Loop-reading closure
// (Loop.AuthSubject), so a widget viewer's read was authorized against whoever
// happened to be the session's current requester — a viewer with no `view` on
// the resource received data the session subject could see.
func TestInfoLeakRead_PerCallPrincipal(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("LLM path: view check runs against the session subject (unchanged)", func(t *testing.T) {
		ft := &fakeDispatchTool{name: "srv", kind: tool.KindMCP, result: tool.Result{Content: "{}"}}
		l := &Loop{Tools: []tool.Tool{ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		rec := &recordingLeakCheck{allow: true}
		loopWithInfoLeakRead(t, l, rec)

		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			leakArgs(), "tu-1", "why",
			containParams{requester: l.authSubject, subjects: []string{l.authSubject.String()}})

		require.Equal(t, containRanOK, oc.Phase)
		require.Equal(t, 1, rec.count(), "the mapped read must run the view check")
		assert.Equal(t, "user:session-subject", rec.subject())
	})

	t.Run("app-tool path: view check runs against the widget viewer (D-D1)", func(t *testing.T) {
		ft := &fakeAppTool{name: "srv", roHint: true, result: tool.Result{Content: "{}"}}
		l := &Loop{AppTools: map[string]tool.Tool{"srv": ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		// The loop-level subject differs from the viewer, so a check against
		// l.authSubject (the pre-fix behavior) fails this test.
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		rec := &recordingLeakCheck{allow: true}
		loopWithInfoLeakRead(t, l, rec)

		var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			leakArgs(), "tu-1", "why",
			containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})

		require.Equal(t, containRanOK, oc.Phase)
		require.Equal(t, 1, rec.count())
		assert.Equal(t, "user:widget-viewer", rec.subject(),
			"the app-tool path must check the VIEWER, never the loop's session subject")
	})

	t.Run("app-tool path: a viewer without view is denied even when the session subject has it", func(t *testing.T) {
		ft := &fakeAppTool{name: "srv", roHint: true, result: tool.Result{Content: "{}"}}
		l := &Loop{AppTools: map[string]tool.Tool{"srv": ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		// The check denies whoever it is handed. With the pre-fix behavior the
		// gate would have consulted the session subject; the assertion that
		// matters is that the widget call is refused and the tool output is
		// withheld from the browser.
		rec := &recordingLeakCheck{allow: false}
		loopWithInfoLeakRead(t, l, rec)

		var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
		res, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			leakArgs(), "tu-1", "why",
			containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})

		assert.Equal(t, containPostDeny, oc.Phase, "an unauthorized viewer must be denied at PostToolCall")
		assert.True(t, res.IsError, "the raw tool output must be withheld from the widget")
		assert.Equal(t, "user:widget-viewer", rec.subject())
	})
}

// TestInfoLeakRead_BothMode_ActingRequester is the downstream cost of "both"
// mode never resolving an acting principal.
//
// ResolveAuthSubjects's "both" branch used to set only authSubjectStartedBy +
// authSubjects, leaving l.authSubject "". dispatchToolUses builds
// containParams{requester: l.authSubject, …} — reproduced verbatim below — so
// every LLM-path tool call carried an empty pipeline.Input.Requester. The read
// gate treats an absent principal as a check it could not evaluate
// (infoleakread.go: `case r == "": unevaluated = "no_requester"`), and enforcing
// mode fails closed on an unevaluated check. Net effect: with
// informationLeakage.mode=enforcing, a "both"-mode session had EVERY mapped tool
// read denied — the view check never ran at all — despite the session carrying a
// perfectly good channel identity.
//
// The session here gives the two components different identities so the
// assertion pins WHICH principal the gate checks: the current requester (alice),
// not the frozen initiator (carol).
func TestInfoLeakRead_BothMode_ActingRequester(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	ft := &fakeDispatchTool{name: "srv", kind: tool.KindMCP, result: tool.Result{Content: "{}"}}
	l := &Loop{Tools: []tool.Tool{ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	// ResolveAuthSubjects no-ops without an authz client.
	l.AuthzCli = noopAuthzClient{}
	rec := &recordingLeakCheck{allow: true}
	loopWithInfoLeakRead(t, l, rec)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey:       "alice-canonical",
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:carol-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("both"))

	// Verbatim the containParams dispatchToolUses builds on the LLM path.
	_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
		leakArgs(), "tu-1", "why",
		containParams{requester: l.authSubject, subjects: l.authSubjects})

	require.Equal(t, containRanOK, oc.Phase,
		"a both-mode read must reach the gate with a resolvable principal, not fail closed as unevaluated")
	require.Equal(t, 1, rec.count(), "the mapped read must actually run the view check")
	assert.Equal(t, "user:alice-canonical", rec.subject(),
		"the view check must run against the acting requester, not the frozen initiator")
}

// TestInfoLeakRead_ProxyExec_NoRaceWithAdvanceRequester is the -race proof.
//
// The interleaving it reproduces: a browser widget's app-tool call runs
// HandleAppToolCall → executeToolContained → PostToolCall → InfoLeakRead on the
// NATS-subscription goroutine (readonly auto-run) or on the detached approval
// goroutine, while the loop goroutine drains the inbox and calls
// advanceRequester, which writes l.authSubject. Resolving the leakage requester
// from a Loop-reading closure made that an unsynchronized read of a string
// field concurrent with a write to it.
//
// Not meaningful without -race: the pre-fix code passes a plain `go test`
// (a torn read still produces *some* string) and fails under `go test -race`.
func TestInfoLeakRead_ProxyExec_NoRaceWithAdvanceRequester(t *testing.T) {
	ft := &fakeAppTool{name: "srv", roHint: true, result: tool.Result{Content: "{}"}}
	l := &Loop{AppTools: map[string]tool.Tool{"srv": ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	l.authSubject = identity.CanonicalFromTrusted("start", "test fixture")
	rec := &recordingLeakCheck{allow: true}
	loopWithInfoLeakRead(t, l, rec)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				// What advanceRequester does on the loop goroutine when a new
				// inbound turn drains: move the current-requester subject.
				l.authSubject = identity.CanonicalFromTrusted(fmt.Sprintf("flip-%d", i), "test fixture")
			}
		}
	}()

	var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
	for i := 0; i < 50; i++ {
		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			leakArgs(), "tu-1", "why",
			containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})
		require.Equal(t, containRanOK, oc.Phase)
	}
	close(stop)
	<-done

	assert.Equal(t, "user:widget-viewer", rec.subject(),
		"the proxy-exec view check must observe the viewer, never a flipped loop subject")
}
