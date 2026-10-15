package installcmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// MemoryTokenLinks names existing Secrets to link per component instead of
// operator-minting. An empty string means "operator mints this one" (the
// default). The three are kept separate on purpose: webd must not hold a
// write-capable memory credential, so linking is per-component.
type MemoryTokenLinks struct {
	Channelsd string
	Webd      string
	Authzd    string
}

// RenderConfig is the full input to `oap render`. It is populated from flags and
// is the sole input to WriteOverlay (which must be a pure function of it).
type RenderConfig struct {
	OutputDir    string
	ClusterKind  string
	Tags         manifests.Tags
	StorageClass string
	Routing      WebdRoutingOpts
	WithPostgres bool
	MemoryToken  MemoryTokenLinks
	NoDigestPin  bool
	// ExternalSpiceDB/ExternalPostgres: the user points at an EXTERNAL instance and
	// provides its token Secret themselves, so the bootstrap Job must NOT mint that
	// token (it would fight the external instance).
	ExternalSpiceDB  bool
	ExternalPostgres bool
	// ExternalWebhookTLS: do not emit the cert-manager Issuer/Certificate for the
	// operator webhook serving cert; you provide spicebox-webhook-tls + the VWC
	// caBundle yourself (for clusters without cert-manager).
	ExternalWebhookTLS bool
	Strat              cloud.Strategy
}

func NewRenderCmd(g *apcmd.Globals) *cobra.Command {
	cfg := RenderConfig{OutputDir: "deploy/oap"}
	var validate bool
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render the install as a GitOps-ready kustomize overlay (no cluster mutation)",
		Long: "Render the Agent Primitives install as a self-contained kustomize overlay for Argo CD / Flux.\n\n" +
			"Unlike `oap install`, render never mutates a cluster (its only remote call is read-only image-digest " +
			"resolution, skipped with --no-digest-pin). Commit the output directory and point your GitOps controller " +
			"at it. The overlay provisions every internal Secret: a pre-start Job mints the tokens/NATS material " +
			"before the operator starts, and a cert-manager Certificate populates the webhook cert (so cert-manager " +
			"is a dependency). Use --external-spicedb/--external-postgres/--external-webhook-tls to provide any of " +
			"those yourself, or link an existing memory token with the per-component --*-memory-token-secret flags. " +
			"Verify a synced cluster with `oap check`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRender(cmd.Context(), cmd.OutOrStdout(), g, cfg, validate)
		},
	}
	cmd.Flags().StringVar(&cfg.OutputDir, "output-dir", cfg.OutputDir, "Directory to write the kustomize overlay into")
	cmd.Flags().StringVar(&cfg.ClusterKind, "cluster-kind", "", clusterKindFlagUsage)
	cmd.Flags().StringVar(&cfg.Tags.Registry, "image-registry", "", "Rewrite first-party images to <registry>/<name>")
	cmd.Flags().StringVar(&cfg.StorageClass, "storage-class", "", "StorageClass for stateful PVCs/StatefulSets")
	cmd.Flags().BoolVar(&cfg.WithPostgres, "with-postgres", false, "Include the Postgres dependency component")
	cmd.Flags().BoolVar(&cfg.NoDigestPin, "no-digest-pin", false, "Emit mutable tags instead of pinning digests (skips registry digest resolution)")
	cmd.Flags().BoolVar(&cfg.ExternalSpiceDB, "external-spicedb", false, "You run an external SpiceDB and provide spicebox-spicedb-token yourself; the bootstrap Job will not mint it")
	cmd.Flags().BoolVar(&cfg.ExternalPostgres, "external-postgres", false, "You run an external Postgres and provide spicebox-postgres-token yourself; the bootstrap Job will not mint it")
	cmd.Flags().BoolVar(&cfg.ExternalWebhookTLS, "external-webhook-tls", false, "Do not emit cert-manager resources for the operator webhook cert; you provide spicebox-webhook-tls + the VWC caBundle yourself (clusters without cert-manager)")
	// Per-component only, by design: there is deliberately no "one Secret for all
	// three" flag — webd's token must differ from channelsd/authzd (it is
	// read-only; they are write-capable), and a single shared value would break
	// that (see validateRenderFlags).
	cmd.Flags().StringVar(&cfg.MemoryToken.Channelsd, "channelsd-memory-token-secret", "", "Link channelsd memory token to an existing Secret (default: operator mints)")
	cmd.Flags().StringVar(&cfg.MemoryToken.Webd, "webd-memory-token-secret", "", "Link webd memory token to an existing Secret; must differ from the channelsd/authzd Secrets (webd is read-only)")
	cmd.Flags().StringVar(&cfg.MemoryToken.Authzd, "authzd-memory-token-secret", "", "Link authzd memory token to an existing Secret (default: operator mints)")
	cmd.Flags().StringVar(&cfg.Routing.trustedHostname, "trusted-hostname", "", "Bare hostname for webd's trusted origin; emits a Gateway + HTTPRoute (opt-in). Requires --gateway-class. Omit to bring your own ingress.")
	cmd.Flags().StringVar(&cfg.Routing.sandboxHostname, "sandbox-hostname", "", "Bare hostname for webd's artifact-viewer sandbox origin (adds a second HTTPS listener + HTTPRoute). Requires --trusted-hostname.")
	cmd.Flags().StringVar(&cfg.Routing.gatewayClass, "gateway-class", "", "GatewayClass name for the emitted Gateway (required with --trusted-hostname; render cannot auto-detect it).")
	cmd.Flags().BoolVar(&validate, "validate", false, "After rendering, lint the overlay for completeness and consistency")
	return cmd
}

// runRender resolves the cluster strategy (without mutating the cluster), writes
// the overlay, and self-checks it. g is used only for lazy, read-only registry
// digest resolution (added by a later stage), never for mutation.
func runRender(ctx context.Context, out io.Writer, g *apcmd.Globals, cfg RenderConfig, validate bool) error {
	// Resolve the strategy WITHOUT a cluster call: cloud.For on an explicit kind,
	// cloud.Default when unset. A render targeting a specific GitOps cluster kind
	// passes --cluster-kind, so the empty-kind path is the rare "just give me the
	// default shape" case.
	if cfg.Strat == nil {
		if cfg.ClusterKind == "" {
			strat, err := cloud.Default()
			if err != nil {
				return err
			}
			cfg.Strat = strat
		} else {
			strat, err := cloud.For(cfg.ClusterKind)
			if err != nil {
				return err
			}
			cfg.Strat = strat
		}
	}

	if err := validateRenderFlags(cfg); err != nil {
		return err
	}

	// Registry digest resolution is the ONLY remote call render makes, and it is
	// read-only. Skipped entirely under --no-digest-pin or a local (no-registry)
	// install, so an offline render never reaches out. It does not touch the
	// target cluster (it queries the image registry), so g is not consulted here.
	if cfg.Tags.Registry != "" && !cfg.NoDigestPin {
		digests, err := resolveMissingRegistryDigests(ctx, cfg.Tags.Registry, cfg.Tags.Digests)
		if err != nil {
			return err
		}
		cfg.Tags.Digests = digests
	}

	if err := WriteOverlay(cfg); err != nil {
		return err
	}
	cliout.Step(out, "rendered kustomize overlay to %s", cfg.OutputDir)

	if validate {
		return runRenderValidate(ctx, out, cfg, cfg.OutputDir)
	}
	if _, err := buildOverlay(cfg.OutputDir); err != nil {
		return fmt.Errorf("rendered overlay failed to build: %w", err)
	}
	return nil
}

// validateRenderFlags rejects configurations that cannot produce a safe GitOps
// bundle.
//
// The memory-datastore refusal is the load-bearing one: local/desktop run an
// in-memory SpiceDB whose base schema is seeded at startup via
// --datastore-bootstrap-files (spicedbModeFor(...).bootstrapsSchemaAtStartup()).
// That is safe ONLY for an ephemeral, single-replica datastore — against the
// durable, multi-replica cluster a GitOps render targets it is fatal once the
// datastore is non-empty (one replica wins, the rest crashloop forever). The
// operator-applied schema path (pkg/platform/bootstrap) is what persistent
// datastores use instead, so a memory-datastore kind simply is not a GitOps
// target. Refuse it rather than emit an unsafe bundle.
func validateRenderFlags(cfg RenderConfig) error {
	if cfg.Strat != nil && spicedbModeFor(cfg.Strat.InstallProfile().SpiceDBDatastore()).bootstrapsSchemaAtStartup() {
		return fmt.Errorf("cluster kind %q uses an in-memory SpiceDB datastore whose schema is bootstrapped at startup, which is fatal against the durable, multi-replica cluster a GitOps render targets; render a persistent-datastore kind (the operator applies the schema there)", cfg.Strat.Key())
	}
	if cfg.Routing.trustedHostname == "" && cfg.Routing.sandboxHostname != "" {
		return fmt.Errorf("--sandbox-hostname requires --trusted-hostname")
	}
	if cfg.Routing.trustedHostname != "" && cfg.Routing.gatewayClass == "" {
		return fmt.Errorf("--trusted-hostname requires --gateway-class (render cannot auto-detect the cluster's GatewayClass)")
	}
	// webd's memory token is READ-ONLY by value (pkg/memory/tokens): the channelsd
	// and authzd tokens are accepted for writes (append/forge the audit log), the
	// webd one is not, because webd is browser-facing. Token capability is a plain
	// value compare, so webd mounting the SAME Secret as channelsd or authzd
	// silently hands webd that write capability. Compare EFFECTIVE names — the
	// link if set, else the default name the bootstrap Job mints — because the Job
	// mints the defaults regardless of links: `--webd-memory-token-secret` pointed
	// at channelsd's default name (channelsd left to mint) is the same leak as
	// linking both to one Secret, and must be refused just the same.
	eff := func(link string, i int) string {
		if link != "" {
			return link
		}
		return bootstrap.MemoryTokenSecretNames[i]
	}
	webd := eff(cfg.MemoryToken.Webd, 1)
	if webd == eff(cfg.MemoryToken.Channelsd, 0) || webd == eff(cfg.MemoryToken.Authzd, 2) {
		return fmt.Errorf("webd's memory token (--webd-memory-token-secret, or its operator-minted default %q) must resolve to a different Secret than the channelsd/authzd memory tokens: webd is browser-facing and must not hold a write-capable memory credential", bootstrap.MemoryTokenSecretNames[1])
	}
	return nil
}
