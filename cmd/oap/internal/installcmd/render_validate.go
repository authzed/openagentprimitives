package installcmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
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
	for _, ref := range secretRefsWhere(docs, func(r secretRef) bool { return !r.optional }) {
		switch {
		case satisfied[ref.secret]:
			// satisfied
		case external[ref.secret]:
			provideExternally[ref.secret] = true
		default:
			problems = append(problems, fmt.Sprintf("%s/%s references Secret %q with no creator (not shipped, not minted by the bootstrap Job, not linked)", ref.workloadKind, ref.workload, ref.secret))
		}
	}

	// 2. No workload references an awaited Secret optionally. Such a pod can
	// start before the bootstrap Job has minted the Secret (or the user has
	// provided it) and, reading it once at startup, never pick it up — the
	// overlay's required-secrets patch exists to prevent exactly that.
	awaited := awaitedSecretNames(cfg)
	for _, ref := range secretRefsWhere(docs, func(r secretRef) bool { return r.optional && awaited[r.secret] }) {
		problems = append(problems, fmt.Sprintf("%s/%s references Secret %q as optional: its pod can start before the Secret exists and never read it (the overlay's %s must make it required)", ref.workloadKind, ref.workload, ref.secret, requiredSecretsPatchFile))
	}

	// 3. AP_CLUSTER_KIND is stamped on the operator and webd (both fail closed
	// without it).
	for _, dep := range []string{cloud.OperatorDeploymentName, webdDeploymentName} {
		if !deploymentHasEnv(docs, dep, cloud.ClusterKindEnvVar) {
			problems = append(problems, fmt.Sprintf("Deployment %s is missing the %s env var", dep, cloud.ClusterKindEnvVar))
		}
	}

	// 4. SpiceDB is reachable: the endpoint ConfigMap every client reads is
	// present, and when it names the in-cluster Service, the SpiceDBCluster that
	// serves it ships too. Without these the overlay syncs green and nothing can
	// authorize.
	problems = append(problems, spicedbProblems(docs)...)

	// 5. First-party images are pinned (no mutable :dev), unless --no-digest-pin.
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
// It is bootstrap.MintedSecretNames under the same skips bootstrapJobArgs passes
// the Job, so the two cannot disagree.
func jobMintedSecretNames(cfg RenderConfig) []string {
	return bootstrap.MintedSecretNames(bootstrap.MintOptions{
		SkipSpiceDBToken:  cfg.externalSpiceDB(),
		SkipPostgresToken: cfg.ExternalPostgres,
	})
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
	if cfg.externalSpiceDB() {
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

// secretRef is one reference to a Secret from a workload's pod template.
type secretRef struct {
	workloadKind string
	workload     string
	secret       string
	// optional is the reference's own `optional: true`; unset means required,
	// as the kubelet reads it.
	optional bool
	// Where the reference sits, for requiredSecretsPatch: an env
	// valueFrom.secretKeyRef (containerList is "containers" or
	// "initContainers"), or a `secret:` volume. All empty for an envFrom or a
	// projected-volume source, which carry no strategic-merge key.
	containerList, container, env string
	volume                        string
}

// podSecretRefs collects every Secret reference in d's pod template: env
// valueFrom.secretKeyRef, envFrom secretRef, volume secrets and
// projected-volume secret sources, across containers and initContainers.
func podSecretRefs(d *unstructured.Unstructured) []secretRef {
	spec, found, _ := unstructured.NestedMap(d.Object, "spec", "template", "spec")
	if !found {
		return nil
	}
	var refs []secretRef
	add := func(ref map[string]any, nameKey string, at secretRef) {
		if name, ok, _ := unstructured.NestedString(ref, nameKey); ok {
			at.workloadKind, at.workload, at.secret, at.optional = d.GetKind(), d.GetName(), name, optionalTrue(ref)
			refs = append(refs, at)
		}
	}
	for _, cf := range []string{"containers", "initContainers"} {
		for _, cm := range mapsAt(spec, cf) {
			container, _, _ := unstructured.NestedString(cm, "name")
			for _, em := range mapsAt(cm, "env") {
				if skr, found, _ := unstructured.NestedMap(em, "valueFrom", "secretKeyRef"); found {
					env, _, _ := unstructured.NestedString(em, "name")
					add(skr, "name", secretRef{containerList: cf, container: container, env: env})
				}
			}
			for _, efm := range mapsAt(cm, "envFrom") {
				if sr, found, _ := unstructured.NestedMap(efm, "secretRef"); found {
					add(sr, "name", secretRef{})
				}
			}
		}
	}
	for _, vm := range mapsAt(spec, "volumes") {
		if sec, found, _ := unstructured.NestedMap(vm, "secret"); found {
			volume, _, _ := unstructured.NestedString(vm, "name")
			add(sec, "secretName", secretRef{volume: volume})
		}
		for _, src := range mapsAt(vm, "projected", "sources") {
			if sec, found, _ := unstructured.NestedMap(src, "secret"); found {
				add(sec, "name", secretRef{})
			}
		}
	}
	return refs
}

// secretRefsWhere returns the references across docs' pod templates that keep
// reports true for.
func secretRefsWhere(docs []*unstructured.Unstructured, keep func(secretRef) bool) []secretRef {
	var refs []secretRef
	for _, d := range docs {
		for _, ref := range podSecretRefs(d) {
			if keep(ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// mapsAt returns the map elements of the list at path in m (non-maps skipped).
func mapsAt(m map[string]any, path ...string) []map[string]any {
	list, _, _ := unstructured.NestedSlice(m, path...)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if em, ok := e.(map[string]any); ok {
			out = append(out, em)
		}
	}
	return out
}

// spicedbProblems checks that the built overlay gives its SpiceDB clients
// something to connect to (see runRenderValidate step 3).
func spicedbProblems(docs []*unstructured.Unstructured) []string {
	var endpoint string
	var cluster bool
	for _, d := range docs {
		switch {
		case d.GetKind() == "ConfigMap" && d.GetName() == spicedb.SharedConfigMapName:
			endpoint, _, _ = unstructured.NestedString(d.Object, "data", spicedb.SharedConfigMapEndpointKey)
		case d.GetKind() == "SpiceDBCluster" && d.GetName() == "spicebox-spicedb":
			cluster = true
		}
	}
	switch {
	case endpoint == "":
		return []string{fmt.Sprintf("no ConfigMap %s with a %q key: the SpiceDB clients have no endpoint", spicedb.SharedConfigMapName, spicedb.SharedConfigMapEndpointKey)}
	case endpoint == inClusterSpiceDBEndpoint && !cluster:
		return []string{fmt.Sprintf("ConfigMap %s points at the in-cluster SpiceDB (%s) but no SpiceDBCluster ships to serve it", spicedb.SharedConfigMapName, endpoint)}
	}
	return nil
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
// are NOT digest-pinned. Every occurrence is checked on its own: one pinned
// reference must not vouch for an unpinned one elsewhere. Scans textually since
// image refs appear in container specs and a few CR fields alike.
func firstPartyMutableImages(built []byte) []string {
	var mutable []string
	text := string(built)
	for _, img := range apimage.All {
		// A pinned ref is "<name>:<tag>@sha256:..."; flag any "<name>:dev"
		// occurrence not followed by a digest.
		needle := img.Name + ":dev"
		for i := strings.Index(text, needle); i >= 0; {
			end := i + len(needle)
			if !strings.HasPrefix(text[end:], "@sha256:") {
				mutable = append(mutable, needle)
				break
			}
			next := strings.Index(text[end:], needle)
			if next < 0 {
				break
			}
			i = end + next
		}
	}
	return mutable
}
