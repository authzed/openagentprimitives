package installcmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagemode"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	idpregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// wizardMode controls how (or whether) the settings wizard runs after a
// successful oap init.
type wizardMode int

const (
	// wizardSkip: no wizard; used when stdin is not a TTY and no flags given.
	wizardSkip wizardMode = iota
	// wizardOffer: print a Y/n prompt on a TTY; wizard runs only on "y".
	wizardOffer
	// wizardInteractive: run the full interactive huh form (--wizard flag).
	wizardInteractive
	// wizardDefaults: apply the recommended secure baseline non-interactively (--defaults flag).
	wizardDefaults
)

// decideWizard is a pure decision helper so the branch logic is unit-testable.
// Priority: --defaults wins; then --wizard; then TTY → offer; else skip.
func decideWizard(defaults, wizard, isTTY bool) wizardMode {
	switch {
	case defaults:
		return wizardDefaults
	case wizard:
		return wizardInteractive
	case isTTY:
		return wizardOffer
	default:
		return wizardSkip
	}
}

func NewInitCmd(g *apcmd.Globals) *cobra.Command {
	skipBuild := false
	skipInstall := false
	// A real cloud install (cert-manager install + waits, image pulls, a cloud
	// load balancer that takes minutes) does not fit in 120s; the old default
	// expired mid-install and failed later applies with "context deadline
	// exceeded". The install + readiness check each get this budget.
	timeout := 10 * time.Minute
	tags := manifests.Tags{}
	localMode := false
	var clusterKindFlag string
	allowNonLocal := false
	var pinningMode string
	var wizardFlag bool
	var defaultsFlag bool
	var imagePullSecret string
	var mirrorDeps bool
	var createRegistry bool
	var trustedHostname string
	var sandboxHostname string
	var hostnameSuffix string
	var disableViewer bool
	var manualWebdRouting bool
	var acmeEmail string
	var gatewayClass string
	var tlsIssuer string
	var assumeYes bool
	var noIdp bool
	var noMonitoring bool
	var noDigestPin bool
	var artifactStoreURL string
	var externalSpiceDBEndpoint string
	var externalSpiceDBToken string
	var externalSpiceDBInsecure bool
	var acceptExisting bool
	// dm is populated below via settingscmd.RegisterDefaultModelFlags(cmd), after cmd
	// exists. The RunE closure captures the variable (not its zero value at
	// closure-literal time) — by the time cobra invokes RunE, dm has already
	// been assigned.
	var dm *settingscmd.DefaultModelOpts

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Bring up agent-primitives end-to-end (build + install + check)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateInitFlags(localMode, clusterKindFlag, developMode, externalSpiceDBEndpoint, externalSpiceDBToken, acceptExisting, skipInstall); err != nil {
				return err
			}
			// --local is sugar for --cluster-kind=local; validateInitFlags has
			// already refused a conflicting explicit --cluster-kind alongside it.
			clusterKind := clusterKindFlag
			if localMode {
				clusterKind = cloud.KeyLocal
			}
			r := WebdRoutingOpts{
				trustedHostname:   trustedHostname,
				sandboxHostname:   sandboxHostname,
				hostnameSuffix:    hostnameSuffix,
				disableViewer:     disableViewer,
				manualWebdRouting: manualWebdRouting,
				acmeEmail:         acmeEmail,
				gatewayClass:      gatewayClass,
				tlsIssuer:         tlsIssuer,
				assumeYes:         assumeYes,
				noDigestPin:       noDigestPin,
			}
			// The tunnel is no longer something this command does. `oap install`
			// creates a PublicEndpoint on a kind whose policy says it should
			// (cloud.PublicEndpointPolicy.CreatedAtInstall), and the operator's
			// PublicEndpoint reconciler owns the tunnel and webd's external-URL
			// ConfigMap from there. `oap init` therefore returns when install +
			// check are done, on every kind, instead of blocking on a
			// foreground tunnel whose death left that ConfigMap advertising a
			// URL that forwarded nothing.
			return runInit(cmd.Context(), cmd.OutOrStdout(), g, skipBuild, skipInstall, timeout, tags, pinningMode, clusterKind, allowNonLocal, r, wizardFlag, defaultsFlag, imagePullSecret, mirrorDeps, createRegistry, noIdp, noMonitoring, *dm, artifactStoreURL, externalSpiceDBEndpoint, externalSpiceDBToken, externalSpiceDBInsecure, acceptExisting)
		},
	}
	cmd.Flags().BoolVar(&skipBuild, "skip-build", false, "Skip oap build all")
	cmd.Flags().BoolVar(&skipInstall, "skip-install", false, "Skip oap install")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Overall deadline for the install + check loop")
	cmd.Flags().StringVar(&tags.Operator, "operator-image", "", "Override operator image")
	cmd.Flags().StringVar(&tags.Runner, "runner-image", "", "Override runner image")
	cmd.Flags().StringVar(&tags.Registry, "image-registry", "", "Rewrite first-party images to <registry>/<name>:<tag> and cross-build+push for the cluster arch")
	cmd.Flags().BoolVar(&localMode, "local", false,
		"Sugar for --cluster-kind=local: the lightweight developer install (sqlite memory, in-memory "+
			"SpiceDB, local :dev images), which also creates the PublicEndpoint that exposes webd "+
			"publicly through a reconciled tunnel. Refuses against non-local clusters.")
	cmd.Flags().BoolVar(&allowNonLocal, "allow-non-local-cluster", false,
		"Override the cluster-safety check that gates --local. Only for unusual but legitimate "+
			"setups (e.g. a corporate dev cluster on a private VPN).")
	cmd.Flags().StringVar(&clusterKindFlag, "cluster-kind", "", clusterKindFlagUsage)
	// init runs the same install flow (RunInstall), which applies the package-
	// level developMode. Bind --develop here too so `oap init --develop` (and
	// `oap init --local --develop`) enable webd dev mode, mirroring `oap install`.
	cmd.Flags().BoolVar(&developMode, "develop", false,
		"DEV ONLY: run webd in dev mode (--web-dev) so the UI loads from a local Vite dev server; run `mage web:dev` alongside")
	cmd.Flags().StringVar(&pinningMode, "pinning-mode", "", pinningModeUsage)
	cmd.Flags().BoolVar(&wizardFlag, "wizard", false, "Force interactive security-settings wizard after install")
	cmd.Flags().BoolVar(&defaultsFlag, "defaults", false, "Apply the recommended secure baseline non-interactively after install")
	cmd.Flags().StringVar(&imagePullSecret, "image-pull-secret", "", "Reference an existing pull Secret on first-party component + runner/detector pods (private registries)")
	cmd.Flags().BoolVar(&mirrorDeps, "mirror-dependencies", false, "Mirror public dependency images under --image-registry and rewrite refs (air-gapped clusters)")
	cmd.Flags().BoolVar(&createRegistry, "create-registry", false, "Create the registry repository if missing without prompting (GKE Artifact Registry)")
	cmd.Flags().StringVar(&trustedHostname, "trusted-hostname", "",
		"Bare hostname for webd's trusted origin (e.g. webd.example.com; no scheme/port/path). "+
			"Sets up external access: Gateway + cert + the trusted-url ConfigMap. Gates the whole flow.")
	cmd.Flags().StringVar(&artifactStoreURL, "artifact-store-url", "",
		"Durable artifact store URL for the operator (gs://<bucket> | s3://<bucket> | azblob://<container> | file:///<path>). Skips cloud auto-provisioning.")
	cmd.Flags().StringVar(&sandboxHostname, "sandbox-hostname", "",
		"Bare hostname for webd's artifact-viewer sandbox origin (must differ from --trusted-hostname). "+
			"Required with --trusted-hostname unless --disable-artifact-viewer.")
	cmd.Flags().StringVar(&hostnameSuffix, "hostname-suffix", "",
		"Convenience for external access on a domain you control: derives --trusted-hostname=webd.<suffix> "+
			"and --sandbox-hostname=sandbox.<suffix> (sandbox omitted with --disable-artifact-viewer). "+
			"Mutually exclusive with --trusted-hostname/--sandbox-hostname/--manual-webd-routing.")
	cmd.Flags().BoolVar(&disableViewer, "disable-artifact-viewer", false,
		"Install webd with only the trusted origin and no artifact viewer (sandbox-url empty). "+
			"The conscious alternative to --sandbox-hostname.")
	cmd.Flags().BoolVar(&manualWebdRouting, "manual-webd-routing", false,
		"Do not set up webd external access (no Gateway/issuer). Use when you bring your own routing "+
			"or run internal-only / Slack-only. Required to install on a non-local cluster without --trusted-hostname.")
	cmd.Flags().StringVar(&acmeEmail, "acme-email", "",
		"ACME account email for the Let's Encrypt ClusterIssuer (required with --trusted-hostname unless --tls-issuer).")
	cmd.Flags().StringVar(&gatewayClass, "gateway-class", "",
		"GatewayClass to use (default: auto-resolve — GKE built-in, else the installed Envoy Gateway class).")
	cmd.Flags().StringVar(&tlsIssuer, "tls-issuer", "",
		"Use an existing cert-manager ClusterIssuer instead of creating a Let's Encrypt one (skips --acme-email).")
	cmd.Flags().BoolVarP(&assumeYes, "assume-yes", "y", false,
		"Accept offered component installs (cert-manager, Gateway controller) non-interactively.")
	cmd.Flags().BoolVar(&noIdp, "no-idp", false,
		"Skip the identity-provider setup prompt after install.")
	cmd.Flags().BoolVar(&noMonitoring, "no-monitoring", false,
		"Skip the monitoring channel setup prompt after install.")
	cmd.Flags().BoolVar(&noDigestPin, "no-digest-pin", false, "Install remote first-party images by mutable :dev tag instead of pinning their digest (debugging; not recommended). Each image must still be present in the registry — this skips pinning, not the existence check")
	cmd.Flags().StringVar(&externalSpiceDBEndpoint, "external-spicedb-endpoint", "",
		"Use an existing SpiceDB instance instead of installing the bundled operator-managed one (gRPC host:port). "+
			"Omit on a TTY to be prompted instead. Setting this skips the operator/CR/database install for SpiceDB.")
	cmd.Flags().StringVar(&externalSpiceDBToken, "external-spicedb-token", "",
		"Bearer token for --external-spicedb-endpoint. Requires --external-spicedb-endpoint. "+
			"A value passed here is recorded by your shell's history; omitting BOTH external-spicedb flags on a TTY "+
			"asks for the endpoint and token interactively instead, and reads the token without echoing it.")
	cmd.Flags().BoolVar(&externalSpiceDBInsecure, "external-spicedb-insecure", false,
		"Connect to --external-spicedb-endpoint without TLS (default: TLS on).")
	cmd.Flags().BoolVar(&acceptExisting, "accept-existing", false,
		"Detect and keep every current cluster setting, skipping the interactive config steps (re-runs / CI). "+
			"Requires an existing install; fails if none is detected.")
	dm = settingscmd.RegisterDefaultModelFlags(cmd)
	return cmd
}

// validateInitFlags rejects flag combinations that can't work. --develop turns
// on webd's --web-dev, which makes the UI reference http://localhost:5173 Vite
// scripts; over the HTTPS ngrok tunnel a local install opens, the browser
// blocks those as mixed content, so the UI never hydrates. Fail closed with an
// actionable message rather than silently shipping a broken tunnel.
//
// --local is sugar for --cluster-kind=local (applied by the caller after this
// validates), so the mixed-content guard above must trigger on EITHER
// spelling of "this is a local install" — isLocal below, not the raw
// localMode bool alone. --cluster-kind=local --develop (no literal --local)
// reaches the exact same DevProfile + ngrok tunnel + --web-dev combination and
// must be refused identically; checking localMode alone let it slip through
// (a real bug this comment exists to keep from regressing).
//
// --local together with a conflicting explicit --cluster-kind (e.g.
// --local --cluster-kind=gke) is almost certainly a mistake — there is no
// sensible "which one wins" answer — so it is rejected rather than silently
// letting one override the other. This check stays keyed on the literal
// localMode flag (not isLocal): it is specifically about the two spellings
// disagreeing, which only literal --local can do.
//
// --external-spicedb-token only makes sense alongside --external-spicedb-endpoint
// (it authenticates to that endpoint); a token with no endpoint is very likely
// a typo'd flag pair, so it is rejected rather than silently discarded.
//
// --accept-existing together with --skip-install is refused for the same
// silently-does-nothing reason: --accept-existing's whole job is detecting +
// seeding an existing install's settings, which only the unified wizard
// dispatch (runInit's `!skipInstall && (wizardFlag || acceptExisting)` gate)
// performs — with --skip-install set, that gate never fires, acceptExisting is
// never read again anywhere in the linear tail, and the flag's own documented
// contract ("fails if none is detected") quietly does not apply either. A
// user relying on that promise would get neither the detection nor the
// failure — just a silent no-op — so it is rejected outright instead.
func validateInitFlags(localMode bool, clusterKind string, developMode bool, externalSpiceDBEndpoint, externalSpiceDBToken string, acceptExisting, skipInstall bool) error {
	isLocal := localMode || clusterKind == cloud.KeyLocal
	if isLocal && developMode {
		return fmt.Errorf("--develop cannot be combined with --local (or --cluster-kind=local): --web-dev points the UI at " +
			"http://localhost:5173, which the browser blocks as mixed content over the HTTPS tunnel. Use --develop only when " +
			"you reach webd over HTTP on localhost (e.g. a direct `kubectl port-forward` to spicebox-webd:8080 + " +
			"http://localhost:8080), or run `oap init --local` (embedded build, served same-origin) for ngrok testing")
	}
	if localMode && clusterKind != "" && clusterKind != cloud.KeyLocal {
		return fmt.Errorf("--local sets --cluster-kind=%s; --cluster-kind=%s conflicts with it — pass one or the other",
			cloud.KeyLocal, clusterKind)
	}
	if externalSpiceDBToken != "" && externalSpiceDBEndpoint == "" {
		return fmt.Errorf("--external-spicedb-token requires --external-spicedb-endpoint")
	}
	if acceptExisting && skipInstall {
		return fmt.Errorf("--accept-existing requires an install to detect and seed settings from; it cannot be combined with --skip-install, which would silently do nothing")
	}
	return nil
}

// clusterKind (oap init --cluster-kind, with --local as sugar for
// cloud.KeyLocal) selects the resolved cloud.Strategy. Its DevProfile is what
// injects --allow-shared-origin into the webd Deployment via RunInstall,
// since only the local tunnel flow points both webd origins at one ngrok
// host — a non-local install never sets it. The cluster kind is resolved
// exactly ONCE below (resolveClusterKind, given clusterKind as-is — "" for
// auto-detect) and threaded to both the preflight checks and the install
// call — never re-detected independently — so a --cluster-kind=local run
// against a managed cloud is refused by local.Validate BEFORE any
// build/install work runs, rather than resolving a DevProfile override on top
// of a mismatched Strategy and only discovering the mismatch after the whole
// build+install has completed. allowNonLocal threads --allow-non-local-cluster
// into that same resolution (local's kubeconfig-host heuristic only; the managed-
// providerID refusal is never relaxable — see pkg/platform/cloud/local/validate.go).
//
// wizardFlag routes the whole run through the unified init wizard
// (runInitWizardFn) — one rail across detection, build, install, status, and the
// post-install offers — as does acceptExisting, which needs the wizard for
// detection + seeding. Either dispatches, but only when install is not skipped.
// defaultsFlag stays on the linear path: it is the non-interactive baseline, and
// after a linear install it drives the post-install settings wizard offer below
// (decideWizard). When no flag is set and stdin is a TTY, that post-install offer
// is a Y/n prompt. dm carries the optional --default-model* flow (see
// registerDefaultModelFlags in settings_wizard.go); a zero-value dm is a no-op.
//
// externalSpiceDBEndpoint/-Token/-Insecure carry the raw --external-spicedb-*
// flags; when externalSpiceDBEndpoint is empty and stdin is a TTY, resolveExternalSpiceDB
// prompts for it instead (see there). A non-empty result skips the bundled
// SpiceDB install (RunInstall) and, after install completes, best-effort
// applies the base schema to the caller's endpoint (applySchemaToEndpoint).
func runInit(ctx context.Context, out io.Writer, g *apcmd.Globals, skipBuild, skipInstall bool, timeout time.Duration, tags manifests.Tags, pinningMode string, clusterKind string, allowNonLocal bool, r WebdRoutingOpts, wizardFlag, defaultsFlag bool, imagePullSecret string, mirrorDeps, createRegistry bool, noIdp bool, noMonitoring bool, dm settingscmd.DefaultModelOpts, artifactStoreURL string, externalSpiceDBEndpoint, externalSpiceDBToken string, externalSpiceDBInsecure bool, acceptExisting bool) error {
	if err := validPinningMode(pinningMode); err != nil {
		return err
	}
	// Expand --hostname-suffix into webd.<suffix>/sandbox.<suffix> up front so the
	// derived opts flow to both the pre-build validation below and RunInstall.
	r, herr := r.withHostnameSuffix()
	if herr != nil {
		return herr
	}

	// Build the kube bundle once (needed for image-mode/arch detection, the
	// token Secret, and the check loop).
	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// wizardPath is true when this run dispatches to the unified init wizard
	// (--wizard, or --accept-existing which needs it for detection + seeding) and
	// there is an install to run. Computed once here so the pre-dispatch routing
	// preflight below and the dispatch further down cannot disagree on the
	// predicate.
	wizardPath := !skipInstall && (wizardFlag || acceptExisting)

	// Decide local :dev vs registry, and the build platform, before building.
	// Skip image-mode resolution when there is no work that needs images
	// (--skip-build --skip-install). Resolving on a remote cluster without
	// --image-registry would error, but that error is irrelevant when neither
	// build nor install will run.
	registry := ""
	platform := "linux/amd64"
	// strat is resolved ONCE below (when there is build/install work to do)
	// and reused by the install-time block further down — never re-detected
	// independently. Left as a nil interface (never dereferenced) when both
	// --skip-build and --skip-install are set, matching that block's own
	// "nothing needs the cluster kind either" scope.
	var strat cloud.Strategy
	if !(skipBuild && skipInstall) {
		kctx, kctxErr := g.CurrentContext()
		if kctxErr != nil {
			cliout.Warn(out, "could not resolve kube-context (assuming non-managed): %v", kctxErr)
		}
		// An explicit --cluster-kind=local (or --local, which sets it) makes
		// local.Validate's refusal (a managed-cloud cluster under a local
		// install) fire HERE, before any build/install work runs, rather than
		// resolving a DevProfile override on top of whatever cloud.Detect found
		// and installing sqlite and an ephemeral SpiceDB datastore onto durable
		// infrastructure before anything notices.
		var stratErr error
		strat, stratErr = resolveClusterKind(ctx, clusterKind, b.Typed, b.REST, kctx, allowNonLocal)
		if stratErr != nil {
			return stratErr
		}
		// The flag-only routing preflight is correct for the LINEAR path (its
		// RunInstall never re-derives hostnames from the cluster), but premature
		// for the wizard path: the wizard DETECTS the cluster's installed HTTPRoute
		// hostnames (seedStateFromDetected) and gathers routing interactively, and
		// RunInstall's own validateWebdRouting (install.go) re-checks the resolved
		// values before applying. Running it here on the raw FLAG values would
		// refuse `oap init --accept-existing`/`--wizard` on a managed cloud
		// (RequiresExternalHostname) whenever no --trusted-hostname was re-passed —
		// exactly the flag's advertised re-run/CI case, on the cluster kinds that
		// actually have detectable routing. So skip it for the wizard path; the
		// wizard runs the same validation post-gather.
		if !wizardPath {
			if verr := validateWebdRouting(strat.InstallProfile(), r.trustedHostname, r.sandboxHostname, r.disableViewer, r.manualWebdRouting); verr != nil {
				return verr
			}
		}
		useLocal := false
		needsRemote := false
		var merr error
		registry, useLocal, needsRemote, merr = imagemode.Resolve(tags.Registry, strat.InstallProfile(), kctx)
		if merr != nil {
			return merr
		}
		if needsRemote {
			registry, merr = imagemode.ResolveRemoteFromCluster(ctx, b.Typed, strat, kctx, out, os.Stdin)
			if merr != nil {
				return merr
			}
			useLocal = false
		}
		if !useLocal {
			tags.Registry = registry
			if arch := imagemode.DetectNodeArch(ctx, b.Typed); arch != "" {
				platform = "linux/" + arch
			}
			cliout.Step(out, "image registry: %s (platform %s)", registry, strings.TrimPrefix(platform, "linux/"))
		}
	}

	// The unified init wizard owns the whole interactive path — config detection
	// and seeding, one step rail across build/install/status, and the
	// post-install offers — so it dispatches HERE, after strat/registry/platform
	// are resolved (it consumes them) and before the linear build/install/status
	// tail below. --wizard selects it explicitly; --accept-existing needs it for
	// detection + seeding, so either flag routes to it. Only when there is an
	// install to run, though: --skip-install keeps the linear path (nothing to
	// configure against), matching the wizard offer's own !skipInstall gate. Note
	// --defaults deliberately does NOT route here — it is the non-interactive
	// baseline and stays linear, exactly as decideWizard maps it.
	if wizardPath {
		return runInitWizardFn(ctx, out, g, initWizardParams{
			strat:                   strat,
			registry:                registry,
			platform:                platform,
			tags:                    tags,
			r:                       r,
			pinningMode:             pinningMode,
			artifactStoreURL:        artifactStoreURL,
			imagePullSecret:         imagePullSecret,
			mirrorDeps:              mirrorDeps,
			createRegistry:          createRegistry,
			timeout:                 timeout,
			skipBuild:               skipBuild,
			acceptExisting:          acceptExisting,
			defaults:                defaultsFlag,
			noIdp:                   noIdp,
			noMonitoring:            noMonitoring,
			dm:                      dm,
			externalSpiceDBEndpoint: externalSpiceDBEndpoint,
			externalSpiceDBToken:    externalSpiceDBToken,
			externalSpiceDBInsecure: externalSpiceDBInsecure,
		})
	}

	if !skipBuild {
		cliout.Step(out, "oap build all")
		builtDigests, err := runBuildFn(ctx, out, "all", "", false, registry, platform, mirrorDeps, createRegistry, false /*raw*/, g, nil /*rail: the linear oap init draws no wizard rail*/)
		if err != nil {
			return err
		}
		// --no-digest-pin keeps remote images on their mutable tag: don't carry
		// the build's digests into install (install then pins nothing and
		// Substitute uses the registry :dev tag). Any other remote install
		// carries the digests forward regardless of cluster kind — see the
		// matching gate in cmd/oap/internal/installcmd/install.go.
		if !r.noDigestPin {
			tags.Digests = builtDigests
		}
	}

	// ext is resolved inside the !skipInstall block below and consumed by the
	// shared post-install schema tail after it. Declared here (not with := inside
	// the block) so the tail can run on the zero value when install was skipped —
	// matching the pre-refactor behavior, where the external-endpoint schema apply
	// was reachable only after an install but the SPICEDB_ENDPOINT env apply ran
	// regardless.
	var ext ExternalSpiceDB
	if !skipInstall {
		cliout.Step(out, "oap install")
		// Resolve the external-SpiceDB decision before the sigCtx/timeout setup
		// below: an explicit --external-spicedb-endpoint answers it outright, a
		// TTY with no flag prompts for it, and a non-TTY with no flag disables it
		// (falls back to the bundled operator-managed SpiceDB).
		ext = resolveExternalSpiceDB(os.Stdin, out, externalSpiceDBEndpoint, externalSpiceDBToken, externalSpiceDBInsecure, isInteractive())
		// sigCtx is canceled by SIGINT and is NOT subject to the install timeout.
		// It becomes the recheckCtx for RunInstall so that an exhausted overall
		// timeout does not make the keep-waiting "Y" a no-op.
		sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		installCtx, cancel := context.WithTimeout(sigCtx, timeout)
		defer cancel()
		// RunInstall no longer detects the cloud itself. strat was already
		// resolved (and validated) exactly once, in the preflight block above —
		// reused here rather than re-detected, so there is a single
		// resolveClusterKind round-trip per `oap init` invocation, not two
		// independent ones that could disagree.
		// `oap init` has no --builder-starters equivalent and no notion of "the
		// installer's own identity" to default one to, so it skips the builder
		// rather than guessing an identity that would silently lock the class to
		// nobody real. A user who wants the workshop on an init'd cluster runs
		// `oap install --builder-starters=...` afterward (Install converges).
		if err := runInstallFn(installCtx, sigCtx, InstallConfig{
			Out:                 out,
			G:                   g,
			Tags:                tags,
			DryRun:              "",
			Routing:             r,
			PinningMode:         pinningMode,
			Strat:               strat,
			Ext:                 ext,
			Workspace:           WorkspaceResolveOptions{},
			Stateful:            StatefulResolveOptions{},
			Artifact:            ArtifactStoreOptions{ExplicitURL: artifactStoreURL},
			ImagePullSecret:     imagePullSecret,
			MirrorDeps:          mirrorDeps,
			ImagesPrebaked:      false,
			ClampUndersizedPVCs: false,
			WithoutBuilder:      true,
			BuilderStarters:     nil,
			Rail:                nil, // today's linear `oap init` draws no wizard rail
		}); err != nil {
			return err
		}
	} else if pinningMode != "" {
		fmt.Fprintf(out, "note: --pinning-mode=%q was not applied (--skip-install skips the install step where pinning is seeded)\n", pinningMode)
	}

	// Post-install SpiceDB schema writes — the external-endpoint apply (the ONLY
	// schema-write path for an external instance) and the dev-only SPICEDB_ENDPOINT
	// env apply. Extracted into one helper so the unified init wizard runs the
	// identical tail (it used to skip both, coming up schema-less with green
	// checks). ext is the zero value when --skip-install was set, so the
	// external-endpoint apply no-ops there while the env apply still runs — exactly
	// the pre-refactor behavior.
	applyExternalSpiceDBSchemaTail(ctx, out, ext)

	cliout.Step(out, "oap check (watch)")
	if err := checksFn(ctx, out, b, timeout, nil /*rail: the linear oap init draws no wizard rail*/); err != nil {
		return err
	}

	// Offer the security-defaults wizard after a successful install. Skip when
	// install was skipped (nothing meaningful to configure against).
	if !skipInstall {
		isTTY := term.IsTerminal(int(os.Stdin.Fd()))
		mode := decideWizard(defaultsFlag, wizardFlag, isTTY)
		if err := runInitWizardOffer(ctx, out, g, mode, registry, tags.Digests, dm); err != nil {
			// A wizard failure does NOT fail oap init — install already succeeded.
			// Surface the error so the user knows, but return nil.
			cliout.Warn(out, "settings wizard failed: %v", err)
		}

		// Offer directory-sync source configuration too, driven off the SAME
		// mode decideWizard already produced above (see runInitDirectoryScreenOffer's
		// doc comment for why wizardDefaults maps to skip here, unlike the settings
		// wizard). A configure failure does NOT fail oap init either.
		if err := runInitDirectoryScreenOffer(ctx, os.Stdin, out, g, mode); err != nil {
			cliout.Warn(out, "directory-sync setup: %v", err)
		}
	}

	// installProfile feeds shouldPromptIdp/shouldPromptMonitoring below. strat
	// is a genuine nil interface (never a typed-nil pointer — it is declared as
	// the interface type itself) only when both --skip-build and --skip-install
	// were set, in which case shouldPromptIdp/Monitoring's own !skipInstall gate
	// already answers false regardless of the profile — so ProductionProfile
	// here is a safe placeholder, never a real decision, and avoids calling
	// InstallProfile() on a nil strat.
	installProfile := cloud.ProductionProfile
	if strat != nil {
		installProfile = strat.InstallProfile()
	}

	// Offer IdP setup after a successful non-local install on a TTY, unless
	// --no-idp was passed. A setup failure does NOT fail oap init.
	if shouldPromptIdp(installProfile, noIdp, skipInstall, term.IsTerminal(int(os.Stdin.Fd()))) {
		if err := runInitIdpOffer(ctx, os.Stdin, out, g, b.Controller); err != nil {
			cliout.Warn(out, "identity provider setup: %v", err)
		}
	}

	// Offer monitoring channel setup after a successful non-local install on
	// a TTY, unless --no-monitoring was passed. A setup failure does NOT fail
	// oap init.
	if shouldPromptMonitoring(installProfile, noMonitoring, skipInstall, term.IsTerminal(int(os.Stdin.Fd()))) {
		if err := runInitMonitoringOffer(ctx, os.Stdin, out, g, b.Controller, b.Namespace); err != nil {
			cliout.Warn(out, "monitoring channel setup: %v", err)
		}
	}
	return nil
}

// waitUntilChecksPass runs `oap check` until it passes or the timeout elapses,
// discarding per-attempt output so only the final pass line is printed (a fail
// surfaces through the returned error / exit code). Extracted so the linear
// `oap init` path and the unified init wizard share ONE wait loop rather than
// each hand-rolling a poll + runCheck.
//
// rail == nil (every non-wizard caller — the linear `oap init` tail) keeps the
// original fixed-2s wait.Until loop, byte-for-byte, with its final line printed
// straight to out: there is no live region for a direct write to corrupt.
//
// rail != nil (the unified init wizard) instead drives the wait through a
// rail-aware Phase, so the Status slot shows the same gutter Build/Install
// already draw instead of the gutter vanishing for this phase alone. It uses
// AwaitOptional, which — like the loop above — NEVER prompts to keep waiting:
// it returns the timeout error once the deadline elapses. The polling cadence
// this takes on (a doubling backoff, 2s up to 20s) differs from the fixed 2s
// above; that's an accepted, wizard-only behavior change in exchange for a live
// spinner + rail, not a regression in the non-wizard path this branch never
// touches. Done()/Fail() replace the raw fmt.Fprintln — a direct write to out
// while this phase's live region is up would corrupt it exactly as Finding 1
// describes.
func waitUntilChecksPass(ctx context.Context, out io.Writer, b *kube.Bundle, timeout time.Duration, rail progress.RailProvider) error {
	if rail == nil {
		return wait.Until(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
			// Suppress per-attempt output to avoid spam; print only on the final
			// pass or final fail (caller sees exit code on fail).
			sink := io.Discard
			if err := runCheck(ctx, sink, b, false); err == nil {
				fmt.Fprintln(out, "all checks passed")
				return true, nil
			}
			return false, nil
		})
	}

	rep := progress.NewWithRail(out, os.Stdin, true /*assumeYes: AwaitOptional never prompts to keep waiting*/, rail)
	defer func() { _ = rep.Close() }()
	phase := rep.Phase("checks")
	poll := func(ctx context.Context) (bool, error) {
		if err := runCheck(ctx, io.Discard, b, false); err == nil {
			return true, nil
		}
		return false, nil
	}
	if err := phase.AwaitOptional(ctx, ctx, timeout, 0 /*eta: no soft-warn*/, poll, nil /*diagnose*/); err != nil {
		phase.Fail()
		return err
	}
	phase.DoneWith("all checks passed")
	return nil
}

// applySchemaToEndpointFn is a package-var seam (mirroring runInstallFn/checksFn
// in wizard_run.go) so a same-package test can prove the external-SpiceDB schema
// tail runs on the wizard path without dialing a real SpiceDB endpoint.
// Production points it at the real applySchemaToEndpoint.
var applySchemaToEndpointFn = applySchemaToEndpoint

// applyExternalSpiceDBSchemaTail runs the post-install SpiceDB schema writes both
// `oap init` paths share — the linear runInit tail and the unified init wizard.
//
// An external SpiceDB instance (ext.enabled()) has NO startup bootstrap: the
// bundled operator-managed SpiceDB applies its embedded schema at startup, an
// external one does not, so this applySchemaToEndpoint call is the ONLY
// schema-write path for it. Skipping it — as the wizard used to — leaves the
// whole authorization system schema-less while the install reports success and
// the checks go green (a schema-less SpiceDB still serves), and every authz
// check then fails silently, hours later. The SPICEDB_ENDPOINT env block is the
// dev-only apply the linear tail also runs.
//
// Both are best-effort: a failure warns and does NOT fail `oap init` — install
// already succeeded, and a re-run of `oap spicedb apply-schema` (or another
// `oap init`) retries it.
func applyExternalSpiceDBSchemaTail(ctx context.Context, out io.Writer, ext ExternalSpiceDB) {
	if ext.enabled() {
		cliout.Step(out, "apply SpiceDB schema to external endpoint")
		if err := applySchemaToEndpointFn(ctx, out, ext.Endpoint, ext.Token, ext.Insecure); err != nil {
			cliout.Warn(out, "SpiceDB schema apply to external endpoint %s failed: %v — re-run `oap spicedb apply-schema --endpoint=%s` once it's reachable", ext.Endpoint, err, ext.Endpoint)
		}
	}

	if endpoint := os.Getenv("SPICEDB_ENDPOINT"); endpoint != "" {
		cliout.Step(out, "apply SpiceDB schema")
		tokenFile := os.Getenv("SPICEDB_TOKEN_FILE")
		if err := runSpiceDBApplySchema(ctx, out, endpoint, tokenFile, true); err != nil {
			// Best-effort: warn but don't bail.
			cliout.Warn(out, "SpiceDB schema apply to %s failed: %v — re-run `oap init` once SpiceDB is reachable", endpoint, err)
		}
	}
}

// resolveExternalSpiceDB decides whether `oap init` installs the bundled
// operator-managed SpiceDB or points at a caller-supplied external instance.
// A non-empty flagEndpoint answers it outright (flags fully specify the
// external connection — no prompt). Otherwise, on a TTY, the user is offered a
// Y/N prompt (default No — the bundled SpiceDB is the common case) and, on
// yes, is asked for the endpoint and token via the same inline
// bufio.Scanner + strings.TrimSpace pattern the IdP/monitoring offers use
// above. A non-TTY with no flagEndpoint returns the zero value (disabled)
// without touching stdin. isTTY is threaded in explicitly (rather than
// checked internally) so this stays unit-testable against a fake reader.
func resolveExternalSpiceDB(stdin io.Reader, out io.Writer, flagEndpoint, flagToken string, flagInsecure, isTTY bool) ExternalSpiceDB {
	if flagEndpoint != "" {
		return ExternalSpiceDB{Endpoint: flagEndpoint, Token: flagToken, Insecure: flagInsecure}
	}
	if !isTTY {
		return ExternalSpiceDB{}
	}
	if !apcmd.Confirm(stdin, out, "Use an existing external SpiceDB instance instead of installing one?", false, isTTY) {
		return ExternalSpiceDB{}
	}
	return collectExternalSpiceDBEndpoint(stdin, out, isTTY, "")
}

// collectExternalSpiceDBEndpoint gathers the endpoint, bearer token, and
// insecure setting for a caller who has ALREADY decided to use an external
// SpiceDB instance — it never asks the "use external?" yes/no confirm itself.
// That split is what lets the wizard trust its SpiceDB screen's answer: the
// linear resolveExternalSpiceDB owns the confirm (it has no screen), while
// resolveExternalSpiceDBFromState calls THIS directly once the screen (or a
// detected-external seed) confirmed external, so the same question is never
// asked twice — a second confirm, defaulting to No, would silently reverse the
// wizard's Yes on a bare Enter. Requires a terminal; callers gate on isTTY
// before reaching here.
//
// defaultEndpoint, when non-empty, pre-fills a bare Enter with a previously
// detected connection (the wizard's keep-external path, R1) instead of the
// "no endpoint given" bundled fallback — a bare Enter then means "keep the
// endpoint the cluster is already using", not "install bundled". The linear
// path has no detected endpoint to offer and always passes "". The bearer
// token can never be pre-filled (it is a secret nothing here has read) and is
// always re-prompted regardless of defaultEndpoint.
func collectExternalSpiceDBEndpoint(stdin io.Reader, out io.Writer, isTTY bool, defaultEndpoint string) ExternalSpiceDB {
	scanner := bufio.NewScanner(stdin)
	if defaultEndpoint != "" {
		cliout.Prompt(out, "SpiceDB gRPC endpoint (host:port) [%s]: ", defaultEndpoint)
	} else {
		cliout.Prompt(out, "SpiceDB gRPC endpoint (host:port): ")
	}
	if !scanner.Scan() {
		return ExternalSpiceDB{} // EOF — treat as "no change"
	}
	endpoint := strings.TrimSpace(scanner.Text())
	if endpoint == "" {
		if defaultEndpoint != "" {
			endpoint = defaultEndpoint
			fmt.Fprintf(out, "Keeping the detected endpoint: %s\n", endpoint)
		} else {
			fmt.Fprintln(out, "No endpoint given; installing the bundled SpiceDB instead.")
			return ExternalSpiceDB{}
		}
	}

	// The bearer token is the external SpiceDB's pre-shared key — the unscoped
	// root credential for the whole authorization system — so it is read
	// without echo when there is a terminal to suppress echo on, and the user
	// is told when there is not.
	token, echoSuppressed := readSecretLine(stdin, scanner, out, "SpiceDB bearer token: ")
	if !echoSuppressed && token != "" {
		cliout.Warn(out, "the token was read with terminal echo still on, so it is visible on screen and in scrollback — clear it if this terminal is shared or recorded")
	}

	insecure := apcmd.Confirm(stdin, out, "Dial the external SpiceDB without TLS (insecure)?", false, isTTY)

	return ExternalSpiceDB{Endpoint: endpoint, Token: token, Insecure: insecure}
}

// shouldPromptIdp is a pure decision helper for the post-install IdP prompt,
// so the logic is unit-testable without touching stdin or the network. Reads
// from the resolved cloud.InstallProfile rather than a raw local/remote bool
// so a future profile can opt in or out without a branch at this call site.
func shouldPromptIdp(p cloud.InstallProfile, noIdp, skipInstall, isTTY bool) bool {
	return p.PromptsForIdentityProvider() && !noIdp && !skipInstall && isTTY
}

// shouldPromptMonitoring is a pure decision helper for the post-install
// monitoring channel prompt, so the logic is unit-testable without touching
// stdin or the network. Same shape as shouldPromptIdp.
func shouldPromptMonitoring(p cloud.InstallProfile, noMonitoring, skipInstall, isTTY bool) bool {
	return p.PromptsForMonitoring() && !noMonitoring && !skipInstall && isTTY
}

// idpOfferOptions builds the display labels and the 1-based default index for
// the oap init IdP offer. Real kinds come first, "none" last. When currentKind
// matches a kind, it is labeled "(current)" and becomes the default; otherwise
// "none" is the default.
func idpOfferOptions(kinds []string, currentKind string) (labels []string, defaultIdx int) {
	labels = make([]string, 0, len(kinds)+1)
	defaultIdx = len(kinds) + 1 // "none" by default
	for i, k := range kinds {
		if k == currentKind && currentKind != "" {
			labels = append(labels, k+" (current)")
			defaultIdx = i + 1
		} else {
			labels = append(labels, k)
		}
	}
	labels = append(labels, "none — skip for now (run `oap idp setup <kind>` later)")
	return labels, defaultIdx
}

// monitoringOfferOptions builds the display labels and 1-based default index
// for the oap init monitoring offer. When one or more monitoring channels
// already exist, a "keep existing" entry leads the menu and is the default;
// otherwise the menu is set-up / skip and the default is skip (matching the
// historical [y/N] default of No). The skip entry is always last. Mirrors
// idpOfferOptions.
func monitoringOfferOptions(existing []spiceboxv1alpha1.Channel) (labels []string, defaultIdx int) {
	const skip = "none — skip for now (run `oap channel create --kind slack` later)"
	if len(existing) == 0 {
		return []string{"set up a monitoring channel", skip}, 2
	}
	keep := "keep using existing monitoring channel(s)"
	if len(existing) == 1 {
		keep = fmt.Sprintf("keep using existing %q (%s)", existing[0].Name, existing[0].Spec.Kind)
	}
	return []string{keep, "set up / reconfigure a monitoring channel", skip}, 1
}

// runInitIdpOffer shows a menu of registered (non-test) IdP kinds and runs the
// setup wizard for the chosen kind. Choosing "none" (the default) prints a hint
// and returns nil. Errors are returned to the caller, which treats them as
// non-fatal warnings.
func runInitIdpOffer(ctx context.Context, stdin io.Reader, out io.Writer, g *apcmd.Globals, c client.Client) error {
	// Collect real (non-test) registered kinds.
	var kinds []string
	for _, name := range idpregistry.Names() {
		if name == "fake" {
			continue
		}
		kinds = append(kinds, name)
	}
	if len(kinds) == 0 {
		// No real kinds registered — nothing to offer.
		return nil
	}

	// Load the currently-configured kind (if any) to default the selection.
	currentKind := ""
	var curCR spiceboxv1alpha1.ClusterIdentityProvider
	switch err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &curCR); {
	case err == nil:
		currentKind = curCR.Spec.Kind
	case apierrors.IsNotFound(err):
		// none configured yet
	default:
		return fmt.Errorf("check existing identity provider: %w", err)
	}

	options := append(append([]string{}, kinds...), "none")
	labels, defaultIdx := idpOfferOptions(kinds, currentKind)

	cliout.Step(out, "identity provider setup (optional)")
	for i, label := range labels {
		fmt.Fprintf(out, "  %d) %s\n", i+1, label)
	}
	cliout.Prompt(out, "Connect an identity provider? [1-%d, default=%d]: ", len(options), defaultIdx)

	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return nil // EOF — treat as "no change"
	}
	answer := strings.TrimSpace(scanner.Text())

	chosen := options[defaultIdx-1]
	if answer != "" {
		// Try a 1-based numeric index first.
		var idx int
		if n, _ := fmt.Sscan(answer, &idx); n == 1 && idx >= 1 && idx <= len(options) {
			chosen = options[idx-1]
		} else {
			// Try a literal name match (case-insensitive).
			lower := strings.ToLower(answer)
			for _, opt := range options {
				if lower == opt {
					chosen = opt
					break
				}
			}
		}
	}

	if chosen == "none" {
		fmt.Fprintln(out, "Skipping identity provider setup. Run `oap idp setup <kind>` later to connect one.")
		return nil
	}

	return identitycmd.RunIdpSetup(ctx, stdin, out, g, c, chosen)
}

// runInitMonitoringOffer shows the monitoring-channel menu after install.
// When one or more monitoring channels already exist (detected via c across
// all kinds), the menu leads with "keep using existing" as the default and
// keeping is a pure no-op. Otherwise the menu is set-up / skip with skip as
// the default (matching the historical [y/N] default of No). Choosing set up /
// reconfigure runs the kind's wizard via runMonitoringChannelCreate (which
// performs an in-place update when it finds the existing Channel). Errors are
// returned to the caller, which treats them as non-fatal warnings.
func runInitMonitoringOffer(ctx context.Context, stdin io.Reader, out io.Writer, g *apcmd.Globals, c client.Client, namespace string) error {
	cliout.Step(out, "monitoring channel setup (optional)")

	// Detect existing monitoring channels (any kind) before a kind is chosen.
	// A list failure must not break the offer: warn and fall back to the
	// no-existing (set up / skip) menu.
	existing, err := channelcmd.ListMonitoringChannels(ctx, c, namespace)
	if err != nil {
		cliout.Warn(out, "check existing monitoring channels: %v", err)
		existing = nil
	}

	labels, defaultIdx := monitoringOfferOptions(existing)
	for i, label := range labels {
		fmt.Fprintf(out, "  %d) %s\n", i+1, label)
	}
	cliout.Prompt(out, "Monitoring channel? [1-%d, default=%d]: ", len(labels), defaultIdx)

	scanner := bufio.NewScanner(stdin)
	choice := defaultIdx
	if scanner.Scan() {
		if s := strings.TrimSpace(scanner.Text()); s != "" {
			var idx int
			if n, _ := fmt.Sscan(s, &idx); n == 1 && idx >= 1 && idx <= len(labels) {
				choice = idx
			}
			// Unrecognized / out-of-range input keeps the default.
		}
	}

	keepPresent := len(existing) > 0
	switch {
	case keepPresent && choice == 1:
		// Keep — pure no-op. The existing Channel is already applied and
		// reconciling; leave it (and its credentials) untouched.
		if len(existing) == 1 {
			fmt.Fprintf(out, "Keeping existing monitoring channel %q (%s).\n", existing[0].Name, existing[0].Spec.Kind)
		} else {
			fmt.Fprintln(out, "Keeping existing monitoring channel(s).")
		}
		return nil
	case choice == len(labels):
		// Skip is always the last option.
		fmt.Fprintln(out, "Skipping monitoring channel setup. Run `oap channel create --kind slack` later to set one up.")
		return nil
	default:
		// Set up / reconfigure: pick a kind and run its wizard.
		kindName := pickMonitoringKind(scanner, out)
		if kindName == "" {
			return nil
		}
		return channelcmd.RunMonitoringCreate(ctx, stdin, out, g, kindName)
	}
}

// pickMonitoringKind returns the chosen production channel kind, prompting when
// more than one is registered (test/local kinds are skipped). It returns "" and
// prints a note when no production kind is registered. Reads from the shared
// scanner so buffered stdin stays consistent with the wizard that follows.
func pickMonitoringKind(scanner *bufio.Scanner, out io.Writer) string {
	var kinds []string
	for _, name := range channelcmd.KindNames() {
		if channelcmd.IsDemoKind(name) {
			continue
		}
		kinds = append(kinds, name)
	}
	if len(kinds) == 0 {
		fmt.Fprintln(out, "No production channel kinds registered; skipping monitoring channel setup.")
		return ""
	}
	kindName := kinds[0]
	if len(kinds) > 1 {
		for i, n := range kinds {
			fmt.Fprintf(out, "  %d) %s\n", i+1, n)
		}
		cliout.Prompt(out, "Channel kind [1]: ")
		if scanner.Scan() {
			if s := strings.TrimSpace(scanner.Text()); s != "" {
				var idx int
				if n, _ := fmt.Sscan(s, &idx); n == 1 && idx >= 1 && idx <= len(kinds) {
					kindName = kinds[idx-1]
				} else {
					for _, k := range kinds {
						if k == s {
							kindName = k
							break
						}
					}
				}
			}
		}
	}
	return kindName
}

// runInitWizardOffer runs or offers the settings wizard based on mode. It is
// a no-op for wizardSkip. The cobra.Command stub passed to runSettingsWizard
// uses out as its output writer. dm carries the optional --default-model*
// flow (registered on the init command via registerDefaultModelFlags); a
// zero-value dm is a no-op (see settingscmd.DefaultModelOpts.enabled()).
func runInitWizardOffer(ctx context.Context, out io.Writer, g *apcmd.Globals, mode wizardMode, registry string, digests map[string]string, dm settingscmd.DefaultModelOpts) error {
	// Build a minimal cobra.Command so runSettingsWizard has an OutOrStdout()
	// that writes to our out writer.
	fakeCmd := &cobra.Command{}
	fakeCmd.SetOut(out)

	switch mode {
	case wizardSkip:
		return nil

	case wizardDefaults:
		cliout.Step(out, "applying security defaults (non-interactive)")
		return settingscmd.RunWizard(ctx, fakeCmd, g, true /*defaults*/, false /*dryRun*/, registry, digests, dm)

	case wizardInteractive:
		return settingscmd.RunWizard(ctx, fakeCmd, g, false, false, registry, digests, dm)

	case wizardOffer:
		cliout.Prompt(out, "Configure security defaults now? [Y/n] ")
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if answer == "" || answer == "y" || answer == "yes" {
				return settingscmd.RunWizard(ctx, fakeCmd, g, false, false, registry, digests, dm)
			}
		}
		return nil
	}
	return nil
}

// runDirectoryConfigureFn performs the actual `oap directory configure` flow:
// resolve the clients + namespace from g (via directorycmd.ClientFactory, the
// same seam `oap directory configure` itself uses), walk
// directorycmd.RunWizard over in/out, and — when a kind was selected —
// server-side-apply it with directorycmd.Apply.
//
// A package var, mirroring directorycmd's own driverForRun and
// ClientFactory, so a same-package test can substitute a spy that proves
// runInitDirectoryScreenOffer's four wizardMode branches call (or correctly
// don't call) it, without driving the full interactive TUI or standing up a
// real cluster.
var runDirectoryConfigureFn = runDirectoryConfigure

func runDirectoryConfigure(ctx context.Context, in io.Reader, out io.Writer, g *apcmd.Globals) error {
	dyn, ctrl, ns, err := directorycmd.ClientFactory(g)
	if err != nil {
		return fmt.Errorf("resolve cluster client: %w", err)
	}
	pres := directorycmd.NewPresentation(g.Theme(out), in, out)
	sel, err := directorycmd.RunWizard(ctx, directorycmd.NewDeps(dyn, ctrl, ns), directorycmd.WizardOpts{Pres: pres})
	if err != nil {
		return err
	}
	if sel == nil {
		fmt.Fprintln(out, "No kind selected; nothing configured.")
		return nil
	}
	if err := directorycmd.Apply(ctx, dyn, ns, *sel); err != nil {
		return err
	}
	fmt.Fprintf(out, "Configured directory source %q (kind %s) in namespace %q.\n", sel.Name, sel.Kind, ns)
	return nil
}

// runInitDirectoryScreenOffer runs or offers the `oap directory configure`
// flow based on mode, over in/out (a plain io.Reader/io.Writer rather than
// os.Stdin/out directly, so a test can drive it with canned input the same
// way runInitIdpOffer/runInitMonitoringOffer already do). It is a no-op for
// wizardSkip.
//
// wizardDefaults maps to skip too — a DELIBERATE asymmetry with
// runInitWizardOffer, where --defaults applies a recommended secure
// baseline. There is no recommended set of directories to sync: a
// RelationshipSource writes authorization data (an ongoing read grant over
// whatever its credential can see), that credential may not even exist yet,
// and which orgs/repos/vaults to sync is a deployment decision nobody can
// guess on the operator's behalf. A non-interactive install that silently
// started a directory sync against a credential it picked would be exactly
// the failure this mapping exists to prevent.
func runInitDirectoryScreenOffer(ctx context.Context, in io.Reader, out io.Writer, g *apcmd.Globals, mode wizardMode) error {
	switch mode {
	case wizardSkip, wizardDefaults:
		return nil

	case wizardInteractive:
		return runDirectoryConfigureFn(ctx, in, out, g)

	case wizardOffer:
		cliout.Prompt(out, "Configure a directory-sync source now? [Y/n] ")
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if answer == "" || answer == "y" || answer == "yes" {
				return runDirectoryConfigureFn(ctx, in, out, g)
			}
		}
		return nil
	}
	return nil
}

// ensureChannelsdMemoryToken creates the spicebox-channelsd-memory-token Secret.
func ensureChannelsdMemoryToken(ctx context.Context, restCfg *rest.Config) error {
	return ensureMemoryTokenSecret(ctx, restCfg, "spicebox-channelsd-memory-token")
}

// ensureWebdMemoryToken creates the spicebox-webd-memory-token Secret — webd's
// dedicated READ-ONLY memory bearer (mounted into both webd and the operator).
// Keeping it separate from the channelsd token is the whole point: a
// browser-facing service must not hold a credential that can write the audit
// log or register publisher keys.
func ensureWebdMemoryToken(ctx context.Context, restCfg *rest.Config) error {
	return ensureMemoryTokenSecret(ctx, restCfg, "spicebox-webd-memory-token")
}

// ensureAuthzdMemoryToken creates the agentprimitives-authzd-memory-token
// Secret — authzd's memory bearer. It is consumed as a non-optional
// MEMORY_TOKEN secretKeyRef by authzd (so the Deployment blocks in
// CreateContainerConfigError until it exists) and mounted into the operator,
// which registers it as a valid system token via IsAuthzdToken. Mirrors the
// channelsd/webd token bootstrap; the name keeps the deployment's existing
// (non-spicebox-prefixed) reference.
func ensureAuthzdMemoryToken(ctx context.Context, restCfg *rest.Config) error {
	return ensureMemoryTokenSecret(ctx, restCfg, "agentprimitives-authzd-memory-token")
}

// ensureMemoryTokenSecret creates a `token`-keyed random-bearer Secret named
// `name` in agentprimitives-system if it does not already exist. Idempotent: a
// second call is a no-op so repeated oap init does not rotate the token (which
// would invalidate the in-memory auth state of whoever holds it).
func ensureMemoryTokenSecret(ctx context.Context, restCfg *rest.Config, name string) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	var existing corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      name,
	}, &existing); err == nil {
		return nil // already exists; idempotent
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s secret: %w", name, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(hex.EncodeToString(buf))},
	}
	return cli.Create(ctx, sec)
}
