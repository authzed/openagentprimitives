package podspec

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	// unpackVolumeName is the single shared emptyDir every tarGz mount expands
	// into. One volume regardless of mount count: subPath keeps each mount's
	// content isolated inside it, so N archives cost one initContainer and one
	// emptyDir, not N.
	unpackVolumeName = "unpacked"
	// unpackMountPath is where the shared unpack volume is mounted, read-write,
	// on the init container. The sandbox container never mounts this path
	// directly — it only sees the per-mount subPath views applied below.
	unpackMountPath = "/unpacked"
	// mountStagingPath is where each tarGz mount's source ConfigMap is mounted,
	// read-only, on the init container: <mountStagingPath>/<mount.Name>/bundle.tar.gz.
	mountStagingPath = "/staging"
	// mountArchiveKey is the ConfigMap binaryData key an archive mount's
	// content is staged under (mirrors skillBundleTarKey).
	mountArchiveKey = "bundle.tar.gz"
	// mountUnpackContainerName is the single init container that verifies (when
	// a Digest is set) and expands every tarGz mount into the shared unpack
	// volume.
	mountUnpackContainerName = "mount-unpack"
)

// applyMounts materializes every mount. raw mounts (Format == "" or
// MountFormatRaw — the CRD default is applied by the API server, so an
// in-memory SpiceboxMount reads "" not "raw") land as direct read-only
// volumes, byte-identical to the pre-Format shape. tarGz mounts are expanded
// by ONE initContainer into a single shared emptyDir, then surfaced per-mount
// via subPath, so three archives do not mean three initContainers or three
// emptyDirs.
//
// applyMounts is safe to call more than once against the same *corev1.PodSpec
// — e.g. once for a SpiceboxClass's authored mounts, once for a
// SpiceboxSession's derived ones, which is exactly what podspec.Build does.
// The shared unpack volume (unpackVolumeName) and init container
// (mountUnpackContainerName) are looked up by name on entry and REUSED when
// already present, rather than appended unconditionally: a second call whose
// mounts include a tarGz entry merges its archive into the existing init
// container's script and VolumeMounts instead of adding a second
// identically-named Volume and InitContainer, which the API server would
// reject. Per-archive source-volume names stay globally unique across calls
// by continuing the index from how many are already mounted on the init
// container, not restarting at 0 each call.
//
// initImage is the sandbox class image: it carries tar + sha256sum + mkdir on
// PATH, so reusing it avoids pulling a second image into the pod.
func applyMounts(spec *corev1.PodSpec, mounts []spiceboxv1alpha1.SpiceboxMount, initImage string) error {
	var archives []spiceboxv1alpha1.SpiceboxMount

	for _, m := range mounts {
		if m.Source.ConfigMapRef == nil {
			// Fail closed. A future MountSource member that this builder does
			// not yet handle must not appear to work and mount nothing.
			return fmt.Errorf("mount %q: no recognised source (set source.configMapRef)", m.Name)
		}

		if m.Format != "" && m.Format != spiceboxv1alpha1.MountFormatRaw {
			// Deferred: tarGz mounts are staged together, once, below.
			archives = append(archives, m)
			continue
		}

		// raw: a direct read-only ConfigMap volume. Unchanged from the shape
		// every mount had before Format existed.
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: *m.Source.ConfigMapRef,
					DefaultMode:          ptr.To(int32(0o444)),
				},
			},
		})
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath, ReadOnly: true})
	}

	if len(archives) == 0 {
		// No tarGz mounts: no shared volume, no init container. A raw-only (or
		// mount-less) pod must be byte-identical to its pre-Format shape.
		return nil
	}

	// Reuse the shared unpack volume and init container if a PRIOR applyMounts
	// call on this same spec already created them (e.g. BuildClassSpec's call
	// for class.Mounts, ahead of podspec.Build's call for session-provenance
	// mounts). Appending a second Volume/InitContainer under the same fixed
	// name would be an invalid PodSpec — Kubernetes rejects duplicate names —
	// so this call folds its archives into what is already there instead.
	if !hasVolumeNamed(spec.Volumes, unpackVolumeName) {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         unpackVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	ic := findContainerNamed(spec.InitContainers, mountUnpackContainerName)
	// nextSrcIndex continues the per-archive source-volume numbering from
	// where a prior call left off, so "mount-src-N" stays globally unique
	// across calls rather than colliding by both starting at 0. The init
	// container's VolumeMounts are exactly [unpackVolumeName] + one entry per
	// archive already merged into it, so len-1 is that count.
	nextSrcIndex := 0
	var existingScript string
	newMounts := []corev1.VolumeMount(nil)
	if ic == nil {
		newMounts = append(newMounts, corev1.VolumeMount{Name: unpackVolumeName, MountPath: unpackMountPath})
	} else {
		nextSrcIndex = len(ic.VolumeMounts) - 1
		if len(ic.Command) == 3 {
			existingScript = ic.Command[2]
		}
	}

	// steps is one shell statement per archive (an optional digest-verify guard
	// plus the extract); joined with && so any failure — a digest mismatch or a
	// bad archive — aborts the whole init container rather than partially
	// staging content. Volume names are derived by index to stay within the
	// DNS-1123-label limit (MountName can be up to 253 chars and contain dots,
	// which are invalid in a k8s volume name).
	steps := make([]string, 0, len(archives))
	for i, m := range archives {
		srcVolName := fmt.Sprintf("mount-src-%d", nextSrcIndex+i)
		srcDir := fmt.Sprintf("%s/%s", mountStagingPath, m.Name)
		srcFile := srcDir + "/" + mountArchiveKey
		dstDir := fmt.Sprintf("%s/%s", unpackMountPath, m.Name)

		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: srcVolName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: *m.Source.ConfigMapRef,
					Items: []corev1.KeyToPath{{
						Key:  mountArchiveKey,
						Path: mountArchiveKey,
					}},
					DefaultMode: ptr.To(int32(0o444)),
				},
			},
		})
		newMounts = append(newMounts, corev1.VolumeMount{
			Name: srcVolName, MountPath: srcDir, ReadOnly: true,
		})

		// Verification happens in the container that extracts, before it
		// extracts: an empty Digest skips this entirely, so raw and
		// un-digested tarGz mounts are unaffected. Content staged from a
		// ConfigMap can change between staging and use (spec §3.4); the mount
		// carries its expected identity so a mismatch fails the pod rather
		// than silently extracting drifted content.
		if m.Digest != "" {
			hexDigest := strings.TrimPrefix(m.Digest, "sha256:")
			steps = append(steps, fmt.Sprintf(
				`got=$(sha256sum %s | cut -d" " -f1) && [ "$got" = %s ] || { echo %s >&2; exit 1; }`,
				shellQuote(srcFile), shellQuote(hexDigest),
				shellQuote(fmt.Sprintf("mount %s: content digest mismatch (want %s, got $got)", m.Name, hexDigest)),
			))
		}

		steps = append(steps, fmt.Sprintf("mkdir -p %s && tar -xzf %s -C %s",
			shellQuote(dstDir), shellQuote(srcFile), shellQuote(dstDir)))
	}

	script := strings.Join(steps, " && ")
	if existingScript != "" {
		// A prior call already staged its own archives; append rather than
		// replace, so both sets of extract (and digest-check) steps run in the
		// one init container.
		script = existingScript + " && " + script
	}

	if ic == nil {
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:            mountUnpackContainerName,
			Image:           initImage,
			Command:         []string{"/bin/sh", "-c", script},
			SecurityContext: HardenedContainerSecurityContext(),
			VolumeMounts:    newMounts,
		})
	} else {
		ic.Command = []string{"/bin/sh", "-c", script}
		ic.VolumeMounts = append(ic.VolumeMounts, newMounts...)
	}

	// The sandbox container mounts the shared unpack volume once per archive,
	// each pinned to its own subPath so N archives never collide inside the
	// single emptyDir.
	for _, m := range archives {
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      unpackVolumeName,
				MountPath: m.MountPath,
				SubPath:   m.Name,
				ReadOnly:  true,
			})
	}

	return nil
}

// hasVolumeNamed reports whether vols already contains a Volume with the
// given name.
func hasVolumeNamed(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

// findContainerNamed returns a pointer into containers at the entry named
// name, or nil if absent. A pointer into the slice (not a copy) so the caller
// can mutate the container in place.
func findContainerNamed(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}
