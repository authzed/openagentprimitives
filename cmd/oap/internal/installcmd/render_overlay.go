package installcmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// processBaseBytes applies the render's image substitution and memory-token
// linking to a raw bundle. Image refs are rewritten by the SAME textual
// substitution oap install uses (reaching container AND non-container refs,
// no-op for a local install); memory-token links rewrite every reference to a
// linked component's default Secret name. Both are byte replacements, so the
// output stays well-formed and diffable.
func processBaseBytes(raw []byte, cfg RenderConfig) ([]byte, error) {
	out, err := manifests.Substitute(raw, cfg.Tags)
	if err != nil {
		return nil, err
	}
	return applyMemoryTokenLinks(out, cfg.MemoryToken), nil
}

// processBundle loads a separate embedded bundle (channelsd/nats), injects the
// storage class into any stateful doc (a no-op for the rest), joins, and applies
// the same image + memory-token processing as the base.
func processBundle(load func() ([][]byte, error), cfg RenderConfig, storageClass string) ([]byte, error) {
	docs, err := load()
	if err != nil {
		return nil, err
	}
	if storageClass != "" {
		for i := range docs {
			m, err := manifests.InjectStorageClass(docs[i], storageClass)
			if err != nil {
				return nil, fmt.Errorf("inject storageClassName=%q: %w", storageClass, err)
			}
			docs[i] = m
		}
	}
	return processBaseBytes(bytes.Join(docs, []byte("\n---\n")), cfg)
}

// applyMemoryTokenLinks rewrites each linked component's default memory-token
// Secret name to the user-supplied existing name, across the whole base (every
// reference). An unset link leaves the default name (the operator mints it).
// Index-aligned with bootstrap.MemoryTokenSecretNames: [channelsd, webd, authzd].
func applyMemoryTokenLinks(base []byte, links MemoryTokenLinks) []byte {
	repl := func(b []byte, def, linked string) []byte {
		if linked == "" {
			return b
		}
		return bytes.ReplaceAll(b, []byte(def), []byte(linked))
	}
	base = repl(base, bootstrap.MemoryTokenSecretNames[0], links.Channelsd)
	base = repl(base, bootstrap.MemoryTokenSecretNames[1], links.Webd)
	base = repl(base, bootstrap.MemoryTokenSecretNames[2], links.Authzd)
	return base
}

// clusterKindPatchFile is the overlay patch that stamps AP_CLUSTER_KIND onto the
// operator and webd Deployments. Kept as a relative path constant so the writer
// and the kustomization emitter cannot disagree.
const clusterKindPatchFile = "patches/cluster-kind.yaml"

// WriteOverlay renders a self-contained kustomize overlay into cfg.OutputDir. It
// is a pure function of cfg (and the digests already resolved into cfg.Tags):
// no wall-clock, no randomness, deterministic ordering — so re-rendering
// identical inputs is byte-identical and GitOps controllers see no drift.
func WriteOverlay(cfg RenderConfig) error {
	baseDir := filepath.Join(cfg.OutputDir, "base")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	// The base is the embedded bundle with first-party image refs rewritten by
	// the SAME textual substitution oap install uses (manifests.Substitute). This
	// reaches every ref — container AND non-container (toolchain overlays, AgentClass
	// runner image) — which a kustomize `images:` transformer cannot, and is a no-op
	// for a local install (empty Tags). The result is still well-formed YAML (a byte
	// replace preserves formatting), so the base stays diffable.
	// The core workload set is base/install.yaml (= the embedded `kubectl
	// kustomize config/` bundle) PLUS channelsd and nats, which `oap install`
	// applies from separate embedded bundles (they are not in config/). All three
	// are always-on; without channelsd + nats the install does not function.
	// Each is image-substituted and memory-token-linked identically; nats, a
	// StatefulSet, also takes the storage class.
	install, err := processBaseBytes(manifests.Install, cfg)
	if err != nil {
		return fmt.Errorf("process base: %w", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "install.yaml"), install, 0o644); err != nil {
		return err
	}
	channelsd, err := processBundle(manifests.ChannelsD, cfg, "")
	if err != nil {
		return fmt.Errorf("process channelsd: %w", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "channelsd.yaml"), channelsd, 0o644); err != nil {
		return err
	}
	nats, err := processBundle(manifests.NATS, cfg, cfg.StorageClass)
	if err != nil {
		return fmt.Errorf("process nats: %w", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "nats.yaml"), nats, 0o644); err != nil {
		return err
	}

	// AP_CLUSTER_KIND is stamped as an overlay patch (not baked into the base) so
	// the one value a GitOps reader most wants to see is visible in the overlay,
	// not buried in a 1.5 MB base. The operator and webd both fail closed when it
	// is unset, so it is emitted on every render.
	patchDir := filepath.Join(cfg.OutputDir, "patches")
	if err := os.MkdirAll(patchDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.OutputDir, clusterKindPatchFile), clusterKindPatch(cfg.Strat.Key()), 0o644); err != nil {
		return err
	}

	// Routing is opt-in: emit the Gateway + HTTPRoute(s) only when a trusted
	// hostname is configured. renderKustomization includes routing.yaml in
	// resources under the same condition, so the two cannot disagree.
	routing, emitted, err := renderRoutingResources(cfg.Routing)
	if err != nil {
		return err
	}
	if emitted {
		if err := os.WriteFile(filepath.Join(cfg.OutputDir, routingResourceFile), routing, 0o644); err != nil {
			return err
		}
	}

	// Pre-start Job that mints the internal Secrets (always emitted — a GitOps
	// apply has no `oap install` to mint them).
	job, err := renderBootstrapJob(cfg)
	if err != nil {
		return fmt.Errorf("render bootstrap job: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.OutputDir, bootstrapJobFile), job, 0o644); err != nil {
		return err
	}

	// Webhook serving cert via cert-manager (unless the user provides it). The
	// Issuer+Certificate populate spicebox-webhook-tls; the VWC patch below adds
	// the ca-injector annotation.
	if !cfg.ExternalWebhookTLS {
		if err := os.WriteFile(filepath.Join(cfg.OutputDir, webhookCertResourceFile), renderWebhookCertResources(), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cfg.OutputDir, webhookCAInjectPatchFile), webhookCAInjectPatch(), 0o644); err != nil {
			return err
		}
	}

	// Optional dependencies (postgres/graphiti/neo4j) become kustomize Components,
	// pulled in via `components:` only when their --with-* flag is set.
	if err := writeDependencyComponents(cfg); err != nil {
		return err
	}

	k, err := renderKustomization(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.OutputDir, "kustomization.yaml"), k, 0o644)
}

// clusterKindPatch is a two-document strategic-merge patch setting the
// AP_CLUSTER_KIND env on the operator and webd app containers (both single-
// container Deployments; the container names `operator`/`webd` are the strategic-
// merge keys). A pure function of key.
func clusterKindPatch(key string) []byte {
	one := func(deployment, container string) string {
		return "apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: " + deployment + "\n" +
			"  namespace: " + cloud.WebdServiceNamespace + "\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: " + container + "\n" +
			"          env:\n" +
			"            - name: " + cloud.ClusterKindEnvVar + "\n" +
			"              value: " + key + "\n"
	}
	return []byte(one(cloud.OperatorDeploymentName, "operator") + "---\n" + one(webdDeploymentName, "webd"))
}

// renderKustomization builds the overlay kustomization.yaml bytes
// deterministically. Components and additional patches are appended by later
// render stages; order is fixed so re-renders are byte-identical.
func renderKustomization(cfg RenderConfig) ([]byte, error) {
	resources := "resources:\n  - base/install.yaml\n  - base/channelsd.yaml\n  - base/nats.yaml\n  - " + bootstrapJobFile + "\n"
	if cfg.Routing.trustedHostname != "" {
		resources += "  - " + routingResourceFile + "\n"
	}
	if !cfg.ExternalWebhookTLS {
		resources += "  - " + webhookCertResourceFile + "\n"
	}
	patches := "patches:\n  - path: " + clusterKindPatchFile + "\n"
	if !cfg.ExternalWebhookTLS {
		patches += "  - path: " + webhookCAInjectPatchFile + "\n"
	}
	k := "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		resources +
		patches
	if names := enabledComponentNames(cfg); len(names) > 0 {
		k += "components:\n"
		for _, n := range names {
			k += "  - components/" + n + "\n"
		}
	}
	return []byte(k), nil
}

// buildOverlay runs kustomize build in-process (krusty, no binary dependency)
// over dir and returns the rendered YAML. Used both as a render-time self-check
// and by `oap render --validate`.
func buildOverlay(dir string) ([]byte, error) {
	fsys := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	res, err := k.Run(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	return res.AsYaml()
}
