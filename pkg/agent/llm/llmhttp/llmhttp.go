// Package llmhttp is the one HTTP client every LLM SDK in this repo is built
// on. A provider SDK left on its default client has no timeouts at all, so a
// streamed response that goes silent on a live connection blocks the caller
// forever, holding its session in Running long past its budget.
//
// The client bounds a call at the transport, where it is uniform across every
// SDK (Anthropic, OpenAI, OpenRouter, and the secondary-LLM helpers) without a
// per-provider copy:
//
//   - connect, TLS handshake, and response-header timeouts;
//   - HTTP/2 health pings, so a connection whose peer vanished is torn down;
//   - a stream-idle timeout: once headers arrive, every body read that
//     returns data re-arms a timer, and a body that delivers nothing for
//     StreamIdleTimeout is aborted with ErrStreamIdle.
//
// There is deliberately no total-request timeout: a long generation that is
// still streaming is healthy. Providers send keepalive traffic while they
// work (Anthropic emits `ping` events during long generations), so minutes of
// total silence means the stream is dead, not slow.
package llmhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// ErrStreamIdle is the cause of a response body aborted for going silent.
// Callers can errors.Is against it through the SDK's wrapped stream error.
var ErrStreamIdle = errors.New("llm response stream idle")

// Defaults for Config's zero fields.
const (
	DefaultDialTimeout           = 30 * time.Second
	DefaultTLSHandshakeTimeout   = 30 * time.Second
	DefaultResponseHeaderTimeout = 3 * time.Minute
	DefaultStreamIdleTimeout     = 5 * time.Minute
	// HTTP/2: send a PING after this long without a frame, and drop the
	// connection if the PING is not answered within DefaultPingTimeout.
	DefaultHTTP2SendPingTimeout = 30 * time.Second
	DefaultPingTimeout          = 15 * time.Second
)

// Config tunes a client. Zero fields take the Default* values; tests shrink
// them to exercise the timeouts quickly.
type Config struct {
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.TLSHandshakeTimeout <= 0 {
		c.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if c.StreamIdleTimeout <= 0 {
		c.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	return c
}

// NewClient builds an *http.Client with cfg's bounds. Most callers want
// Client(), which shares one connection pool process-wide.
func NewClient(cfg Config) *http.Client {
	cfg = cfg.withDefaults()
	base, ok := http.DefaultTransport.(*http.Transport)
	var t *http.Transport
	if ok {
		t = base.Clone() // keeps proxy-from-env, ForceAttemptHTTP2, pool sizes
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}
	t.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	t.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	t.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: DefaultHTTP2SendPingTimeout,
		PingTimeout:     DefaultPingTimeout,
	}
	return &http.Client{Transport: &idleTransport{base: t, idle: cfg.StreamIdleTimeout}}
}

var shared = sync.OnceValue(func() *http.Client { return NewClient(Config{}) })

// Client returns the process-wide client with default bounds. Pass it to an
// SDK via its WithHTTPClient option.
func Client() *http.Client { return shared() }

// idleTransport wraps every response body in an idle watchdog.
type idleTransport struct {
	base http.RoundTripper
	idle time.Duration
}

func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	b := &idleBody{rc: resp.Body, ctx: ctx, cancel: cancel, idle: t.idle}
	b.timer = time.AfterFunc(t.idle, func() { cancel(ErrStreamIdle) })
	resp.Body = b
	return resp, nil
}

// idleBody aborts the request (cancelling its context, which unblocks an
// in-flight Read) when no data arrives for idle.
type idleBody struct {
	rc     io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   time.Duration
	timer  *time.Timer
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && errors.Is(context.Cause(b.ctx), ErrStreamIdle) {
		return n, fmt.Errorf("%w: no data received for %s", ErrStreamIdle, b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel(nil)
	return err
}
