package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// exitingSource is a socketSource whose RunContext does whatever runFn says,
// standing in for socketmode.Client.RunContext returning (it only ever does on
// shutdown or a fatal connect error).
type exitingSource struct {
	runFn func(ctx context.Context) error
}

func (exitingSource) Events() <-chan socketmode.Event        { return nil }
func (s exitingSource) RunContext(ctx context.Context) error { return s.runFn(ctx) }
func (exitingSource) Ack(socketmode.Request, ...interface{}) {}

func TestRunReportsTransportLostOnlyWhenNotShuttingDown(t *testing.T) {
	cases := []struct {
		name string
		// runFn receives the listener and the cancel for its ctx so a case can
		// simulate a shutdown racing the return.
		runFn   func(l *slackListener, cancel context.CancelFunc) func(context.Context) error
		wantErr string // "" = TransportLost must NOT be called
	}{
		{
			name: "fatal connect error: TransportLost called with the cause",
			runFn: func(*slackListener, context.CancelFunc) func(context.Context) error {
				return func(context.Context) error { return errors.New("invalid_auth") }
			},
			wantErr: "slack socket mode: invalid_auth",
		},
		{
			name: "nil return without shutdown: TransportLost still called",
			runFn: func(*slackListener, context.CancelFunc) func(context.Context) error {
				return func(context.Context) error { return nil }
			},
			wantErr: "exited without an error",
		},
		{
			name: "ctx cancelled: shutdown, TransportLost not called",
			runFn: func(_ *slackListener, cancel context.CancelFunc) func(context.Context) error {
				return func(ctx context.Context) error { cancel(); return ctx.Err() }
			},
		},
		{
			name: "Stop called: shutdown, TransportLost not called",
			runFn: func(l *slackListener, _ context.CancelFunc) func(context.Context) error {
				return func(ctx context.Context) error {
					require.NoError(t, l.Stop(ctx))
					return errors.New("use of closed network connection")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var lost []error
			l := &slackListener{
				deps: channelkinds.Deps{
					Channel:       &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-channel"}},
					TransportLost: func(err error) { lost = append(lost, err) },
				},
				stop: make(chan struct{}),
			}
			l.src = exitingSource{runFn: tc.runFn(l, cancel)}

			l.run(ctx) // returns once RunContext does

			if tc.wantErr == "" {
				assert.Empty(t, lost, "a shutdown must not be reported as a lost transport")
				return
			}
			require.Len(t, lost, 1, "a lost transport must be reported exactly once")
			assert.Contains(t, lost[0].Error(), tc.wantErr)
		})
	}
}
