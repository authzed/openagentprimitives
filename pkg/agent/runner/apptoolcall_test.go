package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInteract is a stub InteractChecker recording the arguments it was
// called with. allow/err drive the verdict; a nil *fakeInteract passed as
// the interface tests the fail-closed nil path (handled by the caller).
type fakeInteract struct {
	allow      bool
	err        error
	calls      int
	gotNS      string
	gotName    string
	gotSubject identity.CanonicalUserID
	gotFully   bool
}

func (f *fakeInteract) CheckInteract(_ context.Context, ns, name string, subject identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	f.calls++
	f.gotNS, f.gotName, f.gotSubject, f.gotFully = ns, name, subject, fullyConsistent
	return f.allow, f.err
}

// fakeAppTool is an app-tool stub that implements tool.Tool plus the optional
// ServerReadOnlyHint() the readonly gate probes and Origin() (tool.OriginTool)
// the rate-limit gate probes. It records whether Execute ran and the
// SessionContext it was handed (so a test can assert the proxy-exec runs with
// Operations nil — D-B6).
type fakeAppTool struct {
	name     string
	perm     authz.Permission
	variants []authz.PermissionVariant // PermissionVariants() return value; nil for most fixtures
	roHint   bool
	origin   string // Origin() return value; "" is still a valid (if unconfigured) origin
	result   tool.Result
	block    chan struct{} // if non-nil, Execute blocks until closed (after recording it ran)
	mu       sync.Mutex
	execers  int
	gotSess  *tool.SessionContext
}

func (f *fakeAppTool) Name() string                                  { return f.name }
func (f *fakeAppTool) Kind() tool.Kind                               { return tool.KindMCP }
func (f *fakeAppTool) Description() string                           { return "fake app tool" }
func (f *fakeAppTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeAppTool) Permission() authz.Permission                  { return f.perm }
func (f *fakeAppTool) PermissionVariants() []authz.PermissionVariant { return f.variants }
func (f *fakeAppTool) ServerReadOnlyHint() bool                      { return f.roHint }
func (f *fakeAppTool) Origin() string                                { return f.origin }
func (f *fakeAppTool) Execute(_ context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	f.mu.Lock()
	f.execers++
	f.gotSess = sess
	b := f.block
	f.mu.Unlock()
	if b != nil {
		<-b // park until the test releases the detached exec (proves non-blocking handler)
	}
	return f.result, nil
}
func (f *fakeAppTool) executed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execers
}

func newAppLoop(t *testing.T, reg *pipeline.Registry, tools map[string]tool.Tool) *Loop {
	t.Helper()
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		AppTools:   tools,
	}
	if reg != nil {
		loopWithInjectedExecutor(t, l, reg)
	}
	return l
}

// appToolEnvelope builds the exact wire bytes webd's RequestIn produces: a
// channelevents.Envelope of KindAppToolCall carrying an AppToolCallRequest.
func appToolEnvelope(t *testing.T, toolName, requester string, args json.RawMessage) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope("default", "disp", channelevents.KindAppToolCall,
		channelevents.AppToolCallRequest{
			ToolName:  toolName,
			Args:      args,
			Requester: requester,
			RequestID: "req-1",
		})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func TestHandleAppToolCall(t *testing.T) {
	const ns, name = "default", "disp"
	roPerm := authz.Permission{StateImpact: authz.Readonly}
	rwPerm := authz.Permission{StateImpact: authz.Readwrite}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("interact denied: denied, no lookup, no execute", func(t *testing.T) {
		ia := &fakeInteract{allow: false}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		l := newAppLoop(t, nil, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(context.Background(), ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.Equal(t, 0, ft.executed(), "a denied interact check must not execute the tool")
		assert.Equal(t, 1, ia.calls, "interact check runs exactly once")
		assert.Equal(t, identity.CanonicalFromTrusted("viewer", "test fixture"), ia.gotSubject, "the user: prefix is stripped to the canonical id")
		assert.True(t, ia.gotFully, "the re-auth check must be fully consistent")
	})

	t.Run("nil InteractChecker: denied (fail-closed), no execute", func(t *testing.T) {
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		l := newAppLoop(t, nil, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(context.Background(), nil, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.Equal(t, 0, ft.executed(), "a nil interact checker must never execute the tool")
	})

	t.Run("unknown tool: not_found", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		l := newAppLoop(t, nil, map[string]tool.Tool{})

		resp := l.HandleAppToolCall(context.Background(), ia, ns, name,
			appToolEnvelope(t, "nope", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusNotFound, resp.Status)
	})

	// A side-effecting (non-readonly) tool does NOT run synchronously: the handler
	// returns requires_approval (pending) and spawns a detached exec that fires
	// the approval and runs on approval. Here the Allow-only registry lets the
	// detached exec run to completion, so its eventual execution proves the spawn.
	t.Run("readwrite tool: requires_approval, spawns detached exec", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_write", perm: rwPerm, roHint: true, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_write": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_write", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status)
		require.Eventually(t, func() bool { return ft.executed() == 1 }, 2*time.Second, 5*time.Millisecond,
			"a non-readonly call spawns a detached exec (it does not run synchronously)")
	})

	t.Run("readonly but server hint false: requires_approval, spawns detached exec", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: false, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status,
			"a StateImpact-readonly tool whose server readOnlyHint disagrees demotes to approval")
		require.Eventually(t, func() bool { return ft.executed() == 1 }, 2*time.Second, 5*time.Millisecond,
			"the demoted call runs detached, not synchronously")
	})

	t.Run("tool without ServerReadOnlyHint (non-MCP): requires_approval, spawns detached exec", func(t *testing.T) {
		// A tool that does not implement the optional interface defaults the
		// hint to false, which correctly denies auto-run even for a
		// StateImpact-readonly permission — so it demotes to the detached path.
		ia := &fakeInteract{allow: true}
		ft := &fakeDispatchTool{name: "plain_read", kind: tool.KindMCP, perm: roPerm, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"plain_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "plain_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status)
		require.Eventually(t, func() bool { return ft.executed() == 1 }, 2*time.Second, 5*time.Millisecond,
			"the demoted non-MCP call runs detached, not synchronously")
	})

	t.Run("readonly + hint true + authorized: ok + result, execute once", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry() // no denying hook: Allow at both points
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{"q":"x"}`)))

		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
		assert.Equal(t, `"body"`, string(resp.Result), "the string result is JSON-encoded into Result")
		assert.False(t, resp.IsError)
		assert.Equal(t, 1, ft.executed(), "an authorized readonly call executes exactly once")
	})

	// The rate limiter is a DEDICATED gate on this path (not a toolguard rule) —
	// see pkg/agent/runner/apptool_ratelimit.go. This exercises the wiring in
	// HandleAppToolCall: an over-budget origin must short-circuit BEFORE
	// executeToolContained ever runs (Execute must see 0 calls).
	t.Run("readonly + authorized but origin over rate budget: rate_limited, no execute", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, origin: "mcpserver/widgets", result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry() // no denying hook: would Allow at both points if reached
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})
		// Zero-allowance limiter: the configured origin's burst is exhausted
		// before the very first call (NewAppToolRateLimiter with n=1 and a
		// clock held fixed — see apptool_ratelimit_test.go's fixedClock pattern
		// — then Allow consumed once here to force the next call over budget).
		l.AppToolRateLimiter = NewAppToolRateLimiter(map[string]int32{"mcpserver/widgets": 1}, fixedClock(time.Now()))
		require.True(t, l.AppToolRateLimiter.Allow("mcpserver/widgets"), "consume the single allowed call to force the next one over budget")

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{"q":"x"}`)))

		assert.Equal(t, channelevents.AppToolCallStatusRateLimited, resp.Status)
		assert.Equal(t, 0, ft.executed(), "an over-budget origin must not execute the tool")
	})

	t.Run("readonly tool returns IsError result: ok with IsError set", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "boom", IsError: true}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status, "the call reached the tool and completed")
		assert.True(t, resp.IsError, "a tool-level error surfaces as IsError, not a transport error")
	})

	t.Run("pipeline pre-deny: denied", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "secret"}}
		reg := pipeline.NewRegistry()
		reg.Register(&fakeHook{point: pipeline.PreToolCall, reason: "blocked by fake pre hook"}, 10)
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.Contains(t, resp.Message, "blocked by fake pre hook")
		assert.Equal(t, 0, ft.executed(), "a PreToolCall deny must not execute the tool")
		assert.NotContains(t, string(resp.Result), "secret")
	})

	t.Run("malformed envelope bytes: error", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		l := newAppLoop(t, nil, map[string]tool.Tool{})

		resp := l.HandleAppToolCall(context.Background(), ia, ns, name, []byte("{not json"))

		assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
		assert.Equal(t, 0, ia.calls, "a malformed request never reaches the interact check")
	})

	t.Run("valid envelope, malformed payload: error", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		l := newAppLoop(t, nil, map[string]tool.Tool{})
		env := channelevents.Envelope{
			Version: 1,
			Kind:    channelevents.KindAppToolCall,
			Payload: json.RawMessage(`123`), // a number, not an AppToolCallRequest object
		}
		b, err := json.Marshal(env)
		require.NoError(t, err)

		resp := l.HandleAppToolCall(context.Background(), ia, ns, name, b)

		assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
		assert.Equal(t, 0, ia.calls)
	})

	// Phase-D prerequisite behavior change (security-correct): now that
	// ResolvePermission reaches l.AppTools, the readonly app-tool path evaluates
	// the tool's per-resource Check against the VIEWER — which the l.Tools-only
	// scan never did (it handed the checker the zero permission). Under the
	// DEFAULT enforcing mode this test exercises, an authorized viewer runs and an
	// unauthorized viewer is denied BY THE APP-TOOL'S OWN Check (repo:read), not
	// by the pre-fix zero-perm path.
	t.Run("readonly app-tool now enforces the viewer Check", func(t *testing.T) {
		newLoop := func(t *testing.T, ft *fakeAppTool, rec *recordingToolChecker) *Loop {
			t.Helper()
			l := &Loop{
				SessionKey: memory.NamespacedName{Namespace: ns, Name: name},
				AppTools:   map[string]tool.Tool{"srv_read": ft},
			}
			loopWithRecordingAuthz(t, l, rec) // real ToolCallAuthz hook, enforcing, checker=rec
			return l
		}

		t.Run("authorized viewer: runs, and the checker saw the real per-resource Check", func(t *testing.T) {
			ia := &fakeInteract{allow: true}
			ft := &fakeAppTool{name: "srv_read", perm: checkPerm(), roHint: true, result: tool.Result{Content: "body"}}
			rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
			l := newLoop(t, ft, rec)

			resp := l.HandleAppToolCall(execCtx, ia, ns, name,
				appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{"q":"x"}`)))

			assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
			assert.Equal(t, 1, ft.executed(), "an authorized readonly call still executes exactly once")
			require.Equal(t, 1, rec.calls, "the Check must actually run for the app-tool")
			assert.Equal(t, "viewer", rec.subject(), "the Check runs against the widget viewer")
			require.NotNil(t, rec.perm().Check, "the app-tool's per-resource Check must reach the checker (previously skipped)")
			assert.Equal(t, "repo", rec.perm().Check.ResourceType)
		})

		t.Run("unauthorized viewer: denied by the Check, tool never runs", func(t *testing.T) {
			ia := &fakeInteract{allow: true} // interact re-auth passes; the tool Check is what denies
			ft := &fakeAppTool{name: "srv_read", perm: checkPerm(), roHint: true, result: tool.Result{Content: "secret"}}
			rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "viewer lacks repo:read"}}
			l := newLoop(t, ft, rec)

			resp := l.HandleAppToolCall(execCtx, ia, ns, name,
				appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{"q":"x"}`)))

			assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status,
				"a viewer the per-resource Check denies must be blocked under enforcing mode")
			assert.Equal(t, 0, ft.executed(), "a denied Check must not execute the tool")
			assert.NotContains(t, string(resp.Result), "secret")
			// The deny is the app-tool's OWN per-resource Check, not a spurious
			// zero-permission path: before the lookup fix ResolvePermission handed
			// the checker authz.Permission{} (Check==nil); now it sees repo:read.
			require.NotNil(t, rec.perm().Check, "the deny must be driven by the app-tool's real Check")
			assert.Equal(t, "repo", rec.perm().Check.ResourceType)
		})
	})

	// D-B6: a proxy-exec is audited but MUST NOT enter the operation-activity
	// tree / transcript. The proxy-exec runs with a SessionContext whose
	// Operations is nil, and it must not mutate the loop's own SessionContext.
	t.Run("proxy-exec runs with Operations nil and does not mutate loop state (D-B6)", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{name: "srv_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		reg := pipeline.NewRegistry()
		l := newAppLoop(t, reg, map[string]tool.Tool{"srv_read": ft})
		origOps := operations.New(time.Now, nil)
		l.SessionContext = &tool.SessionContext{Namespace: ns, Name: name, Operations: origOps}

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "srv_read", "user:viewer", json.RawMessage(`{}`)))

		require.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
		require.NotNil(t, ft.gotSess, "Execute must have been handed a SessionContext")
		assert.Nil(t, ft.gotSess.Operations,
			"proxy-exec must run with Operations nil so its call never enters the operation-activity tree")
		assert.NotNil(t, l.SessionContext.Operations,
			"the proxy-exec must not nil the loop's own SessionContext.Operations (shallow copy, not in place)")
		assert.Empty(t, origOps.All(), "the proxy-exec must record no operation in the loop's op registry")
		assert.Equal(t, ns+"/"+name, l.SessionContext.Namespace+"/"+l.SessionContext.Name)
	})
}
