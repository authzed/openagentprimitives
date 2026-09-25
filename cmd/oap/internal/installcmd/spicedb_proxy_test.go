package installcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/zedctx"
)

// fakeForwarder satisfies the proxyForwarder interface used by runSpiceDBProxy.
// doneCh defaults to nil, which is never selectable — only test cases that
// want to simulate mid-session stream death prime it (via primedDoneCh).
type fakeForwarder struct {
	startErr  error
	startedOn uint16
	stopped   bool
	doneCh    chan error
}

func (f *fakeForwarder) Start(ctx context.Context, out io.Writer) error { return f.startErr }
func (f *fakeForwarder) LocalPort() uint16                              { return f.startedOn }
func (f *fakeForwarder) Done() <-chan error                             { return f.doneCh }
func (f *fakeForwarder) Stop()                                          { f.stopped = true }

// primedDoneCh returns a buffered channel preloaded with err. Used by the
// stream-death test case so the orchestrator's select picks Done() immediately.
func primedDoneCh(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

// queuedReturn is a single (stdout/stderr, error) pair to hand back to the
// next zed invocation in order.
type queuedReturn struct {
	out []byte
	err error
}

// recorder is a zedctx runFn that captures argv per call and returns queued
// (out, err) pairs in order. Mirrors the recorder pattern in
// cmd/oap/internal/zedctx/zedctx_test.go.
type recorder struct {
	calls   [][]string
	returns []queuedReturn
	idx     int
}

func (r *recorder) fn(cmd *exec.Cmd) ([]byte, error) {
	r.calls = append(r.calls, append([]string{}, cmd.Args...))
	if r.idx >= len(r.returns) {
		return nil, nil
	}
	ret := r.returns[r.idx]
	r.idx++
	return ret.out, ret.err
}

func newRecorder(returns ...queuedReturn) *recorder {
	return &recorder{returns: returns}
}

func newTokenSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-token", Namespace: "agentprimitives-system"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

// unavailableRunner implements proxyZedRunner but reports Available()==false
// and records nothing. Used by the "zed not on PATH" subtest.
type unavailableRunner struct{}

func (unavailableRunner) Available() bool                                         { return false }
func (unavailableRunner) Set(context.Context, string, string, string, bool) error { return nil }
func (unavailableRunner) Use(context.Context, string) error                       { return nil }
func (unavailableRunner) Remove(context.Context, string) error                    { return nil }

func TestRunSpiceDBProxy(t *testing.T) {
	cases := []struct {
		name              string
		secrets           []runtime.Object
		forwarder         *fakeForwarder
		zedReturns        []queuedReturn
		zedRunnerOverride proxyZedRunner // non-nil → ignore zedReturns and use this
		signalAfter       time.Duration  // 0 → cancel ctx immediately
		wantErrSubstr     string
		wantZedArgvs      [][]string // expected zed argvs in order (nil → no calls expected)
		wantOutSubstrs    []string   // substrings expected in stdout (nil → no stdout assertions)
		wantOutAbsent     []string   // substrings that must NOT appear in stdout (secret material)
		wantStopped       bool
	}{
		{
			name:        "happy path: set + use on start, remove on signal",
			secrets:     []runtime.Object{newTokenSecret("supersecret")},
			forwarder:   &fakeForwarder{startedOn: 60061},
			zedReturns:  []queuedReturn{{}, {}, {}}, // set, use, remove
			signalAfter: 5 * time.Millisecond,
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "supersecret", "--insecure"},
				{"zed", "context", "use", "agentprimitives"},
				{"zed", "context", "remove", "agentprimitives"},
			},
			wantStopped: true,
		},
		{
			name:          "token secret missing: returns 'read spicedb token secret' error",
			secrets:       nil,
			forwarder:     &fakeForwarder{startedOn: 60061},
			wantErrSubstr: "read spicedb token secret",
		},
		{
			name:          "token secret has empty token key: returns 'empty token' error",
			secrets:       []runtime.Object{newTokenSecret("")},
			forwarder:     &fakeForwarder{startedOn: 60061},
			wantErrSubstr: "empty token",
		},
		{
			name:          "port-forward start error propagates",
			secrets:       []runtime.Object{newTokenSecret("tok")},
			forwarder:     &fakeForwarder{startErr: errors.New("bind: addr in use")},
			wantErrSubstr: "start port-forward",
		},
		{
			// The failure message travels up through runSpiceDBProxy to the
			// user's stderr, so it must not carry the PSK that was passed to
			// `zed context set`. The executed argv still holds the real token.
			name:          "zed context set fails: error wrapped, no use call, PSK absent from the error",
			secrets:       []runtime.Object{newTokenSecret("sdbtok_live_root_credential_wxyz")},
			forwarder:     &fakeForwarder{startedOn: 60061},
			zedReturns:    []queuedReturn{{out: []byte("kaboom"), err: errors.New("exit 1")}},
			wantErrSubstr: "zed context set",
			wantStopped:   true,
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "sdbtok_live_root_credential_wxyz", "--insecure"},
			},
			wantOutAbsent: []string{"sdbtok_live_root_credential_wxyz"},
		},
		{
			name:      "zed context use fails after set: cleanup remove invoked, error propagates",
			secrets:   []runtime.Object{newTokenSecret("tok")},
			forwarder: &fakeForwarder{startedOn: 60061},
			zedReturns: []queuedReturn{
				{}, // set: ok
				{out: []byte("use kaboom"), err: errors.New("exit 1")}, // use: fail
				{}, // remove cleanup: ok
			},
			wantErrSubstr: "zed context use",
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "tok", "--insecure"},
				{"zed", "context", "use", "agentprimitives"},
				{"zed", "context", "remove", "agentprimitives"},
			},
			wantStopped: true,
		},
		{
			name:        "zed remove fails on shutdown: warning, no error",
			secrets:     []runtime.Object{newTokenSecret("tok")},
			forwarder:   &fakeForwarder{startedOn: 60061},
			zedReturns:  []queuedReturn{{}, {}, {out: []byte("rm boom"), err: errors.New("exit 1")}},
			signalAfter: 5 * time.Millisecond,
			wantStopped: true,
			// Expect 3 zed calls — set, use, remove (the failing one). Caller
			// must not return error from the orchestrator.
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "tok", "--insecure"},
				{"zed", "context", "use", "agentprimitives"},
				{"zed", "context", "remove", "agentprimitives"},
			},
			wantOutSubstrs: []string{`warning: failed to remove zed context "agentprimitives"`},
		},
		{
			// zed not on PATH: port-forward still starts; runner returns
			// Available()==false and the orchestrator prints copy-pasteable
			// commands instead of invoking zed. No zed calls expected.
			//
			// The printed command must NOT embed the live pre-shared key. This
			// is the common case on a fresh workstation, and the PSK is the
			// unscoped root credential for the whole authorization system, so
			// echoing it drops the secret into terminal scrollback and any
			// pasted bug report. A placeholder plus a pointer to the Secret is
			// just as copy-pasteable and leaks nothing.
			name:              "zed not on PATH: continues, printing a placeholder rather than the live PSK",
			secrets:           []runtime.Object{newTokenSecret("sdbtok_live_root_credential_wxyz")},
			forwarder:         &fakeForwarder{startedOn: 60061},
			zedRunnerOverride: unavailableRunner{},
			signalAfter:       5 * time.Millisecond,
			wantStopped:       true,
			wantZedArgvs:      nil,
			wantOutSubstrs: []string{
				"zed not found on PATH",
				"zed context set agentprimitives 127.0.0.1:60061 '<preshared-key>' --insecure",
				"zed context use agentprimitives",
				// The hint must name where to read the real key from, or the
				// placeholder would make the command unusable.
				"spicebox-spicedb-token",
			},
			wantOutAbsent: []string{"sdbtok_live_root_credential_wxyz"},
		},
		{
			// Mid-session stream death: the forwarder's Done() channel is primed
			// with a non-nil error before runSpiceDBProxy is called, so the
			// orchestrator's select picks Done() on its first iteration. We do
			// NOT cancel ctx — that path is the "user shut down" branch we want
			// to lose this race against.
			name:    "port-forward dies mid-session: error wrapped, context removed",
			secrets: []runtime.Object{newTokenSecret("tok")},
			forwarder: &fakeForwarder{
				startedOn: 60061,
				doneCh:    primedDoneCh(errors.New("stream EOF")),
			},
			zedReturns:    []queuedReturn{{}, {}, {}}, // set, use, remove
			wantErrSubstr: "port-forward died",
			wantStopped:   true,
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "tok", "--insecure"},
				{"zed", "context", "use", "agentprimitives"},
				{"zed", "context", "remove", "agentprimitives"},
			},
			wantOutSubstrs: []string{
				"==> port-forward stream closed: stream EOF",
				`==> removed zed context "agentprimitives"`,
			},
		},
		{
			// Mid-session stream "death" with nil error (clean ForwardPorts exit
			// without anyone asking) — orchestrator must still surface this as
			// an error, not swallow it.
			name:    "port-forward closes mid-session with nil error: still surfaces as error",
			secrets: []runtime.Object{newTokenSecret("tok")},
			forwarder: &fakeForwarder{
				startedOn: 60061,
				doneCh:    primedDoneCh(nil),
			},
			zedReturns:    []queuedReturn{{}, {}, {}},
			wantErrSubstr: "port-forward stream closed unexpectedly",
			wantStopped:   true,
			wantZedArgvs: [][]string{
				{"zed", "context", "set", "agentprimitives", "127.0.0.1:60061", "tok", "--insecure"},
				{"zed", "context", "use", "agentprimitives"},
				{"zed", "context", "remove", "agentprimitives"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := fake.NewSimpleClientset(tc.secrets...)

			rec := newRecorder(tc.zedReturns...)
			var runner proxyZedRunner = zedctx.NewRunnerForTest("zed", rec.fn)
			if tc.zedRunnerOverride != nil {
				runner = tc.zedRunnerOverride
			}

			// 2s timeout is the backstop in case a bug leaves the orchestrator
			// blocked in its wait; real cases either signalAfter (cancel ctx)
			// or prime forwarder.doneCh to exit the wait via Done().
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			switch {
			case tc.signalAfter > 0:
				time.AfterFunc(tc.signalAfter, cancel)
			case tc.forwarder != nil && tc.forwarder.doneCh != nil:
				// Stream-death case — let Done() fire naturally; don't cancel.
			case tc.wantErrSubstr == "":
				t.Fatalf("test case must set signalAfter or prime forwarder.doneCh for non-error paths")
			default:
				cancel() // immediate, for cases that error before the wait
			}

			var out bytes.Buffer
			err := runSpiceDBProxy(ctx, &out, runSpiceDBProxyDeps{
				Namespace:   "agentprimitives-system",
				ContextName: "agentprimitives",
				LocalPort:   60061,
				Typed:       cli,
				Forwarder:   tc.forwarder,
				Zed:         runner,
			})

			if tc.wantErrSubstr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSubstr)
			} else {
				require.NoError(t, err)
			}
			if tc.wantStopped {
				assert.True(t, tc.forwarder.stopped, "forwarder should be stopped on exit")
			}
			if tc.zedRunnerOverride == nil {
				require.Len(t, rec.calls, len(tc.wantZedArgvs), "expected exactly %d zed calls", len(tc.wantZedArgvs))
				for i, want := range tc.wantZedArgvs {
					assert.Equal(t, want, rec.calls[i], "zed call #%d", i)
				}
			}
			for _, want := range tc.wantOutSubstrs {
				assert.Contains(t, out.String(), want, "expected stdout to contain %q", want)
			}
			for _, unwanted := range tc.wantOutAbsent {
				assert.NotContains(t, out.String(), unwanted,
					"secret material must never reach stdout")
			}
			if tc.wantErrSubstr != "" && err != nil {
				for _, unwanted := range tc.wantOutAbsent {
					assert.NotContains(t, err.Error(), unwanted,
						"secret material must never reach an error message")
				}
			}
		})
	}
}

func TestSpiceDBProxySelectorTargetsOperatorLabel(t *testing.T) {
	assert.Equal(t, "authzed.com/cluster-component=spicedb", apspicedb.Selector,
		"port-forward must select operator-managed SpiceDB pods")
}
