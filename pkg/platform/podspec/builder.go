// Package podspec builds a sandboxed corev1.Pod from a SpiceboxSession + resolved SpiceboxClass spec.
package podspec

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain"
	tcregistry "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/registry"
)

const (
	// ContainerName is the name of the sandbox container in every pod this
	// package builds. Exported because the pod sandbox kind binds its exec
	// transport to this container.
	ContainerName = "sandbox"
	workMountPath = "/work"
	tmpMountPath  = "/tmp"

	// SkillsMountPath is where staged skill bundles land in the sandbox: each
	// bundle unpacks to /skills/<LocalName>/ — the skill's own AgentSkill name,
	// which is what a disk-based consumer (Claude Code) looks for because it
	// must match the staged SKILL.md's frontmatter name (see
	// ResolvedSkillBundle.LocalName and BuildBundleSession in
	// pkg/controllers/agentsession/bundles.go). NOT <MountName>: that value is
	// only the Kubernetes-object-safe volume/ConfigMap name and is never the
	// sandbox-visible directory.
	//
	// Exported (rather than the package-private form the rest of this file's
	// constants use) so that internal/cmd/claudeshim/main.go — which needs the
	// same path to link staged skills into Claude Code's discovery path, and
	// today hand-duplicates the "/skills" literal under its own constant of
	// the same name — can import this one instead of re-typing the value.
	// Not wired up in this change: internal/cmd/claudeshim/ is out of scope
	// pending a separate architecture decision on the claudeshim/
	// toolchain-claude split.
	SkillsMountPath = "/skills"

	// toolchainsVolumeName is the shared emptyDir each toolchain's init
	// container copies its payload into, mounted read-only by the sandbox.
	toolchainsVolumeName = "toolchains"
	// toolchainInitMountPath is where that volume is mounted read-write on the
	// init containers. Deliberately NOT the final root path: a container that
	// mounts a volume over its own image's payload directory would shadow the
	// bytes it is trying to copy.
	toolchainInitMountPath = "/dst"
	// cacheVolumeName / its mount path give toolchains a writable, disk-backed
	// scratch area. /work and /tmp are memory-backed emptyDirs charged against
	// the pod memory limit, so GOCACHE and TMPDIR cannot live there.
	cacheVolumeName = "ap-cache"
	// basePATH is the debian default PATH of the sandbox base image. Container
	// env PATH overrides the image's ENV PATH, so the composed value must
	// re-include this or the sandbox loses /bin.
	basePATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// BuildClassSpec renders the class-derived PodSpec: everything that does not
// depend on a particular session. This is exactly the shape a pre-warmed
// sandbox has, and comparing a session's PodSpec against it is how the
// agent-sandbox backend decides whether that session may adopt one.
//
// toolchains is the already-RESOLVED mount list (see pkg/tools/toolchain/resolve),
// not class.Toolchains itself. Resolution is a pure function of the class, so a
// pool builder and a session reach this function with the same mounts for the
// same class and get byte-identical output — that equality IS the adoption
// test. Pass nil only for a class with no toolchains: passing nil for a class
// whose Toolchains is non-empty renders a spec missing the overlay, which no
// session that resolved those toolchains can ever compare equal to, so that
// pool entry becomes unadoptable.
func BuildClassSpec(class spiceboxv1alpha1.SpiceboxClassSpec, toolchains []spiceboxv1alpha1.ToolchainMount) (corev1.PodSpec, error) {
	if class.Image == "" {
		return corev1.PodSpec{}, fmt.Errorf("class.image is required")
	}

	spec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: ptr.To(false),
		EnableServiceLinks:           ptr.To(false),
		HostNetwork:                  false,
		HostPID:                      false,
		HostIPC:                      false,
		ShareProcessNamespace:        ptr.To(true),
		SecurityContext:              HardenedPodSecurityContext(),
		Containers: []corev1.Container{{
			Name:  ContainerName,
			Image: class.Image,
			// Nothing in this container forks children, so it needs no init to
			// reap zombies and any image with `sleep` on PATH will do. Adding a
			// tool path that forks means adding one.
			Command:         []string{"sleep", "infinity"},
			SecurityContext: HardenedContainerSecurityContext(),
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              class.Resources.CPU,
					corev1.ResourceMemory:           class.Resources.Memory,
					corev1.ResourceEphemeralStorage: class.Resources.EphemeralStorage,
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "work", MountPath: workMountPath},
				{Name: "tmp", MountPath: tmpMountPath},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"/bin/true"}},
				},
				PeriodSeconds: 5,
			},
		}},
		Volumes: []corev1.Volume{
			{
				Name: "work",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						// Memory-backed on purpose: pod-local scratch that never
						// reaches the node's disk. Its pages are charged against the
						// container's memory limit, which is why the class validates
						// tmpSize + workSize < memory — otherwise a generous value
						// buys an OOM kill, not a bigger scratch area.
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: ptr.To(class.Resources.EffectiveWorkSize()),
					},
				},
			},
			{
				Name: "tmp",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: ptr.To(class.Resources.EffectiveTmpSize()),
					},
				},
			},
		},
	}

	if class.RuntimeClassName != nil && *class.RuntimeClassName != "" {
		spec.RuntimeClassName = ptr.To(*class.RuntimeClassName)
	}

	if err := applyMounts(&spec, class.Mounts, class.Image); err != nil {
		return corev1.PodSpec{}, err
	}

	// Class-level env defaults. Sorted-by-key application keeps the
	// generated PodSpec deterministic across reconciles (controller-runtime
	// cache equality skips a redundant Update).
	keys := make([]string, 0, len(class.EnvDefaults))
	for k := range class.EnvDefaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.Containers[0].Env = append(spec.Containers[0].Env, corev1.EnvVar{
			Name:  k,
			Value: class.EnvDefaults[k],
		})
	}

	// Class-level private volumes: per-pod emptyDir mounts NOT shared
	// across bundles. Phase C uses these to keep a git bundle's .git
	// directory off the shared workspace so credentials in .git/config
	// never reach a co-located claude bundle.
	for _, pv := range class.PrivateVolumes {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: pv.Name,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		spec.Containers[0].VolumeMounts = append(
			spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: pv.Name, MountPath: pv.MountPath},
		)
	}

	// Toolchains are class-derived: the SET comes from class.Toolchains and the
	// resolution is a pure function of it. A session merely FREEZES that
	// resolution at bind time. Applying it here is what lets a warm pool carry
	// the overlay, so the per-toolchain init containers run once at pool-fill
	// instead of on every cold start.
	if len(toolchains) > 0 {
		if err := applyToolchainsToSpec(&spec, class, toolchains); err != nil {
			return corev1.PodSpec{}, err
		}
	}

	return spec, nil
}

// Build returns a Pod that implements the sandbox for the given Session.
// Returns an error if the class spec is missing fields the Pod requires.
func Build(s *spiceboxv1alpha1.SpiceboxSession, class spiceboxv1alpha1.SpiceboxClassSpec) (*corev1.Pod, error) {
	// The toolchain overlay was frozen onto status at first bind by the
	// SpiceboxSession reconciler, so the pod builder reads no CRs and a catalog
	// edit cannot mutate a running pod. Resolution itself is a pure function of
	// the class, which is exactly why it is applied inside BuildClassSpec: a
	// pre-warmed sandbox built from the same (class, mounts) pair is byte-
	// identical to what a session would render.
	spec, err := BuildClassSpec(class, s.Status.ResolvedToolchains)
	if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodNameFor(s),
			Namespace: s.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/session": s.Name,
				"agentprimitives.authzed.com/class":   s.Spec.Class,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:               "SpiceboxSession",
				Name:               s.Name,
				UID:                s.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spec,
	}

	// Shared workspace PVC: mounted at /workspace in addition to the pod-local
	// /work emptyDir. Present only when the AgentSession controller stamped a
	// shared-mode WorkspaceConfig with a claim name.
	if s.Spec.Workspace.Mode == spiceboxv1alpha1.WorkspaceShared && s.Spec.Workspace.SharedClaimName != "" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: s.Spec.Workspace.SharedClaimName,
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "workspace", MountPath: workspaceMountPath})

		// Run tool execs from the shared workspace so CWD-rooted tools
		// (Claude Code, bare `git`) operate on the cloned repo directly
		// instead of the pod-local /work emptyDir.
		pod.Spec.Containers[0].WorkingDir = workspaceMountPath
	}

	// Session-derived mounts: a second provenance layered on top of the
	// class-authored ones BuildClassSpec already applied above via its own
	// applyMounts(class.Mounts, ...) call. Re-including class.Mounts here would
	// feed applyMounts a second, overlapping copy of them — duplicate Volumes
	// and VolumeMounts (an invalid PodSpec) — so only session-provenance mounts
	// are passed. applyMounts is safe to call twice on one spec (see its own
	// doc comment): the second call reuses the shared unpack volume/init
	// container by name instead of re-adding one, regardless of which call's
	// mounts are tarGz.
	//
	// Mounts is preferred; SkillBundles (DEPRECATED — see
	// SpiceboxSessionSpec.SkillBundles) is converted into the same SpiceboxMount
	// shape and consulted only when Mounts is empty, so a session created before
	// Mounts existed and re-hydrated afterwards still builds a pod at all.
	//
	// The fallback does NOT render the same address as the native path: below,
	// MountPath is built from b.MountName (the content-hashed slug), while
	// BuildBundleSession's native conversion (pkg/controllers/agentsession/
	// bundles.go) builds it from sb.LocalName (the frontmatter-matching name a
	// disk-based consumer like Claude Code actually looks for). A rehydrated
	// legacy session therefore mounts its skill at the PRE-fix address, not the
	// one a fresh session gets — that is deliberate, not a bug to converge: the
	// stored SkillBundleMount has no LocalName field to convert from (it
	// predates that field), so there is nothing to build the native address
	// from here.
	//
	// It self-heals on the next AgentSession reconcile instead of needing a
	// fix here: resolveAndStageSkillBundles' already-staged short-circuit
	// (pkg/controllers/agentsession/skills.go) requires prev.LocalName ==
	// sk.Name to carry a status entry forward untouched. A legacy
	// ResolvedSkillBundle predates the LocalName field and unmarshals it as
	// "", which never equals a real AgentSkill.Name, so the check misses,
	// the skill is restaged, and the resulting AgentSession writes a fresh
	// spec.mounts (populated) onto the SpiceboxSession — after which this
	// SkillBundles branch is never reached again for that skill. Do not
	// "simplify" that comparison to drop LocalName: it is the only thing that
	// forces the one-time restage this upgrade path depends on.
	sessionMounts := s.Spec.Mounts
	if len(sessionMounts) == 0 {
		for _, b := range s.Spec.SkillBundles {
			// No Digest: the legacy SkillBundleMount shape never carried one, so
			// this content extracts unverified. Log the downgrade — this is the
			// one case in this builder where a tarGz mount is deliberately staged
			// without integrity verification, and it should be visible to
			// whoever is diagnosing why a session's skill mount isn't
			// digest-checked.
			log.Log.Info("podspec.Build: staging legacy SkillBundles mount without a content digest (pre-Mounts session, unverified on extract)",
				"session", s.Namespace+"/"+s.Name, "mount", b.MountName)
			sessionMounts = append(sessionMounts, spiceboxv1alpha1.SpiceboxMount{
				Name:      b.MountName,
				MountPath: SkillsMountPath + "/" + b.MountName,
				Format:    spiceboxv1alpha1.MountFormatTarGz,
				Source: spiceboxv1alpha1.MountSource{
					ConfigMapRef: &corev1.LocalObjectReference{Name: b.ConfigMapName},
				},
			})
		}
	}
	if len(sessionMounts) > 0 {
		if err := applyMounts(&pod.Spec, sessionMounts, class.Image); err != nil {
			return nil, err
		}
	}

	return pod, nil
}

// applyToolchainsToSpec composes N read-only toolchain overlays plus one
// writable disk-backed cache into the sandbox container.
//
// Layout: each toolchain's own image runs as an init container that copies its
// payload into the shared `toolchains` emptyDir; the sandbox mounts that volume
// read-only at v1alpha1.ToolchainRootPath. Because every payload is built at
// /opt/ap-toolchains/<name> and lands at /opt/ap-toolchains/<name>, no path
// inside a binary ever needs rewriting.
//
// mounts is sorted by name first: the PodSpec must be a pure function of its
// inputs or every reconcile churns field ownership under server-side apply.
func applyToolchainsToSpec(spec *corev1.PodSpec, class spiceboxv1alpha1.SpiceboxClassSpec, mounts []spiceboxv1alpha1.ToolchainMount) error {
	sorted := make([]spiceboxv1alpha1.ToolchainMount, len(mounts))
	copy(sorted, mounts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var payloadBytes int64
	for _, m := range sorted {
		payloadBytes += m.SizeBytes
	}

	// overlayBytes is what the toolchains emptyDir is ALLOWED to reach (payload
	// plus 10% slack for filesystem overhead). The ephemeral-storage limit below
	// must be raised by this same number, not by payloadBytes: a SizeLimit the
	// container limit does not cover lets the volume grow legally right up to an
	// eviction.
	overlayBytes := payloadBytes + payloadBytes/10

	// cacheBytes is the disk-backed /var/ap-cache SizeLimit: the class's
	// CacheSize, or the 2Gi default. It is applied to the emptyDir here AND
	// added to the container's ephemeral-storage limit below — the two must use
	// the same value or the volume can fill legally and still trip the limit.
	cacheBytes := class.Resources.EffectiveCacheSize()

	spec.Volumes = append(spec.Volumes,
		corev1.Volume{
			Name: toolchainsVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr.To(*resource.NewQuantity(overlayBytes, resource.BinarySI)),
				},
			},
		},
		corev1.Volume{
			Name: cacheVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr.To(cacheBytes),
				},
			},
		},
	)

	// One init container per toolchain, contributed by its delivery Kind. The
	// registry Kind interface is written against *corev1.Pod (it needs no more
	// than InitContainers, which live on the Spec), so wrap spec in a throwaway
	// Pod for the call and write the mutated Spec back — cheaper than changing
	// the Kind contract for a field it never uses outside Spec.
	tmpPod := &corev1.Pod{Spec: *spec}
	for _, m := range sorted {
		k, ok := tcregistry.ByKind(m.SourceKind)
		if !ok {
			return fmt.Errorf("toolchain %q: unknown source kind %q (known: %v)",
				m.Name, m.SourceKind, tcregistry.Names())
		}
		if err := k.Apply(toolchain.ApplyParams{
			Pod:             tmpPod,
			Mount:           m,
			VolumeName:      toolchainsVolumeName,
			InitMountPath:   toolchainInitMountPath,
			SecurityContext: HardenedContainerSecurityContext(),
		}); err != nil {
			return fmt.Errorf("toolchain %q: apply %s: %w", m.Name, m.SourceKind, err)
		}
	}
	*spec = tmpPod.Spec

	c := &spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: toolchainsVolumeName, MountPath: spiceboxv1alpha1.ToolchainRootPath, ReadOnly: true},
		corev1.VolumeMount{Name: cacheVolumeName, MountPath: spiceboxv1alpha1.ToolchainCachePath},
	)

	// PATH must be a fully-resolved literal. pkg/tools/exec/remote wraps every tool
	// exec as `env KEY=val ... CMD`, which REPLACES the variable rather than
	// expanding it, so a "$PATH:..." value would reach the tool verbatim.
	paths := make([]string, 0, len(sorted)+1)
	for _, m := range sorted {
		root := spiceboxv1alpha1.ToolchainRootFor(m.Name)
		for _, b := range m.Bin {
			paths = append(paths, root+"/"+b)
		}
	}
	paths = append(paths, basePATH)

	// Appended AFTER class.EnvDefaults so a toolchain's own GOROOT/GOCACHE wins
	// over a stale class default; PATH and TMPDIR are rejected in EnvDefaults at
	// class validation, so they cannot be shadowed at all.
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "PATH", Value: strings.Join(paths, ":")},
		corev1.EnvVar{Name: "TMPDIR", Value: spiceboxv1alpha1.ToolchainCachePath + "/tmp"},
	)
	for _, m := range sorted {
		keys := make([]string, 0, len(m.Env))
		for k := range m.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic PodSpec
		for _, k := range keys {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: m.Env[k]})
		}
	}

	// The overlay + cache emptyDirs are charged against the container's
	// ephemeral-storage limit. The limit must be raised by exactly the two
	// SizeLimits granted above (overlay + cacheBytes) — anything less lets a
	// volume fill legally and still trip the container limit, evicting the pod
	// partway through the copy.
	limit := class.Resources.EphemeralStorage.DeepCopy()
	limit.Add(*resource.NewQuantity(overlayBytes, resource.BinarySI))
	limit.Add(cacheBytes)
	c.Resources.Limits[corev1.ResourceEphemeralStorage] = limit

	return nil
}

// PodNameFor returns the deterministic Pod name for a given SpiceboxSession.
func PodNameFor(s *spiceboxv1alpha1.SpiceboxSession) string {
	return s.Name + "-pod"
}
