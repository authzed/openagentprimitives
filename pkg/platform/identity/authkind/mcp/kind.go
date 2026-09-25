// Package mcp is the authkind impl for the "mcp" prefix. Targets are
// MCPServer CRs. SetupRequirements emits one IsBearer requirement using
// the server's auth.provider and auth.credential.
package mcp

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
)

// Kind is the authkind impl for "mcp".
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

func (Kind) Prefix() string { return "mcp" }

// ResolveTarget looks up an MCPServer CR by name in the given namespace.
// MCPServer is namespace-scoped, so namespace is used in the lookup.
func (Kind) ResolveTarget(ctx context.Context, c client.Client, namespace, suffix string) (authkind.Target, error) {
	if c == nil {
		return nil, fmt.Errorf("mcp: ResolveTarget needs a cluster client")
	}
	var srv spiceboxv1alpha1.MCPServer
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: suffix}, &srv); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("mcp: %q in %q: %w", suffix, namespace, authkind.ErrTargetNotFound)
		}
		return nil, fmt.Errorf("mcp: get %q: %w", suffix, err)
	}
	return mcpTarget{srv: srv.DeepCopy()}, nil
}

// SetupRequirements returns one IsBearer requirement using the MCPServer's
// auth.provider, which drives the setup engine's credential-acquisition flow. Its
// Inject carries the explicit header projection (name + value prefix) the
// credential is applied with at runtime; pkg/agent/runner/descriptor.go derives
// the same thing on the runtime path.
//
// Returns nil for an unauthenticated server (CredentialNameForServer == ""): an
// empty credential name is meaningless and there is nothing to set up.
func (Kind) SetupRequirements(_ context.Context, tgt authkind.Target) []authkind.CredentialRequirement {
	t, ok := tgt.(mcpTarget)
	if !ok {
		return nil
	}
	name := passthroughcatalog.CredentialNameForServer(t.srv)
	if name == "" {
		return nil
	}
	header := t.srv.Spec.Auth.Header
	if header == "" {
		header = "Authorization"
	}
	prefix := t.srv.Spec.Auth.ValuePrefix
	if prefix == "" && t.srv.Spec.Auth.Header == "" {
		prefix = "Bearer "
	}
	return []authkind.CredentialRequirement{{
		SuggestedName: name,
		ProviderID:    t.srv.Spec.Auth.Provider,
		IsBearer:      true,
		Inject:        authkind.Injection{Header: &authkind.HeaderInjection{Name: header, ValuePrefix: prefix}},
	}}
}

type mcpTarget struct{ srv *spiceboxv1alpha1.MCPServer }

func (t mcpTarget) Name() string               { return t.srv.Name }
func (t mcpTarget) BindingMatchString() string { return "mcp:" + t.srv.Name }
func (t mcpTarget) Intent() string             { return t.srv.Spec.Intent }

// MCPServer exposes the underlying CR so the oauth-mcp builtin can read
// server.URL. Returns a pointer the caller must not mutate.
func (t mcpTarget) MCPServer() *spiceboxv1alpha1.MCPServer { return t.srv }

// NewTarget wraps an existing MCPServer CR into a Target, for callers that
// already have the CR in scope and want to skip the lookup hop. The pointer must
// not be mutated by the caller.
func NewTarget(srv *spiceboxv1alpha1.MCPServer) authkind.Target {
	return mcpTarget{srv: srv}
}
