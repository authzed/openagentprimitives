// Command authzd is the async authorization worker. It has two jobs.
//
// Per-turn entity extraction: it subscribes to KindUserMessage on NATS and
// runs entity extraction (LLM-driven) per user turn, writing results to the
// memory store as extracted_entity / extraction_state entries for binding
// autofill.
//
// Metaagent scope orchestration: it subscribes to in.metaagent_request and
// runs the staged cold-start / mid-session scope lifecycle, and it subscribes
// to in.metaagent_approval_applied, where it DOES check SpiceDB — the
// agentsession#manage_scope (= owner) gate — before routing an approve/deny
// decision to the approval orchestrator. SpiceDB is required: authzd refuses
// to start without SPICEDB_ENDPOINT rather than route decisions it cannot
// authorize.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"

	llmanthropic "github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	authzanthropic "github.com/authzed/openagentprimitives/pkg/authz/extract/anthropic"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the credkind.Kinds oap.DeriveInherited dispatches to via registry.Get when the pinning oap kind loads a .oap bundle
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/startup"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// config holds authzd's non-secret configuration, populated from flags and
// (via clikit) the environment. Secret values (MEMORY_TOKEN, SPICEDB_TOKEN,
// ANTHROPIC_API_KEY) are read directly from the environment in run() so they
// never appear in argv or --help.
type config struct {
	natsURL         string
	memoryURL       string
	spicedbEndpoint string
	spicedbInsecure bool
	sessionIdle     time.Duration
	extractTimeout  time.Duration
	extractorModel  string
	composerModel   string
}

func main() {
	// Before anything can compile a SpiceDB schema: its compiler logs a trace
	// line per definition through zerolog's process-global logger, and this
	// binary's stderr is `kubectl logs`. authzd's own logging is untouched.
	// See pkg/platform/deplogs.
	deplogs.Silence()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "authzd",
		Short:        "Async authorization worker: per-turn entity extraction + metaagent orchestration.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run(cmd.Context(), cfg)
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.natsURL, "nats-url", "", "NATS connection URL")
	fs.StringVar(&cfg.memoryURL, "memory-url", "", "Operator memory base URL")
	fs.StringVar(&cfg.spicedbEndpoint, "spicedb-endpoint", "", "SpiceDB gRPC endpoint (required: authzd enforces the manage_scope owner gate)")
	fs.BoolVar(&cfg.spicedbInsecure, "spicedb-insecure", false, "Use a plaintext (non-TLS) SpiceDB gRPC connection")
	fs.DurationVar(&cfg.sessionIdle, "session-idle", 10*time.Minute, "Idle timeout before a session's extraction state is finalized")
	fs.DurationVar(&cfg.extractTimeout, "extract-timeout", 10*time.Second, "Per-turn entity-extraction timeout")
	fs.StringVar(&cfg.extractorModel, "metaagent-extractor-model", "", "Anthropic model for metaagent entity extraction")
	fs.StringVar(&cfg.composerModel, "metaagent-composer-model", "", "Anthropic model for metaagent scope composition")
	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"nats-url":                  clikit.EnvNATSURL,
		"memory-url":                "MEMORY_URL",
		"spicedb-endpoint":          spicedb.EnvEndpoint,
		"spicedb-insecure":          spicedb.EnvInsecure,
		"session-idle":              "AP_AUTHZD_SESSION_IDLE",
		"extract-timeout":           "AP_AUTHZD_EXTRACT_TIMEOUT",
		"metaagent-extractor-model": "METAAGENT_EXTRACTOR_MODEL",
		"metaagent-composer-model":  "METAAGENT_COMPOSER_MODEL",
	})
	return cmd
}

// authzdDepCeiling bounds authzd's in-process retry on its startup dependencies
// (NATS, and the operator it POSTs its publisher key to). On ceiling authzd exits
// loud; CrashLoopBackoff is the last-resort backstop, not the first line.
const authzdDepCeiling = 2 * time.Minute

// publisherKeyRegistrar is the subset of the memory client authzd needs to
// register its signing key. Declared as an interface so the retry wrapper is
// unit-testable without a live operator. *httpclient.Client satisfies it.
type publisherKeyRegistrar interface {
	RegisterPublisherKey(ctx context.Context, keyID string, pub ed25519.PublicKey) error
}

// registerPublisherKeyWithRetry POSTs authzd's publisher key to the operator,
// retrying with bounded backoff so a not-yet-ready operator at startup does not
// crash authzd into CrashLoopBackoff. Exits loud (returns a terminal error) only
// after the ceiling.
func registerPublisherKeyWithRetry(ctx context.Context, reg publisherKeyRegistrar,
	keyID string, pub ed25519.PublicKey, ceiling time.Duration) error {
	return startup.Retry(ctx, "register publisher key with operator", ceiling,
		func(ctx context.Context) error { return reg.RegisterPublisherKey(ctx, keyID, pub) },
		func(attempt int, err error, next time.Duration) {
			slog.Info("authzd: operator not ready for publisher-key registration, backing off",
				"attempt", attempt, "keyID", keyID, "err", err.Error(), "retryIn", next.String())
		})
}

func run(ctx context.Context, cfg *config) {
	// The debug route is gated on the component's own memory token, which an
	// operator debugging this pod already has and no other pod does. Unset
	// means the route is not served at all — see healthState.handler for why
	// falling open is the thing to avoid here.
	hs := &healthState{debugToken: os.Getenv("MEMORY_TOKEN")}
	httpSrv := &http.Server{Addr: ":8080", Handler: hs.handler()}
	safehttp.HardenServer(httpSrv)
	go func() {
		// A bind failure (EADDRINUSE) leaves the healthz/readyz probes
		// permanently down; without this log+exit k8s would see a NotReady /
		// crash-loop pod with no diagnostic. ErrServerClosed is the expected
		// outcome of the graceful Shutdown below — not a failure.
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("authzd: probe server ListenAndServe failed", "addr", ":8080", "err", err)
			os.Exit(1)
		}
	}()

	var nc *nats.Conn
	if err := startup.Retry(ctx, "connect to NATS", authzdDepCeiling,
		func(context.Context) error {
			// ctx is intentionally unused: apnats.ConnectFromEnv takes no context.
			c, e := apnats.ConnectFromEnv(cfg.natsURL, "authzd")
			if e != nil {
				return e
			}
			nc = c
			return nil
		},
		func(attempt int, err error, next time.Duration) {
			slog.Info("authzd: NATS not ready, backing off",
				"attempt", attempt, "err", err.Error(), "retryIn", next.String())
		}); err != nil {
		slog.Error("authzd: nats connect failed after retry ceiling", "err", err)
		os.Exit(1)
	}
	hs.natsConnected.Store(true)
	defer nc.Drain()

	mem := httpclient.New(cfg.memoryURL, os.Getenv("MEMORY_TOKEN"))

	// authzd mints an Ed25519 signing key at startup and signs its
	// append-only writes (scope_audit, metaagent_audit) as "system:authzd".
	// The matching public key MUST be registered with the operator before
	// any write: with verify-on-write enabled, an unregistered key means
	// every append-only write is rejected. Fail closed (os.Exit) rather
	// than write unverifiable audit records.
	memPub, memPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		slog.Error("authzd: mint signing key", "err", err)
		os.Exit(1)
	}
	memKeyID := provenance.KeyID(memPub)
	if err := registerPublisherKeyWithRetry(ctx, mem, memKeyID, memPub, authzdDepCeiling); err != nil {
		slog.Error("authzd: register publisher key with operator failed after retry ceiling",
			"keyID", memKeyID, "err", err)
		os.Exit(1)
	}
	slog.Info("authzd: registered publisher key", "keyID", memKeyID)
	// signedMem attests authzd's append-only writes. Reads pass through to
	// the inner client unchanged. Every authzd writer of an append-only
	// Kind takes signedMem (writers of mutable kinds may also take it — a
	// no-op pass-through — so the whole write path is uniform).
	signedMem := provenance.NewSigningMemory(mem, provenance.NewSigner(memPriv, "system:authzd"))
	hs.deps.Memory = signedMem

	// authzd REQUIRES SpiceDB at startup: it must enforce the mid-session
	// manage_scope owner gate (agentsession#manage_scope = owner). Without
	// SpiceDB the gate cannot be evaluated, so authzd fails closed by refusing to
	// start (a crash-loop is the intended fail-closed signal that SPICEDB_ENDPOINT
	// is unconfigured). Per AGENTS.md's nil-interface rule, manageScopeChecker is
	// declared as the INTERFACE and only assigned the constructed concrete client,
	// never a typed-nil pointer.
	var manageScopeChecker authz.ManageScopeChecker
	spiceEndpoint := cfg.spicedbEndpoint
	if spiceEndpoint == "" {
		slog.Error("authzd: SPICEDB_ENDPOINT is unset; authzd cannot enforce manage_scope and will not start",
			"hint", "set SPICEDB_ENDPOINT/SPICEDB_TOKEN/SPICEDB_INSECURE")
		os.Exit(1)
	}
	spiceClient, err := spicedb.NewClient(spiceEndpoint, os.Getenv("SPICEDB_TOKEN"), cfg.spicedbInsecure)
	if err != nil {
		slog.Error("authzd: SpiceDB dial failed; authzd cannot enforce manage_scope and will not start",
			"endpoint", spiceEndpoint, "err", err.Error())
		os.Exit(1)
	}
	defer func() { _ = spiceClient.Close() }()
	manageScopeChecker = spiceClient

	var prov extract.Provider
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		llmProv := llmanthropic.New(k)
		prov = authzanthropic.New(llmProv, "")
	}

	w := NewWorker(WorkerDeps{
		Memory:         signedMem,
		Extractor:      prov,
		IdleTimeout:    cfg.sessionIdle,
		ExtractTimeout: cfg.extractTimeout,
	})

	subj := channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindUserMessage)
	sub, err := nc.QueueSubscribe(subj, "authzd", func(m *nats.Msg) {
		ns, name, ok := channelevents.ParseInSubject(m.Subject)
		if !ok {
			slog.Warn("authzd: user_message on an unparseable subject; dropping", "subject", m.Subject)
			return
		}
		scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
		_ = w.Handle(context.Background(), scope)
	})
	if err != nil {
		slog.Error("authzd: subscribe", "subject", subj, "err", err)
		os.Exit(1)
	}
	hs.subActive.Store(true)
	defer sub.Drain()

	// Metaagent pipeline: Extractor + Composer backed by the same llm provider
	// when ANTHROPIC_API_KEY is set; falls back to nil (CannotAddress on every
	// request) when the key is absent.
	var metaExtractor Extractor
	var metaComposer Composer
	var coldStartExtractor coldStartExtractorIface = NewColdStartExtractor(nil, "")
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		llmProv := llmanthropic.New(k)
		metaExtractor = NewAnthropicExtractor(llmProv, cfg.extractorModel)
		metaComposer = NewAnthropicComposer(llmProv, cfg.composerModel)
		coldStartExtractor = NewColdStartExtractor(llmProv, cfg.extractorModel)
	}

	// Hard-deny is enforced entirely at Layer 2 (the session_scope memory
	// Disallow set the runner's Scope hook reads), so the metaagent needs no
	// SpiceDB Granter.
	approvalOrch := approval.New()
	mg := &Metaagent{
		Memory:    signedMem,
		Extractor: metaExtractor,
		Composer:  metaComposer,
	}
	// NoticePublish delivers user-facing notices (deny / extractor-error /
	// confirmation) to channelsd's out.metaagent_notice subscriber, which posts
	// an ephemeral message on the session's channel.
	mg.NoticePublish = func(_ context.Context, scopeRef memory.Scope, requester, body string) error {
		return publishMetaagentNotice(nc, scopeRef.ID, requester, body)
	}
	// ColdStartHandler runs the new-session first-turn flow. When no LLM
	// provider is configured the extractor is nil-backed and ExtractColdStart
	// errors; the handler fails CLOSED (StatusScopeReviewFailed → the runner
	// halts the session), so it is safe to construct unconditionally.
	coldStartHandler := &ColdStartHandler{Metaagent: mg, Extractor: coldStartExtractor}
	maWorker := NewMetaagentWorker(mg, approvalOrch, nc, coldStartHandler)
	// Inject the mandatory manage_scope checker (a genuine non-nil interface — the
	// concrete *spicedb.Client constructed above; os.Exit'd already if unavailable).
	maWorker.SetManageScopeChecker(manageScopeChecker)

	maSubj := channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindMetaagentRequest)
	maSub, err := nc.Subscribe(maSubj, metaagentRequestHandler(ctx, maWorker))
	if err != nil {
		slog.Error("authzd: subscribe metaagent_request", "subject", maSubj, "err", err)
		os.Exit(1)
	}
	defer maSub.Drain()

	apSubj := channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindMetaagentApprovalApplied)
	apRoute := newMetaagentApprovalRoute(maWorker, manageScopeChecker)
	apSub, err := nc.Subscribe(apSubj, func(m *nats.Msg) {
		// context.Background(), not the run context: a decision that arrives as
		// authzd is shutting down still mutates scope + writes audit records, and
		// cancelling that mid-apply would leave the outcome half-recorded.
		apRoute.handle(context.Background(), m.Subject, m.Data)
	})
	if err != nil {
		slog.Error("authzd: subscribe metaagent_approval_applied", "subject", apSubj, "err", err)
		os.Exit(1)
	}
	defer apSub.Drain()

	<-ctx.Done()
	slog.Info("authzd: shutdown")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
}

// metaagentRequestHandler builds the NATS callback for
// ap.session.*.*.in.metaagent_request: parse the session out of the subject,
// decode the payload, hand the request to the per-session goroutine.
//
// ctx MUST be authzd's run context, never context.Background():
// MetaagentWorker.Handle selects on ctx.Done() to unblock a full per-session
// queue, and the per-session goroutine it spawns exits on that same signal.
// Background().Done() is a NIL channel, so a background context makes both arms
// permanently unselectable — the goroutines outlive shutdown, and a queue that
// fills while an approval is pending blocks this callback forever, wedging the
// whole subscription (nats.go dispatches one subscription serially).
func metaagentRequestHandler(ctx context.Context, w *MetaagentWorker) func(*nats.Msg) {
	return func(m *nats.Msg) {
		// The SUBJECT names the session, and the decode enforces the whole
		// grammar — "in" segment, non-empty ns/name, the metaagent_request leaf
		// — rather than trusting the subscription pattern to have done it. An
		// envelope-shaped body claiming a different session is refused here too,
		// so the per-session NATS grant stays load-bearing no matter which wire
		// shape a publisher sends.
		dec, err := channelevents.DecodeMetaagentIn(m.Subject, m.Data, channelevents.KindMetaagentRequest)
		if err != nil {
			slog.Warn("metaagent: request refused before dispatch; dropping",
				"subject", m.Subject, "err", err.Error())
			return
		}
		scopeRef := memory.Scope{Kind: "session", ID: dec.SessionRef()}
		// autoApply is deliberately absent from the payload type: whether the
		// human approval gate is waived is resolved from the session's
		// authz_session_config snapshot (cold_start_policy.go), not from a field
		// the publisher chooses. This subject is in the runner's own NATS grant,
		// so everything decoded here is publisher-supplied. Unknown keys —
		// including a stale "autoApply" from an older runner — are ignored by
		// encoding/json.
		var payload channelevents.MetaagentRequestPayload
		if err := json.Unmarshal(dec.Payload, &payload); err != nil {
			slog.Warn("metaagent: malformed payload", "session", scopeRef.ID, "err", err.Error())
			return
		}
		// The AgentClass envelope stays raw JSON on the wire so pkg/channels/channelevents
		// need not depend on pkg/authz/scope; it is decoded here, where the
		// consumer's type lives. A malformed one costs only in-envelope
		// classification, so it is logged and the request proceeds rather than
		// being dropped on the requester.
		var classEnvelope scope.AgentClassEnvelope
		if len(payload.Envelope) > 0 {
			if err := json.Unmarshal(payload.Envelope, &classEnvelope); err != nil {
				slog.Warn("metaagent: malformed AgentClass envelope; classifying without it",
					"session", scopeRef.ID, "err", err.Error())
			}
		}
		var approvalTimeout time.Duration
		if payload.ApprovalTimeout != "" {
			d, err := time.ParseDuration(payload.ApprovalTimeout)
			if err != nil {
				slog.Warn("metaagent: unparseable approvalTimeout; falling back to the executor default",
					"session", scopeRef.ID, "approvalTimeout", payload.ApprovalTimeout, "err", err.Error())
			} else {
				approvalTimeout = d
			}
		}
		if err := w.Handle(ctx, HandleInput{
			Scope:           scopeRef,
			Requester:       payload.Requester,
			Text:            payload.Text,
			ColdStart:       payload.ColdStart,
			Ambient:         payload.Ambient,
			Shadow:          payload.Shadow,
			Envelope:        classEnvelope,
			ApprovalTimeout: approvalTimeout,
		}); err != nil {
			slog.Warn("metaagent worker handle", "session", scopeRef.ID, "err", err.Error())
		}
	}
}

// authorizeMetaagentClicker reports whether the clicker may resolve a metaagent
// scope decision. The gate is agentsession#manage_scope (= owner), checked
// fully-consistent so a just-written owner tuple is visible. Every path that is
// not a positive answer fails CLOSED (denied) — a check error, and an absent
// checker alike: the decision is dropped so an unauthorized or unverifiable
// click can never resolve a scope change.
//
// A nil checker denies rather than allowing "defensively". This is the only
// gate on in.metaagent_approval_applied, so allowing on nil is a bypass, not a
// safety net — and it is the disposition the sibling gate already takes: the
// same nil, arriving through cold_start_pipeline's wiring, makes
// hooks.MetaagentReceived Deny. Not reachable in production, where authzd
// os.Exit's without SpiceDB rather than starting ungated; the point is that a
// typed-nil pointer compares != nil (see AGENTS.md), so this must not depend on
// how a caller in another file happens to build its checker.
func authorizeMetaagentClicker(ctx context.Context, checker authz.ManageScopeChecker, ns, name, clickerCanonical string) bool {
	if checker == nil {
		slog.Error("metaagent approval: manage_scope checker not wired; dropping (fail-closed)",
			"session", ns+"/"+name)
		return false
	}
	// CheckManageScope wants the BARE canonical (it prepends the "user:" type
	// itself); channelsd publishes the full "user:<canonical>" subject, so
	// strip the prefix here — mirrors cold_start_pipeline's MetaagentReceived
	// wiring. Skipping this denies every legitimate owner (ObjectId becomes
	// "user:user:<...>").
	bare := strings.TrimPrefix(clickerCanonical, "user:")
	allowed, err := checker.CheckManageScope(ctx, ns, name, identity.CanonicalFromTrusted(bare,
		"clicker canonical resolved server-side by channelsd from the signed interaction"), true /*fullyConsistent*/)
	if err != nil {
		slog.Error("metaagent approval: clicker authz check errored; dropping (fail-closed)",
			"session", ns+"/"+name, "err", err.Error())
		return false
	}
	return allowed
}
