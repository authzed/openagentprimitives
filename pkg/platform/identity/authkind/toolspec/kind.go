// Package toolspec is the authkind impl for the "toolspec" prefix. Targets
// are SpiceboxToolspec CRs resolved from the cluster. SetupRequirements
// delegates to the underlying toolkit's cli requirements, narrowed by the
// toolspec's AllowSubcommands list.
package toolspec

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// Kind is the authkind impl for "toolspec".
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

func (Kind) Prefix() string { return "toolspec" }

// ResolveTarget looks up a SpiceboxToolspec CR by name. SpiceboxToolspec is
// cluster-scoped, so the namespace parameter is carried for interface
// compatibility and not used in the lookup — only in the returned target, where
// the toolkit hop needs it.
func (Kind) ResolveTarget(ctx context.Context, c client.Client, namespace, suffix string) (authkind.Target, error) {
	if c == nil {
		return nil, fmt.Errorf("toolspec: ResolveTarget needs a cluster client")
	}
	var ts spiceboxv1alpha1.SpiceboxToolspec
	// SpiceboxToolspec is cluster-scoped: empty namespace key.
	if err := c.Get(ctx, client.ObjectKey{Name: suffix}, &ts); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("toolspec: %q: %w", suffix, authkind.ErrTargetNotFound)
		}
		return nil, fmt.Errorf("toolspec: get %q: %w", suffix, err)
	}
	return tsTarget{ts: ts.DeepCopy(), client: c, namespace: namespace}, nil
}

// SetupRequirements returns the credential requirements for the toolspec's
// underlying toolkit, narrowed to the env vars used by the allowed
// subcommands. When AllowSubcommands is empty the full sensitive-env set is
// returned (safe default — never under-report a credential need).
func (Kind) SetupRequirements(ctx context.Context, tgt authkind.Target) []authkind.CredentialRequirement {
	tt, ok := tgt.(tsTarget)
	if !ok {
		return nil
	}
	cliKind := cli.New()
	toolkitName := tt.ts.Spec.Toolkit.Name
	if toolkitName == "" {
		return nil
	}
	tkTgt, err := cliKind.ResolveTarget(ctx, tt.client, tt.namespace, toolkitName)
	if err != nil {
		// SetupRequirements has no error return, so a failed toolkit lookup is
		// indistinguishable from "no requirements" to the caller. Log it, so an
		// operator can trace a "missing env" symptom back to the lookup error.
		besteffort.Log(log.FromContext(ctx).Info, "toolspec.SetupRequirements: ResolveTarget toolkit",
			err, "namespace", tt.namespace, "toolkit", toolkitName)
		return nil
	}
	all := cliKind.SetupRequirements(ctx, tkTgt)
	required := requiredEnvKeys(tt.ts, tkTgt)
	if len(required) == 0 {
		// Safe default — return the full sensitive-env set rather than miss
		// a real credential.
		return all
	}
	out := make([]authkind.CredentialRequirement, 0, len(all))
	for _, r := range all {
		for env := range r.BindingEnv {
			if required[env] {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// tsTarget wraps the resolved SpiceboxToolspec CR.
type tsTarget struct {
	ts        *spiceboxv1alpha1.SpiceboxToolspec
	client    client.Client
	namespace string
}

func (t tsTarget) Name() string               { return t.ts.Name }
func (t tsTarget) BindingMatchString() string { return "toolspec:" + t.ts.Name }
func (t tsTarget) Intent() string             { return t.ts.Spec.Intent }

// requiredEnvKeys returns the union of creds.required across subcommands whose
// path (joined with "/") appears in the toolspec's AllowSubcommands list. nil
// when AllowSubcommands is empty, which sends the caller to the full
// sensitive-env set.
func requiredEnvKeys(ts *spiceboxv1alpha1.SpiceboxToolspec, tkTgt authkind.Target) map[string]bool {
	cT, ok := tkTgt.(interface{ Toolkit() *toolkit.Toolkit })
	if !ok {
		return nil
	}
	// Each AllowSubcommands entry is a slash-joined subcommand path ("pr/list",
	// "" for root). An empty slice means "not narrowed".
	if len(ts.Spec.AllowSubcommands) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(ts.Spec.AllowSubcommands))
	for _, s := range ts.Spec.AllowSubcommands {
		allowed[s] = true
	}
	tk := cT.Toolkit()
	out := map[string]bool{}
	for _, sub := range tk.Subcommands {
		path := strings.Join(sub.Path, "/")
		if !allowed[path] {
			continue
		}
		for _, env := range sub.Effects.Creds.Required {
			out[env] = true
		}
	}
	return out
}
