// Package image delivers a toolchain payload by running the toolchain's own
// OCI image as an init container that copies the payload into a shared
// emptyDir, which the sandbox container then mounts read-only.
//
// This is the portable mechanism: it needs no feature gate, no minimum
// Kubernetes version, and no particular container runtime — unlike the native
// image-volume source (KEP-4639), which is GA only in 1.36 and requires
// containerd >= 2.1. The cost is one copy and transient disk.
package image

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/registry"
)

func init() { registry.Register(Kind{}) }

type Kind struct{}

func (Kind) Name() string { return "image" }

// Validate rejects m.Name if it is not a DNS-1123 label. This Kind is the
// thing that builds "toolchain-"+m.Name into a corev1.Container.Name (which
// must be a DNS-1123 label), so it must not trust the name to already be
// well-formed — ValidateToolchainSpec checks the same invariant at CR
// admission, but this Kind is reachable independently and cannot rely on
// that caller having run first.
func (Kind) Validate(m spiceboxv1alpha1.ToolchainMount) error {
	if m.Image == "" {
		return fmt.Errorf("toolchain %q: source.image is required for kind %q", m.Name, "image")
	}
	if !strings.HasPrefix(m.Prefix, "/") {
		return fmt.Errorf("toolchain %q: source.prefix %q must be absolute", m.Name, m.Prefix)
	}
	if errs := validation.IsDNS1123Label(m.Name); len(errs) > 0 {
		return fmt.Errorf("toolchain %q: name is not a DNS-1123 label: %s", m.Name, strings.Join(errs, "; "))
	}
	return nil
}

// Apply adds one init container running m.Image that copies m.Prefix into
// <InitMountPath>/<m.Name>. Since m.Prefix is pinned to
// ToolchainRootPath/<name> and the sandbox mounts the shared volume at
// ToolchainRootPath, the payload's absolute paths are identical at build time
// and at run time.
//
// cp -R, not cp -a: the init container runs as uid 1000 with all capabilities
// dropped, so preserving ownership (-a implies -p implies chown) fails. -R
// preserves the mode bits, which is all that matters — the payload is laid
// down world-readable (chmod -R a+rX) by its Dockerfile.
func (k Kind) Apply(p toolchain.ApplyParams) error {
	if err := k.Validate(p.Mount); err != nil {
		return err
	}
	dst := p.InitMountPath + "/" + p.Mount.Name
	script := fmt.Sprintf("mkdir -p %s && cp -R %s %s",
		shellQuote(dst), shellQuote(p.Mount.Prefix+"/."), shellQuote(dst))

	p.Pod.Spec.InitContainers = append(p.Pod.Spec.InitContainers, corev1.Container{
		Name:            "toolchain-" + p.Mount.Name,
		Image:           p.Mount.Image,
		Command:         []string{"/bin/sh", "-c", script},
		SecurityContext: p.SecurityContext,
		VolumeMounts: []corev1.VolumeMount{
			{Name: p.VolumeName, MountPath: p.InitMountPath},
		},
	})
	return nil
}

// shellQuote wraps s in single quotes for safe interpolation into a /bin/sh -c
// script, escaping an embedded quote by closing, emitting a literal, reopening.
// NOT interchangeable with fmt.Sprintf's %q, which applies Go string-literal
// escaping and leaves "$" and the backtick — both command substitution inside
// double quotes — untouched. Single quotes suppress every expansion except the
// quote character itself.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
