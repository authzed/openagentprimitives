// Package sidecartoolbox is the authkind impl for the "toolbox" prefix. Targets
// are SidecarToolbox CRs; SetupRequirements emits one requirement using
// SidecarToolbox.spec.upstreamAuth.provider. Projecting that credential into the
// sidecar's env happens at pod-build time in the agentsession controller, via the
// token broker — not here.
package sidecartoolbox

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
)

// Kind is the authkind impl for "toolbox".
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

func (Kind) Prefix() string { return "toolbox" }

// ResolveTarget looks up a SidecarToolbox CR by name in the given namespace.
// SidecarToolbox is namespace-scoped, so namespace is used in the lookup.
func (Kind) ResolveTarget(ctx context.Context, c client.Client, namespace, suffix string) (authkind.Target, error) {
	if c == nil {
		return nil, fmt.Errorf("toolbox: ResolveTarget needs a cluster client")
	}
	var cr spiceboxv1alpha1.SidecarToolbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: suffix}, &cr); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("toolbox: %q in %q: %w", suffix, namespace, authkind.ErrTargetNotFound)
		}
		return nil, fmt.Errorf("toolbox: get %q: %w", suffix, err)
	}
	return target{cr: cr.DeepCopy()}, nil
}

// SetupRequirements returns one env-projected requirement using the
// SidecarToolbox's upstreamAuth.provider, or none when the provider is the
// spiceboxv1alpha1.UpstreamAuthProviderNone sentinel.
//
// That sentinel means this sidecar's credential is the controller-issued
// projected ServiceAccount token + operator bearer (materialized entirely by
// the AgentSession/Workshop reconcilers), never an AgentIdentity credential —
// so there is nothing for `oap agent setup-identity` to walk. Returning a
// requirement here would carry the sentinel as ProviderID into the setup
// engine's provider.ByID lookup, which fails with "unknown provider" (there is
// and must never be a real provider registered under that name), hard-erroring
// a setup run for every OTHER credential requirement as well. Mirrors the
// empty-provider/empty-envVar case resolveSidecarCredential (controller.go)
// already treats as "no credential needed, write an empty Secret" — this is
// the CLI-setup-side half of that same sentinel semantics.
//
// Inject.EnvVar carries the upstream-auth env var name, which on this path is
// informational: the credential is resolved controller-side into a per-session
// Secret the sidecar envFrom's (materializeSidecarSecret in
// pkg/controllers/agentsession/controller.go), never projected by the runtime
// resolver. It is populated so every kind's requirement carries an explicit
// Inject.
func (Kind) SetupRequirements(_ context.Context, tgt authkind.Target) []authkind.CredentialRequirement {
	t, ok := tgt.(target)
	if !ok {
		return nil
	}
	if t.cr.Spec.UpstreamAuth.Provider == spiceboxv1alpha1.UpstreamAuthProviderNone {
		return nil
	}
	return []authkind.CredentialRequirement{{
		SuggestedName: t.cr.Name + "-creds",
		ProviderID:    t.cr.Spec.UpstreamAuth.Provider,
		IsBearer:      false,
		Inject:        authkind.Injection{EnvVar: t.cr.Spec.UpstreamAuth.EnvVar},
	}}
}

type target struct {
	cr *spiceboxv1alpha1.SidecarToolbox
}

func (t target) Name() string               { return t.cr.Name }
func (t target) BindingMatchString() string { return "toolbox:" + t.cr.Name }
func (t target) Intent() string             { return t.cr.Spec.Intent }

// SidecarToolbox exposes the underlying CR for callers that need
// upstreamAuth.provider or other spec fields. Returns a pointer the caller must
// not mutate.
func (t target) SidecarToolbox() *spiceboxv1alpha1.SidecarToolbox { return t.cr }

// NewTarget wraps an existing SidecarToolbox CR into a Target, for callers that
// already have the CR in scope and want to skip the lookup hop. The pointer must
// not be mutated by the caller.
func NewTarget(cr *spiceboxv1alpha1.SidecarToolbox) authkind.Target {
	return target{cr: cr}
}
