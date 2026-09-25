package runner

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// BundleRuntimeIdentity resolves the RuntimeIdentity used to stamp a single
// tool bundle's CLI credential descriptors, honoring the session's RESOLVED
// effective identity mode. It is the single source of truth for that decision,
// shared by the production runner (internal/cmd/runner/main.go) and the e2e harness
// (test/e2e/inprocess_runner_factory.go): drift between the two lets a
// cross-namespace passthrough-credential bug ship, because a harness hardcoding
// the agent-identity path never exercises the passthrough sandbox-credential
// source, which must resolve from the per-session materialized Secret in the
// session namespace so a sandbox ToolCall passes
// ValidateCredentialSourceNamespaces.
//
// effectiveMode is the RESOLVED mode ("agent" | "userPassthrough"), NOT the raw
// class.Spec.IdentityMode. For static classes the two are identical; for
// ask|dynamic the caller resolves it (provisional agent on first boot, or the
// user's choice on a re-spawn). Switching on class.Spec.IdentityMode instead
// would take the agent arm for a re-spawn resolved to userPassthrough and run
// bundle tools with AGENT credentials — an identity-confusion / scope-widening
// defect.
//
//   - effectiveMode=userPassthrough: every bundle uses the single
//     session-scoped sessionRuntimeIdentity (resolved by the caller from the
//     per-session SessionUserIdentity). Bundle-level AgentIdentity overrides do
//     not apply, so the AgentIdentity is never consulted. The caller owns the
//     session-fatal handling of a missing SessionUserIdentity; this function
//     never reloads it.
//   - effectiveMode=agent (or unset): ToolBundle.AgentIdentity overrides the
//     class-level AgentIdentity for this bundle. An absent name means no
//     credentials — a nil AgentIdentity projects to a zero RuntimeIdentity, so
//     credresolve.Descriptors resolves no descriptors when the toolkit declares
//     no sensitive envs (and errors when it does).
func BundleRuntimeIdentity(
	ctx context.Context,
	c client.Client,
	ns string,
	sessionRuntimeIdentity RuntimeIdentity,
	class *spiceboxv1alpha1.AgentClass,
	bundleCfg spiceboxv1alpha1.ToolBundle,
	effectiveMode string,
) (RuntimeIdentity, error) {
	switch effectiveMode {
	case spiceboxv1alpha1.IdentityModeUserPassthrough:
		return sessionRuntimeIdentity, nil
	default: // effectiveMode=agent (or unset)
		bundleIdentityName := bundleCfg.AgentIdentity
		if bundleIdentityName == "" {
			bundleIdentityName = class.Spec.AgentIdentity
		}
		var bundleAI *spiceboxv1alpha1.AgentIdentity
		if bundleIdentityName != "" {
			var ai spiceboxv1alpha1.AgentIdentity
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: bundleIdentityName}, &ai); err != nil {
				return RuntimeIdentity{}, fmt.Errorf("get AgentIdentity %q for bundle %q: %w", bundleIdentityName, bundleCfg.Name, err)
			}
			bundleAI = &ai
		}
		return RuntimeIdentityFromAgentIdentity(bundleAI), nil
	}
}
