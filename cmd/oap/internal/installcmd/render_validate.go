package installcmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// runRenderValidate lints an already-rendered overlay at dir. It is the runtime
// twin of pkg/platform/manifests/secret_bootstrap_test.go, run against the built
// output rather than the embedded bundle, plus the GitOps-specific checks a
// synced cluster depends on. cfg carries the render's own decisions (linked
// token Secrets, digest-pinning) so the lint does not false-positive on them.
//
// What it CANNOT check (no cluster): whether a LINKED Secret actually exists —
// that is a cluster-state fact `oap check` verifies post-sync. The lint confirms
// the overlay is structurally complete and will sync, not that the external
// dependencies it references are present.
func runRenderValidate(ctx context.Context, out io.Writer, cfg RenderConfig, dir string) error {
	built, err := buildOverlay(dir)
	if err != nil {
		return err // a non-buildable overlay is the first thing that would fail at sync
	}
	docs, err := manifests.Split(built)
	if err != nil {
		return fmt.Errorf("parse built overlay: %w", err)
	}

	var problems []string

	// 1. Every non-optional Secret reference has a creator: shipped in the bundle,
	// minted by the bootstrap Job / operator, or linked by this render. A
	// reference to an app-internal secret nothing here yet provisions (the NATS
	// stack, webhook-tls, or a token the user externalized) is not a hard failure
	// — it is listed as "provide this".
	satisfied := shippedSecretNames(docs)
	for _, n := range jobMintedSecretNames(cfg) {
		satisfied[n] = true
	}
	// webhook-tls is populated by the emitted cert-manager Certificate (unless
	// the user provides it themselves).
	if !cfg.ExternalWebhookTLS {
		satisfied["spicebox-webhook-tls"] = true
	}
	for _, n := range []string{cfg.MemoryToken.Channelsd, cfg.MemoryToken.Webd, cfg.MemoryToken.Authzd} {
		if n != "" {
			satisfied[n] = true
		}
	}
	external := externalSecretNames(cfg)
	provideExternally := map[string]bool{}
	for _, ref := range nonOptionalSecretRefs(docs) {
		switch {
		case satisfied[ref.secret]:
			// satisfied
		case external[ref.secret]:
			provideExternally[ref.secret] = true
		default:
			problems = append(problems, fmt.Sprintf("%s/%s references Secret %q with no creator (not shipped, not minted by the bootstrap Job, not linked)", ref.workloadKind, ref.workload, ref.secret))
		}
	}

	// 2. AP_CLUSTER_KIND is stamped on the operator and webd (both fail closed
	// without it).
	for _, dep := range []string{cloud.OperatorDeploymentName, webdDeploymentName} {
		if !deploymentHasEnv(docs, dep, cloud.ClusterKindEnvVar) {
			problems = append(problems, fmt.Sprintf("Deployment %s is missing the %s env var", dep, cloud.ClusterKindEnvVar))
		}
	}

	// 3. First-party images are pinned (no mutable :dev), unless --no-digest-pin.
	if !cfg.NoDigestPin {
		for _, img := range firstPartyMutableImages(built) {
			problems = append(problems, fmt.Sprintf("image %q uses a mutable tag; GitOps wants an immutable digest (render with a registry, or pass --no-digest-pin to accept this)", img))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("overlay validation found %d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	cliout.Step(out, "overlay validated: %d resources, all Secret references satisfied, cluster-kind stamped", len(docs))
	if len(provideExternally) > 0 {
		names := make([]string, 0, len(provideExternally))
		for n := range provideExternally {
			names = append(names, n)
		}
		sort.Strings(names)
		cliout.Warn(out, "provide these app-internal Secret(s) yourself until a later release has the operator mint them (e.g. via External Secrets Operator or a sealed secret):\n  - %s", strings.Join(names, "\n  - "))
	}
	return nil
}

// jobMintedSecretNames is the set of internal Secrets the pre-start bootstrap Job
// mints for this config — so --validate treats their references as satisfied.
// The memory tokens, admind token and passthrough key are always minted; the
// SpiceDB/Postgres tokens are minted unless the user externalized that instance.
func jobMintedSecretNames(cfg RenderConfig) []string {
	names := append([]string{}, bootstrap.MemoryTokenSecretNames...)
	names = append(names, bootstrap.AdmindTokenSecretName, v1alpha1.PassthroughLinkSigningKeySecret,
		"spicebox-nats-identity", "spicebox-nats-tls", "spicebox-channelsd-nats-creds", "spicebox-webd-nats-creds",
		"spicebox-postgres-token", "spicebox-spicedb-token")
	out := names[:0:len(names)]
	for _, n := range names {
		if n == "spicebox-spicedb-token" && cfg.ExternalSpiceDB {
			continue
		}
		if n == "spicebox-postgres-token" && cfg.ExternalPostgres {
			continue
		}
		out = append(out, n)
	}
	return out
}

// externalSecretNames are the Secrets the USER chose to provide themselves via
// the --external-* flags (an external SpiceDB/Postgres instance, or webhook-tls
// on a cluster without cert-manager). --validate lists these as "provide these"
// rather than failing. By default the overlay provisions every internal Secret
// (the bootstrap Job + the cert-manager webhook Certificate), so this set is
// empty and --validate reports nothing to provide.
func externalSecretNames(cfg RenderConfig) map[string]bool {
	out := map[string]bool{}
	if cfg.ExternalWebhookTLS {
		out["spicebox-webhook-tls"] = true
	}
	if cfg.ExternalSpiceDB {
		out["spicebox-spicedb-token"] = true
	}
	if cfg.ExternalPostgres {
		out["spicebox-postgres-token"] = true
	}
	return out
}

// shippedSecretNames returns the set of Secret object names present in docs.
func shippedSecretNames(docs []*unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}
	for _, d := range docs {
		if d.GetKind() == "Secret" {
			out[d.GetName()] = true
		}
	}
	return out
}

type secretRef struct {
	workloadKind string
	workload     string
	secret       string
}

// nonOptionalSecretRefs collects every required (optional!=true) Secret reference
// from pod templates in docs — env valueFrom.secretKeyRef and volume secrets.
// A missing optional field defaults to required, matching the kubelet.
func nonOptionalSecretRefs(docs []*unstructured.Unstructured) []secretRef {
	var refs []secretRef
	for _, d := range docs {
		spec, found, _ := unstructured.NestedMap(d.Object, "spec", "template", "spec")
		if !found {
			continue
		}
		for _, cf := range []string{"containers", "initContainers"} {
			containers, _, _ := unstructured.NestedSlice(spec, cf)
			for _, c := range containers {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				env, _, _ := unstructured.NestedSlice(cm, "env")
				for _, e := range env {
					em, ok := e.(map[string]any)
					if !ok {
						continue
					}
					skr, found, _ := unstructured.NestedMap(em, "valueFrom", "secretKeyRef")
					if !found {
						continue
					}
					if optionalTrue(skr) {
						continue
					}
					if name, ok, _ := unstructured.NestedString(skr, "name"); ok {
						refs = append(refs, secretRef{d.GetKind(), d.GetName(), name})
					}
				}
			}
		}
		volumes, _, _ := unstructured.NestedSlice(spec, "volumes")
		for _, v := range volumes {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			sec, found, _ := unstructured.NestedMap(vm, "secret")
			if !found {
				continue
			}
			if optionalTrue(sec) {
				continue
			}
			if name, ok, _ := unstructured.NestedString(sec, "secretName"); ok {
				refs = append(refs, secretRef{d.GetKind(), d.GetName(), name})
			}
		}
	}
	return refs
}

func optionalTrue(m map[string]any) bool {
	b, ok, _ := unstructured.NestedBool(m, "optional")
	return ok && b
}

// deploymentHasEnv reports whether the named Deployment has an env var of the
// given name on any of its containers.
func deploymentHasEnv(docs []*unstructured.Unstructured, name, envName string) bool {
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != name {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			env, _, _ := unstructured.NestedSlice(cm, "env")
			for _, e := range env {
				if em, ok := e.(map[string]any); ok {
					if n, ok, _ := unstructured.NestedString(em, "name"); ok && n == envName {
						return true
					}
				}
			}
		}
	}
	return false
}

// firstPartyMutableImages returns first-party image refs in the built YAML that
// are NOT digest-pinned (a mutable "<name>:tag" with no @sha256). Scans textually
// since image refs appear in container specs and a few CR fields alike.
func firstPartyMutableImages(built []byte) []string {
	var mutable []string
	text := string(built)
	for _, img := range apimage.All {
		// A pinned ref contains "<name>...@sha256:"; a mutable one is "<name>:dev"
		// (or any tag) with no digest. Flag the bare ":dev" refs that survive.
		needle := img.Name + ":dev"
		if strings.Contains(text, needle) && !strings.Contains(text, img.Name+":dev@sha256:") {
			mutable = append(mutable, needle)
		}
	}
	return mutable
}
