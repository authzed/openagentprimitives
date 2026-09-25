// Package cli is the authkind impl for the "cli" prefix. Targets are
// Toolkits resolved via the embedded /toolkits/ catalog or
// SpiceboxToolkit CRs. SetupRequirements enumerates sensitive env vars
// that need credentials.
package cli

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// Kind is the authkind impl for "cli".
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

func (Kind) Prefix() string { return "cli" }

// ResolveTarget looks up a Toolkit by name. Prefers cluster SpiceboxToolkit CRs
// (when c is non-nil) and falls back to the compile-time embedded built-ins.
func (Kind) ResolveTarget(ctx context.Context, c client.Client, namespace, suffix string) (authkind.Target, error) {
	// Cluster lookup first (cluster-scoped CR; namespace ignored for SpiceboxToolkit).
	if c != nil {
		var sbtk spiceboxv1alpha1.SpiceboxToolkit
		// SpiceboxToolkit is cluster-scoped: empty namespace key.
		err := c.Get(ctx, client.ObjectKey{Name: suffix}, &sbtk)
		switch {
		case err == nil:
			tk, terr := sbtk.Spec.ToToolkit()
			if terr != nil {
				return nil, fmt.Errorf("cli: SpiceboxToolkit %q malformed: %w", suffix, terr)
			}
			return cliTarget{tk: tk}, nil
		case !apierrors.IsNotFound(err):
			// Only a genuine NotFound falls through to the embedded built-in: a
			// transient or RBAC error must NOT silently mask a (possibly locked-down)
			// operator CR behind a more-permissive embedded toolkit.
			return nil, fmt.Errorf("cli: get SpiceboxToolkit %q: %w", suffix, err)
		}
	}
	// Embedded fallback.
	for _, tk := range toolkits.All() {
		if tk.Name == suffix {
			cp := tk
			return cliTarget{tk: &cp}, nil
		}
	}
	return nil, fmt.Errorf("cli: toolkit %q: %w", suffix, authkind.ErrTargetNotFound)
}

func (Kind) SetupRequirements(_ context.Context, tgt authkind.Target) []authkind.CredentialRequirement {
	t, ok := tgt.(cliTarget)
	if !ok {
		return nil
	}
	var out []authkind.CredentialRequirement
	for _, e := range t.tk.Env.Allowed {
		if !e.Sensitive {
			continue
		}
		// e.Credential is required for sensitive envs (toolkit validation) — no inference fallback.
		out = append(out, authkind.CredentialRequirement{
			SuggestedName: e.Credential,
			ProviderID:    e.Provider,
			InlinePrompt:  e.Prompt,
			BindingEnv:    map[string]string{e.Name: ""},
			Inject:        authkind.Injection{EnvVar: e.Name},
		})
	}
	return out
}

// NewTarget wraps an already-parsed Toolkit into a Target, for callers that want
// to skip the catalog/CR lookup hop (e.g. the runtime credential resolver). The
// pointer must not be mutated by the caller.
func NewTarget(tk *toolkit.Toolkit) authkind.Target {
	return cliTarget{tk: tk}
}

// cliTarget wraps a parsed Toolkit. Exposed via the Toolkit() accessor
// for the toolspec authkind to inspect subcommand effects.
type cliTarget struct {
	tk *toolkit.Toolkit
}

func (t cliTarget) Name() string               { return t.tk.Name }
func (t cliTarget) BindingMatchString() string { return "cli:" + t.tk.Name }
func (t cliTarget) Intent() string             { return "" } // toolkits carry per-subcommand descriptions, not a top-level intent

// Toolkit exposes the underlying parsed toolkit so the toolspec kind can inspect
// subcommand effects when narrowing the credential requirement set. Returns a
// pointer callers must not mutate.
func (t cliTarget) Toolkit() *toolkit.Toolkit { return t.tk }
