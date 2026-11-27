package installcmd

import (
	"bytes"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// requiredSecretsPatchFile is the overlay patch that makes every workload wait
// for the Secrets a GitOps install provisions after the workloads are applied.
const requiredSecretsPatchFile = "patches/required-secrets.yaml"

// awaitedSecretNames is every Secret that does not exist when a GitOps
// controller first applies the overlay but will: the ones the bootstrap Job
// mints, the ones the user opted to provide (--external-*), and linked memory
// tokens.
func awaitedSecretNames(cfg RenderConfig) map[string]bool {
	out := externalSecretNames(cfg)
	for _, n := range jobMintedSecretNames(cfg) {
		out[n] = true
	}
	for _, l := range cfg.MemoryToken.byComponent() {
		if l.linked != "" {
			out[l.linked] = true
		}
	}
	return out
}

// requiredSecretsPatch builds the strategic-merge patch that turns every
// `optional: true` reference to an awaited Secret (awaitedSecretNames) into a
// required one, in every workload of bundles. emitted is false when there is
// nothing to patch.
//
// The base marks these references optional because `oap install` applies the
// workloads first, mints the Secrets second, and then ROLLS any pod that
// started without them (rollOperatorIfPredatesMemoryTokens). A GitOps apply has
// no such second step. An optional reference lets the pod start before the
// bootstrap Job has minted the Secret, and a process that reads it once at
// startup never sees it afterward: the operator registers the three memory
// tokens once, from subPath mounts the kubelet never refreshes, so channelsd,
// authzd and webd would be refused and crash-loop until someone restarted the
// operator. Required, the kubelet itself holds the pod (ContainerCreating for a
// volume, CreateContainerConfigError for an env reference) until the Secret
// exists, on Flux's single-pass apply and under Argo CD's waves alike.
//
// Derived from the bundles rather than written out by hand, so an optional
// reference added to the base later is covered without touching this file, and
// `oap render --validate` fails on any that still slips through.
func requiredSecretsPatch(cfg RenderConfig, bundles ...[]byte) (patch []byte, emitted bool, err error) {
	awaited := awaitedSecretNames(cfg)
	var out [][]byte
	for _, bundle := range bundles {
		docs, err := manifests.Split(bundle)
		if err != nil {
			return nil, false, fmt.Errorf("parse bundle: %w", err)
		}
		for _, d := range docs {
			var refs []secretRef
			for _, ref := range podSecretRefs(d) {
				if ref.optional && awaited[ref.secret] {
					refs = append(refs, ref)
				}
			}
			if len(refs) == 0 {
				continue
			}
			doc, err := requiredSecretsPatchDoc(d, refs)
			if err != nil {
				return nil, false, err
			}
			out = append(out, doc)
		}
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return bytes.Join(out, []byte("---\n")), true, nil
}

// strategicMergeWorkloadKinds are the kinds whose pod template kustomize merges
// by container, env and volume name. Anything else would have its lists
// replaced wholesale by the patch.
var strategicMergeWorkloadKinds = map[string]bool{
	"apps/v1/Deployment":  true,
	"apps/v1/StatefulSet": true,
	"apps/v1/DaemonSet":   true,
	"batch/v1/Job":        true,
}

// requiredSecretsPatchDoc is the patch for one workload: each of refs restated
// by its merge keys (container + env name, or volume name) with only
// `optional: false` beneath, so everything else about the reference stays the
// base's. It refuses a reference it cannot address that way rather than leaving
// it optional.
func requiredSecretsPatchDoc(d *unstructured.Unstructured, refs []secretRef) ([]byte, error) {
	if !strategicMergeWorkloadKinds[d.GetAPIVersion()+"/"+d.GetKind()] {
		return nil, fmt.Errorf("%s %s references Secret %q optionally and render cannot patch a %s to require it", d.GetKind(), d.GetName(), refs[0].secret, d.GetKind())
	}
	required := map[string]any{"optional": false}
	podSpec := map[string]any{}
	// containerList → container name → its env patches, in first-seen order.
	containers := map[string][]map[string]any{}
	for _, ref := range refs {
		switch {
		case ref.volume != "":
			vols, _ := podSpec["volumes"].([]any)
			podSpec["volumes"] = append(vols, map[string]any{"name": ref.volume, "secret": required})
		case ref.env != "":
			env := map[string]any{"name": ref.env, "valueFrom": map[string]any{"secretKeyRef": required}}
			list := containers[ref.containerList]
			var c map[string]any
			for _, existing := range list {
				if existing["name"] == ref.container {
					c = existing
				}
			}
			if c == nil {
				c = map[string]any{"name": ref.container}
				containers[ref.containerList] = append(list, c)
			}
			envs, _ := c["env"].([]any)
			c["env"] = append(envs, env)
		default:
			return nil, fmt.Errorf("%s %s references Secret %q optionally through envFrom or a projected volume, which render cannot patch to required; reference it as a secret volume or an env secretKeyRef", d.GetKind(), d.GetName(), ref.secret)
		}
	}
	for list, cs := range containers {
		items := make([]any, 0, len(cs))
		for _, c := range cs {
			items = append(items, c)
		}
		podSpec[list] = items
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": d.GetAPIVersion(),
		"kind":       d.GetKind(),
		"metadata":   map[string]any{"name": d.GetName(), "namespace": d.GetNamespace()},
		"spec":       map[string]any{"template": map[string]any{"spec": podSpec}},
	})
}
