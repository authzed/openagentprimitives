package installcmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aifix"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/gatewayhealth"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagemode"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/initpipeline"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

var developMode bool

// installWaitDeadline is the per-phase initial time budget passed to Phase.Await.
// The progress reporter loops (offering to keep waiting) if the component is still
// not ready when the deadline elapses.
const installWaitDeadline = 3 * time.Minute

// WebdRoutingOpts carries the webd external-access flags into RunInstall.
type WebdRoutingOpts struct {
	trustedHostname   string
	sandboxHostname   string
	hostnameSuffix    string // convenience: derives webd.<suffix> + sandbox.<suffix>
	disableViewer     bool
	manualWebdRouting bool
	acmeEmail         string
	// acmeEmailDetected is NOT a flag value: it is the ACME email read off the
	// cluster's existing Let's Encrypt ClusterIssuer (seededACMEEmail), used as
	// the cert-email prompt's editable Default. Kept distinct from acmeEmail so
	// a detected value pre-fills the question rather than answering it — an
	// operator can bare-Enter to keep it or type over it, and a flagged
	// --acme-email still wins and skips the prompt entirely.
	acmeEmailDetected string
	gatewayClass      string
	tlsIssuer         string
	assumeYes         bool
	noDigestPin       bool // remote installs pin by digest unless this is set
	// preconfirmed marks that the unified init wizard already gathered and
	// confirmed everything RunInstall would otherwise ask for. It suppresses two
	// re-asks on the same TTY: confirmPlan's go/no-go (the wizard asks "Proceed?"
	// once, as its own rail step) and askWebdRoutingInputs' hostname/sandbox/email
	// prompts (the wizard's routing screens already collected them and folded them
	// into this struct). Only the orchestrator sets it — every flag-driven caller
	// leaves it false, so confirmPlan and the routing ASK behave exactly as they
	// did (--yes / non-TTY proceed silently; an interactive user is asked). It
	// changes only WHETHER a value is re-prompted, never the value applied.
	preconfirmed bool
}

// UnattendedRoutingOpts is the routing decision an install with nobody at the
// terminal makes: take every default rather than ask, and skip digest pinning
// because the images are already on the node rather than pulled from a
// registry. `oap desktop` installs this way.
//
// A constructor rather than exported fields: the two answers below only make
// sense together, and every other field of WebdRoutingOpts comes from a flag
// no unattended caller has.
func UnattendedRoutingOpts() WebdRoutingOpts {
	return WebdRoutingOpts{assumeYes: true, noDigestPin: true}
}

// installDefaultTimeout is the default --timeout budget for `oap install`'s
// K8s-side work. Named so the interrupt path can assert it is NOT what a
// teardown inherits — see handleInstallInterrupt.
const installDefaultTimeout = 120 * time.Second

func NewInstallCmd(g *apcmd.Globals) *cobra.Command {
	tags := manifests.Tags{}
	dryRun := ""
	timeout := installDefaultTimeout
	externalToolTimeout := 30 * time.Minute
	var (
		workspaceClass      string
		noWorkspaceRWX      bool
		workspaceRecheck    bool
		statefulClass       string
		trustedHostname     string
		sandboxHostname     string
		hostnameSuffix      string
		disableViewer       bool
		manualWebdRouting   bool
		acmeEmail           string
		gatewayClass        string
		tlsIssuer           string
		assumeYes           bool
		pinningMode         string
		imagePullSecret     string
		mirrorDeps          bool
		noDigestPin         bool
		artifactStoreURL    string
		clampUndersizedPVCs bool
		clusterKind         string
		withoutBuilder      bool
		builderStarters     []string
		allowNonLocal       bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the operator + CRDs into the configured cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			// First, before a signal handler, a timeout, or a cluster
			// connection: a mode this command will refuse must be refused
			// while refusing is still free. RunInstall re-checks, because it
			// is exported and `oap init` reaches it without passing here.
			if err := apcmd.ValidateDryRunMode(dryRun); err != nil {
				return err
			}
			// sigCtx is canceled only by SIGINT; it is NOT subject to the install
			// --timeout. This is the recheckCtx passed into RunInstall so that an
			// exhausted overall timeout does not make the keep-waiting "Y" a no-op.
			sigCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			// ctx adds the --timeout deadline on top of sigCtx. When the timeout
			// fires, ctx is canceled but sigCtx remains live — so recheckBase polls
			// can still run after the initial deadline passes.
			// The K8s-side --timeout is extendable: an interactive AI-fix ([f])
			// session can run for many minutes, and that user time must not count
			// against the install budget. resetDeadline (invoked when the fix session
			// returns) restarts the window so resuming synchronous work gets a fresh
			// deadline instead of an already-expired one.
			ctx, resetDeadline, cancel := newExtendableTimeout(sigCtx, timeout)
			defer cancel()
			// External CLI calls (gcloud) drive their own multi-minute LROs and must
			// not be SIGKILLed by the K8s-side --timeout; they use this generous
			// bound instead, carried on ctx to every gcloud call downstream.
			ctx = cloud.WithExternalToolTimeout(ctx, externalToolTimeout)
			// Carry the reset handle on ctx so the AI-fix hook (set deep in
			// RunInstall) can reach it regardless of later context wrapping.
			ctx = withDeadlineReset(ctx, resetDeadline)
			// Records what the pipeline actually applied. The interrupt handler
			// below refuses to offer a teardown when this stayed empty.
			applied := &appliedSet{}
			ctx = withAppliedSet(ctx, applied)
			out := cmd.OutOrStdout()
			// Whether this cluster already carried an installation, answered BELOW
			// but BEFORE anything is applied, so it describes the cluster as oap
			// found it. The interrupt handler refuses to offer a teardown when it
			// is true, because `oap clean` is a whole-installation uninstall. It
			// starts true so every path that cannot probe (--dry-run=client has no
			// cluster access) declines to delete.
			hadPriorInstall := true
			// Resolve the cluster kind ONCE, here at the CLI edge — RunInstall no
			// longer detects the cloud itself. --dry-run=client has no cluster
			// access, so an explicit --cluster-kind is still honored via cloud.For
			// (no cluster call), but Validate is skipped (it needs live node/
			// kubeconfig data); an empty --cluster-kind falls back to
			// cloud.Default(), exactly as RunInstall used to before any live API
			// server was available to inspect nodes against.
			var strat cloud.Strategy
			if dryRun == apcmd.DryRunClient {
				var derr error
				if clusterKind != "" {
					strat, derr = cloud.For(clusterKind)
				} else {
					strat, derr = cloud.Default()
				}
				if derr != nil {
					return derr
				}
			} else {
				bundle, berr := g.Bundle()
				if berr != nil {
					return berr
				}
				prior, probeErr := preexistingInstall(ctx, bundle)
				if probeErr != nil {
					cliout.Warn(out, "could not determine whether an installation already exists (%v); an interrupted install will decline to clean up", probeErr)
				}
				hadPriorInstall = prior
				installKctx, kctxErr := g.CurrentContext()
				if kctxErr != nil {
					cliout.Warn(out, "could not resolve the kube-context for --cluster-kind resolution: %v", kctxErr)
				}
				var rerr error
				strat, rerr = resolveClusterKind(ctx, clusterKind, bundle.Typed, bundle.REST, installKctx, allowNonLocal)
				if rerr != nil {
					return rerr
				}
			}
			installErr := RunInstall(ctx, sigCtx, InstallConfig{
				Out:    out,
				G:      g,
				Tags:   tags,
				DryRun: dryRun,
				Routing: WebdRoutingOpts{
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
				},
				PinningMode: pinningMode,
				Strat:       strat,
				Ext:         ExternalSpiceDB{}, // no external SpiceDB for a plain `oap install`
				Workspace: WorkspaceResolveOptions{
					ExplicitClass: workspaceClass,
					NoRWX:         noWorkspaceRWX,
					Recheck:       workspaceRecheck,
				},
				Stateful:            StatefulResolveOptions{ExplicitClass: statefulClass},
				Artifact:            ArtifactStoreOptions{ExplicitURL: artifactStoreURL},
				ImagePullSecret:     imagePullSecret,
				MirrorDeps:          mirrorDeps,
				ImagesPrebaked:      false,
				ClampUndersizedPVCs: clampUndersizedPVCs,
				WithoutBuilder:      withoutBuilder,
				BuilderStarters:     builderStarters,
				Rail:                nil, // no wizard rail on a standalone `oap install`
			})
			// Part 2: On SIGINT, decide what to do about what was applied.
			// sigCtx.Err() != nil means SIGINT fired; the timeout only cancels ctx,
			// not sigCtx, so this branch is SIGINT-exclusive.
			if sigCtx.Err() != nil && !assumeYes && isInteractive() {
				// Release the install's SIGINT trap and re-arm a fresh one over the
				// teardown alone. Without this, the trap installed for the install
				// swallows every further Ctrl-C, so an operator who changes their
				// mind cannot interrupt a multi-minute teardown they just accepted.
				stop()
				cleanCtx, stopClean := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stopClean()
				go func() {
					// Once that second Ctrl-C has cancelled the teardown, hand SIGINT
					// back to the runtime so a third one terminates the process
					// instead of being absorbed by a handler with nothing left to do.
					<-cleanCtx.Done()
					stopClean()
				}()
				if err := handleInstallInterrupt(cleanCtx, interruptDeps{
					Out:      out,
					Applied:  applied,
					HadPrior: hadPriorInstall,
					Confirm: func(p string) bool {
						return apcmd.Confirm(os.Stdin, out, p, false, true)
					},
					Clean: func(ctx context.Context, opts cleanOptions) error {
						return runClean(ctx, out, g, opts)
					},
				}); err != nil {
					return err
				}
			}
			return installErr
		},
	}
	cmd.Flags().StringVar(&tags.Operator, "operator-image", "", "Override operator image (default: spicebox-operator:dev)")
	cmd.Flags().StringVar(&tags.Runner, "runner-image", "", "Override runner image (default: agentprimitives-runner:dev)")
	cmd.Flags().StringVar(&tags.Registry, "image-registry", "", "Rewrite first-party images to <registry>/<name>:<tag> (remote clusters; build+push via `oap init`/`oap build --image-registry`)")
	apcmd.DryRunModeFlag(cmd, &dryRun)
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Timeout for K8s-side install work (does not bound external CLI calls; see --external-tool-timeout)")
	cmd.Flags().DurationVar(&externalToolTimeout, "external-tool-timeout", externalToolTimeout, "Timeout for a single external CLI call (gcloud); generous because cloud LROs like GKE node-pool recreation run for many minutes")
	cmd.Flags().StringVar(&workspaceClass, "workspace-storage-class", "", "Override workspace StorageClass (skips auto-detection)")
	cmd.Flags().BoolVar(&noWorkspaceRWX, "no-workspace-rwx", false, "Disable RWX workspace support (multi-bundle agents will use isolated /work emptyDirs)")
	cmd.Flags().BoolVar(&workspaceRecheck, "workspace-storage-class-recheck", false, "Force re-detection of workspace storage class, ignoring the cached marker")
	cmd.Flags().StringVar(&statefulClass, "stateful-storage-class", "", "Override RWO StorageClass for bundled Postgres/Neo4j (skips auto-detection)")
	cmd.Flags().BoolVar(&developMode, "develop", false, "DEV ONLY: run webd in dev mode (--web-dev) so the UI loads from a local Vite dev server; run `mage web:dev` alongside")
	cmd.Flags().StringVar(&trustedHostname, "trusted-hostname", "",
		"Bare hostname for webd's trusted origin (e.g. webd.example.com; no scheme/port/path). "+
			"Sets up external access: Gateway + cert + the trusted-url ConfigMap. Gates the whole flow.")
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
	cmd.Flags().StringVar(&pinningMode, "pinning-mode", "", pinningModeUsage)
	cmd.Flags().StringVar(&imagePullSecret, "image-pull-secret", "", "Reference an existing pull Secret on first-party component + runner/detector pods (private registries)")
	cmd.Flags().BoolVar(&mirrorDeps, "mirror-dependencies", false, "Rewrite dependency image refs to mirrors under --image-registry (air-gapped); requires --image-registry")
	cmd.Flags().BoolVar(&noDigestPin, "no-digest-pin", false, "Install remote first-party images by mutable :dev tag instead of pinning their digest (debugging; not recommended). Each image must still be present in the registry — this skips pinning, not the existence check")
	cmd.Flags().StringVar(&artifactStoreURL, "artifact-store-url", "",
		"Durable artifact store URL for the operator (gs://<bucket> | s3://<bucket> | azblob://<container> | file:///<path>). Skips cloud auto-provisioning.")
	cmd.Flags().BoolVar(&clampUndersizedPVCs, "clamp-undersized-pvcs", false,
		"When a provisioned PVC requests less than the target cluster's StorageClass floor, raise it to the floor automatically instead of aborting (non-interactive self-heal)")
	cmd.Flags().StringVar(&clusterKind, "cluster-kind", "", clusterKindFlagUsage)
	cmd.Flags().BoolVar(&allowNonLocal, "allow-non-local-cluster", false,
		"Override the cluster-safety check that gates the local and desktop kinds. Only for unusual but "+
			"legitimate setups (a desktop VM reached on its own IP, a corporate dev cluster on a private VPN). "+
			"Relaxes ONLY the kubeconfig-host heuristic, never a managed-cloud refusal.")
	cmd.Flags().BoolVar(&withoutBuilder, "without-builder", false, "Skip installing the agent-builder workshop bundle")
	cmd.Flags().StringSliceVar(&builderStarters, "builder-starters", nil, "Identities (user:/group:) allowed to start the agent builder; required unless --without-builder")
	return cmd
}

// clusterKindEnvName carries the resolved kind to the operator and webd,
// which resolve the same registry `oap install` did rather than inferring
// local-ness from unrelated config — the operator and webd both read it
// fail-closed at startup (internal/cmd/operator/main.go's resolveClusterKindFromEnv,
// internal/cmd/webd/main.go's cloud.For call). It gets NO default in config/: an
// unset value must stay fatal at startup, and a base default would silently
// make "unset" mean "default", defeating that fail-closed contract.
// It is cloud.ClusterKindEnvVar rather than a second literal: cloud.Stamped
// reads this stamp back host-side, and a reader looking for a different
// spelling would report an unknown cluster kind instead of the one installed.
const clusterKindEnvName = cloud.ClusterKindEnvVar

// stampClusterKind writes the resolved kind onto both the operator and webd
// Deployments. A pure function of key — no wall-clock, no randomness, no
// counter — so a byte-identical re-install re-applies byte-identical env and
// stays an SSA no-op (setDeploymentEnv upserts one entry; it never appends a
// duplicate on a second call with the same key).
func stampClusterKind(docs []*unstructured.Unstructured, key string) error {
	if err := setOperatorEnv(docs, clusterKindEnvName, key); err != nil {
		return fmt.Errorf("inject %s into operator Deployment: %w", clusterKindEnvName, err)
	}
	if err := setDeploymentEnv(docs, webdDeploymentName, clusterKindEnvName, key); err != nil {
		return fmt.Errorf("inject %s into webd Deployment: %w", clusterKindEnvName, err)
	}
	return nil
}

// appliedSet records which pipeline components this run actually applied.
//
// initpipeline's executor applies components from up to maxConcurrent
// goroutines and calls OnApplied from each (executor.go applyOne), so every
// method here takes the mutex. The interrupt handler reads this to decide
// whether an interrupted install may offer a teardown at all.
type appliedSet struct {
	mu    sync.Mutex
	comps map[string]struct{}
}

func (a *appliedSet) add(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.comps == nil {
		a.comps = make(map[string]struct{})
	}
	a.comps[name] = struct{}{}
}

func (a *appliedSet) empty() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.comps) == 0
}

// names returns the applied component names, sorted, so operator-facing output
// is deterministic across runs.
func (a *appliedSet) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.comps))
	for n := range a.comps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// appliedSetKey carries this run's appliedSet on the context.
//
// It rides the context rather than RunInstall's parameter list for the same
// reason withDeadlineReset does: RunInstall wraps ctx repeatedly on the way
// down, and only `oap install` — not `oap init` or `oap desktop`, which share
// RunInstall — has an interrupt handler that consumes the set.
type appliedSetKey struct{}

// withAppliedSet returns a context carrying applied, so RunInstall can report
// what landed back to the caller that owns the interrupt prompt.
func withAppliedSet(ctx context.Context, applied *appliedSet) context.Context {
	if applied == nil {
		return ctx
	}
	return context.WithValue(ctx, appliedSetKey{}, applied)
}

// appliedSetFrom returns the context's appliedSet, or a fresh throwaway one when
// the caller did not install a recorder. Never nil: the executor's OnApplied
// callback is wired to its add method unconditionally.
func appliedSetFrom(ctx context.Context) *appliedSet {
	if a, ok := ctx.Value(appliedSetKey{}).(*appliedSet); ok && a != nil {
		return a
	}
	return &appliedSet{}
}

// installMutations is how `oap install` mutates the cluster in the region
// between the plan-confirm gate and the pipeline executor. Narrating a mutation
// and recording it on the run's appliedSet are deliberately ONE call: leaving
// them as two independent things to remember at each mutation site is what
// produced the defect this type exists to prevent — the base-region loop
// narrated "applied …" for every CRD, the agentprimitives-system Namespace,
// RBAC and both Deployments while the recorder stayed empty, so a FIRST install
// interrupted at the stateful-storage wait was told "nothing was applied before
// the interrupt" with all of that live in the cluster. That withheld the
// teardown offer in exactly the case it was written for.
//
// The set records ATTEMPTED mutations, not effective ones: a write the
// apiserver accepted counts, even when server-side apply merged a
// byte-identical object and changed nothing. kube.Apply reports no
// changed/unchanged signal, and this is the safe direction for the one consumer
// — over-recording can at most offer a teardown to an operator who is then
// shown the full `oap clean` scope and asked, and only on a cluster that carried
// no prior installation (interruptTeardownDecision refuses outright when one
// did), while under-recording is the defect above.
//
// A handful of pre-pipeline mutations deliberately stay off it, because each
// one may legitimately do nothing and recording a write that never happened
// would make the "this run applied: …" summary untrue: the cloud DNS-egress
// policy (a no-op wherever DNS resolves via kube-system), the conditional
// operator roll, and the stateful-storage probe. All three run strictly AFTER
// applyBaseRegion, and the pipeline that follows records through the executor's
// OnApplied callback, so the predicate itself is correct from the first applied
// doc onward — they cost detail in the summary, never a wrong answer.
type installMutations struct {
	rep progress.Reporter
	// applied is this run's recorder, shared with `oap install`'s interrupt
	// handler and with the pipeline executor.
	applied *appliedSet
}

// mutationsFor binds the run's recorder (carried on ctx by withAppliedSet) to
// rep. Callers outside `oap install` — `oap init`, `oap desktop` — install no
// recorder and get a throwaway one, exactly as the pipeline's OnApplied does.
func mutationsFor(ctx context.Context, rep progress.Reporter) *installMutations {
	return &installMutations{rep: rep, applied: appliedSetFrom(ctx)}
}

// apply server-side-applies one manifest doc and, only once the apiserver has
// accepted it, records it under label and narrates it. Recording under a label
// rather than the resource's own ID lets a whole region share one entry — see
// baseRegionLabel.
func (m *installMutations) apply(ctx context.Context, dyn dynamic.Interface, d *unstructured.Unstructured, label string) error {
	if err := kube.Apply(ctx, dyn, d, "ap-install"); err != nil {
		return fmt.Errorf("apply %s/%s: %w", d.GetKind(), d.GetName(), err)
	}
	m.applied.add(label)
	m.rep.Info("applied %s", resourceID(d.GetKind(), d.GetNamespace(), d.GetName()))
	return nil
}

// ensure runs one imperative prerequisite — the Secret/ConfigMap writes the
// declarative Component model cannot express — narrating it as it starts and
// recording label once it succeeds. fn keeps its own error wrapping so each
// step retains its specific message; ensure adds none.
func (m *installMutations) ensure(ctx context.Context, label string, fn func(context.Context) error) error {
	m.rep.Info("ensure %s", label)
	if err := fn(ctx); err != nil {
		return err
	}
	m.applied.add(label)
	return nil
}

// baseRegionLabel names the base manifest region as a single entry in the
// interrupt handler's "this run applied: …" summary. One label rather than the
// ~90 resource IDs the region actually contains: that summary is read by an
// operator deciding what to undo by hand, and a wall of IDs serves that worse
// than a phrase naming the region.
const baseRegionLabel = "core manifests (CRDs, namespace, RBAC, operator, webd)"

// applyBaseRegion applies the base manifest region: every doc in the embedded
// install bundle outside the workspace-provisioner tier — all the CRDs, the
// agentprimitives-system Namespace, RBAC, the operator and webd Deployments,
// their Services, the bundled NetworkPolicies and the validating webhook. It is
// the first thing `oap install` writes to the cluster, and it runs before the
// imperative prerequisite secrets and the pipeline executor — so recording it
// is what makes the interrupt gate's "did this run change anything?" question
// answerable at every later interrupt point.
//
// mirrorMap rewrites image refs to their air-gapped mirrors (empty → no-op).
func applyBaseRegion(ctx context.Context, dyn dynamic.Interface, docs []*unstructured.Unstructured, mirrorMap map[string]string, m *installMutations) error {
	for _, d := range docs {
		if err := mirrorImageRefs(d, mirrorMap); err != nil {
			return fmt.Errorf("mirror image refs: %w", err)
		}
		if err := mirrorOperatorSnapshotImage(d, mirrorMap, apimage.Operator.Name); err != nil {
			return fmt.Errorf("mirror operator snapshot image: %w", err)
		}
		if err := m.apply(ctx, dyn, d, baseRegionLabel); err != nil {
			return err
		}
	}
	return nil
}

// interruptDeps are the collaborators handleInstallInterrupt needs, injected so
// the interrupt path can be tested without a cluster, a TTY, or a signal.
type interruptDeps struct {
	Out     io.Writer
	Applied *appliedSet
	// HadPrior reports whether the cluster already carried an oap installation
	// before this run touched it, probed by preexistingInstall.
	HadPrior bool
	Confirm  func(prompt string) bool
	Clean    func(ctx context.Context, opts cleanOptions) error
}

// preexistingInstall reports whether this cluster already carried an oap
// installation before this run touched it, probed via the AgentSession CRD —
// `oap install` creates the CRDs, so their presence beforehand means an earlier
// install is already here.
//
// It must be called BEFORE any mutation, so the answer describes the cluster as
// oap found it. The bool is fail-safe: on any error other than a definite
// NotFound it answers "yes, assume one exists", because its only consumer is
// the interrupt teardown gate and being wrong in that direction declines to
// delete. The error is returned as well so the caller can surface it rather
// than silently acting on a guess.
func preexistingInstall(ctx context.Context, b *kube.Bundle) (bool, error) {
	if b == nil || b.Controller == nil {
		return true, fmt.Errorf("no controller-runtime client available to probe for an existing installation")
	}
	var crd apiextv1.CustomResourceDefinition
	err := b.Controller.Get(ctx, client.ObjectKey{Name: "agentsessions.agentprimitives.authzed.com"}, &crd)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return true, fmt.Errorf("probe for an existing installation: %w", err)
	}
}

// interruptTeardownDecision decides whether an interrupted install may offer to
// tear down what it did.
//
// The only teardown available is `oap clean`, which is a FULL uninstall: every
// CR in the group across all namespaces, the spicebox-postgres-data and
// data-spicebox-neo4j-0 PVCs (the memory store and knowledge graph), every CRD,
// the agentprimitives-system namespace, and any oap-installed cert-manager /
// Envoy Gateway. There is no per-component teardown, so offering it against a
// cluster that already had an installation destroys work this run did not do —
// and `oap install` is the documented upgrade path, so re-running it against a
// live cluster is ordinary.
//
// Therefore: offer only when this run applied something AND the cluster had no
// prior installation. Otherwise explain, and leave the decision with the
// operator.
func interruptTeardownDecision(applied *appliedSet, preexisting bool) (offer bool, why string) {
	if preexisting {
		return false, "an existing installation is present, and `oap clean` removes the whole installation rather than only this run's changes"
	}
	if applied == nil || applied.empty() {
		return false, "nothing was applied before the interrupt"
	}
	return true, ""
}

// handleInstallInterrupt decides what an interrupted `oap install` offers to do
// about the work it already applied.
func handleInstallInterrupt(ctx context.Context, d interruptDeps) error {
	offer, why := interruptTeardownDecision(d.Applied, d.HadPrior)
	if !offer {
		cliout.Warn(d.Out, "Installation interrupted; leaving the cluster as-is (%s).", why)
		if d.Applied != nil {
			if names := d.Applied.names(); len(names) > 0 {
				cliout.Info(d.Out, "  this run applied: %s", strings.Join(names, ", "))
			}
		}
		cliout.Info(d.Out, "  to remove the whole installation, run: oap clean")
		return nil
	}

	// Yes:true below suppresses oap clean's own scope banner, so this is the only
	// place the operator is told what is about to be deleted.
	cliout.Warn(d.Out, "Installation interrupted after applying: %s", strings.Join(d.Applied.names(), ", "))
	cliout.Warn(d.Out, "Cleaning up removes the ENTIRE installation: all agentprimitives CRs")
	cliout.Warn(d.Out, "across ALL namespaces, the spicebox-postgres-data and")
	cliout.Warn(d.Out, "data-spicebox-neo4j-0 PVCs (the memory store and knowledge graph),")
	cliout.Warn(d.Out, "every CRD, the agentprimitives-system namespace, and any")
	cliout.Warn(d.Out, "cert-manager / Envoy Gateway oap installed.")

	if !d.Confirm("Remove it?") {
		return nil
	}
	// oap clean's own budget, not oap install's --timeout: a full teardown
	// routinely outlasts the install budget, and being cut off mid-teardown is
	// worse than taking longer.
	return d.Clean(ctx, cleanOptions{Yes: true, Timeout: cleanDefaultTimeout})
}

// strat is the resolved cloud.Strategy for this install (the caller resolves
// and validates it — RunInstall no longer detects the cloud itself). Its
// InstallProfile() answers what used to be branched on via the allowSharedOrigin
// bool: `oap init --local` passes cloud.MustFor(cloud.KeyLocal) and `oap desktop`
// passes cloud.MustFor(cloud.KeyDesktop) — both dev profiles inject the
// INSECURE --allow-shared-origin debug flag into the webd Deployment, and
// differ from each other only in InstallProfile().ServesLocalWebChat(), an
// answer nothing currently reads; a plain `oap install` resolves the actual
// cluster kind, so production installs never carry it.
// imagePullSecret, when non-empty, injects imagePullSecrets:[{name}] into every
// first-party component Deployment's pod template and passes
// --image-pull-secret=<name> to the operator container so it stamps the secret
// onto runner/detector pods. Empty → no change.
//
// recheckCtx is a context that is NOT subject to the overall --timeout; it is
// used for keep-waiting recheck windows so an exhausted timeout cannot make "Y"
// a no-op. Callers pass a SIGINT-cancelable context (signal.NotifyContext) here.
//
// imagesPrebaked, when true, means the target cluster's images are already
// present (baked into the cluster's own rootfs, not pulled or built at
// install time — see `oap desktop`'s air-gapped VM) so preflight's "building"
// docker check is skipped even though tags.Registry == "". Every other
// caller (plain `oap install`, `oap init`/`oap init --local`) passes false —
// they build/load local :dev images via docker and still need that check.
//
// ext, when ext.enabled() (the user brought their own SpiceDB instance via
// `oap init --external-spicedb-endpoint` or the equivalent prompt), means:
// buildCoreComponents omits the SpiceDB/SpiceDBOperator/SpiceDBDatabase
// components entirely (no CR, no operator wait, no DB Job); the token/
// bootstrap/endpoint-config prerequisite writes below are skipped in favor of
// applyExternalSpiceDBConfig pointing the shared ConfigMap/Secret at ext's
// endpoint+token; and the Postgres datastore_uri write is skipped (the
// external instance brings its own datastore). Every caller except `oap init`
// passes the zero value (disabled) — plain `oap install` and `oap desktop`
// always install the bundled operator-managed SpiceDB. When ext.enabled() and
// the instance is over TLS (!ext.Insecure), every in-cluster SpiceDB client
// (operator/authzd/webd via flipSpiceDBInsecureForExternalTLS, channelsd via
// appendChannelsdInsecureFlag, runners transitively via the operator) is
// flipped to dial TLS instead of the bundle's plaintext default. The
// standard TLS port (:443) is already covered by the existing egress
// allowlists; a non-:443 external endpoint needs manual egress widening.
// InstallConfig bundles every non-context input to RunInstall. The two
// contexts (ctx, recheckCtx) stay as explicit RunInstall parameters per Go
// idiom — a context never belongs in a struct.
type InstallConfig struct {
	Out io.Writer
	G   *apcmd.Globals
	// Tags carries the image tags/registry/digests substituted into the
	// install bundle; RunInstall may mutate its local copy (e.g. resolving
	// Registry/Digests) without affecting the caller's value.
	Tags manifests.Tags
	// DryRun selects apcmd.DryRunClient/DryRunServer/"" — see
	// apcmd.ValidateDryRunMode.
	DryRun  string
	Routing WebdRoutingOpts
	// PinningMode is validated by validPinningMode before any cluster read.
	PinningMode string
	// Strat is the resolved cloud.Strategy for this install — the caller
	// resolves and validates it; RunInstall no longer detects the cloud
	// itself. See the InstallProfile() discussion above.
	Strat cloud.Strategy
	// Ext carries a user-supplied external SpiceDB endpoint/token; the zero
	// value (disabled) means RunInstall installs the bundled
	// operator-managed SpiceDB. See the ext discussion above.
	Ext       ExternalSpiceDB
	Workspace WorkspaceResolveOptions
	Stateful  StatefulResolveOptions
	Artifact  ArtifactStoreOptions
	// ImagePullSecret, when non-empty, injects imagePullSecrets:[{name}] into
	// every first-party component Deployment's pod template and passes
	// --image-pull-secret=<name> to the operator container. Empty → no change.
	ImagePullSecret string
	MirrorDeps      bool
	// ImagesPrebaked, when true, means the target cluster's images are
	// already present (baked into the cluster's own rootfs, not pulled or
	// built at install time — see `oap desktop`'s air-gapped VM) so
	// preflight's "building" docker check is skipped even though
	// Tags.Registry == "". Every other caller passes false.
	ImagesPrebaked bool
	// ClampUndersizedPVCs allows preflight to clamp a requested PVC size up
	// to a provider's storage-class minimum instead of failing.
	ClampUndersizedPVCs bool
	WithoutBuilder      bool
	BuilderStarters     []string
	// Rail is nil for every flag-driven caller (`oap install`, `oap
	// desktop`, today's `oap init`); NewWithRail forwards a nil rail to
	// progress.New verbatim, so their checklist is byte-for-byte unchanged.
	// Only the unified init wizard passes a non-nil rail, to draw its step
	// gutter beside the install checklist.
	Rail progress.RailProvider
}

func RunInstall(ctx, recheckCtx context.Context, cfg InstallConfig) error {
	out, g, tags, dryRun, r, pinningMode, strat, ext, wsOpts, statefulOpts, artifactOpts, imagePullSecret, mirrorDeps, imagesPrebaked, clampUndersizedPVCs, withoutBuilder, builderStarters, rail := cfg.Out, cfg.G, cfg.Tags, cfg.DryRun, cfg.Routing, cfg.PinningMode, cfg.Strat, cfg.Ext, cfg.Workspace, cfg.Stateful, cfg.Artifact, cfg.ImagePullSecret, cfg.MirrorDeps, cfg.ImagesPrebaked, cfg.ClampUndersizedPVCs, cfg.WithoutBuilder, cfg.BuilderStarters, cfg.Rail

	if err := validPinningMode(pinningMode); err != nil {
		return err
	}
	// Before any cluster read: an unrecognized mode found later would make the
	// user wait through registry resolution and routing prompts for a run that
	// was always going to be refused.
	if err := apcmd.ValidateDryRunMode(dryRun); err != nil {
		return err
	}
	// p is the install profile in effect for this call — the single source of
	// truth every allowSharedOrigin-branch below used to read a bool for.
	p := strat.InstallProfile()

	// Resolve the image registry BEFORE substituting the install bundle so that
	// tags.Registry is final when manifests.Substitute runs. Without this order,
	// a standalone `oap install` on a remote cluster would render the base docs
	// with an empty registry (`:dev` images, silent ImagePullBackOff) even though
	// the interactive resolver later sets tags.Registry correctly.
	//
	// --dry-run=client has no cluster access and skips this block entirely;
	// the real install path goes through it.
	// A profile that uses local dev images (only `local`, via `oap init --local`/
	// `oap desktop`) skips the remote-registry check below.
	var bundle *kube.Bundle
	if dryRun != apcmd.DryRunClient {
		var berr error
		bundle, berr = g.Bundle()
		if berr != nil {
			return berr
		}

		if !p.UsesLocalDevImages() {
			installKctx, kctxErr := g.CurrentContext()
			if kctxErr != nil {
				cliout.Warn(out, "could not resolve the kube-context — assuming a non-managed (local) cluster, which uses local :dev images. Pass --image-registry if this is a remote cluster. (%v)", kctxErr)
			}
			registry, _, needsRemote, merr := imagemode.Resolve(tags.Registry, p, installKctx)
			if merr != nil {
				return merr
			}
			if needsRemote {
				registry, merr = imagemode.ResolveRemoteFromCluster(ctx, bundle.Typed, strat, installKctx, out, os.Stdin)
				if merr != nil {
					return merr
				}
				tags.Registry = registry
			}
		}
	}

	// Expand --hostname-suffix into the two webd origins, ask for whichever of
	// them (and of the certificate email) this install is about to refuse to
	// run without, then validate. Expansion and validation are pure
	// preconditions run BEFORE any apply so a missing/invalid hostname fails
	// fast instead of aborting mid-install (which leaves a half-deployed,
	// crash-looping cluster); the ASK sits BETWEEN them so a typed answer is
	// indistinguishable from a flag by the time the gate reads it. The gate
	// itself does not move and does not soften: an install with no terminal,
	// or one that leaves a question blank, fails exactly where and how it did.
	r, herr := r.withHostnameSuffix()
	if herr != nil {
		return herr
	}
	// Not under --dry-run, which resolves and prompts for nothing — the same
	// rule the workspace-storage decision below follows. The gate still runs,
	// so a dry run refuses exactly as it did.
	if dryRun == "" {
		// Seed the ACME email prompt with the cluster's existing Let's Encrypt
		// ClusterIssuer, when there is one, before asking — see seededACMEEmail.
		// bundle is nil only under --dry-run=client, which this branch already
		// excludes, but the guard keeps this call safe if that changes.
		if bundle != nil {
			r = seededACMEEmail(ctx, bundle.Dynamic, r, out)
		}
		var aerr error
		r, aerr = askWebdRoutingInputs(ctx, out, os.Stdin, g.NoColor, isInteractive(), p, r)
		if aerr != nil {
			return aerr
		}
	}
	if err := validateWebdRouting(p, r.trustedHostname, r.sandboxHostname, r.disableViewer, r.manualWebdRouting); err != nil {
		return err
	}

	// Validate and build the mirror map for air-gapped installs. This must happen
	// after the registry may have been resolved interactively above so tags.Registry
	// is final.
	if mirrorDeps && tags.Registry == "" {
		return fmt.Errorf("--mirror-dependencies requires an image registry (set --image-registry)")
	}
	var mirrorMap map[string]string
	if mirrorDeps && tags.Registry != "" {
		mirrorMap = apimage.MirrorMap(tags.Registry)
	}

	// Remote installs pin first-party images by digest so a warm node can't serve
	// a stale :dev (IfNotPresent caches by tag). `oap init` populates tags.Digests
	// from the build it just pushed; anything that build did not cover — and a
	// standalone `oap install --image-registry`, which built nothing — is resolved
	// from the registry here. Fail-closed per image: no silent fallback to the
	// mutable tag, and no skipping the check for image B just because image A
	// came back with a digest. The earlier all-or-nothing gate
	// (len(tags.Digests) == 0) did exactly that, and let the install plant
	// SpiceboxToolchain refs to overlay images that were never pushed.
	//
	// Both remote branches are nested under one `if tags.Registry != ""` so a
	// future edit cannot introduce a registry install that reaches neither and
	// silently plants unverified refs: --no-digest-pin alone selects mutable
	// tags over digests, and that is the ONLY axis this gate varies — it must
	// never become a third way to skip both inner branches. Any install that
	// supplies a registry gets digest pinning by default, regardless of
	// cluster kind: a stale :dev tag under IfNotPresent is a risk on every
	// remote install, not just ones the InstallProfile happens to flag.
	// Either way, every image the manifest rewrite is about to reference is
	// proven to exist first.
	if tags.Registry != "" {
		if !r.noDigestPin {
			digests, derr := resolveMissingRegistryDigests(ctx, tags.Registry, tags.Digests)
			if derr != nil {
				return derr
			}
			tags.Digests = digests
		} else {
			cliout.Info(out, "installing remote images by mutable tag (not pinning digests)")
			if err := verifyRegistryRefs(ctx, tags.Registry); err != nil {
				return err
			}
		}
	} else if !imagesPrebaked {
		// Local install: the same "planted but never provisioned" gap exists for
		// the toolchain overlays, but it can only be warned about, not proven —
		// see warnMissingLocalToolchainImages. Skipped when images are prebaked
		// (the desktop VM's image store, not the local docker daemon, is the
		// source of truth there, so probing docker would warn about images that
		// are genuinely present).
		kctx, kerr := g.CurrentContext()
		if kerr != nil {
			cliout.Info(out, "could not resolve kube-context for the local toolchain-image check: %v", kerr)
		}
		warnMissingLocalToolchainImages(ctx, out, kctx)
	}

	yaml, err := manifests.Substitute(manifests.Install, tags)
	if err != nil {
		return err
	}
	docs, err := manifests.Split(yaml)
	if err != nil {
		return err
	}

	if dryRun == apcmd.DryRunClient {
		for _, d := range docs {
			cliout.Info(out, "%s %s/%s (would apply)", d.GetKind(), d.GetNamespace(), d.GetName())
		}
		return nil
	}

	// Construct the progress reporter AFTER all dry-run paths have returned, so
	// the dry-run output continues to flow through cliout directly (and
	// TestInstallDryRunPrintsObjects stays green). Keep `out` in scope — cloud.*
	// calls, newCloudReporter(out), and the ensure helpers below use it.
	// rail is nil for every flag-driven caller (`oap install`, `oap desktop`,
	// today's `oap init`), and NewWithRail forwards a nil rail to progress.New
	// verbatim — so their checklist is byte-for-byte unchanged. Only the unified
	// init wizard passes a non-nil rail, to draw its step gutter beside the
	// install checklist.
	rep := progress.NewWithRail(out, os.Stdin, r.assumeYes, rail)
	defer func() { _ = rep.Close() }()

	// AI-fix hotkey: when the user already has a supported AI CLI installed
	// (claude / codex / gemini), offer an [f] key on a stalled keep-waiting
	// prompt that launches it pre-loaded with the gathered diagnostics, repo
	// root, and cluster context to diagnose & fix the failure. USER-INITIATED:
	// `oap` only assembles context and execs the user's OWN CLI in their OWN
	// shell with their OWN credentials — the launched agent's permission model
	// governs whether it applies any change. Only set the hook when a fixer is
	// actually present, so the prompt shows [Y/n/f] only when [f] would work.
	if _, ok := aifix.FirstAvailable(); ok {
		rep.SetFixHook(func(fixCtx context.Context, component string, diag wait.Diagnosis) error {
			launcher, ok := aifix.FirstAvailable()
			if !ok {
				return fmt.Errorf("no supported AI CLI found on PATH (looked for %s)", aifix.Names())
			}
			kubeCtx, _ := g.CurrentContext() // best-effort; an empty context is fine in the prompt
			prompt := aifix.BuildPrompt(installRepoRoot(), component, diag, kubeCtx, strat.Key())
			rep.Info("launching %s to diagnose %s…", launcher.Name(), component)
			// The fix session is interactive and unbounded; exclude its wall-clock
			// from the install budget so synchronous steps resuming after it get a
			// fresh --timeout window rather than an already-expired one.
			defer resetInstallDeadline(ctx)
			return launcher.Launch(fixCtx, prompt)
		})
	}

	// §A8: a short preflight "doctor" — read-only, high-value checks run before
	// any cluster change. It is past every --dry-run guard (those returned
	// above), so it never runs under --dry-run. A hard failure (cluster
	// unreachable) returns here, before anything has been applied.
	//
	// building is true when this install relies on locally-built first-party
	// :dev images (local image mode — no remote registry was resolved above).
	// Those images are produced/loaded by docker into the local cluster, so in
	// that mode preflight also verifies the docker CLI is present — a missing one
	// is caught now with a plain fix rather than surfacing later as an opaque
	// ImagePullBackOff. A remote-registry install builds and loads nothing
	// locally — the cluster pulls instead — so this particular check does not
	// apply to it. That does NOT make a remote install docker-free: the digest /
	// existence gate above reads each planted ref's metadata via `docker buildx
	// imagetools`, so docker/buildx and registry read credentials are required
	// there, on both the pinned and the --no-digest-pin path, and a remote
	// install missing either has already failed before reaching preflight.
	//
	// imagesPrebaked (see the doc comment above) also skips it: the target
	// cluster's images are already there (baked into its rootfs), so
	// tags.Registry == "" does NOT imply this run will build/load anything via
	// docker.
	building := tags.Registry == "" && !imagesPrebaked
	if err := preflight(ctx, bundle, strat, rep, building); err != nil {
		return err
	}

	// Split the bundle: base region applies always; the workspace-provisioner
	// region applies only when the cloud workspace-storage resolver decides bundledNeeded.
	wsDocs, baseDocs := manifests.FilterByInstallTier(docs, installTierLabelKey, installTierWorkspaceProvisioner)

	// ── Ask up front: fully resolve the workspace-storage decision (including the
	// interactive Filestore cost-confirm prompt) BEFORE any data-plane apply, so
	// the install never stops to ask mid-flow once components are coming up. This
	// is read-only on the cluster except the prompt; the side-effecting WORK
	// (bundled provisioner apply, provisioning probe, marker write, operator
	// patch) still runs later, after the core components, consuming this choice.
	// Runs only past the dry-run guards above (those paths returned), so a
	// --dry-run never resolves or prompts — exactly as before.
	wsChoice, err := resolveWorkspaceChoice(ctx, bundle, strat, wsOpts, rep, out, os.Stdin, r.assumeYes, isInteractive())
	if err != nil {
		return err
	}

	// Resolve the RWO stateful-storage decision up front (pure detection), same
	// as workspace storage. The class name feeds buildCoreComponents (plan
	// summary + manifest injection); the side-effecting create+probe runs later
	// in runStatefulStorageWork, before the data-plane pipeline applies the
	// (immutable) Postgres/Neo4j PVCs.
	statefulDec, err := resolveStatefulChoice(ctx, bundle, strat, statefulOpts, rep)
	if err != nil {
		return err
	}

	// Build the first-party image set once (values of apimage.ResolveDigests after
	// tags substitution) so injectImagePullSecret can distinguish first-party from
	// public images. Done after the registry guard above, where tags.Registry may
	// have been updated by the interactive resolver.
	var firstPartyRefs map[string]bool
	if imagePullSecret != "" {
		firstPartyRefs = map[string]bool{}
		for _, ref := range apimage.ResolveDigests(tags.Registry, tags.Digests, map[string]string{
			apimage.Operator.Name: tags.Operator,
			apimage.Runner.Name:   tags.Runner,
		}) {
			firstPartyRefs[ref] = true
		}
		for _, d := range baseDocs {
			if err := injectImagePullSecret(d, imagePullSecret, firstPartyRefs, apimage.Operator.Name); err != nil {
				return fmt.Errorf("inject imagePullSecret into %s/%s: %w", d.GetKind(), d.GetName(), err)
			}
		}
	}

	// Inject webd flags into the Deployment doc BEFORE the apply loop so the
	// SSA apply set itself carries them (single field manager owns args in one
	// pass; no extra post-install rollout). --web-dev for --develop;
	// --allow-shared-origin only for a profile that allows it (only `local`,
	// via `oap init --local`/`oap desktop`). A plain `oap install` injects
	// neither, so SSA re-applies the baseline args and removes any prior flag
	// in the same rollout — this is what keeps --allow-shared-origin OFF by
	// default on a production install.
	var webdFlags []string
	if developMode {
		webdFlags = append(webdFlags, webDevFlag)
	}
	if p.AllowsSharedOrigin() {
		webdFlags = append(webdFlags, allowSharedOriginFlag)
	}
	if err := injectWebdFlags(baseDocs, webdFlags...); err != nil {
		return fmt.Errorf("inject webd flags %v into webd Deployment: %w", webdFlags, err)
	}

	// Stamp the resolved workspace StorageClass onto the operator Deployment doc
	// BEFORE the base apply, so the SSA apply set itself carries
	// --workspace-storage-class. This makes the operator come up with the right
	// workspace mode from the start rather than being applied in isolated mode
	// and only patched at the very end: the base manifest carries no workspace
	// flag, so SSA strips any prior value, and the post-apply patch re-added it
	// only AFTER the failable provisioning probe — an interrupt or a failed
	// dependency install in that window left the operator running isolated with a
	// live workspace marker, silently bricking every stateful (git) tool call.
	//
	// Only stamp a class we already trust EXISTS (marker / cost-confirm /
	// kept-current — Probe=false). A ProbeBeforeUse class (fresh bundled
	// provisioner, explicit --workspace-storage-class, or a newly-picked class)
	// is withheld here and set by the later patchOperatorWorkspaceClass AFTER its
	// probe proves the StorageClass exists. See injectWorkspaceClassUpFront.
	if injectWorkspaceClassUpFront(wsChoice) {
		if err := injectOperatorWorkspaceClass(baseDocs, wsChoice.ClassName); err != nil {
			return fmt.Errorf("inject --workspace-storage-class into operator Deployment: %w", err)
		}
	}

	// Memory backend: the profile names the value for every kind (see
	// cloud.InstallProfile.MemoryBackend) — "sqlite" for `local`
	// (`oap init --local`/`oap desktop`), a lightweight single-node dev cluster,
	// where it is a persistent single-file backend (backed by the
	// spicebox-operator-memory PVC + MEMORY_SQLITE_PATH env, both already in
	// the base manifest) that survives pod restarts without standing up the
	// bundled Postgres; "postgres" (the bundle's own default) everywhere else.
	// Applied to baseDocs BEFORE the apply loop so the single ap-install field
	// manager carries it. Unconditional (not gated on profile): for the
	// non-local case this re-applies "postgres" onto a field that already
	// reads "postgres" in the base manifest (config/manager/deployment.yaml),
	// so the resulting doc is byte-identical whether or not this call runs —
	// setOperatorEnv/upsertEnvValue replaces an existing entry's value with the
	// same string and only touches valueFrom when one is present (never the
	// case here), so this is a true SSA no-op, not a silent field-ownership
	// change. Both paths bundle postgres, so postgres would also work locally
	// — sqlite is the deliberate lighter-weight choice for the local dev flow.
	if err := setOperatorMemoryBackend(baseDocs, p.MemoryBackend()); err != nil {
		return fmt.Errorf("set operator MEMORY_BACKEND=%s: %w", p.MemoryBackend(), err)
	}

	// External SpiceDB over TLS: flip every in-cluster client's SPICEDB_INSECURE
	// from the base bundle's plaintext default ("true") to "false" on the
	// operator, authzd, and webd Deployments, so they (and, via the operator's
	// own value-copy onto runner pods, every runner) dial the caller's instance
	// with TLS. No-op for --external-spicedb-insecure or a non-external install
	// (see flipSpiceDBInsecureForExternalTLS). channelsd is flipped separately
	// inside buildCoreComponents' channelsd Manifests closure (it has no
	// SPICEDB_INSECURE env; it takes a CLI flag instead). Applied to baseDocs
	// BEFORE the apply loop, same as the memory-backend override above. A
	// non-:443 external endpoint needs manual egress widening.
	if err := flipSpiceDBInsecureForExternalTLS(baseDocs, ext); err != nil {
		return fmt.Errorf("flip SPICEDB_INSECURE for external TLS SpiceDB: %w", err)
	}

	// §A8: build the core-component list now (pure — buildCoreComponents only
	// closes over bundle, with no cluster writes), summarize the whole plan in
	// plain language, and get a final go/no-go BEFORE the first apply below.
	// Declining leaves the cluster untouched (nothing has been applied yet). On
	// --yes / --defaults / a non-TTY this never prompts — it only adds the
	// summary lines, so automation is not blocked.
	comps := buildCoreComponents(bundle, tags, statefulDec.ClassName, strat, ext)
	proceed, err := confirmPlan(comps, r, rep, os.Stdin, out, r.assumeYes, isInteractive())
	if err != nil {
		return err
	}
	if !proceed {
		rep.Info("aborted; nothing was changed.")
		return nil
	}

	// Artifact store: resolve (and on GKE consent-provision) the operator's
	// ARTIFACT_STORE_URL, then inject it into baseDocs. Runs every install:
	// SSA under the oap-install field manager strips values absent from the
	// applied doc, so a gated injection would silently vanish on re-install.
	//
	// The kind that chooses a file:// path (local) is the one that declares the
	// mount it needs via RequiresOperatorVolume — both halves of that decision
	// live in pkg/platform/cloud/local, so they cannot drift. An explicit
	// --artifact-store-url short-circuits Ensure entirely, so a user pointing
	// at their own pre-provisioned mount never gets a PVC injected under them.
	artifactURL, vol, err := resolveArtifactStoreURL(ctx, bundle, strat, artifactOpts, r.assumeYes, rep)
	if err != nil {
		return err
	}
	if err := setOperatorEnv(baseDocs, artifactStoreURLEnvName, artifactURL); err != nil {
		return fmt.Errorf("inject %s into operator Deployment: %w", artifactStoreURLEnvName, err)
	}
	if vol != nil {
		if err := injectOperatorArtifactVolume(baseDocs, *vol); err != nil {
			return fmt.Errorf("inject artifact PVC volume into operator Deployment: %w", err)
		}
		baseDocs = append(baseDocs, localArtifactPVCDoc(*vol))
	}
	rep.Info("artifact store: %s", artifactURL)

	// AP_CLUSTER_KIND: stamp the resolved kind onto the operator and webd
	// Deployments so each resolves the same registry `oap install` did, rather
	// than inferring local-ness from unrelated config (see stampClusterKind).
	// Both binaries read it fail-closed at startup (internal/cmd/operator/main.go's
	// resolveClusterKindFromEnv, internal/cmd/webd/main.go's cloud.For call) — an
	// unset or unrecognized value crash-loops rather than silently defaulting,
	// so a pre-cluster-kind install must be re-applied via `oap install` to
	// pick this up.
	if err := stampClusterKind(baseDocs, strat.Key()); err != nil {
		return err
	}

	// Preflight: no PVC may request less than the target cluster's StorageClass
	// disk floor, or it will never bind and stall the install (the operator-memory
	// hyperdisk incident). Runs BEFORE any apply so an abort leaves the cluster
	// untouched; a clamp rewrites the in-memory baseDocs about to be applied.
	pvcChecks := baseBundlePVCChecks(baseDocs)
	if statefulChecks, serr := statefulPVCChecks(statefulDec.ClassName); serr != nil {
		rep.Warn("could not enumerate stateful PVCs for size preflight: %v", serr)
	} else {
		pvcChecks = append(pvcChecks, statefulChecks...)
	}
	if err := validatePVCFloors(ctx, bundle.Typed, pvcChecks, rep, os.Stdin, r.assumeYes, isInteractive(), clampUndersizedPVCs); err != nil {
		return err
	}

	// Every cluster mutation from here to the pipeline executor goes through m,
	// which records what it applied in the same call that narrates it — see
	// installMutations for why those are not two calls.
	m := mutationsFor(ctx, rep)

	// Apply the base region first — it includes the agentprimitives-system
	// Namespace, which both NATS and channelsd need before they can be applied.
	if err := applyBaseRegion(ctx, bundle.Dynamic, baseDocs, mirrorMap, m); err != nil {
		return err
	}

	// Layer the detected cloud's DNS egress (e.g. GKE Autopilot NodeLocal
	// DNSCache on a link-local address) on top of the bundled, cloud-agnostic
	// allow-dns policy just applied — without it, name resolution hangs under
	// default-deny on those clouds. No-op on clouds that resolve via kube-system.
	if err := cloud.ApplyDNSEgressPolicy(ctx, cloudClients(bundle), strat.DNSEgressCIDRs()); err != nil {
		return fmt.Errorf("ensure cloud DNS egress: %w", err)
	}

	// channelsd's Deployment mounts the spicebox-channelsd-memory-token
	// Secret without `optional: true`, so the pod blocks in ContainerCreating
	// until this Secret exists. Create it now (idempotent) before we start
	// waiting on channelsd readiness.
	if err := m.ensure(ctx, "channelsd memory token", func(ctx context.Context) error {
		if err := ensureChannelsdMemoryToken(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure channelsd memory token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := m.ensure(ctx, "admind token", func(ctx context.Context) error {
		if err := ensureMemoryTokenSecret(ctx, bundle.REST, "spicebox-admind-token"); err != nil {
			return fmt.Errorf("ensure admind token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// webd's dedicated read-only memory token. Mounted into both webd (the
	// holder) and the operator (which registers it as a valid read-only system
	// token). Separate from the channelsd token so a webd compromise cannot
	// write the audit log or register publisher keys.
	if err := m.ensure(ctx, "webd memory token", func(ctx context.Context) error {
		if err := ensureWebdMemoryToken(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure webd memory token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// authzd's memory token. Consumed as a non-optional MEMORY_TOKEN
	// secretKeyRef (the pod blocks in CreateContainerConfigError until it
	// exists) and mounted into the operator, which registers it as a valid
	// system token. Create it before waiting on authzd readiness.
	if err := m.ensure(ctx, "authzd memory token", func(ctx context.Context) error {
		if err := ensureAuthzdMemoryToken(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure authzd memory token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// The operator reads the three memory tokens above ONCE at startup, from
	// *optional* subPath mounts. The operator Deployment is applied in the base
	// bundle moments before these Secrets exist, so its first pod started without
	// them — and a subPath mount of a not-yet-existent Secret materializes as a
	// directory that is never refreshed afterward (not even across container
	// restarts). Such an operator pod can never read the tokens: it never calls
	// Set{Channelsd,Authzd,Webd}Token, so every component's
	// POST /memory/_publisher_key 403s ("requires a system token") and
	// channelsd/authzd/webd crash-loop forever. Roll the operator so a fresh pod
	// remounts the now-present Secrets as real files. Conditional on the pod
	// predating the tokens, so a healthy re-run doesn't churn the operator while
	// a cluster left in the broken state still self-heals.
	if err := rollOperatorIfPredatesMemoryTokens(ctx, rep, bundle.Typed); err != nil {
		return fmt.Errorf("roll operator to pick up memory tokens: %w", err)
	}

	// ── Core data-plane prerequisites (secrets + setup) ──────────────────────
	// These run imperatively BEFORE the pipeline executor because they need
	// clients / `out` that aren't threaded into the declarative Component model.
	// Crucially, EVERY secret here — including webhook TLS — is written before
	// initpipeline.Run applies any component manifest. That secrets-all → apply-
	// all ordering is the structural guarantee that fixes the operator/webhook-TLS
	// startup deadlock (the operator is awaited as a WAIT-phase component below).
	if err := m.ensure(ctx, "NATS identity + TLS + client creds", func(ctx context.Context) error {
		if err := ensureNATSIdentity(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure nats identity: %w", err)
		}
		if err := ensureNATSTLS(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure nats tls: %w", err)
		}
		if err := ensureNATSClientCreds(ctx, bundle.REST, rep.Info); err != nil {
			return fmt.Errorf("ensure nats client creds: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Webhook TLS is ensured here (moved up from the old NATS block) so it lands
	// before ANY component manifest is applied — the operator, once rolled, must
	// always find a CA-backed webhook to become Ready.
	webhookCLI, err := client.New(bundle.REST, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return fmt.Errorf("build client for webhook tls: %w", err)
	}
	if err := m.ensure(ctx, "webhook TLS", func(ctx context.Context) error {
		webhookCAPEM, err := ensureWebhookTLSWithClient(ctx, webhookCLI)
		if err != nil {
			return fmt.Errorf("ensure webhook tls: %w", err)
		}
		if err := patchWebhookVWCBundle(ctx, webhookCLI, webhookCAPEM); err != nil {
			return fmt.Errorf("patch webhook VWC caBundle: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// SpiceDB prerequisites. External: the user brought their own instance, so
	// point the shared endpoint ConfigMap + token Secret at it instead — no
	// token generation, no bootstrap ConfigMap (nothing here writes its schema;
	// the caller applies the base schema out-of-band, see runInit). In-cluster
	// (the default): token Secret + endpoint ConfigMap, plus the bootstrap
	// ConfigMap for the modes that actually read it.
	//
	// The bootstrap ConfigMap is gated on the same mode as the CR's
	// datastoreBootstrapFiles (see SpiceDBClusterMode.bootstrapsSchemaAtStartup):
	// only an ephemeral single-replica datastore may seed itself at startup. In
	// every other mode the AP operator's guardian composer is the authoritative
	// schema writer, so there is nothing for this ConfigMap to do.
	if ext.enabled() {
		if err := m.ensure(ctx, "external SpiceDB endpoint + token config", func(ctx context.Context) error {
			if err := applyExternalSpiceDBConfig(ctx, webhookCLI, ext); err != nil {
				return fmt.Errorf("apply external spicedb config: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	} else {
		if err := m.ensure(ctx, "SpiceDB token + bootstrap + endpoint config", func(ctx context.Context) error {
			if err := ensureSpiceDBToken(ctx, bundle.REST); err != nil {
				return fmt.Errorf("ensure spicedb token: %w", err)
			}
			// strat is the same value buildCoreComponents derives the CR's mode
			// from, so both sides agree by construction.
			if spicedbModeFor(p.SpiceDBDatastore()).bootstrapsSchemaAtStartup() {
				if err := ensureSpiceDBBootstrap(ctx, bundle.REST); err != nil {
					return fmt.Errorf("ensure spicedb bootstrap: %w", err)
				}
			}
			if err := ensureSpiceDBEndpointConfig(ctx, bundle.REST); err != nil {
				return fmt.Errorf("ensure spicedb endpoint config: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// webd secrets + external-URL ConfigMap (kept alongside the SpiceDB
	// prerequisites, as before the migration).
	webdCli, err := client.New(bundle.REST, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return fmt.Errorf("build client for webd resources: %w", err)
	}
	if err := m.ensure(ctx, "webd secrets + external-URL ConfigMap", func(ctx context.Context) error {
		if err := ensurePassthroughLinkSigningKey(ctx, rep.Info, webdCli); err != nil {
			return fmt.Errorf("ensure passthrough link signing key: %w", err)
		}
		if err := ensureSlackOAuthSecret(ctx, rep.Info, webdCli); err != nil {
			return fmt.Errorf("ensure slack oauth secret: %w", err)
		}
		if err := ensureWebdExternalURLConfigMap(ctx, rep.Info, webdCli, r.trustedHostname != ""); err != nil {
			return fmt.Errorf("ensure webd external-url configmap: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// channelsd needs no install-time secret here — its memory token was created
	// in the prerequisite block above; it is applied + awaited as a pipeline
	// component below.

	// Postgres prerequisite: token Secret. The memory store connects using the
	// URI from the Secret.
	if err := m.ensure(ctx, "postgres token", func(ctx context.Context) error {
		if err := ensurePostgresToken(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure postgres token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// SpiceDB datastore_uri: only needed on the remote/postgres SpiceDB path
	// (external brings its own datastore; a profile that needs no bundled
	// Postgres — today, only `local` — runs SpiceDB on the in-memory datastore
	// engine with no DB Job at all).
	if !ext.enabled() && cloud.NeedsBundledPostgres(strat) {
		if err := m.ensure(ctx, "SpiceDB datastore URI", func(ctx context.Context) error {
			if err := ensureSpiceDBDatastoreURI(ctx, bundle.REST); err != nil {
				return fmt.Errorf("ensure spicedb datastore uri: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// Neo4j prerequisite: token Secret. The graph database backing Graphiti's
	// knowledge graph.
	if err := m.ensure(ctx, "neo4j token", func(ctx context.Context) error {
		if err := ensureNeo4jToken(ctx, bundle.REST); err != nil {
			return fmt.Errorf("ensure neo4j token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Graphiti prerequisite: config Secret (OpenAI API key). The KG entity
	// extraction service that builds the knowledge graph. The config ensure is
	// fatal (as before); graphiti's READINESS is optional and handled by the
	// pipeline component below, which warns + continues in a degraded state.
	var graphitiKeyConfigured bool
	if err := m.ensure(ctx, "graphiti config", func(ctx context.Context) error {
		configured, err := ensureGraphitiConfig(ctx, out, bundle.REST)
		if err != nil {
			return fmt.Errorf("ensure graphiti config: %w", err)
		}
		graphitiKeyConfigured = configured
		return nil
	}); err != nil {
		return err
	}
	// Surface the most common degraded-state cause up front (was previously
	// reported only after the graphiti readiness wait). Read from the Secret
	// ensureGraphitiConfig just reconciled, not the installer's local env: a
	// re-install can export OPENAI_API_KEY correctly while an earlier,
	// keyless install's empty Secret is what graphiti actually reads — the
	// local env alone would wrongly say "fixed".
	if !graphitiKeyConfigured {
		rep.Info("graphiti: KG extraction disabled until OPENAI_API_KEY is set")
	}

	// Stateful storage WORK runs HERE — past the plan-confirm gate and after the
	// base namespace exists (the probe runs in agentprimitives-system), but
	// BEFORE the pipeline applies the Postgres/Neo4j PVCs, whose storageClassName
	// (and the Neo4j volumeClaimTemplate) are immutable post-create.
	if err := runStatefulStorageWork(ctx, recheckCtx, statefulDec, bundle, rep); err != nil {
		return err
	}

	// ── Run the core data-plane components through the pipeline executor ──────
	// All prerequisite secrets above are written; the executor now applies every
	// component's manifests, then waits on all readiness concurrently (strict
	// secrets-all → apply-all → wait-all). The operator (patch-rolled earlier to
	// pick up the memory tokens) is awaited here as a WAIT-phase component via
	// deploymentRolledOut — there is no separate late operator-rollout phase, and
	// because webhook TLS was ensured above, that wait can never deadlock.
	// graphiti is Optional: a not-ready graphiti warns + continues (the core
	// memory stack works without it). comps was built above for the plan summary.
	deps := initpipeline.ExecDeps{
		Rep:     rep,
		Dynamic: bundle.Dynamic,
		Typed:   bundle.Typed,
		Apply: func(ctx context.Context, groups [][]byte) error {
			return applyManifestGroups(ctx, bundle.Dynamic, groups, imagePullSecret, firstPartyRefs, mirrorMap, func(id string) {
				rep.Info("applied %s", id)
			})
		},
		Recheck: recheckCtx, // SIGINT-cancelable, not timeout-bound
		// What this run applied. `oap install`'s interrupt handler reads it to
		// decide whether a Ctrl-C may offer a teardown at all; callers that did
		// not install a recorder get a throwaway set.
		OnApplied: appliedSetFrom(ctx).add,
	}
	if err := initpipeline.Run(ctx, comps, deps); err != nil {
		return err
	}

	// Restore the explicit degraded-state warning: graphiti is Optional so Run
	// never fails because of it, but a not-ready graphiti means knowledge-graph
	// and memory features are unavailable until it becomes ready. A single
	// readiness check immediately after Run surfaces the impact to the operator.
	if ready, err := deploymentReady(bundle.Typed, "agentprimitives-system", "spicebox-graphiti")(ctx); err != nil {
		rep.Warn("couldn't confirm graphiti readiness: %v", err)
	} else if !ready {
		rep.Warn("graphiti is not ready: knowledge-graph and memory features are degraded until graphiti is running")
	}

	// Same treatment for extractord: Optional so Run never fails because of it,
	// but a not-ready extractord means every inbound file attachment reports
	// "could not be retrieved" until it comes up — a failure mode with no other
	// install-time signal, so it is surfaced explicitly here rather than left
	// for the user to discover on their first attachment.
	if ready, err := deploymentReady(bundle.Typed, "agentprimitives-system", "agentprimitives-extractord")(ctx); err != nil {
		rep.Warn("couldn't confirm extractord readiness: %v", err)
	} else if !ready {
		rep.Warn("extractord is not ready: inbound file attachments cannot be extracted until extractord is running")
	}

	if pinningMode != "" {
		rep.Info("ensure cluster pinning posture")
		pinnCli, err := client.New(bundle.REST, client.Options{Scheme: kube.Scheme})
		if err != nil {
			return fmt.Errorf("build client for pinning settings: %w", err)
		}
		if err := ensureClusterPinningMode(ctx, rep.Info, pinnCli, pinningMode); err != nil {
			return fmt.Errorf("ensure cluster pinning mode: %w", err)
		}
	}

	// The workspace-storage DECISION (short-circuits + cloud Resolve + the
	// interactive Filestore cost-confirm prompt) was already made up-front into
	// wsChoice, before the data-plane came up. Here we only do the side-effecting
	// WORK that decision implies: emit its outcome message, apply the bundled
	// provisioner when needed, run the patient provisioning probe, then persist
	// the marker + patch the operator. No re-resolution and no re-prompting.
	rep.Info("resolve workspace storage")
	if wsChoice.Message != "" {
		rep.Info("%s", wsChoice.Message)
	}
	chosen := ""
	if wsChoice.NeedsBundled {
		rep.Info("apply bundled workspace provisioner")
		for _, d := range wsDocs {
			if err := mirrorImageRefs(d, mirrorMap); err != nil {
				return fmt.Errorf("mirror image refs: %w", err)
			}
			if err := mirrorWorkspaceHelperImage(d, mirrorMap); err != nil {
				return fmt.Errorf("mirror workspace helper image: %w", err)
			}
		}
		if err := applyWorkspaceProvisioner(ctx, wsDocs, bundle.Dynamic, bundle.Typed, kube.Apply); err != nil {
			rep.Warn("bundled workspace provisioner failed: %v", err)
			// §A3: never silently degrade — require explicit opt-out or interactive consent.
			degrade, ferr := handleBundledProvisionerFailure(err, wsOpts, r.assumeYes, isInteractive(), rep, os.Stdin, out)
			if ferr != nil {
				return ferr
			}
			if degrade {
				wsChoice.ClassName = "" // degrade to isolated /work per explicit choice
			}
		}
	}
	if wsChoice.ClassName != "" {
		if wsChoice.Probe {
			// The bundled local-path provisioner runs in nodePathMap mode, which
			// only provisions ReadWriteOnce (it rejects RWX outright). Its PVs carry
			// nodeAffinity, so RWO is single-node-shareable — the mode we actually
			// want. Probe the bundled class as RWO and wait for the consumer pod to
			// run (RWO can bind yet fail to attach). A managed/detected class is
			// still probed as RWX to confirm genuine cross-node ReadWriteMany.
			probeOpts := cloud.ProbeOptions{AccessMode: corev1.ReadWriteMany}
			if wsChoice.NeedsBundled {
				probeOpts = cloud.ProbeOptions{AccessMode: corev1.ReadWriteOnce, WaitForPodRunning: true}
			}
			probe, cleanup, perr := cloud.NewProvisioningProbe(ctx, bundle.Typed, wsChoice.ClassName, probeOpts)
			if perr != nil {
				return fmt.Errorf("probe workspace storage class %q: %w", wsChoice.ClassName, perr)
			}
			defer cleanup()
			c, perr := awaitProvisioningProbe(ctx, recheckCtx, "workspace storage "+wsChoice.ClassName, wsChoice.ClassName, probe, rep)
			if perr != nil {
				return perr
			}
			chosen = c
		} else {
			// Used directly: explicit override, cached marker, or an accepted
			// cost-confirm — already trusted up-front, no probe.
			chosen = wsChoice.ClassName
		}
	}
	if chosen != "" {
		if err := writeWorkspaceMarker(ctx, bundle.Typed, chosen); err != nil {
			rep.Warn("persist workspace marker: %v", err)
		}
	}
	if err := patchOperatorWorkspaceClass(ctx, bundle.Typed, chosen); err != nil {
		return fmt.Errorf("patch operator workspace flag: %w", err)
	}
	if developMode {
		// --web-dev was injected into the webd Deployment doc within the SSA
		// apply set above (injectWebDevIntoDocs), so there's no post-install
		// patch here — just remind the operator to run the Vite dev server. Routed
		// through the reporter, not fmt.Fprintln(os.Stderr): the checklist region is
		// live here, and on an interactive run stderr is the same terminal, so a raw
		// write displaces the cursor and corrupts the region exactly as an out write
		// would. It is narration, not an error. (audit2-tui NEW-4)
		rep.Info("--develop set — webd will serve UI from the Vite dev server; run `mage web:dev` in another terminal")
	}
	if chosen == "" {
		rep.Info("workspace storage: isolated (no RWX); multi-bundle agents use pod-local /work only")
	} else {
		rep.Info("workspace storage class: %s", chosen)
	}

	// External-access setup runs LAST: it synthesizes the webd Gateway + certs and
	// then waits (minutes) on the cloud load balancer to print the DNS records, so
	// it must not block the core service applies (SpiceDB etc.) above — doing so
	// previously consumed the whole install deadline and failed the SpiceDB apply.
	// validateWebdRouting already ran up-front.
	//
	// gatewayAddrReady tracks whether the Gateway load-balancer address was fully
	// obtained so the completion message can tell the operator whether the web UI
	// is immediately reachable or still waiting on DNS.
	// hostnameChangeDeclined is set when the user was prompted about a hostname
	// change and chose not to proceed; in that case the existing Gateway and DNS
	// records are left intact and the completion message must NOT advertise the
	// new (un-applied) hostname URL.
	// externalAccessSkipped is set when external access was skipped for any
	// reason (declined Gateway API enable, declined Envoy Gateway install, etc.);
	// the strategy already surfaced the reason, and the completion message defers to it.
	gatewayAddrReady := false
	hostnameChangeDeclined := false
	externalAccessSkipped := false

	// BEFORE the external-access block, because the failure this catches is one
	// where the install SUCCEEDS. A Gateway, a certificate and the real
	// hostnames all land, and a PublicEndpoint left by an earlier install then
	// re-applies the tunnel's address over the two ConfigMap keys under
	// ForceOwnership on its next reconcile, with nobody told. Refusing after
	// provisioning would leave that half-built; refusing here costs the
	// operator nothing but the flag they typed.
	//
	// installCreatesWebdPublicEndpoint cannot answer this: it decides whether
	// install CREATES one, from install's own flags, and an endpoint that is
	// already there is invisible to it. Both questions route through
	// cmd/oap/internal/publicendpoint, which is the one place this command may
	// name the type at all.
	if webdExternalURLHasAnotherOwner(r) {
		// Its proceed path narrates one line directly to out (cliout.Info) while
		// the checklist region is live; Suspend the region around the call so that
		// line lands in cleared space instead of corrupting the rail. The refusal
		// path returns an error (no direct write) and aborts as before. Same leaf
		// package as EnsureWebd below — Suspend rather than thread the reporter, for
		// the same reason. (audit2-tui NEW-3)
		var refuseErr error
		rep.Suspend(func(w io.Writer, _ io.Reader) {
			refuseErr = publicendpoint.RefuseWebdEndpointWhenURLIsClaimed(ctx, w, bundle.Controller, webdURLClaimFlag(r))
		})
		if refuseErr != nil {
			return refuseErr
		}
	}

	if r.trustedHostname != "" {
		// Ensure a Gateway controller + the Gateway API exist BEFORE anything reads
		// or applies Gateway/HTTPRoutes. On GKE this offers to enable the managed
		// Gateway API (and waits until served); on EKS/AKS/local it installs Envoy
		// Gateway. Doing this first means confirmWebdHostnameChange's HTTPRoute read
		// below can never hit the "no matches for gateway.networking.k8s.io/v1"
		// discovery error.
		bundledCtrl := envoyGatewayCloudComponent(bundle)
		res, gErr := strat.EnsureGatewayController(ctx, cloud.GatewayControllerParams{
			Clients:              cloudClients(bundle),
			Reporter:             newCloudReporterRep(rep),
			In:                   os.Stdin,
			AssumeYes:            r.assumeYes,
			BundledController:    &bundledCtrl,
			GatewayClassOverride: r.gatewayClass,
		})
		if gErr != nil {
			return gErr
		}
		if !res.Proceed {
			// EnsureGatewayController already surfaced why (declined / not enabled).
			externalAccessSkipped = true
		} else {
			proceed, cErr := confirmWebdHostnameChange(ctx, bundle.Controller, r, rep, os.Stdin, out, isInteractive())
			if cErr != nil {
				return cErr
			}
			if !proceed {
				rep.Info("skipping webd external-access reconfiguration; existing Gateway and DNS records left intact")
				hostnameChangeDeclined = true
			} else {
				var gaErr error
				gatewayAddrReady, gaErr = setupWebdExternalAccess(ctx, recheckCtx, rep, bundle, strat, r, res.GatewayClass)
				if gaErr != nil {
					return gaErr
				}
			}
		}
	} else if r.manualWebdRouting {
		rep.Info("  --manual-webd-routing: skipping ap-managed webd external access (configure routing + the spicebox-webd-external-url ConfigMap yourself)")
	}

	// The tunnel counterpart of the block above, and genuinely its mutual
	// exclusive — enforced, not merely asserted. The whole condition lives in
	// installCreatesWebdPublicEndpoint so it is testable across both of its
	// inputs; see there for why the routing flags and the cluster kind are
	// independent questions and both must be asked.
	//
	// spec.localURL is webdLocalSeedURL — the SAME constant
	// ensureWebdExternalURLConfigMap seeded the ConfigMap with earlier in this
	// install, on exactly the no-hostname path this branch runs on — so the
	// controller's first write carries the value already stored and webd never
	// sees the URL change.
	//
	// Runs after external access rather than beside the other ensures so the
	// CRDs applied in the base region are long since established, and so a
	// failure here cannot cost the core install.
	if installCreatesWebdPublicEndpoint(r, strat) {
		if err := m.ensure(ctx, "public endpoint (tunnel)", func(ctx context.Context) error {
			// EnsureWebd lives in the leaf publicendpoint package and narrates
			// directly to its io.Writer (cliout.Info) — the tunnel-credential hint
			// is among those lines. Threading the reporter into it would ripple
			// through its desktop and `oap agent install` callers too, so instead
			// quiesce the live checklist region around the call: Suspend clears the
			// region, EnsureWebd's lines land in that cleared space (kept in
			// scrollback, not stranded mid-rail), and the region is redrawn when it
			// returns. Without this those direct writes displace the cursor below
			// the region and the completion summary's next printAbove erases the
			// hint. (audit2-tui NEW-1)
			var ensureErr error
			rep.Suspend(func(w io.Writer, _ io.Reader) {
				ensureErr = publicendpoint.EnsureWebd(ctx, w, bundle.Controller, strat, webdLocalSeedURL, nil)
			})
			return ensureErr
		}); err != nil {
			return err
		}
	}

	// The agent-builder workshop bundle: a locked-down AgentClass + its
	// sidecar toolbox into the fixed agentprimitives-system namespace, so
	// every cluster gets exactly one, at exactly this identity, with no
	// --name/--namespace override to reconcile later. --without-builder is
	// the explicit opt-out; absent that, --builder-starters is REQUIRED — the
	// bundle's one install question has no safe default (an empty
	// allowedStarters plus onlyStartersInteract locks everyone, including the
	// installer, out of a class nobody can then fix from inside the UI), so a
	// missing answer is a hard error here rather than a silent skip.
	if !withoutBuilder {
		if len(builderStarters) == 0 {
			return fmt.Errorf("the agent builder needs at least one --builder-starters identity (or pass --without-builder to skip it)")
		}
		if err := m.ensure(ctx, "agent builder", func(ctx context.Context) error {
			if _, err := applyBuilderBundle(ctx, bundle.Controller, builderStarters); err != nil {
				return err
			}
			return seedBuilderClass(ctx, bundle.Controller)
		}); err != nil {
			return err
		}
	}

	// Completion summary — a few lines that tell the operator they are done and
	// what to do next. Runs only past all dry-run guards (those returned earlier)
	// so it never appears under --dry-run.
	printInstallCompletionSummary(rep, r.trustedHostname, gatewayAddrReady, hostnameChangeDeclined, externalAccessSkipped)

	return nil
}

// printInstallCompletionSummary emits the "all set" completion lines to rep.
// trustedHostname, gatewayAddrReady, hostnameChangeDeclined, and
// externalAccessSkipped together drive the URL hint:
//   - externalAccessSkipped: external access was skipped (reason already printed by the strategy); skip message, no URL.
//   - hostnameChangeDeclined: the user declined a hostname change, so the new
//     hostname was never applied to the Gateway; don't advertise it.
//   - trustedHostname set + declined=false + addrReady: print the live URL.
//   - trustedHostname set + declined=false + !addrReady: print with DNS caveat.
//   - trustedHostname empty: print the generic "oap --help" next-step.
func printInstallCompletionSummary(rep progress.Reporter, trustedHostname string, gatewayAddrReady, hostnameChangeDeclined, externalAccessSkipped bool) {
	rep.OK("✓ all set — your agent platform is ready.")
	switch {
	case externalAccessSkipped:
		rep.Info("  webd external access was skipped; the core install is complete — see the note above for how to enable it, then re-run `oap init`.")
	case hostnameChangeDeclined:
		rep.Info("  webd external access unchanged — still serving the previously-configured hostname")
	case trustedHostname != "":
		trustedURL := "https://" + trustedHostname
		if gatewayAddrReady {
			rep.Info("  open the web UI: %s", trustedURL)
		} else {
			rep.Info("  open the web UI: %s (once DNS resolves to the Gateway address)", trustedURL)
		}
	default:
		rep.Info("  next: run `oap --help` to explore commands.")
		// Says WHICH address, because webd answers on the one it advertises and
		// a port-forward to any other 404s every route. On a kind that serves
		// both origins from one host the loopback keeps answering after a
		// public endpoint takes the advertised address over (see
		// webui.ServeHTTP's shared-origin dispatch), so both are offered rather
		// than one of them going quietly stale.
		rep.Info("  the web UI answers at the address webd advertises — %s unless you have changed it, "+
			"which a port-forward to %s:%d gives you. A public endpoint's tunnel takes that advertised "+
			"address over; the local one keeps serving alongside it.",
			webdLocalSeedURL, cloud.WebdServiceName, cloud.WebdServicePort)
	}
}

// dockerLookPath resolves the docker CLI. It is a package var (defaulting to
// exec.LookPath) so preflight's build-tool check can be exercised in tests
// without a real docker install.
var dockerLookPath = exec.LookPath

// unwedgeNamespaceWait is the maximum time to wait for a Terminating namespace
// to disappear after calling UnwedgeTerminatingNamespace. A package-level var
// so tests can shorten it without sleeping the full 30 s.
var unwedgeNamespaceWait = 30 * time.Second

// unwedgeLeftoverNamespace handles a prior install wedge: if the operator
// namespace still exists with a deletionTimestamp (stuck Terminating), run the
// cloud's unwedge routine and wait for it to clear before the install proceeds.
// A namespace that can't be cleared returns an error with the manual fix —
// never a silent attempt to create resources into a Terminating namespace (which
// fails with confusing API errors).
//
//   - Namespace absent → return nil (clean slate).
//   - Namespace exists, no deletionTimestamp → return nil (normal reinstall).
//   - Namespace Terminating → call strat.UnwedgeTerminatingNamespace, then poll
//     until it disappears (up to unwedgeNamespaceWait); nil on success, error
//     (containing the namespace name and any manual-fix commands) on timeout.
func unwedgeLeftoverNamespace(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, rep progress.Reporter, ns string) error {
	got, err := bundle.Typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil // clean slate — nothing to unwedge
	}
	if err != nil {
		return fmt.Errorf("check namespace %s: %w", ns, err)
	}
	if got.DeletionTimestamp == nil {
		return nil // active namespace — normal reinstall path handles it
	}

	rep.Warn("namespace %s is stuck Terminating from a prior teardown — attempting to unwedge", ns)
	report, uerr := strat.UnwedgeTerminatingNamespace(ctx, cloudClients(bundle), newCloudReporterRep(rep), ns, true)
	if uerr != nil {
		return fmt.Errorf("unwedge namespace %s: %w", ns, uerr)
	}
	for _, c := range report.FinalizersCleared {
		rep.OK("  cleared finalizer on %s", c)
	}

	// Poll until the namespace disappears or the budget elapses.
	waitCtx, cancel := context.WithTimeout(ctx, unwedgeNamespaceWait)
	defer cancel()
	for {
		_, gerr := bundle.Typed.CoreV1().Namespaces().Get(waitCtx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(gerr) {
			rep.OK("  namespace %s cleared; proceeding", ns)
			return nil
		}
		select {
		case <-waitCtx.Done():
			msg := fmt.Sprintf("namespace %s is still Terminating and could not be unwedged automatically.", ns)
			if len(report.ManualCommands) > 0 {
				msg += " Run these commands, then retry `oap install`:\n  " + strings.Join(report.ManualCommands, "\n  ")
			}
			return fmt.Errorf("%s", msg)
		case <-time.After(time.Second):
		}
	}
}

// preflight runs a few high-value, read-only "doctor" checks before oap makes any
// change to the cluster, each with a plain-language message and the fix to try
// on failure (§A8: dead-simple for non-technical users). It is invoked only past
// the --dry-run guards, so it never runs under --dry-run. A hard failure (the
// cluster is unreachable, or docker is missing when this run needs it) returns
// the error so the install aborts before anything is applied; benign findings
// are surfaced through rep and the install continues. All output flows through
// rep so the preflight reads like the rest of the install checklist.
//
// building, when true, means this run relies on locally-built first-party images
// (local image mode); in that case preflight also verifies the docker CLI is
// installed, since that is what produces/loads those images.
func preflight(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy, rep progress.Reporter, building bool) error {
	rep.Info("preflight checks")

	// Cluster reachable: a lightweight discovery call (no writes). If the API
	// server can't be reached there is nothing to install onto — abort now, with
	// the plain fix, before touching the cluster.
	if _, err := bundle.Typed.Discovery().ServerVersion(); err != nil {
		rep.Warn("can't reach a Kubernetes cluster — check your kubeconfig/context (try `kubectl cluster-info`).")
		return fmt.Errorf("can't reach a Kubernetes cluster — check your kubeconfig/context is pointed at a running cluster. (%v)", err)
	}
	rep.OK("  cluster reachable")

	// Cloud detected: surface what cloud.Detect found so the operator can sanity-
	// check it (a wrong context tends to show up here as the wrong cloud). The
	// "(local-style install)" note is keyed off the install profile, not the
	// kind: it fires for strategies that use local :dev images with no digest
	// pinning (today, only the opt-in `local` kind — which cloud.Detect can
	// never return, see cloud.Detect's doc comment). It must NOT key off
	// strat.Key() == cloud.KeyDefault: since the local/unmanaged split, that
	// key names the unmanaged fallback (on-prem, bare-metal, kind), a real
	// cluster carrying the full production profile — a benign note either way,
	// not a failure, since local, unmanaged, and unknown clusters are all fine
	// to install on.
	if strat.InstallProfile().UsesLocalDevImages() {
		rep.Info("  detected: %s (local-style install)", strat.DisplayName())
	} else {
		rep.Info("  detected: %s cluster", strat.DisplayName())
	}

	// A prior teardown can leave agentprimitives-system stuck Terminating (e.g. on
	// GKE, the NEG finalizer outlives the namespace and blocks termination). Creating
	// resources into a Terminating namespace fails with confusing API errors, so we
	// unwedge it (or fail fast with manual-fix commands) before applying anything.
	if err := unwedgeLeftoverNamespace(ctx, bundle, strat, rep, cloud.WebdServiceNamespace); err != nil {
		rep.Warn("%v", err)
		return err
	}

	// Docker present: only checked when this run will build/load local images
	// (building). A missing docker there can never produce the :dev images the
	// install references, so fail now with the plain fix instead of letting the
	// pods land in ImagePullBackOff minutes later.
	if building {
		if _, err := dockerLookPath("docker"); err != nil {
			rep.Warn("Docker isn't installed (or not on your PATH) — it builds and loads the local agent images this install uses. Install it from https://docs.docker.com/get-docker/, then re-run.")
			return fmt.Errorf("docker not found — install Docker (https://docs.docker.com/get-docker/) and make sure the `docker` command is on your PATH, then re-run. (%v)", err)
		}
		rep.OK("  docker present")
	}
	return nil
}

// componentPlanDescriptions maps a core-component Name (from buildCoreComponents)
// to a plain-language, non-technical description for the plan summary printed by
// confirmPlan. A component absent from this map is omitted from the summary.
var componentPlanDescriptions = map[string]string{
	"postgres":        "PostgreSQL — agent memory store",
	"neo4j":           "Neo4j — knowledge-graph store",
	"NATS":            "NATS — internal message bus",
	"SpiceDB":         "SpiceDB — authorization",
	"SpiceDBOperator": "SpiceDB operator — manages the SpiceDBCluster",
	"SpiceDBDatabase": "SpiceDB database — ensures the 'spicedb' DB in Postgres",
	"channelsd":       "channel transports (Slack, …)",
	"graphiti":        "Graphiti — knowledge-graph extraction (optional)",
	"operator":        "the agent operator",
	"webd":            "the web UI",
	"authzd":          "the authorization service",
}

// confirmPlan prints a plain-language summary of what the install will set up
// (each core component mapped to a friendly description, the external-access
// origin when configured, and a rough time estimate), then — only on an
// interactive TTY without --yes — asks for a final go/no-go (§A8). It returns
// proceed=true for --yes / --defaults / a non-TTY (automation must never block
// on a prompt) and on an explicit "yes"; proceed=false only when an interactive
// user declines. confirmPlan performs no cluster writes, so a decline lets the
// caller return cleanly with nothing applied. It returns (bool, error) — the
// same shape as confirmWebdHostnameChange — so callers handle it uniformly; the
// error is reserved for future plan-validation and is nil today.
func confirmPlan(comps []initpipeline.Component, r WebdRoutingOpts, rep progress.Reporter, in io.Reader, out io.Writer, assumeYes, isTTY bool) (bool, error) {
	rep.Info("Here's what oap will set up:")
	for _, c := range comps {
		if desc, ok := componentPlanDescriptions[c.Name]; ok {
			rep.Info("  • %s", desc)
		}
	}
	if r.trustedHostname != "" {
		rep.Info("  • Secure HTTPS at https://%s", r.trustedHostname)
		rep.Info("Estimated time: ~3–7 minutes (the cloud load balancer is the slow part).")
	} else {
		rep.Info("Estimated time: ~2–5 minutes.")
	}

	// A plan the caller already confirmed (the init wizard's own Proceed step),
	// --yes / --defaults, and a non-TTY all proceed without prompting so neither
	// the wizard re-asks nor automation is blocked. Only an interactive user who
	// has not already answered is asked; declining returns false.
	if r.preconfirmed || assumeYes || !isTTY {
		return true, nil
	}
	return apcmd.Confirm(in, out, "Proceed with the install?", assumeYes, isTTY), nil
}

// setupWebdExternalAccess sets up webd's external access end to end and returns
// (gotAddr, err). gotAddr is true when the Gateway load-balancer address was
// successfully obtained; false when the wait timed out and a placeholder was
// printed instead. The caller uses gotAddr to tailor the completion message.
//
// TLS dispatch goes through strat.TLS() — one method on the registered
// cloud.Strategy — so this consumer carries no per-cloud branch: a new per-cloud
// TLS flow is a new cloud.Strategy.TLS() implementation, not a new case here.
//
// All user-facing output here narrates through rep (the install progress
// reporter / checklist UI) so the cloud/gateway flow stays visually consistent
// with the data plane — no raw streaming to a separate writer. The cloud-layer
// helpers (the TLS strategy, gatewayhealth) get a rep-backed
// cloud.Reporter via newCloudReporterRep.
func setupWebdExternalAccess(ctx, recheckCtx context.Context, rep progress.Reporter, bundle *kube.Bundle, strat cloud.Strategy, r WebdRoutingOpts, gwClass string) (bool, error) {
	// The Gateway controller + API are already ensured by EnsureGatewayController
	// (called before confirmWebdHostnameChange); gwClass is the resolved
	// GatewayClass to bind the webd Gateway to.

	// Get the cloud's TLS strategy from the registered cloud.Strategy. This is
	// never nil — every cloud.Strategy implementation must return a valid TLS
	// strategy (the local/default strategy returns certmanager.Strategy).
	tlsStrat := strat.TLS()

	// The Google-managed strategy needs the GCP project; derive it from a node's
	// providerID (same source as the registry derivation). Cheap + harmless for
	// other clouds (returns "").
	project, err := deriveCloudProject(ctx, bundle, strat)
	if err != nil {
		return false, fmt.Errorf("derive cloud project for TLS strategy: %w", err)
	}

	gwTLS, extraDNS, proceed, err := tlsStrat.Prepare(ctx, cloud.PrepareParams{
		Reporter:        newCloudReporterRep(rep),
		Clients:         cloudClients(bundle),
		Cloud:           strat.Key(),
		TrustedHostname: r.trustedHostname,
		SandboxHostname: r.sandboxHostname,
		Project:         project,
		ACMEEmail:       r.acmeEmail,
		TLSIssuer:       r.tlsIssuer,
		AssumeYes:       r.assumeYes,
	})
	if err != nil {
		return false, fmt.Errorf("prepare %s TLS: %w", tlsStrat.Name(), err)
	}
	if !proceed {
		// The strategy declined (e.g. cert-manager not installed) and already
		// surfaced a warning. Skip the rest of external-access setup.
		return false, nil
	}

	// The synthesis below is a fast sequence of applies (Gateway, TLS finalize,
	// HTTPRoutes, ingress/health policies) with no readiness wait, so it narrates
	// through rep.Info rather than becoming a Phase row — the meaningful wait (the
	// load-balancer address) is the Phase row below.
	rep.Info("synthesize webd Gateway + HTTPRoutes")
	if err := cloud.ApplyWebdGateway(ctx, cloudClients(bundle), gwClass, r.trustedHostname, r.sandboxHostname, gwTLS); err != nil {
		return false, fmt.Errorf("apply webd Gateway: %w", err)
	}

	// Finalize the TLS flow now the Gateway exists (cert-manager applies the
	// per-hostname Certificates that fill the listener Secrets; google-managed is
	// a no-op — its cert is attached by the Gateway annotation above).
	if err := tlsStrat.Complete(ctx, cloud.CompleteParams{
		Reporter:        newCloudReporterRep(rep),
		Clients:         cloudClients(bundle),
		TrustedHostname: r.trustedHostname,
		SandboxHostname: r.sandboxHostname,
		TLSIssuer:       r.tlsIssuer,
	}); err != nil {
		return false, fmt.Errorf("complete %s TLS: %w", tlsStrat.Name(), err)
	}

	if err := applyWebdHTTPRoute(ctx, bundle.Controller, webdTrustedRouteName, r.trustedHostname); err != nil {
		return false, fmt.Errorf("apply trusted HTTPRoute: %w", err)
	}
	trustedURL := "https://" + r.trustedHostname
	sandboxURL := ""
	if r.sandboxHostname != "" {
		if err := applyWebdHTTPRoute(ctx, bundle.Controller, webdSandboxRouteName, r.sandboxHostname); err != nil {
			return false, fmt.Errorf("apply sandbox HTTPRoute: %w", err)
		}
		sandboxURL = "https://" + r.sandboxHostname
	}

	// Allow the cloud managed Gateway's LB source ranges to reach webd. Its L7
	// health checks arrive from the LB ranges (not the node), so default-deny
	// would otherwise drop them and the backend serves 503 despite a Ready pod.
	if err := cloud.ApplyGatewayBackendIngressPolicy(ctx, cloudClients(bundle), strat.GatewayBackendIngressCIDRs()); err != nil {
		return false, fmt.Errorf("allow webd Gateway ingress: %w", err)
	}

	// Provision the cloud's backend health check now the HTTPRoutes reference the
	// webd Service (the backend exists). Dispatched through gatewayhealth.For —
	// a REGISTRY — so this consumer carries no per-cloud branch: GKE applies a
	// HealthCheckPolicy pointing the managed L7 probe at /healthz (GKE's Gateway
	// API doesn't infer the check from the readiness probe, so it defaults to "/"
	// which webd 404s → 503); other clouds (Envoy Gateway) get the default no-op.
	// Sibling of ApplyGatewayBackendIngressPolicy above: that opens the LB
	// health-check source ranges to webd, this points the probe at the right path.
	if err := gatewayhealth.For(strat.Key()).Provision(ctx, gatewayhealth.Params{Reporter: newCloudReporterRep(rep), Bundle: bundle}); err != nil {
		return false, fmt.Errorf("provision Gateway backend health check: %w", err)
	}

	// Wait for the load balancer address so we can print the exact DNS records.
	// The TLS strategy already provisioned the cert machinery the controller
	// needs to program the LB, but cloud LBs still take a few minutes (GKE's first
	// global L7 provision routinely exceeds the initial window). A timeout here is
	// NOT silently swallowed: we warn that the wait gave up and — at a terminal —
	// let the operator keep watching rather than guess. CI / --yes / a non-TTY take
	// the non-blocking placeholder fallback, but only AFTER the warning, so the
	// give-up is never silent.
	//
	// The progress reporter handles the keep-waiting loop + assumeYes fast-exit
	// internally, so the bare cliout.Await + manual loop is replaced by a single
	// Phase.Await call. The phase is non-fatal: on timeout we warn + print the
	// placeholder, but do NOT return the error.
	// addrWait is the initial poll budget for the Gateway load-balancer address;
	// addrETA is the expected time-to-ready that drives the soft-warn. Both are
	// cloud-specific (the Strategy owns the value): GKE's managed global external
	// L7 routinely takes ~10m for a first provision — the LB frontend (forwarding
	// rules + target proxy, and thus the address oap polls for) lags the reserved
	// address by several minutes — so it asks for a larger budget, while the
	// bundled-Envoy clouds come up faster and keep a tighter one so a genuine
	// stall there surfaces promptly.
	addrWait, addrETA := strat.GatewayAddressWait()
	var addr string
	gotAddr := false
	phaseAddr := rep.Phase("webd gateway address")
	if err := phaseAddr.Await(ctx, recheckCtx, addrWait, addrETA,
		func(ctx context.Context) (bool, error) {
			a, present, aerr := wait.GatewayAddress(ctx, bundle.Controller, cloud.WebdServiceNamespace, cloud.WebdGatewayName)
			if present {
				addr = a
			}
			return present, aerr
		},
		// Diagnose a stalled address: surface the Gateway's blocking conditions
		// and, when GKE's managed-gateway program is wedged (a stuck duplicate
		// GCE operation leaving the backend service "not ready"), flag it
		// explicitly instead of letting the wait silently burn its budget.
		func(ctx context.Context) (wait.Diagnosis, error) {
			return wait.DiagnoseGatewayAddress(ctx, bundle.Controller, cloud.WebdServiceNamespace, cloud.WebdGatewayName)
		},
	); err != nil {
		phaseAddr.Fail()
		if errors.Is(err, context.DeadlineExceeded) {
			rep.Warn("the Gateway load-balancer address still isn't assigned (waited %s); GKE's first provision can take several more minutes.", addrWait)
		} else if strings.Contains(err.Error(), "waiting was stopped") {
			rep.Warn("stopped waiting for the Gateway load-balancer address; printing DNS records with a placeholder.")
		} else {
			rep.Warn("couldn't read the Gateway load-balancer address: %v", err)
		}
		rep.Info("  printing the DNS records with a placeholder; fill in the address once the load balancer is up.")
	} else {
		gotAddr = true
		phaseAddr.Done()
	}
	target := addr
	if !gotAddr {
		target = "<GATEWAY-ADDRESS>"
	}
	// The DNS records are the actionable deliverable, but they must still flow
	// through rep — a raw write to a separate writer would desync the checklist's
	// live region. rep.Info preserves the column alignment verbatim and scrolls it
	// into the scrollback above the live rows.
	rep.Info("  Create these DNS records (both hostnames point at the one Gateway address):")
	rep.Info("    %-30s A   %s", r.trustedHostname, target)
	if r.sandboxHostname != "" {
		rep.Info("    %-30s A   %s", r.sandboxHostname, target)
	}
	// Print any extra validation records the strategy returned (e.g. the
	// Certificate Manager DNS-authorization CNAMEs for Google-managed certs)
	// under their own header — they validate the cert, they don't route traffic,
	// so lumping them with the A records above misleads.
	if len(extraDNS) > 0 {
		rep.Info("  ...and these certificate-validation records (so the managed cert can issue + auto-renew):")
		for _, rec := range extraDNS {
			rep.Info("    %-42s %-5s %s", rec.Name, rec.Type, rec.Data)
		}
	}
	if !gotAddr {
		rep.Info("  (load balancer still provisioning — get its address with:\n     kubectl get gateway %s -n %s -o wide)", cloud.WebdGatewayName, cloud.WebdServiceNamespace)
	}
	if !strat.IsManaged() {
		rep.Info("  note: Let's Encrypt cannot validate a non-public host; for local TLS use `oap init --local` (ngrok).")
	}

	// Best-effort: the external-url ConfigMap records webd's public URLs. A
	// failure here (e.g. API client throttling under a slow LB wait) must not
	// fail the whole install — the core stack is already up.
	if err := patchWebdExternalURLs(ctx, bundle.Controller, trustedURL, sandboxURL); err != nil {
		rep.Warn("could not set webd external URL: %v", err)
		rep.Info("  (non-fatal; re-run oap init once the Gateway is ready.)")
	}
	return gotAddr, nil
}

// deriveCloudProject reads a node's spec.providerID and returns the cloud's
// project ID (GKE: the GCP project) for TLS strategies that need it; "" for
// clouds with no derivable project. It is best-effort about a node-less cluster
// only in that an empty providerID yields "" — a List failure is returned so the
// caller surfaces it rather than silently proceeding with no project.
func deriveCloudProject(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy) (string, error) {
	_, providerID, found, err := imagemode.FirstNodeInfo(ctx, bundle.Typed)
	if err != nil {
		return "", fmt.Errorf("inspect nodes: %w", err)
	}
	if !found {
		return "", nil
	}
	return strat.ProjectFromProviderID(providerID), nil
}

// operatorMemoryTokenSecrets are the per-component memory-token Secrets the
// operator mounts and reads once at startup. See rollOperatorIfPredatesMemoryTokens.
var operatorMemoryTokenSecrets = []string{
	"spicebox-channelsd-memory-token",
	"spicebox-webd-memory-token",
	"agentprimitives-authzd-memory-token",
}

// rollOperatorIfPredatesMemoryTokens rolls the operator Deployment when its
// running pod started before any of the memory-token Secrets it mounts, so a
// fresh pod remounts them as real files (see the call site for the subPath race
// this closes). It is a no-op when the operator already started after the
// tokens existed — a healthy re-run does not churn the operator.
func rollOperatorIfPredatesMemoryTokens(ctx context.Context, rep progress.Reporter, typed kubernetes.Interface) error {
	const ns = "agentprimitives-system"

	// Newest token-Secret creation time: the operator pod must have started
	// after this to have read every token.
	var newestSecret time.Time
	for _, name := range operatorMemoryTokenSecrets {
		s, err := typed.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get memory-token secret %s: %w", name, err)
		}
		if t := s.CreationTimestamp.Time; t.After(newestSecret) {
			newestSecret = t
		}
	}
	if newestSecret.IsZero() {
		return nil // no token Secrets present; nothing to roll for
	}

	pods, err := typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=spicebox-operator",
	})
	if err != nil {
		return fmt.Errorf("list operator pods: %w", err)
	}
	var newestPod time.Time
	var running bool
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil || p.Status.StartTime == nil {
			continue
		}
		running = true
		if t := p.Status.StartTime.Time; t.After(newestPod) {
			newestPod = t
		}
	}
	if !running {
		// No live operator pod yet; whatever starts next mounts the now-present
		// Secrets correctly. Nothing to roll.
		return nil
	}
	if !newestPod.Before(newestSecret) {
		return nil // operator already started after the tokens existed
	}

	// Trigger the rollout (patch only) — do NOT wait here. The operator only
	// becomes Ready once webhook-TLS + its memory-token Secrets exist and its
	// runtime deps (NATS/SpiceDB/postgres) are up, none of which is guaranteed
	// at this early point. Waiting here deadlocks (webhook-TLS is created later).
	// The readiness wait runs last, after the core components are up.
	rep.Info("rolling operator to pick up memory tokens (pod predates secrets)")
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"ap.authzed.com/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339Nano)))
	if _, err := typed.AppsV1().Deployments(ns).Patch(ctx, "spicebox-operator",
		types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("rollout-restart operator: %w", err)
	}
	return nil
}

// resourceID formats kind+namespace+name for output, omitting the namespace
// segment for cluster-scoped resources (so it reads "ClusterRole spicebox-operator"
// rather than "ClusterRole /spicebox-operator").
func resourceID(kind, namespace, name string) string {
	if namespace == "" {
		return kind + " " + name
	}
	return kind + " " + namespace + "/" + name
}

// deploymentReady returns a Poll that reports whether the named Deployment has at
// least one ready replica. Used by Phase.Await for component readiness checks.
func deploymentReady(typed kubernetes.Interface, ns, name string) progress.Poll {
	return func(ctx context.Context) (bool, error) {
		d, err := typed.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		}
		return d.Status.ReadyReplicas >= 1, nil
	}
}

// deploymentRolledOut returns a Poll that reports whether the named Deployment
// has fully rolled out its current pod template — the equivalent of
// `kubectl rollout status`: observed generation caught up, every desired replica
// updated and available, and no old (surge) replicas linger.
func deploymentRolledOut(typed kubernetes.Interface, ns, name string) progress.Poll {
	return func(ctx context.Context) (bool, error) {
		d, err := typed.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		}
		if d.Generation != d.Status.ObservedGeneration {
			return false, nil // controller hasn't observed the new spec yet
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		return d.Status.UpdatedReplicas == want &&
			d.Status.Replicas == want && // no old/surge replicas remain
			d.Status.AvailableReplicas == want, nil
	}
}

// statefulSetReady returns a Poll that reports whether the named StatefulSet has
// at least one ready replica. Used by Phase.Await for component readiness checks.
func statefulSetReady(typed kubernetes.Interface, ns, name string) progress.Poll {
	return func(ctx context.Context) (bool, error) {
		s, err := typed.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get statefulset %s/%s: %w", ns, name, err)
		}
		return s.Status.ReadyReplicas >= 1, nil
	}
}

// injectStatefulClass loads a component's embedded manifests and pins every PVC
// / StatefulSet template among them to the resolved RWO class. A "" class is a
// pass-through (trust the cluster default). The per-document mutation is a no-op
// on non-PVC/non-STS docs (Service, Deployment), so mapping over all of them is
// safe.
func injectStatefulClass(load func() ([][]byte, error), class string) ([][]byte, error) {
	docs, err := load()
	if err != nil {
		return nil, err
	}
	if class == "" {
		return docs, nil
	}
	for i := range docs {
		m, err := manifests.InjectStorageClass(docs[i], class)
		if err != nil {
			return nil, fmt.Errorf("inject storageClassName=%q: %w", class, err)
		}
		docs[i] = m
	}
	return docs, nil
}

// buildCoreComponents returns the declarative pipeline Components for the core
// data-plane services (NATS, SpiceDB, channelsd, postgres, neo4j, graphiti,
// operator, webd, authzd). Their install-time Secrets are written imperatively
// by RunInstall BEFORE this set is handed to initpipeline.Run, so the
// Components carry only Manifests + Wait. The executor runs SECRETS as a global
// phase, then drives the components as a dependency DAG: each component's
// apply+wait starts only once every component it DependsOn has reached READY.
//
// channelsd, operator, webd, authzd, and graphiti carry DependsOn edges so their
// own apply+wait open only after their runtime deps are Ready — e.g. channelsd
// applies and awaits only once NATS and SpiceDB are up, rather than
// crash-looping until then. Independent components (NATS, SpiceDB, postgres,
// neo4j) run fully in parallel; graphiti's optional AwaitOptional gates only its
// own dependent-less tail, and is short-circuited once all required components
// finish.
// The operator, webd, and authzd carry no Manifests — they ship in the base
// bundle applied earlier (operator was also patch-rolled in RunInstall); here
// they are only awaited. graphiti is Optional, so a not-ready graphiti warns +
// continues rather than failing the install.
// The ETAs below are EXPECTATIONS, not deadlines: each drives the "~Xm expected"
// hint and the one-time "taking longer than expected" soft-warn, while the hard
// give-up window is derived from it (deadlineGrace × ETA — see
// initpipeline.waitDeadline). Size them as the honest warm-node estimate; a
// component that needs materially more than a multiple of its expectation wants
// an explicit WaitSpec.Deadline, not an inflated ETA — inflating the ETA buys
// the deadline at the cost of a warning that no longer warns in time.

// dataPlaneETA is the expected time-to-ready for standard Deployments in the
// core data-plane stack. Most single-replica Deployments pull their image once
// and start within ~60–90 s on a warm node; 90 s is a conservative estimate
// that avoids spurious "taking longer than expected" warns on cold nodes.
const dataPlaneETA = 90 * time.Second

// postgresETA is the data-plane ETA for PostgreSQL specifically. Unlike the
// stateless single-replica Deployments dataPlaneETA covers, postgres is a data
// store: on a fresh cluster its cold-start serializes local-path PVC
// provisioning, a ~155 MB pgvector/pgvector image pull, and first-time initdb,
// which routinely exceeds the 90 s dataPlaneETA. 3 m is the realistic
// expectation for that sequence.
const postgresETA = 3 * time.Minute

// statefulSetETA is the expected time-to-ready for StatefulSets (NATS, neo4j)
// and the operator rollout (which uses the stricter deploymentRolledOut poll).
// StatefulSets boot sequentially and allocate PVCs, so 2 m is a realistic
// baseline on a healthy cluster — neo4j on a warm node lands just inside it
// (~10 s image pull, ~60 s store creation + JVM boot behind a readinessProbe
// with initialDelaySeconds: 30), and the cold case is the deadline's job.
const statefulSetETA = 2 * time.Minute

// graphitiDeadline is graphiti's explicit hard wait window. It is the one
// component that opts out of the derived deadlineGrace × ETA: it is gated on
// neo4j, so by the time its wait opens the database it dials at startup is
// already Ready, and the cold-dependency case the grace exists to cover is gone.
// A gated optional also waits under the run context rather than the optional-tail
// short-circuit, so this number is exactly how long a broken graphiti can hold an
// otherwise-finished install.
const graphitiDeadline = 2 * time.Minute

// buildCoreComponents assembles the core data-plane component list for the
// install pipeline. SpiceDB is no longer a static Deployment/Service the
// pipeline applies directly: unless ext.enabled() (the user brought their own
// instance), it is managed by the authzed spicedb-operator (shipped in the
// base bundle) via a SpiceDBCluster CR — see "SpiceDBOperator" (waits on the
// operator Deployment), "SpiceDBDatabase" (remote-only Job that creates the
// 'spicedb' Postgres database ahead of the operator's own migration Job), and
// "SpiceDB" (applies the CR and waits on the operator-created Deployment).
// strat's InstallProfile selects the CR's datastore engine (memory for
// `local`, postgres otherwise); ext.enabled() omits all three components
// entirely, and every other component's DependsOn is stripped of "SpiceDB" so
// the DAG validator never sees a reference to an absent component. When
// ext.enabled() and the caller's instance is reached over TLS (!ext.Insecure),
// the channelsd component's Manifests additionally flip channelsd's
// --spicedb-insecure default to false (see appendChannelsdInsecureFlag) — the
// operator/authzd/webd Deployments are flipped separately in RunInstall, on
// baseDocs, via flipSpiceDBInsecureForExternalTLS, since they ship in the base
// bundle and carry no Manifests here.
func buildCoreComponents(bundle *kube.Bundle, tags manifests.Tags, statefulClass string, strat cloud.Strategy, ext ExternalSpiceDB) []initpipeline.Component {
	typed := bundle.Typed
	const ns = "agentprimitives-system"
	const spicedbOperatorNS = "spicedb-operator"
	depWait := func(name string) *initpipeline.WaitSpec {
		return &initpipeline.WaitSpec{
			Poll: deploymentReady(typed, ns, name),
			Diagnose: func(ctx context.Context) (wait.Diagnosis, error) {
				return wait.DiagnoseDeployment(ctx, typed, ns, name)
			},
			ETA: dataPlaneETA,
		}
	}
	stsWait := func(name string) *initpipeline.WaitSpec {
		return &initpipeline.WaitSpec{
			Poll: statefulSetReady(typed, ns, name),
			Diagnose: func(ctx context.Context) (wait.Diagnosis, error) {
				return wait.DiagnoseStatefulSet(ctx, typed, ns, name)
			},
			ETA: statefulSetETA,
		}
	}
	// pgWait is depWait with postgres's longer cold-start budget (postgresETA)
	// in place of the standard dataPlaneETA — see postgresETA's comment.
	pgWait := func(name string) *initpipeline.WaitSpec {
		w := depWait(name)
		w.ETA = postgresETA
		return w
	}
	// graphitiWait is depWait with an explicit hard deadline — see the graphiti
	// component's Wait comment for why it does not take the derived 3×ETA.
	graphitiWait := func(name string) *initpipeline.WaitSpec {
		w := depWait(name)
		w.Deadline = graphitiDeadline
		return w
	}

	comps := []initpipeline.Component{
		{
			Name:        "NATS",
			DisplayName: "NATS (message bus)",
			Manifests:   func() ([][]byte, error) { return manifests.NATS() },
			Wait:        stsWait("spicebox-nats"),
		},
	}

	// SpiceDB: unless the user brought their own external instance, the
	// operator (shipped in the base bundle) manages a SpiceDBCluster.
	if !ext.enabled() {
		mode := spicedbModeFor(strat.InstallProfile().SpiceDBDatastore())
		spicedbDeps := []string{"SpiceDBOperator"}

		comps = append(comps, initpipeline.Component{
			Name:        "SpiceDBOperator",
			DisplayName: "SpiceDB operator",
			// No Manifests: the spicedb-operator bundle ships in the base bundle
			// (applied earlier), so this component only awaits its rollout — the
			// same pattern as the "operator" (AP operator) component below. This
			// is authzed's spicedb-operator in its own "spicedb-operator"
			// namespace; do not confuse it with the AP operator (spicebox-operator
			// in agentprimitives-system).
			Wait: &initpipeline.WaitSpec{
				Poll: deploymentRolledOut(typed, spicedbOperatorNS, "spicedb-operator"),
				Diagnose: func(ctx context.Context) (wait.Diagnosis, error) {
					return wait.DiagnoseDeployment(ctx, typed, spicedbOperatorNS, "spicedb-operator")
				},
				ETA: statefulSetETA,
			},
		})

		if mode == SpiceDBPostgres {
			spicedbDeps = append(spicedbDeps, "SpiceDBDatabase")
			comps = append(comps, initpipeline.Component{
				Name:        "SpiceDBDatabase",
				DisplayName: "SpiceDB database (Postgres)",
				DependsOn:   []string{"postgres"},
				Manifests: func() ([][]byte, error) {
					doc, err := buildSpiceDBDatabaseJob()
					if err != nil {
						return nil, err
					}
					return [][]byte{doc}, nil
				},
				Wait: &initpipeline.WaitSpec{
					Poll: jobSucceeded(typed, ns, "spicebox-spicedb-createdb"),
					ETA:  dataPlaneETA,
				},
			})
		}

		comps = append(comps, initpipeline.Component{
			Name:        "SpiceDB",
			DisplayName: "SpiceDB (authorization)",
			DependsOn:   spicedbDeps,
			Manifests: func() ([][]byte, error) {
				doc, err := buildSpiceDBClusterDoc(mode)
				if err != nil {
					return nil, err
				}
				return [][]byte{doc}, nil
			},
			// The operator creates spicebox-spicedb-spicedb asynchronously after
			// reconciling the CR just applied, so this must tolerate a NotFound
			// Deployment as "not ready yet" — spicedbClusterServing does that;
			// the generic deploymentRolledOut treats NotFound as fatal.
			Wait: &initpipeline.WaitSpec{
				Poll: spicedbClusterServing(typed, ns, "spicebox-spicedb-spicedb"),
				// Diagnose against the CR as well as the Deployment: the
				// spicedb-operator writes the failing pod's fatal error into the
				// SpiceDBCluster's RolloutError condition, and does so even before
				// the Deployment it manages exists.
				Diagnose: func(ctx context.Context) (wait.Diagnosis, error) {
					return wait.DiagnoseSpiceDBCluster(ctx, typed, bundle.Dynamic, ns, "spicebox-spicedb-spicedb", "spicebox-spicedb")
				},
				ETA: statefulSetETA,
			},
		})
	}

	// spicedbDep strips "SpiceDB" from a DependsOn list when ext.enabled() is
	// true, so consumers never reference the now-absent component (the
	// pipeline's DAG validator rejects an unknown DependsOn name).
	spicedbDep := func(deps []string) []string {
		if !ext.enabled() {
			return deps
		}
		out := deps[:0:0]
		for _, d := range deps {
			if d != "SpiceDB" {
				out = append(out, d)
			}
		}
		return out
	}

	comps = append(comps,
		initpipeline.Component{
			Name:        "channelsd",
			DisplayName: "channel service",
			DependsOn:   spicedbDep([]string{"NATS", "SpiceDB"}),
			// channelsd carries a first-party image tag, so its manifests are
			// run through manifests.Substitute (the pull-secret injection happens
			// in the shared Apply closure for first-party refs).
			Manifests: func() ([][]byte, error) {
				ms, err := manifests.ChannelsD()
				if err != nil {
					return nil, err
				}
				for i := range ms {
					ms[i], err = manifests.Substitute(ms[i], tags)
					if err != nil {
						return nil, fmt.Errorf("substitute channelsd manifest: %w", err)
					}
				}
				// External SpiceDB over TLS: channelsd has no SPICEDB_INSECURE env
				// in the base bundle (unlike operator/authzd/webd), so flip its
				// --spicedb-insecure default (true) via an appended CLI arg instead.
				if ext.enabled() && !ext.Insecure {
					if err := appendChannelsdInsecureFlag(ms); err != nil {
						return nil, fmt.Errorf("flip channelsd --spicedb-insecure for external TLS SpiceDB: %w", err)
					}
				}
				return ms, nil
			},
			Wait: depWait("spicebox-channelsd"),
		},
		initpipeline.Component{
			Name:        "postgres",
			DisplayName: "PostgreSQL (agent memory)",
			Manifests: func() ([][]byte, error) {
				return injectStatefulClass(manifests.Postgres, statefulClass)
			},
			Wait: pgWait("spicebox-postgres"),
		},
		initpipeline.Component{
			Name:        "neo4j",
			DisplayName: "Neo4j (knowledge graph)",
			Manifests: func() ([][]byte, error) {
				return injectStatefulClass(manifests.Neo4j, statefulClass)
			},
			Wait: stsWait("spicebox-neo4j"),
		},
		initpipeline.Component{
			Name:        "graphiti",
			DisplayName: "Graphiti (KG extraction)",
			Optional:    true,
			// Graphiti opens its neo4j bolt connection during application startup
			// (NEO4J_URI=bolt://spicebox-neo4j.agentprimitives-system.svc:7687) and
			// EXITS 255 — "ValueError: Cannot resolve address …" / "Application
			// startup failed" — when neo4j is not yet serving. Applied in parallel
			// with a cold neo4j it therefore crash-loops on kubelet restart backoff
			// until neo4j wins, which on a fresh cluster is guaranteed to outlast
			// graphiti's own wait window: neo4j must first pull a ~353 MB image,
			// create its store, boot a JVM, and clear a readinessProbe with
			// initialDelaySeconds: 30. Gate its apply behind neo4j being Ready so
			// the container starts once, into a reachable database.
			DependsOn: []string{"neo4j"},
			Manifests: func() ([][]byte, error) { return manifests.Graphiti() },
			// Gated optional: it waits under the run context rather than the
			// optional-tail short-circuit (see runDAG), so its deadline is what
			// bounds a broken graphiti's hold on the install. Cap it explicitly
			// rather than take the derived 3×ETA: unlike an ungated component
			// this one starts into a neo4j that is ALREADY Ready, so there is no
			// cold-dependency case left for the grace to cover — only its own
			// small image pull and bolt connect.
			Wait: graphitiWait("spicebox-graphiti"),
		},
		initpipeline.Component{
			Name:        "extractord",
			DisplayName: "attachment extraction",
			// No Manifests: extractord ships in the base bundle
			// (config/extractord/, listed unconditionally in
			// config/kustomization.yaml for every cluster kind — see
			// config/manager/deployment.yaml's EXTRACTORD_ENDPOINT comment) and
			// is applied earlier in the baseDocs loop, same as
			// operator/webd/authzd. No DependsOn: unlike every other component
			// here it has no database, no SpiceDB, no NATS, and no Secret — it
			// is a pure HTTP function service, so nothing in the base bundle
			// needs to precede it. Listed ahead of "operator" (the operator
			// dials it over EXTRACTORD_ENDPOINT) so the checklist reads as a
			// prerequisite even though both are wave-0-or-earlier and apply/wait
			// concurrently in practice. Optional, like graphiti: a stalled
			// rollout degrades one feature (inbound-attachment text extraction)
			// rather than chat/memory/auth, so it warns instead of aborting the
			// whole install — but unlike today (zero mentions of extractord in
			// this file), the warning is no longer silent.
			Optional: true,
			Wait:     depWait("agentprimitives-extractord"),
		},
		initpipeline.Component{
			Name:        "operator",
			DisplayName: "agent operator",
			DependsOn:   spicedbDep([]string{"NATS", "SpiceDB", "postgres"}),
			// No Manifests: the operator ships in the base bundle (applied
			// earlier) and was patch-rolled in RunInstall. Await its rollout with
			// the strict deploymentRolledOut poll (observed generation caught up,
			// every replica updated + available, no surge replicas linger).
			Wait: &initpipeline.WaitSpec{
				Poll: deploymentRolledOut(typed, ns, "spicebox-operator"),
				Diagnose: func(ctx context.Context) (wait.Diagnosis, error) {
					return wait.DiagnoseDeployment(ctx, typed, ns, "spicebox-operator")
				},
				ETA: statefulSetETA,
			},
		},
		initpipeline.Component{
			Name:        "webd",
			DisplayName: "web UI",
			DependsOn:   spicedbDep([]string{"NATS", "SpiceDB", "operator"}),
			// No Manifests: webd ships in the base bundle applied earlier. Its
			// runtime deps (NATS_URL, SPICEDB_ENDPOINT, OPERATOR_MEMORY_URL) are
			// confirmed from the Deployment spec in the embedded manifests.
			Wait: depWait("spicebox-webd"),
		},
		initpipeline.Component{
			Name:        "authzd",
			DisplayName: "authorization service",
			DependsOn:   spicedbDep([]string{"NATS", "SpiceDB", "operator"}),
			// No Manifests: authzd ships in the base bundle applied earlier. Its
			// runtime deps (NATS_URL, SPICEDB_ENDPOINT, MEMORY_URL→operator) are
			// confirmed from the Deployment spec in the embedded manifests.
			Wait: depWait("agentprimitives-authzd"),
		},
	)

	return comps
}

// spicedbClusterServing polls the operator-created SpiceDB Deployment. The
// operator creates it asynchronously after reconciling the SpiceDBCluster, so a
// NotFound is "not created yet" (keep polling), not a failure — unlike the
// generic deploymentRolledOut, which surfaces NotFound as a fatal error.
func spicedbClusterServing(typed kubernetes.Interface, ns, deployName string) progress.Poll {
	return func(ctx context.Context) (bool, error) {
		d, err := typed.AppsV1().Deployments(ns).Get(ctx, deployName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil // operator hasn't created the Deployment yet
		}
		if err != nil {
			return false, fmt.Errorf("get deployment %s/%s: %w", ns, deployName, err)
		}
		if d.Generation != d.Status.ObservedGeneration {
			return false, nil
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		return d.Status.UpdatedReplicas == want &&
			d.Status.Replicas == want &&
			d.Status.AvailableReplicas == want, nil
	}
}

// applyManifestGroups splits each manifest in groups into its documents, mirrors
// dependency image refs (air-gapped installs), injects an imagePullSecret into
// first-party Deployment docs when secret is set, and server-side-applies every
// document via kube.Apply under the "ap-install" field manager. It is the shared
// apply body used by the install pipeline's Apply closure; emit, when non-nil, is
// called with a human-readable id per applied doc. injectImagePullSecret is a
// no-op for non-first-party (dependency) images, so passing firstPartyRefs for
// every component is safe — only channelsd's first-party image is affected.
func applyManifestGroups(ctx context.Context, dyn dynamic.Interface, groups [][]byte, secret string, firstPartyRefs map[string]bool, mirrorMap map[string]string, emit func(id string)) error {
	for _, m := range groups {
		docs, err := manifests.Split(m)
		if err != nil {
			return fmt.Errorf("split manifest: %w", err)
		}
		for _, d := range docs {
			if err := mirrorImageRefs(d, mirrorMap); err != nil {
				return fmt.Errorf("mirror image refs: %w", err)
			}
			if secret != "" && firstPartyRefs != nil {
				if err := injectImagePullSecret(d, secret, firstPartyRefs, apimage.Operator.Name); err != nil {
					return fmt.Errorf("inject imagePullSecret into %s %s/%s: %w", d.GetKind(), d.GetNamespace(), d.GetName(), err)
				}
			}
			if err := kube.Apply(ctx, dyn, d, "ap-install"); err != nil {
				return fmt.Errorf("apply %s %s/%s: %w", d.GetKind(), d.GetNamespace(), d.GetName(), err)
			}
			if emit != nil {
				emit(resourceID(d.GetKind(), d.GetNamespace(), d.GetName()))
			}
		}
	}
	return nil
}

// ensureSpiceDBToken creates the spicebox-spicedb-token Secret if it does
// not already exist. The token is the gRPC pre-shared key used by both
// SpiceDB (via env from this Secret) and channelsd (mounted as a file).
// Idempotent: a second call is a no-op so repeated installs don't rotate.
func ensureSpiceDBToken(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	return ensureSpiceDBTokenWithClient(ctx, cli)
}

// mintSpiceDBToken returns a fresh pre-shared key: 32 random bytes, hex-encoded.
func mintSpiceDBToken() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	return []byte(hex.EncodeToString(buf)), nil
}

// ensureSpiceDBTokenWithClient creates spicebox-spicedb-token (32 random
// bytes, hex-encoded) if it does not already exist, and guarantees
// preshared_key == token. Neither existing value is ever rotated:
//
//   - `token` only: the shape oap itself provisions (and older installs). It is
//     authoritative, and `preshared_key` is mirrored from it.
//   - `preshared_key` only: the shape the spicedb-operator documents, and what
//     an External Secrets Operator / GitOps pre-provision produces. It is
//     ADOPTED — SpiceDB reads only this key, so overwriting it boots SpiceDB
//     with an empty pre-shared key and wedges the install — and `token` is
//     backfilled from it for oap's own components.
//   - both, disagreeing: `token` wins, because that is the key every oap
//     component reads.
//
// Idempotent in every one of those shapes: a converged secret is not written
// again, so repeated installs neither rotate nor churn resourceVersion.
func ensureSpiceDBTokenWithClient(ctx context.Context, cli client.Client) error {
	var existing corev1.Secret
	err := cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-spicedb-token",
	}, &existing)
	if err == nil {
		tok := existing.Data["token"]
		pre := existing.Data["preshared_key"]
		if len(tok) > 0 && bytes.Equal(tok, pre) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		switch {
		case len(tok) > 0:
			existing.Data["preshared_key"] = tok
		case len(pre) > 0:
			existing.Data["token"] = pre
		default:
			minted, mintErr := mintSpiceDBToken()
			if mintErr != nil {
				return mintErr
			}
			existing.Data["token"] = minted
			existing.Data["preshared_key"] = minted
		}
		if err := cli.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update spicedb token secret: %w", err)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get spicedb token secret: %w", err)
	}
	tok, err := mintSpiceDBToken()
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-spicedb-token",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": tok, "preshared_key": tok},
	}
	return cli.Create(ctx, sec)
}

// ensureSpiceDBDatastoreURI creates a client from restCfg and delegates to
// ensureSpiceDBDatastoreURIWithClient.
func ensureSpiceDBDatastoreURI(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	return ensureSpiceDBDatastoreURIWithClient(ctx, cli)
}

// ensureSpiceDBDatastoreURIWithClient writes a datastore_uri onto
// spicebox-spicedb-token pointing at a dedicated 'spicedb' database in the
// shared Postgres instance (remote/postgres installs only). The password is
// read from the existing spicebox-postgres-token secret and never
// regenerated. Idempotent: a no-op if the URI is already set to the same
// value. Requires spicebox-spicedb-token to already exist (ensureSpiceDBToken
// must run first); it hard-errors via Get otherwise.
func ensureSpiceDBDatastoreURIWithClient(ctx context.Context, cli client.Client) error {
	var pg corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-postgres-token",
	}, &pg); err != nil {
		return fmt.Errorf("get postgres token secret: %w", err)
	}
	pw := string(pg.Data["password"])
	if pw == "" {
		return fmt.Errorf("postgres token secret has no password")
	}
	uri := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", pw),
		Host:     "spicebox-postgres.agentprimitives-system.svc:5432",
		Path:     "/spicedb",
		RawQuery: "sslmode=disable",
	}).String()

	var sp corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-spicedb-token",
	}, &sp); err != nil {
		return fmt.Errorf("get spicedb token secret: %w", err)
	}
	if string(sp.Data["datastore_uri"]) == uri {
		return nil
	}
	if sp.Data == nil {
		sp.Data = map[string][]byte{}
	}
	sp.Data["datastore_uri"] = []byte(uri)
	return cli.Update(ctx, &sp)
}

// ensureSpiceDBBootstrap creates/updates the spicebox-spicedb-bootstrap
// ConfigMap holding the bootstrap YAML SpiceDB applies at startup via
// --datastore-bootstrap-files. The bootstrap-files format is a YAML
// envelope with `schema: |` (and optional `relationships: |`) — NOT
// a raw .zed file. We marshal via sigs.k8s.io/yaml so the block scalar
// indentation is correct.
//
// Call this ONLY for a mode whose CR actually sets datastoreBootstrapFiles
// (SpiceDBClusterMode.bootstrapsSchemaAtStartup) — the ConfigMap exists purely
// to back that flag, and creating it elsewhere leaves an unread object behind.
// It seeds the cold-start window before the AP operator's first schema
// reconcile; the operator, not this ConfigMap, is the authoritative writer.
func ensureSpiceDBBootstrap(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	bootstrapBytes, err := yaml.Marshal(struct {
		Schema string `json:"schema"`
	}{Schema: apspicedb.Schema})
	if err != nil {
		return fmt.Errorf("marshal spicedb bootstrap: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-spicedb-bootstrap",
			Namespace: "agentprimitives-system",
		},
		Data: map[string]string{"schema.zed": string(bootstrapBytes)},
	}
	var existing corev1.ConfigMap
	err = cli.Get(ctx, types.NamespacedName{
		Namespace: cm.Namespace, Name: cm.Name,
	}, &existing)
	if err == nil {
		existing.Data = cm.Data
		return cli.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return cli.Create(ctx, cm)
}

// inClusterSpiceDBEndpoint is the gRPC address the bundled operator-managed
// SpiceDB serves on — the value ensureSpiceDBEndpointConfig writes into the
// shared endpoint ConfigMap. detectExternalSpiceDB compares the ConfigMap's
// stored endpoint against it: anything else means the cluster points at an
// external instance, so the two must agree by referencing this one constant.
const inClusterSpiceDBEndpoint = "spicebox-spicedb.agentprimitives-system.svc:50051"

// ensureSpiceDBEndpointConfig creates the spicebox-spicedb-config ConfigMap
// channelsd reads via valueFrom.configMapKeyRef. Always points at the
// in-cluster Service.
func ensureSpiceDBEndpointConfig(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-spicedb-config",
			Namespace: "agentprimitives-system",
		},
		Data: map[string]string{
			"endpoint": inClusterSpiceDBEndpoint,
		},
	}
	var existing corev1.ConfigMap
	err = cli.Get(ctx, types.NamespacedName{
		Namespace: cm.Namespace, Name: cm.Name,
	}, &existing)
	if err == nil {
		existing.Data = cm.Data
		return cli.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return cli.Create(ctx, cm)
}

// ExternalSpiceDB carries the connection details for a user-supplied SpiceDB
// instance, set via `oap init --external-spicedb-endpoint/-token/-insecure` or
// the equivalent interactive prompt. The zero value (Endpoint == "") means "no
// external instance" — RunInstall falls back to the bundled operator-managed
// SpiceDBCluster.
type ExternalSpiceDB struct {
	Endpoint string
	Token    string
	Insecure bool
}

// enabled reports whether the caller opted into an external SpiceDB instance.
func (e ExternalSpiceDB) enabled() bool {
	return e.Endpoint != ""
}

// applyExternalSpiceDBConfig points every client that resolves SpiceDB
// connection info (channelsd, webd, authzd, the operator) at e's external
// instance, by upserting the exact same two resources the in-cluster path
// writes: spicebox-spicedb-config (endpoint) and spicebox-spicedb-token
// (token). No token generation, no bootstrap ConfigMap — the caller's
// instance already has (or will have, via a separate best-effort schema
// apply) its own schema.
func applyExternalSpiceDBConfig(ctx context.Context, cli client.Client, e ExternalSpiceDB) error {
	if err := upsertConfigMap(ctx, cli, spicedb.SharedConfigMapName, "agentprimitives-system", map[string]string{
		spicedb.SharedConfigMapEndpointKey: e.Endpoint,
	}); err != nil {
		return fmt.Errorf("apply external spicedb endpoint configmap: %w", err)
	}
	if err := upsertSecretKey(ctx, cli, spicedb.SharedTokenSecretName, "agentprimitives-system", spicedb.SharedTokenSecretKey, e.Token); err != nil {
		return fmt.Errorf("apply external spicedb token secret: %w", err)
	}
	return nil
}

// upsertConfigMap creates the named ConfigMap with the given Data if absent,
// or overwrites Data on an existing one (Get→Update-or-Create), mirroring the
// ensure* helpers' style above.
func upsertConfigMap(ctx context.Context, cli client.Client, name, namespace string, data map[string]string) error {
	var existing corev1.ConfigMap
	err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if err == nil {
		existing.Data = data
		return cli.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return cli.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	})
}

// upsertSecretKey creates the named Secret with the given single key/value if
// absent, or sets that one key on an existing Secret without touching any
// other keys already present (Get→Update-or-Create), mirroring the ensure*
// helpers' style above.
func upsertSecretKey(ctx context.Context, cli client.Client, name, namespace, key, value string) error {
	var existing corev1.Secret
	err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if err == nil {
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[key] = []byte(value)
		return cli.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return cli.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{key: []byte(value)},
	})
}

// ensureNATSIdentity creates the spicebox-nats-identity Secret if it does not
// already exist. The Secret holds the operator + account decentralized-JWT
// trust material plus a rendered NATS server-auth include file. Idempotent: a
// re-install must not rotate the identity, so an existing Secret is left alone.
func ensureNATSIdentity(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	return ensureNATSIdentityWithClient(ctx, cli)
}

func ensureNATSIdentityWithClient(ctx context.Context, c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-nats-identity",
	}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get nats identity secret: %w", err)
	}

	id, err := apnats.GenerateIdentity()
	if err != nil {
		return fmt.Errorf("generate nats identity: %w", err)
	}

	// The interpolated values (OperatorJWT, AccountPublicKey, AccountJWT) are
	// base32/base64url-encoded and contain no whitespace, quotes, or braces, so
	// they are safe to inline into the NATS config format without escaping.
	authConf := fmt.Sprintf(`operator: %s
resolver: MEMORY
resolver_preload: {
  %s: %s
}
`, id.OperatorJWT, id.AccountPublicKey, id.AccountJWT)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-nats-identity",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"operator-jwt":               []byte(id.OperatorJWT),
			"account-jwt":                []byte(id.AccountJWT),
			"account-public-key":         []byte(id.AccountPublicKey),
			"account-signing-seed":       id.AccountSigningSeed,
			"account-signing-public-key": []byte(id.AccountSigningPublicKey),
			"auth.conf":                  []byte(authConf),
		},
	}
	return c.Create(ctx, sec)
}

// ensureNATSTLS creates the spicebox-nats-tls Secret if it does not already
// exist. The Secret holds a self-signed CA plus a NATS server cert/key whose
// SANs cover the in-cluster Service DNS names. Idempotent: a re-install must
// not rotate the TLS material, so an existing Secret is left alone.
func ensureNATSTLS(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	return ensureNATSTLSWithClient(ctx, cli)
}

func ensureNATSTLSWithClient(ctx context.Context, c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-nats-tls",
	}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get nats tls secret: %w", err)
	}

	m, err := apnats.GenerateTLS([]string{
		"spicebox-nats",
		"spicebox-nats.agentprimitives-system",
		"spicebox-nats.agentprimitives-system.svc",
		"spicebox-nats.agentprimitives-system.svc.cluster.local",
	})
	if err != nil {
		return fmt.Errorf("generate nats tls: %w", err)
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-nats-tls",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  m.CACertPEM,
			"tls.crt": m.ServerCertPEM,
			"tls.key": m.ServerKeyPEM,
		},
	}
	return c.Create(ctx, sec)
}

func ensureWebhookTLSWithClient(ctx context.Context, c client.Client) ([]byte, error) {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-webhook-tls",
	}, &existing)
	if err == nil {
		caPEM := existing.Data["ca.crt"]
		if len(caPEM) == 0 {
			return nil, fmt.Errorf("spicebox-webhook-tls secret exists but is missing ca.crt")
		}
		return caPEM, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get webhook tls secret: %w", err)
	}

	m, err := apnats.GenerateTLS([]string{
		"spicebox-webhook.agentprimitives-system.svc",
		"spicebox-webhook.agentprimitives-system.svc.cluster.local",
	})
	if err != nil {
		return nil, fmt.Errorf("generate webhook tls: %w", err)
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-webhook-tls",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  m.CACertPEM,
			"tls.crt": m.ServerCertPEM,
			"tls.key": m.ServerKeyPEM,
		},
	}
	if err := c.Create(ctx, sec); err != nil {
		return nil, fmt.Errorf("create webhook tls secret: %w", err)
	}
	return m.CACertPEM, nil
}

// patchWebhookVWCBundle sets clientConfig.caBundle on every webhook in the
// ValidatingWebhookConfiguration named "spicebox-settings" to caPEM. It is
// idempotent: re-running oap install re-patches with the same CA. If the VWC
// does not exist yet (Task 6 creates it in a later unit), the patch is
// silently skipped rather than failing install — the patch will succeed once
// the manifest bundle is applied.
func patchWebhookVWCBundle(ctx context.Context, c client.Client, caPEM []byte) error {
	for _, configurationName := range []string{"spicebox-settings", "spicebox-goal-execution"} {
		var vwc admissionregistrationv1.ValidatingWebhookConfiguration
		if err := c.Get(ctx, types.NamespacedName{Name: configurationName}, &vwc); err != nil {
			if apierrors.IsNotFound(err) {
				// VWC not yet created (Task 6 lands it in a later unit). Skip the
				// caBundle patch — install must not fail before the manifest bundle
				// includes the VWC. The patch will succeed on the next oap install
				// after the manifest is applied.
				continue
			}
			return fmt.Errorf("get ValidatingWebhookConfiguration %s: %w", configurationName, err)
		}
		for i := range vwc.Webhooks {
			vwc.Webhooks[i].ClientConfig.CABundle = caPEM
		}
		if err := c.Update(ctx, &vwc); err != nil {
			return fmt.Errorf("update ValidatingWebhookConfiguration %s caBundle: %w", configurationName, err)
		}
	}
	return nil
}

// ensureNATSClientCreds mints per-client NATS user JWTs for channelsd (the
// broker, full ap.> access) and the oap CLI (scoped to session in/out
// subjects), storing each as a Secret alongside the CA cert. It runs after
// ensureNATSIdentity and ensureNATSTLS: both prerequisite Secrets must exist.
// Each output Secret is created idempotently — a re-install does not re-mint.
// The principal name minted into each creds file. Named once because the name
// is also the input to apnats.InboxSubjectFor, and the client rederives the
// matching prefix from the same name in the JWT — writing it twice invites the
// two to drift, which breaks request/reply rather than merely widening a grant.
const (
	channelsdNATSUser = "channelsd"
	cliNATSUser       = "cli"
	webdNATSUser      = "webd"
)

// webdNATSGrant is webd's minted NATS user grant. webd SUBSCRIBES to session
// out-subjects (chat + live-view mirror) AND does NATS request-reply: the
// builtin web-chat listener and the live-view submit view_message via
// RequestIn, so webd both PUBLISHES the request and must SUBSCRIBE to its
// reply inbox. The original "never publishes, subscribe-only" assumption
// predated those surfaces and was the bug — a missing inbox entry made every
// view-originated send time out even though the message was delivered.
//
// PubAllow is enumerated because an OMITTED one is publish-ANYWHERE, not
// deny-all (see apnats.UserGrant). This grant used to omit it, which left the
// browser-facing process holding publish rights on the entire control bus:
// ap.revocation, every session's out-subjects (forging the agent's own voice),
// and every inbound kind — including in.user_message, whose channelsd handler
// is the audit-signing inbound writer webd is deliberately kept away from
// (pkg/channels/channelkinds/clienthosted.SubmitUserMessage spells out why: webd holds
// no memory-write credential, so it must round-trip through view_message).
//
// The five subjects below are webd's complete publish surface, each a verified
// call site: view_message (chat SubmitUserMessage and the /interact
// user_message | annotation_batch | mcp_ui_action kinds, all via
// channelkinds.RequestViewMessage), interrupt_request and interaction_decision
// (clienthosted.Listener), resurface_request (channelkinds.
// PublishResurfaceRequest) and app_tool_call (webui/interact). webd links
// pkg/channels/channelsd/pipeline but only for its Authz interface type — it constructs
// no Pipeline, so none of that package's publishes can fire here.
//
// webd needs no publish grant on any inbox: it is never a NATS responder
// (nothing in its import graph calls msg.Respond), and a requester publishes to
// the target subject, not to its own reply inbox.
//
// TestWebdNATSGrantPublishSurface asserts every line of this against a real
// server; add a subject there and here together.
var webdNATSGrant = apnats.UserGrant{
	Name: webdNATSUser,
	PubAllow: []string{
		"ap.session.*.*.in.view_message",
		"ap.session.*.*.in.interrupt_request",
		"ap.session.*.*.in.interaction_decision",
		"ap.component.interaction_decision.*.*",
		"ap.session.*.*.in.resurface_request",
		"ap.session.*.*.in.app_tool_call",
		// ui_data_binding is the agent-UI's own data path: pkg/web/webui/agentui's
		// bindingsHandler resolves every `source: tool` binding by asking the
		// session's runner, via pkg/web/uibindings/tool.
		//
		// Its absence is why that feature had never worked on a real install.
		// The failure gave nothing away: a publish the server refuses does not
		// fail the caller's request — NATS reports the violation as an ASYNC
		// error on the connection, while the request itself simply waits for a
		// reply that can now never come and dies on its own timeout. So every
		// binding surfaced as "the runner did not answer", which reads as a
		// missing or busy runner and is why the first diagnosis of it chased a
		// sleeping session instead of a permission.
		"ap.session.*.*.in.ui_data_binding",
		// ui_presence is the liveness heartbeat webd republishes while a
		// viewer has an agent-UI open and focused, so the runner answering
		// that UI's bindings does not idle out underneath them.
		//
		// Added in the SAME change as the kind and as its row in
		// TestWebdNATSGrantPublishSurface, which is the discipline the line
		// above exists to teach: ui_data_binding shipped without its grant and
		// the whole agent-UI data path silently never worked.
		"ap.session.*.*.in.ui_presence",
		// ui_action is the agent-UI's INVOKE path: pkg/web/webui/agentui's
		// actions handler sends the declared action to the session's runner and
		// relays the synchronous reply. It shipped without this entry, so every
		// agent-UI action failed exactly the way ui_data_binding did — see above.
		"ap.session.*.*.in.ui_action",
		// channelevents.WebhookInboundSubject — not a *.* session template
		// (there is no session yet at delivery time, see that constant's own
		// doc) but the same failure shape as ui_data_binding above: without
		// this entry, pkg/web/webui/channelwebhook's Publish call is silently
		// refused by the server, its returned error is nil either way (NATS
		// reports permission violations asynchronously on the connection, not
		// to the caller), and the handler answers the webhook sender with 202
		// for a delivery that was actually dropped — the one status the
		// route's whole retry contract promises will never happen. Referenced
		// via the exported constant, not a copied literal, so this and the
		// route's own Publish call cannot drift apart.
		channelevents.WebhookInboundSubject,
	},
	SubAllow: []string{"ap.session.*.*.out.>", apnats.InboxSubjectFor(webdNATSUser)},
}

func ensureNATSClientCreds(ctx context.Context, restCfg *rest.Config, logf func(string, ...any)) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	return ensureNATSClientCredsWithClient(ctx, cli, logf)
}

// logf narrates a credential rotation. It is threaded rather than written to a
// package-level writer because a rotation is the one thing here an operator
// must be able to read afterwards: everything else this function does is a
// no-op on a healthy re-run. nil is accepted and discards.
func ensureNATSClientCredsWithClient(ctx context.Context, c client.Client, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var identitySec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-nats-identity",
	}, &identitySec); err != nil {
		return fmt.Errorf("get nats identity secret (run ensureNATSIdentity first): %w", err)
	}

	var tlsSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-nats-tls",
	}, &tlsSec); err != nil {
		return fmt.Errorf("get nats tls secret (run ensureNATSTLS first): %w", err)
	}
	caCrt := tlsSec.Data["ca.crt"]
	if len(caCrt) == 0 {
		return fmt.Errorf("nats tls secret is missing ca.crt")
	}

	// MintUser only consults AccountPublicKey + AccountSigningSeed.
	id := &apnats.Identity{
		AccountPublicKey:   string(identitySec.Data["account-public-key"]),
		AccountSigningSeed: identitySec.Data["account-signing-seed"],
	}
	if id.AccountPublicKey == "" || len(id.AccountSigningSeed) == 0 {
		return fmt.Errorf("nats identity secret is missing account-public-key or account-signing-seed")
	}

	// Creds are minted lazily, per Secret, by ensureNATSCredsSecret: a Secret
	// already carrying the shipped grant is left byte-identical (minting is
	// non-deterministic — a fresh user nkey every call — so writing
	// unconditionally would rotate every principal's credential on every
	// re-run), and one carrying a STALE grant is re-minted. See that
	// function's doc for why "already exists" is not a sufficient answer.
	//
	// Both the channelsd broker and the `oap` CLI get broad `ap.>`. channelsd IS
	// the broker (it relays every session's in/out traffic and serves
	// read_thread_history via NATS request/reply). The `oap chat` command embeds
	// a full channelsd pipeline + outbound relay in-process, so the CLI
	// legitimately publishes the same `ap.session.*.*.out.*` and `ap.channel.*`
	// subjects. The runner's per-session JWT (minted in the AgentSession
	// controller) stays narrowly scoped to its own session.
	//
	// Both keep `_INBOX.>` on PUBLISH and narrow only SUBSCRIBE. Both are
	// responders — `m.Respond` publishes to whichever principal asked, on a
	// top-level inbox subject NOT under `ap.>` — and a responder cannot know
	// its requesters' names, so its publish side must span the whole inbox
	// root. Subscribing is the direction that leaks (any `_INBOX.>` subscriber
	// reads every reply on the bus), and that is the direction scoped here.
	//
	// The channelsd creds Secret is mounted by three binaries — channelsd,
	// authzd and the operator — which is exactly why the inbox prefix is
	// derived from the JWT's name rather than from each process's connection
	// name: all three resolve to the same "channelsd" principal and the same
	// inbox root, with their own nuids underneath.
	for _, cred := range natsClientCreds() {
		if err := ensureNATSCredsSecret(ctx, c, cred, id, caCrt, logf); err != nil {
			return err
		}
	}
	return nil
}

// ensureWebdExternalURLConfigMap creates the spicebox-webd-external-url
// ConfigMap if it does not already exist. The ConfigMap holds the externally
// reachable URL for webd (the browser host) under the "trusted-url" key —
// used when minting signed credential + artifact deep-links and as the OAuth
// redirect base. Operators override the default after install. Idempotent:
// if the ConfigMap already exists it is left untouched.
//
// logf narrates each outcome. It is threaded from the install reporter
// (rep.Info) rather than writing straight to a writer: while the checklist's
// live region is up, a direct write to the terminal displaces the cursor
// without touching the region's line accounting and corrupts the redraw — see
// the rail live-region contract in cmd/oap/internal/progress.
func ensureWebdExternalURLConfigMap(ctx context.Context, logf func(string, ...any), c client.Client, managedExternal bool) error {
	// urlOwnership explains who sets the URLs. When oap manages external access
	// (--trusted-hostname → setupWebdExternalAccess patches this ConfigMap with
	// the real https:// URLs at the END of the install), the localhost value is
	// just a transient placeholder — telling the operator to set it by hand here
	// is misleading, so say oap will instead.
	urlOwnership := func() {
		if managedExternal {
			logf("  seeded '%s' + '%s' empty; oap will set them to your external webd URLs once the Gateway is ready (credential/artifact links fail closed until then)",
				v1alpha1.WebdTrustedURLKey, v1alpha1.WebdSandboxURLKey)
			return
		}
		logf("  hint: defaults to "+webdLocalSeedURL+" — set '%s' + '%s' to your externally reachable webd URLs",
			v1alpha1.WebdTrustedURLKey, v1alpha1.WebdSandboxURLKey)
	}

	var existing corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      v1alpha1.WebdExternalURLConfigMap,
	}, &existing)
	if err == nil {
		logf("  %s ConfigMap exists, skipping", v1alpha1.WebdExternalURLConfigMap)
		urlOwnership()
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s configmap: %w", v1alpha1.WebdExternalURLConfigMap, err)
	}
	// The seed value depends on who owns the URL. When oap manages external
	// access (--trusted-hostname), setupWebdExternalAccess patches the real
	// https:// URLs at the END of the install. Seeding localhost here would
	// ship a silently-wrong "Connect your accounts" link (pointing at
	// localhost:8080) if that patch is skipped, declined, or fails — so seed
	// EMPTY and let externalurl.Provider report "not configured" until the
	// real URL lands. channelsd then fails CLOSED (surfaces an error to the
	// session, a CR status, and the monitoring channel) instead of minting a
	// broken link. When oap does NOT manage external access (local /
	// port-forward), http://localhost:8080 is the genuinely-correct value,
	// not a silent fallback.
	seedURL := webdLocalSeedURL
	if managedExternal {
		seedURL = ""
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.WebdExternalURLConfigMap,
			Namespace: "agentprimitives-system",
		},
		Data: map[string]string{
			v1alpha1.WebdTrustedURLKey: seedURL,
			v1alpha1.WebdSandboxURLKey: seedURL,
		},
	}
	if err := c.Create(ctx, cm); err != nil {
		return fmt.Errorf("create %s configmap: %w", v1alpha1.WebdExternalURLConfigMap, err)
	}
	logf("  created %s ConfigMap", v1alpha1.WebdExternalURLConfigMap)
	urlOwnership()
	return nil
}

// ensurePassthroughLinkSigningKey guarantees the spicebox-passthrough-link-key
// Secret exists and carries a usable key: it creates the Secret when absent,
// and fills in the key when the Secret exists but carries none the readers
// accept. The key is 32 random bytes hex-encoded (64 hex chars). Idempotent: a
// usable key is never regenerated — rotating it would invalidate every
// in-flight passthrough link (channelsd just minted; identityd is mid-verify).
//
// "Usable" is passthroughlink.DecodeHexKey's verdict, not len > 0. DecodeHexKey
// is the single gate every production reader of this Secret decodes through —
// webd's readHexKey (fatal at startup), channelsd's loadPassthroughSigner
// (logs and disables the watcher), `oap`'s buildChatViewMinter — and it enforces
// hex-ness and a MinKeyLen floor, because HMAC-SHA256 under a short key is
// guessable. Gating the repair on emptiness alone let install skip a 16-byte or
// non-hex key, print "exists with key" and report SUCCESS, leaving webd unable
// to start. Applying the readers' own check here means install repairs exactly
// what they would refuse.
//
// The repair arm is not hypothetical. The Secret is routinely provisioned by
// something other than `oap install` — External Secrets Operator, a GitOps
// apply, a hand-written manifest — any of which can land an existing object
// with an empty, short, or non-hex "key". That is worse than an absent Secret:
// webd's volume mount is optional, so an absent Secret fails closed, while an
// empty value used to decode to a zero-length HMAC key that signs session
// cookies and deep-links with publicly computable material. Hence
// update-or-create rather than create-only, which returned AlreadyExists and
// repaired nothing.
//
// logf narrates each outcome through the install reporter (rep.Info); see
// ensureWebdExternalURLConfigMap for why these do not write straight to a
// writer while the checklist's live region is up.
func ensurePassthroughLinkSigningKey(ctx context.Context, logf func(string, ...any), c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      v1alpha1.PassthroughLinkSigningKeySecret,
	}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s secret: %w", v1alpha1.PassthroughLinkSigningKeySecret, err)
	}
	exists := err == nil
	// unusable is why the existing key is being replaced — the readers' own
	// verdict, phrased in their words ("is empty", "is not valid hex", "is too
	// short"). It doubles as the text of the repair message below, so an
	// operator whose provisioner owns this Secret sees that their material was
	// REJECTED rather than a bare "regenerated". DecodeHexKey deliberately never
	// echoes key bytes into its error (see its doc), so this is log-safe.
	var unusable error
	if exists {
		if _, unusable = passthroughlink.DecodeHexKey(existing.Data["key"]); unusable == nil {
			logf("  %s Secret exists with a usable key, skipping", v1alpha1.PassthroughLinkSigningKeySecret)
			return nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate passthrough link signing key: %w", err)
	}
	encoded := []byte(hex.EncodeToString(buf))

	// Update in place when the Secret is already there, so we fill in the key
	// without disturbing labels, annotations, or other data keys its
	// provisioner owns.
	if exists {
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["key"] = encoded
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update %s secret: %w", v1alpha1.PassthroughLinkSigningKeySecret, err)
		}
		logf("  regenerated the key in the existing %s Secret (%v)",
			v1alpha1.PassthroughLinkSigningKeySecret, unusable)
		return nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.PassthroughLinkSigningKeySecret,
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": encoded},
	}
	if err := c.Create(ctx, sec); err != nil {
		return fmt.Errorf("create %s secret: %w", v1alpha1.PassthroughLinkSigningKeySecret, err)
	}
	logf("  generated %s Secret", v1alpha1.PassthroughLinkSigningKeySecret)
	return nil
}

// ensureSlackOAuthSecret creates the spicebox-slack-oauth Secret with empty
// client_id, client_secret, and team_id keys if it does not already exist.
// The empty keys ensure volume mounts in identityd don't fail at startup;
// the operator populates the values before deploying identityd. team_id is
// the bot's installed workspace ID — identityd refuses to register the
// Slack OIDC authenticator without it, since the cross-workspace email
// collision attack is only defended by matching the id_token's team claim
// against the installed workspace. Idempotent: if the Secret already exists
// (operator may have populated it) it is left untouched.
//
// logf narrates each outcome through the install reporter (rep.Info); see
// ensureWebdExternalURLConfigMap for why these do not write straight to a
// writer while the checklist's live region is up.
func ensureSlackOAuthSecret(ctx context.Context, logf func(string, ...any), c client.Client) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      v1alpha1.SlackOAuthSecret,
	}, &existing)
	if err == nil {
		logf("  %s Secret exists, skipping", v1alpha1.SlackOAuthSecret)
		logf("  hint: %s Secret is empty — populate client_id + client_secret + team_id to enable identityd's Sign in with Slack flow", v1alpha1.SlackOAuthSecret)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s secret: %w", v1alpha1.SlackOAuthSecret, err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.SlackOAuthSecret,
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"client_id":     []byte(""),
			"client_secret": []byte(""),
			"team_id":       []byte(""),
		},
	}
	if err := c.Create(ctx, sec); err != nil {
		return fmt.Errorf("create %s secret: %w", v1alpha1.SlackOAuthSecret, err)
	}
	logf("  created %s Secret (empty skeleton)", v1alpha1.SlackOAuthSecret)
	logf("  hint: %s Secret is empty — populate client_id + client_secret + team_id to enable identityd's Sign in with Slack flow", v1alpha1.SlackOAuthSecret)
	return nil
}

// createNATSCredsSecret creates a per-client NATS creds Secret if it does not
// already exist. Idempotent per-secret so a re-install does not re-mint creds.
// natsClientCred pairs one creds Secret with the grant its user JWT must
// carry. Pairing them in one value is what lets the mint and the staleness
// check read the SAME grant: a subject added to a grant reaches both by
// construction, instead of by remembering to change a second place.
type natsClientCred struct {
	secretName string
	grant      apnats.UserGrant
}

// natsClientCreds is every principal `oap install` mints a bus credential for.
//
// A function rather than a var because two of the three grants call
// apnats.InboxSubjectFor, and building them at package-init time would put
// their evaluation order before the constants they read in a way nothing
// enforces; there is one caller and it runs once per install.
func natsClientCreds() []natsClientCred {
	return []natsClientCred{
		{
			secretName: "spicebox-channelsd-nats-creds",
			grant: apnats.UserGrant{
				Name:     channelsdNATSUser,
				PubAllow: []string{"ap.>", "_INBOX.>"},
				SubAllow: []string{"ap.>", apnats.InboxSubjectFor(channelsdNATSUser)},
			},
		},
		{
			secretName: "spicebox-cli-nats-creds",
			grant: apnats.UserGrant{
				Name:     cliNATSUser,
				PubAllow: []string{"ap.>", "_INBOX.>"},
				SubAllow: []string{"ap.>", apnats.InboxSubjectFor(cliNATSUser)},
			},
		},
		{secretName: "spicebox-webd-nats-creds", grant: webdNATSGrant},
	}
}

// ensureNATSCredsSecret reconciles one creds Secret against the grant this
// binary ships, minting a user JWT only when the stored one does not already
// carry it.
//
// "The Secret already exists" is NOT a sufficient answer, and treating it as
// one was a real, silent defect: a user JWT is a frozen copy of the grant it
// was minted from, so a cluster installed before a subject was added to a
// grant kept a JWT without it forever. Every upgrade re-ran this, saw the
// Secret, and skipped. The webd grant gaining ap.channel.webhook_inbound is
// the case that exposed it — nats-server reports a permission violation
// asynchronously, so the webhook handler's Publish returned nil and it
// answered 202 Accepted for a delivery the bus had dropped.
//
// A re-mint rotates the user nkey, so it MUST be rare: minting is
// non-deterministic and an unconditional write would rotate every principal on
// every re-run, disconnecting live clients for nothing. apnats.UserGrant's
// CredsDrift is the gate, and it compares allow-lists as sets so ordering
// alone never triggers a rotation.
//
// ca.crt is refreshed on the same write, because a creds Secret carrying a CA
// that no longer matches the server's is the same class of stale.
func ensureNATSCredsSecret(
	ctx context.Context, c client.Client, cred natsClientCred, id *apnats.Identity, caCrt []byte,
	logf func(string, ...any),
) error {
	key := types.NamespacedName{Namespace: "agentprimitives-system", Name: cred.secretName}

	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		drift := cred.grant.CredsDrift(string(existing.Data["nats.creds"]))
		if drift == "" && bytes.Equal(existing.Data["ca.crt"], caCrt) {
			return nil
		}
		if drift == "" {
			drift = "the stored CA no longer matches the NATS server's"
		}
		// Say why out loud. The whole defect class here is a permission the
		// bus refuses without telling anyone, so an operator who later sees a
		// component reconnect deserves a line explaining what rotated and why.
		logf("re-minting %s: %s", cred.secretName, drift)

		creds, mintErr := apnats.MintUser(id, cred.grant)
		if mintErr != nil {
			return fmt.Errorf("re-mint %s nats creds: %w", cred.grant.Name, mintErr)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["nats.creds"] = []byte(creds)
		existing.Data["ca.crt"] = caCrt
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update %s secret: %w", cred.secretName, err)
		}
		// A running process read its creds file once, at connect time; the
		// kubelet refreshing the mounted file changes nothing for it. Without
		// this the re-mint lands in the cluster and the stale JWT stays on the
		// wire — the exact half-fix this function exists to close.
		return restartWorkloadsMountingSecret(ctx, c, key, logf)

	case apierrors.IsNotFound(err):
		creds, mintErr := apnats.MintUser(id, cred.grant)
		if mintErr != nil {
			return fmt.Errorf("mint %s nats creds: %w", cred.grant.Name, mintErr)
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cred.secretName,
				Namespace: "agentprimitives-system",
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"nats.creds": []byte(creds),
				"ca.crt":     caCrt,
			},
		}
		return c.Create(ctx, sec)

	default:
		return fmt.Errorf("get %s secret: %w", cred.secretName, err)
	}
}

// restartWorkloadsMountingSecret rolls every Deployment in the Secret's
// namespace whose pod template mounts it, so a rotated credential actually
// reaches the processes using it.
//
// Which Deployments those are is DERIVED from the live pod specs rather than
// transcribed into a table here: the channelsd creds Secret alone is mounted
// by three different binaries, and a table would go stale the first time a
// fourth mounted it — silently, in the direction of "the rotation did not
// reach someone".
//
// Nothing to roll is the normal case on a first install: the component
// manifests have not been applied yet, so whatever starts later mounts the
// current Secret and needs no restart.
func restartWorkloadsMountingSecret(ctx context.Context, c client.Client, secret types.NamespacedName, logf func(string, ...any)) error {
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps, client.InNamespace(secret.Namespace)); err != nil {
		return fmt.Errorf("list deployments in %s to roll after rotating %s: %w",
			secret.Namespace, secret.Name, err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range deps.Items {
		d := &deps.Items[i]
		if !podSpecMountsSecret(&d.Spec.Template.Spec, secret.Name) {
			continue
		}
		patch := []byte(fmt.Sprintf(
			`{"spec":{"template":{"metadata":{"annotations":{"ap.authzed.com/restartedAt":%q}}}}}`, stamp))
		if err := c.Patch(ctx, d, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
			return fmt.Errorf("rollout-restart %s/%s after rotating %s: %w",
				d.Namespace, d.Name, secret.Name, err)
		}
		logf("restarting %s to pick up the re-minted %s", d.Name, secret.Name)
	}
	return nil
}

// podSpecMountsSecret reports whether the pod template reads the named Secret
// — as a volume (the creds-file shape) or through env/envFrom, so a future
// consumer that reads it a different way is still rolled.
func podSpecMountsSecret(spec *corev1.PodSpec, name string) bool {
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == name {
			return true
		}
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.Secret != nil && src.Secret.Name == name {
				return true
			}
		}
	}
	containers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	containers = append(containers, spec.Containers...)
	containers = append(containers, spec.InitContainers...)
	for _, ctr := range containers {
		for _, ef := range ctr.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == name {
				return true
			}
		}
		for _, e := range ctr.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == name {
				return true
			}
		}
	}
	return false
}

// ensurePostgresToken creates the spicebox-postgres-token Secret if it does
// not already exist. The Secret holds the Postgres password and a full
// connection URI for the in-cluster Postgres instance backing the memory
// store. Idempotent: a re-install does not rotate the password.
func ensurePostgresToken(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	var existing corev1.Secret
	err = cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-postgres-token",
	}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get postgres token secret: %w", err)
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate postgres password: %w", err)
	}
	password := hex.EncodeToString(buf)
	pgURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", password),
		Host:     "spicebox-postgres.agentprimitives-system.svc:5432",
		Path:     "/memory",
		RawQuery: "sslmode=disable",
	}
	uri := pgURL.String()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-postgres-token",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte(password),
			"uri":      []byte(uri),
		},
	}
	return cli.Create(ctx, sec)
}

// ensureNeo4jToken creates the spicebox-neo4j-token Secret if it does not
// already exist. The Secret holds the Neo4j auth string (in NEO4J_AUTH format)
// and the raw password for Graphiti to reference. Idempotent: a re-install
// does not rotate the password.
func ensureNeo4jToken(ctx context.Context, restCfg *rest.Config) error {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return err
	}
	var existing corev1.Secret
	err = cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-neo4j-token",
	}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get neo4j token secret: %w", err)
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate neo4j password: %w", err)
	}
	password := hex.EncodeToString(buf)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-neo4j-token",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"auth":     []byte(fmt.Sprintf("neo4j/%s", password)),
			"password": []byte(password),
		},
	}
	return cli.Create(ctx, sec)
}

// ensureGraphitiConfig creates or refreshes the spicebox-graphiti-config
// Secret from the current OPENAI_API_KEY environment variable, and reports
// whether the Secret ends up carrying a non-empty key.
func ensureGraphitiConfig(ctx context.Context, out io.Writer, restCfg *rest.Config) (bool, error) {
	cli, err := client.New(restCfg, client.Options{Scheme: kube.Scheme})
	if err != nil {
		return false, err
	}
	return ensureGraphitiConfigWithClient(ctx, out, cli)
}

// ensureGraphitiConfigWithClient is ensureGraphitiConfig's client-injected
// half, split out for testing.
//
// "The Secret already exists" is not a sufficient answer on its own — the
// same class of bug ensureNATSCredsSecret's doc comment describes: a Secret
// minted while OPENAI_API_KEY was unset stored an empty key, and every later
// re-install saw the Secret and skipped, so exporting the key correctly and
// re-running `oap install` never actually fixed graphiti. The stored key is
// therefore reconciled against the current environment on every run: a
// non-empty env value that differs from what's stored is written; an unset
// env value never clobbers a key already stored (a shell that forgot to
// export it must not un-configure a working installation).
func ensureGraphitiConfigWithClient(ctx context.Context, out io.Writer, cli client.Client) (bool, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	var existing corev1.Secret
	err := cli.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system",
		Name:      "spicebox-graphiti-config",
	}, &existing)
	switch {
	case err == nil:
		storedKey := string(existing.Data["openai-api-key"])
		if apiKey == "" || storedKey == apiKey {
			return storedKey != "", nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["openai-api-key"] = []byte(apiKey)
		if err := cli.Update(ctx, &existing); err != nil {
			return false, fmt.Errorf("update graphiti config secret: %w", err)
		}
		cliout.Info(out, "graphiti: refreshed OPENAI_API_KEY in spicebox-graphiti-config")
		return true, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get graphiti config secret: %w", err)
	}
	if apiKey == "" {
		cliout.Warn(out, "OPENAI_API_KEY not set; KG entity extraction will not work until populated.")
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spicebox-graphiti-config",
			Namespace: "agentprimitives-system",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"openai-api-key": []byte(apiKey),
		},
	}
	if err := cli.Create(ctx, sec); err != nil {
		return false, fmt.Errorf("create graphiti config secret: %w", err)
	}
	return apiKey != "", nil
}

// Gateway/Route helpers (migrated from install_gateway.go).

const (
	webdTrustedRouteName = "spicebox-webd-trusted"
	webdSandboxRouteName = "spicebox-webd-sandbox"
)

// imagetoolsDigest resolves the digest a registry tag currently points to via
// `docker buildx imagetools inspect <ref> --format '{{json .Manifest}}'`, then
// reads the descriptor's top-level digest. It is a package var so tests can stub
// the registry round-trip.
//
// We deliberately do NOT use the more obvious `--format '{{.Manifest.Digest}}'`:
// current Docker Desktop buildx silently ignores that template and emits its
// default human-readable blob instead (Name:/MediaType:/Digest: lines), which the
// resolver would then mistake for a digest. `{{json .Manifest}}` is honored and
// its `digest` field is the descriptor digest for both a single-platform manifest
// and a multi-arch index — exactly the immutable ref we want to pin.
var imagetoolsDigest = func(ctx context.Context, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", ref, "--format", "{{json .Manifest}}").Output()
	if err != nil {
		return "", err
	}
	return parseImagetoolsManifestDigest(out)
}

// parseImagetoolsManifestDigest extracts the descriptor digest from the JSON
// `docker buildx imagetools inspect --format "{{json .Manifest}}"` emits. A parse
// failure here is what catches buildx falling back to its default human-readable
// output (e.g. when the older `{{.Manifest.Digest}}` template is ignored): the
// blob is not valid JSON, so we surface an actionable error rather than returning
// it as a bogus "digest".
func parseImagetoolsManifestDigest(jsonOut []byte) (string, error) {
	var desc struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(jsonOut, &desc); err != nil {
		return "", fmt.Errorf("parse imagetools manifest JSON (got %q): %w", strings.TrimSpace(string(jsonOut)), err)
	}
	return strings.TrimSpace(desc.Digest), nil
}

// imagesMissingDigests returns the catalog images with no entry in have — the
// set the install must still resolve from the registry itself.
//
// It exists because "the build already gave us digests" is not the same
// question as "do we have a digest for THIS image". `oap init` seeds
// tags.Digests from its own build, which expands over a subset of the catalog;
// the old gate (len(tags.Digests) == 0) skipped the fail-closed resolve
// entirely the moment that build contributed a single digest, so an image
// outside the build's expansion was never checked at all. That is how three
// SpiceboxToolchain CRs shipped pointing at images no registry held.
func imagesMissingDigests(have map[string]string) []apimage.Image {
	var out []apimage.Image
	for _, im := range apimage.Catalog() {
		if have[im.Name] == "" {
			out = append(out, im)
		}
	}
	return out
}

// resolveMissingRegistryDigests resolves the current digest of every first-party
// image absent from have, merged over have and returned as a new map (have is
// not mutated).
//
// Fail-closed: an image that cannot be resolved aborts the install rather than
// degrading to the mutable tag. The error names the exact `oap build` invocation
// that provisions it, because the operator's next question is always "so how do
// I fix it" and the answer differs per image.
//
// The error deliberately does NOT offer --no-digest-pin as a way past this.
// That flag opts out of pinning, not out of the image existing: it routes
// through verifyRegistryRefs, which runs the same imagetoolsDigest lookup
// against the same refs and fails on both of the causes named here (image
// absent, registry unreadable). Recommending it would walk the operator into a
// second wall about the same image.
func resolveMissingRegistryDigests(ctx context.Context, registry string, have map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(apimage.Catalog()))
	for k, v := range have {
		out[k] = v
	}
	for _, im := range imagesMissingDigests(have) {
		ref := im.RegistryRef(registry)
		d, err := imagetoolsDigest(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("could not resolve a digest for %s: %w — build and push it with `oap build %s --image-registry %s`; if it was already pushed, check that docker/buildx is authenticated to %s with read access (--no-digest-pin does not bypass this: it skips digest pinning, not the image existing)",
				ref, err, im.Target, registry, registry)
		}
		if !strings.HasPrefix(d, "sha256:") {
			return nil, fmt.Errorf("registry returned an unexpected digest %q for %s", d, ref)
		}
		out[im.Name] = d
	}
	return out, nil
}

// verifyRegistryRefs proves every reference the manifest rewrite will plant
// actually exists in the registry, WITHOUT pinning any of them. --no-digest-pin
// is a choice to accept a mutable tag, not a choice to install a reference to
// an image nobody pushed: the second one is not a weaker guarantee, it is a
// broken install that reports success and fails inside a session later.
//
// The digest is discarded — nothing is pinned here — but it is still validated
// against the sha256: prefix, exactly as resolveMissingRegistryDigests does.
// parseImagetoolsManifestDigest returns ("", nil) for any well-formed JSON
// object without a top-level `digest` field, so a drift in buildx's
// `{{json .Manifest}}` output shape (the failure imagetoolsDigest's doc comment
// describes, which has bitten this codebase once) would otherwise turn this
// whole loop into a no-op that "verifies" every image while checking none.
func verifyRegistryRefs(ctx context.Context, registry string) error {
	for _, im := range apimage.Catalog() {
		ref := im.RegistryRef(registry)
		d, err := imagetoolsDigest(ctx, ref)
		if err != nil {
			return fmt.Errorf("image %s is not in the registry: %w — build and push it with `oap build %s --image-registry %s`",
				ref, err, im.Target, registry)
		}
		if !strings.HasPrefix(d, "sha256:") {
			return fmt.Errorf("registry returned an unexpected digest %q for %s", d, ref)
		}
	}
	return nil
}

// localImagePresent reports whether ref exists in the local docker daemon. A
// package-level var so tests can answer it without shelling out.
var localImagePresent = func(ctx context.Context, ref string) bool {
	return exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run() == nil
}

// warnMissingLocalToolchainImages warns about language/agent toolchain overlays
// that are absent from the local docker daemon on a LOCAL install.
//
// It is the local counterpart of verifyRegistryRefs, and it warns where that
// one fails, because the two cases differ in what can actually be proven. A
// registry says authoritatively whether an image is there. Here the images the
// manifest plants are unqualified (`ap-toolchain-go:dev`), so the only thing
// that can serve them is the node's own image store — which this process cannot
// read. What it CAN prove is the contrapositive: an overlay absent from the
// local daemon was never built, so it cannot have been loaded into the node
// from this machine either. Presence is not proof of the converse (the image
// may be built but never loaded), which is exactly why this warns instead of
// failing.
//
// The gap is real: a local `oap build all` deliberately expands over
// apimage.All only — compiling gopls from source on every dev-loop build is the
// cost the overlays were split out to avoid — while the manifest rewrite plants
// a SpiceboxToolchain ref for every catalog image regardless. Without this, a
// local install has the same "exits 0, dies in a session days later" shape the
// registry path used to have.
func warnMissingLocalToolchainImages(ctx context.Context, out io.Writer, kctx string) {
	var missing []apimage.Image
	for _, im := range apimage.Toolchains {
		if !localImagePresent(ctx, im.LocalRef()) {
			missing = append(missing, im)
		}
	}
	if len(missing) == 0 {
		return
	}
	refs := make([]string, 0, len(missing))
	for _, im := range missing {
		refs = append(refs, im.LocalRef())
	}
	cliout.Warn(out, "toolchain overlay image(s) not built locally: %s", strings.Join(refs, ", "))
	cliout.Info(out, "  `oap build all` does not build these on purpose, but the install still registers them,")
	cliout.Info(out, "  so an agent whose sandbox composes one will fail to start with ErrImagePull. To provision:")
	for _, im := range missing {
		cmdline := "oap build " + im.Target
		if p := imageload.For(kctx, im.LocalRef()); p.Disposition == imageload.LocalLoad {
			cmdline += " && " + strings.Join(p.Argv, " ")
		}
		cliout.Info(out, "    %s", cmdline)
	}
}

// ptr returns a pointer to v. Used for Gateway API fields that require *T.
func ptr[T any](v T) *T { return &v }

// readWebdRouteHostname reads an installed webd HTTPRoute (trusted or sandbox)
// and returns its first spec.hostname, or "" when the route does not exist,
// its CRD isn't installed, or it carries no hostname. Neither a NotFound nor a
// NoKindMatch is an error — a fresh cluster simply has no route, and a cluster
// with no Gateway API CRDs at all (any --local install, or one ahead of `oap
// install`) can't even resolve HTTPRoute's REST mapping — but any other API
// failure is returned raw for the caller to wrap.
//
// Both the reconfigure guard (confirmWebdHostnameChange) and the wizard's
// cluster detection (detectSettings) read these two routes the same way, so the
// read lives here once rather than being copy-pasted into each.
func readWebdRouteHostname(ctx context.Context, c client.Client, ns, name string) (string, error) {
	var route gatewayv1.HTTPRoute
	switch err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &route); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return "", nil
	case err != nil:
		return "", err
	}
	if len(route.Spec.Hostnames) == 0 {
		return "", nil
	}
	return string(route.Spec.Hostnames[0]), nil
}

// confirmWebdHostnameChange reads the installed HTTPRoutes for the webd trusted
// and sandbox origins, compares their spec.hostnames[0] against the new flag
// values, and if any differ, warns the user and (in interactive mode) asks for
// confirmation before reconfiguring. Returns proceed=true when the caller should
// continue with webd external-access reconfiguration; proceed=false when the user
// declined (the existing Gateway/routes/DNS remain intact — the rest of the
// install is unaffected). Fresh installs (no existing routes) always return
// proceed=true with no output. Must NOT be called under --dry-run.
//
// isTTY mirrors the same parameter as apcmd.Confirm(): callers pass isInteractive() so
// tests can inject false without depending on os.Stdin's terminal state.
func confirmWebdHostnameChange(ctx context.Context, c client.Client, r WebdRoutingOpts, rep progress.Reporter, in io.Reader, out io.Writer, isTTY bool) (bool, error) {
	type hostChange struct{ label, oldVal, newVal string }
	var changes []hostChange

	readRoute := func(ns, name string) (string, error) {
		return readWebdRouteHostname(ctx, c, ns, name)
	}

	if r.trustedHostname != "" {
		existing, err := readRoute(cloud.WebdServiceNamespace, webdTrustedRouteName)
		if err != nil {
			return false, fmt.Errorf("read existing webd trusted HTTPRoute: %w", err)
		}
		if existing != "" && existing != r.trustedHostname {
			changes = append(changes, hostChange{"webd trusted hostname", existing, r.trustedHostname})
		}
	}

	if r.sandboxHostname != "" {
		existing, err := readRoute(cloud.WebdServiceNamespace, webdSandboxRouteName)
		if err != nil {
			return false, fmt.Errorf("read existing webd sandbox HTTPRoute: %w", err)
		}
		if existing != "" && existing != r.sandboxHostname {
			changes = append(changes, hostChange{"webd sandbox hostname", existing, r.sandboxHostname})
		}
	}

	if len(changes) == 0 {
		return true, nil
	}

	rep.Warn("webd external-access settings are changing on this existing install:")
	for _, ch := range changes {
		rep.Info("  %s: %s → %s", ch.label, ch.oldVal, ch.newVal)
	}
	rep.Warn("this reconfigures the Gateway, TLS certificates, and the DNS records you must set.")

	// Non-interactive or --yes: warn only, always proceed (don't block automation).
	if r.assumeYes || !isTTY {
		return true, nil
	}
	// Run the prompt under Suspend so the checklist's live region quiesces around
	// it — this runs mid-pipeline (webd external-access reconfigure) with rows on
	// screen, so a direct read/write would corrupt the region. The prompt keeps
	// using the caller's own in/out; Suspend only supplies the clear/redraw.
	var proceed bool
	rep.Suspend(func(_ io.Writer, _ io.Reader) {
		proceed = apcmd.Confirm(in, out, "Apply these webd hostname changes?", false, true)
	})
	return proceed, nil
}

// applyWebdHTTPRoute creates/updates an HTTPRoute attaching host to the Gateway
// and routing all paths to the webd Service. Idempotent.
func applyWebdHTTPRoute(ctx context.Context, c client.Client, name, host string) error {
	h := gatewayv1.Hostname(host)
	desired := gatewayv1.HTTPRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(cloud.WebdGatewayName)}},
		},
		Hostnames: []gatewayv1.Hostname{h},
		Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(cloud.WebdServiceName),
						Port: ptr(gatewayv1.PortNumber(cloud.WebdServicePort)),
					},
				},
			}},
		}},
	}

	var existing gatewayv1.HTTPRoute
	key := client.ObjectKey{Namespace: cloud.WebdServiceNamespace, Name: name}
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		existing.Spec = desired
		return c.Update(ctx, &existing)
	case apierrors.IsNotFound(err):
		return c.Create(ctx, &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: cloud.WebdServiceNamespace, Name: name},
			Spec:       desired,
		})
	default:
		return fmt.Errorf("get HTTPRoute %s/%s: %w", cloud.WebdServiceNamespace, name, err)
	}
}

// installRepoRoot returns the path the AI-fix prompt points the launched CLI at:
// the git repository root when invoked inside a checkout, falling back to the
// working directory. Best-effort — the prompt is still useful with just the cwd.
func installRepoRoot() string {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
