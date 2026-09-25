package fake_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
)

// mustStreamingExecutor asserts e implements exec.StreamingExecutor, the way
// the interactive bridge (pkg/controllers/toolcall/stream.go) type-asserts a
// bound executor before calling StreamExec.
func mustStreamingExecutor(t *testing.T, e exec.Executor) exec.StreamingExecutor {
	t.Helper()
	se, ok := e.(exec.StreamingExecutor)
	require.True(t, ok, "fake bound executor must implement exec.StreamingExecutor")
	return se
}

func TestFake_ReturnsProgrammedResult(t *testing.T) {
	b := fake.New()
	b.Program("ns/pod:sandbox", fake.Response{
		Stdout:   []byte("hello\n"),
		Stderr:   []byte(""),
		ExitCode: 0,
	})

	e := b.For("ns", "pod", "sandbox")
	res, err := e.Exec(context.Background(), exec.Request{
		Command: []string{"/bin/echo", "hello"},
	})
	require.NoError(t, err, "Exec")
	assert.Equal(t, "hello\n", string(res.Stdout), "stdout")
	assert.EqualValues(t, 0, res.ExitCode, "exit code")
}

func TestFake_CapturesStdin(t *testing.T) {
	b := fake.New()
	b.Program("ns/pod:sandbox", fake.Response{ExitCode: 0})
	e := b.For("ns", "pod", "sandbox")
	_, err := e.Exec(context.Background(), exec.Request{
		Command: []string{"/bin/cat"},
		Stdin:   strings.NewReader("input bytes"),
	})
	require.NoError(t, err, "Exec")
	calls := b.Calls()
	require.Len(t, calls, 1, "exactly one recorded call")
	assert.Equal(t, []byte("input bytes"), calls[0].Stdin, "captured stdin")
}

func TestFake_RespectsContextCancel(t *testing.T) {
	b := fake.New()
	b.Program("ns/pod:sandbox", fake.Response{
		Delay:    200 * time.Millisecond,
		ExitCode: 0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	e := b.For("ns", "pod", "sandbox")
	_, err := e.Exec(ctx, exec.Request{
		Command: []string{"/bin/sleep", "1"},
	})
	require.Error(t, err, "context cancel must error")
}

func TestFake_UnprogrammedKeyErrors(t *testing.T) {
	b := fake.New()
	e := b.For("x", "y", "z")
	_, err := e.Exec(context.Background(), exec.Request{
		Command: []string{"anything"},
	})
	require.Error(t, err, "unprogrammed key must error")
}

func TestFake_StreamExecEchoesStdin(t *testing.T) {
	b := fake.New()
	captured := make(chan []byte, 1)
	b.ProgramStream("ns/pod:sandbox", fake.StreamResponse{
		Stdout:   []byte("hello\n"),
		ExitCode: 0,
		OnStdin:  func(got []byte) { captured <- got },
	})

	e := mustStreamingExecutor(t, b.For("ns", "pod", "sandbox"))
	s, err := e.StreamExec(context.Background(), exec.Request{
		Command: []string{"/bin/cat"},
	})
	require.NoError(t, err, "StreamExec")
	defer s.Close()

	_, err = s.Stdin.Write([]byte("payload"))
	require.NoError(t, err, "Stdin.Write")
	require.NoError(t, s.Stdin.Close(), "Stdin.Close")

	buf, err := io.ReadAll(s.Stdout)
	require.NoError(t, err, "ReadAll stdout")
	assert.Equal(t, "hello\n", string(buf), "stdout")

	res, err := s.Wait()
	require.NoError(t, err, "Wait")
	assert.EqualValues(t, 0, res.ExitCode, "exit code")

	select {
	case got := <-captured:
		assert.Equal(t, "payload", string(got), "OnStdin captured payload")
	case <-time.After(time.Second):
		t.Fatalf("OnStdin never called")
	}
}

// TestStreamExec_FixedStdinReaderEOFsImmediately asserts that when
// Request.Stdin is a non-nil finite reader, StreamExec feeds those bytes to
// the tool and the tool sees stdin at EOF without the caller writing to (or
// closing) Stream.Stdin. This is the path stream-mode ToolCalls use so a
// one-shot tool like `claude --print` does not stall on an open never-written
// pipe.
func TestStreamExec_FixedStdinReaderEOFsImmediately(t *testing.T) {
	b := fake.New()
	captured := make(chan []byte, 1)
	b.ProgramStream("ns/pod:sandbox", fake.StreamResponse{
		Stdout:   []byte("ok\n"),
		ExitCode: 0,
		OnStdin:  func(got []byte) { captured <- got },
	})

	e := mustStreamingExecutor(t, b.For("ns", "pod", "sandbox"))
	s, err := e.StreamExec(context.Background(), exec.Request{
		Command: []string{"/bin/cat"},
		Stdin:   strings.NewReader("fixed-input"),
	})
	require.NoError(t, err, "StreamExec")
	defer s.Close()

	// The caller does NOT write to or close Stream.Stdin — the fixed reader
	// already supplied stdin and reached EOF on its own.
	buf, err := io.ReadAll(s.Stdout)
	require.NoError(t, err, "ReadAll stdout")
	assert.Equal(t, "ok\n", string(buf), "stdout")

	res, err := s.Wait()
	require.NoError(t, err, "Wait")
	assert.EqualValues(t, 0, res.ExitCode, "exit code")

	select {
	case got := <-captured:
		assert.Equal(t, "fixed-input", string(got), "tool received the fixed stdin and saw EOF")
	case <-time.After(time.Second):
		t.Fatalf("OnStdin never called — fixed stdin reader did not reach the tool / EOF")
	}
}

func TestProgramStreamFunc_BackAndForth(t *testing.T) {
	b := fake.New()
	// Driver: echo each line of stdin back as stdout; exit 0 on EOF.
	b.ProgramStreamFunc("default/p:c", func(stdin io.Reader, stdout, stderr io.Writer) int32 {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			_, _ = io.WriteString(stdout, "echo: "+sc.Text()+"\n")
		}
		return 0
	})

	e := mustStreamingExecutor(t, b.For("default", "p", "c"))
	stream, err := e.StreamExec(t.Context(), exec.Request{})
	require.NoError(t, err)
	defer stream.Close()

	// Feed two lines and close stdin.
	go func() {
		_, _ = io.WriteString(stream.Stdin, "hello\nworld\n")
		_ = stream.Stdin.Close()
	}()

	// Read stdout to EOF — the driver closes stdout when it returns.
	got, rerr := io.ReadAll(stream.Stdout)
	require.NoError(t, rerr, "ReadAll stdout")

	res, werr := stream.Wait()
	require.NoError(t, werr, "Wait")
	assert.Equal(t, int32(0), res.ExitCode, "exit code")
	assert.Equal(t, "echo: hello\necho: world\n", string(got), "stdout echo")
}

// A response scripted against a composed "ns/pod:container" key must be found
// by an executor bound to that same target. This is what keeps every existing
// Program(...) registration working after Request lost its identity fields.
func TestBinder_BoundExecutorFindsScriptedResponse(t *testing.T) {
	b := fake.New()
	b.Program("demo-ns/demo-pod:sandbox", fake.Response{Stdout: []byte("hello")})

	e := b.For("demo-ns", "demo-pod", "sandbox")
	res, err := e.Exec(context.Background(), exec.Request{Command: []string{"echo", "hello"}})
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), res.Stdout)
}

// An executor bound to a different target must not pick up another's script.
func TestBinder_BoundExecutorDoesNotSeeAnotherTargetsScript(t *testing.T) {
	b := fake.New()
	b.Program("demo-ns/demo-pod-a:sandbox", fake.Response{Stdout: []byte("a")})
	b.Program("demo-ns/demo-pod-b:sandbox", fake.Response{Stdout: []byte("b")})

	e := b.For("demo-ns", "demo-pod-b", "sandbox")
	res, err := e.Exec(context.Background(), exec.Request{Command: []string{"true"}})
	require.NoError(t, err)
	assert.NotEqual(t, []byte("a"), res.Stdout, "a bound executor must not read another target's script")
}

// Recorded calls must still say which target they ran against. Request no
// longer carries it, so Call records the binding instead — without this, every
// "this command ran in that pod" assertion in the e2e suite loses its meaning.
func TestBinder_RecordsTargetOnCalls(t *testing.T) {
	b := fake.New()
	b.Program("demo-ns/demo-pod:sandbox", fake.Response{})
	e := b.For("demo-ns", "demo-pod", "sandbox")

	_, err := e.Exec(context.Background(), exec.Request{Command: []string{"echo", "hi"}})
	require.NoError(t, err)

	calls := b.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "demo-ns", calls[0].Namespace)
	assert.Equal(t, "demo-pod", calls[0].Pod)
	assert.Equal(t, "sandbox", calls[0].Container)
	assert.Equal(t, []string{"echo", "hi"}, calls[0].Request.Command)
}

// ---- ProgramFunc: per-COMMAND dispatch at one pod key -------------------
//
// A key names a POD, and every tool a SpiceboxClass declares runs in that one
// pod. These tests pin the thing Program cannot express: two commands at the
// SAME key answering differently.

var (
	errNoCommand      = errors.New("fake responder: empty command")
	errUnknownCommand = errors.New("fake responder: no recorded output for this command")
)

// programFuncBinder returns an executor bound to one key whose responder
// answers per argv[0]. It mirrors what the threadrun driver installs: a lookup
// keyed by the class tool's command, and an explicit failure for anything not
// in the catalog.
func programFuncBinder(t *testing.T, byArgv0 map[string]fake.Response) exec.Executor {
	t.Helper()
	b := fake.New()
	b.ProgramFunc("demo-ns/demo-pod:sandbox", func(req exec.Request) fake.Response {
		if len(req.Command) == 0 {
			return fake.Response{ExitCode: -1, Err: errNoCommand}
		}
		resp, ok := byArgv0[req.Command[0]]
		if !ok {
			return fake.Response{ExitCode: -1, Err: errUnknownCommand}
		}
		return resp
	})
	return b.For("demo-ns", "demo-pod", "sandbox")
}

func TestProgramFunc_AnswersPerCommandAtOneKey(t *testing.T) {
	e := programFuncBinder(t, map[string]fake.Response{
		"/usr/local/bin/sre-tool": {Stdout: []byte("cluster-a\ncluster-b\n")},
		"/usr/bin/gh":             {Stdout: []byte("no open pull requests\n")},
	})

	cases := []struct {
		name       string
		command    []string
		wantStdout string
	}{
		{
			name:       "first tool's argv0: its own recorded stdout, not the other's",
			command:    []string{"/usr/local/bin/sre-tool", "list-clusters"},
			wantStdout: "cluster-a\ncluster-b\n",
		},
		{
			name:       "second tool's argv0 at the SAME key: the other recorded stdout",
			command:    []string{"/usr/bin/gh", "pr", "view"},
			wantStdout: "no open pull requests\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Exec(context.Background(), exec.Request{Command: tc.command})
			require.NoError(t, err, "Exec")
			assert.Equal(t, tc.wantStdout, string(res.Stdout), "stdout for %v", tc.command)
		})
	}
}

// An unrecognized command must FAIL loudly. A responder that returned a zero
// Response instead would replay as a tool that ran fine and printed nothing —
// the silent-divergence shape the capture path exists to avoid.
func TestProgramFunc_UnrecognizedCommandErrorsRatherThanReturningEmpty(t *testing.T) {
	e := programFuncBinder(t, map[string]fake.Response{"/usr/bin/gh": {Stdout: []byte("x")}})

	res, err := e.Exec(context.Background(), exec.Request{Command: []string{"/bin/nope"}})
	require.Error(t, err, "an unrecorded command must not succeed")
	assert.ErrorIs(t, err, errUnknownCommand, "the responder's own error must reach the caller")
	assert.EqualValues(t, -1, res.ExitCode, "transport-error exit code")
}

// The responder sees the WHOLE request, not just argv[0] — the toolcall
// controller appends the tool's DefaultArgs and the call's own args, and a
// responder narrowing on a subcommand needs them.
func TestProgramFunc_ResponderSeesTheFullRequest(t *testing.T) {
	b := fake.New()
	var got exec.Request
	b.ProgramFunc("demo-ns/demo-pod:sandbox", func(req exec.Request) fake.Response {
		got = req
		return fake.Response{}
	})
	e := b.For("demo-ns", "demo-pod", "sandbox")

	_, err := e.Exec(context.Background(), exec.Request{
		Command: []string{"/usr/bin/gh", "pr", "view", "123"},
		Env:     map[string]string{"GH_TOKEN": "t"},
	})
	require.NoError(t, err, "Exec")
	assert.Equal(t, []string{"/usr/bin/gh", "pr", "view", "123"}, got.Command, "full argv")
	assert.Equal(t, map[string]string{"GH_TOKEN": "t"}, got.Env, "env")
}

// Precedence mirrors ProgramStreamFunc over ProgramStream: registering both is
// a caller deciding the func is the answer, never a merge.
func TestProgramFunc_WinsOverProgramForTheSameKey(t *testing.T) {
	b := fake.New()
	b.Program("demo-ns/demo-pod:sandbox", fake.Response{Stdout: []byte("static")})
	b.ProgramFunc("demo-ns/demo-pod:sandbox", func(exec.Request) fake.Response {
		return fake.Response{Stdout: []byte("from func")}
	})

	res, err := b.For("demo-ns", "demo-pod", "sandbox").Exec(
		context.Background(), exec.Request{Command: []string{"x"}})
	require.NoError(t, err, "Exec")
	assert.Equal(t, "from func", string(res.Stdout), "ProgramFunc must win over Program")
}

// A func registered for one key must not answer another's calls — the same
// isolation Program has, and the reason a per-pod key exists at all.
func TestProgramFunc_DoesNotLeakAcrossKeys(t *testing.T) {
	b := fake.New()
	b.ProgramFunc("demo-ns/demo-pod-a:sandbox", func(exec.Request) fake.Response {
		return fake.Response{Stdout: []byte("a")}
	})

	_, err := b.For("demo-ns", "demo-pod-b", "sandbox").Exec(
		context.Background(), exec.Request{Command: []string{"x"}})
	require.Error(t, err, "pod-b has no program of its own")
	assert.Contains(t, err.Error(), "demo-ns/demo-pod-b:sandbox", "the error names the unprogrammed key")
}

// Program keeps working untouched at a key with no func: ProgramFunc is
// additive, and every existing scenario registration must survive it.
func TestProgramFunc_LeavesPlainProgramKeysAlone(t *testing.T) {
	b := fake.New()
	b.Program("demo-ns/plain-pod:sandbox", fake.Response{Stdout: []byte("static")})
	b.ProgramFunc("demo-ns/func-pod:sandbox", func(exec.Request) fake.Response {
		return fake.Response{Stdout: []byte("dynamic")}
	})

	res, err := b.For("demo-ns", "plain-pod", "sandbox").Exec(
		context.Background(), exec.Request{Command: []string{"x"}})
	require.NoError(t, err, "Exec against the plain key")
	assert.Equal(t, "static", string(res.Stdout), "a key with no func still reads its static Response")
}

// The responder must be able to call back into the Binder. It runs outside the
// binder lock precisely so a caller inspecting Calls (or programming a further
// key) from inside it does not deadlock the test it was meant to drive.
func TestProgramFunc_ResponderMayReenterTheBinder(t *testing.T) {
	b := fake.New()
	var seen int
	b.ProgramFunc("demo-ns/demo-pod:sandbox", func(exec.Request) fake.Response {
		seen = len(b.Calls()) // re-enters Binder.mu
		return fake.Response{}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := b.For("demo-ns", "demo-pod", "sandbox").Exec(
			context.Background(), exec.Request{Command: []string{"x"}})
		assert.NoError(t, err, "Exec")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Exec deadlocked: the responder must not run while Binder.mu is held")
	}
	assert.Equal(t, 1, seen, "the call is recorded before the responder runs")
}
