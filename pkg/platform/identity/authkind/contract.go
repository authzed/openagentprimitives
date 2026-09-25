// Package authkind defines the per-prefix plug-in surface for AgentIdentity
// setup routing. Each prefix ("cli", "toolspec", "mcp", "toolbox") is handled by
// exactly one Kind impl registered with
// pkg/platform/identity/authkind/registry at init time.
//
// This package covers SETUP only. Resolving a credential at runtime is the token
// broker's job (pkg/platform/identity/broker).
package authkind

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// ErrTargetNotFound is returned by Kind.ResolveTarget when no entity by
// the given suffix exists. Callers translate this into a user-visible
// error during setup or a Failed=True condition at runtime.
var ErrTargetNotFound = errors.New("authkind: target not found")

// Kind is the per-prefix plug-in. One impl per binding-match prefix.
type Kind interface {
	// Prefix returns the binding-match prefix this kind handles, with no
	// trailing colon. E.g. "cli", "toolspec", "mcp".
	Prefix() string

	// ResolveTarget looks up the entity referenced by `<prefix>:<suffix>` in the
	// given namespace. Returns ErrTargetNotFound when no entity by that suffix
	// exists; other errors are transport / lookup errors.
	ResolveTarget(ctx context.Context, c client.Client, namespace, suffix string) (Target, error)

	// SetupRequirements lists the credentials needed to use this target. Setup
	// walks each requirement, picks the provider, runs the flow, and writes the
	// credential. A pure function of the target.
	SetupRequirements(ctx context.Context, target Target) []CredentialRequirement
}

// Target is the resolved entity behind a binding-match suffix.
type Target interface {
	// Name returns the resource name (e.g. "gh", "linear-readonly").
	Name() string
	// BindingMatchString returns "<prefix>:<name>".
	// Inverse of pkg/platform/identity/authkind/registry.ParseBindingMatch.
	BindingMatchString() string
	// Intent returns a human-readable intent string suitable for setup
	// system-prompt context. Empty if the target declares no intent.
	Intent() string
}

// CredentialRequirement describes one credential that must exist on an
// AgentIdentity for the target to be used. Setup walks each requirement
// independently and is idempotent per requirement.
type CredentialRequirement struct {
	// SuggestedName is the credential name proposed for the AgentIdentity (e.g.
	// "github-pat"). Setup uses it to detect "already set up" before re-running a
	// flow.
	SuggestedName string

	// ProviderID names a provider in the /providers/ library. When set, the setup
	// engine dispatches to the provider's builtin Go flow if one is registered,
	// falling back to the LLM-driven path with the provider's prompt. Exactly one
	// of ProviderID or InlinePrompt must be non-empty; pkg/platform/identity/setup
	// validates that.
	ProviderID string

	// InlinePrompt is the toolkit-supplied free text the LLM fallback uses when no
	// provider applies. Read only when ProviderID == "".
	InlinePrompt string

	// BindingEnv names the env vars this requirement binds to the resulting
	// credential; empty/nil for MCP kinds. Every value MUST be "" — the runtime
	// overwrites each entry with the resolved credential value, so a non-empty one
	// is a contract violation.
	//
	// Superseded by Inject as the injection descriptor, but still load-bearing:
	// the toolspec kind narrows a toolkit's requirements by intersecting these
	// keys with the allowed subcommands' required creds.
	BindingEnv map[string]string

	// IsBearer is true for MCP-style credentials whose access_token is applied as
	// the HTTP bearer, false for env-projected ones. Superseded by Inject as the
	// injection descriptor; still read by the setup agent's prompt builder to word
	// its store_credential guidance.
	IsBearer bool

	// Inject is the explicit, kind-neutral description of how the resolved
	// credential value is projected at runtime, and the successor to
	// BindingEnv/IsBearer. Each Kind populates it (cli/toolspec set EnvVar, mcp
	// sets Header) and the runtime resolver (credresolve.Descriptors) reads it
	// verbatim rather than re-deriving the injection shape per kind.
	Inject Injection
}

// Injection describes how the resolved credential value is projected at
// runtime: exactly one of EnvVar or Header is set. Each Kind populates it
// explicitly; the runtime resolver (credresolve.Descriptors) reads it verbatim.
type Injection struct {
	// EnvVar is the environment-variable name the credential value is projected
	// into. Set by env-projected kinds (cli, toolspec, sidecar).
	EnvVar string

	// Header, when non-nil, projects the credential value into an HTTP request
	// header. Set by header-projected kinds (mcp); mutually exclusive with EnvVar.
	Header *HeaderInjection
}

// HeaderInjection describes an HTTP-header projection of a credential value.
type HeaderInjection struct {
	// Name is the header name (e.g. "Authorization").
	Name string
	// ValuePrefix is prepended to the credential value (e.g. "Bearer ").
	ValuePrefix string
}

// ResolvedCredential is the credential value the credresolve layer produces for
// runtime injection.
type ResolvedCredential struct {
	// AccessToken is the credential value injected at runtime, wrapped so it
	// cannot be accidentally logged or serialized. Reach the bytes only at the
	// injection site, via AccessToken.UnderlyingValue().
	AccessToken sensitive.SensitiveValue
}
