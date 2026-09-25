package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// fakeSPDY is a remotecommand.Executor stub whose StreamWithContext optionally
// writes to the caller's streams and then returns a fixed error (or nil),
// letting tests exercise the capture, transport-error and ExitStatus branches
// without an API server.
type fakeSPDY struct {
	streamErr error
	write     func(remotecommand.StreamOptions) // nil = write nothing
}

func (f fakeSPDY) Stream(o remotecommand.StreamOptions) error {
	return f.StreamWithContext(context.Background(), o)
}
func (f fakeSPDY) StreamWithContext(_ context.Context, o remotecommand.StreamOptions) error {
	if f.write != nil {
		f.write(o)
	}
	return f.streamErr
}

// exitStatusErr is a non-pointer error carrying an exit code, mirroring
// client-go's util/exec.CodeExitError duck-typed shape.
type exitStatusErr struct{ code int }

func (e exitStatusErr) Error() string   { return "command exited" }
func (e exitStatusErr) ExitStatus() int { return e.code }

// spdyCall records what a bound executor asked the kubelet for. The exec
// request's PodExecOptions (container, command argv, stdin/stdout/stderr) are
// encoded as query parameters on the URL, so capturing the URL is enough to
// assert on the whole request shape without an API server.
type spdyCall struct {
	url      *url.URL
	buildErr error // returned instead of an executor when non-nil
}

func (c *spdyCall) command() []string { return c.url.Query()["command"] }

// installSPDY swaps the package-level newSPDYExecutor seam for the duration of
// a test, recording the exec URL and returning the given fake behavior.
func installSPDY(t *testing.T, call *spdyCall, streamErr error, write func(remotecommand.StreamOptions)) {
	t.Helper()
	prev := newSPDYExecutor
	t.Cleanup(func() { newSPDYExecutor = prev })
	newSPDYExecutor = func(_ *rest.Config, _ string, u *url.URL) (remotecommand.Executor, error) {
		call.url = u
		if call.buildErr != nil {
			return nil, call.buildErr
		}
		return fakeSPDY{streamErr: streamErr, write: write}, nil
	}
}

// installFakeSPDY is the no-capture form used by the streaming tests.
func installFakeSPDY(t *testing.T, streamErr error) {
	t.Helper()
	installSPDY(t, &spdyCall{}, streamErr, nil)
}

func newTestExecutor(t *testing.T) *boundExecutor {
	t.Helper()
	// A real clientset built from a non-connecting config: RESTClient() only
	// builds the request URL here; the actual stream is served by the fake
	// SPDY executor installed via the newSPDYExecutor seam, so no API server
	// is contacted.
	cfg := &rest.Config{Host: "https://example.invalid"}
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "build clientset")
	b := &Binder{cfg: cfg, clientset: cs}
	return b.For("ns", "pod", "c").(*boundExecutor)
}

// A bound executor addresses exactly one target, and nothing in the Request can
// change that.
func TestBinder_ForProducesIndependentlyBoundExecutors(t *testing.T) {
	b, err := New(&rest.Config{Host: "https://demo.invalid"})
	require.NoError(t, err)

	one := b.For("demo-ns", "demo-pod-a", "sandbox")
	two := b.For("demo-ns", "demo-pod-b", "sandbox")

	require.NotNil(t, one)
	require.NotNil(t, two)
	assert.NotSame(t, one, two, "each For must yield its own bound executor")

	_, ok := one.(exec.StreamingExecutor)
	assert.True(t, ok, "the kubelet transport streams; a bound executor must keep that")
}

func TestStreamExecWaitSurfacesTransportError(t *testing.T) {
	transportErr := errors.New("spdy: connection reset by peer")
	cases := []struct {
		name         string
		streamErr    error
		wantExitCode int32
		wantErr      error // nil means "expect no error"
	}{
		{
			name:         "non-ExitStatus transport error: Wait returns ExitCode=-1 AND the cause",
			streamErr:    transportErr,
			wantExitCode: -1,
			wantErr:      transportErr,
		},
		{
			name:         "ExitStatus error: Wait returns the exit code, nil error",
			streamErr:    exitStatusErr{code: 7},
			wantExitCode: 7,
			wantErr:      nil,
		},
		{
			name:         "clean exit: Wait returns ExitCode=0, nil error",
			streamErr:    nil,
			wantExitCode: 0,
			wantErr:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeSPDY(t, tc.streamErr)
			e := newTestExecutor(t)

			st, err := e.StreamExec(context.Background(), exec.Request{
				Command: []string{"/bin/true"},
			})
			require.NoError(t, err, "StreamExec setup must succeed")
			t.Cleanup(func() { _ = st.Close() })

			res, waitErr := st.Wait()
			assert.Equal(t, tc.wantExitCode, res.ExitCode, "ExitCode")
			if tc.wantErr != nil {
				assert.ErrorIs(t, waitErr, tc.wantErr, "Wait must surface the transport cause")
			} else {
				assert.NoError(t, waitErr, "Wait error")
			}
		})
	}
}

// Exec is the SYNCHRONOUS path every non-streaming sandbox ToolCall takes
// (pkg/toolcall's controller drives it). The tests below cover the request it
// builds and each of the three ways it can finish.

func TestExec_RefusesAnEmptyCommand(t *testing.T) {
	call := &spdyCall{}
	installSPDY(t, call, nil, nil)

	res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{})

	require.Error(t, err, "an empty argv has no binary to run and must not reach the kubelet")
	assert.Equal(t, int32(-1), res.ExitCode, "a refused call never ran, so it has no exit status")
	assert.Nil(t, call.url, "the request must be refused before an exec stream is built")
}

func TestExec_SurfacesAnExecutorBuildFailure(t *testing.T) {
	buildErr := errors.New("no kubelet transport")
	call := &spdyCall{buildErr: buildErr}
	installSPDY(t, call, nil, nil)

	res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{Command: []string{"/bin/true"}})

	require.ErrorIs(t, err, buildErr, "the build failure must reach the caller, not be flattened to a bare exit code")
	assert.Equal(t, int32(-1), res.ExitCode)
}

// Env delivery must not disturb the caller's argv. This replaces an earlier
// test that asserted the opposite — that argv was rewritten to
// `env KEY=val … CMD` — which pinned the leak described in env.go as intended
// behaviour: it read the KEY=val pairs back out of the exec request's QUERY
// STRING and asserted they were there.
func TestExec_LeavesTheToolArgvIntactWhenDeliveringEnv(t *testing.T) {
	call := &spdyCall{}
	installSPDY(t, call, nil, nil)

	_, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
		Command: []string{"/usr/local/bin/tool", "--flag", "value"},
		// The value must be one no hex path can contain by chance: the tool's
		// argv carries the staged file's 128-bit hex nonce, and a short
		// lowercase-hex needle like "abc" turns this assertion into a ~1-in-150
		// flake rather than a claim about env delivery.
		Env: map[string]string{"TOKEN": theSecret, "MODE": "fast"},
	})
	require.NoError(t, err)

	// installSPDY records the LAST exec, which is the tool's own — env staging
	// ran first, on its own request.
	got := call.command()
	require.GreaterOrEqual(t, len(got), 3)
	assert.Equal(t, []string{"/usr/local/bin/tool", "--flag", "value"}, got[len(got)-3:],
		"the caller's argv must reach the tool unchanged and in order")
	for _, a := range got {
		assert.NotContains(t, a, "TOKEN=", "no key=value pair may appear in argv")
		assert.NotContains(t, a, theSecret, "no env value may appear in argv")
	}
}

func TestExec_NoEnvLeavesTheCommandUnwrapped(t *testing.T) {
	call := &spdyCall{}
	installSPDY(t, call, nil, nil)

	_, err := newTestExecutor(t).Exec(context.Background(), exec.Request{Command: []string{"/bin/echo", "hi"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/echo", "hi"}, call.command(),
		"with no Env there is nothing to wrap; `env` must not appear")
}

// The exec request must address the container the executor was BOUND to, and
// ask for stdin only when the caller supplied a source.
func TestExec_RequestTargetsTheBoundContainer(t *testing.T) {
	cases := []struct {
		name      string
		stdin     io.Reader
		wantStdin string
	}{
		// PodExecOptions marshals its booleans omitempty, so "not requested" is
		// an absent parameter rather than "false".
		{name: "no stdin source: stdin not requested, the tool sees a closed stdin", stdin: nil, wantStdin: ""},
		{name: "stdin source supplied: stdin=true", stdin: strings.NewReader("payload"), wantStdin: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := &spdyCall{}
			installSPDY(t, call, nil, nil)

			_, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
				Command: []string{"/bin/true"}, Stdin: tc.stdin,
			})
			require.NoError(t, err)

			q := call.url.Query()
			assert.Equal(t, "c", q.Get("container"), "a bound executor addresses exactly one container")
			assert.Contains(t, call.url.Path, "/namespaces/ns/pods/pod/exec", "and exactly one pod")
			assert.Equal(t, tc.wantStdin, q.Get("stdin"))
			assert.Equal(t, "true", q.Get("stdout"))
			assert.Equal(t, "true", q.Get("stderr"))
			assert.NotEqual(t, "true", q.Get("tty"), "a TTY would interleave stdout and stderr into one stream")
		})
	}
}

// The three terminal shapes of a completed exec. Output captured before the
// failure must survive in every one of them — a tool that printed a diagnostic
// and then died is the case an operator most needs to read.
func TestExec_TerminalOutcomes(t *testing.T) {
	transportErr := errors.New("spdy: connection reset by peer")
	write := func(o remotecommand.StreamOptions) {
		_, _ = o.Stdout.Write([]byte("out"))
		_, _ = o.Stderr.Write([]byte("err"))
	}
	cases := []struct {
		name         string
		streamErr    error
		wantExitCode int32
		wantErr      error
	}{
		{
			name:         "clean exit: ExitCode=0, no error, output captured",
			streamErr:    nil,
			wantExitCode: 0,
		},
		{
			name:         "non-zero exit: the duck-typed ExitStatus becomes ExitCode, NOT an error",
			streamErr:    exitStatusErr{code: 42},
			wantExitCode: 42,
		},
		{
			name:         "transport failure: ExitCode=-1 AND the cause is returned",
			streamErr:    transportErr,
			wantExitCode: -1,
			wantErr:      transportErr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installSPDY(t, &spdyCall{}, tc.streamErr, write)

			res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{Command: []string{"/bin/true"}})

			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr, "a stream failure with no exit status must surface its cause")
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.wantExitCode, res.ExitCode)
			assert.Equal(t, []byte("out"), res.Stdout, "captured stdout must survive every outcome")
			assert.Equal(t, []byte("err"), res.Stderr, "captured stderr must survive every outcome")
			assert.False(t, res.StdoutTruncated)
			assert.False(t, res.StderrTruncated)
		})
	}
}

// A tool that floods stdout must not be able to grow the runner's heap without
// bound; the cap is reported so the caller can say so rather than silently
// handing the model a half result.
func TestExec_CapsAndFlagsOversizedOutput(t *testing.T) {
	installSPDY(t, &spdyCall{}, nil, func(o remotecommand.StreamOptions) {
		_, _ = o.Stdout.Write(bytes.Repeat([]byte("a"), exec.MaxStreamBytes+1024))
		_, _ = o.Stderr.Write([]byte("small"))
	})

	res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{Command: []string{"/bin/yes"}})
	require.NoError(t, err)

	assert.Len(t, res.Stdout, exec.MaxStreamBytes, "capture must stop at the cap")
	assert.True(t, res.StdoutTruncated, "a truncated stream must say so; a silent cut looks like the tool's real output")
	assert.Equal(t, []byte("small"), res.Stderr)
	assert.False(t, res.StderrTruncated, "the streams are capped independently")
}

// boundedBuffer's own boundaries, which the Exec-level test cannot reach: the
// exact-limit write, the write that straddles the limit, and the writes after
// it. Every Write must report the FULL length it was given — a short count is
// an io.Writer contract violation that makes io.Copy abort with ErrShortWrite,
// which would turn a truncated stream into a hard exec failure.
func TestBoundedBuffer_Boundaries(t *testing.T) {
	cases := []struct {
		name          string
		limit         int
		writes        []string
		wantContent   string
		wantTruncated bool
	}{
		{name: "under the limit: kept whole, not flagged", limit: 8, writes: []string{"abc", "de"}, wantContent: "abcde"},
		{name: "exactly at the limit: kept whole, not flagged", limit: 5, writes: []string{"abcde"}, wantContent: "abcde"},
		{name: "one write straddles the limit: prefix kept, flagged", limit: 4, writes: []string{"abcdef"}, wantContent: "abcd", wantTruncated: true},
		{name: "writes after the limit: discarded, flagged", limit: 3, writes: []string{"abc", "def"}, wantContent: "abc", wantTruncated: true},
		{name: "zero limit: nothing kept, flagged on the first byte", limit: 0, writes: []string{"a"}, wantContent: "", wantTruncated: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &boundedBuffer{limit: tc.limit}
			for _, w := range tc.writes {
				n, err := b.Write([]byte(w))
				require.NoError(t, err)
				assert.Equal(t, len(w), n, "Write must report the full length it was handed, even when discarding")
			}
			assert.Equal(t, tc.wantContent, string(b.Bytes()))
			assert.Equal(t, tc.wantTruncated, b.truncated)
		})
	}
}
