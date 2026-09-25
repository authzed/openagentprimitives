package sandbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// TestToolkitEnvDefaults_Floor pins the merge itself: the toolkit's declared
// defaults fill in keys nobody has set, and never replace one that is already
// present. A nil toolkit and a toolkit with no envDefaults both leave the
// ToolCall's env exactly as it was — an empty map here would be a behaviour
// change (spec.env stops being omitempty, and the toolcall controller's
// secret-like-key scan starts running over a map it never saw before).
func TestToolkitEnvDefaults_Floor(t *testing.T) {
	tkWith := &toolkit.Toolkit{EnvDefaults: map[string]string{
		"A_MAX_RETRIES": "1",
		"A_QUIET":       "true",
	}}
	cases := []struct {
		name     string
		tk       *toolkit.Toolkit
		existing map[string]string
		want     map[string]string
	}{
		{
			name: "nil toolkit: env untouched (stays nil)",
			tk:   nil,
			want: nil,
		},
		{
			name: "toolkit without envDefaults: env untouched (stays nil)",
			tk:   &toolkit.Toolkit{},
			want: nil,
		},
		{
			name: "toolkit with envDefaults, nothing set: every default lands",
			tk:   tkWith,
			want: map[string]string{"A_MAX_RETRIES": "1", "A_QUIET": "true"},
		},
		{
			name:     "explicit value for the same key wins; the other default still lands",
			tk:       tkWith,
			existing: map[string]string{"A_MAX_RETRIES": "7"},
			want:     map[string]string{"A_MAX_RETRIES": "7", "A_QUIET": "true"},
		},
		{
			name:     "unrelated explicit keys survive alongside the defaults",
			tk:       tkWith,
			existing: map[string]string{"B_OTHER": "x"},
			want:     map[string]string{"A_MAX_RETRIES": "1", "A_QUIET": "true", "B_OTHER": "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.ToolkitEnvDefaults(tc.tk, tc.existing)
			assert.Equal(t, tc.want, got, "merged env")
		})
	}
}

// TestToolkitEnvDefaults_DoesNotAliasTheToolkit guards the shared-map hazard:
// the toolkit object is built once per bundle at synthesis and reused by every
// tool call, so returning it (or writing through it) would let one call's env
// leak into the next.
func TestToolkitEnvDefaults_DoesNotAliasTheToolkit(t *testing.T) {
	tk := &toolkit.Toolkit{EnvDefaults: map[string]string{"A_MAX_RETRIES": "1"}}
	got := sandbox.ToolkitEnvDefaults(tk, nil)
	require.Equal(t, "1", got["A_MAX_RETRIES"], "default lands")
	got["A_MAX_RETRIES"] = "mutated"
	got["A_LEAKED"] = "yes"
	assert.Equal(t, map[string]string{"A_MAX_RETRIES": "1"}, tk.EnvDefaults,
		"the toolkit's own map must not be the one handed to a call")
}

// TestSandboxToolStampsToolkitEnvDefaults covers the sync dispatch path: the
// ToolCall the runner creates carries the toolkit's declared env, so the
// operator's exec composes it and `kubectl get toolcall -o yaml` records it.
func TestSandboxToolStampsToolkitEnvDefaults(t *testing.T) {
	cases := []struct {
		name string
		tk   *toolkit.Toolkit
		want map[string]string
	}{
		{
			name: "toolkit declares envDefaults: they reach spec.env",
			tk:   &toolkit.Toolkit{EnvDefaults: map[string]string{"A_MAX_RETRIES": "1"}},
			want: map[string]string{"A_MAX_RETRIES": "1"},
		},
		{
			name: "toolkit declares none: spec.env stays unset",
			tk:   &toolkit.Toolkit{},
			want: nil,
		},
		{
			name: "no toolkit at all: spec.env stays unset",
			tk:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t)
			reg := operations.New(nil, nil)
			op := reg.Begin("stamp envDefaults")

			st := &tool.SessionContext{
				Namespace:       "default",
				Name:            "sess",
				AgentSessionUID: "uid-envdefaults",
				K8sClient:       c,
				ArtifactClient:  &fakeArtifact{},
				BundleSessions:  map[string]string{"code": "sess-code"},
				Operations:      reg,
			}

			stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
				BundleName:   "code",
				Suffix:       "claude",
				ToolspecName: "claude-code",
				Timeout:      30 * time.Second,
				PollInterval: 5 * time.Millisecond,
				Toolkit:      tc.tk,
			})

			patchToolCallStatus(t, c,
				client.ObjectKey{Namespace: "default", Name: "sess-1-tu-env"},
				func(tc *spiceboxv1alpha1.ToolCall) {
					exit := int32(0)
					tc.Status.ExitCode = &exit
					tc.Status.Conditions = []metav1.Condition{{
						Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue,
						Reason: spiceboxv1alpha1.ReasonProcessExited, LastTransitionTime: metav1.Now(),
					}}
				})

			ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
			t.Cleanup(cancel)
			argsJSON, err := json.Marshal(map[string]any{
				"operation_id": op.ID,
				"_reason":      "run the wrapped CLI once",
				"args":         []string{"--print", "hello"},
			})
			require.NoError(t, err, "marshal args")

			_, err = stool.ExecuteWithIDs(ctx, json.RawMessage(argsJSON), st,
				sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_env"})
			require.NoError(t, err, "ExecuteWithIDs must succeed")

			var created spiceboxv1alpha1.ToolCall
			require.NoError(t,
				c.Get(context.Background(),
					client.ObjectKey{Namespace: "default", Name: "sess-1-tu-env"}, &created),
				"get ToolCall")
			assert.Equal(t, tc.want, created.Spec.Env, "ToolCall spec.env")
		})
	}
}

// TestStreamingToolStampsToolkitEnvDefaults covers the streaming dispatch path,
// which builds its own ToolCall spec. It is the path that matters most here:
// the Claude toolkit's only subcommand declares mode: stream, so a fix that
// only covered the sync path would not reach the CLI whose retry loop this
// exists to cap.
func TestStreamingToolStampsToolkitEnvDefaults(t *testing.T) {
	c := newInteractiveFakeClient(t)

	go func() {
		key := client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}
		for {
			var got spiceboxv1alpha1.ToolCall
			if err := c.Get(context.Background(), key, &got); err == nil {
				got.Status.Streaming = &spiceboxv1alpha1.StreamingEndpoint{
					Available:       true,
					GatewayEndpoint: "passthrough:///bufnet",
				}
				if err := c.Status().Update(context.Background(), &got); err == nil {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, dialer, stop := startEchoGateway(t)
	t.Cleanup(stop)

	reg := operations.New(nil, nil)
	op := reg.Begin("streaming envDefaults")

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "alice",
		AgentSessionUID: "uid-1",
		K8sClient:       c,
		BundleSessions:  map[string]string{"code": "alice-code"},
		Operations:      reg,
	}

	hooks := sandbox.InteractiveHooks{
		OnOutput:           func(_, _ string, _ []byte) {},
		OnTerminal:         func(_, _ string, _ int32) {},
		Register:           func(_ string, _ func([]byte) error) func() { return func() {} },
		IdleTimeoutDefault: time.Minute,
		BridgeDialOpts:     []grpc.DialOption{grpc.WithContextDialer(dialer)},
	}

	ctx, cancel := context.WithTimeout(sandbox.WithInteractiveHooks(context.Background(), hooks), 10*time.Second)
	t.Cleanup(cancel)
	ctx = sandbox.WithIDs(ctx, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu1"})

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "claude",
		ToolspecName: "claude-code",
		PollInterval: 5 * time.Millisecond,
		Subcommand:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeStream},
		Toolkit:      &toolkit.Toolkit{EnvDefaults: map[string]string{"CLAUDE_CODE_MAX_RETRIES": "1"}},
	})

	argsJSON, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "drive a streaming session",
		"args":         []string{},
	})
	require.NoError(t, err, "marshal args")

	// The echo gateway only sends Exit after a CloseSend the bridge never
	// issues, so the dispatch exits on the 10s ctx timeout. The assertion is
	// about the created CR, which Create already landed.
	_, runErr := stool.Execute(ctx, json.RawMessage(argsJSON), sess)
	require.NoError(t, runErr, "Execute must not error")

	var created spiceboxv1alpha1.ToolCall
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}, &created),
		"get ToolCall")
	assert.Equal(t, map[string]string{"CLAUDE_CODE_MAX_RETRIES": "1"}, created.Spec.Env,
		"streaming ToolCall spec.env")
}
