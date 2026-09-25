package remote

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// theSecret is the credential value the leak tests below hunt for. It is
// deliberately URL-hostile (a `/` and a `+`) so a percent-encoded occurrence is
// caught as well as a literal one.
const theSecret = "ghp_liveCredential/Value+1234567890"

// urlRecorder captures EVERY exec URL an executor builds, not just the last.
// A credential-delivery mechanism that needs more than one round trip must be
// checked on all of them — asserting only on the final URL is how a leak in a
// staging call goes unnoticed.
type urlRecorder struct {
	urls   []*url.URL
	stdins []string
}

// leaks reports whether any recorded URL carries needle, in raw or
// percent-encoded form. Query-parameter values are percent-encoded on the way
// into RawQuery, so a naive Contains against the raw string alone would miss
// the very encoding the apiserver writes to its audit log; both forms are
// checked, plus the decoded parameter values.
func (r *urlRecorder) leaks(t *testing.T, needle string) (bool, string) {
	t.Helper()
	for _, u := range r.urls {
		if strings.Contains(u.String(), needle) || strings.Contains(u.String(), url.QueryEscape(needle)) {
			return true, u.String()
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				if strings.Contains(v, needle) {
					return true, u.String()
				}
			}
		}
	}
	return false, ""
}

// stdins holds what each recorded exec was handed on stdin, index-aligned with
// urls. The staged env script travels this way and nowhere else, so a test that
// wants to prove the values were delivered at all has to read it here.
func (r *urlRecorder) argv(i int) []string { return r.urls[i].Query()["command"] }

// installRecordingSPDY swaps the newSPDYExecutor seam for one that records
// every URL and the stdin of every exec, reporting a clean exit so a
// multi-exec delivery path runs to completion.
func installRecordingSPDY(t *testing.T) *urlRecorder {
	t.Helper()
	rec := &urlRecorder{}
	prev := newSPDYExecutor
	t.Cleanup(func() { newSPDYExecutor = prev })
	newSPDYExecutor = func(_ *rest.Config, _ string, u *url.URL) (remotecommand.Executor, error) {
		rec.urls = append(rec.urls, u)
		return fakeSPDY{write: func(o remotecommand.StreamOptions) {
			var got []byte
			if o.Stdin != nil {
				got, _ = io.ReadAll(o.Stdin)
			}
			rec.stdins = append(rec.stdins, string(got))
		}}, nil
	}
	return rec
}

// A resolved credential must never reach the exec request's URL. PodExecOptions
// are encoded as QUERY PARAMETERS (see spdyCall's doc comment), so anything in
// argv lands verbatim in the apiserver audit log's requestURI — and in any
// proxy access log in front of it. It is also visible in-container via
// /proc/<pid>/cmdline to every other tool sharing the sandbox.
func TestExec_NeverPutsEnvValuesInTheRequestURL(t *testing.T) {
	rec := installRecordingSPDY(t)

	_, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
		Command: []string{"/usr/local/bin/tool", "--flag"},
		Env:     map[string]string{"GH_TOKEN": theSecret, "MODE": "fast"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, rec.urls, "the exec must actually have been attempted")

	leaked, where := rec.leaks(t, theSecret)
	assert.False(t, leaked, "credential value reached the exec URL (apiserver audit log + /proc/*/cmdline): %s", where)

	// Absent from the URL is only half the claim — the values must still
	// REACH the tool. They ride the staging exec's stdin, which the kubelet
	// does not put in the URL.
	require.Len(t, rec.urls, 2, "env delivery stages the file, then runs the tool")
	require.Len(t, rec.stdins, 2)
	assert.Contains(t, rec.stdins[0], "export GH_TOKEN='"+theSecret+"'",
		"the credential must be delivered, over stdin, as a real exported env var")

	stageArgv, toolArgv := rec.argv(0), rec.argv(1)
	require.GreaterOrEqual(t, len(stageArgv), 5)
	path := stageArgv[len(stageArgv)-1]
	assert.Equal(t, []string{"sh", "-c", stageScript, "ap-stage-env"}, stageArgv[:4],
		"the staging exec's argv names only the destination path")
	assert.True(t, strings.HasPrefix(path, envFileDir+"/"+envFilePrefix), "staged path: %s", path)

	assert.Equal(t, []string{"sh", "-c", envLoaderScript, "ap-env", path, "/usr/local/bin/tool", "--flag"}, toolArgv,
		"the tool argv references the staged path and is otherwise unchanged")
}

// Two concurrent tool calls in one sandbox must not be able to read each
// other's staged file, so the path may never be a fixed name.
func TestStageEnvFile_UsesAFreshUnguessablePathPerCall(t *testing.T) {
	rec := installRecordingSPDY(t)
	e := newTestExecutor(t)

	for range 2 {
		_, err := e.Exec(context.Background(), exec.Request{
			Command: []string{"/bin/true"},
			Env:     map[string]string{"K": "v"},
		})
		require.NoError(t, err)
	}

	require.Len(t, rec.urls, 4, "two calls, each staging then running")
	first, second := rec.argv(0)[4], rec.argv(2)[4]
	assert.NotEqual(t, first, second, "each call gets its own env file")
	assert.Len(t, first, len(envFileDir)+1+len(envFilePrefix)+32, "128 bits of hex nonce")
}

// A staging failure must abort the call. Falling back to argv delivery on a
// transient error would make the leak intermittent — present only when the
// cluster is already unhealthy, and invisible in every test.
func TestExec_FailsClosedWhenEnvStagingFails(t *testing.T) {
	cases := []struct {
		name      string
		streamErr error
		wantMsg   string
	}{
		{
			name:      "staging transport error: the call is refused, not run with argv env",
			streamErr: errors.New("spdy: connection reset by peer"),
			wantMsg:   "stage env file",
		},
		{
			name:      "staging exits non-zero: the call is refused, not run with argv env",
			streamErr: exitStatusErr{code: 1},
			wantMsg:   "exit 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &urlRecorder{}
			prev := newSPDYExecutor
			t.Cleanup(func() { newSPDYExecutor = prev })
			newSPDYExecutor = func(_ *rest.Config, _ string, u *url.URL) (remotecommand.Executor, error) {
				rec.urls = append(rec.urls, u)
				return fakeSPDY{streamErr: tc.streamErr}, nil
			}

			res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
				Command: []string{"/usr/local/bin/tool"},
				Env:     map[string]string{"GH_TOKEN": theSecret},
			})

			require.Error(t, err, "a call whose credentials could not be staged must not run")
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.Equal(t, int32(-1), res.ExitCode)

			// The only exec after the failed staging attempt is the unlink.
			// `cat > "$1"` truncates the file into existence before the first
			// byte arrives, so a staging exec that started and then failed can
			// leave a partially written credential — and nothing else would
			// ever remove it, since the script's self-unlink only runs when the
			// file is sourced.
			require.Len(t, rec.urls, 2)
			for _, u := range rec.urls {
				assert.NotContains(t, u.Query()["command"], "/usr/local/bin/tool",
					"the tool must never be exec'd after staging failed")
			}
			assert.Equal(t, []string{"sh", "-c", unlinkScript, "ap-unstage-env", rec.argv(0)[4]}, rec.argv(1),
				"the staged path must be unlinked, not left readable to the next tool call in this sandbox")

			leaked, where := rec.leaks(t, theSecret)
			assert.False(t, leaked, "not even the failed staging attempt may carry the value in its URL: %s", where)
		})
	}
}

// A value the shell cannot express, or a name it cannot export, is an error —
// never a variable silently missing from the tool's environment, which reads
// downstream as an upstream auth outage.
func TestBuildEnvScript(t *testing.T) {
	const path = "/tmp/.ap-env-deadbeef"
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		want    []string // lines that must appear when no error is expected
	}{
		{
			name: "plain values: exported, sorted, after the self-unlink",
			env:  map[string]string{"B_VAR": "two", "A_VAR": "one"},
			want: []string{"rm -f -- '" + path + "'", "export A_VAR='one'", "export B_VAR='two'"},
		},
		{
			name: "embedded single quote: spliced as '\\'' so the word stays closed",
			env:  map[string]string{"V": "it's"},
			want: []string{`export V='it'\''s'`},
		},
		{
			name: "newlines, $ and backticks: single quotes suppress every expansion",
			env:  map[string]string{"PEM": "-----BEGIN KEY-----\n$(rm -rf /)\n`id`\n-----END KEY-----"},
			want: []string{"export PEM='-----BEGIN KEY-----\n$(rm -rf /)\n`id`\n-----END KEY-----'"},
		},
		{
			name:    "non-POSIX name: refused, because no shell can export it",
			env:     map[string]string{"NOT-A-NAME": "v"},
			wantErr: `env key "NOT-A-NAME" is not a POSIX name`,
		},
		{
			name:    "NUL in a value: refused, because execve would truncate it",
			env:     map[string]string{"V": "before\x00after"},
			wantErr: "contains a NUL byte",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildEnvScript(tc.env, path)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got, "a refused script must not be half-emitted")
				return
			}
			require.NoError(t, err)
			for _, line := range tc.want {
				assert.Contains(t, string(got), line)
			}
		})
	}
}

// scriptedSPDY records every exec a bound executor builds and can fail a chosen
// one, which is what lets a test drive the REAL Exec/StreamExec through the
// window between staging the env file and the tool reaching the loader that
// unlinks it. Indices are stable: 0 is the staging exec, 1 the tool's own (its
// URL is recorded even when building the executor then fails), 2+ are whatever
// the transport does to clean up.
//
// Mutex-guarded because the streaming path finishes its exec — and any cleanup
// — on StreamExec's own goroutine.
type scriptedSPDY struct {
	mu          sync.Mutex
	urls        []*url.URL
	buildErrAt  map[int]error
	streamErrAt map[int]error
}

func (s *scriptedSPDY) install(t *testing.T) *scriptedSPDY {
	t.Helper()
	prev := newSPDYExecutor
	t.Cleanup(func() { newSPDYExecutor = prev })
	newSPDYExecutor = func(_ *rest.Config, _ string, u *url.URL) (remotecommand.Executor, error) {
		s.mu.Lock()
		i := len(s.urls)
		s.urls = append(s.urls, u)
		buildErr, streamErr := s.buildErrAt[i], s.streamErrAt[i]
		s.mu.Unlock()
		if buildErr != nil {
			return nil, buildErr
		}
		return fakeSPDY{streamErr: streamErr}, nil
	}
	return s
}

// argvs returns the argv of every exec recorded so far, in order.
func (s *scriptedSPDY) argvs() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, 0, len(s.urls))
	for _, u := range s.urls {
		out = append(out, u.Query()["command"])
	}
	return out
}

// stagedPath is the destination the staging exec was pointed at — the last word
// of its argv.
func (s *scriptedSPDY) stagedPath(t *testing.T) string {
	t.Helper()
	argvs := s.argvs()
	require.NotEmpty(t, argvs, "no exec was recorded at all")
	require.Equal(t, stageScript, argvs[0][2], "exec 0 must be the env staging exec")
	return argvs[0][len(argvs[0])-1]
}

// cleanupArgvs is everything the executor ran beyond the staging exec and the
// tool's own. Matching by position rather than by a helper's identifier keeps
// these tests a test of the DEFECT — an orphaned credential file — rather than
// of the fix's internals.
func (s *scriptedSPDY) cleanupArgvs() [][]string {
	argvs := s.argvs()
	if len(argvs) <= 2 {
		return nil
	}
	return argvs[2:]
}

// requireUnlinked asserts the executor issued exactly one extra exec and that it
// removes path. The file is otherwise readable, as the same UID that every later
// tool call in the sandbox runs as, for the whole life of the pod.
func requireUnlinked(t *testing.T, s *scriptedSPDY, path string) {
	t.Helper()
	cleanups := s.cleanupArgvs()
	require.Len(t, cleanups, 1,
		"the staged credential file must be unlinked when the tool exec never reached the loader; execs issued: %v", s.argvs())
	joined := strings.Join(cleanups[0], " ")
	assert.Contains(t, joined, path, "the cleanup exec must name the staged path")
	assert.Contains(t, joined, "rm ", "the cleanup exec must unlink the staged file")
}

// Every failure between staging the env file and the tool reaching the loader
// orphans a plaintext credential in the sandbox's /tmp — a memory emptyDir that
// lives for the POD's whole lifetime, not the tool's. Every subsequent exec into
// that container runs as the same UID, so `umask 077` is no barrier and the file
// is `cat /tmp/.ap-env-*`-readable by every later tool call in the session: the
// exact cross-tool capability leak this delivery mechanism exists to close,
// unbounded in time instead of bounded by one process's lifetime.
func TestExec_UnlinksTheStagedEnvFileWhenTheToolNeverReachesTheLoader(t *testing.T) {
	cases := []struct {
		name string
		spdy *scriptedSPDY
	}{
		{
			name: "transport drop on the tool exec: the staged file is removed, not left for the next tool call",
			spdy: &scriptedSPDY{streamErrAt: map[int]error{1: errors.New("spdy: connection reset by peer")}},
		},
		{
			name: "executor build failure: the staged file is removed, not left for the next tool call",
			spdy: &scriptedSPDY{buildErrAt: map[int]error{1: errors.New("no kubelet transport")}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.spdy.install(t)

			res, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
				Command: []string{"/usr/local/bin/tool"},
				Env:     map[string]string{"GH_TOKEN": theSecret},
			})
			require.Error(t, err, "the tool exec failed, so the call must report it")
			assert.Equal(t, int32(-1), res.ExitCode)

			requireUnlinked(t, tc.spdy, tc.spdy.stagedPath(t))
		})
	}
}

// The streaming path stages the identical credential file for stream-mode and
// interactive ToolCalls, and its exec finishes on a goroutine the caller never
// sees. A cleanup that covers only the synchronous path leaves every interactive
// tool call orphaning credentials.
func TestStreamExec_UnlinksTheStagedEnvFileWhenTheToolNeverReachesTheLoader(t *testing.T) {
	cases := []struct {
		name string
		spdy *scriptedSPDY
	}{
		{
			name: "transport drop on the tool exec: the staged file is removed after Wait reports the cause",
			spdy: &scriptedSPDY{streamErrAt: map[int]error{1: errors.New("spdy: connection reset by peer")}},
		},
		{
			name: "executor build failure: the staged file is removed before StreamExec returns",
			spdy: &scriptedSPDY{buildErrAt: map[int]error{1: errors.New("no kubelet transport")}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.spdy.install(t)

			st, err := newTestExecutor(t).StreamExec(context.Background(), exec.Request{
				Command: []string{"/usr/local/bin/tool"},
				Env:     map[string]string{"GH_TOKEN": theSecret},
			})
			if err == nil {
				t.Cleanup(func() { _ = st.Close() })
				res, waitErr := st.Wait()
				require.Error(t, waitErr, "a transport drop must surface through Wait")
				assert.Equal(t, int32(-1), res.ExitCode)
			}

			path := tc.spdy.stagedPath(t)
			// The unlink is issued off the stream goroutine, deliberately after
			// the exit outcome is published so Wait is never delayed by it.
			require.Eventually(t, func() bool { return len(tc.spdy.cleanupArgvs()) > 0 }, 5*time.Second, 10*time.Millisecond,
				"the staged credential file must be unlinked when the tool exec never reached the loader; execs issued: %v", tc.spdy.argvs())
			requireUnlinked(t, tc.spdy, path)
		})
	}
}

// The loader IS the tool's argv, so a remote process that reported any exit
// status already ran `rm -f` on the staged file. Re-unlinking it would be a
// wasted apiserver round trip per tool call, and — worse — a second unlink is
// the shape that could one day race a loader that has not yet opened the file.
func TestExec_DoesNotUnlinkAgainWhenTheToolRan(t *testing.T) {
	cases := []struct {
		name string
		spdy *scriptedSPDY
	}{
		{
			name: "clean exit: the loader unlinked the file, so no cleanup exec is issued",
			spdy: &scriptedSPDY{},
		},
		{
			name: "non-zero exit status: the loader still ran, so no cleanup exec is issued",
			spdy: &scriptedSPDY{streamErrAt: map[int]error{1: exitStatusErr{code: 3}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.spdy.install(t)

			_, err := newTestExecutor(t).Exec(context.Background(), exec.Request{
				Command: []string{"/usr/local/bin/tool"},
				Env:     map[string]string{"GH_TOKEN": theSecret},
			})
			require.NoError(t, err, "an exit status is not a transport error")

			assert.Empty(t, tc.spdy.cleanupArgvs(),
				"a tool that ran already unlinked its own env file; execs issued: %v", tc.spdy.argvs())
		})
	}
}

// The streaming path carries the same credentials for stream-mode and
// interactive ToolCalls. A fix that covers only Exec leaves every interactive
// tool call leaking.
func TestStreamExec_NeverPutsEnvValuesInTheRequestURL(t *testing.T) {
	rec := installRecordingSPDY(t)

	st, err := newTestExecutor(t).StreamExec(context.Background(), exec.Request{
		Command: []string{"/usr/local/bin/tool", "--flag"},
		Env:     map[string]string{"GH_TOKEN": theSecret, "MODE": "fast"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NotEmpty(t, rec.urls, "the exec must actually have been attempted")

	leaked, where := rec.leaks(t, theSecret)
	assert.False(t, leaked, "credential value reached the exec URL (apiserver audit log + /proc/*/cmdline): %s", where)

	require.Len(t, rec.urls, 2, "the streaming path stages its env file the same way Exec does")
	require.NotEmpty(t, rec.stdins)
	assert.Contains(t, rec.stdins[0], "export GH_TOKEN='"+theSecret+"'",
		"the streaming path must deliver the credential too, not merely omit it")
	assert.Equal(t,
		[]string{"sh", "-c", envLoaderScript, "ap-env", rec.argv(0)[4], "/usr/local/bin/tool", "--flag"},
		rec.argv(1), "identical loader shape to Exec — one delivery mechanism, not two")
}
