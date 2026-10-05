// Package main is the agentprimitives-runner binary. One Pod per AgentSession.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/derivevalidator"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	_ "github.com/authzed/openagentprimitives/pkg/agent/llm/openai" // registers "openai"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openaicompat"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openrouter" // registers "openrouter"; also used directly for its base-URL/header accessors
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files/anthropicbridge"
	"github.com/authzed/openagentprimitives/pkg/agent/preview/markup"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/approval/summarizer"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	"github.com/authzed/openagentprimitives/pkg/agent/secretout"

	// The completion-requirement kinds an AgentClass may declare. Blank
	// imports, in the binary rather than at library level: which requirements
	// exist is a property of this build, and the gate resolves a declared key
	// through the registry at call time.
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/plansteps"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/triggerconcluded"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"

	// Registers the "deliveries" state kind, which respond_to_user writes and
	// the artifact-delivered requirement reads. Named here for the same reason
	// the requirement kinds above are — this list is where a reader looks to
	// learn what a runner session carries — though the meta package's own
	// import of it already makes the registration unavoidable in this binary.
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	// Registers the "openingsummary" state kind, which the enrichment tool
	// writes and the runner reads to keep a triggered session's pinned
	// opening message current.
	goalmodel "github.com/authzed/openagentprimitives/pkg/agent/goals"
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/authfail"
	mcpdispatch "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/authz/slotspec"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories" // registers the category set the applied-envelope bridge resolves against; also referenced directly for notice categories
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"

	// The runner resolves each session's input-channel Kind (resolve.ForSession)
	// to offer channel-sourced meta tools (mention_lookup, channel_history, the
	// info-leakage gate). Those Kinds self-register via init(), so the runner —
	// which runs sessions of every kind — must blank-import them or registry.Get
	// misses and "channel-sourced meta tools disabled" fires for every session.
	_ "github.com/authzed/openagentprimitives/pkg/agent/postsession/cost" // registers the session-cost SessionEnd hook
	"github.com/authzed/openagentprimitives/pkg/agent/runner/leakagewiring"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/userprofilegate"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection" // register prompt-injection inspector
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"    // register url-allowlist inspector
	cgregistry "github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	clipin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/sessionhold"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports" // completes the relsource claim table: this binary evaluates relwrites' arbitrary CEL-resolved writes and must see every real claim to refuse a conflicting one
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"   // register agent (session-to-session) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"   // register bento (cron) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser" // register browser (webd-hosted) kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"    // register fake kind (tests/e2e)
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"  // register github kind: input-only, but its INPUT binding is what the trigger-status capability resolves
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"   // register local (TUI) kind
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	clikind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the static/oauth/federated/githubApp credkind.Kinds the broker dispatches to via registry.Get
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation/idjag"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git" // register git workspace-source driver
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/claude" // registers claude-stream-json
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

var scheme = runtime.NewScheme()

// stopLooper is the subset of *runner.Loop used by runStopHandler. Defined as
// an interface so the stop-handler body can be tested without constructing a
// full Loop (which requires a k8s client, LLM provider, etc.).
// *runner.Loop satisfies this interface automatically.
type stopLooper interface {
	MarkPlanStoppedBestEffort(ctx context.Context)
}

// runStopHandler is the testable body of the SIGTERM/SIGINT goroutine.
// It records an explicit Stopped lifecycle transition (so the operator's fold
// sees an intentional stop rather than inferring a crash) and marks the plan
// stopped so channel surfaces reflect the stop before the pod dies.
// cancel() is intentionally left to the goroutine so it fires after both
// best-effort steps complete.
//
// loop may be nil when the signal arrives before the Loop is built (the
// process-start race window); runStopHandler is safe to call with nil.
func runStopHandler(ctx context.Context, m memory.Memory, scope memory.Scope, loop stopLooper, sessionUID string, now time.Time) {
	// Stopped is stamped with the terminal-stop sentinel key so it sorts AFTER
	// every in-loop keyed event. A racing Succeeded-then-Stopped log folds to
	// Succeeded (terminal stickiness wins); an admin-kill Stopped with no prior
	// terminal folds to Failed as intended. sessionUID attributes it to this
	// AgentSession instance so that Failed stays with the instance that stopped.
	if err := lifecyclekind.Append(ctx, m, scope, lifecyclecore.Stopped{}, now, lifecyclekind.TerminalStopKey(sessionUID)); err != nil {
		slog.Info("record Stopped lifecycle event failed (best-effort)",
			"scope", scope.ID, "err", err.Error())
	}
	// The Stopped append above bypasses the runner sequencer's applyEvent, so
	// its MarkPlanStopped effect never runs. Mirror it directly so plan items
	// are flushed to stopped before the pod exits.
	if loop != nil {
		loop.MarkPlanStoppedBestEffort(ctx)
	}
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spiceboxv1alpha1.AddToScheme(scheme))
}

// config holds the runner's non-secret configuration, populated from flags and
// (via clikit) the environment. Secret VALUES (memory token, LLM API key,
// args-hash key, audit signing key, SPICEDB_TOKEN, DEV_RELWRITES_USER_EMAIL)
// are never flags — they are read at run() time from their mounted files or
// directly from the environment so they never appear in argv or --help. The
// flags below carry the non-secret PATHS to those files, plus the required
// session-identity and memory-URL values and the typed durations.
type config struct {
	hostname string

	agentSessionNamespace string
	agentSessionName      string
	operatorMemoryURL     string
	// webdBaseURL is webd's externally reachable base URL, stamped by the
	// operator (which can read the ConfigMap this pod cannot). Empty is a
	// normal state — the cluster has no external address yet — and costs only
	// the details link on a trigger's status surface.
	webdBaseURL string

	memoryTokenPath     string
	llmAPIKeyPath       string
	argsHashKeyPath     string
	auditSigningKeyPath string

	idleTTL time.Duration
	// serveOnly parks the runner as a browser responder with NO agent loop —
	// see runServeOnly. Set by the operator when it spawns a pod because a
	// dashboard asked for data, never for a conversation.
	serveOnly               bool
	bindingAutofillDeadline time.Duration
}

func main() {
	// Before the runner's own logger is installed, and before anything can
	// compile a SpiceDB schema: its compiler logs a trace line per definition
	// through zerolog's process-global logger, and this binary's stderr is
	// `kubectl logs` for every session pod. The slog logger below is untouched.
	// See pkg/platform/deplogs.
	deplogs.Silence()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// run() owns its own context + manual SIGTERM/SIGINT handling (see below),
	// so the root command uses Execute() rather than ExecuteContext() — the
	// signal/shutdown logic is unchanged from the pre-cobra binary.
	if err := newCommand().Execute(); err != nil {
		logger.Error("runner exited with error", "err", err.Error())
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "agentprimitives-runner",
		Short:        "Per-AgentSession agent runner pod.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(cfg)
		},
	}
	fs := cmd.Flags()
	// --hostname keeps its exact prior shape: it is passed to the container as
	// --hostname=$(POD_NAME); the HOSTNAME env binding below reproduces the old
	// os.Getenv("HOSTNAME") default for callers that don't pass the flag.
	fs.StringVar(&cfg.hostname, "hostname", "", "Pod hostname (for log messages and stale-runner notes).")

	fs.StringVar(&cfg.agentSessionNamespace, "agentsession-namespace", "", "Namespace of the AgentSession this pod runs (required).")
	fs.StringVar(&cfg.agentSessionName, "agentsession-name", "", "Name of the AgentSession this pod runs (required).")
	fs.StringVar(&cfg.operatorMemoryURL, "operator-memory-url", "", "Operator memory/artifact base URL (required).")
	// Optional, unlike --operator-memory-url: a runner with no webd address
	// still runs its agent loop in full. All it loses is the durable artifact
	// link conclude_trigger_status would otherwise put on a check run.
	fs.StringVar(&cfg.webdBaseURL, "webd-base-url", "", "Webd's externally reachable base URL, used to compose durable artifact links. Empty when the cluster has no external address yet.")

	fs.StringVar(&cfg.memoryTokenPath, "memory-token-path", "/var/run/agent/memory-token", "Path to the mounted operator-memory bearer token file.")
	fs.StringVar(&cfg.llmAPIKeyPath, "llm-api-key-path", "/var/run/agent/llm-api-key", "Path to the mounted LLM API key file.")
	fs.StringVar(&cfg.argsHashKeyPath, "args-hash-key-path", "/var/run/agent/args-hash-key", "Path to the mounted per-session args-hash HMAC key file.")
	fs.StringVar(&cfg.auditSigningKeyPath, "audit-signing-key-path", "/var/run/agent/audit-signing-key", "Path to the mounted per-session audit Ed25519 signing-key seed file.")

	// These durations go through pflag so a malformed value is a startup error
	// (fail-closed) rather than a silent revert to the default.
	//
	// idleTTL covers the gap between messages, NOT somebody reading: presence
	// holds a watched session open independently (meta.AwaitConfig.PresenceCh),
	// because an agent-UI's data bindings involve no conversation and a
	// dashboard being actively driven would otherwise look like silence.
	//
	// Finite on purpose. Longer trades idle pods for fewer cold starts, and the
	// ceiling is what stops an unattended session holding a runner forever;
	// presence extends it, it does not disable it.
	fs.DurationVar(&cfg.idleTTL, "idle-ttl", 15*time.Minute, "Idle timeout before await_user_message gives up and the session idles out. A ui_presence heartbeat from a watched agent-UI restarts it.")
	fs.BoolVar(&cfg.serveOnly, "serve-only", false, "Serve agent-UI browser requests without running the agent loop: no LLM turn, no phase change, no terminal status. Exits when no ui_presence heartbeat has arrived within --idle-ttl.")
	fs.DurationVar(&cfg.bindingAutofillDeadline, "binding-autofill-deadline", 2*time.Second, "Per-tool binding-autofill wait deadline.")

	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"hostname":                  "HOSTNAME",
		"agentsession-namespace":    "AGENTSESSION_NAMESPACE",
		"agentsession-name":         "AGENTSESSION_NAME",
		"operator-memory-url":       clikit.EnvOperatorMemoryURL,
		"webd-base-url":             externalurl.EnvWebdBaseURL,
		"memory-token-path":         "MEMORY_TOKEN_PATH",
		"llm-api-key-path":          "LLM_API_KEY_PATH",
		"args-hash-key-path":        "ARGS_HASH_KEY_PATH",
		"audit-signing-key-path":    "AUDIT_SIGNING_KEY_PATH",
		"idle-ttl":                  "IDLE_TTL",
		"serve-only":                "RUNNER_SERVE_ONLY",
		"binding-autofill-deadline": "AP_BINDING_AUTOFILL_DEADLINE",
	})
	return cmd
}

func run(cfg *config) error {
	hostname := cfg.hostname
	ns := cfg.agentSessionNamespace
	name := cfg.agentSessionName
	if ns == "" || name == "" {
		return fmt.Errorf("AGENTSESSION_NAMESPACE and AGENTSESSION_NAME env vars are required")
	}
	memURL := cfg.operatorMemoryURL
	if memURL == "" {
		return fmt.Errorf("OPERATOR_MEMORY_URL env var is required")
	}

	memTokenPath := cfg.memoryTokenPath
	apiKeyPath := cfg.llmAPIKeyPath
	argsHashKeyPath := cfg.argsHashKeyPath

	memToken, err := readSecretFile(memTokenPath)
	if err != nil {
		return fmt.Errorf("read memory token: %w", err)
	}
	apiKey, err := readSecretFile(apiKeyPath)
	if err != nil {
		return fmt.Errorf("read llm api key: %w", err)
	}
	argsHashKey, err := readSecretFile(argsHashKeyPath)
	if err != nil {
		// Fail-closed: without the session key, grant arguments_hash
		// bindings would be silently keyed on nil (forgeable by anything
		// that can compute a plain HMAC). Refuse to start, like a missing
		// memory token.
		return fmt.Errorf("read args-hash key: %w", err)
	}

	restCfg := ctrl.GetConfigOrDie()
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}
	mcpBroker := inproc.NewWithMinter(c, idjag.New(safehttp.Client()))
	// githubApp is minted on demand, like federated above; GitHub's API host
	// is fixed (never attacker-influenceable), so this does not need the
	// SSRF-guarded safehttp client federation's discovery leg does.
	mcpBroker.GitHubApp = githubapp.Adapt(githubapp.NewHTTPMinter())

	// mcpAuthInv tracks which live MCPTools hold a credential resolved from which
	// Secret. The broker's cache is not the only copy of a resolved token: each
	// MCPTool freezes (header, value) into struct fields at session start. A
	// credential revoke must invalidate both, so both are registered as
	// credential.SecretInvalidator targets on the revocation subscriber below.
	mcpAuthInv := newMCPAuthInvalidator()

	sessKey := client.ObjectKey{Namespace: ns, Name: name}
	memKey := memory.NamespacedName{Namespace: ns, Name: name}

	// SIGTERM handling. The signal-consuming goroutine is started below, once
	// the signing memory + session scope exist, so it can record an explicit
	// Stopped lifecycle transition before cancelling the loop.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// loopRef is stored once the runner Loop is fully constructed (below). The
	// SIGTERM handler and the await OnYield/OnResume closures load it late-bound.
	// atomic.Pointer makes the cross-goroutine read from the signal handler
	// race-free and establishes a happens-before with the Loop's field writes.
	var loopRef atomic.Pointer[runner.Loop]

	// All memory access — turn transcripts and the gap Kinds
	// (authz_decision, relwrites_audit, label, approval, lifecycle) —
	// goes over HTTP to the operator-hosted Memory. The runner keeps no
	// in-process backend; the operator owns the one Local and runs the
	// lifecycle hook. memHTTP implements memory.Memory.
	memHTTP := httpclient.New(memURL, memToken)

	// Per-session audit signing key: every append-only memory write
	// (transcript, authz decisions, audits) is Ed25519-signed so it can
	// be verified as coming from this session. Fail-closed — without the
	// key the operator's verify-on-write would reject our writes anyway.
	auditKeyPath := cfg.auditSigningKeyPath
	auditSeedHex, err := readSecretFile(auditKeyPath)
	if err != nil {
		return fmt.Errorf("read audit signing key: %w", err)
	}
	auditSeed, err := hex.DecodeString(auditSeedHex)
	if err != nil || len(auditSeed) != ed25519.SeedSize {
		return fmt.Errorf("audit signing key malformed (want %d-byte hex seed): %v", ed25519.SeedSize, err)
	}
	auditSigner := provenance.NewSigner(ed25519.NewKeyFromSeed(auditSeed), provenance.SessionPublisher(ns, name))
	// memSigned wraps memHTTP so every append-only Put is signed and the
	// per-scope hash chain is seeded (lazily, on the first Put per scope,
	// by querying the inner memHTTP). All append-only writers below go
	// through memSigned; read-only paths and the concrete-typed KG client
	// keep the raw memHTTP.
	//
	// Declared as the memory.Memory INTERFACE, not the concrete
	// *provenance.SigningMemory `:=` would infer — every downstream
	// assignment (uiview.Runtime.Mem among them) takes an interface field,
	// and AGENTS.md's typed-nil rule is about the declared type at exactly
	// this kind of construction site, not just about this value ever being
	// nil in practice.
	var memSigned memory.Memory = provenance.NewSigningMemory(memHTTP, auditSigner)

	// The transcript appender. turn.Appender wraps memSigned for one
	// session scope and satisfies runner.MemoryAppender structurally
	// (ReadAll/ReadAfter/Append). Turns are append-only, so they are
	// signed.
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	turnAppender := turn.NewAppender(memSigned, scope)
	artifactSvc := artifacts.NewService(memHTTP, nil)

	// Read the AgentSession before the signal handler is armed: the Stopped the
	// handler records must name the AgentSession INSTANCE that stopped
	// (lifecycle.ForIncarnation), and the scope it writes into outlives every CR
	// that has held this namespace/name. An unattributed Stopped folds to Failed
	// for whichever session next holds the name.
	var sess spiceboxv1alpha1.AgentSession
	if err := c.Get(rootCtx, sessKey, &sess); err != nil {
		return fmt.Errorf("get AgentSession: %w", err)
	}

	// On SIGTERM/SIGINT (admin-kill / supersede / pod eviction), record an
	// explicit Stopped transition in the signed lifecycle log BEFORE cancelling
	// the loop, so the operator's post-region fold sees an interrupted
	// termination (→ Failed + plan items stopped) rather than inferring a crash.
	// Best-effort within the termination grace period; on a fresh context since
	// rootCtx is about to be cancelled. Started here (not at rootCtx creation)
	// because it needs memSigned + scope.
	sessionUID := string(sess.UID)

	// envSigner signs every envelope this runner publishes with the session's
	// audit identity key, so a receiving component can verify which session
	// instance actually produced it rather than trusting the wire-carried
	// Session field alone. Constructed once, here — the earliest point both
	// the audit seed and the real session UID (from the Get above) are in
	// scope — and threaded to every publish site below.
	envSigner, err := channelevents.NewEnvelopeSigner(
		ed25519.NewKeyFromSeed(auditSeed), provenance.SessionPublisher(ns, name), sessionUID)
	if err != nil {
		return fmt.Errorf("construct envelope signer: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		s := <-sigCh
		slog.Info("signal received; recording Stopped + canceling loop", "signal", s.String())
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		// Avoid passing a typed-nil *runner.Loop as a stopLooper interface (which
		// would produce a non-nil interface wrapping nil — see AGENTS.md nil-interface
		// rule). Check the atomic pointer first, then assign through the interface.
		var loop stopLooper
		if l := loopRef.Load(); l != nil {
			loop = l
		}
		runStopHandler(stopCtx, memSigned, scope, loop, sessionUID, time.Now().UTC())
		stopCancel()
		cancel()
	}()

	statusPatcher := runner.NewStatusPatcher(c, sessKey)

	// failSession is this file's ONE terminal-failure path. Every boot-time
	// failure below goes through it rather than calling WriteFailed directly,
	// because the status write alone is not a record: it lands on the
	// AgentSession's status, and a reader who has only the session's durable
	// log — a workshop reading back a test run, `oap audit verify`, anyone
	// looking after the CR is gone — learns THAT the session failed and never
	// why. A person hit exactly that: a pinned tool the connector no longer
	// served failed the session at start, and the log they were handed was
	// empty, so the failure read as "nothing ever happened".
	//
	// The append carries the same (reason, message) pair as the status write,
	// under the terminal-stop ordering key: like the SIGTERM handler's Stopped,
	// this runs off the turn loop with no turn index to pack, and sorts after
	// every keyed event because it is the last thing that happened before the
	// process exits. Best-effort — a log the operator cannot accept must not
	// cost the session its status.
	failSession := func(reason, message string) error {
		if err := lifecyclekind.Append(rootCtx, memSigned, scope,
			lifecyclecore.RunnerTerminal{Phase: lifecyclecore.PhaseFailed, Reason: reason, Message: message},
			time.Now().UTC(), lifecyclekind.TerminalStopKey(sessionUID),
		); err != nil {
			slog.Info("record the terminal failure in the lifecycle log failed (best-effort)",
				"session", ns+"/"+name, "reason", reason, "err", err.Error())
		}
		return statusPatcher.WriteFailed(rootCtx, reason, message)
	}

	// Stale-runner short-circuit.
	exited, err := runner.HandleStaleSession(rootCtx, c, turnAppender, statusPatcher, sessKey, hostname, memKey)
	if err != nil {
		return fmt.Errorf("stale-session check: %w", err)
	}
	if exited {
		slog.Info("session already terminal; exiting")
		return nil
	}

	// The AgentSession was read above, ahead of the signal handler that needs its
	// UID; nothing between there and here mutates the spec this reads.
	var class spiceboxv1alpha1.AgentClass
	if err := c.Get(rootCtx, client.ObjectKey{Namespace: ns, Name: sess.Spec.Class}, &class); err != nil {
		return fmt.Errorf("get AgentClass %q: %w", sess.Spec.Class, err)
	}

	// agentUIRequestedTools is condition (1) of the three-way browser-tool
	// grant (pkg/web/uigrant.Materialize via runner.MaterializeAppTools): the
	// AgentUI's own spec.tools request. class.Spec.AgentUI (condition 3, the
	// deployment grant) is read directly off class below — no separate fetch
	// needed for that half. runner.ResolveAgentUITools short-circuits on a nil
	// grant (no AgentUI configured at all); when the grant's Ref cannot be
	// resolved (not found, forbidden, or any other error) this logs loudly and
	// falls through with agentUIRequestedTools left nil, which is sufficient
	// on its own to make runner.MaterializeAppTools's result — and therefore
	// Loop.AppTools — empty. This is deliberately NOT a session-failing
	// error: an unresolvable browser-UI grant must not take down the agent's
	// core LLM loop, only the browser-callable surface, which fails closed
	// instead.
	agentUIRequestedTools, auiErr := runner.ResolveAgentUITools(rootCtx, c, ns, class.Spec.AgentUI)
	if auiErr != nil {
		// auiErr != nil implies a non-nil grant (a nil grant returns nil,nil),
		// so class.Spec.AgentUI.Ref is safe to read here.
		slog.Default().Info("resolve AgentUI for browser-tool grant errored; AppTools will be empty",
			"session", sessKey.Namespace+"/"+sessKey.Name,
			"agentClass", ns+"/"+sess.Spec.Class,
			"agentUI", ns+"/"+class.Spec.AgentUI.Ref,
			"err", auiErr.Error())
		// A pod log an operator has to go find is not a surface. Every sibling
		// ref failure lands on AgentSession status, and this one is worse than
		// most to diagnose from the outside: the agent runs normally and only
		// the browser UI is dead. Best-effort — the note failing must not take
		// down a session whose core loop is fine.
		besteffort.Log(slog.Default().Info, "AppendRunnerNote (AgentUI unresolved)",
			statusPatcher.AppendRunnerNote(rootCtx, fmt.Sprintf(
				"AgentUI/%s could not be resolved (%v); no browser-callable tools for this session",
				class.Spec.AgentUI.Ref, auiErr)),
			"session", sessKey.Namespace+"/"+sessKey.Name,
			"agentUI", ns+"/"+class.Spec.AgentUI.Ref)
	}
	agentUIKey := ""
	if class.Spec.AgentUI != nil {
		agentUIKey = ns + "/" + class.Spec.AgentUI.Ref
	}

	systemPrompt, err := runner.ResolvePrompt(rootCtx, c, ns, class.Spec.SystemPrompt)
	if err != nil {
		return fmt.Errorf("resolve system prompt: %w", err)
	}
	userPrompt, err := runner.ResolvePrompt(rootCtx, c, ns, sess.Spec.Prompt)
	if err != nil {
		return fmt.Errorf("resolve user prompt: %w", err)
	}

	// The AgentSession reconciler resolves the 4-tier settings and stamps
	// status.effectiveSettings before creating this pod. Read the clamped
	// budget + resolved model directly — do not re-merge here.
	if sess.Status.EffectiveSettings == nil {
		return fmt.Errorf("AgentSession %s/%s has no status.effectiveSettings; operator must stamp it before pod creation", ns, sess.Name)
	}
	budgetCfg := sess.Status.EffectiveSettings.Budget

	// Budget run-time is measured by a RunClock seeded from the session's
	// persisted run-time (status.runDuration), so active run-time accumulates
	// across the sleep/resume boundary — a session that slept and resumed does
	// NOT get a fresh MaxDuration window. Idle/human-wait time is excluded by
	// the clock's pause/resume. The wall-clock lifetime cap is sessionExpiration,
	// enforced from status.startedAt (below + by the operator).
	var runSeed time.Duration
	if sess.Status.RunDuration != nil {
		runSeed = sess.Status.RunDuration.Duration
	}
	runClock := runner.NewRunClock(runSeed, time.Now)

	// Session StartedAt for the sessionExpiration backstop. Zero until the
	// operator/first-turn stamps it, in which case the backstop is inert (a
	// brand-new session can't have exceeded its lifetime).
	var sessionStartedAt time.Time
	if sess.Status.StartedAt != nil {
		sessionStartedAt = sess.Status.StartedAt.Time
	}

	provider, err := providers.New(sess.Status.EffectiveSettings.Model.Provider, apiKey)
	if err != nil {
		return fmt.Errorf("runner: build LLM provider: %w", err)
	}

	// SpiceDB is REQUIRED — per-tool authz checks and JIT relationship
	// writes both depend on it, and silently disabling authz when the
	// runner can't reach SpiceDB is exactly the footgun we want to
	// prevent. Both SPICEDB_ENDPOINT and SPICEDB_TOKEN must be set
	// (the agentsession podspec injects them via valueFrom against the
	// shared ConfigMap + Secret); a missing value or unreachable server
	// fails the pod before the first tool is dispatched.
	spdbCfg, err := spicedb.LoadEnvConfig()
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	spdbCli, err := spicedb.NewClient(spdbCfg.Endpoint, spdbCfg.Token, spdbCfg.Insecure)
	if err != nil {
		return fmt.Errorf("runner: connect SpiceDB at %s: %w", spdbCfg.Endpoint, err)
	}

	// Bind the SpiceDB client into channel-kind audience resolvers. Mirror
	// of internal/cmd/channelsd/main.go's wiring — without this, the write-side
	// info-leakage gate at respond_to_user time would call
	// Kind.ResolveAudience on a kind whose internal resolver is nil and
	// return "audience resolver not wired", failing every Slack send.
	leakagewiring.WireChannelKindAudienceResolvers(spdbCli)

	// SpiceDB-backed relwrites.Writer for slice-4 JIT relationship writes.
	// RelWriter's WriteRelationships is option-free by construction, so it
	// satisfies relwrites.SpiceDBClient directly — no adapter required.
	var relWriter relwrites.Writer = &relwrites.SpiceDBWriter{Client: spdbCli.Writer(relwrites.Source)}

	// DEV/TEST override: when DEV_RELWRITES_USER_EMAIL is set on the
	// runner pod, every JIT relwrites tuple whose subject is `user:*`
	// is rewritten to the override email's canonical id BEFORE the
	// SpiceDB write. Used to redirect ownership relationships onto
	// the developer's own account so they can play both the requester
	// and the approver in a dev cluster. See
	// pkg/authz/relwrites/override_writer.go for the security caveats —
	// this is NOT for production use.
	if overrideEmail := strings.TrimSpace(os.Getenv("DEV_RELWRITES_USER_EMAIL")); overrideEmail != "" {
		canonical, cerr := identity.EmailReference(identity.Email(overrideEmail)).Canonical()
		if cerr != nil {
			slog.Warn("DEV_RELWRITES_USER_EMAIL set but not canonicalizable; ignoring override",
				"email", overrideEmail, "err", cerr.Error())
		} else {
			slog.Warn("DEV_RELWRITES_USER_EMAIL is set; ALL relwrites user: subjects will be redirected",
				"email", overrideEmail, "canonical", canonical)
			relWriter = &relwrites.UserSubjectOverrideWriter{
				Inner:             relWriter,
				OverrideCanonical: canonical,
				LogFn:             func(msg string, kv ...any) { slog.Info(msg, kv...) },
			}
		}
	}

	// Read side of the slot-bound gate: a relwrites block declaring
	// requireSlotBound may only write onto an instance THIS session holds a
	// slot grant on. Built once here and wired onto both tool kinds below,
	// beside each SetRelWriter — the sandbox loop and the MCP loop are two
	// sites, and test/e2e/inprocess_runner_factory.go mirrors both for the
	// harness's own Loop.
	//
	// Deliberately NOT wrapped by the DEV_RELWRITES_USER_EMAIL override above:
	// that override rewrites a tuple's user SUBJECT so a developer can play
	// both requester and approver, and it has no business relaxing which
	// RESOURCE a write may land on.
	slotBoundChecker := relwrites.NewSlotBoundChecker(spdbCli, sessKey.Namespace, sessKey.Name)

	// LabelStore caches (resourceType, id) → friendly label tuples
	// extracted from MCP tool responses. Per-runner-Pod, in-memory,
	// FIFO-bounded. Threaded into every MCPTool (SetLabelSink) and
	// onto the Loop (LabelStore field) below. Threat model note:
	// labels NEVER reach any LLM context — consulted only by
	// deterministic channel renderers at approval-emit time.
	labelStore := runner.NewLabelStore(0)

	// Channel-attached wiring: connect NATS, subscribe for inbound messages,
	// and build the respond_to_user + await_user_message meta-tools.
	// When CHANNEL_ATTACHED is unset or "false" this entire block is skipped
	// and the runner behaves exactly as before (kubectl-driven sessions).
	chanAttached := channelAttachedFromEnv()
	var natsRT *natsRuntime
	var pubFn func(ctx context.Context, subject string, payload []byte) error
	var natsRequestFn func(ctx context.Context, subject string, payload []byte) ([]byte, error)
	var inboundCh <-chan struct{}
	// presenceCh carries ui_presence heartbeats into await_user_message, which
	// restarts its idle timer on each. Buffered at 1 and written
	// non-blockingly (subscribeUIPresence): the channel means "at least one
	// heartbeat since the last read", so a second arriving before the loop
	// reads the first adds nothing.
	//
	// Left nil when not channel-attached, which is the honest shape: without
	// NATS there is nothing to carry a heartbeat, and a nil channel blocks
	// forever in await's select — exactly the pre-existing behaviour.
	var presenceCh chan struct{}
	var subjectPrefix string
	// resolve.ForSession result, resolved ONCE here and shared by every
	// channel-sourced capability (mention_lookup, channel_history, …) via
	// RunnerEnv.Resolved*/ResolveErr. Zero-valued when not channel-attached.
	var resolvedChannel *spiceboxv1alpha1.Channel
	var resolvedSecret *corev1.Secret
	var resolvedKind channelkinds.Kind
	var resolveErr error
	apdNote := runner.AppendSystemNoteFunc(turnAppender)
	// leakageGateFn is set after loop is built (loop references itself via
	// Loop.LeakageGateForRespond). The RunnerEnv.LeakageGate closure captures it
	// so the tool is assembled before the loop is constructed but the actual
	// gate function pointer is late-bound.
	var leakageGateFn func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error
	// loopRef (declared above, before the SIGTERM handler) is stored after the
	// loop is built; late-bound via the OnAwaitYield/Resume closures like
	// leakageGateFn so the await tool is assembled before the loop exists.
	//
	// toolLookupFn is late-bound once mergedTools (below) is final — the
	// complete LLM-facing tool table, including NonMetaTools and introspection.
	// Set immediately after capability.Assemble returns; RunnerEnv.ToolLookup
	// (assigned before Assemble runs, so credential_update's Offer can capture
	// it) is a wrapper that forwards to this variable, mirroring leakageGateFn.
	var toolLookupFn func(name string) (tool.Tool, bool)
	if chanAttached {
		if sess.Spec.InputChannel == nil {
			return fmt.Errorf("CHANNEL_ATTACHED=true but spec.inputChannel is nil for %s/%s", ns, name)
		}
		binding := sess.Spec.InputChannel
		subjectPrefix = binding.NATSSubjectPrefix

		var initErr error
		natsRT, initErr = initNATS(rootCtx, natsURLFromEnv(), subjectPrefix)
		if initErr != nil {
			return fmt.Errorf("init nats: %w", initErr)
		}
		if natsRT == nil {
			// CHANNEL_ATTACHED=true but NATS_URL was empty — no real NATS
			// connection. The publish function will error on use; the
			// inboundCh is a permanently-blocking channel so await will
			// always time out at IdleTTL. This is a misconfiguration but
			// not worth aborting for — the runner will idle-exit quickly.
			natsRT = &natsRuntime{inboundCh: make(chan struct{}, 1)}
		} else {
			defer natsRT.close()
		}

		pubFn = natsPublishFunc(natsRT.conn)
		natsRequestFn = natsRequestFunc(natsRT.conn)
		inboundCh = natsRT.inboundCh
		presenceCh = make(chan struct{}, 1)

		// Resolve the bound Channel/Secret/Kind ONCE — the shared helper
		// channelsd also uses. Every channel-sourced capability (mention_lookup,
		// channel_history) reads this via RunnerEnv.Resolved*/ResolveErr rather
		// than resolving again. A resolve error is NOT fatal: those capabilities
		// decline (mention_lookup logs a SkipReason at assembly, channel_history
		// quietly), so warn once here and carry the error forward.
		resolvedChannel, resolvedSecret, resolvedKind, resolveErr = resolve.ForSession(rootCtx, c, &sess)
		if resolveErr != nil {
			slog.Warn("channel resolve failed; channel-sourced meta tools disabled",
				"session", ns+"/"+name, "err", resolveErr)
		}
	}

	// Resolve the RuntimeIdentity once, before the bundle and MCP loops.
	//
	// identityMode=userPassthrough: read the SessionUserIdentity (named after
	// the AgentSession, in the same namespace). The operator's D4 gate creates
	// it before the runner pod is spawned, so its absence is session-fatal.
	//
	// identityMode=agent (or unset): load the class-level AgentIdentity. An
	// absent agentIdentity field means no credentials; leave the identity
	// zero-valued so credresolve.Descriptors resolves no descriptors (a kind
	// with no requirements yields an empty slice; one with requirements errors).
	//
	// identityMode=ask|dynamic: resolve an EFFECTIVE mode. On first boot the
	// choice is not yet made (status.effectiveIdentityMode empty), so boot on a
	// PROVISIONAL agent identity — always available for ask|dynamic, since CEL
	// requires spec.agentIdentity — and set gatePending so the SessionStart
	// IdentityChoiceGate asks the initiating user to confirm (agent) or hand off
	// (userPassthrough). On a re-spawn past the choice,
	// status.effectiveIdentityMode carries the resolved mode and no gate runs.
	// Static modes pass through unchanged with gatePending false.
	effectiveMode := class.Spec.IdentityMode
	gatePending := false
	switch effectiveMode {
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		if sess.Status.EffectiveIdentityMode != "" {
			effectiveMode = sess.Status.EffectiveIdentityMode // re-spawn after a prior choice
		} else {
			effectiveMode = spiceboxv1alpha1.IdentityModeAgent // provisional; the gate confirms or hands off
			gatePending = true
		}
	}

	var runtimeIdentity runner.RuntimeIdentity
	switch effectiveMode {
	case spiceboxv1alpha1.IdentityModeUserPassthrough:
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := c.Get(rootCtx, client.ObjectKey{Namespace: ns, Name: name}, &suid); err != nil {
			if werr := failSession(
				spiceboxv1alpha1.ReasonAgentSessionMCPAuthResolutionFailed,
				fmt.Sprintf("userPassthrough: SessionUserIdentity %s/%s not found: %v", ns, name, err)); werr != nil {
				return werr
			}
			return nil
		}
		runtimeIdentity = runner.RuntimeIdentityFromSessionUserIdentity(&suid)
	default: // effectiveMode=agent (static agent/unset, ask|dynamic provisional, or re-spawn→agent)
		if class.Spec.AgentIdentity != "" {
			var agentIdentity spiceboxv1alpha1.AgentIdentity
			if err := c.Get(rootCtx, client.ObjectKey{Namespace: ns, Name: class.Spec.AgentIdentity}, &agentIdentity); err != nil {
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPAuthResolutionFailed,
					fmt.Sprintf("get AgentIdentity %q: %s", class.Spec.AgentIdentity, err.Error())); werr != nil {
					return werr
				}
				return nil
			}
			runtimeIdentity = runner.RuntimeIdentityFromAgentIdentity(&agentIdentity)
		}
	}

	// provenanceState records per-tool pin-origin facts for every synthesized
	// tool. Populated in the bundle, MCP, and sidecar loops below; consumed by
	// recordAuthzDecision to stamp pin facts onto each authz_decision audit entry.
	provenanceState := runner.NewToolProvenance()

	// authFailureByOrigin maps every credential-bearing tool origin to the
	// provider's declared authFailure: block, so the credential-update
	// corroboration recorder can tell "this credential was rejected" from "this
	// call failed for some other reason". Populated by the bundle (toolkit),
	// MCP, and sidecar loops below — each origin kind exposes a different
	// failure surface, and an origin absent from the map is uncorroborable, so
	// all three must be recorded here or that kind's credentials silently lose
	// corroboration. See the type's doc for why it is boot-only and read-only
	// afterwards.
	//
	// test/e2e/inprocess_runner_factory.go populates it identically.
	authFailureByOrigin := runner.NewAuthFailureOrigins()

	// Build sandbox tools for each bundle.
	sandboxTools := []tool.Tool{}
	bundleSessionMap := map[string]string{}
	for _, bs := range sess.Status.BundleSessions {
		bundleSessionMap[bs.Name] = bs.SpiceboxSessionName

		// Get the SpiceboxSession to read its resolved-class tool catalog.
		var sboxSess spiceboxv1alpha1.SpiceboxSession
		if err := c.Get(rootCtx, client.ObjectKey{Namespace: ns, Name: bs.SpiceboxSessionName}, &sboxSess); err != nil {
			return fmt.Errorf("get bundle SpiceboxSession %q: %w", bs.SpiceboxSessionName, err)
		}
		if sboxSess.Status.ResolvedClass == nil {
			return fmt.Errorf("bundle SpiceboxSession %q has no ResolvedClass yet", bs.SpiceboxSessionName)
		}
		classTools := sboxSess.Status.ResolvedClass.Tools

		// Find the AgentClass bundle config (we already loaded class above).
		var bundleCfg spiceboxv1alpha1.ToolBundle
		bundleFound := false
		for _, b := range class.Spec.ToolBundles {
			if b.Name == bs.Name {
				bundleCfg = b
				bundleFound = true
				break
			}
		}
		if !bundleFound {
			return fmt.Errorf("bundle %q present in AgentSession status but not found in AgentClass.spec.toolBundles", bs.Name)
		}

		// Resolve the per-bundle RuntimeIdentity for CLI credential descriptors
		// through runner.BundleRuntimeIdentity — the SAME helper the e2e harness
		// uses, so the two cannot drift. For identityMode=userPassthrough every
		// bundle reuses the single session-scoped runtimeIdentity computed above
		// from the SessionUserIdentity; for identityMode=agent the helper loads
		// the bundle/class AgentIdentity.
		bundleRuntimeIdentity, err := runner.BundleRuntimeIdentity(rootCtx, c, ns, runtimeIdentity, &class, bundleCfg, effectiveMode)
		if err != nil {
			return err
		}

		// Resolve each referenced toolspec.
		specs := []*spec.Spec{}
		toolkits := []*toolkit.Toolkit{}
		credentials := [][]spiceboxv1alpha1.CredentialDescriptor{}
		for _, tsName := range bundleCfg.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := c.Get(rootCtx, client.ObjectKey{Name: tsName}, &ts); err != nil {
				return fmt.Errorf("get SpiceboxToolspec %q for bundle %q: %w", tsName, bs.Name, err)
			}
			if !runner.ToolkitAllowed(sess.Status.EffectiveSettings, ts.Spec.Toolkit.Name) {
				slog.Default().Info("skipping toolkit not in effective allowlist",
					"toolkit", ts.Spec.Toolkit.Name, "bundle", bs.Name, "session", sess.Name)
				continue
			}
			s, err := ts.Spec.ToSpec()
			if err != nil {
				return fmt.Errorf("convert SpiceboxToolspec %q: %w", tsName, err)
			}
			specs = append(specs, s)
			tk, terr := runner.ResolveToolkitForBundle(rootCtx, c, ts.Spec.Toolkit.Name)
			if terr != nil {
				return fmt.Errorf("toolspec %q in bundle %q: %w", tsName, bs.Name, terr)
			}
			toolkits = append(toolkits, tk)
			reqs := clikind.New().SetupRequirements(rootCtx, clikind.NewTarget(tk))
			// Record this toolkit origin's auth-failure shape. A CLI toolkit has
			// no HTTP status to observe, so exitCodes/stderrPatterns are the
			// whole corroboration surface here.
			authFailureByOrigin.RecordRequirements(originfmt.ForToolkit(tk.Name), reqs)
			creds, cerr := credresolve.Descriptors(reqs, bundleRuntimeIdentity, bundleCfg.CredentialRemap)
			if cerr != nil {
				return fmt.Errorf("resolve credentials for toolspec %q in bundle %q: %w", tsName, bs.Name, cerr)
			}
			credresolve.Log(logr.FromSlogHandler(slog.Default().Handler()), bundleRuntimeIdentity, creds)
			credentials = append(credentials, creds)
		}

		tools, err := sandbox.Synthesize(bundleCfg, specs, toolkits, credentials, classTools)
		if err != nil {
			return fmt.Errorf("synthesize tools for bundle %q: %w", bs.Name, err)
		}
		// Attach the shared relwrites.Writer + logger + memory to each
		// synthesized sandbox tool so its post-Succeeded WritesRelationships
		// (from the toolspec's spec.Spec.WritesRelationships) and Observes
		// (spec.Spec.Observes) both evaluate. Mirrors the MCP loop's
		// SetRelWriter/SetMemory wiring below — relWriter and memSigned are the
		// SAME shared instances, so both tool kinds fan into one SpiceDB write
		// client and one signed memory facade.
		for _, t := range tools {
			if st, ok := t.(*sandbox.SandboxTool); ok {
				st.SetRelWriter(relWriter)
				st.SetSlotBoundChecker(slotBoundChecker)
				st.SetLogger(slog.Default())
				st.SetMemory(memSigned)
			}
		}
		sandboxTools = append(sandboxTools, tools...)

		// Record cli-kind provenance for each synthesized sandbox tool. The
		// synthesized name is deterministically <bundleCfg.Name>_<classTool.Name>,
		// where classTool.Name == spec.Toolkit.Name (the key sandbox.Synthesize
		// uses to look up classTools). PinnedBinaryHash is NOT preserved by the
		// toolkit.Toolkit JSON round-trip (toolkit.Target has no such field); we
		// fetch it from the SpiceboxToolkit CR when one exists. Builtins (from the
		// embedded toolkits catalog) never carry a PinnedBinaryHash, so Digest is
		// empty for them and strength is derived from VersionRange only.
		for i, ts := range specs {
			var pinnedHash string
			var stk spiceboxv1alpha1.SpiceboxToolkit
			if gerr := c.Get(rootCtx, client.ObjectKey{Name: ts.Toolkit.Name}, &stk); gerr == nil {
				pinnedHash = stk.Spec.Target.PinnedBinaryHash
			} else if !k8serrors.IsNotFound(gerr) {
				// NotFound is expected for embedded-catalog builtins; anything
				// else silently downgrading the audit record's pin facts would
				// hide a real API problem.
				slog.Default().Info("SpiceboxToolkit get for pin provenance failed",
					"toolkit", ts.Toolkit.Name, "err", gerr.Error())
			}
			versionRange := ""
			if i < len(toolkits) && toolkits[i] != nil {
				versionRange = toolkits[i].Target.VersionRange
			}
			strength := string(clipin.StrengthFor(pinnedHash, versionRange))
			toolName := bundleCfg.Name + "_" + ts.Toolkit.Name
			provenanceState.Set(toolName, runner.ProvenanceRecord{
				Name: ts.Toolkit.Name,
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     clipin.KindName,
					Strength: strength,
					Digest:   pinnedHash,
					Version:  versionRange,
				},
			})
		}
	}

	// pinDriftState is constructed before the MCP loop so it can be populated
	// during boot synthesis and later wired into the Loop for per-call approval
	// escalation. It must outlive the MCP loop to accumulate marks.
	pinDriftState := runner.NewPinDriftState()

	// One persistent MCP session per server URL for the whole AgentSession,
	// shared across every MCPTool synthesized below (keyed internally by URL, so
	// all tools of one server share a session). Server-side per-session state —
	// the dedicated-mcp sidecar's PermissionSystem/cluster selection, which it
	// keys by the MCP session id — must survive across tool calls; opening a
	// fresh session per call reset it every time ("no PermissionSystem selected"
	// even right after selecting one). Closed at session end.
	mcpSessionCache := mcpprobe.NewSessionCache()
	defer func() { _ = mcpSessionCache.Close() }()

	// Build MCP tools. runtimeIdentity was resolved above.
	mcpTools := []tool.Tool{}
	// mcpAppTools accumulates MCP-UI app-visible-only tools across every
	// MCPServer ref (mcpdispatch.Synthesize's split). These are wired onto
	// Loop.AppTools below — a SEPARATE registry from mcpTools/l.Tools, never
	// offered to the LLM and not callable in this phase (no transport yet).
	mcpAppTools := []tool.Tool{}
	// mcpAppOrigins captures, per MCPServer processed below, condition (2) of
	// the three-way browser-tool grant (pkg/web/uigrant.Materialize): whether
	// this origin has opted into app-visible calls and which of its tools it
	// claims as app-visible. Fed into runner.MaterializeAppTools at the
	// Loop.AppTools choke point below, alongside agentUIRequestedTools
	// (condition 1) and class.Spec.AgentUI.GrantedTools (condition 3).
	mcpAppOrigins := []uigrant.Origin{}
	// appToolRateCfg accumulates each opted-in MCPServer's
	// mcpUiAppTools.maxCallsPerMin, keyed by the REAL CR name (Origin()'s
	// "mcpserver/<name>" — matches cr.Name, NOT the LLM-prefixed cr2.Name).
	// Wired onto runner.NewAppToolRateLimiter below: a DEDICATED per-origin
	// limiter on the app-tool call path, deliberately NOT a toolguard rule
	// (which would also gate the origin's LLM-visible tools and strip its
	// circuit breaker).
	appToolRateCfg := map[string]int32{}
	for _, ref := range class.Spec.MCPServers {
		if !mcpServerAllowed(sess.Status.EffectiveSettings, ref.Ref) {
			slog.Default().Info("skipping MCP server not in effective allowlist",
				"server", ref.Ref, "session", sess.Name)
			continue
		}
		var cr spiceboxv1alpha1.MCPServer
		if err := c.Get(rootCtx, client.ObjectKey{Namespace: ns, Name: ref.Ref}, &cr); err != nil {
			if k8serrors.IsNotFound(err) {
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPServerMissing,
					fmt.Sprintf("AgentClass.spec.mcpServers[%q]: MCPServer/%s not found", ref.Name, ref.Ref)); werr != nil {
					return werr
				}
				return nil
			}
			return fmt.Errorf("get MCPServer %q: %w", ref.Ref, err)
		}
		if cond := apimeta.FindStatusCondition(cr.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid); cond == nil || cond.Status != metav1.ConditionTrue {
			msg := fmt.Sprintf("MCPServer/%s is not Valid", ref.Ref)
			if cond != nil {
				msg = fmt.Sprintf("MCPServer/%s: Valid=%s reason=%s message=%s", ref.Ref, cond.Status, cond.Reason, cond.Message)
			}
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPServerInvalid, msg); werr != nil {
				return werr
			}
			return nil
		}
		mcpReqs := mcpkind.New().SetupRequirements(rootCtx, mcpkind.NewTarget(&cr))
		// Record this MCP origin's auth-failure shape (see authFailureByOrigin).
		authFailureByOrigin.RecordRequirements(originfmt.ForMCPServer(cr.Name), mcpReqs)
		descs, derr := credresolve.Descriptors(mcpReqs, runtimeIdentity, ref.CredentialRemap)
		if derr != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPAuthResolutionFailed, derr.Error()); werr != nil {
				return werr
			}
			return nil
		}
		var header, value string
		if len(descs) > 0 {
			credresolve.Log(logr.FromSlogHandler(slog.Default().Handler()), runtimeIdentity, descs)
			res, rerr := mcpBroker.Resolve(rootCtx, broker.Request{Credentials: descs})
			if rerr != nil {
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPAuthResolutionFailed, rerr.Error()); werr != nil {
					return werr
				}
				return nil
			}
			for h, v := range res.HTTPHeaders {
				header, value = h, v
			}
		}
		probeCtx, cancel := context.WithTimeout(rootCtx, 10*time.Second)
		live, perr := (&mcpprobe.Client{URL: cr.Spec.Server.URL}).ListTools(probeCtx, header, value)
		cancel()
		if perr != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPServerUnreachable, perr.Error()); werr != nil {
				return werr
			}
			return nil
		}
		// The pin is a security control (ASI04) and stays fail-closed: a spec
		// naming a tool the server does not serve fails the session at start,
		// before a single call. What CHANGED is what the refusal says — it now
		// names the server's own tool list beside the absent pins, because that
		// list is usually where the renamed tool is, and without it the reader
		// has a dead end. See probe.AllowlistDrift.
		pinnedNames := make([]string, 0, len(cr.Spec.Tools))
		for _, t := range cr.Spec.Tools {
			pinnedNames = append(pinnedNames, t.Name)
		}
		if missing, driftMsg := mcpprobe.AllowlistDrift(ref.Ref, pinnedNames, live); len(missing) > 0 {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift, driftMsg); werr != nil {
				return werr
			}
			return nil
		}
		// Compile-check CEL constraints at session start so a broken-CEL spec
		// (e.g. one that escaped the controller via direct kubectl edit)
		// fails the session immediately rather than at first call.
		if sp, ferr := cr.Spec.ToSpec(); ferr != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPConstraintCompileError,
				fmt.Sprintf("MCPServer/%s: ToSpec: %v", ref.Ref, ferr)); werr != nil {
				return werr
			}
			return nil
		} else if _, cerr := mcpspec.Compile(sp); cerr != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPConstraintCompileError,
				fmt.Sprintf("MCPServer/%s: %v", ref.Ref, cerr)); werr != nil {
				return werr
			}
			return nil
		}
		// --- MCP manifest pin verify ---
		// Compute the live canonical hash and record it as an observation
		// regardless of whether drift is enforced. Hashing failures are
		// surfaced via a runner note (never silently dropped).
		liveHash, hashErr := mcppin.CanonicalManifestHash(live)
		if hashErr != nil {
			if werr := statusPatcher.AppendRunnerNote(rootCtx,
				fmt.Sprintf("MCPServer/%s: manifest hash failed: %v", ref.Ref, hashErr)); werr != nil {
				return werr
			}
			// Treat hash as "" — no drift evaluation possible, but continue.
		}

		obsStrength := string(pinning.StrengthUnpinned)
		if cr.Spec.PinnedManifestHash != "" {
			obsStrength = string(pinning.StrengthFrozen)
		}
		if liveHash != "" {
			now := metav1.Now()
			if werr := statusPatcher.RecordObservedPin(rootCtx, ref.Ref, spiceboxv1alpha1.PinRecord{
				Kind: mcppin.KindName, Strength: obsStrength, Digest: liveHash, ObservedAt: &now,
			}); werr != nil {
				return werr
			}
		}

		// Baseline: spec assertion wins; else the reconciler's TOFU record.
		baseline := cr.Spec.PinnedManifestHash
		if baseline == "" && cr.Status.Pin != nil {
			baseline = cr.Status.Pin.Digest
		}
		drifted := liveHash != "" && baseline != "" && liveHash != baseline
		driftSummary := fmt.Sprintf("MCPServer/%s tools/list drifted from pinned baseline (%s -> %s)", ref.Ref, baseline, liveHash)

		var pinReq settings.PinRequirement
		mode := "" // no rule → observe only
		if es := sess.Status.EffectiveSettings; es != nil && es.Pinning != nil {
			pinReq = settings.PinRequirementFor(es.Pinning.Cluster, es.Pinning.Namespace, mcppin.KindName, ref.Ref)
			mode = pinReq.Mode
		}

		withhold, escalate, warn := pinDriftAction(mode, drifted)
		if withhold {
			if werr := statusPatcher.AppendRunnerNote(rootCtx,
				driftSummary+"; mode=block: tools withheld from this session"); werr != nil {
				return werr
			}
			continue // do not synthesize tools from this server
		}
		// --- end pin verify ---

		// Override the CR's metadata.name so the synthesized prefix matches
		// AgentClass.MCPServers[].Name (the LLM-prefix from the AgentClass
		// author), not the CR's own metadata.name. But pass the REAL CR name
		// (cr.Name == ref.Ref, the fetched MCPServer's metadata.name) as the
		// revocation origin: every revoke publisher and the runner's restart
		// filter key on the CR name, so Origin() must match it — not the
		// LLM-prefix — or revocation silently never fires when name != ref.
		cr2 := cr
		cr2.Name = ref.Name
		res, err := mcpdispatch.Synthesize(&cr2, live, mcpdispatch.WithOriginName(cr.Name))
		if err != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift, err.Error()); werr != nil {
				return werr
			}
			return nil
		}
		// built is the LLM-visible set; res.AppTools is the app-visible-only
		// set that is accumulated below into mcpAppTools for Loop.AppTools.
		//
		// The two are kept apart for what they are EXPOSED to — an app-only
		// tool is absent from the model's tool list and reachable solely
		// through an agent-UI data binding — and joined for everything about
		// how a call is MADE. The auth/use_token/reauth wiring below iterates
		// over both for that reason; see its own comment. Pin-drift annotation
		// still applies to `built` alone, since it edits the description the
		// model reads and an app-only tool has no such reader.
		built := res.LLMTools
		// authCredID identifies, for the durable per-call use_token check
		// below, which resolved descriptor produced the (header, value)
		// SetAuth is about to freeze — the descriptor whose Header injection
		// name matches the resolved header. MCP tools declare at most one
		// Header-injected credential: mcpkind.Kind.SetupRequirements returns
		// nil or exactly one requirement (never more), and
		// credresolve.Descriptors maps requirements to descriptors 1:1 — so
		// descs holds 0 or 1 entries here, structurally, not just today. An
		// unauthenticated tool (no descs) or a descriptor injected some other
		// way (EnvVar — not applicable to MCP, but defensive) leaves
		// authCredID empty, and no gate is attached below: there is nothing
		// to check for a credential that was never sent upstream.
		var authCredID string
		for _, d := range descs {
			if d.Inject.Header != nil && header != "" && d.Inject.Header.Name == header {
				authCredID = externaltoken.CredID(d.Source)
				break
			}
		}
		// Attach auth header + slice-4 post-effect plumbing to each
		// MCPTool. relWriter may be nil (SpiceDB unavailable); MCPTool's
		// SetRelWriter / Execute handle that case by skipping the
		// post-effect quietly. labelStore is also wired so the labels
		// post-effect stamps tuples used at approval-time render.
		//
		// Dispatchable(), not `built`: an app-only tool reaches the same server
		// over the same credential, so it needs the same wiring. See its doc
		// for what wiring only the LLM-visible half cost.
		for _, t := range res.Dispatchable() {
			if mt, ok := t.(*mcpdispatch.MCPTool); ok {
				mt.SetAuth(header, value)
				if authCredID != "" {
					// The operator writes/revokes externaltoken authorized_token
					// grants directly in SpiceDB; wiring this gate is what makes a
					// mid-session revoke take effect on the very next call, rather
					// than only at the runner's own resolveIfRevoked/reauth paths.
					mt.SetUseTokenGate(&mcpdispatch.UseTokenGate{
						Checker:     spdbCli,
						SessionNS:   sessKey.Namespace,
						SessionName: sessKey.Name,
						HMACKey:     []byte(argsHashKey),
						CredID:      authCredID,
						FailSession: func(reason, msg string) {
							if werr := failSession(reason, msg); werr != nil {
								slog.Default().Error("mcp use_token gate: recording the terminal failure errored",
									"session", sessKey.Namespace+"/"+sessKey.Name,
									"reason", reason, "err", werr.Error())
							}
						},
					})
				}
				// Bind this tool to every Secret its credential was resolved from,
				// so a credential revoke marks the token SetAuth just froze as
				// stale. Without this the revoke drops a broker cache entry that
				// this tool never reads again.
				for _, d := range descs {
					mcpAuthInv.register(d.Source.Namespace, d.Source.Name, mt)
				}
				if len(descs) > 0 {
					// Survive a mid-session OAuth token rotation: the token
					// SetAuth froze is superseded when the operator does its
					// proactive pre-expiry refresh. On a 401, re-resolve from
					// the refreshed Secret and retry once. InvalidateSecret
					// first so the retry recovers immediately: the broker's
					// cache entry is bounded, but waiting out its TTL would
					// hand back the same stale token until it lapsed.
					reauthDescs := descs
					serverName := cr.Name
					mt.SetReauth(func(ctx context.Context) (string, string, error) {
						for _, d := range reauthDescs {
							if ierr := mcpBroker.InvalidateSecret(d.Source.Namespace, d.Source.Name); ierr != nil {
								slog.Default().Info("mcp reauth: InvalidateSecret errored",
									"server", serverName,
									"secret", d.Source.Namespace+"/"+d.Source.Name,
									"err", ierr.Error())
							}
						}
						res, rerr := mcpBroker.Resolve(ctx, broker.Request{Credentials: reauthDescs})
						if rerr != nil {
							return "", "", rerr
						}
						var h, v string
						for hh, vv := range res.HTTPHeaders {
							h, v = hh, vv
						}
						if h == "" {
							return "", "", fmt.Errorf("mcp reauth: broker returned no auth header for %s", serverName)
						}
						return h, v, nil
					})
				}
				mt.SetRelWriter(relWriter)
				mt.SetSlotBoundChecker(slotBoundChecker)
				mt.SetLabelSink(&fanoutLabelSink{
					primary: labelStore,
					mem:     memSigned,
					scope:   scope,
				})
				mt.SetMemory(memSigned)
				mt.SetLogger(slog.Default())
				// Reuse one MCP session per server across this session's tool
				// calls, so a stateful sidecar (dedicated-mcp) keeps its
				// per-session selection instead of resetting every call.
				mt.SetSessionCache(mcpSessionCache)
			}
		}
		// Apply drift enforcement: warn-prefix description and/or MarkDrifted
		// for per-call approval escalation.
		if warn || escalate {
			warning := fmt.Sprintf("⚠ PIN DRIFT: this tool's definition changed after it was pinned (%s). ", driftSummary)
			for _, t := range built {
				if mt, ok := t.(*mcpdispatch.MCPTool); ok {
					mt.PrependDescription(warning)
				}
				if escalate {
					pinDriftState.MarkDrifted(t.Name(), driftSummary)
				}
			}
			if werr := statusPatcher.AppendRunnerNote(rootCtx, driftSummary+"; mode="+mode); werr != nil {
				return werr
			}
		}
		// Record provenance for every synthesized MCP tool. The observed
		// PinRecord (liveHash recorded above) and the bypass reason from the
		// effective pin requirement are both available at this point.
		mcpEffDrift := ""
		if drifted {
			mcpEffDrift = driftSummary
		}
		for _, t := range built {
			provenanceState.Set(t.Name(), runner.ProvenanceRecord{
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     mcppin.KindName,
					Strength: obsStrength,
					Digest:   liveHash,
				},
				Name:         ref.Ref,
				DriftSummary: mcpEffDrift,
				BypassReason: pinReq.BypassReason,
			})
		}
		mcpTools = append(mcpTools, built...)
		mcpAppTools = append(mcpAppTools, res.AppTools...)
		// ui is read once and shared by the origin below and the rate-limit
		// config that follows it.
		ui := cr.Spec.MCPUIAppTools
		mcpAppOrigins = append(mcpAppOrigins, runner.AppToolOrigin(cr.Name, ui != nil && ui.Enabled, res.AppTools))
		// Rate-limit config keys on cr.Name (the REAL fetched CR's metadata.name),
		// NOT cr2.Name (the LLM-prefix override) — Origin() on the synthesized
		// app-tools is keyed on cr.Name (mcpdispatch.WithOriginName(cr.Name)
		// above), and the two can differ when the AgentClass author's ref name
		// != the MCPServer CR's own name.
		if ui != nil && ui.Enabled && ui.MaxCallsPerMin != nil && *ui.MaxCallsPerMin > 0 {
			appToolRateCfg["mcpserver/"+cr.Name] = *ui.MaxCallsPerMin
		}
	}

	// Build sidecar-toolbox tools.
	//
	// The operator creates the runner pod and any separate-pod sidecar pods in
	// parallel. At runner boot the sidecar pod may not yet be Ready — its
	// SidecarPodIP is empty in the session status snapshot we read at line 174.
	// For those cases we poll the AgentSession until the IP is reflected (bounded
	// by sidecarPodIPTimeout). AwaitingSecret sidecars are skipped entirely: the
	// upstream secret hasn't been produced yet so no pod exists yet; this is not
	// an error.
	const sidecarPodIPTimeout = 120 * time.Second
	const sidecarPodIPPollInterval = 2 * time.Second
	// Boot-probe retry: an in-pod sidecar is a regular pod container that starts
	// in parallel with the runner, so the first MCP dial can beat the sidecar's
	// bind ("connection refused"). Retry within the sidecar's own startup budget
	// (healthcheck.timeoutSeconds, default below) instead of failing the session
	// on one refusal — the sidecar's startupProbe budgets the same window.
	const sidecarBootProbePoll = time.Second
	const sidecarBootProbeDefaultTimeout = 30 * time.Second

	sidecarTools := []tool.Tool{}
	// bootSynthedSidecars records every sidecar synthesized in THIS boot pass,
	// keyed by ResolvedSidecarToolbox.Name and valued by the SidecarPodIP it was
	// synthesized against (empty for in-pod, which has no pod IP). It seeds the
	// mid-session ToolRefresher's synthesized set so a separate-pod sidecar that
	// boot already brought up is not re-synthesized on the first refresh — unless
	// its pod IP later changes (operator replaced the pod on a token rotation),
	// in which case the refresher re-synthesizes against the new pod. The
	// AwaitingSecret entries that boot skips below are deliberately absent: the
	// refresher picks them up once their pod goes Ready mid-session.
	bootSynthedSidecars := map[string]string{}

	// Record every RESOLVED sidecar's auth-failure shape before the synthesis
	// pass below, and deliberately for all of them — including the
	// AwaitingSecret entries that pass skips and the separate-pod ones whose IP
	// has not landed yet. A sidecar's provider is declared right on the resolved
	// spec, so no probe is needed to learn it, and recording it here is what
	// keeps the map boot-only: a toolbox the mid-session refresher brings up
	// later would otherwise need a write from a live session, racing the tool
	// goroutines that read this map with no lock.
	//
	// A sidecar wraps a real MCP tool, so its failures DO carry an HTTP status
	// — it was uncorroborable before only because nothing ever recorded a
	// "sidecartoolbox/<ref>" key for the recorder to find.
	for _, rt := range sess.Status.ResolvedSidecarToolboxes {
		authFailureByOrigin.RecordProvider(originfmt.ForSidecar(rt), rt.Spec.UpstreamAuth.Provider)
	}

	for _, rt := range sess.Status.ResolvedSidecarToolboxes {
		// Resolve the probe URL for this sidecar entry.
		probeURL, ready := sidecarProbeURL(rt, os.Getenv)

		if !ready {
			if rt.AwaitingSecret {
				// Secret not yet produced; sidecar pod doesn't exist yet. Skip
				// silently — this is the normal AwaitingSecret steady state, not
				// a boot failure.
				slog.Info("sidecar toolbox awaiting secret; skipping this pass",
					"sidecar", rt.Name)
				continue
			}
			if rt.RunMode == "separate-pod" {
				// Pod exists (AwaitingSecret==false) but IP not yet reflected in
				// status. Poll the AgentSession until it appears.
				slog.Info("separate-pod sidecar IP not yet reflected; polling",
					"sidecar", rt.Name, "timeout", sidecarPodIPTimeout)
				deadline := time.Now().Add(sidecarPodIPTimeout)
				for time.Now().Before(deadline) {
					select {
					case <-rootCtx.Done():
						return rootCtx.Err()
					case <-time.After(sidecarPodIPPollInterval):
					}
					var refreshed spiceboxv1alpha1.AgentSession
					if rerr := c.Get(rootCtx, sessKey, &refreshed); rerr != nil {
						return fmt.Errorf("poll AgentSession for sidecar %q IP: %w", rt.Name, rerr)
					}
					for _, rrt := range refreshed.Status.ResolvedSidecarToolboxes {
						if rrt.Name == rt.Name {
							rt = rrt
							break
						}
					}
					probeURL, ready = sidecarProbeURL(rt, os.Getenv)
					if ready {
						break
					}
				}
				if !ready {
					// Sidecar pod created (not AwaitingSecret) but never became
					// Ready within the timeout — treat as boot failure.
					if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
						fmt.Sprintf("separate-pod sidecar %q: SidecarPodIP not reflected after %s", rt.Name, sidecarPodIPTimeout)); werr != nil {
						return werr
					}
					return nil
				}
			} else {
				// In-pod sidecar: MCP_PORT_* env missing — always a boot error.
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
					fmt.Sprintf("runner: missing MCP_PORT env for in-pod sidecar %q", rt.Name)); werr != nil {
					return werr
				}
				return nil
			}
		}

		// Probe + synthesize this sidecar's tools via the single shared core
		// (sidecarProbeSynth) — the same path the mid-session refresher uses. It
		// probes the live MCP endpoint, enforces the allowlist, stamps image
		// provenance, and records the per-session reachability on status. A
		// boot-time probe/synth failure is fatal for the session: surface it
		// (never a silent hang) via failSession, mirroring the other boot sites.
		bootTimeout := sidecarBootProbeDefaultTimeout
		if ts := rt.Spec.Transport.Healthcheck.TimeoutSeconds; ts > 0 {
			bootTimeout = time.Duration(ts) * time.Second
		}
		bootProber := runner.RetryingProber(mcpProbeFunc, bootTimeout, sidecarBootProbePoll)
		built, serr := runner.ProbeSynthSidecar(rootCtx, probeURL, rt, bootProber, sess.Status.ObservedPins, provenanceState, statusPatcher, mcpSessionCache)
		if serr != nil {
			if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
				fmt.Sprintf("sidecar %q: %v", rt.Name, serr)); werr != nil {
				return werr
			}
			return nil
		}
		sidecarTools = append(sidecarTools, built...)
		bootSynthedSidecars[rt.Name] = rt.SidecarPodIP
	}

	// Resolve opted-in skills for this agent class. The ceiling enforcement
	// (AllowedSkills/DeniedSkills) is already done upstream by the webhook and
	// controller; the runner trusts class.Spec.Skills as validated. Fail-closed:
	// the AgentClass was only allowed to spawn this session because every
	// opted-in skill was Valid at validation time, so a skill that does not
	// resolve here (most often the runner SA missing skills/clusterskills RBAC)
	// is a real inconsistency — mark the session Failed with a diagnosable
	// reason rather than silently composing a skill-less prompt.
	skillMeta, skillBodies, repoInstrs, serr := resolveSkills(rootCtx, c, ns, class.Spec.Skills)
	if serr != nil {
		slog.Error("resolveSkills: failing session — opted-in skill(s) unresolved",
			"namespace", ns, "skills", class.Spec.Skills, "err", serr.Error())
		if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionSkillResolutionFailed, serr.Error()); werr != nil {
			return werr
		}
		return nil
	}

	// modelCaps is the resolved model's capability set — computed once and
	// shared by the RunnerEnv.ModelCapabilities field below and the
	// nativeFileOut gate. Provider.Capabilities is cheap/no network and
	// returns an empty set for an unknown model id.
	modelCaps := provider.Capabilities(sess.Status.EffectiveSettings.Model.Name)

	// nativeFileOut gates Tier-2 (provider-native) files-out wiring for this
	// session: the resolved model must advertise CapNativeFileOut AND the
	// session's effective settings must have opted in (security-sensitive;
	// default false). Mirrors modality.Env.NativeActive's optIn-AND-capability
	// gate — kept as a plain bool here because RunnerEnv itself doesn't exist
	// yet at this point in construction.
	nativeFileOut := modelCaps.Has(llm.CapNativeFileOut) && sess.Status.EffectiveSettings.NativeFileHandling

	// fileDownloader is a genuine nil interface when Tier-2 files-out isn't
	// active (never a typed-nil *anthropicbridge.SDKFilesClient assigned into
	// the meta.FileDownloader field) — artifact_prepare's nil check treats it
	// as "no downloader" and fails closed on source=container_file.
	var fileDownloader meta.FileDownloader
	if nativeFileOut {
		if ap, ok := provider.(*anthropic.Provider); ok {
			fileDownloader = anthropicbridge.NewSDKFilesClient(ap.SDKClient())
		}
		// Non-anthropic providers advertise no CapNativeFileOut, so this branch
		// is unreachable for them; the nil fileDownloader fails closed.
	}

	// uiViewRT is the runner-side handle backing update_view/read_view — nil
	// for the common case of a class whose AgentClass references no AgentUI,
	// in which case both capabilities skip rather than offering a tool that
	// cannot work (see agentui_views.go). Constructed here, BEFORE
	// capability.Assemble (which reads it via env.UIView below), and attached
	// to the live Loop only once it exists — see runner.AttachUIView's own
	// doc comment for why the two steps cannot merge: capability.Assemble
	// runs before Loop.AppTools is materialized, so the Runtime must exist
	// with nil collaborators first and be wired to the Loop afterward.
	var uiViewRT *uiview.Runtime
	if class.Spec.AgentUI != nil {
		uiViewRT = &uiview.Runtime{
			Namespace: ns, Session: name, UIName: class.Spec.AgentUI.Ref,
			Client: c, Mem: memSigned,
		}
	}

	// artifactReader is the Tier-1 read-by-reference backend for both the
	// files modality's fetch_artifact tool (via env.ArtifactReader below) and
	// the runner loop's attachment-hydration pass (via loop.ArtifactReader,
	// wired further down) — the SAME value, not a second client construction,
	// so both consumers agree on what "reading an artifact" means.
	artifactReader := files.ReaderFromClient(sandbox.NewHTTPArtifactClient(memURL, memToken))

	// Assemble the complete tool list through the capability registry. Each
	// capability decides — from the AgentClass grant (or default-on) combined
	// with runtime availability — whether it is active and which meta tools it
	// contributes. This replaces the former hand-rolled meta.Load()/swap/
	// extraTools/load_skill/introspect assembly: the registry now owns the
	// channel-aware agent_work_complete swap (core), load_skill (skills), and
	// introspect_tool (introspection, last, over the full union).
	// Resolved BEFORE the tool envelope is assembled: the capability registry
	// needs it to decide whether to offer select_phase.
	planGateMode := sess.Status.EffectiveSettings.ResolvedPlanGateMode()
	// An INHERITED ceiling raises the mode to enforcing whatever this session's
	// class said. The parent's ceiling is projected into this session's log at
	// delegation time, but it is inert data unless this runner builds a gate —
	// and that build reads the class's own mode, so a parent under an enforcing
	// gate delegating to a class with the gate disabled handed its child more
	// reach than the parent held. Nothing downstream bounds it: tool authz is
	// per-call against this session's own surface and can itself be permissive,
	// and session scope is per-class.
	//
	// A read failure is FATAL to the raise, not folded into "inherited nothing":
	// answering false on an outage is exactly how the gate would silently stay
	// off. The session continues at its class's mode and says so loudly, so the
	// downgrade is greppable rather than invisible.
	if inherited, ierr := plangate.HasInheritedCeiling(
		memory.WithSystemApproval(rootCtx, "plan_gate"), memSigned, scope); ierr != nil {
		slog.Default().Info("plan gate: could not read this session's log for an inherited ceiling; continuing at the class-resolved mode",
			"session", sess.Name, "mode", planGateMode, "err", ierr.Error())
	} else if raised := plangate.EffectiveMode(planGateMode, inherited); raised != planGateMode {
		slog.Default().Info("plan gate: raised to enforcing by an inherited ceiling",
			"session", sess.Name, "classMode", planGateMode, "mode", raised)
		planGateMode = raised
	}

	// The completion-bypass recorder's channel handle. nil for a kubectl-driven
	// session, where the recorder writes status only and says so in the log —
	// there is no second surface to tell anyone on.
	var bypassPublish channelevents.PublishFunc
	if pubFn != nil {
		bypassPublish = func(subject string, b []byte) error { return pubFn(rootCtx, subject, b) }
	}

	env := capability.RunnerEnv{
		GoalsCaller: func(ctx context.Context, req goalmodel.Request) (goalmodel.Response, error) {
			return memHTTP.Goals(ctx, ns, name, req)
		},
		NATSPublish:      pubFn,
		EnvelopeSigner:   envSigner,
		NATSRequest:      natsRequestFn,
		ChannelAttached:  chanAttached,
		SubjectPrefix:    subjectPrefix,
		InboundCh:        inboundCh,
		PresenceCh:       presenceCh,
		IdleTTL:          cfg.idleTTL,
		Clock:            clock.RealClock{},
		Client:           c,
		Artifacts:        artifactSvc,
		AppendSystemNote: apdNote,
		// A closure over the stamped value rather than the string itself,
		// because the capability's contract is a getter: the address is
		// allowed to arrive late, and a future runner that polls for it should
		// be able to without every consumer changing.
		WebdBaseURL: func() string { return cfg.webdBaseURL },
		// resolve.ForSession result, resolved once in the chanAttached block above.
		ResolvedChannel: resolvedChannel,
		ResolvedSecret:  resolvedSecret,
		ResolvedKind:    resolvedKind,
		ResolveErr:      resolveErr,
		// The class's completion requirements, plus the recorder that makes a
		// bypass of one visible: onto session status for an operator, and onto
		// the channel for the person the round was for.
		CompletionRequirements: class.Spec.CompletionRequirements,
		RecordCompletionBypass: runner.CompletionBypassRecorder(
			statusPatcher, bypassPublish, envSigner,
			channelevents.SessionRef{Namespace: ns, Name: name},
			func(msg string, kv ...any) { slog.Info(msg, kv...) },
		),
		// UserPreferences is static, class-declared data — no late binding
		// needed, unlike PreferencesReader/PreferenceSaver below.
		UserPreferences: class.Spec.UserPreferences,
		// Memory / search / knowledge availability. The operator memory endpoint
		// (memURL — required non-empty at startup) fronts query_memory,
		// search_memory, and query_knowledge alike over HTTP; the runner cannot
		// cheaply tell which backends sit behind it, so a wired endpoint means
		// all three are offered when the capability is granted. Assemble logs any
		// runtime skip; defaulting these true never silently drops a grant.
		MemoryAvailable: memURL != "",
		SearchAvailable: memURL != "",
		KGAvailable:     memURL != "",
		// Skill bodies resolved above; the skills capability injects load_skill
		// only when non-empty.
		SkillBodies: skillBodies,
		// ModelCapabilities lets the artifacts capability's modality-contributed
		// file tools (fetch_artifact et al.) decide their tier from what the
		// resolved model can actually do.
		ModelCapabilities: modelCaps,
		// ArtifactReader backs the files modality's Tier-1 fetch_artifact tool —
		// same HTTP artifact client (operator-backed) used by
		// tool.SessionContext.ArtifactClient below.
		ArtifactReader: artifactReader,
		// FileBridge (Tier-2 provider-native files-in / mount_artifact) is still
		// unwired — no bridge is constructed here, so the files modality only
		// ever offers its Tier-1 surface for files-in even when NativeFileOptIn
		// is settings-granted. FileDownloader (Tier-2 files-out) IS wired below:
		// artifact_prepare(source=container_file) can inline provider-container
		// bytes once nativeFileOut is active.
		FileBridge:      nil,
		FileDownloader:  fileDownloader,
		NativeFileOptIn: sess.Status.EffectiveSettings.NativeFileHandling,
		// RenderFetch / MarkupGen back the artifacts capability's artifact_offer_view.
		RenderFetch: func(ctx context.Context, rns, rsess, render string) ([]byte, string, error) {
			return fetchRenderBytes(ctx, memURL, memToken, rns, rsess, render)
		},
		MarkupGen: markup.NewForProvider(sess.Status.EffectiveSettings.Model.Provider, apiKey).Generate,
		// Late-bound closures: leakageGateFn and loopRef are populated after the
		// Loop is built (below), but these closures fire only at tool-execute
		// time — well after that. Same late-binding the pre-registry code used.
		LeakageGate: func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error {
			if leakageGateFn == nil {
				return nil
			}
			return leakageGateFn(ctx, sess, text, attachments)
		},
		// Late-bound like LeakageGate/OnAwaitYield: the Loop owns the pin set
		// and is built below, but this fires only at tool-execute time.
		PinAttachment: func(ctx context.Context, handle string) error {
			l := loopRef.Load()
			if l == nil {
				return fmt.Errorf("show_attachment: session not ready")
			}
			return l.PinAttachment(ctx, handle)
		},
		// PreferencesReader backs the preferences capability's read tool.
		// turnIndex is late-bound the same way — it reads loopRef only at
		// CALL time, so a read always reflects whatever turn is current when
		// the tool actually runs, not whatever was current (or nonexistent)
		// when this Env was assembled.
		PreferencesReader: runner.NewPreferencesReader(memHTTP, ns, name, func() int {
			if l := loopRef.Load(); l != nil {
				return l.CurrentUserTurnIndex()
			}
			return -1
		}),
		// PreferenceSaver backs the preferences capability's save tool. Unlike
		// every other late-bound field here, the Loop is needed to build the
		// full *runnerHost the publish/await round-trip goes through, not
		// just to call a method on it — see NewLateBoundPreferenceSaver's doc.
		PreferenceSaver: runner.NewLateBoundPreferenceSaver(func() *runner.Loop { return loopRef.Load() }),
		// Late-bound like LeakageGate/PinAttachment: the Loop owns "who is
		// speaking now" and is built below, but this fires only at
		// tool-execute time. spdbCli is a concrete *spicedb.Client and is
		// non-nil by here (SpiceDB is required; a connect failure returned
		// above), but the guard keeps that from being an invisible premise —
		// a nil concrete pointer passed as an InteractChecker is a non-nil
		// interface, and the method's own nil check would not catch it.
		ViewerCanInteract: func(ctx context.Context) (bool, error) {
			l := loopRef.Load()
			if l == nil {
				return false, fmt.Errorf("show_agent_ui: session not ready")
			}
			if spdbCli == nil {
				return false, fmt.Errorf("show_agent_ui: no authorization client")
			}
			return l.CurrentSpeakerCanInteract(ctx, spdbCli, ns, name)
		},
		OnAwaitYield: func(ctx context.Context) {
			if l := loopRef.Load(); l != nil {
				l.OnAwaitYield(ctx)
			}
		},
		OnAwaitResume: func(ctx context.Context) {
			if l := loopRef.Load(); l != nil {
				l.OnAwaitResume(ctx)
			}
		},

		// Plan gate. Same late binding: these fire at tool-execute time, long
		// after the Loop is built.
		PlanGateActive: sess.Status.EffectiveSettings.PlanGateActive(),
		FreezePhases: func(ctx context.Context, phases []plans.Phase) ([]string, error) {
			l := loopRef.Load()
			if l == nil {
				return nil, fmt.Errorf("plan gate is not ready for this session")
			}
			authored, err := runner.PreparePlanReminders(ctx, phases, func(ctx context.Context, req goalmodel.Request) (goalmodel.Response, error) {
				return memHTTP.Goals(ctx, ns, name, req)
			}, sess.Spec.GoalExecution == nil && sess.Spec.Parent == nil)
			if err != nil {
				return nil, err
			}
			notices, err := l.FreezeAndRecordPhases(ctx, authored)
			if err != nil {
				return notices, err
			}
			return notices, l.RequestPlanReminderApproval(ctx)
		},
		ActivePlan: func(ctx context.Context) (plangate.Plan, bool) {
			if l := loopRef.Load(); l != nil {
				return l.ActiveFrozenPlan(ctx)
			}
			return plangate.Plan{}, false
		},
		RecordPhaseSelection: func(ctx context.Context, index int) error {
			l := loopRef.Load()
			if l == nil {
				// A selection that cannot be recorded must not report success —
				// the agent would believe it holds a ceiling the fold never saw.
				return fmt.Errorf("plan gate is not ready for this session")
			}
			return l.RecordPhaseSelection(ctx, index)
		},
		RecordPhaseCompletion: func(ctx context.Context, index int, outcome string) error {
			l := loopRef.Load()
			if l == nil {
				// A completion that cannot be recorded must not report success:
				// the agent would believe a dependent phase is unblocked when the
				// fold will not agree.
				return fmt.Errorf("plan gate is not ready for this session")
			}
			return l.RecordPhaseCompletion(ctx, index, outcome)
		},
		PhaseCompletion: func(ctx context.Context) map[int]bool {
			if l := loopRef.Load(); l != nil {
				return l.PhaseCompletionMap(ctx)
			}
			return nil
		},
		PhaseEntered: func(ctx context.Context) map[int]bool {
			if l := loopRef.Load(); l != nil {
				return l.PhaseEnteredMap(ctx)
			}
			return nil
		},
		PhaseEntries: func(ctx context.Context) map[int]int {
			if l := loopRef.Load(); l != nil {
				return l.PhaseEntryCounts(ctx)
			}
			return nil
		},

		WorkspaceSource: workspaceSourceRuntime(&sess),
		// UIView backs the update_view/read_view capabilities — see uiViewRT's
		// construction above for why it must exist before this Assemble call.
		UIView: uiViewRT,
		// CredentialUpdateClient reuses the same direct (uncached) client every
		// other RunnerEnv.Client use does — request_credential_update both
		// creates and polls CredentialUpdateRequest CRs, so a cached client
		// (were one in play here) would risk a stale read.
		CredentialUpdateClient: c,
		// SubagentCreate/SubagentPoll back the delegate tool's create-then-poll
		// round trip against SubagentRequest. Reuses the same direct (uncached)
		// client c as CredentialUpdateClient above, for the identical
		// create-then-immediately-poll reason: a cached Get issued moments
		// after the Create could read an informer that has not observed it yet.
		SubagentCreate: func(ctx context.Context, sr *spiceboxv1alpha1.SubagentRequest) error {
			// Owner-ref to the PARENT session (sess, loaded above), not the
			// request itself -- without this, deleting the parent mid-delegation
			// leaves the request and any already-created child orphaned: the
			// child's own owner ref names the request, not the parent, and the
			// SubagentRequest controller never revisits the parent's existence
			// once status.childRef is set. An orphaned child's spec.parent then
			// names a session that no longer exists, so its standing resolves to
			// nobody -- unclaimed, unapprovable, unstoppable. See
			// meta.DelegateOwnerReference's doc for the full trace.
			sr.OwnerReferences = append(sr.OwnerReferences, meta.DelegateOwnerReference(&sess))
			return c.Create(ctx, sr)
		},
		SubagentPoll: func(ctx context.Context, srName string) (*spiceboxv1alpha1.SubagentRequest, error) {
			var sr spiceboxv1alpha1.SubagentRequest
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: srName}, &sr); err != nil {
				return nil, err
			}
			return &sr, nil
		},
		// SubagentTimeout bounds how long ONE delegate or reply_to_subagent
		// call blocks waiting on a child session. Not derived from cfg.idleTTL,
		// which sizes await_user_message's much shorter human-wait window; a
		// delegated child can legitimately run its own full turn loop. Shares
		// meta.DefaultTimeout with NewDelegateTool's own zero-value fallback and
		// the e2e in-process factory's identical wiring, rather than repeating
		// the literal at three sites.
		SubagentTimeout: meta.DefaultTimeout,
		// SubagentSend backs reply_to_subagent: one KindAgentMessageSend
		// envelope onto THIS session's OWN inbound bus subject, naming the
		// destination child in the payload (see subagentSendFunc and
		// channelevents.KindAgentMessageSend). Its own subject, because that is
		// the only prefix runnerNATSUserGrant lets a runner publish on — and
		// the inversion is the point: the subject authorizes the SENDER, and
		// channelsd corroborates the destination against the single Channel
		// joining the pair before gating it on agentsession#converse.
		//
		// It is NOT the path a child's message to its parent rides. A child
		// reaches its parent through ask_parent, which writes the question onto
		// its own AgentSession status for the SubagentRequest controller to
		// mirror — that direction never touches the bus at all.
		//
		// Left nil when there is no bus (a kubectl-driven parent), which the
		// tool refuses explicitly rather than publishing into the void.
		//
		// Deliberately NOT inside the chanAttached block below: the pair
		// Channel is bound to the CHILD, and the parent is only its
		// counterparty, so a parent needs a bus connection to answer a child
		// but no channel binding of its own.
		SubagentSend: subagentSendFunc(natsRT, ns, name, envSigner),
		// Backs delegate's `inputs`: resolves a tool_use_id from this session
		// to the tag minted for it, so a data slot is filled without the model
		// ever naming a tag. memSigned rather than a bare store for the same
		// reason every other read of this scope uses it.
		SubagentResolveDataTag: runner.ResolveDataTagFor(memSigned, scope),
		// Backs send_input. Appends to the request's SPEC — which this session
		// owns, having created it — after checking it is in fact ours, and
		// leaves the attenuation and grading checks to the controller.
		SubagentBindDataSlot: runner.BindDataSlotFor(c, ns, name),
		// AskParent backs the child half, ask_parent: it records the question
		// on this session's OWN status, which the SubagentRequest controller
		// mirrors onto the request its parent is polling. The child never
		// writes that request itself — RBAC cannot scope a status write to one
		// field, so a runner able to write subagentrequests/status could
		// fabricate a Denied or Succeeded delegation.
		//
		// Wrapped in the delegation's exchange budget so a question that
		// controller would refuse is refused HERE, to the model, in the same
		// turn. The wrapper is legibility only; see askParentBudgetGuard for
		// why it cannot be, and is not, the ceiling.
		AskParent: runner.AskParentBudgetGuard(c, &sess, statusPatcher.AskParent),
		// RequestInput backs the child's OTHER half, request_input: a
		// mid-flight ask for DATA rather than for words. It was declared on
		// RunnerEnv and wired at neither site, so the tool was offered with a
		// nil Record and refused every call — a capability an agent could see
		// and never use.
		//
		// Unguarded, unlike AskParent: a question PARKS the child and consumes
		// a parent exchange, so it needs a budget; asking for data neither
		// parks nor obliges anyone, and the bound on what can arrive is the
		// attenuation check at binding, not the number of asks.
		RequestInput: func(ctx context.Context, slot, why string) error {
			_, err := statusPatcher.RequestInput(ctx, slot, why)
			return err
		},
		// MintDerivedTag + TagAccessCheck back derive_tag (fine_grained_info_leakage).
		// The mint routes through the operator like every other pt_tag write —
		// pt_tag is component-written so a session cannot author its own reader
		// set — with DerivedFrom set, so the minter takes its NewDerived branch
		// (readers = the inputs' intersection). The access check gates each input
		// against this session's pt_tag#access (session + ancestors + granted_to),
		// so a model cannot derive from a tag it was never granted.
		MintDerivedTag: func(ctx context.Context, derivedFrom []string, content string) (string, error) {
			// Store content so egress content-binding accepts the pasted region.
			return memHTTP.MintPtTag(ctx, scope, memory.PtTagMintRequest{
				DerivedFrom: derivedFrom, Content: content, MIME: "application/json",
			})
		},
		TagAccessCheck: func(ctx context.Context, tagID string) (bool, error) {
			return leakagewiring.NewSpiceDBCheck(spdbCli)(ctx, "agentsession:"+scope.ID, "access", "pt_tag:"+tagID)
		},
		ResolveTagContents: func(ctx context.Context, ids []string) ([]capability.DeriveSource, error) {
			// Ask the operator, not the content store directly: pt_tag_content is
			// component-read, so this session-credentialed runner cannot read it
			// over the memory API. The operator gates each id on pt_tag#access —
			// harmless here (TagAccessCheck already gated every input above) but
			// the route's own invariant — and returns the entitled bytes.
			contents, err := memHTTP.ResolvePtTags(ctx, scope, ids)
			if err != nil {
				return nil, err
			}
			byTag := make(map[string]string, len(contents))
			for _, c := range contents {
				byTag[c.TagID] = c.Content
			}
			out := make([]capability.DeriveSource, 0, len(ids))
			for _, id := range ids {
				out = append(out, capability.DeriveSource{TagID: id, Content: byTag[id]})
			}
			return out, nil
		},
		// ToolLookup is late-bound the same way LeakageGate is: this wrapper is
		// captured by credential_update's Offer during capability.Assemble
		// below, while toolLookupFn itself is still nil. It is only ever
		// CALLED much later, at tool-execute time, by which point
		// toolLookupFn has been set (right after Assemble, below).
		ToolLookup: func(name string) (tool.Tool, bool) {
			if toolLookupFn == nil {
				return nil, false
			}
			return toolLookupFn(name)
		},
	}

	// bindingOrNil is the bound ChannelBinding when channel-attached, else nil;
	// capabilities gate their channel-only tools on Binding != nil.
	var bindingOrNil *spiceboxv1alpha1.ChannelBinding
	if chanAttached {
		bindingOrNil = sess.Spec.InputChannel
	}

	// derive_tag's derivation validator is a DEDICATED model — its own provider,
	// model id, and credential, separate from the session's — so a prompt
	// injection in the agent's context cannot also steer the judge (it is called
	// with a clean context by derivevalidator). Configured via its own env vars;
	// unset or misconfigured leaves DeriveValidator nil, and derive_tag is then
	// not offered (fail-closed: no unvalidated derivation).
	if vModel := os.Getenv("PT_DERIVE_VALIDATOR_MODEL"); vModel != "" {
		vProvider, vErr := providers.New(os.Getenv("PT_DERIVE_VALIDATOR_PROVIDER"), os.Getenv("PT_DERIVE_VALIDATOR_API_KEY"))
		if vErr != nil {
			slog.Default().Info("derive-tag validator misconfigured; derive_tag disabled (fail-closed)",
				"model", vModel, "err", vErr.Error())
		} else {
			env.DeriveValidator = derivevalidator.New(vProvider, vModel)
		}
	}

	assembled := capability.AssembleAll(rootCtx, capability.AssembleDeps{
		Class:        &class,
		Session:      &sess,
		Binding:      bindingOrNil,
		Env:          env,
		NonMetaTools: append(append(append([]tool.Tool{}, sandboxTools...), mcpTools...), sidecarTools...),
		Logger:       logr.FromSlogHandler(slog.Default().Handler()),
	})
	mergedTools := assembled.Tools
	if sess.Spec.GoalExecution != nil {
		mergedTools = meta.BoundedGoalTools(mergedTools, sess.Spec.GoalExecution.ConsentDigest, func(ctx context.Context) error {
			_, err := memHTTP.Goals(ctx, ns, name, goalmodel.Request{Operation: "authorize_execution"})
			return err
		}, func(ctx context.Context, req goalmodel.Request) (goalmodel.Response, error) {
			return memHTTP.Goals(ctx, ns, name, req)
		})
	}

	// toolLookupFn late-binds now that mergedTools is final, so a
	// request_credential_update call mid-session resolves against the complete
	// LLM-facing tool table (meta + non-meta + introspection), not a partial
	// snapshot captured mid-assembly.
	toolLookupFn = tool.LookupByName(mergedTools)

	// Fail-closed envelope check. A duplicate tool name silently shadows at
	// dispatch (byName[t.Name()] = t), so it is always fatal.
	//
	// An unhandleable name is fatal ONLY WHEN THE PLAN GATE ENFORCES, and that
	// escalation is now live rather than promised. Such a tool is absent from
	// the permission surface yet still callable, and the gate's no-handle
	// branch records OutcomeAllow for it — so with enforcement on it would be
	// the one thing that walks past a gate everything else is measured by.
	// Under disabled/logging it stays a warning, because a cluster that gates
	// nothing is unharmed and refusing to start over a working name would be a
	// regression for it.
	surfaceEnforced := planGateMode == "enforcing"
	envelopeProblems := tool.ValidateEnvelope(mergedTools)
	for _, p := range envelopeProblems {
		slog.Default().Error("tool envelope problem",
			"session", sess.Name,
			"tool", p.Tool,
			"kind", string(p.Kind),
			"fatal", p.Fatal || (surfaceEnforced && p.Kind == tool.ProblemUnhandleable),
			"planGateMode", planGateMode,
			"detail", p.Detail,
		)
	}
	if tool.FatalFor(envelopeProblems, surfaceEnforced) {
		return fmt.Errorf("tool envelope is invalid: a duplicate name would silently shadow at dispatch, or "+
			"(with the plan gate enforcing) a tool cannot mint a permission handle and would be callable while "+
			"absent from the surface the gate enforces against; planGateMode=%q — see the logged problems above",
			planGateMode)
	}

	// Plan gate. The mode comes from the RESOLVED settings, not from
	// class.Spec.GetAuthz() like the other authz reads in this file: the mode is
	// ceiling-clamped in the settings fold, and reading the class spec would let
	// a class opt out past a cluster floor by being the only value consulted.
	//
	// The SEED plan is synthesized as ONE phase whose ceiling is the whole live
	// surface, so the membership test always passes until the agent authors and
	// freezes real phases. A session that never plans therefore behaves exactly
	// as it would with the gate off — which is what `requirePlan` exists to
	// close when that is not wanted.
	planGateSurface := permsurface.Enumerate(tool.Candidates(mergedTools))
	// Counted per TOOL-that-can-ever-be-gated, not per base handle. A sandbox
	// tool's base permission is its toolkit default, so counting base handles
	// reported gh as ungateable and logged `surfaceHandles=2 gatedTools=0` —
	// handles found, none usable — which reads as a failure and is not one.
	gatedTools := 0
	for _, t := range mergedTools {
		if tool.CanBeGated(t) {
			gatedTools++
		}
	}
	if planGateMode != "" && planGateMode != "disabled" {
		slog.Default().Info("plan gate active",
			"session", sess.Name,
			"mode", planGateMode,
			"surfaceHandles", len(planGateSurface),
			"gatedTools", gatedTools)
	}

	// Prompt asset + modality instructions are both gated on the artifacts
	// capability having actually injected its tools — presence of
	// artifact_prepare in the merged set IS the grant+availability signal, so
	// we do not re-derive the grant here. When present, map the channel's
	// renderer kinds to prompt AssetKind entries (name + kind-owned
	// Instructions), and collect every registered modality's own prompt
	// guidance for the same resolved capabilities/backends that fed
	// artifactsCapability.Offer; otherwise pass none of either.
	var assetKinds []runner.AssetKind
	if toolListHas(mergedTools, "artifact_prepare") {
		for _, k := range capability.AvailableAssetKinds(bindingOrNil) {
			if r, ok := assetregistry.ByKind(k); ok {
				assetKinds = append(assetKinds, runner.AssetKind{Name: r.Kind(), Instructions: r.Instructions()})
			}
		}
	}

	// Modality instructions are gated SEPARATELY from assetKinds: the
	// attachments capability (pkg/agent/tool/meta/capability/attachments.go)
	// can inject a modality's meta tool (fetch_artifact) without the artifacts
	// capability ever being granted, so "artifact_prepare present" is no
	// longer a valid proxy for "some modality tool is present" — an
	// attachments-only agent would otherwise get fetch_artifact in its tool
	// table but no prompt guidance on how to use it. ModalityInstructions
	// checks each registered modality's OWN contributed tool name(s) against
	// mergedTools instead of re-deriving either capability's grant.
	modalityInstructions := runner.ModalityInstructions(env.ModalityEnv(), mergedTools)

	// user_profile: per-turn speaker-profile injection. userprofilegate.Offer
	// is the single decision point — declines unless the AgentClass granted
	// user_profile AND the resolved kind can serve profiles AND (gate.go's
	// field intersection) it can populate at least one configured field.
	// Resolved here, ahead of ComposeSystem, so the SAME profileActive value
	// feeds both consumers: it gates fetchProfile/profileFields on the Loop
	// below, and it is passed straight into ComposeSystem to gate the
	// profile-marker prompt paragraph. One call, two consumers — a second
	// Offer call anywhere else would risk drifting from this decision.
	fetchProfile, profileFields, profileActive := userprofilegate.Offer(
		logr.FromSlogHandler(slog.Default().Handler()), &class, resolvedKind, resolvedSecret)

	// Auto-inject the tool catalog and the agent_work_complete protocol on top
	// of the AgentClass-supplied prompt. The user's prompt should describe the
	// agent's role and task; the runner contributes the runtime contract.
	composeOpts := []runner.ComposeOption{}
	composeOpts = append(composeOpts, runner.WithPlannableHandles(permsurface.Handles(planGateSurface)))
	composeOpts = append(composeOpts, runner.WithPlannableSurface(planGateSurface))
	composeOpts = append(composeOpts, runner.WithAuthoredExamples(runner.AuthoredExamplesOf(&class)))
	composeOpts = append(composeOpts, runner.WithPlanningNotes(runner.PlanningNotesOf(&class)))
	composeOpts = append(composeOpts, runner.WithUserProfileActive(profileActive))
	composeOpts = append(composeOpts, runner.WithFineGrainedInfoLeakage(runner.FineGrainedInfoLeakageActive(&class)))
	if runner.PlanGateRequirePlan(sess.Status.EffectiveSettings) {
		composeOpts = append(composeOpts, runner.WithRequirePlan())
	}
	// Capability-owned prompt sections ride with the tools they describe
	// (capability.SectionOfferer): the views capability's "Your page" exists
	// only when update_view does. Nothing here decides whether to include one.
	composeOpts = append(composeOpts, runner.WithPromptSections(assembled.Sections))
	composedSystem := runner.ComposeSystem(systemPrompt, mergedTools, skillMeta, repoInstrs, assetKinds, modalityInstructions, composeOpts...)

	// Record WHAT the agent was asked, alongside the turn log's record of WHICH
	// model was asked. Without it the chain can prove what an agent did and not
	// say under what instructions, so an investigator cannot separate a prompt
	// regression from a model one — and repoInstrs / skillMeta / assetKinds
	// compose into this text, making it attack surface whose alteration would
	// otherwise leave no trace in a record built to be tamper-evident.
	//
	// Set-once per distinct prompt (the accessor keys the entry on its digest),
	// so this is one write per session in the ordinary case and a second only if
	// repo instructions or skills move mid-session.
	//
	// Best-effort: a failure here must not take down a session that is otherwise
	// able to run, but it is logged loudly because a missing prompt record is
	// exactly the blind spot this closes.
	if err := systemprompt.Record(rootCtx, memSigned, scope, composedSystem); err != nil {
		slog.Default().Info("systemprompt.Record failed; the session runs but its prompt is unrecorded",
			"session", scope.ID, "err", err.Error())
	}

	// Resolve the toolguard policy from the per-tier shapes the operator stamped
	// onto status.effectiveSettings + the AgentClass-level rules. Default-on: the
	// builtin breaker rule applies even when no tier configures anything, so the
	// policy is always non-nil here and the guard hooks always register.
	var effTG *spiceboxv1alpha1.EffectiveToolGuard
	if sess.Status.EffectiveSettings != nil {
		effTG = sess.Status.EffectiveSettings.ToolGuard
	}
	tgTiers := toolguard.Tiers{Class: class.Spec.ToolGuard}
	if effTG != nil {
		tgTiers.Namespace, tgTiers.Cluster, tgTiers.Ceiling = effTG.Namespace, effTG.Cluster, effTG.Ceiling
	}
	toolGuardPolicy, err := toolguard.ResolvePolicy(tgTiers)
	if err != nil {
		// Fail closed: an invalid glob in a tier rule is a session-start config
		// error. Matches the file's idiom for fatal startup errors — write the
		// terminal Failed status and exit clean.
		if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionToolGuardHalt,
			fmt.Sprintf("toolguard: resolve policy: %v", err)); werr != nil {
			return werr
		}
		return nil
	}

	var cgInstances []contentguard.Instance
	var cgIDs []string
	if es := sess.Status.EffectiveSettings; es != nil {
		for _, ci := range es.ContentInspectors {
			insp, ok := cgregistry.Get(ci.ID)
			if !ok {
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
					fmt.Sprintf("content inspector %q is not registered", ci.ID)); werr != nil {
					return werr
				}
				return nil
			}
			inst, err := insp.Configure(ci.Config.Raw)
			if err != nil {
				if werr := failSession(spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt,
					fmt.Sprintf("content inspector %q: invalid config: %v", ci.ID, err)); werr != nil {
					return werr
				}
				return nil
			}
			cgInstances = append(cgInstances, inst)
			cgIDs = append(cgIDs, ci.ID)
		}
	}

	// EchoPublish is maybeEchoAnnotationTurn's raw NATS publish handle (D4,
	// plan D4 task 3). pubFn is assigned unconditionally inside the
	// chanAttached block above (see natsPublishFunc), so pubFn != nil is
	// equivalent to chanAttached — mirrors every other buildLoop* helper's
	// nil-when-not-channel-attached contract. Left nil here (rather than a
	// closure that would call a nil pubFn and panic) for kubectl-driven
	// sessions and tests: Loop.EchoPublish's own nil check then logs and
	// skips the echo instead of dropping it silently or crashing.
	var echoPublish channelevents.PublishFunc
	if pubFn != nil {
		echoPublish = func(subject string, b []byte) error { return pubFn(rootCtx, subject, b) }
	}

	// appTools is the SINGLE choke point Loop.AppTools is assigned through:
	// runner.MaterializeAppTools applies the fail-closed three-way grant
	// (pkg/web/uigrant.Materialize) — what the AgentUI requested
	// (agentUIRequestedTools), what each origin permits (mcpAppOrigins), and
	// what the AgentClass granted (class.Spec.AgentUI) — before mcpAppTools
	// ever reaches the registry a browser can call through. internal/cmd/runner and
	// test/e2e's in-process harness both call runner.MaterializeAppTools so
	// the harness's enforcement cannot silently drift from production's.
	appTools := appToolsByName(runner.MaterializeAppTools(slog.Default(), runner.AppToolsLogContext{
		Session:    sessKey.Namespace + "/" + sessKey.Name,
		AgentClass: ns + "/" + sess.Spec.Class,
		AgentUI:    agentUIKey,
	}, agentUIRequestedTools, mcpAppOrigins, class.Spec.AgentUI, mcpAppTools))

	// Whether agent_work_complete completes this session or parks it Idle —
	// see runner.Loop.DelegatedChild. Fatal rather than defaulted on an
	// unresolvable binding kind: either default is silently wrong for one of
	// the two shapes, and every kind is blank-imported above (guarded by
	// channelkinds_registered_test), so an error here is a wiring bug and not
	// a runtime condition to degrade through.
	delegatedChild, err := spiceboxv1alpha1.IsDelegatedChild(&sess, chregistry.AllowsSessionCounterparty)
	if err != nil {
		return fmt.Errorf("resolve whether this session is a delegated child: %w", err)
	}

	loop := &runner.Loop{
		Provider: provider,
		Memory:   turnAppender,
		Mem:      memSigned,
		// LifecycleMemory is the session-signed facade the runner sequencer
		// Puts typed lifecycle transition events through (publisher
		// session:<ns/name>). Same signing memory as Mem; named separately for
		// the sequencer's call sites. See pkg/agent/runner/sequencer.go.
		LifecycleMemory: memSigned,
		KG:              httpclient.NewKGClient(memHTTP, scope),
		Status:          statusPatcher,
		Tools:           mergedTools,
		// AppTools is the separate MCP-UI app-visible-only registry — NEVER
		// merged into mergedTools/l.Tools, so buildToolDefs (which iterates
		// l.Tools only) structurally never offers these to the LLM. See appTools
		// above for how it was materialized.
		AppTools: appTools,
		// AppToolRateLimiter enforces mcpUiAppTools.maxCallsPerMin on the
		// autonomous app-tool call path only (HandleAppToolCall) — deliberately
		// NOT via toolguard, which would strip the origin's circuit breaker.
		AppToolRateLimiter: runner.NewAppToolRateLimiter(appToolRateCfg, time.Now),
		System:             composedSystem,
		UserPrompt:         userPrompt,
		Budget:             runner.NewBudget(budgetCfg, runClock, sessionStartedAt),
		RunClock:           runClock,
		Model:              sess.Status.EffectiveSettings.Model.Name,
		Routing:            routingToLLM(sess.Status.EffectiveSettings.ModelRouting),
		ReportSessionCost:  sess.Status.EffectiveSettings.ReportSessionCost,
		RecordSessionCost:  sess.Spec.GoalExecution != nil,
		ModelInputPerMTok:  sess.Status.EffectiveSettings.ModelInputPerMTok,
		ModelOutputPerMTok: sess.Status.EffectiveSettings.ModelOutputPerMTok,
		UserID:             string(sess.UID),
		// Per-LLM-call output cap. Sized for agents that emit large
		// inline payloads via tool args — notably artifact_prepare's
		// `payload` field which carries an entire HTML document as
		// a JSON-escaped string. With streaming enabled (anthropic
		// provider routes through Messages.NewStreaming), the SDK's
		// non-streaming cap (21333 / 8192 for Opus 4 variants) no
		// longer applies. 65536 gives substantial headroom for
		// HTML payloads. Lifetime spend is still bounded by
		// AgentClass.spec.budget.maxTokens.
		MaxTokens:                65536,
		SessionKey:               memKey,
		ChannelAttached:          chanAttached,
		DelegatedChild:           delegatedChild,
		AgentName:                sess.Spec.Class,
		Notify:                   buildLoopNotify(chanAttached, natsRT, &sess, envSigner),
		OnToolDispatch:           buildLoopToolDispatch(chanAttached, natsRT, &sess, envSigner),
		OnToolProgress:           buildLoopToolProgress(chanAttached, natsRT, &sess, envSigner),
		PublishTurnActivity:      buildLoopTurnActivity(chanAttached, natsRT, &sess, envSigner),
		OnStreamEvent:            buildLoopOnStreamEvent(chanAttached, natsRT, &sess, envSigner),
		ProgressPublish:          buildLoopTurnProgress(chanAttached, natsRT, &sess, envSigner),
		OperationActivityPublish: buildLoopOperationActivity(chanAttached, natsRT, &sess, envSigner),
		ToolAuthMode:             class.Spec.GetAuthz().GetToolCalls().Mode,
		DisabledNotify:           buildLoopDisabledNotify(chanAttached, natsRT, &sess, envSigner),

		// Plan gate. Mode comes from the resolved settings (see
		// EffectiveSettings.ResolvedPlanGateMode); the plan here is only the SEED — the synthesized
		// one-phase plan over the live surface, replaced once the agent freezes
		// phases of its own.
		PlanGateMode:               planGateMode,
		PlanGatePlan:               plangate.SessionPlan(planGateSurface),
		PlanGateSurface:            planGateSurface,
		PlanGateSlotTypes:          class.Spec.SlotResourceTypes(),
		PlanGateSlotPermissions:    class.Spec.SlotPermissions(),
		PlanGateSlotPermissionSets: class.Spec.SlotPermissionSets(),
		PlanGateSlotTransforms:     runner.SlotTransformsOf(&class),
		PlanGateSlotStanding:       runner.SlotStandingOf(&class),
		PlanGatePermissionTitles:   runner.PermissionTitlesOf(&class),
		PlanGateResourceDisplays:   runner.ResourceDisplaysOf(&class),
		ResourceStandings:          runner.ResourceStandingsOf(&class),
		PlanGateMaxCardHandles:     runner.PlanGateMaxCardHandles(sess.Status.EffectiveSettings),
		PlanGateMaxAutoApprove: func() int {
			if sess.Spec.GoalExecution != nil {
				return 0
			}
			return runner.PlanGateMaxAutoApprove(sess.Status.EffectiveSettings)
		}(),
		PlanGateRequirePlan: runner.PlanGateRequirePlan(sess.Status.EffectiveSettings),
		PlanApprovalDeriver: func() func(context.Context, plangate.Plan, int) (*plangateaudit.ApprovalAuthority, error) {
			if sess.Spec.GoalExecution == nil {
				return nil
			}
			return runner.GoalPlanApprovalDeriver(func(ctx context.Context, req goalmodel.Request) (goalmodel.Response, error) {
				return memHTTP.Goals(ctx, ns, name, req)
			})
		}(),

		// Live CR pointers, so the loop can dispatch autofill and
		// per-user-message binding. Nil disables those paths, as a defence
		// against partial wiring; see Loop.AgentClass / Loop.AgentSession.
		AgentClass:   &class,
		AgentSession: &sess,

		// Client/Artifacts back applyUIResource's widget persistence (an MCP
		// tool result carrying a UIResource is turned into a durable
		// ArtifactRender{Kind:"mcpui"} artifact). Same c/artifactSvc locals
		// capability.RunnerEnv.Client/.Artifacts are wired from above.
		Client:    c,
		Artifacts: artifactSvc,

		// ArtifactReader backs the attachment-hydration pass's native-block
		// reads. Same value as capability.RunnerEnv.ArtifactReader above — not
		// a second client construction.
		ArtifactReader: artifactReader,

		// IdentityGatePending registers the SessionStart IdentityChoiceGate for a
		// first-boot ask|dynamic session (computed above with effectiveMode). False
		// for static modes and re-spawns past the choice.
		IdentityGatePending: gatePending,

		// Isolated summarizer LLM for the approver-facing "What" line.
		// See pkg/agent/runner/approval/summarizer (package docstring
		// + memory/project_approval_summarizer_llm.md) for the threat
		// model — this is NOT a method on the primary LLM; it's a
		// separate Haiku/gpt-5.4-mini-class client that only ever sees
		// the tool schema + args (never primary chat history or tool
		// outputs). NewForProvider selects the summarizer backend to
		// match the session's primary LLM provider (Anthropic sessions
		// get an Anthropic-key summarizer, OpenAI sessions an OpenAI-key
		// one) — reuses the runner's API key; cheap calls.
		ApprovalSummarizer: summarizer.NewForProvider(sess.Status.EffectiveSettings.Model.Provider, apiKey),

		// AnnotationSummarizer is the same isolated summarizer LLM construction
		// as ApprovalSummarizer, reused for the browser annotation-batch echo
		// (D4 plan, task 3). NewForProvider's Provider now also implements
		// SummarizeAnnotations; maybeEchoAnnotationTurn falls back to
		// summarizer.FallbackAnnotationSummary when this is nil or errors.
		AnnotationSummarizer: summarizer.NewForProvider(sess.Status.EffectiveSettings.Model.Provider, apiKey),

		// EchoPublish wires maybeEchoAnnotationTurn's KindUserEcho mirror to the
		// runner's NATS connection; nil for non-channel-attached sessions (see
		// echoPublish construction above).
		EchoPublish: echoPublish,

		// EnvelopeSigner signs every envelope this Loop publishes with the
		// session's identity key (constructed once, above, after the
		// AgentSession Get).
		EnvelopeSigner: envSigner,

		// PinDrift carries the set of tools whose MCP dependency drifted
		// from its pin baseline in "approve" mode. Populated by the MCP
		// boot loop above; consulted per-call by the ToolCallAuthz hook
		// to force per-call user approval for those tools.
		PinDrift: pinDriftState,

		// Provenance maps LLM tool name → pin-origin facts for every synthesized
		// tool. Populated by the bundle, MCP, and sidecar loops above; consumed
		// by recordAuthzDecision to stamp pin facts onto each authz_decision audit
		// entry.
		Provenance: provenanceState,

		// ToolGuardPolicy is the session-start-resolved breaker/rate-limit policy
		// (default-on via the builtin rule). Drives the guard hooks in the runner
		// pipeline.
		ToolGuardPolicy: toolGuardPolicy,

		// AuthFailures records the platform's OWN observation that a tool call
		// failed the way this provider's credentials fail — the corroboration a
		// CredentialUpdateRequest needs when the provider cannot be re-probed.
		// authFailureByOrigin is read-only from here on.
		//
		// Seeded from status so a RE-SPAWNED runner can clear an observation its
		// predecessor wrote; without the seed a later success finds nothing in
		// memory, skips the clear, and leaves a working credential corroborated
		// as broken for the rest of the session.
		//
		// test/e2e/inprocess_runner_factory.go MUST wire this field identically,
		// or the e2e suite exercises a different runner than production ships.
		AuthFailures: authfail.New(statusPatcher, authFailureByOrigin.Lookup, sess.Status.CredentialAuthFailures),

		// ContentInspectors are the session-start-resolved content-guard instances
		// built from status.effectiveSettings.contentInspectors. Empty when no
		// content inspectors are configured (default-off).
		ContentInspectors:   cgInstances,
		ContentInspectorIDs: cgIDs,

		// RevokedOrigins is nil here for non-channel-attached (kubectl) sessions;
		// it is set inside the chanAttached+NATS block below, together with the
		// ap.revocation subscriber that writes to it. nil means the
		// revocation_guard hook is disabled for sessions where no subscriber will
		// ever write revocations.

		// ArgsHashKey is the per-session HMAC key used to sign grant
		// arguments_hash bindings. Read from the pod-mounted secret at
		// startup; startup fails closed if absent or empty.
		ArgsHashKey: []byte(argsHashKey),
	}
	// AttachUIView wires the late-bound ToolOptions collaborator now that the
	// Loop exists; it cannot happen earlier, because Loop.AppTools is not
	// materialized until capability.Assemble has run.
	//
	// Read env.UIView, deliberately NOT the uiViewRT local — env.UIView is what
	// capability.Assemble actually saw. Because of this, deleting the
	// `UIView: uiViewRT` field from the env literal leaves uiViewRT unused and
	// breaks the build, instead of silently opening a capability black hole that
	// skips both tools for every session with no test catching it. Do not
	// reintroduce a bare uiViewRT read here.
	if env.UIView != nil {
		runner.AttachUIView(env.UIView, loop)
	}
	// code_execution is the Anthropic server tool that backs Tier-2
	// files-out: the model uses it to write files into its own
	// code-execution container, which artifact_prepare(source=container_file)
	// then pulls out via fileDownloader above. Declared only when the files-out
	// artifact path is actually usable: nativeFileOut (model+settings opted in)
	// AND the artifacts capability actually injected artifact_prepare into the
	// merged tool set. Without the second check, an AgentClass that opts into
	// native file handling but lacks the artifacts capability would get a live
	// code_execution tool with no prompt guidance and no way to surface its
	// output — a dead-end tool offered to the model.
	if nativeFileOut && toolListHas(mergedTools, "artifact_prepare") {
		loop.ExtraToolDefs = append(loop.ExtraToolDefs, llm.ToolDef{ServerType: "code_execution_20260120"})
	}

	// user_profile: fetchProfile/profileFields/profileActive were resolved by
	// userprofilegate.Offer above, ahead of ComposeSystem. A nil
	// FetchSpeakerProfile is the Loop's own "not granted" state (see its doc
	// comment), so leave both fields zero when declined.
	if profileActive {
		loop.FetchSpeakerProfile = fetchProfile
		loop.SpeakerProfileFields = profileFields
	}

	// Isolated advisory LLM for identityMode=dynamic. Its presence is what turns a
	// plain "ask" into a "dynamic" choice: the IdentityChoiceGate labels the
	// request "dynamic" and surfaces the recommendation. Wired ONLY for dynamic
	// (ask leaves it nil ⇒ "no recommendation"). buildIdentityRecommender reuses
	// the runner's API key for the session's own provider — an OpenAI or
	// OpenRouter session's key would fail against NewAnthropic (wrong API), so
	// dispatch matches the primary provider construction above (providers.New).
	// Advisory only — the human always confirms. Recommender availability is
	// deliberately decoupled from the gate: a recommender error degrades dynamic
	// to a plain ask (see IdentityChoiceGate.Eval), it never fails the session.
	if class.Spec.IdentityMode == spiceboxv1alpha1.IdentityModeDynamic {
		loop.IdentityRecommender = buildIdentityRecommender(sess.Status.EffectiveSettings.Model.Provider, apiKey)
	}

	loop.AuthzCli = spdbCli
	loop.AuthzCache = toolcheck.NewZedTokenCache()
	loop.Engine = engine.New(engine.Deps{
		ToolChecker:            toolcheck.Checker{Cli: spdbCli, Cache: loop.AuthzCache},
		SessionInteractChecker: spdbCli,
		// SessionGrantChecker: nil — the JIT grant path stays inline in
		// authz.CheckToolCall; Engine routing lands in a follow-on PR.
		Granter: spdbCli,
		// RelWriter is the authz.Relation-shaped view of the same client; the
		// raw method names are taken by the v1 BootstrapWriter signature.
		RelWriter:  spdbCli.Relations(),
		SlotLister: spdbCli,
		Lookuper:   spdbCli,
		Memory:     memSigned,
		// Slot grants expire with the session's wall-clock cap, not its
		// active run-time budget: maxDuration can be minutes of a session
		// that stays reachable for days.
		SessionExpiration: sess.Status.EffectiveSettings.Budget.SessionExpiration.Duration,
	})
	loop.LabelStore = labelStore
	loop.ResolveAuthSubjects(&sess, &class)

	// BindClassDefaults: at session cold start, Check each AgentClass.Spec.
	// Authz.Slots[].Defaults against SpiceDB; allowed defaults bind as a slot
	// grant (the authorization the per-tool Check consults) plus a
	// ScopeResource in the Layer-2 scope document.
	//
	// Skipped when:
	//   - the AgentClass declares no slots (nothing to bind),
	//   - the auth subject couldn't be resolved (no canonical to Check against),
	//   - toolAuthMode == "disabled" (operator opted out of authz entirely).
	bindMode := class.Spec.GetAuthz().GetToolCalls().Mode
	if bindMode == "" {
		bindMode = runner.ToolAuthModeEnforcing
	}
	if len(class.Spec.GetSlots()) > 0 &&
		loop.AuthSubject() != "" &&
		bindMode != runner.ToolAuthModeDisabled {
		entities, entErr := boundEntitySpecsFromClass(&class)
		if entErr != nil {
			// A class whose slot preconditions do not compile binds NO defaults
			// — binding without the gate the class declared is the one outcome
			// that must not happen. The reconciler should have refused this
			// class, so say so rather than leaving an empty slot that reads as
			// "the operator configured no defaults".
			slog.Info("authz BindClassDefaults skipped: the class's slot preconditions did not compile",
				"session", sess.Namespace+"/"+sess.Name, "err", entErr.Error())
		} else {
			sessRef := authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
			if err := loop.Engine.BindClassDefaults(rootCtx, scope, sessRef, entities, loop.AuthSubject()); err != nil {
				slog.Info("authz BindClassDefaults failed",
					"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			}
		}
	}

	// The per-session authz config snapshot (authz_session_config) is NOT
	// written here. authzd derives this session's cold-start authorization
	// policy from that record, so the runner authoring it would let a
	// compromised runner choose the gates applied to itself. The AgentSession
	// reconciler writes it instead, from the AgentClass the operator itself
	// read, before this pod is created — and the Kind is now ComponentWritten,
	// so this pod's bearer is refused the write at the memory facade anyway.
	// Nothing in this process reads it either; both readers are in authzd.

	// Approval orchestrator + grant-tuple writer + NATS subscriber. Only wired
	// when channel-attached AND NATS is up — kubectl-driven sessions have no
	// channel surface to render the approval prompt on, so per-tool denials
	// short-circuit through the IsError path instead.
	loop.Approval = approval.New()
	loop.StartedByCanonical = spiceboxv1alpha1.ResolveStartedByCanonical(&sess)
	if sess.Spec.InputChannel != nil {
		loop.ChannelKind = sess.Spec.InputChannel.Kind
	}
	// Same closure-over-the-stamped-value the capability.RunnerEnv.WebdBaseURL
	// above uses (the address can arrive after this process started); consulted
	// only when the per-turn pinned-message recompute has a concluded trigger
	// to link, so a runner with no webd address yet simply projects no link.
	runner.SetWebdBaseURL(func() string { return cfg.webdBaseURL })
	// The kind to address a HUMAN by. Differs from ChannelKind on
	// cron/split-channel sessions: the input is "bento" (no user identity, no
	// transport to a person), while the reader is on the OutputChannel. Using
	// the input kind made the interaction sender skip every approver as
	// "recipient has no Slack identity", so a tool_approval parked for its full
	// timeout with nobody asked.
	//
	// This session's own outbound binding is the best answer available HERE,
	// and it is not always the final one: a conversational subagent's binding
	// is an `agent` Channel that no person reads, and the outbound relay routes
	// its human-directed cards past it to an ancestor's channel. The relay
	// re-stamps the recipients it delivers onto that channel
	// (pkg/channels/channelsd/outbound/recipient_kind.go) — the runner cannot,
	// because its own Role grants `get` on agentsessions pinned to this session
	// alone, so it cannot read an ancestor to find out. Do not add a lineage
	// walk here expecting it to work.
	if ob := spiceboxv1alpha1.OutboundBinding(&sess); ob != nil {
		loop.OutboundChannelKind = ob.Kind
	}
	// RelWriter's WriteRelationships/DeleteRelationships are option-free by
	// construction, so it satisfies grants.Writer's no-option signature
	// directly — no adapter required.
	loop.GuardianGrantWriter = spdbCli.Writer(grants.Source)
	// Withdraws the slot grant an external-effect approval wrote, once the one
	// call it authorized has run. The 30s expiry is the backstop.
	loop.SlotRevoker = spdbCli.Relations()
	// Writes the grant a plan-gate slot approval produces — scoped to the
	// instance the human saw — alongside the session_scope entry recorded next
	// to it. RelationsWithFloor advances the session's ZedToken freshness floor
	// (loop.AuthzCache, set above) to each grant's own write, so the
	// ToolCallAuthz check that immediately follows an in-process approval/
	// amendment reads at-least-as-fresh as the grant instead of a pre-write
	// snapshot — see Loop.AdvanceAuthzFloor.
	loop.SlotBinder = spdbCli.RelationsWithFloor(loop.AdvanceAuthzFloor)
	// Mirror onto status.slotPins any pin the cold-start BindClassDefaults
	// above produced — display-only, read back from SpiceDB per declared
	// single-occupancy type. Placed here rather than beside the bind because
	// the mirror reads through the SlotBinder just wired.
	loop.MirrorDeclaredSlotPins(rootCtx)

	// Info-leakage gate wiring. Resolve the policy from AgentClass spec, set up
	// helpers that bind the gate to the current session's scope.
	//
	// The policy's approvalTTL is deliberately NOT read off this pointer at the
	// gate: that window is one of the four-tier settings, so the runner takes it
	// from status.effectiveSettings.authz (Loop.resolvedLeakageApprovalTTL) and
	// falls back here only when no resolved snapshot exists. Everything else on
	// the policy — mode, onUnsupportedChannel, the bypass toggles — is
	// class-only and is read from this pointer directly.
	loop.LeakageConfig = class.Spec.GetAuthz().InformationLeakage
	loop.LookupToolMapping = func(toolName string) *spiceboxv1alpha1.ToolResourceMapping {
		// The runtime tool name is the LLM-facing prefixed + normalized form
		// (e.g. "linear_get_issue"); the toolResourceMap declares bare names
		// (e.g. `tool: get_issue`). LookupMCPToolResourceMapping reapplies
		// the `<prefix>_<tool>` join + NormalizeName transform so the
		// declaration is found. Sidecar toolboxes carry the same declaration —
		// consult them as a fallback so a declared sidecar tool is not treated
		// as undeclared (which would floor-taint it).
		if m := leakagewiring.LookupMCPToolResourceMapping(rootCtx, c, ns, class.Spec.MCPServers, toolName); m != nil {
			return m
		}
		return leakagewiring.LookupSidecarToolboxToolResourceMapping(rootCtx, c, ns, class.Spec.SidecarToolboxes, toolName)
	}
	// Leg A's input: the tool's own SEP-1913 source declaration. Same name
	// translation as the mapping lookup above, because both are keyed by the
	// LLM-facing prefixed name the hook is handed. Untrusted if EITHER an
	// MCPServer or a SidecarToolbox tool of that name declares it.
	loop.LookupToolUntrustedSource = func(toolName string) bool {
		return leakagewiring.LookupMCPToolUntrustedSource(rootCtx, c, ns, class.Spec.MCPServers, toolName) ||
			leakagewiring.LookupSidecarToolboxToolUntrustedSource(rootCtx, c, ns, class.Spec.SidecarToolboxes, toolName)
	}
	loop.TaintMemoryAppend = func(ctx context.Context, rec infoleakagetaint.TaintRecord) error {
		return infoleakagetaint.Append(ctx, memSigned, scope, rec)
	}
	// memHTTP, not memSigned: minting is a REQUEST to the operator, not an
	// append this session authors. The operator writes the pt_tag record with
	// the component credential and signs it there — pt_tag is component-written
	// so that a session cannot author its own reader set, and routing this
	// through the signing wrapper would be signing a call, not a record.
	//
	// The route answers 405 where per-datum provenance is not enabled, which
	// the hook logs and treats as "fall back to the session-wide taint set".
	loop.PtTagMint = func(ctx context.Context, req memory.PtTagMintRequest) (string, error) {
		return memHTTP.MintPtTag(ctx, scope, req)
	}
	// memHTTP, not memSigned, for the mirror reason to the mint: content-binding
	// is a REQUEST to the operator. pt_tag_content is component-READ, so this
	// session-credentialed runner cannot read it to compare for itself; the
	// operator holds the bytes and answers which ids bound.
	loop.PtTagVerify = func(ctx context.Context, regions []memory.PtTagRegion) (memory.PtTagVerifyResponse, error) {
		return memHTTP.VerifyPtTags(ctx, scope, regions)
	}
	// The trifecta's own mode, read from the class rather than derived from
	// toolCalls.mode: a control that switches off with an unrelated permission
	// check is not a control.
	loop.TrifectaMode = class.Spec.GetAuthz().GetTrifecta().Mode
	// A LIVE lookup, not a snapshot. A tag can be bound into this session
	// mid-run — a parent answering a child's ask_parent hands over data after
	// the runner started — and legs derived from a start-time list would judge
	// the session on data it no longer holds.
	loop.TrifectaBoundTags = func(ctx context.Context) ([]string, error) {
		bindings, err := spdbCli.ListDataSlotGrants(ctx, ns, name)
		if err != nil {
			return nil, err
		}
		tags := make([]string, 0, len(bindings))
		for _, b := range bindings {
			tags = append(tags, b.TagID)
		}
		return tags, nil
	}
	// Data slots: only a delegated child has any, so this is wired only when
	// this session names a parent. The content was minted in the parent's
	// scope, which is why that scope is what gets read — bounded to the tags
	// the operator already bound onto this child.
	if sess.Spec.Parent != nil {
		parentScope := memory.Scope{
			Kind: "session",
			ID:   sess.Spec.Parent.Namespace + "/" + sess.Spec.Parent.Name,
		}
		loop.ResolveBoundSlots = runner.ResolveBoundSlotsFor(memHTTP.ResolvePtTags, parentScope,
			func(ctx context.Context) ([]authz.DataSlotBinding, error) {
				return spdbCli.ListDataSlotGrants(ctx, ns, name)
			})
	}
	loop.TrifectaDeps = trifecta.Deps{
		TagCarriesUntrusted: spdbCli.TagCarriesUntrusted,
		TagReaders:          spdbCli.TagReaders,
		ChildAudience:       spdbCli.SessionReadTranscriptAudience,
	}
	// A live Get of THIS session — the only agentsession the runner's Role
	// permits it to read. The closure fact itself is derived and stamped by the
	// operator; this reads the answer.
	loop.SessionStatusReader = func(ctx context.Context) (bool, error) {
		var cur spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cur); err != nil {
			return false, err
		}
		return cur.Status.ClosureDenied != nil && *cur.Status.ClosureDenied, nil
	}
	loop.TaintMemoryList = func(ctx context.Context) ([]infoleakagetaint.TaintRecord, error) {
		return infoleakagetaint.List(ctx, memSigned, scope)
	}
	loop.AuditMemoryAppend = func(ctx context.Context, rec infoleakageaudit.AuditRecord) error {
		return infoleakageaudit.Append(ctx, memSigned, scope, rec)
	}
	loop.RequesterCanonicalID = func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
		// perCall is the PER-CALL principal the pipeline handed the hook
		// (pipeline.Input.Requester): the loop's session subject on the LLM
		// path, the widget VIEWER on a proxy-exec app-tool call. Deliberately
		// NOT loop.AuthSubject() — that read both mis-attributed a widget
		// viewer's read to the session subject and raced advanceRequester from
		// the off-loop goroutines a proxy-exec runs on.
		//
		// The leakage hook passes the value verbatim into SpiceDB
		// CheckPermission, which requires the "<type>:<id>" form. Prefix with
		// "user:" here so the downstream split parses. An empty canonical
		// (kubectl-driven session with no channel identity) stays "".
		return identity.Subject(leakagewiring.RequesterSubjectRef(perCall.String())), nil
	}
	loop.SpiceDBCheck = leakagewiring.NewSpiceDBCheck(spdbCli)
	loop.SpiceDBLookupSubjects = func(ctx context.Context, resource, permission string) ([]string, error) {
		// Build the subjectRef as "<resource>#<permission>" for spicedb.Client.LookupSubjects.
		return spdbCli.LookupSubjects(ctx, resource+"#"+permission)
	}
	// Approver standing for the plan gate's slot requests. FullyConsistent on
	// both: this runs on a human's click, and the grants they read may have been
	// written moments ago by the same session's fill path — a stale snapshot
	// would hold back a slot the approver demonstrably holds.
	//
	// The per-INSTANCE question is the one that decides what a grant may be
	// written against, so it is wired first and used whenever the phase named an
	// instance; the type-level one below answers only for a slot the phase
	// deferred. Wiring one without the other is a downgrade, not a partial
	// feature — see Loop.SpiceDBHasOnResource.
	loop.SpiceDBHasOnResource = func(ctx context.Context, resourceType, resourceID, permission string, canonicalID identity.CanonicalUserID) (bool, error) {
		return spdbCli.CheckOnResource(ctx, resourceType, resourceID, permission, canonicalID, true)
	}
	loop.SpiceDBHasAnyOfType = func(ctx context.Context, resourceType, permission string, canonicalID identity.CanonicalUserID) (bool, error) {
		return spdbCli.HasAnyOfType(ctx, resourceType, permission, canonicalID, true)
	}
	loop.LeakageGrantWriter = func(ctx context.Context, sessionNs, sessionName string, audience []string, resources []approval.LeakageGrantResource, ttl time.Duration) (string, error) {
		return approval.WriteInfoLeakageGrants(ctx, spdbCli.Writer(approval.LeakageSource), sessionNs, sessionName, audience, resources, ttl)
	}

	// Plan paused/active echoes: when channel-attached (pubFn set), let the
	// loop publish plan_update snapshots carrying the activity flag through the
	// same NATS path update_plan uses. nil pubFn (kubectl-driven) leaves the
	// field nil and emitPlanActivity no-ops.
	if pubFn != nil {
		loop.PublishPlanActivity = func(ctx context.Context, plan plans.Plan, paused bool, cause string, seq uint64, uid string) {
			if err := meta.PublishPlanSnapshot(ctx, pubFn, envSigner, ns, name, plan,
				channelevents.PlanDiff{}, paused, cause, seq, uid); err != nil {
				slog.Default().Info("plan activity publish failed",
					"session", ns+"/"+name, "cause", cause, "err", err.Error())
			}
		}
	}

	if chanAttached {
		// Resolve the channel Kind for the write-side audience resolver. A failure
		// here means ChannelKindImpl stays nil; the gate treats nil as
		// CapabilityUnsupported. This is intentionally non-fatal.
		if _, _, k, err := resolve.ForSession(rootCtx, c, &sess); err != nil {
			slog.Info("info-leakage: channel kind resolve failed; write-side gate will treat as unsupported",
				"session", ns+"/"+name, "err", err)
		} else if ar, ok := k.(channelkinds.AudienceResolver); ok {
			loop.ChannelKindImpl = ar
		}
		// If the kind doesn't implement AudienceResolver, ChannelKindImpl stays
		// nil (CapabilityUnsupported) — onUnsupportedChannel handles it at gate time.
	}

	// Late-bind the leakage gate to the loop. The meta.RespondConfig captures
	// leakageGateFn via a closure (see above), so this assignment wires the
	// gate before the session's first LLM turn executes. The egress gate now
	// runs the PreResponse pipeline point via the per-Loop executor
	// (leakageGateForRespond), replacing the bespoke LeakageGateForRespond.
	leakageGateFn = loop.LeakageGateForRespondAdapter()

	// Headless (no channel) sessions still connect to NATS when a bus address is
	// configured, so a human can drive an approval with `oap session approve`.
	// The chanAttached branch above already connected natsRT for channel
	// sessions; this covers the kubectl / `oap agent run` case, using the
	// session-derived subject prefix. Approvals route on the SESSION prefix (not a
	// channel), and channelsd parks + resolves them cluster-wide, so this is all
	// the plumbing a headless approval needs. Without it InteractionRequestPublish
	// stays nil and every approval fails closed ("interaction publish hook not
	// configured") with no way to answer it.
	if natsRT == nil {
		if url := natsURLFromEnv(); url != "" {
			rt, initErr := initNATS(rootCtx, url, channelevents.SubjectPrefix(ns, name))
			if initErr != nil {
				slog.Warn("headless NATS connect failed; interactive approvals unavailable this session",
					"session", ns+"/"+name, "err", initErr.Error())
			} else if rt != nil {
				natsRT = rt
				defer natsRT.close()
			}
		}
	}

	// Approval interaction bus: publish the request + await the applied decision.
	// Wired whenever NATS is connected — channel OR headless — because approvals
	// route on the session subject prefix, not a channel. Channel-specific
	// publishers (IdentityChoicePublish, UIPublish, channel message pub/sub) stay
	// in the chanAttached block below.
	if natsRT != nil && natsRT.conn != nil {
		loop.InteractionRequestPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			// IN routing: channelsd's HandleInteractionRequest subscribes to
			// in.interaction_request, parks the session + re-emits on OUT for
			// the generic interaction renderer. Publishing on OUT directly
			// would bypass channelsd's park/durable-entry write entirely.
			subject := channelevents.SubjectIn(
				channelevents.SubjectPrefix(envNS, envName), env.Kind)
			if err := envSigner.Sign(subject, &env); err != nil {
				return fmt.Errorf("sign envelope: %w", err)
			}
			body, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal interaction request envelope: %w", err)
			}
			return natsRT.conn.Publish(subject, body)
		}
		loop.TimeoutAppliedPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			prefix := channelevents.SubjectPrefix(envNS, envName)
			// IN: channelsd's Handle*Applied handler clears the pending queue +
			// condition for this kind. OUT: the outbound relay hands it to the
			// channel sender to edit the pending prompt to "expired" (a no-op
			// when no channel is bound); the runner's own applied subscriber
			// ignores it because the orchestrator already forgot this request.
			//
			// Sign is called once per subject (not once for the shared body):
			// the signature is bound to the exact subject it travels on, so the
			// IN and OUT copies each need their own signature over their own
			// digest, computed and marshalled independently.
			inSubj := channelevents.SubjectIn(prefix, env.Kind)
			if err := envSigner.Sign(inSubj, &env); err != nil {
				return fmt.Errorf("sign envelope: %w", err)
			}
			inBody, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal timeout applied envelope: %w", err)
			}
			if err := natsRT.conn.Publish(inSubj, inBody); err != nil {
				return fmt.Errorf("publish timeout applied on IN: %w", err)
			}
			outSubj := channelevents.SubjectOut(prefix, env.Kind)
			if err := envSigner.Sign(outSubj, &env); err != nil {
				return fmt.Errorf("sign envelope: %w", err)
			}
			outBody, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal timeout applied envelope: %w", err)
			}
			if err := natsRT.conn.Publish(outSubj, outBody); err != nil {
				return fmt.Errorf("publish timeout applied on OUT: %w", err)
			}
			return nil
		}
		// tool_approval, info_leakage, and content_inspection all resume via the
		// generic subscribeInteractionApplied bridge.
		go subscribeInteractionApplied(rootCtx, natsRT, loop.Approval, ns, name)
	}

	if chanAttached && natsRT != nil && natsRT.conn != nil {
		loop.ColdStartRequestPublish = func(_ context.Context, envNS, envName string, payload []byte) error {
			subject := channelevents.SubjectIn(
				channelevents.SubjectPrefix(envNS, envName), channelevents.KindMetaagentRequest)
			return natsRT.conn.Publish(subject, payload)
		}
		// IdentityChoicePublish delivers the identity_choice gate's
		// interaction_request envelope (category identity_choice — the unified
		// Interaction model) to channelsd on the OUT subject, where the outbound
		// relay routes it to the surface's "interaction" sub-channel sender. Set
		// ONLY inside this chanAttached block, so a session with no InputChannel
		// leaves it nil and the gate fails closed (the non-interactive guard: an
		// ask|dynamic session with no channel to ask on must NOT silently run as
		// the agent). The gate hands a pre-built envelope, so we marshal+publish
		// it directly (like InteractionRequestPublish below) rather than
		// re-building via channelevents.PublishOut. Generic over env.Kind —
		// unchanged by the gate's identity_choice_request → interaction_request
		// migration.
		loop.IdentityChoicePublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			subject := channelevents.SubjectOut(
				channelevents.SubjectPrefix(envNS, envName), env.Kind)
			if err := envSigner.Sign(subject, &env); err != nil {
				return fmt.Errorf("sign envelope: %w", err)
			}
			body, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal identity choice envelope: %w", err)
			}
			return natsRT.conn.Publish(subject, body)
		}
		// InteractionRequestPublish + TimeoutAppliedPublish are wired ABOVE, in the
		// NATS-gated (channel-or-headless) approval-bus block — approvals route on
		// the session prefix, so they must not be gated on chanAttached.
		// UIPublish delivers one agent-UI push envelope (ui_action_update from
		// uiActionRecorder, ui_view_update from uiview.Runtime.Write) to webd's
		// live route. OUT only for both kinds — unlike
		// InteractionRequestPublish/TimeoutAppliedPublish this has no IN-subject
		// half: nothing on the runner side subscribes to its own push, and
		// channelsd's park/durable-entry machinery is not involved (the durable
		// record — the ui_action memory entry, or the ui_view_model fragment —
		// is already written before either caller reaches this).
		loop.UIPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			subject := channelevents.SubjectOut(channelevents.SubjectPrefix(envNS, envName), env.Kind)
			if err := envSigner.Sign(subject, &env); err != nil {
				return fmt.Errorf("sign envelope: %w", err)
			}
			body, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal %s envelope: %w", env.Kind, err)
			}
			return natsRT.conn.Publish(subject, body)
		}
		// subscribeInteractionApplied (tool_approval / info_leakage /
		// content_inspection resume) is started ABOVE in the NATS-gated block, so
		// a headless session can also hear an `oap session approve` decision.
		go subscribeInterruptRequest(rootCtx, natsRT, loop, ns, name, envSigner)
		// Synchronous MCP-UI app-tool responder: a browser widget's readonly
		// app-tool call round-trips webd→runner and replies with the result. The
		// runner re-authorizes the claimed viewer independently (D-B4) via the
		// same SpiceDB client the use_token gate uses; a nil client would fail
		// closed inside HandleAppToolCall.
		go subscribeAppToolCall(rootCtx, natsRT, loop, spdbCli, ns, name)
		// Synchronous agent-UI data-binding responder: a browser's declared
		// "tool"-sourced binding round-trips webd→runner and replies with the
		// result. Shares handleAppToolCallReq's privileged core with the
		// app-tool responder above (readonly gate, viewer re-authorization,
		// rate limit, full containment pipeline) — see
		// channelevents.KindUIDataBinding's doc comment for why it is a
		// separate kind rather than a flag on KindAppToolCall.
		go subscribeUIDataBinding(rootCtx, natsRT, loop, spdbCli, ns, name)
		// Synchronous agent-UI action responder: a browser's declared action
		// binding round-trips webd→runner and replies with the pending/settled
		// state. Shares handleAppToolCallReq's privileged core with the two
		// responders above; unlike the data-binding responder it also drives the
		// ui_action lifecycle recorder (loop.UIPublish above), so a
		// side-effecting action's DETACHED outcome reaches memory and the
		// browser instead of being discarded the way a bare app-tool call is.
		go subscribeUIAction(rootCtx, natsRT, loop, spdbCli, ns, name)
		// Liveness, not a responder: while a viewer has this session's
		// agent-defined UI open and focused, webd heartbeats here and
		// await_user_message restarts its idle timer. Without it a runner
		// answering a dashboard's bindings exits underneath the person
		// reading it, because from the loop's point of view nobody spoke.
		go subscribeUIPresence(rootCtx, natsRT, presenceCh, ns, name)

		// Wire the unified ap.revocation in-flight revocation subscriber.
		// The revokedOrigins set is created HERE (not at loop construction) so
		// that Loop.RevokedOrigins is non-nil IFF this subscriber is actually
		// running — nil tells the revocation_guard hook to skip all revocation
		// checks for sessions that are not NATS-subscribed (e.g. kubectl
		// sessions). The single subscriber dispatches both tool-origin AND
		// credential revokes by RevokedPayload.Kind: the credential invalidator
		// (registered inside subscribeRevocation with mcpBroker) drops the
		// broker's cached token resolution within ~tens of ms of the operator
		// observing a credential removal or replacement. Fail-loud: a failed subscribe must not
		// leave the runner believing revocation is active while the guard is
		// disabled, and there is no fallback delivery path for revocations.
		revokedOrigins := toolorigin.New()
		loop.RevokedOrigins = revokedOrigins

		// A revocation that FAILS to apply runs no hook (that is what stops a
		// failed invalidation being recorded as a durable Revoked claim), so
		// without this the failure lives only in a log line while the capability
		// may still be live in this pod. The notifier posts one notice per
		// distinct failed revocation to the session's participants; it records
		// nothing durable, and never asserts the access is dead.
		revNotifier := newRevocationFailureNotifier(
			func(subject string, data []byte) error {
				return natsRT.conn.Publish(subject, data)
			},
			envSigner,
			channelevents.SessionRef{Namespace: ns, Name: name},
		)

		// One registry drives BOTH the live subscriber below and claimAndRecover's
		// restart re-application, so a revocable kind can never be handled live but
		// silently dropped on restart. The credential invalidators are every holder
		// of a resolved token: the broker's cache, and the frozen (header, value)
		// inside each live MCPTool resolved from that Secret.
		revReg, err := newRevocationRegistry(revokedOrigins, revNotifier, mcpBroker, mcpAuthInv)
		if err != nil {
			return fmt.Errorf("build revocation registry: %w", err)
		}
		loop.RevocationRegistry = revReg

		// session-hold is the fast path for a forensic hold: it cancels rootCtx,
		// halting the turn loop, any in-flight tool call, and the sequencer
		// together (all three run off rootCtx or a context derived from it).
		// Registered directly on revReg, not through newRevocationRegistry,
		// because unlike credential/toolorigin it is scoped to this runner's own
		// session key (ns+"/"+name) rather than fanning out over a set of
		// targets -- see sessionhold's package doc for why that key check is
		// required, not optional. Latency only: reconcileHold's pod reap is the
		// actual containment guarantee, on this and every later reconcile.
		if err := revReg.Register(sessionhold.New(ns+"/"+name, cancel)); err != nil {
			return fmt.Errorf("register session-hold invalidator: %w", err)
		}

		if err := subscribeRevocation(rootCtx, natsRT, revReg, ns, func(kind, key string) {
			// Emit a Revoked lifecycle event (carrying kind+key) so the revocation
			// appears in the session's signed audit log and can be re-applied on
			// restart by claimAndRecover. The live guard (revocation_guard
			// PreToolCall hook consulting revokedOrigins) already fired above via
			// Invalidate; this append is additive — audit + restart-recovery, no
			// phase change. Best-effort: if LifecycleMemory is nil or the append
			// fails, the live-guard invalidation is unaffected.
			loop.EmitRevoked(rootCtx, kind, key)
		}); err != nil {
			return fmt.Errorf("wire revocation subscriber: %w", err)
		}

		// Interactive tool sessions: wire the tool-session bridge registry +
		// inbound subscriber, and build the InteractiveHooks the runner
		// attaches to each tool-dispatch context.
		toolSessionReg := newToolSessionRegistry()
		go subscribeToolSessionInput(rootCtx, natsRT, toolSessionReg, ns, name)
		pub := natsPublishFunc(natsRT.conn)
		hooksNS, hooksName := sess.Namespace, sess.Name
		loop.InteractiveHooks = &sandbox.InteractiveHooks{
			IdleTimeoutDefault: 15 * time.Minute,
			MaxDurationDefault: 0, // unbounded → controller's interactiveSafetyCeiling
			OnOutput: func(toolCallRef, stream string, data []byte) {
				err := envSigner.PublishOut(
					func(subject string, b []byte) error { return pub(rootCtx, subject, b) },
					hooksNS, hooksName, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: toolCallRef, Stream: stream, Data: data,
					},
				)
				besteffort.Log(slog.Default().Info, "publish tool_session_delta", err,
					"session", hooksNS+"/"+hooksName, "toolCallRef", toolCallRef)
			},
			OnTerminal: func(toolCallRef, exitReason string, exitCode int32) {
				err := envSigner.PublishOut(
					func(subject string, b []byte) error { return pub(rootCtx, subject, b) },
					hooksNS, hooksName, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: toolCallRef, Stream: "stdout",
						Terminal: true, ExitReason: exitReason, ExitCode: exitCode,
					},
				)
				besteffort.Log(slog.Default().Info, "publish tool_session_delta (terminal)", err,
					"session", hooksNS+"/"+hooksName, "toolCallRef", toolCallRef, "exitReason", exitReason)

				// Re-show a working status the instant the streaming tool
				// ends, so the thread doesn't sit with no indicator during
				// the gap before the agent's next update_status — which
				// can read as a frozen agent. A plain "Continuing…" holds
				// until the agent replaces it (or respond_to_user clears
				// it). Skipped on idle-park exits, where the session is
				// intentionally waiting rather than working.
				if exitReason != "idle" {
					nerr := envSigner.PublishOut(
						func(subject string, b []byte) error { return pub(rootCtx, subject, b) },
						hooksNS, hooksName, channelevents.KindNotification,
						channelevents.NotificationPayload{Text: "Continuing…", Short: "Continuing…"},
					)
					besteffort.Log(slog.Default().Info, "publish continuing status", nerr,
						"session", hooksNS+"/"+hooksName, "toolCallRef", toolCallRef)
				}
			},
			OnEvent: buildToolSessionEventPublisher(
				rootCtx, pub, memSigned, scope, class.Spec.ToolSessionLog, hooksNS, hooksName, envSigner,
				loop.AddToolCost),
			Register: toolSessionReg.register,
		}
	}

	loop.BindingAutofillEnabled = bindingAutofillEnabled()
	loop.BindingAutofillDeadline = cfg.bindingAutofillDeadline

	ops := operations.New(nil, nil)
	// Threaded onto the Loop directly (not just SessionContext below) so the
	// operation-activity heartbeat (Loop.Run) can read it independent of
	// SessionContext's finalization; see Loop.Operations doc.
	loop.Operations = ops
	sessionStateRegistry := state.NewRegistry(state.Deps{
		Operations:       ops,
		AppendSystemNote: apdNote,
	})
	// Per-session secret-output store: captures secret values produced by tools
	// in-process so they are never echoed into the LLM context. The HTTPPublisher
	// then ships each captured value to the operator (same base URL + token as
	// the memory client) so the operator can write it into the per-session
	// Secret and record the handle on AgentSession.status.
	secretOutStore := secretout.NewSessionStore(string(sess.UID))
	secretOutPublisher := secretout.NewHTTPPublisher(memURL, ns, name, memToken)

	loop.SetSessionContext(&tool.SessionContext{
		Namespace:        ns,
		Name:             name,
		AgentSessionUID:  sess.UID,
		IsDelegatedChild: sess.Spec.Parent != nil,
		K8sClient:        c,
		ArtifactClient:   sandbox.NewHTTPArtifactClient(memURL, memToken),
		BundleSessions:   bundleSessionMap,
		Operations:       ops,
		State:            sessionStateRegistry,
		SecretOut:        secretOutStore,
		// SubmitResult and Memory are populated inside Loop.Run; the runner
		// loop owns those bindings since they need to capture per-loop state.
	})
	loop.SecretOutPublisher = secretOutPublisher

	// Publish the fully-constructed loop for late-bound readers (the SIGTERM
	// handler's MarkPlanStoppedBestEffort and the await OnYield/OnResume
	// closures). Stored AFTER SessionContext is set so a signal handler that
	// loads a non-nil loopRef also observes the SessionContext write (the atomic
	// store establishes the happens-before). Must precede loop.Run below.
	loopRef.Store(loop)

	// Mid-session re-synthesis of secret-gated separate-pod sidecars. The
	// headline flow is same-session: the agent runs a producer tool (CLI) that
	// emits a gating secret MID-session, the operator brings up the secret-gated
	// sidecar pod, and the runner must — between turns, WITHOUT restarting —
	// notice the pod is Ready, synthesize its tools, and add them to the live
	// tool set. Boot synthesis (above) handles in-pod and already-ready
	// separate-pod sidecars; this refresher handles the ones that were
	// AwaitingSecret / not-yet-ready at boot.
	//
	// Gate on whether any separate-pod sidecar exists at all so we don't re-Get
	// the AgentSession every turn for sessions with no separate-pod sidecars
	// (in-pod-only). We arm the refresher even for separate-pod sidecars already
	// synthesized at boot: the operator can REPLACE such a pod mid-session on a
	// token rotation (new SidecarPodIP), and the refresher must re-synthesize
	// against the new pod. bootSynthedSidecars seeds the refresher's synthesized
	// set (name → SidecarPodIP) so an unchanged pod is not re-synthesized.
	if anySeparatePodSidecar(sess.Status.ResolvedSidecarToolboxes) {
		// loop.Notify is the channel surface; a sidecar that will never come up
		// is reported there directly so the user learns of it even if the model
		// does not relay the advisory.
		loop.ToolRefresher = newSidecarToolRefresher(c, sessKey, mcpProbeFunc, bootSynthedSidecars, provenanceState, statusPatcher, mcpSessionCache, sidecarNoticeSink(loop, sessKey))
	}

	// SERVE-ONLY: everything above this line is the runner's privileged core —
	// resolved credentials, the MCP client, the containment pipeline, and the
	// three browser-facing responders already subscribed above. What follows
	// is the AGENT, and a session being opened as a dashboard does not need it.
	//
	// This mode exists because the only previous way to bring a runner back for
	// a data binding was the ordinary wake, which resumes the CONVERSATION:
	// Loop.Run replays memory, claims the live region, and takes an LLM turn.
	// Opening a page then put content in the agent's context that no human
	// asked for — a correctness problem for the conversation, not a cost one —
	// and observed live, it left a session Failed.
	//
	// Returning here rather than calling Loop.Run is what makes the mode
	// turn-free BY CONSTRUCTION rather than by a flag threaded through the
	// loop: Loop.Run is the sole terminal-status writer and the sole emitter of
	// SigSessionStarted, so a path that never calls it cannot move the
	// session's phase or write a terminal status, whatever it does later.
	if cfg.serveOnly {
		return runServeOnly(rootCtx, presenceCh, cfg.idleTTL, ns, name)
	}

	if err := loop.Run(rootCtx); err != nil {
		return fmt.Errorf("loop terminal write: %w", err)
	}
	// loop.Run is the SOLE terminal-status writer. On an identity handoff it
	// returns nil WITHOUT terminalizing (leaving status.phase=Pending, folded from
	// the signed IdentityChoiceResolved{userPassthrough} event) so the operator
	// re-drives the passthrough credential-link flow and re-spawns the runner.
	// There is no Succeeded write here to gate — we only surface the non-terminal
	// exit for operator visibility (no-silent-errors).
	if loop.IdentityHandoffExited {
		slog.Info("identity handoff: runner exited non-terminally for operator re-drive; phase left Pending",
			"session", ns+"/"+name)
	}
	return nil
}

// toolListHas reports whether the assembled tool list contains a tool with the
// given name. Used to gate the prompt's asset instructions on the artifacts
// capability having actually injected artifact_prepare (the grant+availability
// signal) without re-deriving the grant in internal/cmd/runner.
func toolListHas(tools []tool.Tool, name string) bool {
	for _, t := range tools {
		if t.Name() == name {
			return true
		}
	}
	return false
}

// appToolsByName indexes MCP-UI app-visible-only tools by name for
// Loop.AppTools — a registry deliberately separate from l.Tools (see
// mcpdispatch.SynthesizeResult.AppTools doc). Returns nil (not an empty map)
// when tools is empty, matching the field's "empty for agents with no
// opted-in app-tools" contract.
func appToolsByName(tools []tool.Tool) map[string]tool.Tool {
	if len(tools) == 0 {
		return nil
	}
	m := make(map[string]tool.Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return m
}

// anySeparatePodSidecar reports whether any resolved SidecarToolbox runs in a
// separate pod — i.e., whether the mid-session ToolRefresher has anything to
// watch for. Any separate-pod sidecar can become ready (was AwaitingSecret /
// not-yet-Ready at boot) OR be replaced mid-session (operator deletes+recreates
// the pod on a token rotation, changing SidecarPodIP), so the refresher is
// armed whenever one exists, not just when one was deferred at boot.
func anySeparatePodSidecar(resolved []spiceboxv1alpha1.ResolvedSidecarToolbox) bool {
	for _, rt := range resolved {
		if rt.RunMode == "separate-pod" {
			return true
		}
	}
	return false
}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Trim trailing newline (Kubernetes Secrets often round-trip a \n).
	// Check emptiness after trimming so a file containing only whitespace
	// (e.g. a bare \n) is also rejected fail-closed.
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return "", fmt.Errorf("%s: empty", path)
	}
	return string(b), nil
}

// fetchRenderBytes GETs the materialized render bytes from the operator's
// artifact endpoint, authenticating with this session's own per-session memory
// token — the runner holds no component credential, and the route admits a
// session token scoped to the {ns}/{sess} in the URL. Returns the body +
// Content-Type. Non-200 responses and transport errors are propagated to the
// caller; the body is always closed.
func fetchRenderBytes(ctx context.Context, base, token, ns, sess, render string) ([]byte, string, error) {
	u := strings.TrimRight(base, "/") + "/artifact/" + ns + "/" + sess + "/" + render + "/output"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", fmt.Errorf("runner: build artifact request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("runner: fetch artifact render: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("runner: artifact endpoint status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	// Cap the read: the operator is trusted, but an unbounded ReadAll of a
	// multi-MB render is needless memory pressure. 8 MiB comfortably covers any
	// plausible artifact.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("runner: read artifact body: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// workspaceSourceRuntime builds the capability package's runner-side view of
// the session's bound workspace source from the operator-populated status
// snapshot. Returns nil when no source is bound or the overlay cut hasn't
// completed yet (status.resolvedWorkspaceSource.overlayCut false) — the
// workspace capability treats a nil RunnerEnv.WorkspaceSource as "not
// available" and offers no sync_workspace/apply_workspace tools.
func workspaceSourceRuntime(sess *spiceboxv1alpha1.AgentSession) *capability.WorkspaceSourceRuntime {
	rws := sess.Status.ResolvedWorkspaceSource
	if rws == nil || !rws.OverlayCut {
		return nil
	}
	return &capability.WorkspaceSourceRuntime{
		Kind:                    rws.Kind,
		Locator:                 rws.Locator,
		Revision:                rws.Revision,
		WorkDir:                 "/workspace",
		OverlayPVC:              podspec.WorkspaceClaimName(sess),
		CredSecret:              spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name),
		ReconcileImage:          rws.ReconcileImage,
		ReconcileServiceAccount: rws.ReconcileServiceAccount,
	}
}

// persistToolSessionEvent reports whether an event of type evType should
// be persisted to the tool_session Kind under the given log mode.
// Empty mode is treated as the highSignal default (an AgentClass that
// predates the toolSessionLog field).
func persistToolSessionEvent(mode string, evType toolkitstream.EventType) bool {
	switch mode {
	case spiceboxv1alpha1.ToolSessionLogOff:
		return false
	case spiceboxv1alpha1.ToolSessionLogFull:
		return true
	case spiceboxv1alpha1.ToolSessionLogHighSignal, "":
		return evType != toolkitstream.EventTextDelta
	default:
		return false // unknown mode → conservative: don't persist
	}
}

// buildToolSessionEventPublisher returns a callback that publishes one
// parsed tool-session event from an interactive toolkit's stream
// parser as KindToolSessionEvent, and additionally persists it to the
// tool_session Kind (gated by logMode) so `oap agent logs --follow-live`
// can replay it. Factored out of the InteractiveHooks construction so
// the publish boundary is unit-testable without standing up a NATS
// connection.
func buildToolSessionEventPublisher(
	ctx context.Context,
	pub func(context.Context, string, []byte) error,
	mem memory.Memory,
	scope memory.Scope,
	logMode string,
	ns, name string,
	signer *channelevents.EnvelopeSigner,
	onResult func(outerTool string, costUSD float64, ok bool),
) func(toolCallRef, reason, outerTool string, ev toolkitstream.Event) {
	return func(toolCallRef, reason, outerTool string, ev toolkitstream.Event) {
		// NATS -> channelsd -> Slack — unchanged, always runs.
		err := signer.PublishOut(
			func(subject string, b []byte) error { return pub(ctx, subject, b) },
			ns, name, channelevents.KindToolSessionEvent,
			channelevents.ToolSessionEventPayload{
				ToolCallRef: toolCallRef,
				Reason:      reason,
				OuterTool:   outerTool,
				EventType:   string(ev.Type),
				Text:        ev.Text,
				ToolName:    ev.ToolName,
				ToolID:      ev.ToolID,
				Summary:     ev.Summary,
				OK:          ev.OK,
				DurationMs:  ev.DurationMs,
				CostUSD:     ev.CostUSD,
			},
		)
		besteffort.Log(slog.Default().Info, "publish tool_session_event", err,
			"session", ns+"/"+name,
			"toolCallRef", toolCallRef,
			"eventType", string(ev.Type))

		// Persist to the tool_session Kind so `oap agent logs --follow-live` can show
		// it. Best-effort: a missed persist must not disrupt the agent.
		if persistToolSessionEvent(logMode, ev.Type) {
			perr := toolsession.Record(ctx, mem, scope, toolsession.Event{
				ToolCallRef: toolCallRef,
				Reason:      reason,
				OuterTool:   outerTool,
				EventType:   string(ev.Type),
				Text:        ev.Text,
				ToolName:    ev.ToolName,
				ToolID:      ev.ToolID,
				Summary:     ev.Summary,
				OK:          ev.OK,
				DurationMs:  ev.DurationMs,
				CostUSD:     ev.CostUSD,
			})
			besteffort.Log(slog.Default().Info, "persist tool_session event", perr,
				"session", ns+"/"+name,
				"toolCallRef", toolCallRef,
				"eventType", string(ev.Type))
		}

		// Fold this interactive toolkit's own provider-reported cost into the
		// session total. Fires on the terminal result event only, and regardless
		// of the ToolSessionLog persist gate above — accumulation must not depend
		// on logging being on.
		if ev.Type == toolkitstream.EventResult && onResult != nil {
			onResult(outerTool, ev.CostUSD, ev.OK)
		}
	}
}

// buildLoopToolDispatch returns an OnToolDispatch hook that publishes a
// KindToolActivity envelope on every sandbox/MCP dispatch. channelsd's
// outbound relay routes that envelope to the silence watchdog's Touch,
// auto-keeping the watchdog alive while the agent is making progress.
// Returns nil when not channel-attached or NATS isn't configured — the
// Loop tolerates a nil hook by skipping the call.
func buildLoopToolDispatch(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context, toolName string, kind tool.Kind) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context, toolName string, _ tool.Kind) {
		err := signer.PublishOut(
			func(subject string, data []byte) error { return publish(ctx, subject, data) },
			ns, name,
			channelevents.KindToolActivity,
			channelevents.ToolActivityPayload{Tool: toolName},
		)
		besteffort.Log(slog.Default().Info, "publish tool_activity", err,
			"session", ns+"/"+name, "tool", toolName)
	}
}

// buildLoopTurnActivity returns a PublishTurnActivity hook that publishes a
// planless KindTurnActivity envelope at each active⇄paused transition.
// channelsd's outbound relay routes it to the silence watchdog (active →
// re-arm; paused → tear the indicator down). Returns nil when not
// channel-attached or NATS isn't configured — the Loop tolerates a nil hook by
// skipping the call.
func buildLoopTurnActivity(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context, active bool, cause string, seq uint64, uid string) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context, active bool, cause string, seq uint64, uid string) {
		err := signer.PublishOutSeq(
			func(subject string, data []byte) error { return publish(ctx, subject, data) },
			ns, name,
			channelevents.KindTurnActivity,
			channelevents.TurnActivityPayload{Active: active, Cause: cause},
			seq, uid,
		)
		besteffort.Log(slog.Default().Info, "publish turn_activity", err,
			"session", ns+"/"+name, "active", active, "cause", cause)
	}
}

// buildLoopTurnProgress returns a ProgressPublish hook that emits a throttled
// KindTurnProgress snapshot (cumulative tokens + elapsed + a monotonic seq) for
// the channel to render on its status surface. The runner's progressReporter
// owns the throttling; this hook is pure transport. Best-effort: publish errors
// are logged, never block the LLM stream. Returns nil when not channel-attached
// or NATS isn't configured (no consumer for the snapshots).
func buildLoopTurnProgress(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(inputTokens, outputTokens int64, elapsedSeconds, seq int) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	// Background context: this fires from inside the provider's stream pump
	// (see buildLoopOnStreamEvent); the loop's ctx isn't threaded down.
	pubCtx := context.Background()
	pubFn := func(subject string, data []byte) error { return publish(pubCtx, subject, data) }
	return func(inputTokens, outputTokens int64, elapsedSeconds, seq int) {
		err := signer.PublishOut(pubFn, ns, name, channelevents.KindTurnProgress,
			channelevents.TurnProgressPayload{
				InputTokens:    inputTokens,
				OutputTokens:   outputTokens,
				ElapsedSeconds: elapsedSeconds,
				Seq:            seq,
			})
		besteffort.Log(slog.Default().Info, "publish turn_progress", err,
			"session", ns+"/"+name, "seq", seq, "outputTokens", outputTokens)
	}
}

// buildLoopOperationActivity returns an OperationActivityPublish hook that
// emits a throttled KindOperationActivity snapshot of the live operation
// subtree (see runner.buildOperationActivity) for the channel to render as a
// live progress tree. Mirrors buildLoopTurnProgress's shape: transport-only,
// best-effort (publish errors are logged, never block the loop). Returns nil
// when not channel-attached or NATS isn't configured (no consumer for the
// snapshots).
//
// The session UID is captured once here (sess.UID, the same value threaded
// onto SessionContext.AgentSessionUID) rather than derived per-call: unlike
// tool_progress's dispatch-scoped IDs context, the operation-activity
// heartbeat runs independently of any single tool call and has no per-call
// IDs to read, but the session UID is constant for the runner pod's
// lifetime, so capturing it at construction time is exact, not an
// approximation.
func buildLoopOperationActivity(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(pl channelevents.OperationActivityPayload, seq int) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	uid := string(sess.UID)
	// Background context: this fires from the heartbeat goroutine (see
	// runOperationActivityHeartbeat), not from the loop's ctx.
	pubCtx := context.Background()
	pubFn := func(subject string, data []byte) error { return publish(pubCtx, subject, data) }
	return func(pl channelevents.OperationActivityPayload, seq int) {
		err := signer.PublishOutSeq(pubFn, ns, name, channelevents.KindOperationActivity, pl, uint64(seq), uid)
		besteffort.Log(slog.Default().Info, "publish operation_activity", err,
			"session", ns+"/"+name, "seq", seq, "active", len(pl.Operations) > 0, "cleared", pl.Cleared)
	}
}

// buildLoopToolProgress returns an OnToolProgress hook that publishes a
// KindToolProgress snapshot while a sync tool runs. Seq/uid are derived from
// the dispatch context (like update_status) so channelsd's watchdog can order
// and identity-scope the ticks. Best-effort; nil when not channel-attached.
func buildLoopToolProgress(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context, u sandbox.ToolProgressUpdate) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context, u sandbox.ToolProgressUpdate) {
		var seq uint64
		var uid string
		if ids, ok := sandbox.IDsFromCtx(ctx); ok {
			seq, uid = channelevents.PackSeq(ids.MemTurnIndex, ids.BlockIndex), ids.SessionUID
		}
		err := signer.PublishOutSeq(
			func(subject string, data []byte) error { return publish(ctx, subject, data) },
			ns, name, channelevents.KindToolProgress,
			channelevents.ToolProgressPayload{
				CallID: u.CallID, Name: u.Name, BudgetSeconds: u.BudgetSeconds,
				ElapsedSeconds: u.ElapsedSeconds, Done: u.Done,
				Percent: u.Percent, TailLine: u.TailLine, EtaSeconds: u.EtaSeconds,
			},
			seq, uid,
		)
		besteffort.Log(slog.Default().Info, "publish tool_progress", err,
			"session", ns+"/"+name, "tool", u.Name, "done", u.Done)
	}
}

// buildLoopOnStreamEvent returns a callback that forwards high-signal
// llm.StreamEvents to channelsd via NATS as KindAssistantStreamDelta
// envelopes. Filters out tool_use_delta_args (per-character JSON noise),
// thinking_delta (raw chain-of-thought, security-sensitive), and usage
// (not interesting to channels in v1) — only text_delta, tool_use_start,
// tool_use_stop, and stop are forwarded. Best-effort: serialization or
// publish errors are swallowed so the LLM stream never blocks on a
// transport issue. Returns nil when not channel-attached or NATS isn't
// configured (kubectl-driven runs have no consumer for the deltas).
func buildLoopOnStreamEvent(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(llm.StreamEvent) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	// Background context: stream events fire from inside the provider's
	// SSE pump; the loop's ctx isn't threaded down. Publish is best-
	// effort and synchronous-fast, so no cancellation is required.
	pubCtx := context.Background()
	pubFn := func(subject string, data []byte) error {
		return publish(pubCtx, subject, data)
	}
	return func(e llm.StreamEvent) {
		var pl channelevents.AssistantStreamDeltaPayload
		switch e.Type {
		case llm.StreamEventTextDelta:
			pl = channelevents.AssistantStreamDeltaPayload{
				EventType: "text_delta",
				Text:      e.Text,
				BlockIdx:  e.BlockIndex,
			}
		case llm.StreamEventToolUseStart:
			pl = channelevents.AssistantStreamDeltaPayload{
				EventType: "tool_use_start",
				ToolName:  e.ToolName,
				ToolID:    e.ToolUseID,
				BlockIdx:  e.BlockIndex,
			}
		case llm.StreamEventToolUseStop:
			pl = channelevents.AssistantStreamDeltaPayload{
				EventType: "tool_use_stop",
				ToolID:    e.ToolUseID,
				BlockIdx:  e.BlockIndex,
			}
		case llm.StreamEventStop:
			pl = channelevents.AssistantStreamDeltaPayload{
				EventType: "stop",
			}
		default:
			// Drop tool_use_delta_args, thinking_delta, usage.
			return
		}
		err := signer.PublishOut(pubFn, ns, name, channelevents.KindAssistantStreamDelta, pl)
		besteffort.Log(slog.Default().Info, "publish assistant_stream_delta", err,
			"session", ns+"/"+name, "eventType", pl.EventType)
	}
}

// buildIdentityRecommender constructs the isolated identityMode=dynamic
// advisory LLM for the session's own provider. Anthropic sessions get the
// existing Anthropic-backed provider; openai/openrouter sessions get an
// OpenAI-compatible provider pointed at the matching endpoint — an
// OpenRouter-provisioned key sent to NewAnthropic would fail (wrong API),
// which is the bug this dispatch fixes. Mirrors markup.NewForProvider /
// summarizer.NewForProvider's provider switch, just for identityadvisor
// (whose Provider needs a caller-supplied client rather than owning its own
// NewForProvider, since it's only ever constructed here, gated on
// identityMode=dynamic).
func buildIdentityRecommender(provider, apiKey string) identityadvisor.Provider {
	switch provider {
	case "openai":
		// OPENAI_BASE_URL, if set, overrides the default endpoint — mirrors
		// pkg/agent/llm/openai.New and markup/summarizer's NewOpenAI (test hook /
		// Codex-style alternate endpoint).
		return identityadvisor.NewOpenAICompatible(
			openaicompat.NewClient(apiKey, os.Getenv("OPENAI_BASE_URL"), nil), identityadvisor.OpenAIModel, "openai")
	case "openrouter":
		return identityadvisor.NewOpenAICompatible(
			openaicompat.NewClient(apiKey, openrouter.EffectiveBaseURL(), openrouter.AttributionHeaders()),
			identityadvisor.OpenRouterModel, "openrouter")
	default:
		return identityadvisor.NewAnthropic(apiKey)
	}
}

// buildLoopNotify returns a Notify function that publishes a Notification
// envelope (status update) on the runner's NATS publisher. Returns nil
// when not channel-attached or NATS isn't configured — Loop.Run handles
// nil Notify by skipping the call.
func buildLoopNotify(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context, text string) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context, text string) {
		err := signer.PublishOut(
			func(subject string, data []byte) error { return publish(ctx, subject, data) },
			ns, name,
			channelevents.KindNotification,
			// Ephemeral: every l.Notify caller is a one-shot announcement
			// ("Picking up N messages you sent.", "<agent> thinking…", identity/tool
			// notices) — not a persistent update_status caption (those go through
			// publishStatusNotification). Surfaces that honor the flag (web chat)
			// show it briefly, then fall back to a neutral working indicator.
			channelevents.NotificationPayload{Text: text, Ephemeral: true},
		)
		besteffort.Log(slog.Default().Info, "publish notification", err,
			"session", ns+"/"+name)
	}
}

// buildLoopDisabledNotify returns a DisabledNotify function that publishes
// a one-time notice when the AgentClass has spec.toolAuthMode=disabled, so a
// participant learns the session runs unchecked before they decide what to put
// in it.
//
// Returns nil for kubectl-driven sessions (no channel surface) or when
// NATS isn't wired — Loop.dispatchToolUses handles nil by logging the
// dispatch but not surfacing a user message (the kubectl flow uses the
// AgentClass status condition for visibility).
func buildLoopDisabledNotify(chanAttached bool, rt *natsRuntime, sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context) {
	if !chanAttached || rt == nil || rt.conn == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	publish := natsPublishFunc(rt.conn)
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context) {
		n := notice.New(categories.ToolAuthzDisabled, notice.Args{
			Lead: "Tool permission checks are off for this agent",
			Body: "Every tool call this agent makes runs without a permission check, " +
				"including calls that reach data you can see.",
			// Privacy tone, so a next step is required — and there genuinely is
			// one, even though nothing is broken: the reader can decide how much
			// to entrust to this session.
			NextStep: "Treat anything you share here as reachable by every tool the agent has.",
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
		})
		err := n.PublishSigned(signer,
			func(subject string, data []byte) error { return publish(ctx, subject, data) },
			channelevents.SessionRef{Namespace: ns, Name: name},
			"notice-tool-authz-disabled-"+string(sess.UID))
		besteffort.Log(slog.Default().Info, "publish toolAuthDisabled notice", err,
			"session", ns+"/"+name)
	}
}

// interactionAppliedSubject is the OUT subject subscribeInteractionApplied
// listens on for a session — split out from the subscribe function so the
// subject shape is independently testable (mirrors approvalAppliedSubject).
func interactionAppliedSubject(ns, name string) string {
	return channelevents.SubjectOut(
		channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionApplied)
}

// handleInteractionAppliedMessage decodes a raw .out.interaction_applied NATS
// message and, for the categories whose runner-side gate blocks on this
// Orchestrator, delivers the decoded approval.Decision into orch — unblocking
// the gate's Await. Extracted from the subscription callback so the
// decode+dispatch logic is testable without a live NATS connection (mirrors
// the bus-decoupling in subscribeRevocationOnBus).
//
// Every category is routed synchronously through the same Orchestrator —
// there is no Outcome.Suppressed / async-interrupt path here (that shape
// belongs to Slice B's mid-turn reply queue, not the approval-resume bridge).
//
// Category dispatch:
//
//   - identity_choice: InteractionAppliedPayload has no Action field (a
//     deliberate interaction-model constraint). identity_choice's bound
//     decision handler (pipeline.IdentityChoiceDecisionHandler) encodes the
//     3-way answer ("agent" | "userPassthrough" | "cancel") into
//     Outcome.OutcomeText, which the generic decision pipe copies verbatim
//     into Applied.OutcomeText — read back out here as Decision.Action,
//     exactly what the gate's Eval switches on.
//   - content_inspection: a pure allow/deny gate (pkg/agent/runner's
//     runner-host approval). Outcome maps onto Decision.Approved:
//     approved→true, denied/expired→false — a timeout Applied (Outcome=
//     expired) delivers Approved=false, a no-op once the orchestrator has
//     already forgotten a request that timed out client-side first.
//
// Every other category currently on the interaction model (credential_link,
// portal_access, …) resolves out-of-band and never reaches this subscriber's
// dispatch. A third runner-resuming category would extend this switch, or —
// once the list grows past a couple — earn its own per-category registry
// mirroring channelinteractions.Bind.
func handleInteractionAppliedMessage(data []byte, orch *approval.Orchestrator) {
	var env channelevents.Envelope
	if uerr := json.Unmarshal(data, &env); uerr != nil {
		slog.Info("interaction-applied: unmarshal envelope", "err", uerr.Error())
		return
	}
	var pl channelevents.InteractionAppliedPayload
	if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
		slog.Info("interaction-applied: unmarshal payload", "err", uerr.Error())
		return
	}
	// Dispatch off the category registry, shared with the e2e harness's bridge —
	// see approval.FromApplied for why this is one function and not two switches.
	d, deliver, registered := approval.FromApplied(pl)
	if !registered {
		// A publisher and this consumer disagree about the category set. Never
		// silent: the only symptom downstream is a gate that hangs.
		slog.Info("interaction-applied: unregistered category; nothing to resume",
			"category", pl.Category, "requestRef", pl.RequestRef)
		return
	}
	if !deliver {
		return // ResumeNone: declared as a category the runner does not wait on.
	}
	orch.DeliverDecision(pl.RequestRef, d)
}

// subscribeInteractionApplied subscribes to outbound KindInteractionApplied
// envelopes for this session — the unified Interaction model's runner-side
// back-channel (replaces the legacy per-category subscribeIdentityChoiceApplied
// for identity_choice). channelsd's generic decision pipe
// (HandleInteractionDecision) publishes the applied envelope on both .in and
// .out after a decision resolves; this subscriber only needs .out.
//
// Returns when ctx is cancelled. Subscription errors are logged but not fatal —
// a missing subscriber causes the gate to time out at its choice TTL.
func subscribeInteractionApplied(ctx context.Context, rt *natsRuntime, orch *approval.Orchestrator, ns, name string) {
	if rt == nil || rt.conn == nil || orch == nil {
		return
	}
	subject := interactionAppliedSubject(ns, name)
	sub, err := rt.conn.Subscribe(
		subject,
		func(m *nats.Msg) {
			handleInteractionAppliedMessage(m.Data, orch)
		},
	)
	if err != nil {
		slog.Info("interaction-applied subscribe failed", "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// bindingAutofillEnabled returns true iff AP_BINDING_AUTOFILL=enabled.
// Default OFF (behavior-preserving for this PR). Future PR flips the
// default once CheckEntityCanBind lands and extracted_entity → binding
// promotion is wired.
//
// AP_BINDING_AUTOFILL is deliberately NOT a bool flag: its enabling value is
// the string sentinel "enabled", not a bool literal, so a bool flag bound to
// it would fail-closed on "enabled" and break the contract. Left as os.Getenv.
func bindingAutofillEnabled() bool {
	return os.Getenv("AP_BINDING_AUTOFILL") == "enabled"
}

// turn.Appender implements runner.MemoryAppender; the compile-time
// check keeps the structural satisfaction explicit at the call site.
var _ runner.MemoryAppender = (*turn.Appender)(nil)

// fanoutLabelSink forwards label Put calls to both the legacy LabelStore
// (for approval-time snapshot resolution) and the memory framework's label
// Kind (for forensic audit). Nil-safe on mem: when mem is nil, only the
// primary store is written.
type fanoutLabelSink struct {
	primary *runner.LabelStore
	mem     memory.Memory
	scope   memory.Scope
}

func (s *fanoutLabelSink) Put(resourceType, id, lbl string) {
	s.primary.Put(resourceType, id, lbl)
	if s.mem != nil {
		if err := label.Record(context.Background(), s.mem, s.scope, resourceType, id, lbl); err != nil {
			slog.Default().Info("label.Record dual-write",
				"session", s.scope.ID, "err", err.Error())
		}
	}
}

// resolveSkills lists namespace Skills and cluster-scoped ClusterSkills, merges
// them (namespace wins on duplicate canonical name), then matches class.Spec.Skills
// (by AgentSkill.Ref) against the merged set. Returns the always-on metadata (for
// the prompt) and the body map (for load_skill). It is FAIL-CLOSED: if any
// opted-in skill does not resolve, it returns an error (the caller marks the
// session Failed) rather than dropping the skill — the AgentClass gate already
// guarantees each skill existed + was Valid before spawn, so an unresolved skill
// is an inconsistency (commonly the runner SA lacking skills/clusterskills RBAC),
// not a not-yet-synced skill. The tiered AllowedSkills/DeniedSkills ceiling is
// already enforced upstream (webhook/controller), so this does not re-check it.
//
// Target gates what reaches the agent: SkillTargetSandbox is staged to disk by
// the AgentSession controller (resolveAndStageSkillBundles) for a disk-based
// consumer and is deliberately excluded here — from both skillMeta (the prompt)
// and bodies (load_skill) — so a sandbox-only skill's description and body are
// never billed into the outer agent's context. SkillTargetAgent (the default;
// an in-memory AgentSkill reads Target=="" before the API server applies the
// CRD default) and SkillTargetBoth are unaffected. Because a sandbox-only skill
// is resolved and integrity-checked on the operator side instead, this function
// does not include it in the fail-closed missing-skill check either — a missing
// sandbox-only Skill CR is that reconciler's non-fatal log-and-skip, not a
// reason to fail this session.
func resolveSkills(ctx context.Context, c client.Reader, ns string, want []spiceboxv1alpha1.AgentSkill) ([]runner.SkillMetadata, map[string]string, []runner.RepoInstructions, error) {
	if len(want) == 0 {
		return nil, nil, nil, nil
	}

	// Attempt both lists, capturing (not swallowing) any List error. A failure
	// here is NOT the benign "skill not synced yet" the old log-and-skip
	// assumed: the AgentClass fail-closed gate (validateSkills / classIsValid)
	// already proved every opted-in skill materialized and is Valid=True before
	// this session was allowed to spawn. So a List error (e.g. the per-session
	// runner SA missing skills/clusterskills RBAC) or an unresolved skill is a
	// genuine inconsistency. We don't abort on the List error alone — a
	// namespace-only skill set still resolves if the ClusterSkill list fails,
	// and vice versa — but we DO fail the session below if any wanted skill is
	// left unresolved, attributing the list error as the likely cause.
	var listErrs []string
	var nsList spiceboxv1alpha1.SkillList
	if err := c.List(ctx, &nsList, client.InNamespace(ns)); err != nil {
		listErrs = append(listErrs, fmt.Sprintf("listing namespace Skills: %v", err))
		// nsList.Items remains nil — mergeSkillSources handles that.
	}
	var clusterList spiceboxv1alpha1.ClusterSkillList
	if err := c.List(ctx, &clusterList); err != nil {
		listErrs = append(listErrs, fmt.Sprintf("listing ClusterSkills: %v", err))
		// clusterList.Items remains nil — mergeSkillSources handles that.
	}

	byCanonical := mergeSkillSources(nsList.Items, clusterList.Items)

	seenInstr := map[string]bool{}
	var repoInstrs []runner.RepoInstructions
	var skillMeta []runner.SkillMetadata
	bodies := make(map[string]string, len(want))
	var missing []string
	for _, sk := range want {
		target := sk.Target
		if target == "" {
			// An in-memory AgentSkill reads Target=="" until the API server
			// applies the CRD's +kubebuilder:default=agent — treat empty the
			// same as the explicit "agent" default (mirrors skillsToStage in
			// pkg/controllers/agentsession/skills.go).
			target = spiceboxv1alpha1.SkillTargetAgent
		}
		if target == spiceboxv1alpha1.SkillTargetSandbox {
			// Sandbox-only: staged to disk by the AgentSession controller, never
			// surfaced to the outer agent's prompt or load_skill.
			continue
		}
		name := sk.Ref
		sd, ok := byCanonical[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		skillMeta = append(skillMeta, runner.SkillMetadata{
			CanonicalName: name,
			Description:   sd.Description,
		})
		bodies[name] = sd.Body
		if sd.RepoInstructions != nil && sd.RepoInstructions.Content != "" && !seenInstr[sd.RepoInstructions.Content] {
			seenInstr[sd.RepoInstructions.Content] = true
			repoInstrs = append(repoInstrs, runner.RepoInstructions{
				SourceRepo: sd.SourceRepo,
				SourceFile: sd.RepoInstructions.SourceFile,
				Content:    sd.RepoInstructions.Content,
			})
		}
	}

	// Fail-closed: a skill the AgentClass opted into (and that was Valid at
	// class-validation time) failing to resolve in the runner means the agent
	// would silently lose load_skill + the Agent Skills prompt section. Surface
	// it on the AgentSession instead of degrading invisibly.
	if len(missing) > 0 {
		msg := fmt.Sprintf("opted-in skill(s) did not resolve to a materialized Skill/ClusterSkill: %s", strings.Join(missing, ", "))
		if len(listErrs) > 0 {
			msg += " — " + strings.Join(listErrs, "; ") +
				" (the per-session runner ServiceAccount likely lacks read access to skills/clusterskills)"
		} else {
			msg += " (both lists succeeded, so the Skill was deleted/renamed or lives in another namespace since the AgentClass was validated)"
		}
		return nil, nil, nil, errors.New(msg)
	}
	sort.Slice(repoInstrs, func(i, j int) bool { return repoInstrs[i].SourceRepo < repoInstrs[j].SourceRepo })
	return skillMeta, bodies, repoInstrs, nil
}

// boundEntitySpecsFromClass adapts AgentClass.Spec.Authz.Slots to the
// []authz.BoundEntitySpec shape that pkg/authz consumes.
//
// The conversion itself is pkg/authz/slotspec's, which is the only place a
// BoundEntitySpec is constructed — three hand-written copies of it is how a
// slot's `requires[]` preconditions silently fail to reach the gate at one call
// site while working at the others. This wrapper supplies the transform chains
// from the class's status and nothing else.
func boundEntitySpecsFromClass(class *spiceboxv1alpha1.AgentClass) ([]authz.BoundEntitySpec, error) {
	if class == nil || len(class.Spec.GetSlots()) == 0 {
		return nil, nil
	}
	return slotspec.FromClass(class, runner.SlotTransformsOf(class))
}

// sidecarNoticeSink adapts the loop's interaction publisher into the callback
// the sidecar refresher reports terminal failures through.
//
// It publishes a notice rather than a status caption because a toolset being
// gone lasts the whole session: a caption is replaceable state and would be
// overwritten by the next thing the agent says, leaving no record of why a
// capability vanished. Returns nil when there is no publisher, which the
// refresher documents as "agent-facing advisory only".
func sidecarNoticeSink(loop *runner.Loop, sessKey client.ObjectKey) func(context.Context, *notice.Notice) {
	if loop.InteractionRequestPublish == nil {
		return nil
	}
	return func(ctx context.Context, n *notice.Notice) {
		pl, err := n.Payload(
			channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			"notice-"+n.Category()+"-"+sessKey.Name)
		if err != nil {
			besteffort.Log(slog.Default().Info, "build sidecar failure notice", err,
				"session", sessKey.String())
			return
		}
		env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
			channelevents.KindInteractionRequest, pl)
		if err != nil {
			besteffort.Log(slog.Default().Info, "build sidecar failure envelope", err,
				"session", sessKey.String())
			return
		}
		besteffort.Log(slog.Default().Info, "publish sidecar failure notice",
			loop.InteractionRequestPublish(ctx, sessKey.Namespace, sessKey.Name, env),
			"session", sessKey.String())
	}
}
