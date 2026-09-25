// pkg/agent/tool/sandbox/authfail_carrier_test.go
//
// The sandbox half of the credential-update corroboration surface. A CLI
// toolkit exposes no HTTP status, so the only things the platform can
// independently observe about "did this credential get rejected" are the
// process exit code and the stderr the ToolCall harvested. These tests pin that
// both reach tool.Result — the out-of-band carrier the runner's recorder reads
// — and that they are carried only where they are genuinely observed.
package sandbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func exitPtr(v int32) *int32 { return &v }

// secretOutStdoutSpec is the stdout-sourced secret-output declaration whose
// composition path must not gain a second copy of tool output.
func secretOutStdoutSpec() *spec.SecretOutputSpec {
	return &spec.SecretOutputSpec{Name: "kubeconfig", Source: "stdout", Description: "admin kubeconfig"}
}

// execArgs marshals the minimal valid sandbox tool argv for opID.
func execArgs(t *testing.T, opID string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"operation_id": opID,
		"_reason":      "observe an auth-shaped toolkit failure",
		"args":         []string{"auth", "status"},
	})
	require.NoError(t, err, "marshal sandbox args")
	return b
}

// terminalToolCall builds a terminal ToolCall status the way the toolcall
// controller would: one True condition, an exit code, and artifact refs.
func terminalToolCall(condType string, exit *int32, stdoutRef, stderrRef string) func(*spiceboxv1alpha1.ToolCall) {
	return func(tc *spiceboxv1alpha1.ToolCall) {
		tc.Status.Conditions = []metav1.Condition{{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             "ExitNonZero",
			Message:            "the tool exited non-zero",
			LastTransitionTime: metav1.Now(),
		}}
		tc.Status.ExitCode = exit
		tc.Status.StdoutArtifactRef = stdoutRef
		tc.Status.StderrArtifactRef = stderrRef
	}
}

// TestComposeResultCarriesTheCLIObservation covers the sync ToolCall path.
func TestComposeResultCarriesTheCLIObservation(t *testing.T) {
	ctx := context.Background()
	art := &fakeArtifact{contents: map[string]string{
		"mem://out": "ok\n",
		"mem://err": "gh: Bad credentials (HTTP 401)\n",
	}}

	t.Run("failed call carries both the exit code and the captured stderr", func(t *testing.T) {
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionFailed, exitPtr(4), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		require.True(t, res.IsError, "precondition: the Failed condition composes an error result")
		require.NotNil(t, res.ExitCode, "an exit code the ToolCall reported must not be dropped")
		assert.Equal(t, int32(4), *res.ExitCode)
		assert.Contains(t, res.Stderr, "Bad credentials",
			"stderrPatterns have nothing to match against unless the captured stderr is carried")
	})

	t.Run("failed call with NO exit code recorded leaves ExitCode nil", func(t *testing.T) {
		// A ToolCall that failed before the process ran (image pull, timeout)
		// reports no exit code. Synthesizing a zero here would let a provider
		// declaring exitCodes:[0] corroborate a failure it never saw.
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionTimeout, nil, "", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		assert.Nil(t, res.ExitCode, "no observed exit code must stay unobserved, never a synthesized 0")
	})

	t.Run("succeeded call carries the exit code but not stderr", func(t *testing.T) {
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionSucceeded, exitPtr(0), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		require.False(t, res.IsError, "precondition: Succeeded composes a non-error result")
		require.NotNil(t, res.ExitCode)
		assert.Equal(t, int32(0), *res.ExitCode)
		assert.Empty(t, res.Stderr,
			"a successful call is never classified, and its stderr is not in Content either — do not widen it")
	})

	t.Run("succeeded secret-output call leaks nothing into the carrier", func(t *testing.T) {
		// Content here IS the secret value. The carrier must not become a
		// second copy of anything on this path.
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionSucceeded, exitPtr(0), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, secretOutStdoutSpec())

		require.NotNil(t, res.SecretOutput, "precondition: this is the secret-output composition path")
		assert.Empty(t, res.Stderr)
	})

	t.Run("succeeded call reports the origin as authenticated", func(t *testing.T) {
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionSucceeded, exitPtr(0), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		assert.True(t, res.OriginAuthenticated,
			"a process that ran and exited 0 proves the credential behind it works")
	})

	t.Run("SUCCEEDED call whose secret output failed still reports the origin as authenticated", func(t *testing.T) {
		// The exact shape the corroboration path used to mishandle: the
		// ToolCall SUCCEEDED (the CLI ran, exit 0, credential accepted) but
		// composeToolCallResult returns IsError=true from inside its succeeded
		// branch because the declared secret file was never produced. Gating
		// the positive carrier on !IsError would send this to the recorder as a
		// failure, which retracts nothing — so a stale auth-failure observation
		// would survive a credential that demonstrably works.
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionSucceeded, exitPtr(0), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, &spec.SecretOutputSpec{
			Name:        "kubeconfig",
			Source:      "file:/tmp/kubeconfig",
			Description: "admin kubeconfig",
		})

		require.True(t, res.IsError, "precondition: an unproduced secret file composes an error result")
		assert.True(t, res.OriginAuthenticated,
			"the error is about the SECRET, not the credential; the process still exited 0")
		assert.Empty(t, res.Stderr, "and the succeeded path still withholds stderr")
	})

	t.Run("failed call does NOT report the origin as authenticated", func(t *testing.T) {
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionFailed, exitPtr(4), "mem://out", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		assert.False(t, res.OriginAuthenticated,
			"false must mean 'not observed'; a non-zero exit is not proof of anything about the credential")
	})

	t.Run("a call that never reached a process does NOT report the origin as authenticated", func(t *testing.T) {
		tc := &spiceboxv1alpha1.ToolCall{}
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionTimeout, nil, "", "mem://err")(tc)

		res := sandbox.ComposeResult(ctx, tc, art, nil)

		assert.False(t, res.OriginAuthenticated)
	})
}

// TestComposeStreamResultCarriesTheCLIObservation covers the stream/interactive
// bridge path. A toolkit's authFailure block is declared per PROVIDER, not per
// ToolCall mode, so a credential that dies under `sync` dies identically under
// `stream` — and the long-running agent-shaped CLIs that live on this path are
// exactly the credential-bearing ones.
func TestComposeStreamResultCarriesTheCLIObservation(t *testing.T) {
	t.Run("failed stream carries the bridge exit code and the stderr tail", func(t *testing.T) {
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 4, ExitReason: "failed"},
			nil, []byte("partial\n"), []byte("gh: Bad credentials (HTTP 401)\n"),
		)

		require.True(t, res.IsError, "precondition: ExitReason failed composes an error result")
		require.NotNil(t, res.ExitCode)
		assert.Equal(t, int32(4), *res.ExitCode)
		assert.Contains(t, res.Stderr, "Bad credentials")
	})

	t.Run("parser-reported failure on a CLEANLY EXITED process carries no stderr", func(t *testing.T) {
		// composeStreamResult's isErr is wider than "the process failed": the
		// stream parser raises it for a recognized terminal result with
		// OK=false, on a process that exited 0 and reported ExitReason
		// "completed". Gating the carrier on isErr would hand the classifier a
		// clean process's stderr — precisely what the sync path refuses.
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			&toolkitstream.Outcome{HasResult: true, OK: false, Text: "the model declined"},
			[]byte("done\n"), []byte("gh: Bad credentials (HTTP 401)\n"),
		)

		require.True(t, res.IsError, "precondition: the parser outcome makes this an error result")
		require.NotNil(t, res.ExitCode)
		require.Equal(t, int32(0), *res.ExitCode, "precondition: the process itself exited cleanly")
		assert.Empty(t, res.Stderr,
			"a process that exited 0 is not evidence its credential was rejected, whatever it printed")
	})

	t.Run("a watchdog-killed stream (idle / maxDuration) carries no stderr", func(t *testing.T) {
		// idle and maxDuration are OUR timer killing the process, not the
		// credential failing.
		for _, reason := range []string{"idle", "maxDuration"} {
			res := sandbox.ComposeStreamResult(
				sandbox.BridgeResult{ExitCode: 0, ExitReason: reason},
				nil, nil, []byte("gh: Bad credentials (HTTP 401)\n"),
			)
			assert.Emptyf(t, res.Stderr, "ExitReason %q is a platform-initiated kill, not credential evidence", reason)
		}
	})

	t.Run("a stream that never received an Exit frame reports NO exit code", func(t *testing.T) {
		// BridgeResult is initialized to ExitCode -1 and only an Exit frame
		// overwrites it, so -1 is the bridge's "nothing was observed" sentinel,
		// not a process status (the gateway sources Exit.Code from a wait
		// status, which is never negative). Carrying it would hand the
		// classifier a value it could be told to match on.
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: -1, ExitReason: "completed"},
			nil, []byte("partial\n"), nil,
		)

		assert.Nil(t, res.ExitCode, "the -1 sentinel must stay unobserved, never surface as an exit code")
	})

	t.Run("completed stream carries the exit code but not the stderr tail", func(t *testing.T) {
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			nil, []byte("done\n"), []byte("a warning\n"),
		)

		require.False(t, res.IsError)
		require.NotNil(t, res.ExitCode)
		assert.Equal(t, int32(0), *res.ExitCode)
		assert.Empty(t, res.Stderr)
		assert.True(t, res.OriginAuthenticated, "a clean exit 0 proves the credential behind it works")
	})

	t.Run("parser-reported failure on a CLEANLY EXITED process still reports the origin as authenticated", func(t *testing.T) {
		// Same divergence as the stderr case above, in the other direction:
		// isErr is the parser's verdict on the WORK, and the process still
		// exited 0. This must retract a stale observation, not add to one.
		res := sandbox.ComposeStreamResult(
			sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			&toolkitstream.Outcome{HasResult: true, OK: false, Text: "the model declined"},
			[]byte("done\n"), nil,
		)

		require.True(t, res.IsError, "precondition: the parser outcome makes this an error result")
		assert.True(t, res.OriginAuthenticated)
	})

	t.Run("streams with nothing observed do NOT report the origin as authenticated", func(t *testing.T) {
		cases := map[string]sandbox.BridgeResult{
			// The bridge's initial state: "completed" with the -1 sentinel means
			// no Exit frame ever arrived, so nothing was observed.
			"no Exit frame arrived":     {ExitCode: -1, ExitReason: "completed"},
			"non-zero exit":             {ExitCode: 4, ExitReason: "failed"},
			"watchdog idle kill":        {ExitCode: 0, ExitReason: "idle"},
			"watchdog maxDuration kill": {ExitCode: 0, ExitReason: "maxDuration"},
		}
		for name, br := range cases {
			t.Run(name, func(t *testing.T) {
				res := sandbox.ComposeStreamResult(br, nil, []byte("partial\n"), nil)
				assert.False(t, res.OriginAuthenticated)
			})
		}
	})
}

// TestExecuteWithIDsSurfacesTheCLIObservation proves the carrier survives the
// REAL dispatch path, not just composeResult in isolation: a unit test over the
// composer alone would still pass if ExecuteWithIDs stopped returning its
// result verbatim.
func TestExecuteWithIDsSurfacesTheCLIObservation(t *testing.T) {
	c := newFakeClient(t)
	art := &fakeArtifact{contents: map[string]string{
		"mem://default/sess/uid/stderr": "gh: Bad credentials (HTTP 401)\n",
	}}

	reg := operations.New(nil, nil)
	op := reg.Begin("auth-failure carrier")

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-test-123",
		K8sClient:       c,
		ArtifactClient:  art,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
	}

	opts := defaultSandboxOpts()
	opts.Toolkit = &toolkit.Toolkit{Name: "gh"}
	st := sandbox.NewSandboxTool(opts)
	require.Equal(t, "toolkit/gh", st.Origin(),
		"precondition: the origin the recorder keys on comes from the toolkit name")

	ids := sandbox.IDs{TurnIndex: 0, ToolUseID: "tu-authfail"}
	patchToolCallStatus(t, c, client.ObjectKey{Namespace: "default", Name: "sess-0-tu-authfail"},
		terminalToolCall(spiceboxv1alpha1.ToolCallConditionFailed, exitPtr(4), "", "mem://default/sess/uid/stderr"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := st.ExecuteWithIDs(ctx, execArgs(t, op.ID), sess, ids)

	require.NoError(t, err)
	require.True(t, res.IsError)
	require.NotNil(t, res.ExitCode, "the exit code must survive ExecuteWithIDs, not just composeResult")
	assert.Equal(t, int32(4), *res.ExitCode)
	assert.Contains(t, res.Stderr, "Bad credentials")
}
