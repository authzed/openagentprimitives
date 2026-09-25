package zedctx_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/zedctx"
)

type capturedCall struct {
	Path string
	Args []string
}

type runReturn struct {
	out []byte
	err error
}

type recorder struct {
	calls   []capturedCall
	returns []runReturn
	idx     int
}

func (r *recorder) fn(cmd *exec.Cmd) ([]byte, error) {
	r.calls = append(r.calls, capturedCall{Path: cmd.Path, Args: append([]string{}, cmd.Args...)})
	if r.idx >= len(r.returns) {
		return nil, nil
	}
	ret := r.returns[r.idx]
	r.idx++
	return ret.out, ret.err
}

func newRecorder(returns ...runReturn) *recorder {
	return &recorder{returns: returns}
}

func TestRunnerArgv(t *testing.T) {
	cases := []struct {
		name     string
		do       func(t *testing.T, r *zedctx.Runner)
		wantArgs []string // expected cmd.Args (argv) of the (single) call
	}{
		{
			name: "Set: insecure / argv has --insecure flag",
			do: func(t *testing.T, r *zedctx.Runner) {
				require.NoError(t, r.Set(context.Background(), "agentprimitives", "127.0.0.1:60061", "tok", true))
			},
			wantArgs: []string{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "tok", "--insecure"},
		},
		{
			name: "Set: secure / argv lacks --insecure",
			do: func(t *testing.T, r *zedctx.Runner) {
				require.NoError(t, r.Set(context.Background(), "prod", "grpc.x:443", "tok", false))
			},
			wantArgs: []string{"zed", "context", "set", "prod", "grpc.x:443", "tok"},
		},
		{
			name: "Use: argv is context use <name>",
			do: func(t *testing.T, r *zedctx.Runner) {
				require.NoError(t, r.Use(context.Background(), "agentprimitives"))
			},
			wantArgs: []string{"zed", "context", "use", "agentprimitives"},
		},
		{
			name: "Remove: argv is context remove <name>",
			do: func(t *testing.T, r *zedctx.Runner) {
				require.NoError(t, r.Remove(context.Background(), "agentprimitives"))
			},
			wantArgs: []string{"zed", "context", "remove", "agentprimitives"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			r := zedctx.NewRunnerForTest("zed", rec.fn)
			tc.do(t, r)
			require.Len(t, rec.calls, 1, "expected exactly one zed invocation")
			assert.Equal(t, tc.wantArgs, rec.calls[0].Args, "argv mismatch")
		})
	}
}

func TestRunnerErrorPropagation(t *testing.T) {
	rec := newRecorder(runReturn{out: []byte("boom\n"), err: exec.ErrNotFound})
	r := zedctx.NewRunnerForTest("zed", rec.fn)
	err := r.Set(context.Background(), "n", "e", "t", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom", "stderr/stdout should be surfaced in the error")
	assert.Len(t, rec.calls, 1)
}

// TestSetErrorDoesNotLeakToken pins that a failing `zed context set` never
// puts the SpiceDB pre-shared key into its error message.
//
// The token Set receives is the live cluster PSK: `oap spicedb proxy` reads it
// from the spicebox-spicedb-token Secret and passes it straight in. It is the
// unscoped root credential for the whole authorization system, so an error
// that interpolates it lands the PSK in terminal scrollback, CI logs, and
// pasted bug reports. The error must still be diagnosable — it names the
// subcommand and carries the tool's own output — just never the secret.
func TestSetErrorDoesNotLeakToken(t *testing.T) {
	const psk = "sdbtok_ultrasecret_root_credential_value"
	cases := []struct {
		name     string
		insecure bool
	}{
		{name: "insecure: PSK absent from the error", insecure: true},
		{name: "secure: PSK absent from the error", insecure: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(runReturn{out: []byte("connection refused\n"), err: exec.ErrNotFound})
			r := zedctx.NewRunnerForTest("zed", rec.fn)

			err := r.Set(context.Background(), "agentprimitives", "127.0.0.1:60061", psk, tc.insecure)
			require.Error(t, err, "a failing run must still report an error")

			assert.NotContains(t, err.Error(), psk,
				"the SpiceDB pre-shared key must never appear in an error string")
			assert.Contains(t, err.Error(), "context set",
				"the error must still name the subcommand that failed")
			assert.Contains(t, err.Error(), "connection refused",
				"the error must still surface the tool's own output")

			// The redaction is presentational only: the argv actually handed to
			// zed must still carry the real token, or the command would break.
			require.Len(t, rec.calls, 1)
			assert.Contains(t, rec.calls[0].Args, psk,
				"the executed argv must still contain the real token")
		})
	}
}
