// Command webd is the browser-UI host. It serves a registry of WebUI
// plug-ins across two origins (trusted + sandbox), mirroring identityd's
// startup shape.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd/icons"
	"github.com/authzed/openagentprimitives/pkg/platform/kube"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/pkg/web/adminui"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	// mcpfront is imported BY NAME, not blank, for the same reason agentui and
	// sessions are (see those imports' own comments below): main.go references
	// mcpfront.Minter/mcpfront.MintParams/mcpfront.AccessTokenAuthz directly
	// (buildArtifactViewDeps's Minter construction, AccessTokenAuthz's nil-
	// guarded return, and the compile-time Deps guard below), so a future
	// mcpfront.Deps signature drift is a `go build` failure, not a runtime
	// deps.(mcpfront.Deps) cast that silently 404s. The import ALSO runs
	// mcpfront's init() (webui plugin registration) — Task 9's controller
	// ruling: a blank import would register the plugin but give main.go no way
	// to reference mcpfront.Minter at all.
	"github.com/authzed/openagentprimitives/pkg/web/mcpfront"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
	// agentui is imported BY NAME, not blank: main.go references agentui.Deps
	// directly in the compile-time guard below the artifactViewDeps struct, so
	// the import itself is load-bearing — the compiler refuses to build with it
	// removed, which is what makes a future agentui.Deps signature drift (or an
	// accidental deletion of this import) a `go build` failure rather than a
	// runtime deps.(agentui.Deps) cast that silently 404s. See
	// buildArtifactViewDeps's nil-on-unconfigured-prerequisites return path and
	// the webdDeps type's doc comment for the shipped bug this whole guard
	// chain — cast, guard, and this import — exists to prevent.
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
	"github.com/authzed/openagentprimitives/pkg/web/webui/channelwebhook"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
	"github.com/authzed/openagentprimitives/pkg/web/webui/contenttoken"
	"github.com/authzed/openagentprimitives/pkg/web/webui/livemirror"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
	// sessions is imported BY NAME for the same reason agentui is (see that
	// import's own comment above): main.go references sessions.Deps directly
	// in the compile-time guard below the artifactViewDeps struct, so a future
	// sessions.Deps signature drift is a `go build` failure, not a runtime
	// deps.(sessions.Deps) cast that silently 404s.
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessions"
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessionview"

	// Register the HTML artifact renderer so the live-view content handler can
	// dispatch ServeTransform by the artifact's kind (a no-op for every kind
	// today; the artifact is served inert — see plan D1).
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/oap" // webd dispatches ServeTransform/RenderKind for the workshop draft kind too
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"

	// Register interaction categories (also referenced by name in derivePause).
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"

	// Register built-in WebUIs (self-register via init()).
	// chat-embed serves /chat-embed/{ns}/{name}, the one-session chat framed
	// same-origin by ap:chat (the agent-builder's Test panel); its Routes()
	// casts webui.Deps to chatembed.Deps (CheckInteract + Logger), which
	// *artifactViewDeps already satisfies for interact/sessionview above — no
	// new method needed, only this blank import to register the plugin.
	_ "github.com/authzed/openagentprimitives/pkg/web/webui/chatembed"
	_ "github.com/authzed/openagentprimitives/pkg/web/webui/health"
	// interact serves /session/{ns}/{name}/interact + /interactions; its
	// Routes() casts webui.Deps to interact.Deps, which fails closed (nil
	// routes) when the umbrella isn't the viewer-capable *artifactViewDeps —
	// same fail-closed shape as chat/artifactview.
	_ "github.com/authzed/openagentprimitives/pkg/web/webui/interact"
	// agentui (imported by name above, alongside artifactview/sessionview/chat)
	// serves /agent-ui/{ns}/{name} and /agent-ui/{ns}/{name}/bindings; its
	// Routes() casts webui.Deps to agentui.Deps (CheckInteract + K8s + Logger +
	// TrustedOrigin + NATSRequest + Memory + Artifacts + ArtifactRenderBytes),
	// which fails closed (nil routes, logged through whatever the failed value
	// can still supply) when the umbrella isn't the viewer-capable
	// *artifactViewDeps — same fail-closed shape as chat/artifactview/interact.
	//
	// Register the four uibindings.Resolver sources the bindings route
	// resolves through (pkg/web/uibindings/registry.Get) — a blank import each,
	// self-registering via their own init(), the same registry-over-branching
	// shape as every other pluggable aspect (see AGENTS.md's aspect table). A
	// binary that forgets one of these is exactly the silent-404 class Plan
	// 3's Deps cast guard exists for: bindingsHandler logs "no resolver
	// registered for source" instead of ever reaching a switch statement here.
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/actionstate"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/artifactref"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/memoryref"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/tool"

	// identityd self-registers as a webui.WebUI so webd hosts its browser auth
	// surface in-process. Named rather than blank because artifactViewDeps
	// implements identityd.WebAuthzDeps, whose method returns
	// identityd.AgentIdentityAuthz.
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"

	// Populate the identity-setup flow registry: identityd's paste-form
	// verification (builtins.VerifyCredential) dispatches through it.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"

	// register the static/oauth/federated credkind.Kinds: identityd's
	// browser credential-update flow (agentidentity.Resolve / PutToken, called
	// from pkg/platform/identityd/credupdate_agentowned.go) reaches
	// credresolve.SourceFor / ResolveSecretValue, which dispatch through
	// registry.Get(cred.Type) — without this import every browser credential
	// update fails closed with "unknown credential type".
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"

	// Blank imports for channel-kind registration.
	// fake: scriptable in-process kind (tests + dev / ?auth=fake E2E seam).
	// slack: real "Sign in with Slack" OIDC authenticator.
	// browser: the browser channel kind pkg/web/webui/chat drives. Its real
	// Listener/Sender are built per-session via browser.NewHost — this
	// blank import only registers the Kind so in-process lookups the
	// channelsd pipeline makes (chregistry.Get, SpawnsSessionOnInbound, etc.
	// — the pipeline runs inside webd's chat plugin, not channelsd, for
	// browser sessions) resolve kind "browser".
	// github: registers the Kind so channelwebhook's registry sweep
	// (pkg/web/webui/channelwebhook) finds its WebhookReceiver and mounts
	// /webhooks/github/... — see that package's doc for the sweep.
	// agent: the session-to-session kind. webd hosts no transport for it and
	// mounts nothing on it — it contributes no WebAuthenticator and no
	// WebhookReceiver, so both registry sweeps below skip it. It is linked
	// rather than exempted because the pipeline webd runs for browser sessions
	// resolves kinds through the same registry, and a browser session that
	// delegates has children bound to `agent` Channels: a miss there is a
	// runtime "unknown kind", not a build error. It costs exactly one package —
	// every transitive dependency it has, webd already links.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"

	// Blank imports for identity-provider kind registration: identityd's
	// IdP loader resolves ClusterIdentityProvider.spec.kind through the
	// idp registry at login time.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
)

const cookieName = "idd_session" // shared with identityd; webd only verifies it

// contentTokenTTL bounds how long a minted content-capability token (and thus
// a sandbox-iframe content URL) stays valid. Short: the shell re-mints one on
// every /artifact-view load, after the SpiceDB view check.
const contentTokenTTL = 5 * time.Minute

// config holds webd's non-secret configuration, populated from flags and (via
// clikit) the environment. Secret values are never carried here: the HMAC
// signing key, the webd memory token, the Slack OAuth creds, and the SpiceDB
// preshared token all ride in mounted files or are read directly from the
// environment in run()/buildArtifactViewDeps — never via argv or --help.
type config struct {
	addr               string
	signingKeyPath     string
	trustedBaseURL     string
	sandboxBaseURL     string
	allowSharedOrigin  bool
	operatorURL        string
	natsURL            string
	webdTokenPath      string
	slackOAuthDir      string
	webDev             bool
	webDevURL          string
	insecureTrustLinks bool
	spicedbEndpoint    string
	spicedbInsecure    bool
	spicedbTokenPath   string
	iconMaxEntries     int
	iconTTL            time.Duration
	iconNegTTL         time.Duration
	admindURL          string
	admindTokenPath    string
	// maxLiveSessionsPerSubject / maxLiveSessions bound the live-session table
	// pkg/web/webui/chat keeps. They were hardcoded when this plane served one
	// desktop user; on a shared cluster the ceiling divided by the per-subject
	// limit is how many fully-engaged viewers saturate one webd, and an
	// operator whose users are being refused needs a number to turn rather than
	// a rebuild. Zero leaves the package default (never "unlimited" — see
	// chat.SetLimits).
	maxLiveSessionsPerSubject int
	maxLiveSessions           int
	// startNamespaces is the set of namespaces this webd may CREATE a browser
	// session in. It mirrors the namespaced Role an operator installed, which
	// this process cannot read for itself: creating a session writes a
	// Channel, a creds Secret and an AgentSession, and those verbs are granted
	// per namespace on purpose (config/webd/role-default.yaml). The default
	// matches the one Role `oap install` ships.
	//
	// Its consumers refuse anything outside it — the dashboard's picker marks
	// those entries and says why, and browserstart.Start refuses before
	// reserving — so an operator who adds a Role elsewhere and forgets this
	// flag gets a control that declines, not one that 500s. An EMPTY value
	// means "nowhere", never "anywhere": see browserstart.StartableIn.
	startNamespaces []string
	// clusterKind is the AP_CLUSTER_KIND value `oap install` stamped onto this
	// Deployment (one of cloud.RegisteredKeys()). Resolved fail-closed in run()
	// into a cloud.Strategy, whose InstallProfile().AllowsSharedOrigin() is the
	// one answer this binary reads. It is deliberately NOT a bool, so an
	// unset/unrecognized value cannot silently resolve to a permissive default.
	clusterKind string
	// accessTokenNamespace is where the /mcp Minter creates AccessToken CRs
	// and where the bearer middleware lists them back — the two MUST agree, or
	// a freshly-minted token would never authenticate. Defaults to the
	// standard install namespace.
	accessTokenNamespace string
	// accessTokenLifetime is how long a token minted by /oauth/token stays
	// valid (webd's --accesstoken-lifetime). Default 2160h = 90 days.
	accessTokenLifetime time.Duration
}

func main() {
	// Before anything can compile a SpiceDB schema: its compiler logs a trace
	// line per definition through zerolog's process-global logger, and this
	// binary's stderr is `kubectl logs`. webd's own logging is untouched.
	// See pkg/platform/deplogs.
	deplogs.Silence()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "webd:", err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "webd",
		Short:        "Browser-UI host: serves the WebUI registry across the trusted + sandbox origins.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.addr, "addr", ":8080", "HTTP listen address")
	fs.StringVar(&cfg.signingKeyPath, "signing-key-path", "/etc/passthrough-link-key/key", "path to the HMAC signing-key file (hex-encoded)")
	fs.StringVar(&cfg.trustedBaseURL, "trusted-base-url", "", "trusted-origin base URL (auth surface)")
	fs.StringVar(&cfg.sandboxBaseURL, "sandbox-base-url", "", "sandbox-origin base URL (artifact content)")
	fs.BoolVar(&cfg.allowSharedOrigin, "allow-shared-origin", false, "DEBUG ONLY: permit a single shared origin; honored only under an ngrok host")
	fs.StringVar(&cfg.operatorURL, "operator-url", "", "operator memory/artifact base URL (e.g. http://spicebox-operator.agentprimitives-system.svc:8082)")
	fs.StringVar(&cfg.natsURL, "nats-url", "", "NATS URL for the live-view status panel (session plan_update/notification events); empty disables the status panel")
	fs.StringVar(&cfg.webdTokenPath, "webd-token-path", "/var/run/webd/memory-token/token", "path to webd's dedicated read-only memory-token file")
	fs.StringVar(&cfg.slackOAuthDir, "slack-oauth-dir", "/etc/slack-oauth", "directory containing client_id + client_secret + team_id files (identityd Slack OIDC)")
	fs.BoolVar(&cfg.webDev, "web-dev", false, "DEV ONLY: load UI bundles from a Vite dev server (HMR) instead of the embedded build")
	fs.StringVar(&cfg.webDevURL, "web-dev-url", "", "Vite dev server base URL when --web-dev is set (default http://localhost:5173)")
	fs.BoolVar(&cfg.insecureTrustLinks, "insecure-trust-links", false, "DEV ONLY: trust the signed link subject directly when no IdP or channel authenticator is configured; restores pre-IdP behavior")
	fs.StringVar(&cfg.spicedbEndpoint, "spicedb-endpoint", "", "SpiceDB gRPC endpoint (empty disables the artifact viewer)")
	fs.BoolVar(&cfg.spicedbInsecure, "spicedb-insecure", false, "Use a plaintext (non-TLS) SpiceDB gRPC connection")
	fs.StringVar(&cfg.spicedbTokenPath, "spicedb-token-path", "/var/run/webd/spicedb-token/token", "path to webd's SpiceDB preshared-token file (takes precedence over SPICEDB_TOKEN)")
	fs.IntVar(&cfg.iconMaxEntries, "icon-max-entries", 256, "max cached favicon entries for the identityd icon resolver")
	fs.DurationVar(&cfg.iconTTL, "icon-ttl", 24*time.Hour, "positive cache TTL for resolved favicons")
	fs.DurationVar(&cfg.iconNegTTL, "icon-neg-ttl", 1*time.Hour, "negative cache TTL for failed favicon resolutions")
	fs.IntVar(&cfg.maxLiveSessionsPerSubject, "max-live-sessions-per-subject", 0,
		fmt.Sprintf("max concurrent live browser sessions ONE viewer may hold on this webd (0 = built-in default, %d)", chat.MaxLiveSessionsPerSubject()))
	fs.IntVar(&cfg.maxLiveSessions, "max-live-sessions", 0,
		fmt.Sprintf("max concurrent live browser sessions this webd process will host in total (0 = built-in default, %d)", chat.MaxLiveSessions()))
	fs.StringSliceVar(&cfg.startNamespaces, "session-start-namespaces", []string{"default"},
		"namespaces this webd may CREATE a browser session in; must match the namespaced Roles granting it Channel/Secret/AgentSession writes (`oap install` ships one, for \"default\"). Empty means none — the dashboard then offers no start control and says so, rather than offering one whose every press fails at the API server.")
	fs.StringVar(&cfg.admindURL, "admind-url", "", "admind API base URL; defaults to the operator URL when empty (e.g. http://spicebox-operator.agentprimitives-system.svc:8082)")
	fs.StringVar(&cfg.admindTokenPath, "admind-token-path", "/var/run/webd/admind-token/token", "path to the admind service token file")
	fs.StringVar(&cfg.clusterKind, "cluster-kind", "", fmt.Sprintf("REQUIRED cluster kind stamped by `oap install` (one of: %s); selects the install profile, whose AllowsSharedOrigin() decides whether the trusted and sandbox origins may share one host. Empty or unrecognized is a FATAL startup error — no silent default. A Deployment predating this flag must be re-applied via `oap install`.", strings.Join(cloud.RegisteredKeys(), ", ")))
	fs.StringVar(&cfg.accessTokenNamespace, "accesstoken-namespace", "agentprimitives-system", "namespace the /mcp Minter creates AccessToken CRs in, and the bearer middleware lists them back from")
	fs.DurationVar(&cfg.accessTokenLifetime, "accesstoken-lifetime", 2160*time.Hour, "how long a token minted by /oauth/token stays valid")
	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"trusted-base-url":              "WEBD_TRUSTED_BASE_URL",
		"sandbox-base-url":              "WEBD_SANDBOX_BASE_URL",
		"operator-url":                  clikit.EnvOperatorMemoryURL,
		"nats-url":                      clikit.EnvNATSURL,
		"webd-token-path":               "WEBD_MEMORY_TOKEN_PATH",
		"web-dev-url":                   "WEBD_WEB_DEV_URL",
		"spicedb-endpoint":              spicedb.EnvEndpoint,
		"spicedb-insecure":              spicedb.EnvInsecure,
		"spicedb-token-path":            spicedb.EnvTokenPath,
		"icon-max-entries":              "AP_IDENTITYD_ICON_MAX_ENTRIES",
		"icon-ttl":                      "AP_IDENTITYD_ICON_TTL",
		"icon-neg-ttl":                  "AP_IDENTITYD_ICON_NEG_TTL",
		"admind-url":                    "ADMIND_URL",
		"admind-token-path":             "ADMIND_TOKEN_PATH",
		"cluster-kind":                  "AP_CLUSTER_KIND",
		"max-live-sessions-per-subject": "AP_WEBD_MAX_LIVE_SESSIONS_PER_SUBJECT",
		"max-live-sessions":             "AP_WEBD_MAX_LIVE_SESSIONS",
		"session-start-namespaces":      "AP_WEBD_SESSION_START_NAMESPACES",
		"accesstoken-namespace":         "WEBD_ACCESSTOKEN_NAMESPACE",
		"accesstoken-lifetime":          "WEBD_ACCESSTOKEN_LIFETIME",
	})
	return cmd
}

func run(ctx context.Context, cfg *config) error {
	logger := ctrlzap.New(ctrlzap.UseDevMode(false))
	ctrl.SetLogger(logger)

	// clusterStrategy resolves the AP_CLUSTER_KIND `oap install` stamped onto
	// this Deployment, fail-closed: an unset or unrecognized value is fatal —
	// matching the operator's resolveClusterKindFromEnv contract
	// (internal/cmd/operator/main.go). A silent fallback would serve the wrong
	// shared-origin/local-web-chat posture on a Deployment that simply
	// predates this flag; the accepted consequence is that such a Deployment
	// must be re-applied via `oap install` (or `oap desktop`'s own install path),
	// not patched in place.
	clusterStrategy, err := cloud.For(cfg.clusterKind)
	if err != nil {
		return fmt.Errorf("resolve --cluster-kind/AP_CLUSTER_KIND (fail-closed startup): %w", err)
	}
	// allowsSharedOrigin is whether webd may serve its trusted-auth and
	// untrusted-artifact origins from one host at all (the loopback half of the
	// INSECURE shared-origin debug mode). It is read from the cluster kind's
	// InstallProfile, not a standalone flag, and it is the ONLY InstallProfile
	// answer that drives behavior in this binary: which WebUI plugins mount is
	// each plugin's own per-request authorization question, never a
	// deployment-shape one.
	allowsSharedOrigin := clusterStrategy.InstallProfile().AllowsSharedOrigin()

	// The live-session caps, applied BEFORE any request is served: the registry
	// is built lazily during the WebUI mount loop, and chat.SetLimits is not
	// safe to call once it is serving. Zero leaves the built-in default.
	chat.SetLimits(cfg.maxLiveSessionsPerSubject, cfg.maxLiveSessions)

	// URLs may be empty at startup: a fresh install has no external URLs until
	// the operator (or `oap init --local`) populates the spicebox-webd-external-url
	// ConfigMap. webd still builds the dynamic server — the externalurl providers
	// + dynamic Host dispatch handle the unconfigured case (hostOnly("")=="" → 404
	// until populated). The signing key + k8s client remain hard requirements.
	if cfg.trustedBaseURL == "" || cfg.sandboxBaseURL == "" {
		fmt.Fprintln(os.Stderr, "webd: trusted/sandbox base URLs may be empty until the spicebox-webd-external-url ConfigMap is populated")
	}

	keyBytes, err := readHexKey(cfg.signingKeyPath)
	if err != nil {
		return fmt.Errorf("load signing key: %w", err)
	}
	cookieSigner := passthroughlink.New(keyBytes,
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd),
	)

	// k8s client — identityd (hosted in-process) reads AgentSessions and
	// writes UserIdentity + master Secrets through it. The scheme must carry
	// both the spicebox CRDs and the core types (Secrets) identityd touches.
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spiceboxv1alpha1.AddToScheme(scheme))
	// Mirror identityd: in-cluster config first, KUBECONFIG fallback for
	// local dev.
	restCfg, err := kube.RestConfig()
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	k8sClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}

	// Live trusted + sandbox URLs. externalurl wants a typed client-go
	// clientset (a literal GET on the named ConfigMap), NOT the
	// controller-runtime client (which would force list+watch RBAC). Each
	// provider tracks one ConfigMap key; bootstrapped from the flags so a
	// deployment that sets WEBD_*_BASE_URL works immediately, with the poller
	// overriding once `oap init --local` patches the ConfigMap.
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	trustedURL := externalurl.NewProviderFor(cs, logger, spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdTrustedURLKey, cfg.trustedBaseURL)
	sandboxURL := externalurl.NewProviderFor(cs, logger, spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdSandboxURLKey, cfg.sandboxBaseURL)
	go trustedURL.Run(ctx)
	go sandboxURL.Run(ctx)

	// Shared-origin is gated PER-REQUEST against the LIVE host (the bootstrap
	// URL is empty until oap init --local populates the ConfigMap). When the
	// --allow-shared-origin flag is set, we permit the INSECURE shared-origin
	// debug mode but ONLY on an ngrok host (and ONLY when the two origins
	// resolve to the same host — see webui.ServeHTTP). It can never engage in
	// production, where the two origins are distinct hosts.
	sharedOriginOK := sharedOriginPredicate(cfg.allowSharedOrigin, allowsSharedOrigin)
	if cfg.allowSharedOrigin {
		if allowsSharedOrigin {
			fmt.Fprintf(os.Stderr, "webd: --allow-shared-origin set on cluster-kind=%s (shared origin allowed); shared-origin will engage on the loopback OR an ngrok equal-host (insecure — desktop/ngrok only)\n", clusterStrategy.Key())
		} else {
			fmt.Fprintln(os.Stderr, "webd: --allow-shared-origin set; shared-origin will engage ONLY on an ngrok equal-host (insecure, ngrok-debug only)")
		}
	}

	// authenticate verifies the request's identityd-minted session cookie and
	// returns the canonical subject. Passed to NewServer directly (framework-
	// level; not part of the opaque per-component deps).
	authenticate := func(r *http.Request) (string, bool) {
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" {
			return "", false
		}
		p, err := cookieSigner.Verify(c.Value,
			passthroughlink.WithExpectedIssuer(passthroughlink.IssuerIdentityd),
			passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
		if err != nil || p.Subject == "" {
			return "", false
		}
		return p.Subject.String(), true
	}

	// identityd collaborators (hosted in-process). identityd is configured
	// whenever webd is past the base-URL/idle gate: it needs only the k8s
	// client, the link signer (== cookieSigner), the authenticators map, and
	// the icon handler — all available here. Built once; carried on webdDeps.
	//
	// loadSlackOAuth is non-fatal: webd boots even when the Slack OAuth Secret
	// is absent (the Slack authenticator is simply not registered, and OIDC
	// for the slack kind is off). The trusted base URL is used for OIDC
	// redirect URIs so "Sign in with Slack" lands back on webd, not identityd.
	//
	// NOTE: the authenticators take a STRING snapshot of the trusted URL here,
	// not a live getter — the redirect_uri embedded in each authenticator is
	// pinned at startup. A ConfigMap URL rotation (e.g. `oap init --local`
	// swapping the ngrok tunnel) needs a webd restart for authenticator-based
	// OIDC to pick it up. Everything else (host dispatch, artifact links,
	// beginLogin, identityd's own ExternalBaseURL) follows the URL live; making
	// authenticator redirect_uris live would require channelkinds.WebAuthDeps
	// to take a getter instead of a string (a future change).
	slackID, slackSecret, slackTeamID, err := loadSlackOAuth(cfg.slackOAuthDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webd: slack OAuth creds not loaded from %q (%v); OIDC for the slack kind is OFF\n", cfg.slackOAuthDir, err)
	}
	authenticators := buildAuthenticators(slackID, slackSecret, slackTeamID, trustedURL.Get())
	iconHandler := buildIconHandler(k8sClient, cfg.iconMaxEntries, cfg.iconTTL, cfg.iconNegTTL)

	base := &webdDeps{
		k8s:                k8sClient,
		linkSigner:         cookieSigner,
		externalBaseURL:    trustedURL.Get,
		authenticators:     authenticators,
		iconHandler:        iconHandler,
		insecureTrustLinks: cfg.insecureTrustLinks,
		logger:             logger,
	}

	// Artifact live-viewer deps. Requires SpiceDB + the channelsd system
	// memory token + the operator URL; absent any of them, the viewer stays
	// unconfigured. buildArtifactViewDeps returns a genuine nil *artifactViewDeps
	// in that case, so the umbrella passed to NewServer is the identity-only
	// *webdDeps — deps.(artifactview.Deps) then FAILS (no viewer routes; fail
	// closed) while deps.(identityd.WebDeps) SUCCEEDS (identity keeps serving).
	// When the viewer IS configured, the umbrella is *artifactViewDeps, which
	// embeds *webdDeps and so satisfies BOTH interfaces.
	// NATS powers the live-view status panel (session plan_update/notification
	// events). Best-effort: a missing URL or failed connect leaves nc nil, and
	// the live-view degrades to revisions-only rather than failing to start.
	var nc *nats.Conn
	if cfg.natsURL != "" {
		c, err := apnats.ConnectFromEnv(cfg.natsURL, "webd-live-view")
		if err != nil {
			fmt.Fprintf(os.Stderr, "webd: NATS connect failed (%v); live-view status panel disabled\n", err)
		} else {
			nc = c
			defer nc.Drain() //nolint:errcheck // best-effort drain on shutdown
		}
	}
	// chat.Shutdown tears down every live chat session (unsubscribe relays,
	// delete ephemeral Channels/AgentSessions) — deferred AFTER nc.Drain so it
	// runs BEFORE the drain (defers are LIFO): the sessions' relays must
	// unsubscribe while the shared NATS connection is still live. A nil-safe
	// no-op when the plugin's prerequisites were never met.
	defer chat.Shutdown(context.Background())

	// admind token (best-effort: missing/empty → admin UI disabled). The admind
	// API is served on the same operator listener as /memory; when ADMIND_URL is
	// unset we fall back to OPERATOR_MEMORY_URL (identical service, different path).
	admindURL := cfg.admindURL
	if admindURL == "" {
		admindURL = cfg.operatorURL
	}
	admindToken := ""
	if tokenRaw, readErr := os.ReadFile(cfg.admindTokenPath); readErr != nil {
		fmt.Fprintf(os.Stderr, "webd: admind token absent (%v); admin UI disabled\n", readErr)
	} else if t := strings.TrimSpace(string(tokenRaw)); t != "" {
		admindToken = t
	} else {
		fmt.Fprintln(os.Stderr, "webd: admind token file is empty; admin UI disabled")
	}

	var deps webui.Deps = base
	if av := buildArtifactViewDeps(base, nc, cfg.operatorURL, cfg.webdTokenPath, cfg.spicedbEndpoint, cfg.spicedbInsecure, cfg.spicedbTokenPath, keyBytes, cookieSigner, trustedURL.Get, sandboxURL.Get, logger, admindURL, admindToken, cfg.startNamespaces, cfg.accessTokenNamespace, cfg.accessTokenLifetime); av != nil {
		deps = av
	}
	// The transcript data plane needs NATS + SpiceDB + the operator URL (see
	// pkg/web/webui/chat.Deps); when SpiceDB itself is unconfigured, deps stays the
	// identity-only *webdDeps above, whose deps.(chat.Deps) cast fails —
	// chat.Routes then never even reaches its own NATS/Authz checks, so its
	// three named refusals never fire and nothing says why the conversation
	// routes are absent. Surface that one combination here; the finer-grained
	// NATS/Authz/operator-URL reasons are logged by chat.Routes itself.
	if _, ok := deps.(chat.Deps); !ok {
		fmt.Fprintln(os.Stderr, "webd: SpiceDB is not configured; the conversation routes cannot mount (see the SpiceDB warning above)")
	}

	// beginLogin redirects a cookie-less browser GET on an AuthLoginIfNecessary
	// route to identityd's generic /oidc/login with the signed deep-link (d+sig)
	// and a `next` that returns to the originally-requested page. A signed
	// artifact-view link (d+sig present) is used verbatim. When neither d nor sig
	// is present, a session-less login link is minted on the fly for the three
	// flows that need one — the /admin console, the platform-admin artifact
	// viewer opened with unsigned artifactId params, and the /sessions dashboard
	// — so the browser still gets an interactive login instead of a dead-end
	// 401. See needsSessionlessLogin's own doc comment for exactly which paths
	// qualify.
	beginLogin := func(r *http.Request) (string, bool) {
		q := r.URL.Query()
		d, sig := q.Get("d"), q.Get("sig")
		if d == "" || sig == "" {
			if needsSessionlessLogin(r.URL.Path, q.Get("artifactId")) {
				// Mint a session-less login link so the browser gets a login flow
				// even without a pre-signed link. The link only establishes an IdP
				// identity; CheckView / view_audit still gate what the subject sees.
				var err error
				d, sig, err = adminui.LoginLink(base.linkSigner)
				if err != nil {
					logger.Error(err, "webd: minting session-less login link failed")
					return "", false
				}
			} else {
				return "", false
			}
		}
		b := strings.TrimRight(trustedURL.Get(), "/")
		// next is a same-origin RELATIVE path: identityd is hosted on this
		// same listener, and its redirectNextOrLink only honors relative
		// next values (open-redirect defense). RequestURI is "/path?query".
		next := r.URL.RequestURI()
		return b + "/oidc/login?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig) + "&next=" + url.QueryEscape(next), true
	}

	// channelwebhook is NOT self-registering (unlike every other built-in
	// WebUI in the registry.All() list below): its two dependencies — the k8s
	// client and a NATS publish func — are only available here, at startup,
	// and channelwebhook.New validates both are non-nil before returning a
	// UI. When NATS is not configured (nc == nil, logged above), we simply
	// never call New, rather than handing it a publish closure that would
	// nil-panic on the first delivery — see channelwebhook.New's own doc.
	uis := registry.All()
	if nc != nil {
		whUI, err := channelwebhook.New(k8sClient, func(subject string, payload []byte) error {
			return nc.Publish(subject, payload)
		})
		if err != nil {
			return fmt.Errorf("webd: channelwebhook: %w", err)
		}
		uis = append(uis, whUI)
	} else {
		fmt.Fprintln(os.Stderr, "webd: NATS not configured; the channel webhook route (/webhooks/...) is disabled")
	}

	// Host getters are resolved per-request by the framework. They return the
	// providers' LIVE full URL; webui's hostOnly() strips each to a bare host,
	// so dynamic Host dispatch tracks a rotating ngrok URL without a restart.
	srv, err := webui.NewServer(authenticate, beginLogin, trustedURL.Get, sandboxURL.Get, sharedOriginOK, deps, uis)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	if cfg.webDev {
		u := resolveWebDevURL(cfg.webDevURL)
		srv.SetWebDev(u)
		fmt.Fprintf(os.Stderr, "webd: --web-dev set; loading UI from Vite dev server %s (INSECURE, dev only)\n", u)
	}
	if cfg.insecureTrustLinks {
		fmt.Fprintln(os.Stderr, "webd: --insecure-trust-links set; link subject trusted directly when no IdP/authenticator configured (INSECURE, dev only)")
	}
	oidcKinds := make([]string, 0, len(authenticators))
	for name := range authenticators {
		oidcKinds = append(oidcKinds, name)
	}
	sort.Strings(oidcKinds) // deterministic startup banner
	fmt.Fprintf(os.Stderr, "webd: starting addr=%s trusted=%s sandbox=%s uis=%d oidc-kinds=%v cluster-kind=%s\n", cfg.addr, trustedURL.Get(), sandboxURL.Get(), len(registry.All()), oidcKinds, clusterStrategy.Key())
	// Carry webd's kept logger into every request context. controller-runtime's
	// global logger is poisoned with a Nop sink by a transitive spicedb init()
	// before main() runs (SetLogger is first-write-wins), so handlers'
	// log.FromContext on a bare context would otherwise drop every line. The
	// webui server's BaseContext propagates this into each request context.
	return srv.Run(ctrllog.IntoContext(ctx, logger), cfg.addr)
}

// needsSessionlessLogin reports whether a cookie-less GET on an
// AuthLoginIfNecessary route (with no pre-signed d+sig link) should get a
// session-less login link minted on the fly rather than a hard 401. Three
// flows qualify today: the /admin console, the platform-admin artifact viewer
// opened with unsigned artifactId params, and the /sessions dashboard. Each
// needs only an authenticated IdP identity; the link grants no authorization
// — CheckView / view_audit / LookupInteractableSessions still gate what the
// subject sees.
//
// Without /sessions here, a cookie-less first visit to the session dashboard
// hard-errors: it is an AuthLoginIfNecessary Page but would have no way to
// reach the password login, the exact bug this function's own history
// records for /chat before its Page route was removed (see below).
//
// The built-in web chat's own Page route (/chat, AuthLoginIfNecessary) is
// gone — pkg/web/webui/chat now serves only session-scoped, AuthAuthorized API
// routes under /sessions/api/{ns}/{name}/..., none of which reach this
// decision (their Authorize closure answers 401/403/503 itself; there is no
// cookie-less login flow to offer on an API route). pkg/web/webui/sessions' own
// /sessions/api/sessions and /sessions/api/start are AuthAuthenticated, not
// AuthLoginIfNecessary, for the same reason — they don't reach this decision
// either; only the GET /sessions page itself does.
func needsSessionlessLogin(path, artifactID string) bool {
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		return true
	}
	if path == "/sessions" {
		return true
	}
	return path == "/artifact-view" && artifactID != ""
}

// webdDeps is the identity-only umbrella: it satisfies identityd.WebDeps (the
// five methods below) and nothing else. webd ALWAYS builds one — identityd is
// configured the moment webd is past the base-URL/idle gate. It is also the
// base embedded by artifactViewDeps so the viewer-capable umbrella satisfies
// BOTH interfaces.
//
// Fail-closed is achieved by TYPE SELECTION, not by embedding a possibly-nil
// interface: when the viewer's prerequisites are unmet, webd passes *webdDeps
// (the identity-only type) to NewServer, so deps.(artifactview.Deps) fails and
// the viewer routes are absent — while deps.(identityd.WebDeps) still succeeds.
// No nil-embedded-interface trap: every field here is a concrete value set at
// construction, never a typed-nil interface promoted into a "non-nil" cast.
type webdDeps struct {
	k8s        client.Client
	linkSigner *passthroughlink.Signer
	// externalBaseURL returns the LIVE trusted base URL (the externalurl
	// provider's Get), so identityd's minted OIDC redirect URIs follow a
	// ConfigMap-rotated URL (e.g. `oap init --local` swapping the ngrok tunnel)
	// without a webd pod restart.
	externalBaseURL    func() string
	authenticators     map[string]channelkinds.WebAuthenticator
	iconHandler        http.Handler
	insecureTrustLinks bool
	// logger is webd's own kept logger (run()'s `logger`, the same one
	// srv.Run carries into every request context). Present on the
	// identity-only umbrella too — not just *artifactViewDeps, whose own
	// Logger() method mirrors this same accessor — so a WebUI plugin whose
	// deps.(Deps) cast fails because *webdDeps is missing SOME OTHER
	// collaborator (e.g. a plugin that also needs CheckInteract, absent here)
	// can still log that failure instead of degrading to a silent 404. See
	// pkg/web/webui/agentui's Routes for the consumer this exists for.
	logger logr.Logger
}

// identityd.WebDeps implementation. ExternalBaseURL returns the TRUSTED base
// URL so identityd's minted OIDC redirect URIs land back on webd.
func (d *webdDeps) K8s() client.Client                  { return d.k8s }
func (d *webdDeps) LinkSigner() *passthroughlink.Signer { return d.linkSigner }
func (d *webdDeps) ExternalBaseURL() string             { return d.externalBaseURL() }
func (d *webdDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return d.authenticators
}
func (d *webdDeps) IconHandler() http.Handler { return d.iconHandler }
func (d *webdDeps) InsecureTrustLinks() bool  { return d.insecureTrustLinks }

// Logger mirrors artifactViewDeps.Logger() (below) so a WebUI plugin's
// deps.(Deps) cast-failure log (e.g. pkg/web/webui/agentui.Routes) still has a
// logger to log through even in the degraded case where deps is the
// identity-only *webdDeps — not just when the viewer's full collaborator set
// (SpiceDB + memory token + operator URL) is configured.
func (d *webdDeps) Logger() logr.Logger { return d.logger }

// artifactViewDeps is webd's concrete implementation of artifactview.Deps. It
// stores the collaborators directly; the methods contain the logic. This keeps
// internal/cmd/webd's k8s/memory/spicedb imports out of the artifactview package while
// eliminating the closure-of-closures indirection that was only needed when the
// type lived in the import-free webui package.
//
// It EMBEDS *webdDeps so a single value satisfies BOTH identityd.WebDeps (via
// the embedded methods) and artifactview.Deps (via the methods below). webd
// passes this type to NewServer only when the viewer's prerequisites are met.
type artifactViewDeps struct {
	*webdDeps
	nc           *nats.Conn // session-event subscriptions for the live-view status panel; nil → status disabled
	spdb         *spicedb.Client
	artSvc       *artifacts.Service
	cookieSigner *passthroughlink.Signer
	ctSigner     *contenttoken.Signer
	// mem is the SAME httpclient.Client buildArtifactViewDeps constructs artSvc
	// from, additionally kept here — declared as the memory.Memory INTERFACE,
	// not *httpclient.Client — so agent-ui data bindings (pkg/web/uibindings'
	// "memory" resolver, reached via agentui.Deps.Memory()) can issue a
	// session-scoped Query. Declaring the field as the interface (rather than
	// the concrete pointer) is load-bearing: assigning a typed-nil
	// *httpclient.Client into an interface field produces a NON-nil interface
	// whose Memory()!=nil check would pass and then panic on first call — see
	// AGENTS.md's "never assign a typed-nil pointer to an interface field"
	// rule. mem is only ever assigned from a real, already-constructed client
	// in buildArtifactViewDeps, so on the nil-*artifactViewDeps path (viewer
	// prerequisites unconfigured) this field is never reached at all — the
	// whole struct doesn't exist — and on the configured path it is never a
	// typed nil.
	mem memory.Memory
	// viewMinter mints the browser page's live_view_offer "View live" links.
	// nil when webd has no passthroughlink signing key — the browser sender
	// then surfaces the unavailability loudly instead of dropping offers.
	viewMinter  channelkinds.ArtifactViewMinter
	token       string
	operatorURL string
	// trustedURLGet / sandboxURLGet return the LIVE trusted + sandbox base
	// URLs (the externalurl providers' Get), so the viewer's origin/CSP wiring
	// follows a ConfigMap-rotated URL without a restart.
	trustedURLGet func() string
	sandboxURLGet func() string
	logger        logr.Logger
	// admindURL is the operator service base URL for /admin/v1/* (admind API).
	// admindToken is the bearer token forwarded to admind. Both are empty when
	// the admin UI is unconfigured, and adminui.Routes returns nil (fail-closed).
	admindURL   string
	admindToken string
	// startNamespaces is --session-start-namespaces, carried here because both
	// browser-facing start routes read it per request through
	// StartableNamespaces (see config.startNamespaces for what it means).
	startNamespaces []string
	// accessTokenNamespace is --accesstoken-namespace: where the /mcp Minter
	// creates AccessToken CRs and where the bearer middleware lists them back
	// from (mcpfront.Deps.AccessTokenNamespace).
	accessTokenNamespace string
	// minter mints an AccessToken for an approved /oauth/token exchange. A
	// concrete *mcpfront.Minter field (not an interface) so the nil check in
	// MintAccessToken below is honest — see AGENTS.md's typed-nil rule: this
	// is always either unset (nil) or assigned a real value in
	// buildArtifactViewDeps, never a typed-nil promoted through an interface
	// boundary.
	minter *mcpfront.Minter
}

// var _ agentui.Deps = (*artifactViewDeps)(nil) is a COMPILE-TIME proof that
// webd's viewer-capable umbrella satisfies pkg/web/webui/agentui's Deps interface
// (CheckInteract, embedded K8s, the Logger override below, and — for the
// POST .../bindings route — TrustedOrigin, NATSRequest, Memory, Artifacts,
// and ArtifactRenderBytes, all defined below) — a future signature drift on
// either side (e.g. CheckInteract's argument order, or Logger's return type)
// fails `go build`, not a runtime deps.(agentui.Deps) cast in production.
// Deliberately placed here, in the non-test source file alongside the type
// it certifies, rather than in main_test.go: a guard that only a _test.go
// file evaluates is invisible to `go build ./...` and so silently stops
// catching drift the moment someone runs a build without tests. *webdDeps —
// the degraded, identity-only umbrella used when the artifact-viewer's own
// prerequisites are unconfigured — deliberately does NOT satisfy
// agentui.Deps (it lacks CheckInteract): see TestUmbrellaDepsCasts and
// TestAgentUIRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly
// in main_test.go for that fail-closed case, and buildArtifactViewDeps's
// nil-on-unconfigured-prerequisites return path plus the webdDeps type's own
// doc comment (above) for the shipped silent-404 bug this whole guard chain
// exists to prevent.
var _ agentui.Deps = (*artifactViewDeps)(nil)

// var _ sessions.Deps = (*artifactViewDeps)(nil) is the same compile-time
// proof as the agentui.Deps guard above, for pkg/web/webui/sessions' Deps
// interface (LookupInteractableSessions, CheckInteract, embedded K8s,
// StartBrowserSession, and the Logger override below). *webdDeps —
// the degraded, identity-only umbrella — deliberately does NOT satisfy
// sessions.Deps either, for the same reason it fails agentui.Deps: it lacks
// CheckInteract (identityd never needed one) and LookupInteractableSessions.
var _ sessions.Deps = (*artifactViewDeps)(nil)

// var _ mcpfront.Deps = (*artifactViewDeps)(nil) is the same compile-time
// proof as the two guards above, for pkg/web/mcpfront's Deps interface
// (AccessTokenAuthz, AccessTokenNamespace, LookupReadableSessions, embedded
// K8s/OperatorURL/MemoryToken/Artifacts/ExternalBaseURL/Logger). *webdDeps —
// the degraded, identity-only umbrella — deliberately does NOT satisfy
// mcpfront.Deps either: it lacks AccessTokenAuthz and AccessTokenNamespace,
// so an unconfigured SpiceDB correctly yields NO /mcp surface rather than one
// whose every handler call would 500.
var _ mcpfront.Deps = (*artifactViewDeps)(nil)

// VerifyLink verifies a channelsd→webd deep-link and dispatches on Purpose.
// PurposeArtifactView requires BOTH ArtifactID and SessionRef (unchanged from
// before PurposeSessionView existed). PurposeSessionView requires ONLY
// SessionRef — a session isn't an artifact, so it never carries an
// ArtifactID; the returned artifactID is always "" for this purpose. Any
// other purpose (including empty) is rejected — the bearer capability is
// fail-closed: a link that doesn't match a known purpose grants nothing.
func (d *artifactViewDeps) VerifyLink(raw string) (string, string, string, error) {
	p, err := d.cookieSigner.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	if err != nil {
		return "", "", "", err
	}
	switch p.Purpose {
	case passthroughlink.PurposeArtifactView:
		if p.ArtifactID == "" || p.SessionRef == "" {
			return "", "", "", fmt.Errorf("link missing artifactId/sessionRef")
		}
		return p.ArtifactID, p.SessionRef, p.BackLink, nil
	case passthroughlink.PurposeSessionView:
		if p.SessionRef == "" {
			return "", "", "", fmt.Errorf("link missing sessionRef")
		}
		return "", p.SessionRef, p.BackLink, nil
	default:
		return "", "", "", fmt.Errorf("link purpose %q is not supported", p.Purpose)
	}
}

// AgentIdentityAuthz implements identityd.WebAuthzDeps: the click-time
// permission oracle for replacing an agent's OWN shared credential
// (agentidentity#update_credential). The monitoring-channel variant of that
// card's link is deliberately not subject-bound, so this check is the only
// control between the URL and a credential every session of the agent
// authenticates with.
//
// Returned as the INTERFACE, and nil-guarded: a typed-nil *spicedb.Client
// assigned into an interface field yields a non-nil interface that panics on
// first call, and identityd's fail-closed nil branch would never run. spdb is
// non-nil for every artifactViewDeps buildArtifactViewDeps actually returns
// (it returns nil rather than a client-less value), so this guard is belt to
// that braces — see AGENTS.md's "Nil interfaces" note for the incident.
func (d *artifactViewDeps) AgentIdentityAuthz() identityd.AgentIdentityAuthz {
	if d.spdb == nil {
		return nil
	}
	return d.spdb
}

// AgentCredentialWriter implements identityd.WebAuthzDeps: the client that
// submits an agent-owned credential replacement to the OPERATOR.
//
// webd deliberately has NO Kubernetes Secret access outside the identities
// namespace, so it cannot perform this write itself — and should not be able
// to. The operator re-resolves the target and re-checks
// agentidentity#update_credential on the subject webd asserts before writing;
// see pkg/web/admind/agentcred.
//
// Returned as the INTERFACE and nil when admind is unconfigured, so identityd's
// fail-closed branch runs (an explicit "no operator client wired" message)
// rather than a typed-nil client panicking on first use.
func (d *artifactViewDeps) AgentCredentialWriter() identityd.AgentCredentialWriter {
	if d.admindURL == "" || d.admindToken == "" {
		return nil
	}
	return agentcred.New(d.admindURL, d.admindToken)
}

// AccessTokenAuthz implements mcpfront.Deps: the SpiceDB half of an
// authorized /mcp tool call. Returned as the INTERFACE and nil-guarded for
// the same reason as AgentIdentityAuthz below — a typed-nil *spicedb.Client
// assigned into an interface field would yield a non-nil interface that
// panics on first call, and mcpfront's own fail-closed branch
// (ui.Routes: "AccessTokenAuthz() == nil → no routes") would never run.
func (d *artifactViewDeps) AccessTokenAuthz() mcpfront.AccessTokenAuthz {
	if d.spdb == nil {
		return nil
	}
	return d.spdb
}

// AccessTokenNamespace implements mcpfront.Deps: the SAME namespace
// d.minter creates AccessToken CRs in (see buildArtifactViewDeps), so the
// bearer middleware's hash lookup finds every token this process's Minter
// could have produced.
func (d *artifactViewDeps) AccessTokenNamespace() string { return d.accessTokenNamespace }

// MintAccessToken implements identityd.AccessTokenMinter: identityd cannot
// import pkg/web/mcpfront (the import direction in this repo is web ->
// platform, never the reverse — see handlers_oauthas_token.go's package
// doc), so AccessTokenMinter/MintParams/Minted there are a field-for-field
// MIRROR of mcpfront's real types, not an import of them. This method is the
// one place those two shapes are adapted into each other.
//
// Nil-guarded: d.minter is a concrete *mcpfront.Minter (never a typed-nil
// promoted through an interface — see artifactViewDeps.minter's own doc), set
// only in buildArtifactViewDeps once spdb/k8s are known real. identityd's own
// /oauth/token handler already treats a nil Minter as "unavailable" (503) at
// the webui.go wiring layer; this guard is belt to that braces, in case this
// method is ever reached some other way.
func (d *artifactViewDeps) MintAccessToken(ctx context.Context, p identityd.MintParams) (identityd.Minted, error) {
	if d.minter == nil {
		return identityd.Minted{}, fmt.Errorf("mcpfront: access-token minter not configured")
	}
	minted, err := d.minter.MintAccessToken(ctx, mcpfront.MintParams{
		Owner:        p.Owner,
		Role:         p.Role,
		ScopeClasses: p.ScopeClasses,
		Unfiltered:   p.Unfiltered,
		ClientName:   p.ClientName,
		ClientID:     p.ClientID,
	})
	if err != nil {
		return identityd.Minted{}, err
	}
	return identityd.Minted{Value: minted.Value, TokenID: minted.TokenID, ExpiresAt: minted.ExpiresAt}, nil
}

// consentClassLookupLimit bounds ConsentClasses' two SpiceDB lookups —
// generous ceilings matching pkg/web/webui/sessions' own maxListedSessions /
// maxListedClasses constants (the consent screen asks the SAME two
// questions that dashboard does, for the same subject). A subject who can
// start or interact with more classes than this needs a paginated consent
// screen, a bigger design change than this constant.
const (
	consentClassSessionLookupLimit = 200
	consentClassLookupLimit        = 500
)

// ConsentClasses implements identityd.ConsentDeps: the OAuth consent
// screen's "which agent classes may this subject grant a tool access to?"
// question. Answered as the union of LookupStartableClasses (classes the
// subject may start fresh) and the classes backing LookupInteractableSessions
// (classes behind sessions the subject may already interact with) — v1's
// answer to "what could this subject plausibly want to grant" rather than a
// dedicated SpiceDB relation of its own.
//
// Both lookups read a snapshot (fullyConsistent=false): this is a display
// list for a consent CHECKBOX, not an authorization decision — the actual
// mint narrows only what CheckAccessTokenOp's OwnerHas leg later re-confirms
// per call, so an over- or under-inclusive consent list cannot itself grant
// anything the owner doesn't independently hold.
func (d *artifactViewDeps) ConsentClasses(ctx context.Context, owner identity.CanonicalUserID) ([]identityd.ConsentClass, error) {
	if d.spdb == nil {
		return nil, fmt.Errorf("mcpfront: SpiceDB not configured")
	}

	// classes maps a "ns/name" class id to its resolved AgentClass (nil until
	// resolved) so the display-name pass below can reuse what AgentClassOf
	// already fetched for an interactable session's class, rather than
	// re-Getting it.
	classes := map[string]*spiceboxv1alpha1.AgentClass{}
	var order []string
	note := func(id string) {
		if _, ok := classes[id]; !ok {
			classes[id] = nil
			order = append(order, id)
		}
	}

	startable, err := d.LookupStartableClasses(ctx, owner, consentClassLookupLimit, false)
	if err != nil {
		return nil, fmt.Errorf("consent classes: lookup startable classes: %w", err)
	}
	for _, ref := range startable.Refs {
		note(ref.Namespace + "/" + ref.Name)
	}

	interactable, err := d.LookupInteractableSessions(ctx, owner, consentClassSessionLookupLimit, false)
	if err != nil {
		return nil, fmt.Errorf("consent classes: lookup interactable sessions: %w", err)
	}
	for _, ref := range interactable.Refs {
		ac, acErr := d.AgentClassOf(ctx, ref.Namespace, ref.Name)
		if acErr != nil {
			d.logger.Info("consent classes: resolve session's agent class failed; skipping",
				"session", ref.Namespace+"/"+ref.Name, "err", acErr.Error())
			continue
		}
		note(ac.Namespace + "/" + ac.Name)
		classes[ac.Namespace+"/"+ac.Name] = ac
	}

	sort.Strings(order)
	out := make([]identityd.ConsentClass, 0, len(order))
	for _, id := range order {
		ns, name, ok := strings.Cut(id, "/")
		if !ok {
			continue
		}
		ac := classes[id]
		if ac == nil {
			var fetched spiceboxv1alpha1.AgentClass
			if getErr := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &fetched); getErr == nil {
				ac = &fetched
			} else {
				d.logger.Info("consent classes: resolve display name failed; falling back to the class name",
					"class", id, "err", getErr.Error())
			}
		}
		display := name
		if ac != nil && ac.Spec.DisplayName != "" {
			display = ac.Spec.DisplayName
		}
		out = append(out, identityd.ConsentClass{ID: id, DisplayName: display})
	}
	return out, nil
}

func (d *artifactViewDeps) CheckView(ctx context.Context, artifactID, subject string) (bool, error) {
	// The cookie subject is the canonical form "user:<id>"; SpiceDB's
	// CheckArtifactView builds the user object itself, so it wants the
	// bare id. Strip the prefix like every other SpiceDB-id consumer
	// (slack/resolve_canonical.go, runner/loop.go, …).
	return d.spdb.CheckArtifactView(ctx, artifactID, identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"),
		"webd cookie subject, verified by webd's authenticate"), false)
}

// CheckArtifactView implements interact.Deps: same check as CheckView above,
// under the interface's name — used ONLY to validate an artifact reference
// before minting its Via, never as the send authorization (see CheckInteract).
func (d *artifactViewDeps) CheckArtifactView(ctx context.Context, artifactID, subject string) (bool, error) {
	return d.CheckView(ctx, artifactID, subject)
}

// interactCheckArgs derives the two arguments every session-scoped interact
// check is made with, so they are derived ONCE for every caller of
// CheckInteract below — /interact, the transcript data plane, the agent-UI
// routes, and the session shell's selection all reach SpiceDB through that one
// method.
//
// It is a separate function because both answers are load-bearing and neither
// is observable from the method itself (its collaborator is a concrete
// *spicedb.Client):
//
//   - the canonical id is derived through the TYPED, fail-closed conversion,
//     not strings.TrimPrefix. A subject that is not "user:"-prefixed is an
//     error, not a silently-mangled id checked against
//     "user:service:something" — pkg/web/webui/sessions' own list gate documents
//     the TrimPrefix form as the defect it avoids, and the ended-session start
//     route's subset argument depends on both gates agreeing about what the
//     subject IS.
//   - fullyConsistent is ALWAYS true, unlike CheckView's snapshot read,
//     because a just-granted interact relationship (an owner sharing
//     send-access moments ago) must be visible immediately, and — the other
//     direction, which matters more — a just-REVOKED one must not admit. The
//     ended-session start gate is documented as a strict subset of the
//     dashboard's fully-consistent derivation; a snapshot read here would make
//     that false.
func interactCheckArgs(subject string) (canonical identity.CanonicalUserID, fullyConsistent bool, err error) {
	canonical, err = identity.Subject(subject).CanonicalUserID()
	if err != nil {
		return identity.CanonicalUserID{}, true, fmt.Errorf("webd: derive canonical id for an interact check: %w", err)
	}
	if canonical.IsZero() {
		return identity.CanonicalUserID{}, true, errEmptyInteractSubject
	}
	return canonical, true, nil
}

// errEmptyInteractSubject reports a subject that decoded to an empty canonical
// id — a literal "user:" with nothing after it, which CanonicalUserID accepts
// without error. Returned rather than checked as a denial: an empty id would
// check "user:" against SpiceDB, and an indeterminate input must never read as
// a decision.
var errEmptyInteractSubject = errors.New("webd: interact check with an empty canonical subject")

// errEmptyWorkshopSubject is WorkshopNamespacesFor's twin of
// errEmptyInteractSubject above, for the same reason: a literal "user:" must
// never be compared against Workshop.Spec.StarterCanonical as if it named a
// real owner.
var errEmptyWorkshopSubject = errors.New("webd: workshop namespace lookup with an empty canonical subject")

// CheckInteract implements interact.Deps: the send-authorization check
// (agentsession#interact). See interactCheckArgs for the two properties every
// caller depends on.
func (d *artifactViewDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	canonical, fullyConsistent, err := interactCheckArgs(subject)
	if err != nil {
		return false, err
	}
	return d.spdb.CheckInteract(ctx, ns, name, canonical, fullyConsistent)
}

// LookupInteractableSessions implements sessions.Deps: the session
// dashboard's one-round-trip "which sessions may this subject interact
// with?" read. Mirrors CheckPlatformPermission's fail-closed shape — an error
// when SpiceDB isn't configured, never a bare (zero, nil) — because a page
// whose list silently comes back empty is indistinguishable from a viewer who
// genuinely holds no sessions, which is exactly the ambiguity AGENTS.md's
// no-silent-errors rule (an indeterminate result must never read as a
// negative) forbids.
func (d *artifactViewDeps) LookupInteractableSessions(ctx context.Context, canonicalID identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.InteractableSessions, error) {
	if d.spdb == nil {
		return spicedb.InteractableSessions{}, fmt.Errorf("sessions: SpiceDB not configured")
	}
	return d.spdb.LookupInteractableSessions(ctx, canonicalID, limit, fullyConsistent)
}

// mcpReadableSessionsLimit bounds LookupReadableSessions below, matching
// pkg/web/webui/sessions' own maxListedSessions — the /mcp tool surface
// enumerates the same interactable set the session dashboard does, for the
// same subject.
const mcpReadableSessionsLimit = 200

// LookupReadableSessions implements mcpfront.Deps: v1's answer to "which
// sessions may this token's owner READ" is the interactable set itself
// (interact implies read_transcript for a root session — see
// pkg/authz/spicedb's $sameperm mirror), not a dedicated read-only lookup.
// fullyConsistent=true: an /mcp tool caller enumerating sessions needs a
// just-granted (or just-revoked) interact relationship reflected immediately,
// the same justification interactCheckArgs gives CheckInteract above.
func (d *artifactViewDeps) LookupReadableSessions(ctx context.Context, owner identity.CanonicalUserID) (spicedb.InteractableSessions, error) {
	if d.spdb == nil {
		return spicedb.InteractableSessions{}, fmt.Errorf("mcpfront: SpiceDB not configured")
	}
	return d.spdb.LookupInteractableSessions(ctx, owner, mcpReadableSessionsLimit, true)
}

// LookupStartableClasses implements sessions.Deps: the bootstrap arm of the
// start gate. Fails closed with an error rather than an empty set when SpiceDB
// is absent — an empty answer here is indistinguishable from "you may start
// nothing", and the caller reports an error as an incomplete set (which
// refuses to become a denial) while an empty one would read as complete.
func (d *artifactViewDeps) LookupStartableClasses(ctx context.Context, canonicalID identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.StartableClasses, error) {
	if d.spdb == nil {
		return spicedb.StartableClasses{}, fmt.Errorf("sessions: SpiceDB not configured")
	}
	return d.spdb.LookupStartableClasses(ctx, canonicalID, limit, fullyConsistent)
}

// AgentClassOf implements interact.Deps: resolves the AgentClass backing
// session (ns, name) so the /interact handler can read its session_views
// capability grant. Class names the AgentClass in the SAME namespace as the
// session (spiceboxv1alpha1.AgentSessionSpec.Class doc) — mirrors how
// pkg/web/webui/chat/handlers.go resolves a session's AgentClass. Either Get
// failure is returned as-is so the caller fails closed (no class ⇒ no
// session_views ⇒ no interaction permitted).
func (d *artifactViewDeps) AgentClassOf(ctx context.Context, ns, name string) (*spiceboxv1alpha1.AgentClass, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return nil, err
	}
	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.Class}, &ac); err != nil {
		return nil, err
	}
	return &ac, nil
}

// NATSRequest implements interact.Deps: adapts *nats.Conn.Request to
// channelevents.RequestFunc, same shape as pkg/web/webui/chat/session.go's
// browser.HostConfig wiring. Returns nil when NATS is unconfigured (d.nc is
// nil — independently optional even on a fully-configured artifactViewDeps,
// see the comment above chat.Deps' NATS()) rather than a closure that would
// panic on a nil *nats.Conn: downstream (channelkinds.RequestViewMessage)
// checks deps.NATSRequest == nil and fails closed with an explicit error.
func (d *artifactViewDeps) NATSRequest() channelevents.RequestFunc {
	if d.nc == nil {
		return nil
	}
	return func(subj string, p []byte, timeout time.Duration) ([]byte, error) {
		msg, err := d.nc.Request(subj, p, timeout)
		if err != nil {
			return nil, err
		}
		return msg.Data, nil
	}
}

// Memory implements agentui.Deps (and, via resolverDeps, pkg/web/uibindings.Deps):
// the "memory" data-binding resolver's session-scoped Query. Returns d.mem
// verbatim — a genuine nil memory.Memory interface when this webd instance's
// viewer prerequisites are unconfigured is impossible on THIS type (d.mem is
// only ever assigned from a real client; see the field's own doc comment),
// but on the OTHER *webdDeps umbrella deps.(agentui.Deps) already fails the
// cast entirely, so no caller ever observes a typed-nil here.
func (d *artifactViewDeps) Memory() memory.Memory { return d.mem }

// Artifacts implements agentui.Deps: the "artifact" data-binding resolver's
// handle -> render lookup. artSvc is a concrete *artifacts.Service pointer,
// so — unlike Memory() above — a nil check on it is honest by construction;
// see pkg/web/uibindings.Deps' own doc comment on the three different nil-check
// shapes its four collaborators need.
func (d *artifactViewDeps) Artifacts() *artifacts.Service { return d.artSvc }

// ArtifactRenderBytes implements agentui.Deps: the "artifact" resolver's raw
// byte fetch, reached fresh on every Resolve call. FetchRender already does
// exactly this fetch for the live-view content path (same operator route,
// same channelsd system token) — returned as a method value rather than
// duplicated, so the two paths cannot silently diverge on how they fetch
// render bytes.
func (d *artifactViewDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc {
	return d.FetchRender
}

// browserSessionDeps adapts artifactViewDeps to browsersession.Deps. A
// SEPARATE type, not a method added directly to *artifactViewDeps: this type
// already declares `Authz() pipeline.Authz` (below, for chat.Deps), and
// browsersession.Deps needs `Authz() authz.Granter` — the same method name
// with a different return type cannot coexist on one type. granter is
// declared as the authz.Granter INTERFACE (browsersession.Deps' own doc
// comment), assigned only from StartBrowserSession below once d.spdb is
// confirmed non-nil, so this adapter never carries a typed-nil
// *spicedb.Client promoted into a "non-nil" interface.
type browserSessionDeps struct {
	k8s     client.Client
	granter authz.Granter
	logger  logr.Logger
	checker browsersession.StartChecker
}

func (d browserSessionDeps) K8s() client.Client                        { return d.k8s }
func (d browserSessionDeps) Authz() authz.Granter                      { return d.granter }
func (d browserSessionDeps) Logger() logr.Logger                       { return d.logger }
func (d browserSessionDeps) StartChecker() browsersession.StartChecker { return d.checker }

// StartBrowserSession implements agentui.Deps and sessions.Deps. It returns
// nil — the two start routes then never mount, and the shell reports the
// absence honestly rather than rendering a control that would 404 — unless
// this process can actually carry a browser session's outbound traffic.
//
// That question is chat.CanHostBrowserSessions', not one re-derived here: a
// browser Channel is not relayed by channelsd, so its real Sender and
// StreamDeltaSink come from browser.NewHost, which only pkg/web/webui/chat
// constructs, and only once its own three prerequisites (NATS, Authz, the
// operator memory URL) hold. Two copies of that list is how a webd offers a
// start control whose replies are silently dropped — a session that renders
// as fully live with nothing anywhere to diagnose from.
//
// d.spdb is checked HERE as well, for a different reason: it is what makes the
// authz.Granter below real. browsersession.Create needs one to write the
// started_by relationship that makes the new session immediately interactable
// by the caller who started it. *spicedb.Client already implements
// authz.Granter (TouchStartedBy and its four siblings) — see
// pkg/authz.Granter's own doc comment — so no further adaptation is needed once
// d.spdb is known non-nil. The check is not redundant with the cast in
// CanHostBrowserSessions' Authz() prerequisite: that one asks for a
// pipeline.Authz, and reading a nil interface's concrete pointer out of it is
// exactly the typed-nil trap AGENTS.md warns about.
func (d *artifactViewDeps) StartBrowserSession() browsersession.StartFunc {
	if d.spdb == nil || !chat.CanHostBrowserSessions(d) {
		return nil
	}
	bd := browserSessionDeps{k8s: d.K8s(), granter: d.spdb, logger: d.Logger(), checker: d.spdb}
	return func(ctx context.Context, p browsersession.Params) (browsersession.Created, error) {
		return browsersession.Create(ctx, bd, p)
	}
}

// LiveSessions implements sessions.Deps: this process's table of sessions with
// a live in-process sink, or nil when the built-in chat plugin never mounted
// (a shared-cluster webd, or one whose NATS / SpiceDB / operator-URL
// prerequisites are unconfigured).
//
// Read LAZILY, per request: chat's registry singleton is built during the webui
// server's UI-mount loop, which runs AFTER this deps umbrella is assembled, so
// a value captured at construction time would always be nil.
//
// The nil check is on the CONCRETE pointer and the interface is returned only
// once that pointer is real — returning `chat.CurrentRegistry()` directly would
// hand back a non-nil interface wrapping a nil *Registry, whose every method
// call panics (AGENTS.md's typed-nil rule).
func (d *artifactViewDeps) LiveSessions() sessions.LiveSessions {
	reg := chat.CurrentRegistry()
	if reg == nil {
		return nil
	}
	return reg
}

// StartableNamespaces implements sessions.Deps and agentui.Deps: the
// namespaces this process may CREATE a browser session in, straight from
// --session-start-namespaces (see config.startNamespaces for why it is
// configuration).
//
// A COPY, not the backing slice: this is read per request by two plugins, and
// handing out the config's own array would let any caller's append or sort
// rewrite what every later request sees.
func (d *artifactViewDeps) StartableNamespaces() []string {
	return append([]string(nil), d.startNamespaces...)
}

// WorkshopNamespacesFor lists Ready workshops whose owner is subject and
// returns their provisioned namespaces. One cluster-wide List per CALL,
// bounded by how many workshops exist (the per-starter cap keeps that
// small) — but "one round trip per poll" is a property of the CALLERS, not
// of this method alone: browserstart.Start calls it once because a start
// request only ever names one (subject, ns) pair, and
// pkg/web/webui/sessions.startableClassesFor resolves it exactly ONCE per
// list build and threads the result through browserstart.StartableInSet for
// every (ns, class) row, rather than calling this once per row (which, over
// several rows in several non-static namespaces, would turn one poll into
// several Lists).
//
// The empty-canonical check mirrors interactCheckArgs above rather than the
// bare CanonicalUserID() call the brief for this method sketched: a literal
// "user:" decodes to a valid, EMPTY canonical with no error, and
// WorkshopSpec.StarterCanonical is `+optional` — an empty subject would
// otherwise match every workshop some other bug left that field unset on,
// which is exactly the fail-OPEN direction this must not take.
func (d *artifactViewDeps) WorkshopNamespacesFor(ctx context.Context, subject string) ([]string, error) {
	canonical, err := identity.Subject(subject).CanonicalUserID()
	if err != nil {
		return nil, err
	}
	if canonical.IsZero() {
		return nil, errEmptyWorkshopSubject
	}
	var list spiceboxv1alpha1.WorkshopList
	if err := d.k8s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list workshops: %w", err)
	}
	var out []string
	for i := range list.Items {
		ws := &list.Items[i]
		if ws.Spec.StarterCanonical == canonical.String() && ws.Status.Phase == spiceboxv1alpha1.WorkshopPhaseReady && ws.Status.Namespace != "" {
			out = append(out, ws.Status.Namespace)
		}
	}
	return out, nil
}

// ResolveRender resolves the render to frame for the INITIAL page shell. It
// goes through ContentRender so the initial page never mints a content token
// for the raw bytes of a bundled-only artifact (svg/css). An empty renderName
// (not ready) tells the shell "nothing to frame yet" — it mounts in its loading
// state and the live-view ws poll fills it in when the preview child lands.
func (d *artifactViewDeps) ResolveRender(ctx context.Context, ns, sess, artifactID string) (string, error) {
	rn, ready, err := d.ContentRender(ctx, ns, sess, artifactID)
	if err != nil {
		return "", err
	}
	if !ready {
		return "", nil // no servable content yet; the page shows its loading state
	}
	return rn, nil
}

// ContentRender returns the render whose bytes should be FRAMED for an artifact.
// For a bundled-only kind (svg/css) that is the internal html preview child —
// the raw svg/css bytes are NEVER framed — and ready=false while the preview is
// still generating. For a standalone artifact it is the newest render.
func (d *artifactViewDeps) ContentRender(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	head, ok, err := d.artSvc.GetHead(ctx, scope, artifactID)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	if kindIsBundledOnly(head.RendererKind) {
		sourceRevID := head.Tags[artifacts.TagLatest]
		if sourceRevID == "" {
			return "", false, nil // nothing rendered yet
		}
		// Bundled-only: serve the internal html preview child, never the raw bytes.
		return d.artSvc.GetPreviewChild(ctx, scope, sourceRevID) // (renderName, ok=ready, err)
	}
	rn, err := d.artSvc.ResolveToRender(ctx, scope, artifactID)
	if err != nil {
		return "", false, err
	}
	return rn, true, nil
}

// PreviewChildRender returns the internal html preview child's render for a
// specific source revision (svg/css). ready=false until that revision's preview
// has been generated. The revision handler uses this to frame an OLDER
// bundled-only revision's preview — never the raw bytes.
func (d *artifactViewDeps) PreviewChildRender(ctx context.Context, ns, sess, revID string) (string, bool, error) {
	return d.artSvc.GetPreviewChild(ctx, memory.Scope{Kind: "session", ID: ns + "/" + sess}, revID)
}

// RenderIsBundledOnly reports whether the artifact's kind is delivered
// bundled-only (svg/css). A missing head, a Get error, or an unknown kind all
// report false — the safe default (bundled-only is an explicit, registered set;
// when we can't confirm it, we treat the artifact as standalone). A Get error is
// logged rather than swallowed.
func (d *artifactViewDeps) RenderIsBundledOnly(ctx context.Context, ns, sess, artifactID string) bool {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	head, ok, err := d.artSvc.GetHead(ctx, scope, artifactID)
	if err != nil {
		d.logger.Error(err, "artifactview: RenderIsBundledOnly GetHead errored; treating as standalone",
			"ns", ns, "sess", sess, "artifactID", artifactID)
		return false
	}
	if !ok {
		return false
	}
	return kindIsBundledOnly(head.RendererKind)
}

// kindIsBundledOnly looks up a renderer kind in the channelassets registry and
// reports whether it is delivered bundled-only (its raw bytes are never framed).
func kindIsBundledOnly(rendererKind string) bool {
	r, found := assetregistry.ByKind(rendererKind)
	return found && r.Delivery() == channelassets.DeliveryBundledOnly
}

// ArtifactMeta returns the artifact head's name + description (agent-supplied
// title/summary) for the live-view header. Empty strings when the head is
// absent; the shell falls back to the artifact id.
func (d *artifactViewDeps) ArtifactMeta(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
	head, ok, err := d.artSvc.GetHead(ctx, memory.Scope{Kind: "session", ID: ns + "/" + sess}, artifactID)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", nil
	}
	return head.Name, head.Description, nil
}

// ChannelKind returns the AgentSession's input-channel kind (e.g. "slack") so the
// live-view toolbar can pick the right thread-link icon. Returns "" (with nil
// error) when the session has no input channel (kubectl-driven sessions). On a
// Get error it returns ("", err) so the page can log and fall back.
func (d *artifactViewDeps) ChannelKind(ctx context.Context, ns, sess string) (string, error) {
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &cur); err != nil {
		return "", err
	}
	if cur.Spec.InputChannel == nil {
		return "", nil
	}
	return cur.Spec.InputChannel.Kind, nil
}

// SessionViews implements artifactview.Deps: returns the granted
// session_views.interactions for the session's AgentClass, resolved by
// agentcaps.ResolveSessionViews from spec — NEVER status, so a capability
// revocation is reflected immediately rather than after status catches up. Any
// resolution failure (an unresolvable class, a malformed grant, a malformed
// config) is logged and degrades to nil, the safe default: the shell renders a
// read-only viewer with no chat/annotator, same as an absent grant. This is
// UX ONLY — the real gate remains the /interact endpoint's own
// session_views check (pkg/web/webui/interact); nothing here is a security
// decision.
func (d *artifactViewDeps) SessionViews(ctx context.Context, ns, sess string) []string {
	ac, err := d.AgentClassOf(ctx, ns, sess)
	if err != nil {
		d.logger.Error(err, "artifactview: SessionViews AgentClassOf errored; falling back to read-only",
			"ns", ns, "sess", sess)
		return nil
	}
	views, err := agentcaps.ResolveSessionViews(ac)
	if err != nil {
		d.logger.Error(err, "artifactview: SessionViews grant unresolvable; falling back to read-only",
			"ns", ns, "sess", sess)
		return nil
	}
	return views.Interactions
}

// WatchSessionStatus subscribes to the session's out.plan_update + out.notification
// NATS events — the same stream channelsd renders into the Slack thread — and
// emits an accumulated status snapshot on each. The initial snapshot is seeded
// from the persisted AgentSession phase (NATS events aren't retained, so a
// viewer connecting mid-idle still sees the active/paused banner). Returns
// (nil, nil) when NATS is not wired; the live-view then shows revisions only.
func (d *artifactViewDeps) WatchSessionStatus(ctx context.Context, ns, sess string) (<-chan artifactview.StatusSnapshot, error) {
	if d.nc == nil {
		return nil, nil
	}
	out := make(chan artifactview.StatusSnapshot, 8)

	var mu sync.Mutex
	var snap artifactview.StatusSnapshot
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &cur); err == nil {
		snap.Phase = cur.Status.Phase
		snap.Paused, snap.PauseCause = derivePause(&cur)
	}

	emit := func() {
		mu.Lock()
		cp := snap
		cp.Plan = append([]artifactview.PlanStatusItem(nil), snap.Plan...)
		mu.Unlock()
		select {
		case out <- cp:
		case <-ctx.Done():
		}
	}

	// The raw subscribe-and-decode half is shared with WatchMessages (and,
	// later, the session-view page) via livemirror.WatchOutbound; only the
	// per-kind snapshot accumulation below is artifactview-specific.
	envs, err := livemirror.WatchOutbound(ctx, d.nc, ns, sess,
		channelevents.KindPlanUpdate, channelevents.KindNotification, channelevents.KindTurnActivity)
	if err != nil {
		close(out)
		return nil, err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-envs:
				mu.Lock()
				switch env.Kind {
				case channelevents.KindPlanUpdate:
					var pl channelevents.PlanUpdatePayload
					if json.Unmarshal(env.Payload, &pl) == nil {
						snap.Paused, snap.PauseCause = pl.Paused, pl.PauseCause
						snap.Plan = toPlanItems(pl.Items)
					}
				case channelevents.KindNotification:
					var pl channelevents.NotificationPayload
					if json.Unmarshal(env.Payload, &pl) == nil {
						snap.StatusMessage = pl.Text
					}
				case channelevents.KindTurnActivity:
					// The planless active/paused signal is the authoritative "is the
					// agent working" transition: it fires even when no plan has an
					// in_progress item — e.g. agent_work_complete → idle, where
					// emitActivity publishes NO plan_update (it only snapshots plans with
					// an in_progress step). Without this the live-view's "working"
					// animation never clears when the agent finishes.
					var pl channelevents.TurnActivityPayload
					if json.Unmarshal(env.Payload, &pl) == nil {
						snap.Paused, snap.PauseCause = !pl.Active, pl.Cause
					}
				}
				mu.Unlock()
				emit()
			}
		}
	}()

	// Self-heal from the authoritative AgentSession phase. The live NATS subs
	// above are the fast path, but core NATS has no replay: a view that missed
	// the clearing turn_activity(active:false) — opened mid-run, or across a
	// reconnect — would spin "working" forever after the session parked. Re-read
	// the phase on an interval and, when it says parked/terminal while the stream
	// still shows working, correct it (shouldSelfHeal). Also keeps snap.Phase
	// fresh for display. A fresh subscribe (a UI refresh) already reconstructs
	// via the initial seed below; this covers the still-open view.
	go func() {
		t := time.NewTicker(statusReconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				var s spiceboxv1alpha1.AgentSession
				if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &s); err != nil {
					continue
				}
				paused, cause := derivePause(&s)
				mu.Lock()
				changed := false
				if shouldSelfHeal(paused, snap.Paused) {
					snap.Paused, snap.PauseCause = paused, cause
					changed = true
				}
				if s.Status.Phase != snap.Phase {
					snap.Phase = s.Status.Phase
					changed = true
				}
				mu.Unlock()
				if changed {
					emit()
				}
			}
		}
	}()

	emit() // initial seed (AgentSession-derived active/paused)
	return out, nil
}

// WatchMessages subscribes to the session's out.user_message (the agent's
// respond_to_user replies) and out.user_echo (the user's own view-originated
// sends, mirrored back by channelsd after a view_message routes) NATS
// subjects and emits a MirrorMessage for each — the chat mirror behind the
// live-view's chat panel. Read-only: webd never publishes on these subjects
// or holds a signing key. Returns (nil, nil) when NATS is not wired; the
// live-view then shows no chat stream, exactly like WatchSessionStatus.
func (d *artifactViewDeps) WatchMessages(ctx context.Context, ns, sess string) (<-chan artifactview.MirrorMessage, error) {
	if d.nc == nil {
		return nil, nil
	}
	out := make(chan artifactview.MirrorMessage, 8)

	emit := func(m artifactview.MirrorMessage) {
		m.At = time.Now().UTC().Format(time.RFC3339)
		select {
		case out <- m:
		case <-ctx.Done():
		}
	}

	// The raw subscribe-and-decode half is shared with WatchSessionStatus (and,
	// later, the session-view page) via livemirror.WatchOutbound; only the
	// Envelope→MirrorMessage mapping below is artifactview-specific.
	envs, err := livemirror.WatchOutbound(ctx, d.nc, ns, sess, channelevents.KindUserMessage, channelevents.KindUserEcho)
	if err != nil {
		close(out)
		return nil, err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-envs:
				switch env.Kind {
				case channelevents.KindUserMessage:
					var pl channelevents.OutboundUserMessagePayload
					if json.Unmarshal(env.Payload, &pl) == nil {
						emit(userMessageToMirror(pl))
					}
				case channelevents.KindUserEcho:
					var pl channelevents.UserEchoPayload
					if json.Unmarshal(env.Payload, &pl) == nil {
						emit(userEchoToMirror(pl))
					}
				}
			}
		}
	}()

	return out, nil
}

// userMessageToMirror maps a respond_to_user reply onto the chat mirror's
// wire shape. Extracted as a pure function so the payload→MirrorMessage
// mapping is unit-testable without a live NATS connection.
func userMessageToMirror(pl channelevents.OutboundUserMessagePayload) artifactview.MirrorMessage {
	return artifactview.MirrorMessage{Role: "agent", Text: pl.Text}
}

// userEchoToMirror maps a user_echo (a view-originated send mirrored back by
// channelsd) onto the chat mirror's wire shape. Author is the echo's email —
// the same display form the origin channel's mention resolution falls back
// to. Extracted as a pure function so the mapping is unit-testable without a
// live NATS connection.
func userEchoToMirror(pl channelevents.UserEchoPayload) artifactview.MirrorMessage {
	return artifactview.MirrorMessage{Role: "user", Text: pl.Text, Author: pl.Author.Email.String(), Via: pl.Via}
}

// derivePause maps the persisted AgentSession status to (paused, cause) — the
// same mapping channelsd's watchdog uses — so the live-view shows the correct
// banner before the first NATS plan_update arrives.
// statusReconcileInterval is how often WatchSessionStatus re-reads the
// authoritative AgentSession phase to self-heal a stale status. Core NATS has
// no replay, so a live-view that missed the clearing turn_activity(active:false)
// — opened after it fired, or across a reconnect gap — would otherwise spin
// "working" forever after the session parked.
const statusReconcileInterval = 5 * time.Second

// shouldSelfHeal reports whether a phase-derived pause should override the live
// stream. It corrects ONLY a stuck-working state: the authoritative phase says
// the session is parked/terminal (phasePaused) but the stream still shows
// working (!streamPaused). The reverse — a phase Get lagging a freshly-started
// turn — is deliberately NOT corrected: the more-responsive live turn_activity
// events own the transition to working, so a lagging phase never flips a
// genuinely-working session to paused.
func shouldSelfHeal(phasePaused, streamPaused bool) bool {
	return phasePaused && !streamPaused
}

func derivePause(s *spiceboxv1alpha1.AgentSession) (bool, string) {
	switch {
	// Since Slice C2 every approval family parks on the generic
	// PendingInteractions list. info_leakage keeps its own pause class (its
	// approval prompt is a private ephemeral the requester can't see), so it is
	// checked FIRST; every other pending interaction / requester is the generic
	// in-thread approval class.
	case hasInteractionCategory(s, categories.InfoLeakage):
		return true, channelevents.PauseCauseLeakageApproval
	case len(s.Status.PendingInteractions) > 0 || len(s.Status.PendingRequesters) > 0:
		return true, channelevents.PauseCauseApproval
	}
	switch s.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry:
		return true, channelevents.PauseCauseRetry
	case spiceboxv1alpha1.AgentSessionPhaseFailed:
		return true, channelevents.PauseCauseFailed
	case spiceboxv1alpha1.AgentSessionPhaseIdle:
		return true, channelevents.PauseCauseReply
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
		return true, channelevents.PauseCauseComplete
	default: // Running / Pending → active
		return false, ""
	}
}

// hasInteractionCategory reports whether the session has at least one pending
// generic interaction of the given category (e.g. info_leakage). Used by
// derivePause to give info_leakage its own pause cause while every other
// category folds into the generic approval class.
func hasInteractionCategory(s *spiceboxv1alpha1.AgentSession, category string) bool {
	for _, e := range s.Status.PendingInteractions {
		if e.Category == category {
			return true
		}
	}
	return false
}

// RenderKind implements artifactview.Deps: resolves renderName's
// ArtifactRender.spec.kind via a K8s Get, the same CR lookup ServeTransform
// performs — factored out here so both it and rewriteArtifactRefs (which
// dispatches to a kind's channelassets.RefRewriter, see
// pkg/web/webui/artifactview/rewrite.go) share one lookup path instead of two
// hardcoded ones.
func (d *artifactViewDeps) RenderKind(ctx context.Context, ns, sess, renderName string) (string, error) {
	var cr spiceboxv1alpha1.ArtifactRender
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: renderName}, &cr); err != nil {
		return "", err
	}
	return cr.Spec.Kind, nil
}

// ServeTransform dispatches the artifact kind's live-view serve transform by
// resolving the render CR's kind and handing the bytes to the registered
// renderer. The generic content handler calls this unconditionally; every
// kind's transform is a no-op today (the artifact is served inert — see plan
// D1). On any lookup miss the bytes pass through unchanged.
func (d *artifactViewDeps) ServeTransform(ctx context.Context, ns, sess, renderName string, content []byte) []byte {
	kind, err := d.RenderKind(ctx, ns, sess, renderName)
	if err != nil {
		d.logger.Error(err, "artifactview: ServeTransform render lookup failed; serving untransformed",
			"render", renderName, "session", ns+"/"+sess)
		return content
	}
	r, ok := assetregistry.ByKind(kind)
	if !ok {
		return content
	}
	return r.ServeTransform(content)
}

func toPlanItems(items []channelevents.PlanItemRef) []artifactview.PlanStatusItem {
	out := make([]artifactview.PlanStatusItem, 0, len(items))
	for _, it := range items {
		out = append(out, artifactview.PlanStatusItem{Label: it.Label, Status: it.Status})
	}
	return out
}

func (d *artifactViewDeps) SignContentToken(ns, sess, renderName, artifactID string) (string, error) {
	return d.ctSigner.Sign(contenttoken.Claims{
		Ns: ns, Sess: sess, RenderName: renderName, ArtifactID: artifactID,
		ExpiresAt: time.Now().Add(contentTokenTTL).Unix(),
	})
}

func (d *artifactViewDeps) VerifyContentToken(tok string) (string, string, string, error) {
	c, err := d.ctSigner.Verify(tok)
	if err != nil {
		return "", "", "", err
	}
	return c.Ns, c.Sess, c.RenderName, nil
}

// ResolveAssetURL resolves an `artifact:HANDLE` reference to a token-gated
// same-origin asset URL. Resolution is SCOPED to the primary's own (ns, sess)
// session — artSvc.ResolveToRender queries memory filtered by that exact
// scope, so a handle belonging to a different session simply isn't found;
// this is the enforcement point for "same-session only", not a convention
// the caller has to uphold. ok=false (nil error) on any not-found outcome —
// the caller (rewriteArtifactRefs) drops the reference rather than failing
// the whole primary. A non-ErrNotFound error (a real backend failure, or a
// signing failure) is returned so the caller can log it.
func (d *artifactViewDeps) ResolveAssetURL(ctx context.Context, ns, sess, handle string) (string, bool, error) {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	renderName, err := d.artSvc.ResolveToRender(ctx, scope, handle)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	tok, err := d.ctSigner.SignAsset(contenttoken.Claims{
		Ns: ns, Sess: sess, RenderName: renderName,
		ExpiresAt: time.Now().Add(contentTokenTTL).Unix(),
	})
	if err != nil {
		return "", false, err
	}
	return "/artifacts/a/?ct=" + tok, true, nil
}

func (d *artifactViewDeps) VerifyAssetToken(tok string) (string, string, string, error) {
	c, err := d.ctSigner.VerifyAsset(tok)
	if err != nil {
		return "", "", "", err
	}
	return c.Ns, c.Sess, c.RenderName, nil
}

// ActiveWidgets implements sessionview.Deps: reads the session's
// AgentSession.status.activeWidgets (the runner's durable MCP-UI widget
// refs — pkg/agent/runner/loop.go's applyUIResource/AppendActiveWidget).
func (d *artifactViewDeps) ActiveWidgets(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error) {
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &cur); err != nil {
		return nil, err
	}
	out := make([]sessionview.WidgetRef, 0, len(cur.Status.ActiveWidgets))
	for _, w := range cur.Status.ActiveWidgets {
		out = append(out, sessionview.WidgetRef{ArtifactID: w.ArtifactID, Tool: w.Tool, RendererKind: w.RendererKind})
	}
	return out, nil
}

// WidgetOriginOf implements interact.Deps: reads the MCPServer origin the
// runner recorded alongside this session's widget, so the app-tool-call
// handler can pin a widget's calls to its own server.
//
// The read is SCOPED TO THE SESSION in the request path, which is what makes an
// artifact id safe to accept from the browser: an id belonging to some other
// session resolves to nothing here and the caller refuses it, so the browser
// can only ever name a widget this session already owns.
//
// found=false and a nil error is "this session has no such widget" — distinct
// from an error, which is "the answer is unknown"; the handler treats the two
// differently (403 vs 503) and must be able to tell them apart.
func (d *artifactViewDeps) WidgetOriginOf(ctx context.Context, ns, name, artifactID string) (string, bool, error) {
	var cur spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cur); err != nil {
		return "", false, err
	}
	for _, w := range cur.Status.ActiveWidgets {
		if w.ArtifactID == artifactID {
			return w.Origin, true, nil
		}
	}
	return "", false, nil
}

// SignWidgetToken / VerifyWidgetToken implement sessionview.Deps: mint and
// verify the content-capability token gating /mcpui-content + /mcpui-host
// (Kind=widget — see pkg/web/webui/contenttoken.KindWidget), so a token minted
// here can never be replayed at artifactview's /content or /artifacts/a/.
func (d *artifactViewDeps) SignWidgetToken(ns, sess, artifactID string) (string, error) {
	return d.ctSigner.SignWidget(contenttoken.Claims{
		Ns: ns, Sess: sess, ArtifactID: artifactID,
		ExpiresAt: time.Now().Add(contentTokenTTL).Unix(),
	})
}

func (d *artifactViewDeps) VerifyWidgetToken(tok string) (string, string, string, error) {
	c, err := d.ctSigner.VerifyWidget(tok)
	if err != nil {
		return "", "", "", err
	}
	return c.Ns, c.Sess, c.ArtifactID, nil
}

// FetchWidget implements sessionview.Deps: fetches a persisted MCP-UI
// widget's raw HTML bytes verbatim by resolving its ArtifactID to the
// ArtifactRender CR name that holds its bytes (artSvc.ResolveToRender),
// then fetching those bytes the SAME way FetchRender does — mcpui is an
// identity renderer, so there is no sanitize/ServeTransform step to run.
//
// Deliberately does NOT go through ContentRender/kindIsBundledOnly: mcpui is
// classified DeliveryBundledOnly (pkg/channels/channelassets/mcpui.Renderer.Delivery)
// because it must never be reachable via the generic artifact-view's raw
// content path, but it registers no PreviewComposer — ContentRender's
// bundled-only branch would look for a preview child that never exists.
// Widgets always frame their own raw render directly.
//
// WidgetMeta.CSP carries the widget's declared `_meta.ui.csp`, persisted
// alongside the ArtifactRender CR / artifact revision by
// pkg/agent/runner/loop.go's persistWidget and resolved here via
// artSvc.ResolveWidgetCSP — nil when the widget declared none (or its
// revision predates CSP persistence), which sessionview.buildWidgetCSP
// treats as "use the restrictive default", never as an error.
func (d *artifactViewDeps) FetchWidget(ctx context.Context, ns, sess, artifactID string) ([]byte, sessionview.WidgetMeta, error) {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	renderName, err := d.artSvc.ResolveToRender(ctx, scope, artifactID)
	if err != nil {
		return nil, sessionview.WidgetMeta{}, err
	}
	out, _, err := d.FetchRender(ctx, ns, sess, renderName)
	if err != nil {
		return nil, sessionview.WidgetMeta{}, err
	}
	csp, err := d.artSvc.ResolveWidgetCSP(ctx, scope, artifactID)
	if err != nil {
		// The widget's bytes already resolved above through the SAME
		// underlying revision lookup, so a CSP-only failure here is a rare
		// race (e.g. the revision GC'd between calls) rather than a hard
		// error — degrade to the restrictive default CSP rather than fail
		// to serve a widget whose bytes we already have.
		d.logger.Error(err, "sessionview FetchWidget: ResolveWidgetCSP failed; using restrictive default CSP",
			"ns", ns, "sess", sess, "artifactID", artifactID)
		csp = nil
	}
	return out, sessionview.WidgetMeta{CSP: widgetCSPMetaFrom(csp)}, nil
}

// widgetCSPMetaFrom maps v1alpha1.WidgetCSP (the CRD-facing type) to
// sessionview.WidgetCSPMeta (the package-local DTO that keeps sessionview
// free of the apis/v1alpha1 import — same convention as WidgetRef's mapping
// in ActiveWidgets above). nil in, nil out.
func widgetCSPMetaFrom(csp *spiceboxv1alpha1.WidgetCSP) *sessionview.WidgetCSPMeta {
	if csp == nil {
		return nil
	}
	return &sessionview.WidgetCSPMeta{
		ConnectDomains:  csp.ConnectDomains,
		ResourceDomains: csp.ResourceDomains,
		FrameDomains:    csp.FrameDomains,
	}
}

func (d *artifactViewDeps) FetchRender(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return fetchRenderBytes(ctx, d.operatorURL, d.token, ns, sess, renderName)
}

// FetchRenderBundle implements artifactview.Deps for the direct-download
// path: it hits the operator's smart-passthrough bundle route instead of
// the plain output route, so an html primary with resolvable artifact:
// refs downloads as a self-contained ZIP (see fetchBundleBytes).
func (d *artifactViewDeps) FetchRenderBundle(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return fetchBundleBytes(ctx, d.operatorURL, d.token, ns, sess, renderName)
}

func (d *artifactViewDeps) ListRevisions(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
	tree, err := d.artSvc.RevisionTree(ctx, memory.Scope{Kind: "session", ID: ns + "/" + sess}, artifactID)
	if err != nil {
		return nil, err
	}
	out := make([]artifactview.RevisionMeta, len(tree))
	for i, rv := range tree {
		out[i] = artifactview.RevisionMeta{
			Seq: rv.Seq, RevisionID: rv.RevisionID, RenderName: rv.RenderName,
			ChangeDescription: rv.ChangeDescription, CreatedAt: rv.CreatedAt, Tags: rv.Tags,
			Filename: rv.Filename, Size: rv.Size, MIME: rv.MIME,
		}
	}
	return out, nil
}

func (d *artifactViewDeps) TrustedOrigin() string  { return d.trustedURLGet() }
func (d *artifactViewDeps) SandboxBaseURL() string { return d.sandboxURLGet() }
func (d *artifactViewDeps) Logger() logr.Logger    { return d.logger }

// chat.Deps implementation. NATS/Authz/OperatorURL are the extra
// collaborators the transcript data plane needs beyond identityd.WebDeps +
// artifactview.Deps; K8s/TrustedOrigin/Logger are already implemented above.
// nc/spdb may be nil even on a fully-configured artifactViewDeps (NATS is
// independently optional — see cfg.natsURL), so chat.Routes' ensureRegistry
// checks these return values before mounting rather than assuming a non-nil
// *artifactViewDeps implies both are wired. They are the same three
// chat.CanHostBrowserSessions reports on, which is why StartBrowserSession
// asks it rather than re-listing them.
func (d *artifactViewDeps) NATS() *nats.Conn      { return d.nc }
func (d *artifactViewDeps) Authz() pipeline.Authz { return d.spdb }
func (d *artifactViewDeps) OperatorURL() string   { return d.operatorURL }

// ArtifactViewMinter exposes the live_view_offer link minter to the browser
// session's Host (see pkg/web/webui/chat.newBrowserHost). nil when webd
// lacks a passthroughlink signing key; the sender surfaces that loudly.
func (d *artifactViewDeps) ArtifactViewMinter() channelkinds.ArtifactViewMinter { return d.viewMinter }

// SessionViewMinter exposes the session_view_offer link minter to the
// browser session's Host. Unlike ArtifactViewMinter, this needs no
// signing key — the session-view page enforces its own CheckInteract at open
// time — so it is backed directly by the same live trusted-origin getter
// TrustedOrigin() uses and is never nil.
func (d *artifactViewDeps) SessionViewMinter() channelkinds.SessionViewMinter {
	return newWebdSessionViewMinter(d.trustedURLGet)
}

// MemoryToken exposes webd's read-only operator-memory bearer token (the same
// one artifactview reads artifacts with) to the built-in web chat, which uses
// it to replay a resumed conversation's transcript turns. It is read-only at
// the operator (system:webd token) — see pkg/web/webui/chat.Deps.MemoryToken.
func (d *artifactViewDeps) MemoryToken() string { return d.token }

// adminui.Deps implementation. AdmindBaseURL/AdmindToken return the admind
// service URL and bearer token forwarded by the reverse proxy. When either is
// empty the adminui plugin's Routes cast fails and no admin routes are served
// (fail-closed). CheckPlatformPermission delegates to spdb (already wired).
func (d *artifactViewDeps) AdmindBaseURL() string { return d.admindURL }
func (d *artifactViewDeps) AdmindToken() string   { return d.admindToken }
func (d *artifactViewDeps) CheckPlatformPermission(ctx context.Context, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	if d.spdb == nil {
		return false, fmt.Errorf("adminui: SpiceDB not configured")
	}
	return d.spdb.CheckPlatformPermission(ctx, permission, canonicalID, fullyConsistent)
}

// buildArtifactViewDeps constructs the artifact live-viewer's runtime
// dependencies. It REQUIRES SpiceDB (endpoint + token, via the canonical
// SPICEDB_* env), the channelsd system memory token (a mounted file), and
// the operator memory/artifact base URL. If any of these is missing it logs
// to stderr and returns a genuine nil *artifactViewDeps — the caller then
// passes the identity-only *webdDeps to NewServer, so the viewer's Routes
// cast fails and registers no routes (fail closed) while the rest of webd
// (health, identity surface) keeps serving. It never returns a partially-wired
// deps that could fail open.
//
// The return type is the concrete *artifactViewDeps so the caller's nil check
// is unambiguous; the nil is never assigned into the webui.Deps interface (the
// caller keeps the identity-only umbrella on the nil path), so there is no
// typed-nil-into-interface trap.
//
// The viewer-capable deps EMBEDS the identity-only base so the single value it
// returns also satisfies identityd.WebDeps — webd hosts both surfaces from one
// umbrella.
//
// VerifyLink reuses cookieSigner (same HMAC key): passthroughlink.Verify
// takes the expected issuer/audience as verify-time options, so the
// identityd-minted signer verifies channelsd→webd links by overriding them
// here. ctSigner reuses the same key bytes; webd both mints (trusted side,
// after the SpiceDB view check) and verifies (cookieless sandbox side) the
// content token, so a single shared key suffices.
func buildArtifactViewDeps(base *webdDeps, nc *nats.Conn, operatorURL, webdTokenPath, spicedbEndpoint string, spicedbInsecure bool, spicedbTokenPath string, keyBytes []byte, cookieSigner *passthroughlink.Signer, trustedURLGet, sandboxURLGet func() string, logger logr.Logger, admindURL, admindToken string, startNamespaces []string, accessTokenNamespace string, accessTokenLifetime time.Duration) *artifactViewDeps {
	// SpiceDB connection params come from cfg (endpoint/insecure/token-path,
	// bound to the canonical SPICEDB_* env via clikit). The token VALUE is a
	// secret: read from the token-path file when set (taking precedence, as
	// LoadEnvConfig does), else from SPICEDB_TOKEN. Endpoint and a resolved
	// token are both required; absent either, the viewer is disabled (fail
	// closed), mirroring LoadEnvConfig's missing-var error path.
	if spicedbEndpoint == "" {
		fmt.Fprintln(os.Stderr, "webd: SpiceDB unconfigured; artifact viewer disabled")
		return nil
	}
	spicedbToken := os.Getenv(spicedb.EnvToken)
	if spicedbTokenPath != "" {
		raw, readErr := os.ReadFile(spicedbTokenPath)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "webd: SpiceDB token file %q unreadable (%v); artifact viewer disabled\n", spicedbTokenPath, readErr)
			return nil
		}
		spicedbToken = strings.TrimSpace(string(raw))
	}
	if spicedbToken == "" {
		fmt.Fprintln(os.Stderr, "webd: SpiceDB token unresolved; artifact viewer disabled")
		return nil
	}
	tokenRaw, err := os.ReadFile(webdTokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webd: webd memory token unreadable (%v); artifact viewer disabled\n", err)
		return nil
	}
	token := strings.TrimSpace(string(tokenRaw))
	if token == "" {
		fmt.Fprintln(os.Stderr, "webd: webd memory token file is empty; artifact viewer disabled")
		return nil
	}
	if operatorURL == "" {
		fmt.Fprintln(os.Stderr, "webd: operator memory URL unset; artifact viewer disabled")
		return nil
	}
	spdb, err := spicedb.NewClient(spicedbEndpoint, spicedbToken, spicedbInsecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webd: SpiceDB client init failed (%v); artifact viewer disabled\n", err)
		return nil
	}
	mem := httpclient.New(operatorURL, token)
	artSvc := artifacts.NewService(mem, nil)
	ctSigner := contenttoken.New(keyBytes)
	// The Minter is constructed here, not independently nil-guarded: spdb and
	// base.k8s are both already confirmed real by the fail-closed checks
	// above (this function has already returned nil on any missing
	// prerequisite), so every *artifactViewDeps this function actually
	// returns carries a real Minter.
	minter := &mcpfront.Minter{SpiceDB: spdb, K8s: base.k8s, Namespace: accessTokenNamespace, Lifetime: accessTokenLifetime}

	fmt.Fprintln(os.Stderr, "webd: artifact viewer configured (SpiceDB + memory + content token)")
	return &artifactViewDeps{
		webdDeps:      base,
		nc:            nc,
		spdb:          spdb,
		artSvc:        artSvc,
		mem:           mem, // the SAME *httpclient.Client artSvc wraps, assigned into the memory.Memory interface field
		cookieSigner:  cookieSigner,
		ctSigner:      ctSigner,
		viewMinter:    newArtifactViewMinter(keyBytes, trustedURLGet),
		token:         token,
		operatorURL:   operatorURL,
		trustedURLGet: trustedURLGet,
		sandboxURLGet: sandboxURLGet,
		logger:        logger.WithName("artifactview"),
		admindURL:     admindURL,
		admindToken:   admindToken,
		// Copied on the way IN as well as on the way out (see
		// StartableNamespaces): the caller keeps its own slice, and neither
		// side may reshape the other's.
		startNamespaces:      append([]string(nil), startNamespaces...),
		accessTokenNamespace: accessTokenNamespace,
		minter:               minter,
	}
}

// loadSlackOAuth reads client_id + client_secret + team_id from the mounted
// Secret directory (plain files written by the Kubernetes volume mount).
// client_id and client_secret are required (their read error propagates);
// team_id is optional at load time — the Slack authenticator wiring refuses to
// register without it. Returns the empty string for any field that is absent
// without erroring, except client_id/client_secret whose read error is
// propagated.
func loadSlackOAuth(dir string) (clientID, clientSecret, teamID string, err error) {
	read := func(name string) (string, error) {
		b, readErr := os.ReadFile(dir + "/" + name)
		if readErr != nil {
			return "", readErr
		}
		return string(bytes.TrimSpace(b)), nil
	}
	clientID, err = read("client_id")
	if err != nil {
		return "", "", "", err
	}
	clientSecret, err = read("client_secret")
	if err != nil {
		return "", "", "", err
	}
	if b, readErr := os.ReadFile(dir + "/team_id"); readErr == nil {
		teamID = string(bytes.TrimSpace(b))
	}
	return clientID, clientSecret, teamID, nil
}

// buildAuthenticators builds the channel-kind → WebAuthenticator map with a
// registry-driven loop. The per-kind deps switch loads credentials shaped
// differently per kind; it does NOT dispatch behaviour.
// externalBaseURL is webd's TRUSTED base URL so OIDC redirect URIs land on webd.
//
// The Slack kind is conditional: registered only when client_id + client_secret
// + team_id are all populated (team_id pins OIDC to the bot's workspace; without
// it any cross-workspace user with a matching verified email collapses to the
// same canonical subject). The fake kind is always registered — used by the E2E
// seam via ?auth=fake.
func buildAuthenticators(slackID, slackSecret, slackTeamID, externalBaseURL string) map[string]channelkinds.WebAuthenticator {
	authenticators := map[string]channelkinds.WebAuthenticator{}
	for _, k := range channelregistry.All() {
		name := k.Name()
		var deps channelkinds.WebAuthDeps
		switch name {
		case "slack":
			if slackID == "" || slackSecret == "" {
				fmt.Fprintf(os.Stderr, "webd: slack OAuth not configured; skipping Slack authenticator registration\n")
				continue
			}
			if slackTeamID == "" {
				fmt.Fprintf(os.Stderr, "webd: slack OAuth secret has no team_id; refusing to register Slack OIDC — populate team_id in the spicebox-slack-oauth Secret\n")
				continue
			}
			deps = channelkinds.WebAuthDeps{
				ClientID:        slackID,
				ClientSecret:    slackSecret,
				ExternalBaseURL: externalBaseURL,
				InstalledTeamID: slackTeamID,
			}
		default:
			deps = channelkinds.WebAuthDeps{ExternalBaseURL: externalBaseURL}
		}
		auth := k.WebAuthenticator(deps)
		if auth == nil {
			continue
		}
		authenticators[name] = auth
	}
	return authenticators
}

// buildIconHandler constructs the identityd icon resolver + its HTTP handler.
// safehttp.Client() guards against SSRF by
// rejecting private/loopback destinations; the resolver caches favicons in
// process and falls back to a deterministic SVG initial-badge.
func buildIconHandler(k8s client.Client, maxEntries int, posTTL, negTTL time.Duration) http.Handler {
	safeClient := safehttp.Client()
	safeClient.Timeout = 5 * time.Second
	resolver := &icons.Resolver{
		K8s: k8s,
		Cache: icons.NewCache(icons.CacheConfig{
			Cap:    maxEntries,
			PosTTL: posTTL,
			NegTTL: negTTL,
		}),
		Discoverer: &icons.Discoverer{
			HTTPClient: safeClient,
		},
	}
	return icons.NewHandler(resolver)
}

// fetchRenderBytes GETs the materialized render bytes from the operator's
// artifact endpoint, authenticating with the channelsd system token (the
// same bearer that authorizes memory reads). Returns the body + Content-Type.
// Non-200 responses and transport errors are propagated to the caller (the
// content handler maps them to 502); the body is always closed. Always
// serves raw, inert bytes — used by the live-view content path, which must
// NEVER hand back a ZIP. See fetchBundleBytes for the download path's
// sibling, which hits the smart-passthrough bundle route instead.
func fetchRenderBytes(ctx context.Context, base, token, ns, sess, render string) ([]byte, string, error) {
	u := strings.TrimRight(base, "/") + "/artifact/" + ns + "/" + sess + "/" + render + "/output"
	return fetchOperatorArtifactBytes(ctx, u, token)
}

// fetchBundleBytes GETs the operator's smart-passthrough bundle route
// (pkg/memory/httpsrv's serveBundle) for the direct-download path only. The
// response is the primary's raw bytes unchanged UNLESS it is html with at
// least one resolvable `artifact:HANDLE` reference, in which case it is a
// self-contained ZIP instead (Content-Type: application/zip) — the caller
// (artifactViewDeps.FetchRenderBundle, downloadHandler) doesn't need to know
// which case it got; it just sets headers from what comes back.
func fetchBundleBytes(ctx context.Context, base, token, ns, sess, render string) ([]byte, string, error) {
	u := strings.TrimRight(base, "/") + "/artifact-bundle/" + ns + "/" + sess + "/" + render + "/bundle"
	return fetchOperatorArtifactBytes(ctx, u, token)
}

// fetchOperatorArtifactBytes is the shared HTTP mechanics behind
// fetchRenderBytes and fetchBundleBytes: GET u bearing token, cap the read
// at 8 MiB (comfortably covers any plausible html artifact or its bundled
// ZIP), and return the body + Content-Type. Non-200 responses and transport
// errors are propagated to the caller; the body is always closed.
func fetchOperatorArtifactBytes(ctx context.Context, u, token string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", fmt.Errorf("webd: build artifact request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("webd: fetch artifact render: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("webd: artifact endpoint status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	// Cap the read: the operator is trusted, but an unbounded ReadAll of a
	// multi-MB render on every iframe load is needless memory pressure. 8 MiB
	// comfortably covers any plausible HTML artifact.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("webd: read artifact body: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// readHexKey reads the HMAC signing key from path. The file contains the
// key as hex.EncodeToString(32 random bytes); passthroughlink.DecodeHexKey
// trims, decodes, and enforces the shared minimum-length floor.
//
// The floor is load bearing: this key is the root of webd's entire
// authenticated surface — the idd_session cookie signer, the /content
// capability token, and the artifact view minter all derive from it. An absent
// Secret fails closed at ReadFile, but an externally-provisioned one (ESO,
// GitOps) carrying an EMPTY value does not, and an empty key decodes to a
// zero-length, publicly-computable HMAC. The caller treats any error as fatal.
func readHexKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := passthroughlink.DecodeHexKey(raw)
	if err != nil {
		return nil, fmt.Errorf("signing key %q: %w", path, err)
	}
	return key, nil
}

// resolveWebDevURL defaults the Vite dev server URL when --web-dev-url is empty.
func resolveWebDevURL(u string) string {
	if u == "" {
		return "http://localhost:5173"
	}
	return u
}

// isNgrokHost is the shared-origin predicate webd passes to webui.NewServer:
// shared-origin engages ONLY when the live host is an ngrok domain (and the two
// origins resolve to the same host — see webui.ServeHTTP). Production hosts are
// never ngrok, so the predicate keeps shared-origin off in production.
func isNgrokHost(host string) bool { return strings.Contains(host, "ngrok") }

// sharedOriginPredicate builds the sharedOriginOK gate for webui.NewServer.
// Shared origin is DEBUG-ONLY: it collapses the sandbox isolation onto the auth
// origin (the untrusted artifact still renders in a no-scripts inner frame, so
// it can't read the session, but the defense-in-depth of a cookie-less origin is
// lost). It engages only when explicitly allowed AND the host qualifies:
//   - an ngrok tunnel host (the ngrok-debug path), OR
//   - a loopback host, but ONLY when the cluster kind's InstallProfile allows a
//     shared origin (AllowsSharedOrigin) — the desktop bundle serves both
//     origins from one loopback port, so without this its artifact viewer's
//     sandbox content (/content, /artifact-host) 404s.
//
// A real (non-loopback, non-ngrok) host never shares, in any mode. Returns nil
// when shared origin is not allowed at all ("never shared"). allowsSharedOrigin
// MUST come from InstallProfile().AllowsSharedOrigin(), not ServesLocalWebChat()
// — the two answer different questions and only this one gates the artifact
// viewer's cross-origin isolation.
func sharedOriginPredicate(allowShared, allowsSharedOrigin bool) func(string) bool {
	if !allowShared {
		return nil
	}
	return func(host string) bool {
		return isNgrokHost(host) || (allowsSharedOrigin && webui.IsLoopbackHost(host))
	}
}
