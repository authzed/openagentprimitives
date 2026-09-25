// toolkitresolve.go — toolkit resolution + effective-settings gating for a
// tool bundle's CLI credential surface. Extracted out of internal/cmd/runner (package
// main) so BOTH the production runner's bundle loop AND the operator's
// token-grant writer (pkg/controllers/agentsession) derive a bundle's
// credential Source from the SAME toolkit object and under the SAME allowlist
// gate. Divergence here is not cosmetic: a different toolkit → different
// authkind requirements → a different credential Source → a different
// externaltoken credID than the operator blessed a grant for, silently failing
// the sandbox use_token check closed on first use.
package runner

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// resolveBuiltinToolkit returns the builtin toolkit for the given name, or nil
// when no embedded-catalog entry matches.
func resolveBuiltinToolkit(name string) *toolkit.Toolkit {
	for _, tk := range toolkits.All() {
		if tk.Name == name {
			tk := tk
			return &tk
		}
	}
	return nil
}

// ResolveToolkitForBundle resolves a toolspec's toolkit for CLI credential
// injection: the embedded builtin catalog first (gh/kubectl/git/…), then a
// cluster-scoped SpiceboxToolkit CR of the same name (a custom toolkit — e.g.
// an example agent's own CLI wrapper). Errors only when NEITHER resolves.
//
// The CR fallback is load-bearing: a custom SpiceboxToolkit declares its
// credentials via env.allowed (provider + credential), and those can only be
// injected into the sandbox if the toolkit object is resolvable here. Without
// this fallback the runner exits before serving any turn for any agent whose
// toolspec references a non-builtin toolkit (observed as a CrashLoopBackOff
// with "toolkit \"…\" did not resolve").
func ResolveToolkitForBundle(ctx context.Context, c client.Client, name string) (*toolkit.Toolkit, error) {
	if tk := resolveBuiltinToolkit(name); tk != nil {
		return tk, nil
	}
	var stk spiceboxv1alpha1.SpiceboxToolkit
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &stk); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("toolkit %q did not resolve (no builtin, no SpiceboxToolkit CR) — cannot inject its credentials", name)
		}
		return nil, fmt.Errorf("get SpiceboxToolkit %q: %w", name, err)
	}
	tk, err := stk.Spec.ToToolkit()
	if err != nil {
		return nil, fmt.Errorf("convert SpiceboxToolkit %q: %w", name, err)
	}
	return tk, nil
}

// ToolkitAllowed reports whether the toolkit identified by toolkitName may be
// loaded under the effective settings. Tri-state semantics on
// eff.AllowedToolkits:
//
//   - nil eff, or nil/absent AllowedToolkits → unconstrained → true
//   - non-nil empty AllowedToolkits → allow-none → false
//   - non-nil non-empty AllowedToolkits → true iff toolkitName is listed
func ToolkitAllowed(eff *spiceboxv1alpha1.EffectiveSettings, toolkitName string) bool {
	if eff == nil {
		return true
	}
	if eff.AllowedToolkits == nil {
		// nil slice ≡ no tier constrained toolkits → unconstrained
		return true
	}
	for _, name := range eff.AllowedToolkits {
		if name == toolkitName {
			return true
		}
	}
	return false
}
