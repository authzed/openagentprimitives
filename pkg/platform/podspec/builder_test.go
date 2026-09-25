package podspec_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests/crdschematest"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/image"
)

func baseClass() spiceboxv1alpha1.SpiceboxClassSpec {
	return spiceboxv1alpha1.SpiceboxClassSpec{
		Image: "registry.example.com/toolbelt:1.0.0",
		Resources: spiceboxv1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("500m"),
			Memory:           resource.MustParse("256Mi"),
			EphemeralStorage: resource.MustParse("100Mi"),
			PidsLimit:        64,
		},
		Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
	}
}

func baseSession() *spiceboxv1alpha1.SpiceboxSession {
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-1", Namespace: "ns-a", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "python-toolbelt"},
	}
}

func TestBuild_BasicShape(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")
	assert.Equal(t, "ns-a", pod.Namespace, "Namespace")
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy, "RestartPolicy")
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken, "AutomountServiceAccountToken set")
	assert.False(t, *pod.Spec.AutomountServiceAccountToken, "AutomountServiceAccountToken=false")
	require.Len(t, pod.OwnerReferences, 1, "exactly one OwnerReference")
	assert.Equal(t, types.UID("uid-1"), pod.OwnerReferences[0].UID, "OwnerReference UID")
	assert.Nil(t, pod.Spec.RuntimeClassName, "RuntimeClassName nil when class doesn't specify one")
}

func TestBuild_SecurityContext(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")
	c := pod.Spec.Containers[0]
	require.NotNil(t, c.SecurityContext, "SecurityContext set")
	require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation, "AllowPrivilegeEscalation set")
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation, "AllowPrivilegeEscalation=false")
	require.NotNil(t, c.SecurityContext.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem set")
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem=true")
	require.NotNil(t, c.SecurityContext.Capabilities, "Capabilities set")
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop, "Drop ALL")
}

// TestBuild_MountUnpackInitContainerSecurityContext pins that the
// "mount-unpack" init container gets the SAME hardened SecurityContext as the
// sandbox container (mounts.go:187's HardenedContainerSecurityContext()
// call). TestBuild_SecurityContext only ever looks at
// pod.Spec.Containers[0], so a regression that left the init container's
// SecurityContext nil or under-hardened would pass every existing test —
// on the one container in this pod that untars attacker-influenceable bytes
// from a ConfigMap before the sandbox container ever starts.
func TestBuild_MountUnpackInitContainerSecurityContext(t *testing.T) {
	sess := baseSession()
	sess.Spec.Mounts = []spiceboxv1alpha1.SpiceboxMount{
		{Name: "demo-skill", MountPath: "/skills/demo-skill",
			Format: spiceboxv1alpha1.MountFormatTarGz, Source: cmSource("demo-sess-cm")},
	}
	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")

	require.Len(t, pod.Spec.InitContainers, 1, "the tarGz mount must produce the mount-unpack init container")
	ic := pod.Spec.InitContainers[0]
	require.Equal(t, "mount-unpack", ic.Name, "sanity: this is the init container under test")

	require.NotNil(t, ic.SecurityContext, "mount-unpack init container must have a SecurityContext set")
	require.NotNil(t, ic.SecurityContext.AllowPrivilegeEscalation, "AllowPrivilegeEscalation set")
	assert.False(t, *ic.SecurityContext.AllowPrivilegeEscalation, "AllowPrivilegeEscalation=false")
	require.NotNil(t, ic.SecurityContext.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem set")
	assert.True(t, *ic.SecurityContext.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem=true")
	require.NotNil(t, ic.SecurityContext.Capabilities, "Capabilities set")
	assert.Equal(t, []corev1.Capability{"ALL"}, ic.SecurityContext.Capabilities.Drop, "Drop ALL")
}

func TestBuild_ResourcesApplied(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")
	c := pod.Spec.Containers[0]
	assert.Equal(t, "500m", c.Resources.Limits.Cpu().String(), "cpu limit")
	assert.Equal(t, "256Mi", c.Resources.Limits.Memory().String(), "memory limit")
}

func TestBuild_WorkTmpVolumes(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")
	names := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		names[v.Name] = true
	}
	for _, want := range []string{"work", "tmp"} {
		assert.Truef(t, names[want], "missing volume %q (have %v)", want, names)
	}
}

func TestBuild_RespectsExplicitRuntimeClass(t *testing.T) {
	class := baseClass()
	rc := "kata-fc"
	class.RuntimeClassName = &rc
	pod, err := podspec.Build(baseSession(), class)
	require.NoError(t, err, "Build")
	require.NotNil(t, pod.Spec.RuntimeClassName, "RuntimeClassName set")
	assert.Equal(t, "kata-fc", *pod.Spec.RuntimeClassName, "RuntimeClassName value")
}

func TestBuild_SharedWorkspaceVolume(t *testing.T) {
	sess := baseSession()
	sess.Spec.Workspace = spiceboxv1alpha1.WorkspaceConfig{
		Mode:            spiceboxv1alpha1.WorkspaceShared,
		SharedClaimName: "as-1-workspace",
	}
	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")

	var ws *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			ws = &pod.Spec.Volumes[i]
		}
	}
	require.NotNil(t, ws, "expected a 'workspace' volume")
	require.NotNil(t, ws.PersistentVolumeClaim, "workspace volume must be a PVC source")
	assert.Equal(t, "as-1-workspace", ws.PersistentVolumeClaim.ClaimName)

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == "workspace" {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, mount, "expected a 'workspace' volumeMount")
	assert.Equal(t, "/workspace", mount.MountPath)
}

func TestBuild_IsolatedMode_NoWorkspaceVolume(t *testing.T) {
	sess := baseSession() // default Workspace is zero-value → not shared
	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, "workspace", v.Name, "isolated mode must not add a workspace volume")
	}
}

func TestBuild_WorkingDir(t *testing.T) {
	cases := []struct {
		name        string
		workspace   spiceboxv1alpha1.WorkspaceConfig
		wantWorkDir string
	}{
		{
			// Shared workspace: CWD-rooted tools (Claude Code, bare git)
			// must exec from the workspace mount where the repo is cloned.
			name: "shared workspace: WorkingDir is the workspace mount",
			workspace: spiceboxv1alpha1.WorkspaceConfig{
				Mode:            spiceboxv1alpha1.WorkspaceShared,
				SharedClaimName: "as-1-workspace",
			},
			wantWorkDir: "/workspace",
		},
		{
			name:        "isolated mode: WorkingDir unset, inherits image WORKDIR",
			workspace:   spiceboxv1alpha1.WorkspaceConfig{},
			wantWorkDir: "",
		},
		{
			name:        "shared mode without claim name: WorkingDir unset",
			workspace:   spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceShared},
			wantWorkDir: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := baseSession()
			sess.Spec.Workspace = tc.workspace
			pod, err := podspec.Build(sess, baseClass())
			require.NoError(t, err, "Build")
			assert.Equal(t, tc.wantWorkDir, pod.Spec.Containers[0].WorkingDir)
		})
	}
}

func TestBuild_SharedModeButNoClaimName_NoWorkspaceVolume(t *testing.T) {
	sess := baseSession()
	sess.Spec.Workspace = spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceShared}
	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, "workspace", v.Name, "shared mode with empty SharedClaimName must not add a volume")
	}
}

func TestBuild_AppliesPrivateVolumes(t *testing.T) {
	cases := []struct {
		name           string
		privateVolumes []spiceboxv1alpha1.PrivateVolume
		wantMounts     []string // expected MountPaths on the container
		wantVolumes    []string // expected Pod.Spec.Volumes Names (must be present)
	}{
		{
			name: "nil PrivateVolumes: no extra volumes mounted",
		},
		{
			name: "single PrivateVolume: emptyDir at /var/ap-git",
			privateVolumes: []spiceboxv1alpha1.PrivateVolume{
				{Name: "ap-git", MountPath: "/var/ap-git"},
			},
			wantMounts:  []string{"/var/ap-git"},
			wantVolumes: []string{"ap-git"},
		},
		{
			name: "two PrivateVolumes: both mounted as emptyDir",
			privateVolumes: []spiceboxv1alpha1.PrivateVolume{
				{Name: "ap-git", MountPath: "/var/ap-git"},
				{Name: "ap-cache", MountPath: "/var/cache/ap"},
			},
			wantMounts:  []string{"/var/ap-git", "/var/cache/ap"},
			wantVolumes: []string{"ap-git", "ap-cache"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := baseClass()
			class.PrivateVolumes = tc.privateVolumes
			sess := baseSession()

			pod, err := podspec.Build(sess, class)
			require.NoError(t, err)

			// All declared mount paths must appear on the container.
			mountPaths := map[string]bool{}
			for _, m := range pod.Spec.Containers[0].VolumeMounts {
				mountPaths[m.MountPath] = true
			}
			for _, want := range tc.wantMounts {
				assert.True(t, mountPaths[want], "expected mount %q", want)
			}

			// For each declared private volume, confirm Pod.Spec.Volumes
			// has an emptyDir entry with the same Name.
			volByName := map[string]corev1.Volume{}
			for _, v := range pod.Spec.Volumes {
				volByName[v.Name] = v
			}
			for _, pv := range tc.privateVolumes {
				v, ok := volByName[pv.Name]
				require.True(t, ok, "expected Pod.Spec.Volumes to contain %q", pv.Name)
				require.NotNil(t, v.EmptyDir, "%q must be emptyDir", pv.Name)
			}

			// Also verify wantVolumes (for the nil case this is empty, so no-op).
			for _, want := range tc.wantVolumes {
				_, ok := volByName[want]
				assert.True(t, ok, "expected volume %q", want)
			}
		})
	}
}

func TestBuild_NoSkillBundles_NoInitContainerOrSkillsVolume(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")

	assert.Empty(t, pod.Spec.InitContainers, "no mounts and no bundles → no init container")
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, "unpacked", v.Name, "no tarGz mounts → no shared unpack volume")
		assert.NotContains(t, v.Name, "mount-src", "no tarGz mounts → no mount-src volume")
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, "/skills", m.MountPath, "no bundles → sandbox container has no /skills mount")
	}
}

// TestBuild_SkillBundles_MountsAndUntars pins the deprecated SkillBundles
// fallback's rendered shape now that it is converted to SpiceboxMount and
// routed through the same applyMounts machinery every native tarGz mount
// uses (see TestApplyMounts_TarGzExpandsUnderOneSharedVolume in
// mounts_test.go for that machinery's own coverage) — "unpacked" shared
// volume, "mount-unpack" init container, per-bundle subPath views, not the
// pre-Mounts "skills"/"skill-unpack" shape.
func TestBuild_SkillBundles_MountsAndUntars(t *testing.T) {
	sess := baseSession()
	sess.Spec.SkillBundles = []spiceboxv1alpha1.SkillBundleMount{
		{MountName: "alpha-aa11bb22", ConfigMapName: "skillbundle-sess-1-alpha-aa11bb22"},
		{MountName: "beta-cc33dd44", ConfigMapName: "skillbundle-sess-1-beta-cc33dd44"},
	}
	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")

	// Shared unpack emptyDir present.
	volByName := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		volByName[v.Name] = v
	}
	unpackVol, ok := volByName["unpacked"]
	require.True(t, ok, "expected the shared 'unpacked' emptyDir volume")
	require.NotNil(t, unpackVol.EmptyDir, "'unpacked' must be an emptyDir")

	// One ConfigMap source volume per bundle, projecting bundle.tar.gz.
	srcVolNames := []string{}
	for name, v := range volByName {
		if v.ConfigMap != nil {
			srcVolNames = append(srcVolNames, name)
			require.Len(t, v.ConfigMap.Items, 1, "%s projects exactly the tarball key", name)
			assert.Equal(t, "bundle.tar.gz", v.ConfigMap.Items[0].Key, "%s key", name)
			assert.Equal(t, "bundle.tar.gz", v.ConfigMap.Items[0].Path, "%s path", name)
		}
	}
	sort.Strings(srcVolNames)
	assert.Equal(t, []string{"mount-src-0", "mount-src-1"}, srcVolNames, "two ConfigMap source volumes")
	assert.Equal(t, "skillbundle-sess-1-alpha-aa11bb22", volByName["mount-src-0"].ConfigMap.LocalObjectReference.Name)
	assert.Equal(t, "skillbundle-sess-1-beta-cc33dd44", volByName["mount-src-1"].ConfigMap.LocalObjectReference.Name)

	// Exactly one mount-unpack init container untarring both bundles.
	require.Len(t, pod.Spec.InitContainers, 1, "exactly one init container")
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, "mount-unpack", ic.Name, "init container name")
	assert.Equal(t, baseClass().Image, ic.Image, "init container reuses the sandbox image")
	require.Len(t, ic.Command, 3, "sh -c <script>")
	assert.Equal(t, []string{"/bin/sh", "-c"}, ic.Command[:2], "init container invokes a shell")
	script := ic.Command[2]
	assert.Contains(t, script, `tar -xzf '/staging/alpha-aa11bb22/bundle.tar.gz' -C '/unpacked/alpha-aa11bb22'`, "untars alpha")
	assert.Contains(t, script, `tar -xzf '/staging/beta-cc33dd44/bundle.tar.gz' -C '/unpacked/beta-cc33dd44'`, "untars beta")
	assert.Contains(t, script, `mkdir -p '/unpacked/alpha-aa11bb22'`, "mkdir alpha")
	assert.Contains(t, script, `mkdir -p '/unpacked/beta-cc33dd44'`, "mkdir beta")

	// Init container mounts the shared unpack emptyDir rw at /unpacked plus
	// each source ConfigMap read-only under /staging/<MountName>/.
	icMounts := map[string]corev1.VolumeMount{}
	for _, m := range ic.VolumeMounts {
		icMounts[m.Name] = m
	}
	require.Contains(t, icMounts, "unpacked", "init container mounts the shared unpack emptyDir")
	assert.Equal(t, "/unpacked", icMounts["unpacked"].MountPath, "unpack mount path")
	assert.False(t, icMounts["unpacked"].ReadOnly, "init container writes /unpacked (rw)")
	require.Contains(t, icMounts, "mount-src-0", "init container mounts the alpha source")
	assert.Equal(t, "/staging/alpha-aa11bb22", icMounts["mount-src-0"].MountPath)
	assert.True(t, icMounts["mount-src-0"].ReadOnly, "source ConfigMap is read-only")
	require.Contains(t, icMounts, "mount-src-1", "init container mounts the beta source")
	assert.Equal(t, "/staging/beta-cc33dd44", icMounts["mount-src-1"].MountPath)

	// The sandbox container mounts each bundle at its own path, isolated from
	// the other by subPath into the shared unpack volume.
	sandboxMounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		sandboxMounts[m.MountPath] = m
	}
	require.Contains(t, sandboxMounts, "/skills/alpha-aa11bb22")
	alpha := sandboxMounts["/skills/alpha-aa11bb22"]
	assert.Equal(t, "unpacked", alpha.Name, "backed by the shared unpack volume")
	assert.Equal(t, "alpha-aa11bb22", alpha.SubPath)
	assert.True(t, alpha.ReadOnly, "sandbox mounts staged skills read-only")

	require.Contains(t, sandboxMounts, "/skills/beta-cc33dd44")
	beta := sandboxMounts["/skills/beta-cc33dd44"]
	assert.Equal(t, "unpacked", beta.Name, "backed by the shared unpack volume")
	assert.Equal(t, "beta-cc33dd44", beta.SubPath)
	assert.True(t, beta.ReadOnly, "sandbox mounts staged skills read-only")
}

// cmSource builds a MountSource pointing at a ConfigMap by name — mirrors
// mounts_test.go's helper of the same name, which lives in the internal
// `podspec` test package and so is not visible from this external
// `podspec_test` package.
func cmSource(name string) spiceboxv1alpha1.MountSource {
	return spiceboxv1alpha1.MountSource{
		ConfigMapRef: &corev1.LocalObjectReference{Name: name},
	}
}

// mountPaths returns every VolumeMount on the sandbox container, keyed by
// MountPath, so a test can assert a mount landed (assert.Contains checks map
// keys) and then inspect its full rendered shape — without caring which
// provenance, class-authored or session-derived, produced it.
func mountPaths(t *testing.T, pod *corev1.Pod) map[string]corev1.VolumeMount {
	t.Helper()
	got := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		got[m.MountPath] = m
	}
	return got
}

func TestBuild_MergesAuthoredAndDerivedMounts(t *testing.T) {
	class := baseClass()
	class.Mounts = []spiceboxv1alpha1.SpiceboxMount{
		{Name: "demo-cfg", MountPath: "/etc/demo", Source: cmSource("demo-class-cm")},
	}
	sess := baseSession()
	sess.Spec.Mounts = []spiceboxv1alpha1.SpiceboxMount{
		{Name: "code-review", MountPath: "/skills/code-review",
			Format: spiceboxv1alpha1.MountFormatTarGz, Source: cmSource("demo-sess-cm")},
	}

	pod, err := podspec.Build(sess, class)
	require.NoError(t, err, "Build")

	mounts := mountPaths(t, pod)
	require.Contains(t, mounts, "/etc/demo", "authored class mount landed")
	require.Contains(t, mounts, "/skills/code-review", "controller-derived session mount landed")

	// The class mount is raw: a direct read-only ConfigMap volume, not a
	// subPath view into a shared unpack volume.
	classMount := mounts["/etc/demo"]
	assert.Equal(t, "demo-cfg", classMount.Name, "raw mount's VolumeMount.Name is the mount's own Name")
	assert.True(t, classMount.ReadOnly, "class mount is read-only")
	assert.Empty(t, classMount.SubPath, "a raw mount carries no subPath")

	// The session mount is tarGz: expanded by the shared mount-unpack init
	// container into the shared unpack volume, surfaced via its own subPath —
	// and backed by a DIFFERENT volume than the class's own ConfigMap mount,
	// proving the two provenances did not collide.
	sessionMount := mounts["/skills/code-review"]
	assert.True(t, sessionMount.ReadOnly, "session mount is read-only")
	assert.Equal(t, "code-review", sessionMount.SubPath, "tarGz mount is isolated by subPath")
	assert.NotEqual(t, classMount.Name, sessionMount.Name,
		"the class mount and the session mount are backed by different volumes")

	require.Len(t, pod.Spec.InitContainers, 1, "one init container expands the tarGz session mount")
	assert.Equal(t, "mount-unpack", pod.Spec.InitContainers[0].Name)
}

func TestBuild_FallsBackToDeprecatedSkillBundles(t *testing.T) {
	// A session created before this change and re-hydrated after it must still
	// build a correct pod. Sessions are wakeable; this is not hypothetical.
	sess := baseSession()
	sess.Spec.SkillBundles = []spiceboxv1alpha1.SkillBundleMount{
		{MountName: "legacy-skill", ConfigMapName: "demo-legacy-cm"},
	}

	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")

	mounts := mountPaths(t, pod)
	require.Contains(t, mounts, "/skills/legacy-skill", "deprecated SkillBundles fallback still mounts")
	assert.True(t, mounts["/skills/legacy-skill"].ReadOnly, "staged skill content is never writable")

	// Mounts is empty, so this must have gone through the SkillBundles
	// fallback (converted to a SpiceboxMount and routed through applyMounts,
	// hence "mount-unpack"), not silently produced no init container at all —
	// which is exactly what deleting the fallback branch would do.
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, "mount-unpack", pod.Spec.InitContainers[0].Name,
		"the deprecated fallback is converted to a SpiceboxMount and expanded by applyMounts")
}

// TestBuild_TarGzClassAndSessionMountsShareOneUnpacker pins the observable
// end state of the fix for the two-call collision: applyMounts is called once
// for class.Mounts (inside BuildClassSpec) and once for session-provenance
// mounts (inside Build). Both calls draw the shared tarGz-unpack volume and
// init-container name from the same package constants, so if either call
// blindly appends rather than reusing what the other already created, this
// pod would carry two volumes named "unpacked" and two "mount-unpack" init
// containers — a duplicate-name PodSpec the API server rejects and the
// session never starts. This test must fail against code that appends
// unconditionally.
func TestBuild_TarGzClassAndSessionMountsShareOneUnpacker(t *testing.T) {
	class := baseClass()
	class.Mounts = []spiceboxv1alpha1.SpiceboxMount{
		{Name: "class-skill", MountPath: "/skills/class-skill",
			Format: spiceboxv1alpha1.MountFormatTarGz, Source: cmSource("demo-class-cm")},
	}
	sess := baseSession()
	sess.Spec.Mounts = []spiceboxv1alpha1.SpiceboxMount{
		{Name: "session-skill", MountPath: "/skills/session-skill",
			Format: spiceboxv1alpha1.MountFormatTarGz, Source: cmSource("demo-sess-cm")},
	}

	pod, err := podspec.Build(sess, class)
	require.NoError(t, err, "Build")

	// Exactly one shared unpack volume, not one per applyMounts call.
	unpackVols := 0
	for _, v := range pod.Spec.Volumes {
		if v.Name == "unpacked" {
			unpackVols++
		}
	}
	assert.Equal(t, 1, unpackVols, "class and session tarGz mounts must share ONE unpack volume")

	// Exactly one init container, not one per applyMounts call.
	require.Len(t, pod.Spec.InitContainers, 1, "class and session tarGz mounts must share ONE init container")
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, "mount-unpack", ic.Name)

	// Both archives are actually expanded by that one init container: its
	// script names both mounts' extract steps, and it mounts both source
	// ConfigMaps (not just whichever call happened to run second).
	require.Len(t, ic.Command, 3, "sh -c <script>")
	script := ic.Command[2]
	assert.Contains(t, script, `mkdir -p '/unpacked/class-skill' && tar -xzf '/staging/class-skill/bundle.tar.gz' -C '/unpacked/class-skill'`,
		"the class mount's extract step must survive the session call's merge")
	assert.Contains(t, script, `mkdir -p '/unpacked/session-skill' && tar -xzf '/staging/session-skill/bundle.tar.gz' -C '/unpacked/session-skill'`,
		"the session mount's extract step must be present")

	require.Len(t, ic.VolumeMounts, 3, "one rw mount for the shared unpack volume plus one ro mount per archive source — no duplicates")

	// Distinct, non-restarting source-volume names: if the session call's
	// per-archive numbering restarted from 0 instead of continuing from where
	// the class call left off, both source volumes would be named
	// "mount-src-0" — spec.Volumes would carry two Volumes with the same
	// name (an invalid PodSpec the API server rejects) and the init
	// container's own VolumeMounts would carry the duplicate too, yet every
	// assertion above (which only checks /skills/<name> mounts and the
	// script's extract steps) would still pass, since neither looks at which
	// distinct volume backs each archive's staging source.
	var icSrcVolNames []string
	for _, m := range ic.VolumeMounts {
		if m.Name != "unpacked" {
			icSrcVolNames = append(icSrcVolNames, m.Name)
		}
	}
	assert.ElementsMatch(t, []string{"mount-src-0", "mount-src-1"}, icSrcVolNames,
		"class and session archives must get distinct, non-restarting source-volume names on the init container")

	var podSrcVolNames []string
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil && v.Name != "class-skill" && v.Name != "session-skill" {
			podSrcVolNames = append(podSrcVolNames, v.Name)
		}
	}
	assert.ElementsMatch(t, []string{"mount-src-0", "mount-src-1"}, podSrcVolNames,
		"pod.Spec.Volumes must carry two DISTINCT source volumes, not a duplicate-named pair")

	// Both mounts land on the sandbox container at their own path, each
	// isolated by its own subPath into the ONE shared unpack volume.
	sandboxMounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		sandboxMounts[m.MountPath] = m
	}
	require.Contains(t, sandboxMounts, "/skills/class-skill")
	classMount := sandboxMounts["/skills/class-skill"]
	assert.Equal(t, "unpacked", classMount.Name)
	assert.Equal(t, "class-skill", classMount.SubPath)
	assert.True(t, classMount.ReadOnly)

	require.Contains(t, sandboxMounts, "/skills/session-skill")
	sessionMount := sandboxMounts["/skills/session-skill"]
	assert.Equal(t, "unpacked", sessionMount.Name)
	assert.Equal(t, "session-skill", sessionMount.SubPath)
	assert.True(t, sessionMount.ReadOnly)
}

func TestBuild_AppliesClassEnvDefaults(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantEnv map[string]string
	}{
		{
			name:    "nil EnvDefaults: container.Env is empty",
			env:     nil,
			wantEnv: map[string]string{},
		},
		{
			name:    "single key: container.Env has one entry",
			env:     map[string]string{"GIT_DIR": "/var/ap-git/.git"},
			wantEnv: map[string]string{"GIT_DIR": "/var/ap-git/.git"},
		},
		{
			name:    "multi-key: container.Env sorted by name",
			env:     map[string]string{"GIT_DIR": "/var/ap-git/.git", "GIT_WORK_TREE": "/workspace"},
			wantEnv: map[string]string{"GIT_DIR": "/var/ap-git/.git", "GIT_WORK_TREE": "/workspace"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := baseClass()
			class.EnvDefaults = tc.env
			sess := baseSession()

			pod, err := podspec.Build(sess, class)
			require.NoError(t, err)

			// Build today appends nothing else to container.Env beyond
			// the sorted EnvDefaults entries, so we can assert exact
			// slice equality. This catches both spurious extras and a
			// regression that drops the sort step (the determinism
			// comment in builder.go promises sorted order). Use a nil
			// slice (rather than make()) so the zero-env case compares
			// equal to Build's zero-value (nil) container.Env.
			var expected []corev1.EnvVar
			keys := make([]string, 0, len(tc.wantEnv))
			for k := range tc.wantEnv {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				expected = append(expected, corev1.EnvVar{Name: k, Value: tc.wantEnv[k]})
			}
			assert.Equal(t, expected, pod.Spec.Containers[0].Env)
		})
	}
}

func goMount() spiceboxv1alpha1.ToolchainMount {
	return spiceboxv1alpha1.ToolchainMount{
		Name: "go", SourceKind: "image",
		Image:  "ap-toolchain-go:dev",
		Prefix: "/opt/ap-toolchains/go",
		Bin:    []string{"bin", "tools/bin"},
		Env: map[string]string{
			"GOROOT":  "/opt/ap-toolchains/go",
			"GOCACHE": "/var/ap-cache/gobuild",
		},
		SizeBytes: 500 * 1024 * 1024,
	}
}

func nodeMount() spiceboxv1alpha1.ToolchainMount {
	return spiceboxv1alpha1.ToolchainMount{
		Name: "node", SourceKind: "image",
		Image: "ap-toolchain-node:dev", Prefix: "/opt/ap-toolchains/node",
		Bin: []string{"bin"}, SizeBytes: 150 * 1024 * 1024,
	}
}

func TestBuild_NoToolchains_NoOverlayOrCacheVolume(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")
	for _, v := range pod.Spec.Volumes {
		assert.NotEqual(t, "toolchains", v.Name, "no toolchains volume when none are resolved")
		assert.NotEqual(t, "ap-cache", v.Name, "no cache volume when no toolchains are resolved")
	}
	assert.Empty(t, pod.Spec.InitContainers, "no init containers")
}

func TestBuild_Toolchains_MountsReadOnlyAndAddsDiskCache(t *testing.T) {
	sess := baseSession()
	// Deliberately out of order: the builder must sort by name so the PodSpec
	// is byte-stable across reconciles (SSA idempotency).
	sess.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{nodeMount(), goMount()}

	pod, err := podspec.Build(sess, baseClass())
	require.NoError(t, err, "Build")

	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	require.Contains(t, vols, "toolchains")
	require.NotNil(t, vols["toolchains"].EmptyDir, "toolchains is an emptyDir")
	assert.NotEqual(t, corev1.StorageMediumMemory, vols["toolchains"].EmptyDir.Medium,
		"toolchain payloads are hundreds of MB and must not sit in tmpfs")
	require.Contains(t, vols, "ap-cache")
	require.NotNil(t, vols["ap-cache"].EmptyDir)
	assert.NotEqual(t, corev1.StorageMediumMemory, vols["ap-cache"].EmptyDir.Medium,
		"GOCACHE on a memory emptyDir is charged against the pod memory limit")

	// One init container per toolchain, sorted by name.
	require.Len(t, pod.Spec.InitContainers, 2)
	assert.Equal(t, "toolchain-go", pod.Spec.InitContainers[0].Name)
	assert.Equal(t, "toolchain-node", pod.Spec.InitContainers[1].Name)

	c := pod.Spec.Containers[0]
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	require.Contains(t, mounts, "toolchains")
	assert.Equal(t, "/opt/ap-toolchains", mounts["toolchains"].MountPath)
	assert.True(t, mounts["toolchains"].ReadOnly, "the agent must not be able to tamper with its own toolchain")
	require.Contains(t, mounts, "ap-cache")
	assert.Equal(t, "/var/ap-cache", mounts["ap-cache"].MountPath)
	assert.False(t, mounts["ap-cache"].ReadOnly, "the cache is written by every build")

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t,
		"/opt/ap-toolchains/go/bin:/opt/ap-toolchains/go/tools/bin:/opt/ap-toolchains/node/bin:"+
			"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		env["PATH"],
		"PATH is a literal: pkg/tools/exec/remote wraps exec as `env KEY=val CMD`, which replaces rather than expands")
	assert.Equal(t, "/var/ap-cache/tmp", env["TMPDIR"], "/tmp is a 50Mi tmpfs")
	assert.Equal(t, "/opt/ap-toolchains/go", env["GOROOT"])
	assert.Equal(t, "/var/ap-cache/gobuild", env["GOCACHE"])
}

func TestBuild_Toolchains_EphemeralStorageCoversPayloads(t *testing.T) {
	sess := baseSession()
	sess.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{goMount(), nodeMount()}
	class := baseClass() // EphemeralStorage: 100Mi

	pod, err := podspec.Build(sess, class)
	require.NoError(t, err, "Build")

	payload := int64(650 * 1024 * 1024)
	overlayBytes := payload + payload/10 // 10% slack, same formula the builder applies to the emptyDir SizeLimit

	got := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]
	// 100Mi class + (payload + 10% slack) overlay + 2Gi cache headroom.
	want := int64(100*1024*1024) + overlayBytes + int64(2*1024*1024*1024)
	assert.Equal(t, want, got.Value(),
		"the overlay emptyDir is charged against ephemeral-storage; an unraised limit evicts the pod mid-copy")

	var toolchainsVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "toolchains" {
			toolchainsVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, toolchainsVol, "toolchains volume must exist")
	require.NotNil(t, toolchainsVol.EmptyDir, "toolchains volume must be an emptyDir")
	require.NotNil(t, toolchainsVol.EmptyDir.SizeLimit, "toolchains emptyDir must have a SizeLimit")
	assert.Equal(t, overlayBytes, toolchainsVol.EmptyDir.SizeLimit.Value(),
		"the toolchains emptyDir SizeLimit and the ephemeral-storage limit's overlay term must agree, or the volume can fill legally and still trip the container limit")
}

func TestBuild_Toolchains_UnknownSourceKindFailsClosed(t *testing.T) {
	sess := baseSession()
	m := goMount()
	m.SourceKind = "wormhole"
	sess.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{m}

	_, err := podspec.Build(sess, baseClass())
	require.Error(t, err, "an unregistered delivery kind must fail closed, never silently skip")
	assert.Contains(t, err.Error(), "wormhole")
	assert.Contains(t, err.Error(), "image", "the error lists the known kinds")
}

func TestBuildClassSpec_UnknownMountSourceIsAnError(t *testing.T) {
	_, err := podspec.BuildClassSpec(spiceboxv1alpha1.SpiceboxClassSpec{
		// Image is required before the mount loop runs at all; set it so this
		// test actually reaches (and exercises) the mount source check instead
		// of failing earlier on the class.Image precondition.
		Image: "registry.example.com/demo:1.0.0",
		Mounts: []spiceboxv1alpha1.SpiceboxMount{
			{Name: "demo", MountPath: "/demo"}, // Source has no member set
		},
	}, nil)

	require.Error(t, err, "an unrecognised mount source must fail closed, not mount nothing")
	assert.Contains(t, err.Error(), "demo", "the error must name the offending mount")
}

func TestSpiceboxMount_FormatDefaultsToRawInTheGeneratedCRD(t *testing.T) {
	// The default lives in the CRD, not in Go, so read the shipped bundle.
	schema := crdschematest.FieldSchema(t, "spiceboxclasses", "spec.mounts.items.properties.format")

	require.NotNil(t, schema.Default, "format field must carry a default in the generated CRD")
	assert.JSONEq(t, `"raw"`, string(schema.Default.Raw),
		"an existing mount with no format must keep today's behaviour")

	enum := make([]string, 0, len(schema.Enum))
	for _, e := range schema.Enum {
		var v string
		require.NoError(t, json.Unmarshal(e.Raw, &v), "enum entry must decode to a string")
		enum = append(enum, v)
	}
	assert.ElementsMatch(t, []string{"raw", "tarGz"}, enum)
}

func TestBuild_ScratchSizes_DefaultWhenUnset(t *testing.T) {
	pod, err := podspec.Build(baseSession(), baseClass())
	require.NoError(t, err, "Build")

	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	require.NotNil(t, vols["work"].EmptyDir, "work volume")
	require.NotNil(t, vols["tmp"].EmptyDir, "tmp volume")
	assert.Equal(t, "100Mi", vols["work"].EmptyDir.SizeLimit.String(), "work defaults to 100Mi")
	assert.Equal(t, "50Mi", vols["tmp"].EmptyDir.SizeLimit.String(), "tmp defaults to 50Mi")
	assert.Equal(t, corev1.StorageMediumMemory, vols["tmp"].EmptyDir.Medium,
		"scratch stays memory-backed; a build cache belongs on /var/ap-cache instead")
}

func TestBuild_ScratchSizes_HonorClassOverrides(t *testing.T) {
	class := baseClass()
	class.Resources.Memory = resource.MustParse("4Gi")
	class.Resources.TmpSize = ptr.To(resource.MustParse("512Mi"))
	class.Resources.WorkSize = ptr.To(resource.MustParse("256Mi"))

	pod, err := podspec.Build(baseSession(), class)
	require.NoError(t, err, "Build")

	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	assert.Equal(t, "512Mi", vols["tmp"].EmptyDir.SizeLimit.String())
	assert.Equal(t, "256Mi", vols["work"].EmptyDir.SizeLimit.String())
}

func TestBuild_Toolchains_CacheSizeDefaultsTo2Gi(t *testing.T) {
	sess := baseSession()
	sess.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{goMount(), nodeMount()}

	pod, err := podspec.Build(sess, baseClass()) // no cacheSize override
	require.NoError(t, err, "Build")

	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	require.NotNil(t, vols["ap-cache"].EmptyDir, "ap-cache volume")
	require.NotNil(t, vols["ap-cache"].EmptyDir.SizeLimit, "ap-cache emptyDir must have a SizeLimit")
	assert.Equal(t, int64(2*1024*1024*1024), vols["ap-cache"].EmptyDir.SizeLimit.Value(),
		"an unset cacheSize keeps the historical 2Gi disk cache")
}

func TestBuild_Toolchains_CacheSizeHonorsClassOverride(t *testing.T) {
	sess := baseSession()
	sess.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{goMount(), nodeMount()}
	class := baseClass() // EphemeralStorage: 100Mi
	class.Resources.CacheSize = ptr.To(resource.MustParse("8Gi"))

	pod, err := podspec.Build(sess, class)
	require.NoError(t, err, "Build")

	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	require.NotNil(t, vols["ap-cache"].EmptyDir.SizeLimit, "ap-cache emptyDir must have a SizeLimit")
	assert.Equal(t, int64(8*1024*1024*1024), vols["ap-cache"].EmptyDir.SizeLimit.Value(),
		"a build box reviewing a large-dependency PR needs a bigger disk cache than 2Gi")

	// The cache emptyDir is charged against ephemeral-storage, so the container
	// limit must be raised by the SAME cacheSize — otherwise the volume fills
	// legally and still trips the container limit, evicting the pod mid-build.
	payload := int64(650 * 1024 * 1024)
	overlayBytes := payload + payload/10
	got := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]
	want := int64(100*1024*1024) + overlayBytes + int64(8*1024*1024*1024)
	assert.Equal(t, want, got.Value(),
		"the ephemeral-storage limit must track the configured cacheSize, not a fixed 2Gi")
}
