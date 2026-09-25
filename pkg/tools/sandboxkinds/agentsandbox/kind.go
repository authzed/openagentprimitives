// Package agentsandbox implements the agent-sandbox (sigs.k8s.io) backend: a
// Sandbox custom resource whose lifecycle a third-party controller owns.
//
// Bring-your-own — AP does not install the agent-sandbox controller or its
// CRDs. NewRuntime therefore fails closed when they are absent, which is what
// keeps this kind's owned-object watch unregistered on clusters that cannot
// serve it.
package agentsandbox

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// KindName is this backend's registry key, matching SpiceboxClass.spec.sandbox.kind.
const KindName = "agent-sandbox"

// GroupKind is the peer CRD this backend drives. v1beta1 is both the served
// and the storage version.
var GroupKind = schema.GroupKind{Group: "agents.x-k8s.io", Kind: "Sandbox"}

// prewarmGroupKinds are the pre-warming CRDs this backend addresses on the
// adoption path. A DIFFERENT API group from GroupKind above —
// extensions.agents.x-k8s.io ships as a separate bundle, so a cluster can
// serve Sandbox and not these. Never conflate the two: they are independently
// installable CRDs.
//
// BOTH are listed, not just SandboxClaim: tryAdopt Gets a SandboxWarmPool
// before creating a claim, and an unserved kind yields NoKindMatch rather than
// IsNotFound, so its cold-path guard would miss it and Ensure would fail for
// every eligible session. Availability is one bit, true only when every kind
// this backend addresses is servable.
var prewarmGroupKinds = []schema.GroupKind{
	{Group: "extensions.agents.x-k8s.io", Kind: "SandboxClaim"},
	{Group: "extensions.agents.x-k8s.io", Kind: "SandboxWarmPool"},
}

// apiVersion is the only version this kind speaks. Pinned rather than
// negotiated: the CRD ships a conversion webhook, and touching a non-storage
// version would route every read through a webhook AP does not deploy.
const apiVersion = "v1beta1"

func init() { registry.Register(Kind{}) }

// Kind is the agent-sandbox backend. Stateless; all state lives on Runtime.
type Kind struct{}

func (Kind) Name() string { return KindName }

// Supports matches the pod kind: shared workspaces, ConfigMap mounts,
// toolchain overlays and unpacking init containers all ride in the PodSpec
// this backend renders through podspec.Build, so the answers cannot differ.
func (Kind) Supports(f sandboxkinds.Feature) bool {
	switch f {
	case sandboxkinds.FeatureSharedWorkspace,
		sandboxkinds.FeatureConfigMapMounts,
		sandboxkinds.FeatureToolchainOverlay,
		sandboxkinds.FeatureUnpackMounts:
		return true
	case sandboxkinds.FeatureHostEgressAllowlist:
		// AP enforces L3/L4 only. The merged hostname allowlist is recorded on
		// status for a DNS-aware policy controller to consume; nothing here
		// enforces it, so this must not claim it.
		return false
	default:
		return false
	}
}

// WorkspaceDomain is the same Kubernetes PVC world the pod kind lives in: an
// agent-sandbox pod mounts the session's shared claim exactly as a pod-kind
// sandbox does, so the two can share one workspace.
func (Kind) WorkspaceDomain() string { return sandboxkinds.DomainKubernetesPVC }

// ValidateClass accepts any class podspec.Build can render, mirroring the pod
// kind: this backend renders the same PodSpec and adds no constraint of its own.
func (Kind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }

// NewRuntime fails closed when the agent-sandbox CRDs are not installed.
//
// Detection goes through the RESTMapper rather than reading the CRD object:
// it needs no RBAC on customresourcedefinitions, costs no API round trip
// beyond cached discovery, and asks exactly the question that matters -- can
// this client address a Sandbox at all.
func (Kind) NewRuntime(deps sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	if deps.Client == nil {
		return nil, fmt.Errorf("agent-sandbox kind requires a Kubernetes client")
	}
	if _, err := deps.Client.RESTMapper().RESTMapping(GroupKind, apiVersion); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf(
				"agent-sandbox CRDs are not installed in this cluster (%s/%s): %w",
				GroupKind.Group, apiVersion, err)
		}
		return nil, fmt.Errorf("resolve agent-sandbox REST mapping: %w", err)
	}
	// Pre-warming is OPTIONAL on top of the base CRD, so its absence is not a
	// failure — it is the only shape most clusters have. Resolved here rather
	// than per Ensure because addressing an unserved kind through the
	// operator's cached client starts an informer that can never sync; a
	// non-NoMatch error is treated the same way for the same reason, and is
	// logged rather than swallowed so "prewarming silently never fires" is
	// never the only symptom.
	prewarm := true
	for _, gk := range prewarmGroupKinds {
		if _, err := deps.Client.RESTMapper().RESTMapping(gk, apiVersion); err != nil {
			prewarm = false
			if !meta.IsNoMatchError(err) {
				deps.Logger.Info("agent-sandbox prewarming disabled: could not resolve the extensions REST mapping",
					"groupKind", gk.String(), "version", apiVersion, "err", err.Error())
			}
			break
		}
	}

	// Declared as the interface and assigned only once real: returning a
	// typed-nil *Runtime would satisfy a != nil guard and panic on first call.
	var rt sandboxkinds.Runtime = &Runtime{deps: deps, prewarmAvailable: prewarm}
	return rt, nil
}

var _ sandboxkinds.Kind = Kind{}
