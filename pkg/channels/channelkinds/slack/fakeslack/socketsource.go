package fakeslack

import (
	"context"

	"github.com/slack-go/slack/socketmode"
)

// SocketSource is a channel-backed socketSource
// (pkg/channels/channelkinds/slack/socket_source.go) for e2e: the harness pushes
// socketmode.Events via Push; the real listener event loop consumes them from
// Events() exactly as it would the production socketmode.Client's channel.
type SocketSource struct {
	events chan socketmode.Event
}

// NewSocketSource returns a SocketSource ready to accept Push calls. The
// buffer absorbs bursts from InjectDM/InjectMention without requiring the
// harness to synchronize with the listener's dispatch goroutine.
func NewSocketSource() *SocketSource {
	return &SocketSource{events: make(chan socketmode.Event, 64)}
}

// Events implements socketSource.
func (s *SocketSource) Events() <-chan socketmode.Event { return s.events }

// RunContext implements socketSource. The production socketmode.Client's
// RunContext blocks until ctx is done (handling reconnects internally); this
// fake has no connection to maintain, so it just blocks on ctx.
func (s *SocketSource) RunContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Ack implements socketSource as a no-op: there is no real Slack backend to
// acknowledge to.
func (s *SocketSource) Ack(_ socketmode.Request, _ ...interface{}) {}

// Push enqueues an event for the listener's dispatch goroutine to consume.
func (s *SocketSource) Push(e socketmode.Event) { s.events <- e }
