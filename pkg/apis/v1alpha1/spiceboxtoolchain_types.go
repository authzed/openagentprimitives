package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolchainRootPath is where toolchain overlays are mounted in the sandbox
// container. A toolchain named "go" lands at /opt/ap-toolchains/go. Its image
// payload MUST be built at that same absolute path: build path == mount path is
// what makes the payload relocation-free (shebangs, RPATHs, and wrapper scripts
// all bake absolute paths). Keep in sync with pkg/platform/podspec/builder.go — podspec
// imports this package, not the reverse.
const ToolchainRootPath = "/opt/ap-toolchains"

// ToolchainCachePath is the writable, disk-backed scratch volume every
// toolchain-bearing pod mounts. /tmp and /work are memory-backed emptyDirs
// (50Mi / 100Mi) charged against the pod memory limit, so GOCACHE, TMPDIR,
// CARGO_HOME and friends must point here or a build dies immediately.
const ToolchainCachePath = "/var/ap-cache"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=sbxtc
// +kubebuilder:subresource:status
// +genclient
// +genclient:nonNamespaced
//
// SpiceboxToolchain is a language or tooling overlay a sandbox can mount -- a
// compiler, an SDK, a linter -- delivered by a registered backend and mounted
// at ToolchainRootPath/<name>, with the PATH entries and env it needs.
//
// Cluster-scoped. Reconciled by pkg/controllers/spiceboxtoolchain, which
// validates the spec and then asks the named delivery Kind to validate its own
// fields. A Valid=False toolchain is refused at session-bind time, so a broken
// catalog entry fails the session closed instead of quietly dropping a
// compiler out of the sandbox.
type SpiceboxToolchain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceboxToolchainSpec   `json:"spec,omitempty"`
	Status SpiceboxToolchainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceboxToolchainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceboxToolchain `json:"items"`
}

type SpiceboxToolchainSpec struct {
	// Description is human-facing text surfaced by `oap`.
	// +optional
	Description string `json:"description,omitempty"`

	// Source describes where the payload comes from. Kind selects a registered
	// delivery backend (pkg/tools/toolchain/kinds/registry).
	Source ToolchainSource `json:"source"`

	// Bin lists PATH entries relative to this toolchain's root. Each becomes
	// <ToolchainRootPath>/<name>/<entry> on the sandbox container's PATH.
	// +optional
	Bin []string `json:"bin,omitempty"`

	// Env is the process environment this toolchain needs. Values may reference
	// exactly two template variables, spelled with surrounding spaces:
	// "{{ .Root }}" (this toolchain's root) and "{{ .Cache }}"
	// (ToolchainCachePath). Any other template text is rejected at validation.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// Provides are opaque capability labels (e.g. "go", "gopls") surfaced to
	// operators. Advisory — nothing selects on them.
	// +optional
	Provides []string `json:"provides,omitempty"`

	// Detect are workspace-relative marker files (e.g. "go.mod") that hint this
	// toolchain is applicable. Advisory only — nothing acts on them.
	// +optional
	Detect []string `json:"detect,omitempty"`

	// SizeBytes is the payload's on-disk usage (measured via `du -sk * 1024`),
	// NOT apparent size. The kubelet enforces emptyDir SizeLimit against allocated
	// blocks; under-estimating this field evicts the pod mid-copy. The operator
	// derives both the overlay emptyDir SizeLimit and the pod's ephemeral-storage
	// limit from this value.
	// +kubebuilder:validation:Minimum=1
	SizeBytes int64 `json:"sizeBytes"`
}

type ToolchainSource struct {
	// Kind names a registered delivery backend. "image" today.
	Kind string `json:"kind"`

	// Image is the OCI image carrying the payload. Must be a regular container
	// image, NOT an ORAS artifact: containerd 2.1 silently produces an empty
	// mount for artifacts with custom layer media types.
	// +optional
	Image string `json:"image,omitempty"`

	// Prefix is the absolute path of the payload inside Image. It MUST equal
	// <ToolchainRootPath>/<metadata.name>. Redundant by construction, and kept
	// so the invariant is reviewable in the CR rather than implicit in a
	// Dockerfile.
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

type SpiceboxToolchainStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions carries Valid; see SpiceboxToolchainConditionValid.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ToolchainMount is a resolved, self-contained toolchain: everything the pod
// builder needs, with no further CR reads. Frozen onto
// SpiceboxSessionStatus.ResolvedToolchains at first bind, mirroring how
// SkillBundleMount is threaded onto the session spec.
type ToolchainMount struct {
	// Name is the toolchain name; it is also the directory under the root path.
	Name string `json:"name"`
	// SourceKind is the delivery backend that resolved this mount.
	SourceKind string `json:"sourceKind"`
	// Image is the OCI image carrying the payload.
	Image string `json:"image"`
	// Prefix is the payload's absolute path inside Image.
	Prefix string `json:"prefix"`
	// Bin lists PATH entries relative to this toolchain's root.
	// +optional
	Bin []string `json:"bin,omitempty"`
	// Env is already expanded — no templates remain.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// SizeBytes is the payload's on-disk usage, driving the emptyDir SizeLimit.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

func init() {
	SchemeBuilder.Register(&SpiceboxToolchain{}, &SpiceboxToolchainList{})
}
