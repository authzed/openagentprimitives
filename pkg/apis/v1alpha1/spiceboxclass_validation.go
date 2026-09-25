package v1alpha1

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateEnvDefaults rejects env keys matching a reserved env-var name.
// Reserved names are the toolkits' sensitive env vars, whose values the
// operator controls end to end from the matching AgentIdentity credential; a
// class setting one in EnvDefaults would shadow that value before the runner
// attaches it. The caller computes `reserved` from the toolkit registry, so
// this stays a pure, registry-free check.
//
// Every offender is collected and sorted rather than returned on first hit:
// map order would otherwise vary the status-condition message per reconcile.
// The same applies to the other validators here.
func ValidateEnvDefaults(env map[string]string, reserved []string) error {
	// PATH and TMPDIR are composed by the pod builder from the class's
	// toolchains, so a class that also sets them is silently overridden.
	var reservedTC []string
	for k := range env {
		if _, bad := reservedToolchainEnvKeys[k]; bad {
			reservedTC = append(reservedTC, k)
		}
	}
	if len(reservedTC) > 0 {
		sort.Strings(reservedTC)
		return fmt.Errorf("envDefaults contains reserved key(s) computed from spec.toolchains: %v", reservedTC)
	}

	if len(reserved) == 0 || len(env) == 0 {
		return nil
	}
	reservedSet := make(map[string]struct{}, len(reserved))
	for _, name := range reserved {
		reservedSet[name] = struct{}{}
	}
	var bad []string
	for k := range env {
		if _, isReserved := reservedSet[k]; isReserved {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("envDefaults contains reserved key(s): %v (reserved env names: %v)",
		bad, reserved)
}

// ValidateScratchSizes checks the memory-backed /tmp and /work emptyDirs and
// the disk-backed /var/ap-cache emptyDir: each effective size must be positive.
//
// It deliberately does NOT require tmpSize + workSize to fit inside the memory
// limit. An emptyDir's sizeLimit is a CAP, not a reservation: tmpfs pages are
// charged against the container's memory limit only as they are written, so a
// 64Mi-memory class with the default 50Mi/100Mi caps is valid — the memory
// limit simply binds first. Many shipped classes are exactly that shape.
//
// That raising a cap buys nothing without raising memory too belongs in the
// field documentation, not in a validator that rejects working configurations.
func ValidateScratchSizes(r SpiceboxResources) error {
	var problems []string

	if tmp := r.EffectiveTmpSize(); tmp.Sign() <= 0 {
		problems = append(problems, fmt.Sprintf("resources.tmpSize %q must be greater than zero", tmp.String()))
	}
	if work := r.EffectiveWorkSize(); work.Sign() <= 0 {
		problems = append(problems, fmt.Sprintf("resources.workSize %q must be greater than zero", work.String()))
	}
	// A zero/negative sizeLimit on the disk-backed cache emptyDir means "no
	// limit" to the kubelet, which defeats the ephemeral-storage accounting the
	// pod builder relies on. Reject it like the memory-backed caps above.
	if cache := r.EffectiveCacheSize(); cache.Sign() <= 0 {
		problems = append(problems, fmt.Sprintf("resources.cacheSize %q must be greater than zero", cache.String()))
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("resources invalid: %s", strings.Join(problems, "; "))
}

// reservedVolumeNames are the pod-builder volume names a PrivateVolume
// must not collide with. Keep in sync with pkg/platform/podspec/builder.go — the
// apis package cannot import podspec (podspec imports apis).
var reservedVolumeNames = map[string]struct{}{
	"work":       {},
	"tmp":        {},
	"workspace":  {},
	"toolchains": {},
	"ap-cache":   {},
}

// reservedMountPaths are the pod-builder mount paths a PrivateVolume's
// MountPath must not collide with. Keep in sync with pkg/platform/podspec/builder.go.
var reservedMountPaths = map[string]struct{}{
	"/work":            {},
	"/tmp":             {},
	"/workspace":       {},
	ToolchainRootPath:  {},
	ToolchainCachePath: {},
}

// ValidatePrivateVolumes checks the PrivateVolumes slice for: empty
// name/mountPath, non-DNS-1123 names, non-absolute mount paths, names
// colliding with pod-builder-reserved volume names or with ConfigMap
// mount names from spec.mounts, mount paths colliding with reserved
// paths, and duplicate names or mount paths within the slice itself.
// Returns a single error listing every problem, or nil when clean.
func ValidatePrivateVolumes(pvs []PrivateVolume, mounts []SpiceboxMount) error {
	// ConfigMap mount names occupy the same pod-volume namespace as private
	// volumes; a private volume named after one of them collides at pod-apply.
	mountNames := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		mountNames[m.Name] = struct{}{}
	}

	var problems []string
	seenNames := map[string]int{}
	seenPaths := map[string]int{}
	for i, pv := range pvs {
		switch {
		case pv.Name == "":
			problems = append(problems, fmt.Sprintf("privateVolumes[%d].name is required", i))
		default:
			if errs := validation.IsDNS1123Label(pv.Name); len(errs) > 0 {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].name %q is not a DNS-1123 label: %s",
					i, pv.Name, strings.Join(errs, "; ")))
			}
			if _, reserved := reservedVolumeNames[pv.Name]; reserved {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].name %q collides with a pod-builder-reserved volume name",
					i, pv.Name))
			}
			if _, isMount := mountNames[pv.Name]; isMount {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].name %q collides with a ConfigMap mount name from spec.mounts",
					i, pv.Name))
			}
			if prev, dup := seenNames[pv.Name]; dup {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].name %q duplicates privateVolumes[%d].name",
					i, pv.Name, prev))
			} else {
				seenNames[pv.Name] = i
			}
		}

		switch {
		case pv.MountPath == "":
			problems = append(problems, fmt.Sprintf("privateVolumes[%d].mountPath is required", i))
		case !strings.HasPrefix(pv.MountPath, "/"):
			problems = append(problems, fmt.Sprintf(
				"privateVolumes[%d].mountPath %q must be absolute (start with %q)",
				i, pv.MountPath, "/"))
		default:
			if _, reserved := reservedMountPaths[pv.MountPath]; reserved {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].mountPath %q collides with a pod-builder-reserved mount path",
					i, pv.MountPath))
			}
			if prev, dup := seenPaths[pv.MountPath]; dup {
				problems = append(problems, fmt.Sprintf(
					"privateVolumes[%d].mountPath %q duplicates privateVolumes[%d].mountPath",
					i, pv.MountPath, prev))
			} else {
				seenPaths[pv.MountPath] = i
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("privateVolumes invalid: %s", strings.Join(problems, "; "))
}
