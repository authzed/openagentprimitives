package slack

import (
	"context"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// socketSource is the socket-mode event surface the listener consumes. The
// real impl wraps *socketmode.Client; the e2e fake is channel-backed. Designed
// so a full fake-WS server could implement it later without touching callers.
type socketSource interface {
	Events() <-chan socketmode.Event
	RunContext(ctx context.Context) error
	Ack(req socketmode.Request, payload ...interface{})
}

// realSocketSource adapts *socketmode.Client to socketSource. sm.Events is a
// field (not a method) on the underlying client, so Events() wraps it.
type realSocketSource struct{ sm *socketmode.Client }

var _ socketSource = realSocketSource{}

func (r realSocketSource) Events() <-chan socketmode.Event              { return r.sm.Events }
func (r realSocketSource) RunContext(ctx context.Context) error         { return r.sm.RunContext(ctx) }
func (r realSocketSource) Ack(req socketmode.Request, p ...interface{}) { r.sm.Ack(req, p...) }

// newSocketSource builds the production socket-mode source. Overridable by
// tests via InstallTestTransport (testhooks.go). It takes the listener client;
// the real path needs the concrete *slackapi.Client for socketmode.New, so it
// type-asserts — a fake client + real source is never used together (tests
// inject BOTH a fake client and a fake source).
var newSocketSource = func(api listenerAPIClient) socketSource {
	return realSocketSource{sm: socketmode.New(api.(*slackapi.Client))}
}
