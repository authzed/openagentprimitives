package bento

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/warpstreamlabs/bento/public/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// InboundMessage is the cross-package payload the forward_to_pipeline
// output emits. The bento package keeps the struct local to avoid an
// import cycle with channelsd; channelsd's Plumbing wires a sink
// adapter at startup via SetInboundSink.
type InboundMessage struct {
	ChannelNamespace string
	ChannelName      string
	ChannelKind      string // always "bento"
	Body             string
	AuthzSubject     string
	RoutingKey       string // optional; mapping may set root.routing_key
}

// InboundSink is the channelsd-side hook the bento output flushes
// each generated message through. Implementations MUST be safe for
// concurrent calls.
type InboundSink interface {
	Handle(ctx context.Context, m InboundMessage) error
}

// sinkMu uses RWMutex because Write is called on every generated
// message (potentially many per second across multiple streams) while
// SetInboundSink is invoked once at startup; reader-heavy lock fits.
var (
	sinkMu sync.RWMutex
	sink   InboundSink
)

// SetInboundSink installs (or clears, with nil) the package-level
// sink the forward_to_pipeline output writes to. channelsd calls
// this at startup with an adapter that pushes into the inbound
// pipeline; tests inject a fake.
func SetInboundSink(s InboundSink) {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	sink = s
}

func currentSink() InboundSink {
	sinkMu.RLock()
	defer sinkMu.RUnlock()
	return sink
}

func init() {
	cfg := service.NewConfigSpec().
		Field(service.NewStringField("channel_name")).
		Field(service.NewStringField("channel_namespace")).
		Field(service.NewStringField("authz_subject").Default(""))
	if err := service.RegisterOutput("forward_to_pipeline", cfg, newForwardOutput); err != nil {
		// Init-time panic is appropriate: a name collision or schema
		// error here means the binary is broken.
		panic(fmt.Sprintf("bento: register forward_to_pipeline: %v", err))
	}
}

type forwardOutput struct {
	channelName, channelNamespace, authzSubject string
}

func newForwardOutput(conf *service.ParsedConfig, _ *service.Resources) (out service.Output, maxInFlight int, err error) {
	name, err := conf.FieldString("channel_name")
	if err != nil {
		return nil, 0, err
	}
	ns, err := conf.FieldString("channel_namespace")
	if err != nil {
		return nil, 0, err
	}
	subj, err := conf.FieldString("authz_subject")
	if err != nil {
		return nil, 0, err
	}
	return &forwardOutput{
		channelName:      name,
		channelNamespace: ns,
		authzSubject:     subj,
	}, 1, nil
}

// forwardRetryDelays is the backoff schedule for a failed pipeline delivery:
// one attempt up front plus one per entry. Bounded on purpose — see Write.
// A var so tests can shorten it.
var forwardRetryDelays = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}

func (f *forwardOutput) Connect(_ context.Context) error { return nil }

// Write forwards one generated message into the channelsd pipeline, retrying a
// bounded number of times before giving up.
//
// Bento retries an output that returns an error, immediately and forever, so
// returning the sink's error verbatim turns a message that can never succeed —
// a Channel misconfiguration, a rejected AgentSession — into a hot loop against
// the apiserver. In a live cluster that produced 41 failed AgentSessions in 44
// seconds and a matching alert flood that obscured the actual fault.
//
// A cron firing is a discrete scheduled event, not a durable queue item:
// replaying a stale trigger forever is never right, and the next tick is coming
// regardless. So retry a few times with backoff to ride out a transient blip,
// then log loudly and report the message delivered. Returning nil is the only
// way to tell Bento to stop retrying this one.
func (f *forwardOutput) Write(ctx context.Context, m *service.Message) error {
	s := currentSink()
	if s == nil {
		// No sink wired — a startup/wiring fault, not a per-message one.
		// Surface it so the operator notices early.
		return fmt.Errorf("forward_to_pipeline: InboundSink not installed")
	}
	bodyBytes, err := m.AsBytes()
	if err != nil {
		// Malformed message: retrying cannot help.
		return nil
	}
	routingKey, _ := m.MetaGet("routing_key")
	msg := InboundMessage{
		ChannelNamespace: f.channelNamespace,
		ChannelName:      f.channelName,
		ChannelKind:      "bento",
		Body:             string(bodyBytes),
		AuthzSubject:     f.authzSubject,
		RoutingKey:       routingKey,
	}

	var lastErr error
	for attempt := 0; attempt <= len(forwardRetryDelays); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				// Shutting down: do not hold the stream open on a doomed
				// message, and do not return an error Bento would retry.
				return nil
			case <-time.After(forwardRetryDelays[attempt-1]):
			}
		}
		if lastErr = s.Handle(ctx, msg); lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
	}

	// Give up. Logged at the sink layer too, but log here with the attempt
	// count so "this fired and was dropped" is greppable per channel — the
	// alternative is the silent-drop this method must never become.
	log.FromContext(ctx).Error(lastErr, "forward_to_pipeline: giving up on message after bounded retries",
		"channel", f.channelNamespace+"/"+f.channelName,
		"attempts", len(forwardRetryDelays)+1)
	return nil
}

func (f *forwardOutput) Close(_ context.Context) error { return nil }
