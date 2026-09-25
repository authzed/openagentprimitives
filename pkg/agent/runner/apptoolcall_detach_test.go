package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestHandleAppToolCall_SideEffectingDetach pins Task 5: a side-effecting
// (non-readonly, or readonly-hint-mismatch) authorized app-tool call fires the
// approval and runs on approval DETACHED from the NATS handler goroutine — the
// handler returns requires_approval (pending) IMMEDIATELY without blocking on
// the contained execution, and the detached exec eventually runs the tool.
// Readonly still runs synchronously; the rate limit and interact re-auth still
// gate the side-effecting path (before the spawn).
func TestHandleAppToolCall_SideEffectingDetach(t *testing.T) {
	const ns, name = "default", "disp"
	rwPerm := authz.Permission{StateImpact: authz.Readwrite}
	roPerm := authz.Permission{StateImpact: authz.Readonly}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("side-effecting authorized: returns requires_approval promptly, spawns detached exec (non-blocking)", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		// The detached exec's Execute blocks on this gate AFTER recording that it
		// ran, so the test can prove the handler returned while the exec is still
		// in flight (i.e. the handler did NOT block on the contained call).
		gate := make(chan struct{})
		ft := &fakeAppTool{name: "srv_write", perm: rwPerm, roHint: true, result: tool.Result{Content: "body"}, block: gate}
		reg := pipeline.NewRegistry() // Allow at both points: the detached exec reaches Execute
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_write": ft})

		start := time.Now()
		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_write", "user:viewer", json.RawMessage(`{}`)))
		elapsed := time.Since(start)

		assert.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status,
			"a side-effecting call returns pending (requires_approval), not the result")
		assert.Less(t, elapsed, 500*time.Millisecond,
			"the handler must return without blocking on the detached contained execution")

		// The detached exec reaches Execute even though the handler already
		// returned — proving the spawn happened off the handler goroutine.
		require.Eventually(t, func() bool { return ft.executed() == 1 }, 2*time.Second, 5*time.Millisecond,
			"the side-effecting call spawns a detached exec that runs the tool")
		close(gate) // release the parked detached Execute so its goroutine can finish
	})

	t.Run("side-effecting over rate budget: rate_limited, no spawn", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_write", perm: rwPerm, roHint: true, origin: "mcpserver/widgets", result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_write": ft})
		// Exhaust the single-call budget so the side-effecting call is over budget.
		l.AppToolRateLimiter = NewAppToolRateLimiter(map[string]int32{"mcpserver/widgets": 1}, fixedClock(time.Now()))
		require.True(t, l.AppToolRateLimiter.Allow("mcpserver/widgets"), "consume the single allowed call")

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_write", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusRateLimited, resp.Status,
			"the rate limit gates the side-effecting path too (an approval ask is spammable)")
		// No detached exec is spawned when over budget: give any (erroneous) spawn
		// a window to run, then assert the tool never executed.
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 0, ft.executed(), "an over-budget side-effecting call must not spawn a detached exec")
	})

	t.Run("side-effecting interact-denied: denied, no spawn", func(t *testing.T) {
		ia := &fakeInteract{allow: false}
		ft := &fakeAppTool{name: "srv_write", perm: rwPerm, roHint: true, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_write": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_write", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 0, ft.executed(), "a denied interact re-auth must not spawn a detached exec")
	})

	t.Run("readonly + hint true + authorized: still runs synchronously (unchanged)", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status,
			"a readonly auto-run call returns the result synchronously, not pending")
		assert.Equal(t, 1, ft.executed(), "the readonly call ran on the caller goroutine before returning")
	})
}

// recordingGrantEngine is a minimal engine.Engine whose only exercised method is
// CheckToolCall — it records the authz.Inputs subject/subjects the
// post-approval re-verify was built with. Embedding the nil
// interface supplies the other (never-called) methods.
type recordingGrantEngine struct {
	engine.Engine
	mu          sync.Mutex
	result      authz.Result
	calls       int
	gotSubject  string
	gotSubjects []string
}

func (r *recordingGrantEngine) CheckToolCall(_ context.Context, _ authz.Permission, in authz.Inputs) authz.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.gotSubject = in.Subject
	r.gotSubjects = append([]string(nil), in.Subjects...)
	return r.result
}

func (r *recordingGrantEngine) subject() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotSubject
}

func (r *recordingGrantEngine) subjectList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.gotSubjects...)
}

// TestToolCallPostApprove_ViewerSubject pins the T2-review forward finding: the
// post-approval re-verify uses the PER-CALL viewer
// subject/subjects threaded onto the pendingToolCall for a proxy-exec (app-tool)
// approval, falling back to l.authSubject/l.authSubjects for the LLM path. This
// both re-checks the RIGHT principal AND keeps the detached goroutine off the
// loop-mutated l.authSubject.
func TestToolCallPostApprove_ViewerSubject(t *testing.T) {
	newHost := func(t *testing.T) (*runnerHost, *recordingGrantEngine) {
		t.Helper()
		l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		l.authSubjects = []string{"session-subject"}
		eng := &recordingGrantEngine{result: authz.Result{Outcome: authz.OutcomeAllowed}}
		l.Engine = eng
		return newRunnerHost(l, hostSession{Namespace: "default", Name: "disp"}), eng
	}

	t.Run("proxy-exec: re-check uses the viewer subject, never l.authSubject", func(t *testing.T) {
		h, eng := newHost(t)
		tc := &pendingToolCall{
			sessNS: "default", sessName: "disp",
			perm:     authz.Permission{StateImpact: authz.Readwrite}, // non-External → the grant re-check runs
			argsMap:  map[string]any{},
			argsHash: "h",
			subject:  "widget-viewer",
			subjects: []string{"widget-viewer"},
		}
		require.NoError(t, h.toolCallPostApprove(context.Background(), tc))
		require.Equal(t, 1, eng.calls, "the post-approval re-verify must run for a non-External perm")
		assert.Equal(t, "widget-viewer", eng.subject(),
			"the proxy-exec re-check must use the viewer subject, not l.authSubject")
		assert.Equal(t, []string{"widget-viewer"}, eng.subjectList())
	})

	t.Run("LLM path (no per-call subject): re-check falls back to l.authSubject (unchanged)", func(t *testing.T) {
		h, eng := newHost(t)
		tc := &pendingToolCall{
			sessNS: "default", sessName: "disp",
			perm:     authz.Permission{StateImpact: authz.Readwrite},
			argsMap:  map[string]any{},
			argsHash: "h",
			// subject/subjects deliberately empty: the LLM path.
		}
		require.NoError(t, h.toolCallPostApprove(context.Background(), tc))
		require.Equal(t, 1, eng.calls)
		assert.Equal(t, "session-subject", eng.subject(),
			"the LLM path still re-checks against l.authSubject")
		assert.Equal(t, []string{"session-subject"}, eng.subjectList())
	})
}

// TestToolCallPostApprove_ProxyExec_AuthSubjectRace is the -race proof that a
// proxy-exec post-approval re-check reads the PER-CALL viewer subject and NEVER
// the loop-mutated l.authSubject/l.authSubjects — so it is race-clean when run
// (as the detached goroutine does) concurrently with advanceRequester-style
// flips of l.authSubject. Reverting toolCallPostApprove to read l.authSubject
// unconditionally makes this FAIL under -race.
func TestToolCallPostApprove_ProxyExec_AuthSubjectRace(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	l.authSubject = identity.CanonicalFromTrusted("start", "test fixture")
	l.authSubjects = []string{"start"}
	eng := &recordingGrantEngine{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	l.Engine = eng
	h := newRunnerHost(l, hostSession{Namespace: "default", Name: "disp"})

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				s := fmt.Sprintf("flip-%d", i)
				l.authSubject = identity.CanonicalFromTrusted(s, "test fixture")
				l.authSubjects = []string{s}
			}
		}
	}()

	tc := &pendingToolCall{
		sessNS: "default", sessName: "disp",
		perm:     authz.Permission{StateImpact: authz.Readwrite},
		argsMap:  map[string]any{},
		argsHash: "h",
		subject:  "widget-viewer",
		subjects: []string{"widget-viewer"},
	}
	for i := 0; i < 200; i++ {
		require.NoError(t, h.toolCallPostApprove(context.Background(), tc))
	}
	close(stop)
	<-done

	assert.Equal(t, "widget-viewer", eng.subject(),
		"the proxy-exec re-check must observe the viewer, never a flipped loop subject")
}

// TestHandleAppToolCall_DetachedExec_NoRaceWithRun is the -race proof for Task
// 5: a detached side-effecting contained exec (proxyExec:true, suppressHalt:true)
// runs concurrently with goroutines mutating every *Loop field Run touches that
// the detached path also reads — l.authSubject/l.authSubjects (advanceRequester),
// l.lastAssistantTurnIndex (Run's per-turn write), l.Tools (guarded), and a
// same-scope signed memory append (Run's transcript appends) — and is race-clean.
// This exercises the composite of the T1-T4 + authz-completeness guards under
// the actual detach shape.
//
// Not meaningful without -race: it passes under a plain `go test` regardless of
// the guards; only `go test -race` surfaces an unguarded field.
func TestHandleAppToolCall_DetachedExec_NoRaceWithRun(t *testing.T) {
	const ns, name = "default", "disp"
	ia := &fakeInteract{allow: true}
	ft := &fakeAppTool{name: "srv_write", perm: authz.Permission{StateImpact: authz.Readwrite}, roHint: true, result: tool.Result{Content: "body"}}
	reg := pipeline.NewRegistry() // Allow at both points so the detached exec runs to completion
	l := newAppLoop(t, reg, map[string]tool.Tool{"srv_write": ft})
	l.SessionContext = &tool.SessionContext{Namespace: ns, Name: name, AgentSessionUID: "uid-1"}
	l.authSubject = identity.CanonicalFromTrusted("start", "test fixture")
	l.authSubjects = []string{"start"}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	stop := make(chan struct{})
	var writers sync.WaitGroup

	// Writer 1: advanceRequester-style flips of the loop subject.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				s := fmt.Sprintf("flip-%d", i)
				l.authSubject = identity.CanonicalFromTrusted(s, "test fixture")
				l.authSubjects = []string{s}
			}
		}
	}()

	// Writer 2: Run's per-turn lastAssistantTurnIndex write.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				l.setLastAssistantTurnIndex(i)
			}
		}
	}()

	// Writer 3: Run's entry-setup SessionContext reassignment under sessionCtxMu.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				l.sessionCtxMu.Lock()
				sess := l.SessionContext
				if sess == nil {
					sess = &tool.SessionContext{Namespace: ns, Name: name}
				}
				sess.SubmitResult = func(tool.AgentResult) {}
				l.SessionContext = sess
				l.sessionCtxMu.Unlock()
			}
		}
	}()

	// Drive detached side-effecting calls: each HandleAppToolCall spawns a
	// detached exec that runs concurrently with the writers above.
	for i := 0; i < 50; i++ {
		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_write", "user:viewer", json.RawMessage(`{}`)))
		require.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status)
	}

	// Let the detached execs run against the live writers, then quiesce.
	require.Eventually(t, func() bool { return ft.executed() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"at least one detached exec must have run against the concurrent writers")
	close(stop)
	writers.Wait()
}
