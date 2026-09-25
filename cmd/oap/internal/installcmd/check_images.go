package installcmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// parsePinnedRef splits a container image reference of the form
// "<reg>/<name>:<tag>@sha256:<digest>" into its tag reference (everything before
// the "@"), the digest ("sha256:…"), and the registry root (everything before the
// last "/" of the tag reference, i.e. "<reg>" without the image name/tag).
//
// ok is false for any ref that is not a digest-pinned *registry* ref: a bare local
// tag ("spicebox-operator:dev"), a public dependency tag ("authzed/spicedb:latest"),
// or a registry ref without an "@digest". Those are the images the drift check has
// nothing to compare against, so callers skip them.
func parsePinnedRef(ref string) (tagRef, digest, reg string, ok bool) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return "", "", "", false
	}
	tagRef = ref[:at]
	digest = ref[at+1:]
	if !strings.HasPrefix(digest, "sha256:") {
		return "", "", "", false
	}
	// A registry ref names the registry as a path segment before the image name,
	// so the tag reference must contain a "/". A bare "name:dev@sha256:…" (no
	// registry) has nothing to resolve against.
	slash := strings.LastIndex(tagRef, "/")
	if slash < 0 {
		return "", "", "", false
	}
	reg = tagRef[:slash]
	return tagRef, digest, reg, true
}

// shortDigest trims the "sha256:" prefix and keeps the first 12 hex chars, for
// human-readable log lines (full digests are 64 chars).
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// checkImageDrift reports, for each first-party Deployment in operatorNS whose
// container image is a digest-pinned registry ref, whether the deployed digest
// matches the registry's *current* digest for that same tag. A mismatch means the
// running pods are behind what the registry now serves (a build that wasn't
// followed by an applying install, an out-of-band push, an interrupted run).
//
// Drift is a WARNING, never a hard failure: it does not return an error for a
// mismatch, so `oap check`'s exit code is unchanged. A registry resolve failure
// (docker/buildx missing, unauthenticated, network) is likewise a soft warning —
// the check surfaces it and moves on rather than breaking an otherwise-healthy
// `oap check`. The only returned error is a Deployment *list* failure, which means
// the check could not run at all.
//
// When repair is true, a drifted Deployment's container image is re-pinned to the
// current registry digest, which rolls the pod onto the newer image.
//
// It is invoked ONLY from `oap check`'s command (once per invocation), never from
// the shared runCheck used by `oap init`'s 2s poll loop — resolving digests against
// the registry per image is too slow to run in that hot loop.
func checkImageDrift(ctx context.Context, out io.Writer, b *kube.Bundle, repair bool) error {
	deps, err := b.Typed.AppsV1().Deployments(operatorNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list deployments in %s: %w", operatorNS, err)
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		for _, c := range d.Spec.Template.Spec.Containers {
			tagRef, deployed, reg, ok := parsePinnedRef(c.Image)
			if !ok {
				continue
			}
			current, rerr := imagetoolsDigest(ctx, tagRef)
			if rerr != nil {
				fmt.Fprintf(out, "⚠ %s: could not resolve registry digest for %s: %v (skipped)\n", d.Name, tagRef, rerr)
				continue
			}
			// A resolver that returns no error can still return a non-digest:
			// parseImagetoolsManifestDigest yields ("", nil) for well-formed JSON
			// with no top-level "digest" field, which is what a buildx
			// output-shape change looks like. Comparing that against the deployed
			// digest reports every image as drifted, and --repair then patches the
			// container to "<tagRef>@" — a reference the apiserver accepts and the
			// kubelet cannot pull. Treat it as unresolvable, not as drift.
			if !strings.HasPrefix(current, "sha256:") {
				fmt.Fprintf(out, "⚠ %s: registry returned an unexpected digest %q for %s (skipped)\n", d.Name, current, tagRef)
				continue
			}
			if current == deployed {
				fmt.Fprintf(out, "✓ %s: image up to date (%s)\n", d.Name, shortDigest(deployed))
				continue
			}
			fmt.Fprintf(out, "⚠ %s: running %s but %s resolves to %s — run `oap install --image-registry %s` or `oap check --repair` to adopt it\n",
				d.Name, shortDigest(deployed), tagRef, shortDigest(current), reg)
			if !repair {
				continue
			}
			if perr := repinDeployment(ctx, b, d.Name, c.Name, tagRef+"@"+current); perr != nil {
				fmt.Fprintf(out, "⚠ %s: re-pin failed: %v\n", d.Name, perr)
				continue
			}
			fmt.Fprintf(out, "⚠ %s: re-pinned to %s — re-run oap check to verify\n", d.Name, shortDigest(current))
		}
	}
	return nil
}

// repinDeployment sets a single container's image via a strategic-merge patch
// (containers are merged by "name"), which changes the pod template and rolls the
// Deployment onto newRef.
func repinDeployment(ctx context.Context, b *kube.Bundle, deploy, container, newRef string) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%q,"image":%q}]}}}}`,
		container, newRef))
	if _, err := b.Typed.AppsV1().Deployments(operatorNS).Patch(ctx, deploy, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("re-pin %s: %w", deploy, err)
	}
	return nil
}
