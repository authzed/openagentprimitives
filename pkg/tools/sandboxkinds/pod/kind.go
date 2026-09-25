// Package pod implements the built-in sandbox backend: a corev1.Pod built by
// pkg/platform/podspec. It is the battery — the kind every SpiceboxClass gets when
// spec.sandbox.kind is unset.
package pod

import (
	"fmt"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// KindName is this backend's registry key. It derives from the API package's
// default rather than repeating the literal, so the CRD default marker, the
// registry key and SandboxBackend.ResolvedKind can never drift apart.
const KindName = v1alpha1.DefaultSandboxKind

func init() { registry.Register(Kind{}) }

// Kind is the built-in pod backend. Stateless; all state lives on Runtime.
type Kind struct{}

func (Kind) Name() string { return KindName }

func (Kind) Supports(f sandboxkinds.Feature) bool {
	switch f {
	case sandboxkinds.FeatureSharedWorkspace,
		sandboxkinds.FeatureConfigMapMounts,
		sandboxkinds.FeatureToolchainOverlay,
		sandboxkinds.FeatureUnpackMounts:
		return true
	case sandboxkinds.FeatureHostEgressAllowlist:
		// Per-session NetworkPolicies enforce L3/L4 only. The merged hostname
		// allowlist is recorded on status for a DNS-aware policy controller to
		// consume, but nothing in AP enforces it, so this must not claim it.
		return false
	default:
		return false
	}
}

// WorkspaceDomain is a Kubernetes PVC: any backend reporting the same domain
// can share a workspace with a pod sandbox; a backend outside the cluster
// cannot and reports its own.
func (Kind) WorkspaceDomain() string { return sandboxkinds.DomainKubernetesPVC }

// ValidateClass accepts any class the pod builder can render. Field-level
// checks (scratch sizes, env defaults, private volumes) already run in the
// spiceboxclass controller and are not duplicated here. An empty image is not
// an error: the operator substitutes its configured default sandbox image.
func (Kind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }

// NewRuntime builds the pod runtime. Fails closed without a client rather than
// returning a runtime that would nil-panic on first use.
func (Kind) NewRuntime(deps sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	if deps.Client == nil {
		return nil, fmt.Errorf("pod sandbox kind requires a Kubernetes client")
	}
	// Declared as the interface, assigned only once real: returning a
	// typed-nil *Runtime would satisfy a != nil guard and panic on first call.
	var rt sandboxkinds.Runtime = &Runtime{deps: deps}
	return rt, nil
}

var _ sandboxkinds.Kind = Kind{}
