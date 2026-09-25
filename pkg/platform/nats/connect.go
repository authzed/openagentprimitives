package nats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
)

// ServerName is the TLS server name (and Service DNS name) of the in-cluster
// NATS server. Clients connecting over a port-forward (where the dial host is
// 127.0.0.1) must set Options.ServerName to this so certificate verification
// matches the server cert's SANs.
const ServerName = "spicebox-nats.agentprimitives-system.svc"

// Options configures a NATS connection. The zero value (no CredsPath, no
// CAPath) yields a plain unauthenticated connection — used only by the e2e
// harness against its embedded server. Production callers always set both.
type Options struct {
	URL        string
	CredsPath  string // path to a NATS creds file (jwt.FormatUserConfig output)
	CAPath     string // path to a PEM CA bundle the server cert is verified against
	ServerName string // TLS SNI / verification name; required when URL host != cert SAN
	Name       string // connection name for server-side logging
}

// buildOptions translates Options into nats.Option values.
func buildOptions(o Options) ([]natsgo.Option, error) {
	var opts []natsgo.Option
	if o.Name != "" {
		opts = append(opts, natsgo.Name(o.Name))
	}
	if o.CredsPath != "" {
		opts = append(opts, natsgo.UserCredentials(o.CredsPath))
		// Scope this connection's request/reply inbox to the principal named in
		// its own creds, so it subscribes exactly where its minted grant allows
		// (InboxSubjectFor over the same name) instead of the shared "_INBOX."
		// root every principal could otherwise read.
		//
		// The name comes from the creds file, not o.Name: o.Name is a
		// per-process label chosen by the calling binary and does NOT match the
		// grant — webd connects as "webd-live-view" against a user JWT named
		// "webd", and one creds file is mounted by three binaries under three
		// names. The JWT the server authorizes is the one authority both sides
		// can agree on.
		//
		// Unauthenticated connections (the e2e harness against its embedded
		// server) have no creds and keep nats.go's default inbox — no grant to
		// disagree with.
		prefix, err := inboxPrefixFromCreds(o.CredsPath)
		if err != nil {
			return nil, err
		}
		opts = append(opts, natsgo.CustomInboxPrefix(prefix))
	}
	if o.CAPath != "" {
		caPEM, err := os.ReadFile(o.CAPath)
		if err != nil {
			return nil, fmt.Errorf("read CA %s: %w", o.CAPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("CA %s: no certificates parsed", o.CAPath)
		}
		tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		if o.ServerName != "" {
			tc.ServerName = o.ServerName
		}
		opts = append(opts, natsgo.Secure(tc))
	}
	return opts, nil
}

// inboxPrefixFromCreds reads the principal's name out of the user JWT in a
// creds file and returns its inbox prefix.
//
// Every failure here is returned, never defaulted past: falling back to the
// shared root would mean a client subscribing where its grant does not allow,
// which fails as a total, silent loss of request/reply rather than as anything
// a reader would connect back to this function. An unreadable or malformed
// creds file also means the connection itself is about to fail authentication,
// so erroring here reports the real problem earlier and more precisely.
func inboxPrefixFromCreds(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read NATS creds %s: %w", path, err)
	}
	token, err := jwt.ParseDecoratedJWT(contents)
	if err != nil {
		return "", fmt.Errorf("parse NATS creds %s: %w", path, err)
	}
	uc, err := jwt.DecodeUserClaims(token)
	if err != nil {
		return "", fmt.Errorf("decode user JWT from NATS creds %s: %w", path, err)
	}
	if strings.TrimSpace(uc.Name) == "" {
		return "", fmt.Errorf("NATS creds %s: user JWT has no name, so its reply inbox cannot be scoped", path)
	}
	return InboxPrefixFor(uc.Name), nil
}

// ConnectFromEnv dials NATS with the given URL and connection name, reading the
// creds + CA paths from the canonical NATS_CREDS_PATH / NATS_CA_PATH env vars
// and pinning ServerName to the in-cluster service name. It is the standard
// production connect path for the server binaries (channelsd, authzd, webd,
// runner); callers keep their own error handling around the returned error.
func ConnectFromEnv(url, name string) (*natsgo.Conn, error) {
	return Connect(Options{
		URL:        url,
		CredsPath:  os.Getenv("NATS_CREDS_PATH"),
		CAPath:     os.Getenv("NATS_CA_PATH"),
		ServerName: ServerName,
		Name:       name,
	})
}

// ConnectTimeout bounds the initial connect — INCLUDING DNS resolution. nats.go
// resolves the URL host via net.LookupHost OUTSIDE its per-dial timeout, so a
// hung resolver blocks the connect (and thus the caller's startup) forever with
// no upper bound. We hit this on GKE Autopilot: NodeLocal DNSCache answers on a
// link-local address (169.254.x.x) that the default DNS-egress NetworkPolicy did
// not permit, so every service-name lookup hung. Bounding the whole connect lets
// callers fail fast — log + continue (best-effort) — instead of hanging.
const ConnectTimeout = 10 * time.Second

// outageUnknown is what a reconnect or close line reports when no disconnect
// was recorded first — the initial connect never succeeded (RetryOnFailedConnect
// retries before the first ReconnectedCB) or the process started mid-outage.
// Reported as a word rather than as "0s" so nobody reads a never-measured gap as
// a gap that did not happen.
const outageUnknown = "unknown"

// unnamedConn stands in for Options.Name on connections that do not set one.
const unnamedConn = "unnamed"

// lifecycleLogger records a connection's disconnect / reconnect / close
// transitions, and the duration of each outage.
//
// This is the ONLY place a bus gap is observable. A NATS subject with no
// subscriber is not an error: the server drops the message and Publish returns
// nil. channelsd is the sole subscriber of "ap.session.*.*.out.>", so while its
// connection is down every outbound envelope a runner publishes is discarded
// and every layer above sees success. The silence watchdog cannot back-stop it
// either — its stall notice rides the same bus.
//
// The DURATION is the load-bearing field, not the disconnect line: it lets an
// operator match a user's "the agent never replied at 14:32" to a bus window
// instead of hunting the agent loop.
//
// now and log are fields rather than direct time.Now / slog calls so the
// duration reporting is unit-testable against a fixed clock.
type lifecycleLogger struct {
	name string
	now  func() time.Time
	log  func(msg string, args ...any)

	// mu guards disconnectedAt. nats.go dispatches these callbacks from its own
	// async goroutine, and Close during a disconnect can interleave them.
	mu             sync.Mutex
	disconnectedAt time.Time
}

func newLifecycleLogger(name string) *lifecycleLogger {
	return &lifecycleLogger{
		name: name,
		now:  time.Now,
		log:  func(msg string, args ...any) { slog.Default().Info(msg, args...) },
	}
}

func (l *lifecycleLogger) onDisconnect(err error) {
	l.mu.Lock()
	l.disconnectedAt = l.now()
	l.mu.Unlock()
	// nats.go passes a nil error for a locally-initiated disconnect (Close /
	// Drain); the gap is just as real, so it is logged either way.
	reason := "none reported (local close, drain, or server shutdown)"
	if err != nil {
		reason = err.Error()
	}
	l.log("nats disconnected — messages published to subjects only this connection subscribes are being dropped",
		"conn", l.name, "err", reason)
}

// takeOutage measures and CLEARS the current outage window, so a flapping
// connection reports each gap on its own rather than one ever-growing number.
func (l *lifecycleLogger) takeOutage() string {
	l.mu.Lock()
	at := l.disconnectedAt
	l.disconnectedAt = time.Time{}
	l.mu.Unlock()
	if at.IsZero() {
		return outageUnknown
	}
	return l.now().Sub(at).Round(time.Millisecond).String()
}

func (l *lifecycleLogger) onReconnect() {
	l.log("nats reconnected", "conn", l.name, "outage", l.takeOutage())
}

func (l *lifecycleLogger) onClosed(lastErr string) {
	l.log("nats connection closed — no further reconnect attempts; every publish and subscription on it is now dead",
		"conn", l.name, "outage", l.takeOutage(), "lastErr", lastErr)
}

// connectOptions is buildOptions plus the project-standard reconnect policy and
// the observability handlers. Split out of Connect so the option set can be
// resolved and asserted on without dialing a server — Connect itself is a live
// dial and cannot be unit-tested.
func connectOptions(o Options) ([]natsgo.Option, error) {
	base, err := buildOptions(o)
	if err != nil {
		return nil, err
	}
	// Options.Name is optional (cmd/oap's CLI connections leave it unset), but a
	// log line whose conn field is empty defeats the only purpose these lines
	// have — telling an operator WHICH connection went away. Normalize once, so
	// the async-error line and the lifecycle lines always agree.
	name := o.Name
	if name == "" {
		name = unnamedConn
	}
	lc := newLifecycleLogger(name)
	return append(base,
		natsgo.RetryOnFailedConnect(true),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(2*time.Second),
		// Surface async errors (permission violations, slow consumers, …) as
		// structured logs. nats.go's default handler prints a bare
		// "nats: permissions violation for Subscription to <subj>" to stderr
		// with no connection/subject context — invisible to operators grepping
		// by component or subject. A denied subscription (e.g. a missing
		// SubAllow entry on a minted per-session JWT) otherwise passes silently
		// because Subscribe returns nil and the violation arrives only here.
		natsgo.ErrorHandler(func(_ *natsgo.Conn, sub *natsgo.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			slog.Default().Info("nats async error",
				"conn", name, "subject", subject, "err", err.Error())
		}),
		// The bus gap itself. See lifecycleLogger's doc for why nothing above
		// this layer can observe it.
		natsgo.DisconnectErrHandler(func(_ *natsgo.Conn, err error) { lc.onDisconnect(err) }),
		natsgo.ReconnectHandler(func(_ *natsgo.Conn) { lc.onReconnect() }),
		natsgo.ClosedHandler(func(nc *natsgo.Conn) {
			// nats.go invokes ClosedCB during teardown; take the last error
			// (the "why" — auth failure, exhausted retries) only when there is
			// still a connection to ask.
			lastErr := "none"
			if nc != nil {
				if err := nc.LastError(); err != nil {
					lastErr = err.Error()
				}
			}
			lc.onClosed(lastErr)
		}),
	), nil
}

// Connect dials NATS with the given Options plus the project-standard reconnect
// policy, bounded by ConnectTimeout so a hung DNS lookup or unreachable network
// fails fast instead of blocking the caller indefinitely.
func Connect(o Options) (*natsgo.Conn, error) {
	base, err := connectOptions(o)
	if err != nil {
		return nil, err
	}
	// natsgo.Connect takes no context and resolves DNS synchronously without a
	// deadline, so neither RetryOnFailedConnect nor a custom dialer can bound a
	// hung resolver. Run it in a goroutine and bound the WAIT with a context
	// timeout (context.WithTimeout over time.After so the timer is released on
	// the fast path via cancel). The inner goroutine may still be blocked in
	// net.LookupHost on timeout; it unblocks when the resolver finally errors
	// and then sends to the buffered channel (no leak past that point).
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()
	type result struct {
		nc  *natsgo.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		nc, err := natsgo.Connect(o.URL, base...)
		ch <- result{nc: nc, err: err}
	}()
	select {
	case r := <-ch:
		return r.nc, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("nats connect to %s timed out after %s (DNS resolution or network egress blocked?): %w", o.URL, ConnectTimeout, ctx.Err())
	}
}
