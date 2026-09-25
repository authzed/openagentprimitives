package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/authfail"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// originlessTool implements tool.Tool but NOT tool.OriginTool — the shape a
// meta tool has. fakeAppTool (apptoolcall_test.go) always implements Origin,
// so it cannot stand in for this case.
type originlessTool struct{ name string }

func (f *originlessTool) Name() string                                  { return f.name }
func (f *originlessTool) Kind() tool.Kind                               { return tool.KindMeta }
func (f *originlessTool) Description() string                           { return "fake meta tool" }
func (f *originlessTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (f *originlessTool) Permission() authz.Permission                  { return authz.Permission{} }
func (f *originlessTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *originlessTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

// capturingWriter records which authfail.StatusWriter call each tool result
// produced. The MAPPING is the behavior under test here; the recorder's own
// write policy is covered in pkg/agent/tool/authfail.
type capturingWriter struct {
	mu      sync.Mutex
	records []string
	clears  []string
}

func (c *capturingWriter) RecordCredentialAuthFailure(_ context.Context, origin string, _ int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, origin)
	return nil
}

func (c *capturingWriter) ClearCredentialAuthFailure(_ context.Context, origin string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clears = append(c.clears, origin)
	return nil
}

func (c *capturingWriter) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.records...), append([]string(nil), c.clears...)
}

// newObserveLoop builds a Loop whose recorder classifies every origin against
// the 401-only default authFailure: block.
func newObserveLoop(t *testing.T) (*Loop, *capturingWriter) {
	t.Helper()
	return newObserveLoopWith(t, &provider.AuthFailure{})
}

// newObserveLoopWith builds a Loop whose recorder classifies every origin
// against af — the CLI-shaped cases need a block declaring exitCodes /
// stderrPatterns, which the 401-only default deliberately has none of.
func newObserveLoopWith(t *testing.T, af *provider.AuthFailure) (*Loop, *capturingWriter) {
	t.Helper()
	w := &capturingWriter{}
	return &Loop{AuthFailures: authfail.New(w, func(string) *provider.AuthFailure {
		return af
	}, nil)}, w
}

func int32Ptr(v int32) *int32 { return &v }

// TestObserveAuthFailureMapsResultsToTheRecorder covers the runner-side WIRING
// of the credential-update corroboration path: which tool results reach the
// recorder, and as which call.
func TestObserveAuthFailureMapsResultsToTheRecorder(t *testing.T) {
	ctx := context.Background()
	mcpTool := &fakeAppTool{name: "gh_search", origin: "mcpserver/demo"}

	t.Run("failed call carrying a 401: recorded against the tool's origin", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, HTTPStatus: 401})

		recs, clears := w.snapshot()
		require.Len(t, recs, 1)
		assert.Equal(t, "mcpserver/demo", recs[0])
		assert.Empty(t, clears)
	})

	t.Run("failed call carrying a 403: NOT recorded under the 401-only default", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, HTTPStatus: 403})

		recs, _ := w.snapshot()
		assert.Empty(t, recs,
			"an agent can provoke a 403 on demand; only a provider that opts in may have one corroborate")
	})

	t.Run("failed call with no HTTP status (transport failure): nothing recorded", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true})

		recs, _ := w.snapshot()
		assert.Empty(t, recs, "zero means no status was observed, and must never be read as an auth failure")
	})

	t.Run("SUCCESSFUL call clears the origin", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, HTTPStatus: 401})
		l.observeAuthFailure(ctx, mcpTool, tool.Result{})

		_, clears := w.snapshot()
		require.Len(t, clears, 1, "a call that succeeded proves the credential works and must retract the observation")
		assert.Equal(t, "mcpserver/demo", clears[0])
	})

	t.Run("ERROR result that nevertheless authenticated clears the origin", func(t *testing.T) {
		// The wiring claim: the positive carrier must reach the recorder on the
		// ERROR path, because that is the only path where it changes anything.
		// An MCP server answering 200 with a tool-level isError arrives here as
		// IsError=true + OriginAuthenticated=true, and the credential
		// demonstrably works.
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, HTTPStatus: 401})
		l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, OriginAuthenticated: true})

		_, clears := w.snapshot()
		require.Len(t, clears, 1,
			"a 200 the agent turned into a tool-level error still proves the credential works; dropping the carrier here strands the observation")
		assert.Equal(t, "mcpserver/demo", clears[0])
	})

	t.Run("origin-less tool (meta): never observed", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, &originlessTool{name: "respond_to_user"}, tool.Result{IsError: true, HTTPStatus: 401})

		recs, clears := w.snapshot()
		assert.Empty(t, recs)
		assert.Empty(t, clears, "there is no credential behind an origin-less tool for a human to replace")
	})

	t.Run("empty Origin() (toolkit-less sandbox tool): never observed", func(t *testing.T) {
		l, w := newObserveLoop(t)
		l.observeAuthFailure(ctx, &fakeAppTool{name: "bash", origin: ""}, tool.Result{IsError: true, HTTPStatus: 401})

		recs, clears := w.snapshot()
		assert.Empty(t, recs)
		assert.Empty(t, clears)
	})

	t.Run("no recorder wired: a nil field is a safe no-op, not a panic", func(t *testing.T) {
		l := &Loop{}
		assert.NotPanics(t, func() {
			l.observeAuthFailure(ctx, mcpTool, tool.Result{IsError: true, HTTPStatus: 401})
			l.observeAuthFailure(ctx, mcpTool, tool.Result{})
		})
	})
}

// TestObserveAuthFailureReadsTheCLIObservationSurface is the CLI/toolkit half
// of the mapping above. A toolkit call exposes no HTTP status — only a process
// exit code and a stderr artifact — so if observeAuthFailure builds its
// Observation from res.HTTPStatus alone, every case here silently records
// nothing and a dead CLI credential can never be corroborated.
func TestObserveAuthFailureReadsTheCLIObservationSurface(t *testing.T) {
	ctx := context.Background()
	cliTool := &fakeAppTool{name: "code_gh", origin: "toolkit/gh"}

	t.Run("matching exit code: recorded against the toolkit origin", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{ExitCodes: []int{4}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{IsError: true, ExitCode: int32Ptr(4)})

		recs, clears := w.snapshot()
		require.Len(t, recs, 1, "a CLI exit code is the toolkit half of the corroboration surface")
		assert.Equal(t, "toolkit/gh", recs[0])
		assert.Empty(t, clears)
	})

	t.Run("non-matching exit code: not recorded", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{ExitCodes: []int{4}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{IsError: true, ExitCode: int32Ptr(1)})

		recs, _ := w.snapshot()
		assert.Empty(t, recs, "a generic non-zero exit is not evidence the credential was rejected")
	})

	t.Run("matching stderr pattern: recorded even with an undeclared exit code", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{StderrPatterns: []string{`(?i)bad credentials`}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{
			IsError:  true,
			ExitCode: int32Ptr(1),
			Stderr:   "gh: Bad credentials (HTTP 401)\n",
		})

		recs, _ := w.snapshot()
		require.Len(t, recs, 1, "stderrPatterns is the only signal some CLIs expose")
		assert.Equal(t, "toolkit/gh", recs[0])
	})

	t.Run("non-matching stderr: not recorded", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{StderrPatterns: []string{`(?i)bad credentials`}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{
			IsError:  true,
			ExitCode: int32Ptr(1),
			Stderr:   "fatal: repository not found\n",
		})

		recs, _ := w.snapshot()
		assert.Empty(t, recs)
	})

	t.Run("clean exit CLEARS a prior exit-code observation", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{ExitCodes: []int{4}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{IsError: true, ExitCode: int32Ptr(4)})
		l.observeAuthFailure(ctx, cliTool, tool.Result{ExitCode: int32Ptr(0)})

		recs, clears := w.snapshot()
		require.Len(t, recs, 1)
		require.Len(t, clears, 1, "a toolkit call that just worked is not one whose credential needs replacing")
		assert.Equal(t, "toolkit/gh", clears[0])
	})

	t.Run("exit 0 declared as an auth failure never fires on an UNOBSERVED exit code", func(t *testing.T) {
		// A pathological provider declaring exitCodes: [0] must not turn every
		// call with no observed exit code into corroboration. This is what the
		// pointer on Result.ExitCode buys: nil is "not observed", not "0".
		l, w := newObserveLoopWith(t, &provider.AuthFailure{ExitCodes: []int{0}})
		l.observeAuthFailure(ctx, cliTool, tool.Result{IsError: true})

		recs, _ := w.snapshot()
		assert.Empty(t, recs, "nil ExitCode means nothing was observed and must never match")
	})

	t.Run("empty origin: an exit-code failure is still never observed", func(t *testing.T) {
		l, w := newObserveLoopWith(t, &provider.AuthFailure{ExitCodes: []int{4}})
		l.observeAuthFailure(ctx, &fakeAppTool{name: "bash", origin: ""},
			tool.Result{IsError: true, ExitCode: int32Ptr(4), Stderr: "Bad credentials"})

		recs, clears := w.snapshot()
		assert.Empty(t, recs, "a toolkit-less sandbox tool has no managed credential to replace")
		assert.Empty(t, clears)
	})
}

// originDispatchTool is a fakeDispatchTool (dispatch_pipeline_test.go) that
// also carries an Origin and an HTTP status on its result — an MCP tool, as
// far as the dispatch path is concerned.
type originDispatchTool struct {
	fakeDispatchTool
	origin string
}

func (f *originDispatchTool) Origin() string { return f.origin }

// TestDispatchObservesAuthFailuresAtTheRawUpstreamOutcome pins the CALL SITE,
// not just the mapping: the observation must happen inside the containment
// pipeline, after Execute and BEFORE the PostToolCall gates can rewrite the
// result. Two things break if it moves:
//
//   - Placed after the Post gates, a PostToolCall Deny (which replaces the
//     result with a platform-authored IsError) would fabricate an auth-failure
//     observation for a call that actually SUCCEEDED upstream.
//   - Placed before Execute, a PreToolCall Deny would record a failure for a
//     call that never ran.
func TestDispatchObservesAuthFailuresAtTheRawUpstreamOutcome(t *testing.T) {
	// seeded, when non-empty, primes the recorder as if the session status
	// already carried an observation for that origin — which is what makes a
	// clear observable at all (the recorder writes a clear only when there IS
	// something to clear).
	newLoop := func(t *testing.T, ft tool.Tool, hook *fakeHook, seeded ...string) (*Loop, *capturingWriter) {
		t.Helper()
		w := &capturingWriter{}
		var existing []spiceboxv1alpha1.CredentialAuthFailure
		for _, o := range seeded {
			existing = append(existing, spiceboxv1alpha1.CredentialAuthFailure{Origin: o, Count: 1})
		}
		l := &Loop{
			Tools:      []tool.Tool{ft},
			SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
			AuthFailures: authfail.New(w, func(string) *provider.AuthFailure {
				return &provider.AuthFailure{}
			}, existing),
		}
		reg := pipeline.NewRegistry()
		if hook != nil {
			reg.Register(hook, 10)
		}
		loopWithInjectedExecutor(t, l, reg)
		return l, w
	}
	dispatch := func(t *testing.T, l *Loop, name string) {
		t.Helper()
		uses := []llm.ToolUseBlock{{ID: "tu-1", Name: name, Input: json.RawMessage(`{"args":{}}`)}}
		l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), uses, dispatchTestSession(), 0, 0, nil, nil)
	}

	t.Run("a 401 from the tool reaches the recorder through the dispatch path", func(t *testing.T) {
		ft := &originDispatchTool{
			fakeDispatchTool: fakeDispatchTool{
				name: "gh_search", kind: tool.KindMCP,
				result: tool.Result{Content: "unauthorized", IsError: true, HTTPStatus: 401},
			},
			origin: "mcpserver/demo",
		}
		l, w := newLoop(t, ft, nil)
		dispatch(t, l, "gh_search")

		recs, _ := w.snapshot()
		require.Len(t, recs, 1, "the hook must be wired into the dispatch path, not merely defined")
		assert.Equal(t, "mcpserver/demo", recs[0])
	})

	t.Run("PostToolCall Deny does NOT fabricate an observation from its synthesized IsError", func(t *testing.T) {
		ft := &originDispatchTool{
			fakeDispatchTool: fakeDispatchTool{
				name: "gh_search", kind: tool.KindMCP,
				result: tool.Result{Content: "the secret body"}, // upstream SUCCEEDED
			},
			origin: "mcpserver/demo",
		}
		l, w := newLoop(t, ft, &fakeHook{point: pipeline.PostToolCall, reason: "result disallowed post-fetch"}, "mcpserver/demo")
		dispatch(t, l, "gh_search")

		recs, clears := w.snapshot()
		assert.Empty(t, recs,
			"the call authenticated fine; a post-gate denial must never be read as a credential failure")
		require.Len(t, clears, 1,
			"and the successful upstream call must still retract any observation the origin carried")
	})

	t.Run("PreToolCall Deny records nothing — the tool never ran", func(t *testing.T) {
		ft := &originDispatchTool{
			fakeDispatchTool: fakeDispatchTool{
				name: "gh_search", kind: tool.KindMCP,
				result: tool.Result{Content: "unauthorized", IsError: true, HTTPStatus: 401},
			},
			origin: "mcpserver/demo",
		}
		l, w := newLoop(t, ft, &fakeHook{point: pipeline.PreToolCall, reason: "access disallowed by fake hook"})
		dispatch(t, l, "gh_search")

		require.Equal(t, 0, ft.executed(), "precondition: the Pre deny skipped Execute")
		recs, clears := w.snapshot()
		assert.Empty(t, recs, "a call that never reached the provider is no evidence about the credential")
		assert.Empty(t, clears)
	})
}
