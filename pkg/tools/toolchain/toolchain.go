// Package toolchain defines the pluggable seam by which a resolved toolchain
// payload is delivered into a sandbox pod. Today one Kind exists ("image":
// an initContainer that copies the payload into a shared emptyDir). A future
// "imagevolume" Kind (Kubernetes 1.36 + containerd 2.1) mounts the image
// directly with no copy, and a "nix" Kind mounts a merged content-addressed
// store. Adding either is a new package plus a blank import: pkg/platform/podspec and
// the controllers are untouched.
package toolchain

import (
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ApplyParams carries everything a Kind needs to mutate the pod. The pod
// builder owns the volume name and the init-container mount path so that Kinds
// never invent their own layout.
type ApplyParams struct {
	// Pod is mutated in place.
	Pod *corev1.Pod
	// Mount is the resolved toolchain (env already expanded).
	Mount spiceboxv1alpha1.ToolchainMount
	// VolumeName is the shared destination volume the sandbox container mounts
	// read-only at v1alpha1.ToolchainRootPath.
	VolumeName string
	// InitMountPath is where an init container should mount VolumeName
	// read-write. A Kind that needs no init container ignores it.
	InitMountPath string
	// SecurityContext must be applied to any container the Kind adds. The
	// pointer is shared across every container every Kind adds for this pod —
	// treat it as read-only, never mutate the pointee.
	SecurityContext *corev1.SecurityContext
}

// Kind is one delivery mechanism, dispatched by SpiceboxToolchain
// spec.source.kind.
type Kind interface {
	// Name is the spec.source.kind value this Kind handles.
	Name() string
	// Validate checks the Kind-specific fields of a resolved mount.
	Validate(m spiceboxv1alpha1.ToolchainMount) error
	// Apply mutates p.Pod so the payload is present at
	// v1alpha1.ToolchainRootPath/<m.Name> in the sandbox container.
	Apply(p ApplyParams) error
}
