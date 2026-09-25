// internal/cmd/channelsd/main.go — channelsd process entry point.
//
// Wires together NATS, SpiceDB, Kubernetes, the inbound pipeline, the outbound
// relay, and per-Channel Listeners. One binary per cluster; horizontally
// unscalable in v1 (single replica; informer upgrade tracked as TODO).
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	runtimedebug "runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	// Register kind implementations.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"         // agent kind (session-to-session channels) — channelsd-hosted, delivery is a bus write
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"           // bento kind (cron-driven inputs) — also installs InboundSink at startup
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"       // browser kind (webd-hosted) — registered so channelsd recognizes it as client-hosted and skips it instead of erroring "unknown kind"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"          // fake kind (test/plan-1)
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"        // github kind (webd-hosted webhook receiver) — registered so channelsd recognizes it as client-hosted and skips it instead of erroring "unknown kind"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"         // local TUI kind (CLI-hosted) — registered so channelsd recognizes it as client-hosted and skips it
	slackkind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit/gates/triggeredidle" // register the idle-status gate that flips a triggered, humanless Idle session's pinned badge off "in progress"

	"github.com/authzed/openagentprimitives/internal/cmd/channelsd/internal/assetfetch"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports" // completes the relsource claim table before the guarded GrantSource writer is checked
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories" // registers interaction categories; also used below for the CredentialLink constant
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/historyresp"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the static/oauth/federated credkind.Kinds the broker dispatches to via registry.Get
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/kube"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// natsPub adapts *nats.Conn to the pipeline.NATS interface.
type natsPub struct{ nc *nats.Conn }

func (n *natsPub) Publish(subj string, data []byte) error { return n.nc.Publish(subj, data) }

// personalizableClassLookup adapts *spicedb.Client's LookupPersonalizableClasses
// to channelkinds.PersonalizableClassLookup's plain "<ns>/<class>" string
// contract, so channelkinds never has to import the SpiceDB transport package
// directly — the same discipline channelkinds.AuthzReader's doc comment
// documents for its own methods.
type personalizableClassLookup struct{ az *spicedb.Client }

func (p personalizableClassLookup) LookupPersonalizableClassRefs(ctx context.Context, canonicalID identity.CanonicalUserID, limit uint32, fullyConsistent bool) ([]string, error) {
	classes, err := p.az.LookupPersonalizableClasses(ctx, canonicalID, limit, fullyConsistent)
	if err != nil {
		return nil, err
	}
	return classes.Names(), nil
}

// bentoInboundSink adapts a bento.InboundMessage into a
// channelkinds.InboundEvent and feeds it through the shared inbound
// pipeline. Required because the bento package keeps its sink
// interface local to avoid an import cycle with channelsd; the seam
// is installed once at startup via bento.SetInboundSink.
type bentoInboundSink struct {
	k8s  client.Client
	pipe channelkinds.InboundPipeline
	log  logr.Logger
}

// Handle resolves the Channel CR for the supplied message and dispatches
// the inbound through the pipeline. Errors are logged and returned so
// the bento `forward_to_pipeline` output surfaces them as a write
// failure (bento retries per its output retry policy).
func (s *bentoInboundSink) Handle(ctx context.Context, m bento.InboundMessage) error {
	var ch spiceboxv1alpha1.Channel
	if err := s.k8s.Get(ctx, client.ObjectKey{Namespace: m.ChannelNamespace, Name: m.ChannelName}, &ch); err != nil {
		s.log.Info("bento inbound: Channel CR lookup failed",
			"ns", m.ChannelNamespace, "name", m.ChannelName, "err", err.Error())
		return fmt.Errorf("bento inbound: get Channel %s/%s: %w", m.ChannelNamespace, m.ChannelName, err)
	}
	key := bentoChannelKey(m)
	dec, err := s.pipe.Deliver(ctx, channelkinds.InboundEvent{
		Channel:      &ch,
		ChannelKey:   key,
		MessageText:  m.Body,
		AuthzSubject: m.AuthzSubject,
	})
	if err != nil {
		s.log.Info("bento inbound: pipeline.Deliver errored",
			"ns", m.ChannelNamespace, "name", m.ChannelName, "key", key, "err", err.Error())
		return fmt.Errorf("bento inbound: deliver %s/%s: %w", m.ChannelNamespace, m.ChannelName, err)
	}
	if dec.Outcome == channelkinds.OutcomeInternalError {
		s.log.Info("bento inbound: pipeline returned InternalError",
			"ns", m.ChannelNamespace, "name", m.ChannelName, "key", key, "notice", dec.Notice.Category())
		return fmt.Errorf("bento inbound: pipeline outcome=InternalError for %s/%s", m.ChannelNamespace, m.ChannelName)
	}
	return nil
}

// bentoChannelKey returns the channelKey used to correlate inbounds to
// AgentSessions. If the bento mapping forced one via metadata
// (`meta routing_key = ...`), use it verbatim; otherwise synthesize a
// per-firing key so each cron tick spawns a fresh session
// (`cron:<channel>:<ns-since-epoch>`).
func bentoChannelKey(m bento.InboundMessage) string {
	if m.RoutingKey != "" {
		return m.RoutingKey
	}
	return "cron:" + m.ChannelName + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// config holds channelsd's non-secret configuration, populated from flags
// and (via clikit) the environment. Secret material (the SpiceDB token, the
// memory bearer token, the passthrough signing key, the Anthropic API key) is
// read from the mounted token/key files named by the *Path fields in run(),
// never carried in argv.
type config struct {
	natsURL          string
	opMemoryURL      string
	memTokenPath     string
	spicedbEnd       string
	spicedbTok       string
	insecureGRPC     bool
	signingKeyPath   string
	anthropicKeyPath string
	// approverFanoutLimit caps how many approvers a single approval prompt
	// notifies (delivery-only; click-time authorization admits any eligible
	// approver). 0 ⇒ channelkinds.DefaultApproverFanoutLimit.
	approverFanoutLimit int
}

func main() {
	// Before anything can compile a SpiceDB schema: its compiler logs a trace
	// line per definition through zerolog's process-global logger, and this
	// binary's stderr is `kubectl logs`. channelsd's own logging is slog/logr
	// and is untouched. See pkg/platform/deplogs.
	deplogs.Silence()

	// Earliest stderr breadcrumb: proves main() entered. If this line
	// is absent from container logs the binary is dying during package
	// init (e.g. a transitive init() panic recovered by a parent),
	// NOT during main(). Without it "channelsd ran and went silent"
	// is indistinguishable from "channelsd never started" — exactly
	// the no-logs symptom that hid the SetLogger-race bug below.
	fmt.Fprintln(os.Stderr, "channelsd: main() entered")
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "channelsd: panic at main: %v\n%s\n", r, runtimedebug.Stack())
			os.Exit(1)
		}
	}()
	// Root context: canceled on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "channelsd",
		Short:        "Channel transport daemon: inbound pipeline, outbound relay, and per-Channel listeners.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.natsURL, "nats-url", "", "NATS connection URL")
	fs.StringVar(&cfg.opMemoryURL, "operator-memory-url", "", "Operator memory base URL")
	fs.StringVar(&cfg.memTokenPath, "memory-token-path", "/var/run/channelsd/memory-token", "Path to system memory bearer token file")
	fs.StringVar(&cfg.spicedbEnd, "spicedb-endpoint", "", "SpiceDB gRPC endpoint")
	fs.StringVar(&cfg.spicedbTok, "spicedb-token-path", "/var/run/channelsd/spicedb-token", "Path to SpiceDB bearer token file")
	fs.BoolVar(&cfg.insecureGRPC, "spicedb-insecure", true, "Use insecure (plain-text) SpiceDB gRPC connection")
	// Passthrough credential-request link wiring. Optional: when
	// the signing key is absent the credential-request watcher logs
	// and exits (sessions in AwaitingCredentials get no Slack DM
	// until the operator wires the key). The external base URL is
	// read live from the webd ConfigMap via webdURLProvider.
	fs.StringVar(&cfg.signingKeyPath, "passthrough-signing-key-path", "/etc/passthrough-link-key/key", "path to the HMAC signing-key file (hex-encoded, mounted from "+spiceboxv1alpha1.PassthroughLinkSigningKeySecret+")")
	fs.StringVar(&cfg.anthropicKeyPath, "anthropic-api-key-path", "/etc/anthropic-api-key/key", "path to file containing Anthropic API key (empty disables the LLM explainer)")
	fs.IntVar(&cfg.approverFanoutLimit, "approver-fanout-limit", channelkinds.DefaultApproverFanoutLimit,
		"max approvers a single approval prompt notifies (delivery cap only; any eligible approver may still approve)")
	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"nats-url":              clikit.EnvNATSURL,
		"operator-memory-url":   clikit.EnvOperatorMemoryURL,
		"spicedb-endpoint":      spicedb.EnvEndpoint,
		"approver-fanout-limit": "APPROVER_FANOUT_LIMIT",
	})
	return cmd
}

// inboundSubjectAuthorized reports whether m's SUBJECT authorizes acting on the
// session env claims, and is the single gate both inbound wrappers run before
// dispatching. A refusal returns a caller-facing reason string, never "".
//
// The decision itself is channelevents.AuthorizeInSubject — shared with the e2e
// harness, which stands in for this process for every scenario (a gate living
// only here is a gate no e2e run exercises). What stays here is the logging: the
// publisher-locating trio (subject, claimed session, kind) per AGENTS.md's
// no-silent-errors rule, and the reason string a refused requester is told.
//
// The subject is the routing authority; Envelope.Session is not. Every inbound
// subscription here is the cluster-wide "ap.session.*.*.in.<kind>", while a
// runner's per-session NATS JWT permits publishing under exactly one
// "ap.session.<ns>.<own-name>.>" tree — and that tree covers ".in." as well as
// ".out.". The subject is therefore the only session identity NATS actually
// authorized, and env.Session is publisher-controlled JSON. Trusting the latter
// makes the per-session grant non-load-bearing: a publisher permitted on
// session A's inbound subject could set env.Session to B and have channelsd
// resolve B's parked approval, park or clear B's PendingInteractions entry,
// deliver a message into B, or drive B's resurface.
//
// This is the inbound half of the rule pkg/channels/channelsd/outbound/relay.go's handle
// enforces for ".out." — same reasoning, same drop-with-a-log shape. Note that
// the acting principal on a decision is a separate question, taken from the
// payload's Decider and re-checked against SpiceDB standing in
// HandleInteractionDecision; this gate is about WHICH SESSION is acted on.
//
// Legitimate publishers always agree — PublishIn and every manual publish site
// build subject and envelope from one (ns, name) pair — so a mismatch is a bug
// or an attack, never normal traffic.
func inboundSubjectAuthorized(logger logr.Logger, name string, m *nats.Msg, env channelevents.Envelope) (string, bool) {
	claimed := env.Session.Namespace + "/" + env.Session.Name
	_, _, err := channelevents.AuthorizeInSubject(m.Subject, env)
	switch {
	case errors.Is(err, channelevents.ErrUnroutableSubject):
		logger.Info(name+": drop, unparseable in subject",
			"subject", m.Subject, "claimedSession", claimed, "kind", string(env.Kind))
		return name + ": unroutable subject " + strconv.Quote(m.Subject), false
	case err != nil:
		// Re-parsed for the log alone: a refusal returns no session, on purpose
		// (nothing may act on the pair a mismatch names), but an operator
		// reading this line needs to see WHICH session the publisher was
		// authorized for next to the one it claimed.
		ns, sessName, _ := channelevents.ParseInSubject(m.Subject)
		logger.Info(name+": drop, envelope session does not match the authorized subject",
			"subject", m.Subject, "subjectSession", ns+"/"+sessName,
			"claimedSession", claimed, "kind", string(env.Kind))
		return name + ": envelope session does not match the authorized subject", false
	}
	return "", true
}

// envelopeHandler wraps the shared decode-envelope + cross-check + dispatch
// pattern for the one-way inbound subscriptions: the envelope is JSON-decoded
// from the message, a decode error is logged as "<name>: decode envelope" and
// dropped (malformed inbounds don't cascade), the claimed session is checked
// against the authorized subject (inboundSubjectAuthorized — a mismatch is
// logged and dropped), and otherwise fn runs with its per-handler error logged
// as "<name>: <handler>" plus the session field.
//
// The subject is not an optional input: it arrives on the *nats.Msg every
// subscription hands this wrapper, so there is no path through it that skips
// the check, and a handler bound here cannot reintroduce the gap. Downstream
// handlers keep routing off env.Session precisely because this ran first.
//
// Package-level, not a closure inside run, for the same reason respondingHandler
// is: it is directly unit-testable without a live NATS connection.
func envelopeHandler(
	logger logr.Logger,
	name, handler string,
	fn func(context.Context, channelevents.Envelope) error,
) func(*nats.Msg) {
	ctx := handlerContext(logger)
	return func(m *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			logger.Error(err, name+": decode envelope")
			return
		}
		if _, ok := inboundSubjectAuthorized(logger, name, m, env); !ok {
			return
		}
		if err := fn(ctx, env); err != nil {
			logger.Error(err, name+": "+handler, "session", env.Session)
		}
	}
}

// handlerContext is the context every inbound handler runs under: a
// NON-CANCELLING base carrying the live logger.
//
// The logger is not a nicety. Every inbound pipeline handler logs through
// log.FromContext(ctx), and the global controller-runtime logger this process
// would otherwise fall back to is fulfilled with a NO-OP SINK by a transitive
// init() before main runs (see the SetLogger race at the top of run). Handing a
// handler a bare context.Background() therefore sends every line it emits
// internally to /dev/null — including the ones it emits INSTEAD of returning an
// error, which are the only trace those paths ever leave. A wrapper logging the
// handler's returned error does not cover them, and the gap is silent at build
// time and at runtime alike. The metaagent handlers in the same subscription
// table already carry rootCtx for exactly this reason.
//
// The base stays context.Background() rather than rootCtx on purpose: an
// in-flight inbound handler writes memory, publishes wakeups and resolves
// approvals, so cancelling it on SIGTERM would trade a logging gap for a
// durability one. Shutdown ordering is the subscription's job, not a deadline's.
func handlerContext(logger logr.Logger) context.Context {
	return logf.IntoContext(context.Background(), logger)
}

// respondFunc sends a reply on a NATS request. Injected so respondingHandler is
// unit-testable without a live server.
type respondFunc func(m *nats.Msg, data []byte) error

// respondingHandler is envelopeHandler's request-reply sibling: it decodes the
// envelope, cross-checks the claimed session against the authorized subject,
// runs fn, and ALWAYS replies.
//
// Replying on every path is load-bearing, not defensive. A handler that returns
// without responding leaves the requester blocked until its timeout and gives
// the user a spinner instead of a reason — precisely the class of silent
// failure AGENTS.md forbids. Decode failures, subject refusals, and handler
// errors are logged AND returned in the reply's Error field. A refused request
// is the one case where the reply matters most and the work must NOT happen:
// the caller learns why, and fn never runs. It is a package-level function (not
// a closure over envelopeHandler's fn shape) so it is directly unit-testable
// without a live NATS connection.
func respondingHandler(
	logger logr.Logger,
	name, handler string,
	fn func(context.Context, channelevents.Envelope) (channelevents.ViewMessageResultPayload, error),
	respond respondFunc,
) func(*nats.Msg) {
	ctx := handlerContext(logger)
	reply := func(m *nats.Msg, res channelevents.ViewMessageResultPayload) {
		b, err := json.Marshal(res)
		if err != nil {
			logger.Error(err, name+": marshal reply")
			// Last resort: a hand-rolled minimal reply beats silence.
			b = []byte(`{"outcome":"internal_error","error":"marshal reply failed"}`)
		}
		if err := respond(m, b); err != nil {
			logger.Error(err, name+": respond")
		}
	}
	return func(m *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			logger.Error(err, name+": decode envelope")
			reply(m, channelevents.ViewMessageResultPayload{
				Outcome: "internal_error", Error: name + ": decode envelope: " + err.Error(),
			})
			return
		}
		if reason, ok := inboundSubjectAuthorized(logger, name, m, env); !ok {
			reply(m, channelevents.ViewMessageResultPayload{Outcome: "internal_error", Error: reason})
			return
		}
		res, err := fn(ctx, env)
		if err != nil {
			logger.Error(err, name+": "+handler, "session", env.Session)
			if res.Error == "" {
				res.Error = err.Error()
			}
			if res.Outcome == "" {
				res.Outcome = "internal_error"
			}
		}
		reply(m, res)
	}
}

func run(rootCtx context.Context, cfg *config) error {
	// CRITICAL: controller-runtime's SetLogger is first-write-wins. A
	// transitive dep's init() (today: pkg/authz/spicedb's
	// authzed/spicedb/internal/logging) fulfils the delegating root
	// logger with a no-op sink BEFORE main runs, so our subsequent
	// ctrl.SetLogger is a silent no-op and every ctrl.Log.WithName()
	// .Info() goes to /dev/null — exactly the "zero log output from a
	// busy pod" symptom we observed. Mirror internal/cmd/operator/main.go: keep
	// an explicit reference to OUR zap logger and pass it through ctx
	// via logf.IntoContext so log.FromContext returns it everywhere
	// (relay, listeners, watchdog), independent of the broken global.
	zapLogger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr))
	ctrl.SetLogger(zapLogger) // best-effort; usually a no-op due to the race above
	logger := zapLogger.WithName("channelsd")
	logger.Info("channelsd: logger initialised")

	// LLM provider for the credential-request explainer (γ2/γ3). Declared
	// as the interface type so the variable is a true nil interface when the
	// key is absent — never a typed-nil pointer (see AGENTS.md). When the
	// key file is missing or empty channelsd starts in degraded mode; γ3's
	// explainer call falls back to the operator-stamped static explanation.
	llmProvider, llmErr := loadLLMProvider(cfg.anthropicKeyPath)
	if llmErr != nil {
		logger.Info("LLM explainer disabled — Anthropic API key not configured",
			"path", cfg.anthropicKeyPath, "reason", llmErr.Error())
	} else {
		logger.Info("LLM explainer enabled", "provider", llmProvider.Name())
	}

	if cfg.natsURL == "" || cfg.opMemoryURL == "" || cfg.spicedbEnd == "" {
		return fmt.Errorf("required flags missing: --nats-url=%q --operator-memory-url=%q --spicedb-endpoint=%q",
			cfg.natsURL, cfg.opMemoryURL, cfg.spicedbEnd)
	}

	// Inject our explicit logger into the (SIGINT/SIGTERM-canceled) root
	// context so every log.FromContext(ctx) call downstream sees it,
	// bypassing ctrl.Log's broken delegate.
	rootCtx = logf.IntoContext(rootCtx, logger)

	// Kubernetes client.
	restCfg, err := kube.RestConfig()
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	scm := makeScheme()
	cli, err := client.New(restCfg, client.Options{Scheme: scm})
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}
	// Typed clientset for externalurl.Provider — see the doc on
	// externalurl.NewProviderFor. We avoid controller-runtime's
	// cached client here to keep the configmaps RBAC name-scoped.
	csClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}

	// SpiceDB client.
	spToken, _ := os.ReadFile(cfg.spicedbTok)
	az, err := spicedb.NewClient(cfg.spicedbEnd, strings.TrimSpace(string(spToken)), cfg.insecureGRPC)
	if err != nil {
		return fmt.Errorf("spicedb client: %w", err)
	}
	defer az.Close()

	// NATS connection (reconnects indefinitely).
	nc, err := apnats.ConnectFromEnv(cfg.natsURL, "channelsd")
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Drain() //nolint:errcheck // best-effort drain on shutdown

	// Operator memory HTTP client. channelsd mints an Ed25519 signing key
	// at startup and signs its append-only writes (turn, …) as
	// "system:channelsd"; the matching public key is registered with the
	// operator after the dependency wait below, before any write.
	memTok, memTokErr := os.ReadFile(cfg.memTokenPath)
	if memTokErr != nil {
		// A missing/unreadable token otherwise surfaces only as a downstream
		// auth failure on the first memory POST; log the root cause here so
		// it's greppable (mirrors operator/main.go's token-read logging).
		if os.IsNotExist(memTokErr) {
			logger.Info("channelsd memory token not present; memory POSTs will fail until it appears", "path", cfg.memTokenPath)
		} else {
			logger.Error(memTokErr, "reading channelsd memory token", "path", cfg.memTokenPath)
		}
	}
	memTokStr := strings.TrimSpace(string(memTok))
	memPub, memPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("mint channelsd signing key: %w", err)
	}
	mem := newMemoryClient(cfg.opMemoryURL, memTokStr, memPriv)

	// Asset fetcher: resolves OutboundUserMessagePayload.Attachments[i] to
	// bytes via the operator's /artifact/{ns}/{sess}/{render}/output route.
	// Same operator base URL + same channelsd-system token as the memory
	// client (the operator validates against IsChannelsdToken for both).
	assetFetcher := assetfetch.New(assetfetch.Config{
		OperatorURL: cfg.opMemoryURL,
		Token:       memTokStr,
	})

	// Wait for dependencies: operator memory /healthz + SpiceDB ReadSchema.
	if err := waitForDeps(rootCtx, mem.Healthz, az.Ping, logger); err != nil {
		return fmt.Errorf("dependencies: %w", err)
	}
	logger.Info("dependencies ready")

	// Register the channelsd signing key with the operator. This MUST
	// succeed before any signed write: with verify-on-write enabled, an
	// unregistered key means every append-only write is rejected. Fail
	// closed (return) rather than publish unsigned/unverifiable turns.
	memKeyID := provenance.KeyID(memPub)
	if err := mem.client.RegisterPublisherKey(rootCtx, memKeyID, memPub); err != nil {
		return fmt.Errorf("register channelsd publisher key (keyID=%s): %w", memKeyID, err)
	}
	logger.Info("registered channelsd publisher key", "keyID", memKeyID)

	// Restart-marker signer, over the SAME key just registered above. The
	// operator refuses a status.pendingRestart marker it cannot attribute to
	// channelsd, because the target session's own runner can write that field
	// too and takeover mode skips the SpiceDB fork gate (pkg/agent/restartmarker).
	// Reusing the registered publisher key means no second key, no second
	// registration, and no second trust root: the operator resolves it through
	// the publisher-key registry it already loads for memory provenance.
	//
	// Constructed AFTER RegisterPublisherKey deliberately — a signer whose key
	// the operator has never seen would sign markers that are all refused.
	markerSigner := restartmarker.NewSigner(memPriv, restartmarker.Publisher)

	// Capabilities thunk: used by the pipeline to annotate new sessions.
	caps := func(kind string) []string {
		k, ok := registry.Get(kind)
		if !ok {
			return nil
		}
		return k.Capabilities()
	}

	// Inbound pipeline: authz + memory + NATS publish + session correlation.
	// NewPipeline constructs Engine internally from the Authz arg.
	pl := pipeline.NewPipeline(cli, az, mem, &natsPub{nc}, caps)

	// The durable memory facade. Two consumers: the resource-owner decision
	// gate reads the memapproval record back after a restart, and the parked
	// prompts a session is blocked on are recorded + re-surfaced through it.
	// The outbound relay below writes those prompts and the pipeline reads them,
	// so both share this facade. Correctness does not depend on the sharing: the
	// record is durable and survives this process entirely.
	pl.Mem = mem.MemoryV2()
	pl.MarkerSigner = markerSigner

	// The user_preference_confirm decision handler's committer: the same
	// operator memory HTTP client channelsd already holds (component token,
	// no per-write signing needed — CommitPreference is not an append-only
	// write). Satisfies pipeline.PreferenceCommitter's one method.
	pl.PreferenceCommitter = mem.client

	// Wire RestartCapable kinds. No kind implements it yet, so this loop is
	// currently a no-op.
	restartSubmit := pipeline.NewRestartPublisher((&natsPub{nc}).Publish, cli, markerSigner)
	for _, k := range registry.All() {
		if rk, ok := k.(channelkinds.RestartCapable); ok {
			rk.RegisterRestartTrigger(restartSubmit)
		}
	}

	// Wire metaagent bot user-id for mention routing (F1/F4). When
	// METAAGENT_SLACK_BOT_USER_ID is set, app_mention events targeting
	// that bot are routed to authzd rather than the session runner.
	// Empty disables the feature cleanly — all mentions route as before.
	if metaagentBotUserID := os.Getenv("METAAGENT_SLACK_BOT_USER_ID"); metaagentBotUserID != "" {
		if k, ok := registry.Get(slackkind.KindName); ok {
			if sk, ok := k.(*slackkind.Kind); ok {
				sk.SetMetaagentBotUserID(metaagentBotUserID)
				logger.Info("metaagent: configured Slack mention routing",
					"botUserID", metaagentBotUserID)
			}
		}
	}

	// Wire thread-adoption backfill: resolve the bound channel kind and,
	// when it implements channelkinds.ConversationReader, fetch history.
	// Kinds without the capability return an empty page so the pipeline
	// silently skips backfill.
	pl.ReadHistory = func(
		ctx context.Context,
		ch *spiceboxv1alpha1.Channel,
		channelKey string,
		opts channelkinds.ReadHistoryOpts,
	) (channelkinds.HistoryPage, error) {
		sec, k, err := resolve.ForChannel(ctx, cli, ch)
		if err != nil {
			return channelkinds.HistoryPage{}, fmt.Errorf("resolve channel for history: %w", err)
		}
		cr, ok := k.(channelkinds.ConversationReader)
		if !ok {
			// Kind has no retrievable history — not an error; no backfill.
			return channelkinds.HistoryPage{}, nil
		}
		return cr.ReadHistory(ctx, channelkinds.Deps{
			Channel:   ch,
			Secret:    sec,
			K8sClient: cli,
		}, channelKey, opts)
	}

	// History responder: serves the runner's read_thread_history tool
	// over NATS request/reply, reusing the same ReadHistory func.
	historyResponder := &historyresp.Responder{K8s: cli, ReadHistory: pl.ReadHistory}
	if err := historyResponder.Start(rootCtx, nc); err != nil {
		return fmt.Errorf("history responder start: %w", err)
	}

	// Channel-wide history responder: serves read_channel_history, enforcing
	// opt-in + channel-scope + the info-leakage authz matrix server-side.
	channelHistoryResponder := &historyresp.ChannelResponder{K8s: cli, Authz: az, Clock: clock.RealClock{}}
	if err := channelHistoryResponder.Start(rootCtx, nc); err != nil {
		return fmt.Errorf("channel-history responder start: %w", err)
	}

	// Wire the bento InboundSink: every bento `forward_to_pipeline`
	// output message gets adapted to a channelkinds.InboundEvent and
	// delivered through the same pipeline as slack/fake inbounds.
	// Bento has no per-user attribution (UserAttributable() == false),
	// so the Channel CR's spec.authzSubject is the SpiceDB subject the
	// session attributes work to; the adapter forwards it on the
	// InboundEvent for the pipeline's no-user-identity branch to honor.
	bento.SetInboundSink(&bentoInboundSink{
		k8s:  cli,
		pipe: pl,
		log:  logger.WithName("bento-sink"),
	})
	defer bento.SetInboundSink(nil)
	// Wire the GrantWriter so the tool_approval interaction handler
	// (BindToolApprovalHandler, bound below) can write per-(session, tool, args)
	// SpiceDB grant tuples on approve. Assigned
	// as a concrete (not interface) to avoid a typed-nil interface
	// hiding behind the field's `!= nil` guard if az is ever absent
	// (see AGENTS.md "Nil interfaces: never assign a typed-nil pointer").
	if gw := spicedb.NewGrantWriter(az.Writer(pipeline.GrantSource)); gw != nil {
		pl.GrantWriter = gw
	}

	// metaagent_scope_approval (F2): published by authzd when the metaagent
	// orchestrator needs human approval for a dynamic scope change. Renders
	// a three-region Block Kit message in the session's Slack thread.
	// metaagent_notice (F3): published by authzd for simple ephemeral notices
	// (applied/denied acknowledgments, CannotAddress messages) to the
	// session requester via chat.postEphemeral.
	//
	// These subjects carry raw JSON payloads (not channelevents Envelopes),
	// so they are handled via direct subscriptions rather than the outbound
	// relay. Only active when the session is bound to a Slack channel; other
	// kind sessions see the payload dropped (no Slack client resolved).
	maHandlers := &metaagentHandlers{cli: cli, nc: nc}
	// Wire the registered Slack Kind so handleScopeApproval can record posted
	// approval blocks into the shared metaagentApprovalRefs cache that the
	// listener's Show Details handler reads.
	if k, ok := registry.Get(slackkind.KindName); ok {
		if sk, ok := k.(*slackkind.Kind); ok {
			maHandlers.slackKind = sk
		}
	}

	// Subject subscriptions. The envelope-shaped rows share the decode+dispatch
	// wrapper (envelopeHandler); the three trailing rows carry raw JSON payloads
	// (not channelevents Envelopes) and bind their custom handlers directly.
	//
	// Every row wildcards the two session tokens and composes the SAME leaf
	// builder its publisher calls, so a pattern here and the subject it must
	// match cannot drift. That is also why each handler re-derives (ns, name)
	// from the message's own subject: the wildcard means the subject is the
	// only session identity NATS authorized.
	anySession := channelevents.AnySessionPrefix()
	subs := []struct {
		name    string // log prefix + error-wrap label
		subject string
		handler func(*nats.Msg)
	}{
		// There are deliberately no per-category subscriptions here. Every
		// approval flow — session-join, tool_approval, info_leakage — travels as
		// a category-generic interaction_request / _decision / _applied
		// (subscribed below) plus a bound handler: decidePermission for
		// session-join, BindToolApprovalHandler for tool_approval (it
		// side-effects the grant on approve), and the pure
		// ApprovalDecisionHandler for info_leakage.
		// view_message: published by a browser/TUI *view* of a session (webd
		// chat, the artifact viewer, `oap agent chat`). Unlike every other
		// inbound kind this is REQUEST/REPLY — the caller has no listener
		// behind it and needs the InboundDecision back to render a deny
		// message. channelsd is the sole inbound writer because it is the only
		// component with a write-capable memory token.
		{"view_message", channelevents.SubjectIn(anySession, channelevents.KindViewMessage),
			respondingHandler(logger, "view_message", "HandleViewMessage", pl.HandleViewMessage,
				func(m *nats.Msg, b []byte) error { return m.Respond(b) })},
		// interaction_decision: published by a surface (channel kind listener,
		// webchat route, view interact, hosted decision page) when the user picks
		// an interaction action. Category-generic: the pipe validates fail-closed
		// per the category's DeciderPolicy, invokes the bound decision handler,
		// and publishes Applied on both IN (runner resume) and OUT (surface ack).
		{"interaction_decision", channelevents.SubjectIn(anySession, channelevents.KindInteractionDecision),
			envelopeHandler(logger, "interaction_decision", "HandleInteractionDecision", pl.HandleInteractionDecision)},
		// interaction_request on IN: published by the runner's migrated approval
		// gates (content_inspection in C1) to park a generic interaction. The
		// category-generic park handler writes/dedups PendingInteractions and
		// re-emits on OUT for the generic renderer. It writes NO phase condition —
		// phase derives from the runner's signed lifecycle projection.
		{"interaction_request", channelevents.SubjectIn(anySession, channelevents.KindInteractionRequest),
			envelopeHandler(logger, "interaction_request", "HandleInteractionRequest", pl.HandleInteractionRequest)},
		// interaction_applied on IN: synthetic publishes from the runner's
		// gate-side timeout watcher (Outcome=expired). Clears the matching durable
		// PendingInteractions entry so the park record doesn't leak.
		{"interaction_applied", channelevents.SubjectIn(anySession, channelevents.KindInteractionApplied),
			envelopeHandler(logger, "interaction_applied", "HandleInteractionApplied", pl.HandleInteractionApplied)},
		// resurface_request on IN: published by a VIEW of the session the moment
		// it attaches (a chat tab's websocket, the TUI's outbound relay). Parked
		// prompts are delivered by a one-shot live publish that is never re-sent,
		// so a surface attaching afterwards would otherwise show a parked session
		// with nothing on screen. The handler runs the same resurfacePending an
		// inbound message triggers — the machinery was already here; only this
		// non-message trigger was missing.
		{"resurface_request", channelevents.SubjectIn(anySession, channelevents.KindResurfaceRequest),
			envelopeHandler(logger, "resurface_request", "HandleResurfaceRequest", pl.HandleResurfaceRequest)},
		// agent_message_send on IN: every session-to-session message, in either
		// direction, published by the SENDING session on its OWN subject and
		// naming its destination in the payload. Both producers publish it — a
		// runner's reply_to_subagent, and the `agent` channel kind's Sender
		// (pkg/channels/channelkinds/agent), which runs in this process. A
		// runner has no other option: its per-session NATS grant cannot publish
		// onto another session's subject. The Sender does, holding this
		// process's cluster-wide credential, and declines it — addressing the
		// destination's subject would make the SENDER a payload claim, where
		// here inboundSubjectAuthorized (above) authenticates it against the
		// subject the publisher was authorized on.
		//
		// The `agent` kind has no Listener of its own to subscribe with —
		// channelkinds.Deps grants a kind only NATSPublish/NATSRequest, never a
		// subscribe capability — so, like every other wildcard row here,
		// channelsd is the sole subscriber: the one component holding both a
		// cluster-wide NATS credential and a write-capable memory token.
		{"agent_message_send", channelevents.SubjectIn(anySession, channelevents.KindAgentMessageSend),
			envelopeHandler(logger, "agent_message_send", "HandleAgentMessageSend", pl.HandleAgentMessageSend)},
		// metaagent_scope_approval / metaagent_notice carry raw JSON payloads, so
		// they bind maHandlers' custom handlers directly (not the envelope wrapper).
		//
		// These MUST get a context carrying the live logger. They log via
		// ctrllog.FromContext(ctx), and the delegating root logger is fulfilled
		// with a no-op sink by a transitive init() (see the SetLogger race at the
		// top of run), so a bare context.Background() sends EVERY line they emit,
		// including all their drop paths, to /dev/null. That is how a dropped
		// deny-notice presented as complete silence with no trace at all. rootCtx
		// carries our zap logger via logf.IntoContext; the wrappers above carry the
		// same logger via handlerContext, which additionally keeps its base
		// non-cancelling so a shutdown cannot cut an inbound write short.
		{"metaagent_scope_approval", channelevents.SubjectOut(anySession, channelevents.KindMetaagentScopeApproval),
			func(m *nats.Msg) { maHandlers.handleScopeApproval(rootCtx, m) }},
		{"metaagent_notice", channelevents.SubjectOut(anySession, channelevents.KindMetaagentNotice),
			func(m *nats.Msg) { maHandlers.handleNotice(rootCtx, m) }},
	}
	for _, s := range subs {
		if _, err := nc.Subscribe(s.subject, s.handler); err != nil {
			return fmt.Errorf("subscribe %s: %w", s.name, err)
		}
	}

	// Verified webhook deliveries from webd. QueueSubscribe, not Subscribe (so
	// it cannot live in the subs table above): this pod may be one of several
	// replicas and each delivery must create exactly one session, not one per
	// replica.
	if _, err := nc.QueueSubscribe(channelevents.WebhookInboundSubject, channelevents.WebhookInboundQueueGroup,
		func(m *nats.Msg) { webhookInboundHandler(rootCtx, cli, pl, logger, m) }); err != nil {
		return fmt.Errorf("subscribe %s: %w", channelevents.WebhookInboundSubject, err)
	}

	// Status watchdog: tracks "last setStatus emission" per session and
	// fires warning/timeout messages when the agent goes silent. Listeners
	// and Senders call Touch on every successful setStatus; the outbound
	// relay folds every status envelope (tool_activity, turn_activity,
	// setStatus notifications, plan_update) into the same machine via
	// ApplyEvent / OnTurnActivity, which both re-arms the silence window and
	// gates stale/post-yield captions.
	sr := newSenderResolver(cli, nc, assetFetcher)
	wd := newStatusWatchdog(cli, sr)
	// The watchdog's stall notice rides the bus like every other notice, so the
	// outbound relay resolves the sender and binding. Without a publish func it
	// has nowhere to put the notice and logs instead — loud, but the user still
	// hears nothing about a stalled session.
	wd.publish = func(subject string, data []byte) error { return nc.Publish(subject, data) }
	sr.SetWatchdog(wd)
	sr.SetAuthzReader(az)
	sr.SetApproverFanoutLimit(cfg.approverFanoutLimit)
	// Wire the AudienceResolver on the registered Slack Kind so the
	// information-leakage gate can enumerate channel members at respond_to_user
	// time. The registry holds the same *slack.Kind pointer registered via
	// init(), so SetAudienceResolver here affects all future Sender/Listener
	// construction through that shared pointer.
	if k, ok := registry.Get(slackkind.KindName); ok {
		if sk, ok := k.(*slackkind.Kind); ok {
			sk.SetAudienceResolver(az)
		}
	}
	go wd.Run(rootCtx)

	// Outbound relay: subscribes to ap.session.*.*.out.> and dispatches to Senders.
	// Status envelopes (tool_activity, turn_activity, setStatus notifications,
	// plan_update) are folded into the silence-watchdog machine via ApplyEvent /
	// OnTurnActivity instead of (or before) reaching a Sender, so stale/post-yield
	// captions are gated from one ordered place.
	relay := &outbound.Relay{
		NC:               nc,
		K8s:              cli,
		Senders:          sr,
		ApplyStatusEvent: wd.ApplyEvent,
		OnTurnActivity:   wd.OnTurnActivity,
		Mem:              mem.MemoryV2(),
		// channelsd owns Channel liveness conditions (Connected, ScopesValid),
		// so it is the one relay that also records send outcomes as the
		// Deliverable condition; webd's chat relay and `oap agent chat` stay
		// read-only on cluster state.
		RecordDeliverability: true,
	}
	if err := relay.Start(rootCtx); err != nil {
		return fmt.Errorf("relay start: %w", err)
	}
	defer relay.Stop(rootCtx) //nolint:errcheck

	// Interrupt-applied bridge: subscribes to .out.interrupt_applied and, for
	// SLACK-input sessions only, republishes the runner's interrupt outcome as
	// interaction_applied(queued_messages) so the generic interaction sender
	// resolves the "Interrupt & Send Now" card in place. The async other half of
	// pkg/channels/channelsd/pipeline/queued_interrupt.go's Suppressed decision handler.
	// browser/local sessions consume interrupt_applied directly (this bridge
	// no-ops for them — the double-fire guard).
	interruptBridge := newInterruptAppliedBridge(cli, nc)
	if err := interruptBridge.Start(rootCtx); err != nil {
		return fmt.Errorf("interrupt-applied bridge start: %w", err)
	}
	defer interruptBridge.Stop(rootCtx) //nolint:errcheck

	// Channel manager: polls Channel CRs, starts/stops per-Channel Listeners,
	// and patches Channel.status.conditions[Connected]. The portalMinter
	// is set later (after the signing key + externalurl Provider are
	// constructed); a nil minter degrades to "no Manage button on App
	// Home" rather than failing the listener.
	mgr := newChannelManager(cli, nc, mem, mem.MemoryV2(), mem.Preferences(), personalizableClassLookup{az: az}, pl, wd, az, nil)
	go mgr.Run(rootCtx)
	defer mgr.Stop(logger)

	// Subscribe to channelevents.SessionAttachedSubject: published by the
	// outbound relay after a successful OutputChannel write-back patch.
	// Dispatched to the listener owning the named OutputChannel iff that
	// listener implements channelkinds.SessionWatcher (slack does; other
	// kinds opt in by implementing the interface).
	if _, err := nc.Subscribe(channelevents.SessionAttachedSubject, func(m *nats.Msg) {
		var ev channelevents.SessionAttached
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			logger.Info("session_attached: decode payload",
				"err", err.Error(), "len", len(m.Data))
			return
		}
		mgr.HandleSessionAttached(rootCtx, ev)
	}); err != nil {
		return fmt.Errorf("subscribe session_attached: %w", err)
	}

	// Watch AgentSessions for failure transitions and surface them to channels.
	// Uses the same senderResolver as the outbound relay — no duplication of
	// Channel/Secret fetch logic.
	sw := newSessionWatcher(cli, sr, wd)
	sw.publish = func(subject string, data []byte) error { return nc.Publish(subject, data) }
	go sw.Run(rootCtx)

	// Credential-request watcher: turns operator-parked passthrough
	// sessions (phase=AwaitingCredentials) into user-visible "Connect
	// your accounts" prompts. Mints a signed deep-link from
	// passthroughlink, delivers a KindCredentialRequest envelope on the
	// session's bound channel's credential_request sub-channel, and
	// stamps AgentSessionConditionCredentialRequestPublished=True to
	// dedup. When the signing-key path is unreadable OR the external
	// base URL is empty the watcher is started but immediately exits
	// (the user simply sees nothing — operator must wire both for
	// passthrough sessions to surface a prompt).
	signer, signerErr := loadPassthroughSigner(cfg.signingKeyPath)
	if signerErr != nil {
		// Don't fatal: a channelsd that's not using passthrough should
		// still come up cleanly. Log + leave signer nil so the watchers
		// disable themselves.
		//
		// This is now the NARROW residual case, not the common one: the
		// key's volume mount is non-optional (pkg/platform/manifests/channelsd/
		// deployment.yaml), so the kubelet refuses to start this pod at all
		// when the Secret is absent. Reaching here means the Secret exists
		// but its "key" entry is missing/unreadable — still worth a loud,
		// grep-able line naming EVERY feature that goes dark, not just the
		// first one (the credential_update watcher was silently omitted
		// from this message until the slice-1 final review).
		logger.Info("passthrough signing key unavailable; credential_request + credential_update watchers, "+
			"portal/App-Home links and artifact live-view links are all disabled",
			"path", cfg.signingKeyPath, "err", signerErr.Error())
	}
	// Construct the explainer from the LLM provider, if one was
	// successfully loaded. explainer.New returns nil when p is nil,
	// so we declare exp as the interface type to keep it a true nil
	// interface rather than a typed-nil pointer (see AGENTS.md).
	var exp explainer.Explainer
	if llmProvider != nil {
		exp = explainer.New(llmProvider)
	}
	// Live-update webd's externally reachable URL from its ConfigMap.
	// identityd's pages (/link, /my/accounts, /icon/) are served on webd's
	// trusted origin, so ALL passthrough deep-links — credential-request,
	// portal-access, App Home portal button, credential icon URLs — use this
	// single provider.
	// The "trusted-url" key is populated by the webd install task;
	// it may be absent until that task runs, in which case
	// credential-request + portal-access messages are silently skipped
	// (logged by the respective watcher/sender).
	webdURLProvider := externalurl.NewProviderFor(csClient, logger,
		spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdTrustedURLKey, "")
	go webdURLProvider.Run(rootCtx)

	// Set the channelManager's portal-link minter now that signer +
	// webdURLProvider exist. The slack App Home renderer uses this to
	// deep-link "Manage my connections" into identityd's /my/accounts
	// portal (now served on webd's trusted origin). The channelManager
	// was constructed earlier with a nil minter (the App Home button
	// was simply omitted in the gap between channelsd boot and signer
	// load); now the minter goes live for every subsequent
	// app_home_opened event.
	if signer != nil {
		mgr.SetPortalMinter(&channelsdPortalMinter{signer: signer, externalBaseURL: webdURLProvider.Get})
	}
	// Wire the webdURLProvider into the channelManager so the
	// Slack App Home renderer can compose per-credential icon URLs
	// (served on webd's trusted origin via identityd's /icon/ routes).
	// Done alongside SetPortalMinter so both reach the listener at
	// the same time.
	mgr.SetExternalBaseURL(webdURLProvider.Get)

	// Wire the artifact-view minter into BOTH the sender resolver (which posts
	// the "View live" button) and the channelManager/listener (whose click
	// handler mints a fresh deep-link per click). They MUST share one minter:
	// if only the sender is wired, the button posts but every click reports
	// "Live view isn't configured". Requires the signer + webd URL provider;
	// when the signer is nil (passthrough not configured) neither is wired and
	// the button is never posted (graceful degrade).
	if signer != nil {
		var artifactMinter channelkinds.ArtifactViewMinter = newArtifactViewMinter(signer, webdURLProvider.Get)
		sr.SetArtifactViewMinter(artifactMinter)
		mgr.SetArtifactViewMinter(artifactMinter)
	}

	// Wire the session-view minter into BOTH the sender resolver (which posts
	// the "Open interactive view" anchor) and the channelManager, mirroring
	// the artifact-view wiring above. Unlike the artifact-view minter this
	// needs no passthroughlink.Signer: the session-view page is a durable
	// plain-path link, not a signed capability — authorization happens at
	// open time via the page's own CheckInteract. So it is wired
	// unconditionally from the same webdURLProvider the artifact minter
	// uses; it resolves to a clean per-call skip until the webd trusted-URL
	// ConfigMap is populated.
	var sessionViewMinter channelkinds.SessionViewMinter = newSessionViewMinter(webdURLProvider.Get)
	sr.SetSessionViewMinter(sessionViewMinter)
	mgr.SetSessionViewMinter(sessionViewMinter)

	// Wire the agent-UI minter into the sender resolver only — deliberately
	// NOT into the channelManager. Of the two minters above, only
	// artifactViewMinter is actually read by a listener (the Slack live-view
	// click handler mints a fresh link per click); sessionViewMinter is on the
	// manager for parity alone, as that field's own comment in listeners.go
	// records. This minter does not follow that parity, because the agent-UI
	// offer has no click-time interaction at all — the sender bakes the
	// durable URL into the message at post time — and a collaborator that
	// arrives in the manager's wiring set restarts every listener, which would
	// buy nothing here.
	//
	// Declared as the interface type so an unassigned variable would stay a
	// true nil interface rather than a non-nil interface wrapping a nil
	// pointer. Wired unconditionally: like the session-view page, the shell
	// authorizes at open time, so there is no passthroughlink.Signer to wait
	// for; it resolves to a clean per-call skip until the webd trusted-URL
	// ConfigMap is populated.
	var agentUIMinter channelkinds.AgentUIMinter = newAgentUIMinter(webdURLProvider.Get)
	sr.SetAgentUIMinter(agentUIMinter)

	crw := &pipeline.CredentialRequestWatcher{
		K8s:             cli,
		Senders:         sr,
		LinkSigner:      signer,
		ExternalBaseURL: webdURLProvider.Get,
		Explainer:       exp,
		NATSPublish:     nc.Publish,
	}
	// Bind the credential_link regenerator: resurfacePending's category-generic
	// leg (Park: AwaitingCredentials, Resurface: ResurfaceRegenerate on the
	// credential_link category) finds the cached interaction_request and calls
	// this to re-mint + re-publish it when a re-interacting user gets ahead of
	// the original prompt. crw.ForcePublish bypasses the
	// CredentialRequestPublished dedup condition and shares doPublish's body
	// with the watcher's own first-publish path, so re-mint and first publish
	// stay one dispatch path.
	channelinteractions.BindRegenerator(categories.CredentialLink, func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, _ *channelevents.InteractionRequestPayload) error {
		return crw.ForcePublish(ctx, sess)
	})
	go crw.Run(rootCtx)

	// wcw publishes the workshop_credential card: it watches (polls) Workshops
	// for undelivered spec.credentialRequests entries the sidecar's
	// request_credential tool recorded, and delivers a signed
	// PurposeWorkshopCredential link to the builder session's requester. Reuses
	// the SAME signer + external-URL provider crw uses; Run's own nil-dep guard
	// (mirroring crw's) makes it a safe no-op when the passthrough signer isn't
	// configured. See pkg/channels/channelsd/pipeline/workshop_credential.go's
	// package doc for why this is a separate watcher rather than folded into
	// crw — the credential belongs to the workshop bot's own identity, not the
	// builder session's, and delivering it never parks the builder.
	wcw := &pipeline.WorkshopCredentialWatcher{
		K8s:             cli,
		LinkSigner:      signer,
		ExternalBaseURL: webdURLProvider.Get,
		NATSPublish:     nc.Publish,
	}
	go wcw.Run(rootCtx)

	// whw publishes the install_request / capability_request admin cards: it
	// watches (polls) Workshops for undelivered spec.installRequest /
	// spec.capabilityRequest entries the sidecar's handoff tools recorded, and
	// fans each out as a MonitoringEvent to every role=monitoring Channel —
	// the only mechanism that reaches platform admins cluster-wide, since
	// unlike wcw's credential prompt there is no single builder-session
	// requester to address. No LinkSigner: the card links to the admin
	// console, which is admin-authenticated on its own. See
	// pkg/channels/channelsd/pipeline/workshop_handoff.go's package doc.
	whw := &pipeline.WorkshopHandoffWatcher{
		K8s:             cli,
		ExternalBaseURL: webdURLProvider.Get,
		NATSPublish:     nc.Publish,
		Now:             time.Now,
	}
	go whw.Run(rootCtx)

	// cuw publishes the credential_update card: it watches (polls)
	// CredentialUpdateRequests the operator's credentialupdaterequest
	// reconciler has decided (phase=Open) and builds+mints+publishes the
	// card, reusing the SAME signer + external-URL provider crw uses. See
	// pkg/channels/channelsd/pipeline/credential_update.go's package doc for why this
	// lives here rather than in the operator.
	cuw := &pipeline.CredentialUpdateWatcher{
		K8s:             cli,
		LinkSigner:      signer,
		ExternalBaseURL: webdURLProvider.Get,
		NATSPublish:     nc.Publish,
		// Answers agentidentity#update_credential for the turn's author, which
		// is what decides whether an agent-owned credential's card ALSO renders
		// in-thread. az is non-nil here (spicedb.NewClient's error is checked
		// above), so this cannot become a typed-nil interface; leaving it unset
		// would fail closed, not open, but would silently cost every cluster
		// the in-thread route.
		Authz: az,
	}
	go cuw.Run(rootCtx)

	// Keep the thread caption of a session that has not started yet honest
	// about what is actually blocking it. Without this the only pre-runner
	// caption is crw's persistent "waiting for identity setup…", which nothing
	// retracts: a user who linked their accounts and then hit an unschedulable
	// sandbox was told to redo the one thing they had already done, while the
	// real blocker reached only the CR status and the monitoring channel.
	go (&pipeline.StartupStatusWatcher{K8s: cli, NATSPublish: nc.Publish}).Run(rootCtx)

	// Bind the identity_choice decision handler: the interaction_decision pipe
	// (HandleInteractionDecision, subscribed above) already validates standing
	// (DecideRequester — only the prompt's addressee may answer) and publishes
	// Applied on both .in (runner resume, via internal/cmd/runner's
	// subscribeInteractionApplied) and .out (surface ack); this handler only
	// validates the 3-way action and encodes it into Outcome.OutcomeText for the
	// runner to read back out (InteractionAppliedPayload has no Action field).
	channelinteractions.Bind(categories.IdentityChoice, pipeline.IdentityChoiceDecisionHandler)

	// Bind content_inspection: the interaction_decision pipe validates standing
	// (DecideApprovers — the session approve-set) before invoking this generic
	// approve/deny handler; the runner resumes its content-guard gate via the
	// interaction_applied bridge (subscribeInteractionApplied). No channelsd-side
	// grant is written here (unlike tool_approval) — content_inspection's approve
	// is a pure allow/deny the runner acts on.
	channelinteractions.Bind(categories.ContentInspection, pipeline.ApprovalDecisionHandler)

	// Plan gate: both categories are a pure allow/deny the RUNNER acts on — no
	// channelsd-side grant, unlike tool_approval. plan_phase clears a declared
	// phase to run; plan_amendment grants reach the approved plan lacks. Without
	// these bindings an approve is published, a human clicks it, and the decision
	// is dropped — the runner waits out the full approval timeout and the session
	// reads as hung.
	channelinteractions.Bind(categories.PlanPhase, pipeline.ApprovalDecisionHandler)
	channelinteractions.Bind(categories.PlanAmendment, pipeline.ApprovalDecisionHandler)

	// Bind tool_approval (Slice C2): the SIDE-EFFECTING handler writes the
	// SpiceDB grant tuple on approve. Standing is enforced FIRST by the
	// interaction_decision pipe's DecideResourceOwners policy (the clicker must
	// be an #owner of the tool's gated resource, resolved from the request's
	// Resources / the durable memapproval record) — so the unconditional grant
	// write here is safe. Depends on pl.GrantWriter (wired above) + pl.Mem
	// (wired above at pl.Mem = mem.MemoryV2() — the D3 cross-restart
	// fallback the handler + the pipe's resource-owner recovery read).
	pipeline.BindToolApprovalHandler(pl)

	// Bind data_slot_disclosure: a human ruling on whether one agent may hand a
	// specific datum to another it is delegating to.
	//
	// The handler records CONSENT on the SubagentRequest and writes no grant.
	// The binding is the operator's, re-graded against live state on its next
	// pass — channelsd learns that someone said yes, and must not decide
	// whether the parent still has standing, whether the child is still the
	// one that asked, or whether the slot has since been re-pointed. Depends
	// on pl.K8s (the status write) and pl.Mem (the cross-restart details
	// fallback).
	pipeline.BindDataSlotDisclosureHandler(pl)
	// Bind precondition_waiver: the SIDE-EFFECTING handler binds a SLOT grant on
	// approve exactly as tool_approval does — approving a waiver card IS the
	// precondition waiver (authz.BindApproved skips its precondition filter under
	// PreconditionsWaived, by design). The dispatch hook raises this card when a
	// slot precondition is Refused (toolcallauthz needsApproval → BuildWaiverAsk;
	// Undetermined and Unevaluatable verdicts stay tool results, never a card).
	// Click authorization depends on how the precondition routes: a rule that
	// declares its own approvers leaves the card's Resources empty so the click
	// folds to CheckApprove(agentsession#approve) — the set it was delivered to;
	// otherwise the resource #owner gate applies. Depends on pl.Authz + pl.Mem
	// (both wired above), the same deps tool_approval's handler reads.
	pipeline.BindPreconditionWaiverHandler(pl)

	// Bind info_leakage (Slice C2): the PURE approve/deny handler — its grant is
	// written RUNNER-side (leakagePostApprove, driven via the interaction_applied
	// bridge), so channelsd only records the decision. Standing is data-owner-only:
	// the pipe's DecideResourceOwners policy gates on the taint data #owner
	// (the request's Resources).
	channelinteractions.Bind(categories.InfoLeakage, pipeline.ApprovalDecisionHandler)

	// Bind the permission_request decision handler: the interaction_decision
	// pipe already validates standing (DecideOwner — the session's approve-set)
	// before invoking it; decidePermission's own job is just to apply the
	// decision: grant interact on approve, and clear the durable
	// PendingRequesters entry either way.
	pipeline.BindPermissionHandler(pl)

	// Bind the start_approval decision handler: the pipe validates standing
	// (DecidePlatformAdmin — platform#start_session) before invoking it;
	// decideStartApproval applies the verdict on a guest-started parked
	// session: approve writes started_by + the interact policy and removes
	// the parking marker; deny writes the denied relation + StartFailure.
	pipeline.BindStartApprovalHandler(pl)

	// Bind the provider_error_retry decision handler: the interaction_decision
	// pipe already validates standing (DecideParticipant — any user with
	// interact standing on the session, fail-closed) before invoking it;
	// decideProviderRetry's own job is just to wake the parked runner
	// (annotateWake). Slice B migrated provider_error_retry off its bespoke
	// KindProviderErrorRetryRequested listener path onto this generic path.
	pipeline.BindProviderRetryHandler(pl)

	// Bind the queued_messages decision handler: the interaction_decision pipe
	// already validates standing (DecideParticipant — any user with interact
	// standing on the session, fail-closed via CheckInteract) before invoking it;
	// decideQueuedInterrupt fires a KindInterruptRequest at the runner and returns
	// Suppressed (the async interrupt-applied bridge resolves the card later).
	pipeline.BindQueuedInterruptHandler(pl)

	// Bind the session_release decision handler: the interaction_decision pipe
	// already validates standing (DecideOwner — the session's approve-set)
	// before invoking it; decideSessionRelease's own job is just to apply the
	// decision — it delegates to pkg/controllers/sessionhold.Reconciler.Decide,
	// which clears standing plan approvals BEFORE releasing on approve, and
	// leaves the hold Active on refuse. Without this binding a release card
	// publishes, an owner clicks it, and the click is dropped — the hold can
	// never be released and its only remaining exit destroys the evidence it
	// exists to preserve.
	pipeline.BindSessionReleaseHandler(pl)

	// Bind the user_preference_confirm decision handler: the interaction_decision
	// pipe already validates standing (DecideRequester — only the turn author
	// this card was addressed to may decide it) before invoking it;
	// preferenceCommitHandler's own job is to commit the (key, value) pair
	// channelsd itself cached at publish time to the operator's preferences
	// store, under the verified decider's own canonical subject, on approve —
	// and nothing on deny. Depends on pl.PreferenceCommitter (wired above) and
	// pl.Mem (wired above, the cross-restart details fallback).
	pipeline.BindPreferenceCommitHandler(pl)

	// Portal-access chat trigger: intercepts "manage my accounts" /
	// "link my accounts" / "!my/accounts" on inbound user messages,
	// mints a 10-minute portal-purpose deep-link, and publishes an
	// interaction_request(portal_access) envelope on the session's .out
	// subject — the agent never sees the trigger phrase. Same signer +
	// webdURLProvider as the credential_request watcher; NATS reuses the
	// pipeline's own publisher (pl.NATS) rather than a fresh &natsPub{nc},
	// because delivery goes through the same publish path credential_request
	// uses. Senders is kept only for callers that still construct one.
	pl.PortalAccess = &pipeline.PortalAccessTriggerer{
		LinkSigner:      signer,
		ExternalBaseURL: webdURLProvider.Get,
		Senders:         sr,
		NATS:            pl.NATS,
	}

	// Credential-linked watcher: the out-of-band "X just linked"
	// confirmation envelope. Polls UserIdentity, diffs each user's
	// spec.credentials against an in-memory snapshot, and publishes a
	// KindInteractionApplied(credential_link, resolved) envelope to the
	// user's most recent AgentSession's .out subject when a credential is
	// added or replaced. Pairs with the credential_request watcher's
	// single-use signed links to give legitimate users a real-time
	// confirmation (and a detective signal if a forwarded link was used by
	// someone else) when OIDC is off. See
	// pkg/channels/channelsd/pipeline/credential_linked.go.
	clw := &pipeline.CredentialLinkedWatcher{
		K8s:          cli,
		Senders:      sr,
		NATSPublish:  nc.Publish,
		PollInterval: pipeline.CredentialLinkedWatcherInterval,
	}
	go clw.Run(rootCtx)

	// Join-request timeout watcher: expires PendingRequesters past
	// AgentClass.spec.approvalTimeout (default 15m) and clears the
	// PermissionRequestPending condition + notifies the requester. Joins are
	// decided pre-runner (session Idle), so channelsd owns their TTL. The
	// blocking approvals (tool_call / leakage / content) time out in the
	// runner's orchestrator, which publishes their deny/timeout applied
	// envelopes directly. Without this, a forgotten join strands forever.
	jrw := newJoinRequestTimeoutWatcher(cli, pl)
	go jrw.Run(rootCtx)

	// Monitoring relay: subscribes to ap.monitoring.events and fans each
	// framework MonitoringEvent out to every role=monitoring Channel.
	monRelay := newMonitoringRelay(cli, nc)
	if err := monRelay.Start(rootCtx); err != nil {
		return fmt.Errorf("monitoring relay start: %w", err)
	}
	defer monRelay.Stop(rootCtx) //nolint:errcheck

	logger.Info("channelsd ready")
	<-rootCtx.Done()
	logger.Info("channelsd shutting down")
	return nil
}

// loadLLMProvider reads an Anthropic API key from path and returns a
// non-nil llm.Provider on success. Returns (nil, err) when the file is
// missing, unreadable, or empty — callers log + continue with a nil
// provider (degraded mode). The return type is the interface, not the
// concrete *anthropic.Provider, so the caller's variable stays a true
// nil interface rather than a typed-nil pointer (see AGENTS.md).
func loadLLMProvider(path string) (llm.Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key from %q: %w", path, err)
	}
	key := string(bytes.TrimSpace(data))
	if key == "" {
		return nil, fmt.Errorf("key file %q is empty", path)
	}
	return anthropic.New(key), nil
}

// loadPassthroughSigner reads the hex-encoded HMAC key Secret mounted
// at path and returns a passthroughlink.Signer. Shares webd's loader
// contract via passthroughlink.DecodeHexKey: the file holds
// hex.EncodeToString of 32 random bytes, written verbatim by the Secret
// mount, so it is trimmed, hex-decoded, and checked against the shared
// minimum-length floor. An empty or too-short key is refused rather than
// yielding a guessable HMAC key that would let anyone forge the
// credential deep-links channelsd mints. Returns nil + error when the
// file is missing or invalid — callers log and continue with a disabled
// watcher; channelsd must not crash just because the operator hasn't
// finished passthrough wiring.
func loadPassthroughSigner(path string) (*passthroughlink.Signer, error) {
	rawHex, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key from %q: %w", path, err)
	}
	keyBytes, err := passthroughlink.DecodeHexKey(rawHex)
	if err != nil {
		return nil, fmt.Errorf("signing key from %q: %w", path, err)
	}
	// Channelsd is the link's issuer; identityd is the audience.
	// Defending against cookie-vs-deeplink confusion: identityd's own
	// idd_session cookie has iss=identityd, so a verifier that
	// expects iss=channelsd refuses to accept a cookie as a
	// deep-link (and vice versa).
	return passthroughlink.New(keyBytes,
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd),
	), nil
}
