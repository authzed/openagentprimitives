package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// --- helpers --------------------------------------------------------------

type fakeArtifact struct {
	contents map[string]string
	getErr   map[string]error
}

func (f *fakeArtifact) Get(_ context.Context, ref string) (io.ReadCloser, error) {
	if f.getErr != nil {
		if e, ok := f.getErr[ref]; ok {
			return nil, e
		}
	}
	if v, ok := f.contents[ref]; ok {
		return io.NopCloser(strings.NewReader(v)), nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// createErrClient is a thin wrapper that forces Create to return a pre-set
// error — used to exercise the non-IsAlreadyExists branch in ExecuteWithIDs.
type createErrClient struct {
	client.Client
	err error
}

func (c *createErrClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	return c.err
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "register v1alpha1 with scheme")
	return s
}

// newFakeClient builds a fake client wired with the status subresource for
// ToolCall — required for the Succeeded/Failed/Timeout flows. Any extra objs
// (e.g. an AgentSession for the secret-output write-once pre-check) are
// seeded alongside.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		WithObjects(objs...).
		Build()
}

// testCtxTimeout bounds the caller context in the execute-flow tests below.
// It must never be tighter than the SandboxOpts.Timeout those tests give the
// tool (30s) — a shorter caller context races the tool's own budget under
// contention (GC pauses, -race instrumentation overhead, `-p N` scheduling
// pressure from sibling packages), so the test times out on harness noise
// rather than on the code under test.
const testCtxTimeout = 30 * time.Second

// defaultSandboxOpts returns the SandboxOpts used by every watcher-flow test:
// short PollInterval so the test isn't dominated by the watcher's tick.
func defaultSandboxOpts() sandbox.SandboxOpts {
	return sandbox.SandboxOpts{
		BundleName: "code", Suffix: "git", ToolspecName: "git-readonly",
		Timeout: 30 * time.Second, PollInterval: 5 * time.Millisecond,
	}
}

// patchToolCallStatus runs in a goroutine and, once the named ToolCall
// exists, flips its status using the supplied mutator. This stands in for
// what the real ToolCall controller would do.
//
// It polls for existence instead of sleeping a fixed delay and trying Get
// exactly once: the ToolCall is created by the SandboxTool's own
// synchronous Create call, which races this goroutine's start. Under
// scheduling pressure (GC, -race overhead, concurrent sibling packages)
// that Create can take longer than any fixed delay, so a single early Get
// silently no-ops on NotFound — the status is then never patched, and the
// tool's watch loop spins for its entire budget before failing with
// "context canceled before ToolCall terminal" regardless of how large that
// budget is. Polling makes the handoff correct no matter how long the
// Create takes to land.
func patchToolCallStatus(t *testing.T, c client.Client, key client.ObjectKey, mutate func(*spiceboxv1alpha1.ToolCall)) {
	t.Helper()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
		defer cancel()
		var tc spiceboxv1alpha1.ToolCall
		for {
			err := c.Get(ctx, key, &tc)
			if err == nil {
				break
			}
			if !apierrors.IsNotFound(err) {
				// Abandoning the patch silently would surface only as the
				// tool's watch loop spinning out its whole budget and failing
				// with "context canceled before ToolCall terminal" — a
				// misleading symptom for what is really a client error.
				// t.Errorf is safe from a non-test goroutine (unlike
				// t.Fatalf, which must not be called off the test goroutine).
				t.Errorf("patchToolCallStatus: Get ToolCall %s: %v", key, err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
		mutate(&tc)
		_ = c.Status().Update(context.Background(), &tc)
	}()
}

// TestSandboxTool_NotCancellableYet locks the P1b/P4 boundary: sandbox
// interrupt (killing an in-flight ToolCall CR) is deferred to P4, so
// SandboxTool must NOT implement tool.Cancellable yet. If this test starts
// failing because SandboxTool gained a Cancel method, that's the P4 work —
// update this test (and the design docs) deliberately, don't just delete it.
func TestSandboxTool_NotCancellableYet(t *testing.T) {
	var st tool.Tool = sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "code",
		Suffix:     "git",
	})
	_, ok := st.(tool.Cancellable)
	assert.False(t, ok, "sandbox is non-interruptible until the P4 kill path")
}

// --- execute-flow tests ---------------------------------------------------

// TestSandboxToolExecuteSucceeded covers the happy path: ToolCall created,
// status flipped to Succeeded with a stdout artifact, watcher returns
// content + audit entry recorded.
func TestSandboxToolExecuteSucceeded(t *testing.T) {
	c := newFakeClient(t)
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout": "log line one\nlog line two\n",
	}}

	reg := operations.New(nil, nil)
	op := reg.Begin("git-readonly e2e")

	st := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-test-123",
		K8sClient:       c,
		ArtifactClient:  art,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
	}

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "git",
		Description:  "git read-only",
		ToolspecName: "git-readonly",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-1"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.StdoutArtifactRef = "mem://default/sess/uid/stdout"
			tc.Status.Conditions = []metav1.Condition{{
				Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonProcessExited, LastTransitionTime: metav1.Now(),
			}}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	t.Cleanup(cancel)
	argsJSON, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "fetch the most recent commit log",
		"args":         []string{"log", "--oneline", "-10"},
	})
	require.NoError(t, err, "marshal args")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(argsJSON), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_1"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	require.False(t, res.IsError, "unexpected IsError; content=%s", res.Content)
	assert.Contains(t, res.Content, "log line one", "Content missing stdout")

	var tc spiceboxv1alpha1.ToolCall
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess-1-tu-1"}, &tc),
		"get ToolCall")
	require.Len(t, tc.OwnerReferences, 1, "ToolCall should have one owner ref to AgentSession")
	assert.Equal(t, "uid-test-123", string(tc.OwnerReferences[0].UID), "ToolCall owner UID")
	assert.Equal(t, "git-readonly", tc.Labels["agenttoolspec"], "ToolCall agenttoolspec label")
	assert.Equal(t, op.ID, tc.Labels["ap.operation"], "ToolCall ap.operation label")
	assert.Equal(t, "fetch the most recent commit log",
		tc.Annotations["ap.operation/reason"], "ap.operation/reason annotation")

	// Audit entry on the registry.
	got, _ := reg.Get(op.ID)
	require.Len(t, got.Calls, 1, "registry should have 1 call recorded")
	assert.Equal(t, "code_git", got.Calls[0].Tool, "audit entry Tool")
	assert.Equal(t, "fetch the most recent commit log", got.Calls[0].Reason, "audit entry Reason")
	assert.Equal(t, "sess-1-tu-1", got.Calls[0].ToolCallID, "audit entry ToolCallID")
}

// TestSandboxToolInputValidation collapses the four pre-call validation
// failure paths — missing operation_id, unknown operation_id, closed
// operation, missing _reason — into one table. Each row shares: same
// session context shape (no artifact client, no real watcher), same
// SandboxTool, only the body JSON varies plus the substring we expect
// in res.Content.
func TestSandboxToolInputValidation(t *testing.T) {
	// regWithClosedOp returns an Operations registry containing one closed
	// operation whose ID is returned alongside.
	regWithClosedOp := func() (*operations.Registry, string) {
		reg := operations.New(nil, nil)
		op := reg.Begin("finished work")
		reg.Close(op.ID)
		return reg, op.ID
	}
	regWithOpenOp := func() (*operations.Registry, string) {
		reg := operations.New(nil, nil)
		op := reg.Begin("x")
		return reg, op.ID
	}

	cases := []struct {
		name              string
		bodyFor           func(opID string) []byte
		registry          func() (*operations.Registry, string)
		wantErrSubstrings []string
	}{
		{
			name: "missing operation_id: IsError mentions operation_id is required",
			bodyFor: func(_ string) []byte {
				return []byte(`{"args":["log"]}`)
			},
			registry:          func() (*operations.Registry, string) { return operations.New(nil, nil), "" },
			wantErrSubstrings: []string{"operation_id is required"},
		},
		{
			name: "unknown operation_id: IsError mentions 'not registered'",
			bodyFor: func(_ string) []byte {
				return []byte(`{"operation_id":"op-bogus","_reason":"x","args":["log"]}`)
			},
			registry:          func() (*operations.Registry, string) { return operations.New(nil, nil), "" },
			wantErrSubstrings: []string{"not registered"},
		},
		{
			name: "closed operation: IsError mentions 'closed' and includes operation_id",
			bodyFor: func(opID string) []byte {
				b, _ := json.Marshal(map[string]any{
					"operation_id": opID, "_reason": "retrying", "args": []string{"log"},
				})
				return b
			},
			registry:          regWithClosedOp,
			wantErrSubstrings: []string{"closed"},
		},
		{
			name: "missing _reason: IsError mentions '_reason is required'",
			bodyFor: func(opID string) []byte {
				b, _ := json.Marshal(map[string]any{
					"operation_id": opID, "args": []string{"log"},
				})
				return b
			},
			registry:          regWithOpenOp,
			wantErrSubstrings: []string{"_reason is required"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, opID := tc.registry()
			st := &tool.SessionContext{
				Namespace:      "default",
				Name:           "sess",
				K8sClient:      fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
				BundleSessions: map[string]string{"code": "sess-code"},
				Operations:     reg,
			}
			stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
				BundleName: "code", Suffix: "git", ToolspecName: "git-readonly",
			})
			res, err := stool.ExecuteWithIDs(context.Background(),
				tc.bodyFor(opID), st, sandbox.IDs{TurnIndex: 0, ToolUseID: "x"})
			require.NoError(t, err, "ExecuteWithIDs must return no top-level error")
			require.True(t, res.IsError, "expected IsError; got %+v", res)
			for _, want := range tc.wantErrSubstrings {
				assert.Contains(t, res.Content, want, "res.Content missing expected substring")
			}
			// "closed" case: also assert opID appears in the message.
			if opID != "" && strings.Contains(tc.name, "closed operation") {
				assert.Contains(t, res.Content, opID, "error message should contain operation_id")
			}
		})
	}
}

// TestSandboxToolCreateError verifies a non-IsAlreadyExists Create failure
// surfaces as IsError carrying the underlying message.
func TestSandboxToolCreateError(t *testing.T) {
	s := newScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).Build()
	cli := &createErrClient{Client: base, err: errors.New("apiserver exploded")}

	reg := operations.New(nil, nil)
	op := reg.Begin("x")
	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      cli,
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "code", Suffix: "git", ToolspecName: "git-readonly",
	})
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID, "_reason": "test", "args": []string{"log"},
	})
	require.NoError(t, err, "marshal body")
	res, err := stool.ExecuteWithIDs(context.Background(), body, st, sandbox.IDs{TurnIndex: 0, ToolUseID: "x"})
	require.NoError(t, err, "ExecuteWithIDs must return no top-level error")
	require.True(t, res.IsError, "expected IsError; got %+v", res)
	assert.Contains(t, res.Content, "create ToolCall", "Content must mention create-ToolCall")
	assert.Contains(t, res.Content, "apiserver exploded", "Content must mention underlying cause")
}

// TestSandboxToolWatcherOutcomes covers the five ToolCall-watcher
// outcomes: Succeeded with stderr was already exercised above; here we
// table the five remaining flows that share the same shape (fake
// client, goroutine that patches status, then assertion on the result
// returned by ExecuteWithIDs).
func TestSandboxToolWatcherOutcomes(t *testing.T) {
	type setup struct {
		// artifactContents is keyed by artifact ref; the test wires the
		// matching ToolCall Status fields below.
		artifactContents map[string]string
		artifactErrs     map[string]error
		// mutate is applied to the ToolCall after a short delay to
		// simulate the operator-side reconcile.
		mutate func(tc *spiceboxv1alpha1.ToolCall)
	}

	cases := []struct {
		name           string
		setup          setup
		wantIsError    bool
		wantSubstrings []string
		// tailLowerSubstrings: assert at least one of these appears in
		// lower-cased res.Content (used for timeout-flavored wording).
		anyLowerSubstrings []string
	}{
		{
			name: "Succeeded with stderr-on-failure -> IsError summary + stderr tail",
			setup: setup{
				artifactContents: map[string]string{
					"mem://default/sess/uid/stderr": "line a\nline b\nfatal: bang\n",
				},
				mutate: func(tc *spiceboxv1alpha1.ToolCall) {
					exit := int32(127)
					tc.Status.ExitCode = &exit
					tc.Status.StderrArtifactRef = "mem://default/sess/uid/stderr"
					tc.Status.Conditions = []metav1.Condition{{
						Type:               spiceboxv1alpha1.ToolCallConditionFailed,
						Status:             metav1.ConditionTrue,
						Reason:             spiceboxv1alpha1.ReasonProcessExited,
						Message:            "exit 127",
						LastTransitionTime: metav1.Now(),
					}}
				},
			},
			wantIsError:    true,
			wantSubstrings: []string{"ToolCall failed", "fatal: bang"},
		},
		{
			name: "Timeout condition -> IsError with timeout-flavored content",
			setup: setup{
				mutate: func(tc *spiceboxv1alpha1.ToolCall) {
					tc.Status.Conditions = []metav1.Condition{{
						Type: spiceboxv1alpha1.ToolCallConditionTimeout, Status: metav1.ConditionTrue,
						Reason: "DeadlineExceeded", Message: "exceeded 30s timeout",
						LastTransitionTime: metav1.Now(),
					}}
				},
			},
			wantIsError:        true,
			anyLowerSubstrings: []string{"timeout", "deadlineexceeded"},
		},
		{
			name: "Succeeded but artifact read errors -> non-fatal sentinel mentions ref+error",
			setup: setup{
				artifactErrs: map[string]error{
					"mem://default/sess/uid/stdout": fmt.Errorf("network sad"),
				},
				mutate: func(tc *spiceboxv1alpha1.ToolCall) {
					exit := int32(0)
					tc.Status.ExitCode = &exit
					tc.Status.StdoutArtifactRef = "mem://default/sess/uid/stdout"
					tc.Status.Conditions = []metav1.Condition{{
						Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue,
						Reason: spiceboxv1alpha1.ReasonProcessExited, LastTransitionTime: metav1.Now(),
					}}
				},
			},
			wantIsError:    false,
			wantSubstrings: []string{"mem://default/sess/uid/stdout", "network sad"},
		},
		{
			name: "Succeeded with 64KiB stdout -> truncated with '...(truncated)' marker",
			setup: setup{
				artifactContents: map[string]string{
					// 64 KiB > maxInlineStdout (32 KiB).
					"mem://default/sess/uid/stdout": strings.Repeat("X", 64*1024),
				},
				mutate: func(tc *spiceboxv1alpha1.ToolCall) {
					exit := int32(0)
					tc.Status.ExitCode = &exit
					tc.Status.StdoutArtifactRef = "mem://default/sess/uid/stdout"
					tc.Status.Conditions = []metav1.Condition{{
						Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue,
						Reason: spiceboxv1alpha1.ReasonProcessExited, LastTransitionTime: metav1.Now(),
					}}
				},
			},
			wantIsError:    false,
			wantSubstrings: []string{"...(truncated)"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t)
			art := &fakeArtifact{
				contents: tc.setup.artifactContents,
				getErr:   tc.setup.artifactErrs,
			}
			reg := operations.New(nil, nil)
			op := reg.Begin("e2e")
			st := &tool.SessionContext{
				Namespace: "default", Name: "sess", AgentSessionUID: "uid-1",
				K8sClient: c, ArtifactClient: art,
				BundleSessions: map[string]string{"code": "sess-code"},
				Operations:     reg,
			}
			stool := sandbox.NewSandboxTool(defaultSandboxOpts())

			patchToolCallStatus(t, c,
				client.ObjectKey{Namespace: "default", Name: "sess-1-tu-1"},
				tc.setup.mutate)

			ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
			t.Cleanup(cancel)
			body, err := json.Marshal(map[string]any{
				"operation_id": op.ID, "_reason": "x", "args": []string{"log"},
			})
			require.NoError(t, err, "marshal body")
			res, err := stool.ExecuteWithIDs(ctx, body, st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_1"})
			require.NoError(t, err, "ExecuteWithIDs must succeed")
			assert.Equal(t, tc.wantIsError, res.IsError, "IsError: got %+v", res)
			for _, want := range tc.wantSubstrings {
				assert.Contains(t, res.Content, want, "Content missing expected substring")
			}
			if len(tc.anyLowerSubstrings) > 0 {
				lower := strings.ToLower(res.Content)
				var matched bool
				for _, want := range tc.anyLowerSubstrings {
					if strings.Contains(lower, want) {
						matched = true
						break
					}
				}
				assert.True(t, matched,
					"Content missing any of %v: %q", tc.anyLowerSubstrings, res.Content)
			}
		})
	}
}

// TestSandboxToolPermissionResolution covers the three Permission resolution
// outcomes: subcommand-level wins, toolkit-level fallback, and zero when
// neither declares one.
func TestSandboxToolPermissionResolution(t *testing.T) {
	tkWithReadonly := func() *toolkit.Toolkit {
		return &toolkit.Toolkit{
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType: "x", ResourceIDTemplate: "{a}", Permission: "read",
				},
			},
		}
	}
	subWithReadwrite := func() *toolkit.Subcommand {
		return &toolkit.Subcommand{
			Permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType: "x", ResourceIDTemplate: "{a}", Permission: "admin",
				},
			},
		}
	}

	cases := []struct {
		name       string
		toolkit    *toolkit.Toolkit
		subcommand *toolkit.Subcommand
		wantImpact authz.StateImpact
		toolspecID string
	}{
		{
			name:       "subcommand override: subcommand readwrite wins over toolkit readonly",
			toolkit:    tkWithReadonly(),
			subcommand: subWithReadwrite(),
			wantImpact: authz.Readwrite,
			toolspecID: "git-rw",
		},
		{
			name:       "toolkit default: subcommand has no Permission, toolkit readonly applies",
			toolkit:    tkWithReadonly(),
			subcommand: &toolkit.Subcommand{},
			wantImpact: authz.Readonly,
			toolspecID: "git-ro",
		},
		{
			name:       "neither set: zero Permission (AgentClass validation rejects this)",
			toolkit:    &toolkit.Toolkit{},
			subcommand: &toolkit.Subcommand{},
			wantImpact: "",
			toolspecID: "git-missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
				BundleName: "code", Suffix: "git", ToolspecName: tc.toolspecID,
				Toolkit:    tc.toolkit,
				Subcommand: tc.subcommand,
			})
			assert.Equal(t, tc.wantImpact, st.Permission().StateImpact, "StateImpact")
		})
	}
}

// TestSandboxToolOrigin verifies that SandboxTool implements tool.OriginTool
// and returns "toolkit/<ref>" when a toolkit is present, and "" when the
// tool has no toolkit (origin-less path).
func TestSandboxToolOrigin(t *testing.T) {
	cases := []struct {
		name       string
		toolkit    *toolkit.Toolkit
		wantOrigin string
	}{
		{
			name: "with toolkit ref: returns toolkit/<name>",
			toolkit: &toolkit.Toolkit{
				Name: "git-toolkit",
			},
			wantOrigin: "toolkit/git-toolkit",
		},
		{
			name:       "without toolkit (nil): returns empty string (origin-less)",
			toolkit:    nil,
			wantOrigin: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
				BundleName:   "code",
				Suffix:       "git",
				ToolspecName: "git-readonly",
				Toolkit:      tc.toolkit,
			})
			ot, ok := any(st).(tool.OriginTool)
			require.True(t, ok, "SandboxTool must implement tool.OriginTool")
			assert.Equal(t, tc.wantOrigin, ot.Origin())
		})
	}
}

// TestSandboxTool_Introspect_RendersContract verifies SandboxTool
// satisfies tool.Introspectable and that Introspect renders the full
// allowed-subcommand contract via the toolspec renderer.
func TestSandboxTool_Introspect_RendersContract(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name:            "git",
		ToolkitRevision: "2026-05-01",
		Target:          toolkit.Target{Binary: "git"},
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"log"}, Description: "Show commit history."},
		},
	}
	sp := &spec.Spec{
		Name:             "git-readonly",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "git", Revision: "2026-05-01"},
		AllowSubcommands: []string{"log"},
	}

	st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "git",
		ToolspecName: "git-readonly",
		Toolkit:      tk,
		Spec:         sp,
	})

	intro, ok := tool.Tool(st).(tool.Introspectable)
	require.True(t, ok, "SandboxTool must implement tool.Introspectable")

	out, err := intro.Introspect()
	require.NoError(t, err, "Introspect must succeed")
	assert.NotEmpty(t, out, "Introspect output should be non-empty")
	// The renderer emits "<binary> <subcommand>" for each allowed
	// subcommand — assert the fixture's subcommand survives the rendering.
	assert.Contains(t, out, "git log", "rendered contract must carry the allowed subcommand")
}

// --- secretOutput result composition tests --------------------------------

// succeededTC builds a ToolCall whose Status indicates success, with the given
// stdout artifact ref. Helper shared across secret-output tests.
func succeededTC(stdoutRef string) *spiceboxv1alpha1.ToolCall {
	exit := int32(0)
	return &spiceboxv1alpha1.ToolCall{
		Status: spiceboxv1alpha1.ToolCallStatus{
			ExitCode:          &exit,
			StdoutArtifactRef: stdoutRef,
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonProcessExited,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestComposeResult_SecretOutput_StdoutSource verifies that when a succeeded
// ToolCall carries a secretOutput spec with source "stdout", composeResult
// returns the raw stdout content in Result.Content and populates SecretOutput
// so the runner's applySecretOutput can divert the value to the store. The
// Description comes from the spec, not from stdout (stdout IS the value).
func TestComposeResult_SecretOutput_StdoutSource(t *testing.T) {
	const stdoutValue = "KUBECONFIG-BYTES"
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout": stdoutValue,
	}}
	tc := succeededTC("mem://default/sess/uid/stdout")

	soSpec := &spec.SecretOutputSpec{
		Name:        "kubeconfig",
		Source:      "stdout",
		Description: "admin kubeconfig; expires 1h",
	}
	res := sandbox.ComposeResult(context.Background(), tc, art, soSpec)

	require.False(t, res.IsError, "ComposeResult must not set IsError for a succeeded ToolCall")
	// Content must be the raw stdout value — the runner loop (applySecretOutput)
	// is responsible for diverting it; composeResult just surfaces it here.
	assert.Equal(t, stdoutValue, res.Content, "Content must be the raw stdout value")
	require.NotNil(t, res.SecretOutput, "SecretOutput must be set for stdout-sourced secret")
	assert.Equal(t, "kubeconfig", res.SecretOutput.Name, "SecretOutput.Name")
	assert.Equal(t, "admin kubeconfig; expires 1h", res.SecretOutput.Description, "SecretOutput.Description")
}

// TestComposeResult_SecretOutput_NilSpec verifies that the normal (non-secret)
// result shape is unchanged when no secretOutput spec is present.
func TestComposeResult_SecretOutput_NilSpec(t *testing.T) {
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout": "normal output\n",
	}}
	tc := succeededTC("mem://default/sess/uid/stdout")

	res := sandbox.ComposeResult(context.Background(), tc, art, nil)

	require.False(t, res.IsError, "ComposeResult must not set IsError for a succeeded ToolCall")
	assert.Nil(t, res.SecretOutput, "SecretOutput must be nil when no spec is provided")
	assert.Contains(t, res.Content, "normal output", "Content must include stdout for normal result")
}

// succeededTCWithOutputs builds a succeeded ToolCall whose Status carries both
// a stdout artifact ref and a set of harvested output artifacts. Used by the
// file:-source secret-output tests.
func succeededTCWithOutputs(stdoutRef string, outputs []spiceboxv1alpha1.OutputArtifact) *spiceboxv1alpha1.ToolCall {
	tc := succeededTC(stdoutRef)
	tc.Status.OutputArtifacts = outputs
	return tc
}

// TestComposeResult_SecretOutput_FileSource verifies that when a succeeded
// ToolCall carries a secretOutput spec with source "file:<path>", composeResult
// reads the matching harvested output artifact (the FILE bytes) into
// Result.Content and sets SecretOutput so the runner diverts it. For file:
// source the producer's stdout becomes the Description.
func TestComposeResult_SecretOutput_FileSource(t *testing.T) {
	const fileValue = "FILE-KUBECONFIG-BYTES"
	const stdoutValue = "wrote kubeconfig to /home/agent/.kube/config\n"
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout":  stdoutValue,
		"mem://default/sess/uid/out/cfg": fileValue,
	}}
	// tar strips the leading '/', so the harvested Path is the slash-stripped
	// form of the requested capture path.
	tc := succeededTCWithOutputs("mem://default/sess/uid/stdout", []spiceboxv1alpha1.OutputArtifact{
		{Path: "home/agent/.kube/config", ArtifactRef: "mem://default/sess/uid/out/cfg", Size: int64(len(fileValue))},
	})

	soSpec := &spec.SecretOutputSpec{
		Name:        "kubeconfig",
		Source:      "file:/home/agent/.kube/config",
		Description: "admin kubeconfig; expires 1h",
	}
	res := sandbox.ComposeResult(context.Background(), tc, art, soSpec)

	require.False(t, res.IsError, "ComposeResult must not set IsError for a succeeded ToolCall; content=%s", res.Content)
	assert.Equal(t, fileValue, res.Content, "Content must be the captured FILE bytes")
	require.NotNil(t, res.SecretOutput, "SecretOutput must be set for file-sourced secret")
	assert.Equal(t, "kubeconfig", res.SecretOutput.Name, "SecretOutput.Name")
	// For file: source, the stdout IS the human-visible Description.
	assert.Equal(t, stdoutValue, res.SecretOutput.Description, "SecretOutput.Description must be the producer's stdout")
}

// TestComposeResult_SecretOutput_FileSource_MissingFile verifies that when a
// file:-source secret output is declared but the producer never wrote the
// declared file (no matching harvested artifact), composeResult returns an
// error result with no SecretOutput — the producer failed to produce the secret.
func TestComposeResult_SecretOutput_FileSource_MissingFile(t *testing.T) {
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout": "did not write the file\n",
	}}
	// No OutputArtifacts harvested — the declared file is absent.
	tc := succeededTCWithOutputs("mem://default/sess/uid/stdout", nil)

	soSpec := &spec.SecretOutputSpec{
		Name:        "kubeconfig",
		Source:      "file:/home/agent/.kube/config",
		Description: "admin kubeconfig",
	}
	res := sandbox.ComposeResult(context.Background(), tc, art, soSpec)

	assert.True(t, res.IsError, "missing captured file must be an error result")
	assert.Nil(t, res.SecretOutput, "no SecretOutput when the file was not produced")
	assert.Contains(t, res.Content, "/home/agent/.kube/config", "error must name the missing file path")
}

// TestComposeResult_SecretOutput_FileSource_EmptyFile verifies that an empty
// captured file is treated as a production failure: the producer ran but the
// secret has no bytes. No SecretOutput is set.
func TestComposeResult_SecretOutput_FileSource_EmptyFile(t *testing.T) {
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout":    "ran\n",
		"mem://default/sess/uid/out/empty": "",
	}}
	tc := succeededTCWithOutputs("mem://default/sess/uid/stdout", []spiceboxv1alpha1.OutputArtifact{
		{Path: "home/agent/.kube/config", ArtifactRef: "mem://default/sess/uid/out/empty", Size: 0},
	})

	soSpec := &spec.SecretOutputSpec{
		Name:        "kubeconfig",
		Source:      "file:/home/agent/.kube/config",
		Description: "admin kubeconfig",
	}
	res := sandbox.ComposeResult(context.Background(), tc, art, soSpec)

	assert.True(t, res.IsError, "empty captured file must be an error result")
	assert.Nil(t, res.SecretOutput, "no SecretOutput when the captured file is empty")
}

// These tests drive ExecuteWithIDs against a ToolCall whose status is already
// terminal, so the work is a create, a 5ms poll, and an artifact fetch —
// milliseconds on an idle box. They used to bound that with a hardcoded 2s
// context, which made the CONTEXT the binding constraint rather than the tool's
// own 30s Timeout, and under `-race -p=4` on a loaded machine 2s is not a safe
// margin: the package produced a moving set of failures (this test and
// TestSandboxInteractive_..., each passing in isolation) that read as a
// regression and was scheduling delay.
//
// t.Context() ties the deadline to the test binary's own -timeout instead. It
// still bounds a genuine hang; it just is not a number that has to be re-tuned
// against whatever else the machine is doing. Do not reintroduce a fixed
// deadline here unless the test is specifically asserting timeout BEHAVIOUR.

// TestSandboxTool_SecretOutput_StdoutSource_EndToEnd verifies the full
// ExecuteWithIDs path when SandboxOpts.Spec carries a secretOutput declaration
// with source "stdout": the returned Result has SecretOutput set and Content
// equals the raw stdout bytes (ready for applySecretOutput to divert).
func TestSandboxTool_SecretOutput_StdoutSource_EndToEnd(t *testing.T) {
	// Seed the AgentSession the write-once pre-check re-Gets; empty status
	// means "kubeconfig" is not yet satisfied, so execution proceeds.
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
	})
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/kubeconfig": "KUBECONFIG-BYTES",
	}}

	reg := operations.New(nil, nil)
	op := reg.Begin("generate kubeconfig")

	st := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-so-123",
		K8sClient:       c,
		ArtifactClient:  art,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
		SecretOut:       secretout.NewSessionStore("uid-so-123"),
	}

	sp := &spec.Spec{
		Name:             "kubeconfig-gen",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "kubectl", Revision: "2026-01-01"},
		AllowSubcommands: []string{"config"},
		SecretOutput: &spec.SecretOutputSpec{
			Name:        "kubeconfig",
			Source:      "stdout",
			Description: "admin kubeconfig; expires 1h",
		},
	}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubeconfig",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         sp,
	})

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-kc"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.StdoutArtifactRef = "mem://default/sess/uid/kubeconfig"
			tc.Status.Conditions = []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonProcessExited,
				LastTransitionTime: metav1.Now(),
			}}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	t.Cleanup(cancel)
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "generate kubeconfig for cluster admin",
		"args":         []string{"config", "view", "--raw"},
	})
	require.NoError(t, err, "marshal body")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_kc"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	require.False(t, res.IsError, "unexpected IsError; content=%s", res.Content)
	assert.Equal(t, "KUBECONFIG-BYTES", res.Content, "Content must be raw stdout (not wrapped)")
	require.NotNil(t, res.SecretOutput, "SecretOutput must be set")
	assert.Equal(t, "kubeconfig", res.SecretOutput.Name)
	assert.Equal(t, "admin kubeconfig; expires 1h", res.SecretOutput.Description)
}

// TestSandboxTool_SecretOutput_FileSource_EndToEnd verifies the full
// ExecuteWithIDs path when SandboxOpts.Spec carries a secretOutput declaration
// with source "file:<path>": the created ToolCall requests the file via
// CaptureOutputs, and the returned Result has Content = the captured file bytes,
// SecretOutput set, and Description = the producer's stdout.
func TestSandboxTool_SecretOutput_FileSource_EndToEnd(t *testing.T) {
	const fileValue = "FILE-KUBECONFIG-BYTES"
	const stdoutValue = "wrote /home/agent/.kube/config\n"
	// Seed the AgentSession the write-once pre-check re-Gets; empty status
	// means "kubeconfig" is not yet satisfied, so execution proceeds.
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
	})
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stdout":  stdoutValue,
		"mem://default/sess/uid/out/cfg": fileValue,
	}}

	reg := operations.New(nil, nil)
	op := reg.Begin("generate kubeconfig file")

	st := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-so-file",
		K8sClient:       c,
		ArtifactClient:  art,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
		SecretOut:       secretout.NewSessionStore("uid-so-file"),
	}

	const capturePath = "/home/agent/.kube/config"
	sp := &spec.Spec{
		Name:             "kubeconfig-gen",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "kubectl", Revision: "2026-01-01"},
		AllowSubcommands: []string{"config"},
		SecretOutput: &spec.SecretOutputSpec{
			Name:        "kubeconfig",
			Source:      "file:" + capturePath,
			Description: "ignored for file: source — stdout becomes the description",
		},
	}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubeconfig",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         sp,
	})

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-kc"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.StdoutArtifactRef = "mem://default/sess/uid/stdout"
			// tar strips the leading '/', so the harvested Path is slash-stripped.
			tc.Status.OutputArtifacts = []spiceboxv1alpha1.OutputArtifact{
				{Path: "home/agent/.kube/config", ArtifactRef: "mem://default/sess/uid/out/cfg", Size: int64(len(fileValue))},
			}
			tc.Status.Conditions = []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonProcessExited,
				LastTransitionTime: metav1.Now(),
			}}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	t.Cleanup(cancel)
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "generate kubeconfig file for cluster admin",
		"args":         []string{"config", "view", "--raw"},
	})
	require.NoError(t, err, "marshal body")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_kc"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	require.False(t, res.IsError, "unexpected IsError; content=%s", res.Content)
	assert.Equal(t, fileValue, res.Content, "Content must be the captured file bytes (not stdout)")
	require.NotNil(t, res.SecretOutput, "SecretOutput must be set")
	assert.Equal(t, "kubeconfig", res.SecretOutput.Name)
	assert.Equal(t, stdoutValue, res.SecretOutput.Description, "Description must be the producer's stdout")

	// The created ToolCall must have requested the declared file via CaptureOutputs.
	var tc spiceboxv1alpha1.ToolCall
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sess-1-tu-kc"}, &tc),
		"get ToolCall")
	assert.Contains(t, tc.Spec.CaptureOutputs, capturePath,
		"ToolCall must request the declared secret-output file via CaptureOutputs")
}
