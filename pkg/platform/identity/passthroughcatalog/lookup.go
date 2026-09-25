// Package passthroughcatalog provides MCPServer-by-credential-name lookup
// helpers shared between identityd (which routes credential link clicks to the
// OAuth or PAT flow based on the source MCPServer's Spec.Auth) and channelsd's
// credential_request watcher (which partitions the missing credentials into
// per-credential buttons by the same lookup).
//
// It lives outside identityd so the channelsd pipeline can call it without
// taking identityd's HTTP handlers as a transitive dependency.
package passthroughcatalog

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

// AuthTypeOAuth is the value of MCPServer.Spec.Auth.Type that means the
// credential is obtained via the OAuth Authorization Code flow
// (/link/oauth/<credname>).
const AuthTypeOAuth = "oauth"

// AuthTypeStatic is the value of MCPServer.Spec.Auth.Type that means the
// credential is a long-lived user-supplied token. Empty is equivalent: test
// against AuthTypeOAuth and treat everything else as PAT.
const AuthTypeStatic = "static"

// LookupMCPServerByCredential returns the (single) MCPServer whose auth
// references the given credential name, searching cluster-wide.
//
// The name comes from CredentialNameForServer — spec.auth.credential verbatim,
// with no metadata.name fallback.
//
// Returns an error if:
//   - zero MCPServers match — credName is not registered anywhere.
//   - multiple MCPServers match — operator misconfiguration; two servers
//     should not declare the same credential name without an explicit
//     credentialRemap.
//
// Cluster-wide is the right scope for the per-user /link/oauth/<cred> flow,
// where every credential name in the cluster is a candidate for the signed-in
// visitor's OWN account. It is the WRONG scope for a caller that must stay
// inside one namespace — see LookupMCPServerByCredentialInNamespace.
func LookupMCPServerByCredential(ctx context.Context, c client.Client, credName string) (*spiceboxv1alpha1.MCPServer, error) {
	return lookupMCPServerByCredential(ctx, c, credName, "")
}

// LookupMCPServerByCredentialInNamespace is LookupMCPServerByCredential
// scoped to a single namespace ns via client.InNamespace, so a credential name
// that also happens to match an MCPServer in a DIFFERENT namespace can never
// be chosen.
//
// This is the required lookup for the agent-owned workshop-credential OAuth
// flow: the credential name comes from a builder-authored AgentIdentity in
// workshop namespace W, and W is untrusted input the same way the builder's
// prompt is. A cluster-wide lookup would let a credential name that
// COINCIDENTALLY matches a production MCPServer (in some other namespace)
// redirect the workshop-starter's OAuth authorize step at the production
// provider, and land that provider's token in W via the agent-credential
// write path. Scoping the List to W makes that collision impossible, and
// zero matches in W fails closed — the builder must have actually declared
// the MCPServer inside the workshop namespace.
func LookupMCPServerByCredentialInNamespace(ctx context.Context, c client.Client, ns, credName string) (*spiceboxv1alpha1.MCPServer, error) {
	if ns == "" {
		return nil, fmt.Errorf("LookupMCPServerByCredentialInNamespace: namespace is required")
	}
	return lookupMCPServerByCredential(ctx, c, credName, ns)
}

// lookupMCPServerByCredential is the shared List + match + exactly-one-or-fail
// implementation behind both exported lookups above. ns == "" lists
// cluster-wide; a non-empty ns scopes the List to it.
func lookupMCPServerByCredential(ctx context.Context, c client.Client, credName, ns string) (*spiceboxv1alpha1.MCPServer, error) {
	var opts []client.ListOption
	scope := "cluster-wide"
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
		scope = fmt.Sprintf("namespace %q", ns)
	}
	var list spiceboxv1alpha1.MCPServerList
	if err := c.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list MCPServers (%s): %w", scope, err)
	}
	var matches []*spiceboxv1alpha1.MCPServer
	for i := range list.Items {
		m := &list.Items[i]
		if CredentialNameForServer(m) == credName {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no MCPServer found for credential %q (%s)", credName, scope)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("multiple MCPServers (%d) found for credential %q (%s); operator misconfiguration", len(matches), credName, scope)
	}
}

// CredentialAuthType returns the canonical auth-type string for a
// MCPServer's credential. The result is one of:
//
//   - AuthTypeOAuth ("oauth")  — credential is obtained via OAuth flow
//   - AuthTypeStatic ("static") — credential is a long-lived PAT/API key
//
// An empty Spec.Auth.Type normalizes to AuthTypeStatic, so an MCPServer that
// declares no type stays on the PAT form. A nil receiver returns AuthTypeStatic
// too — the defensive default is the non-OAuth one.
func CredentialAuthType(srv *spiceboxv1alpha1.MCPServer) string {
	if srv == nil {
		return AuthTypeStatic
	}
	if srv.Spec.Auth.Type == AuthTypeOAuth {
		return AuthTypeOAuth
	}
	return AuthTypeStatic
}

// CredentialNameForServer returns the credential-catalog name an MCPServer's
// auth is keyed under: spec.auth.credential, VERBATIM.
//
// There is NO metadata.name fallback — names must be declared explicitly. A
// server with spec.auth.provider set but spec.auth.credential empty is rejected
// at admission (Valid=False, AuthCredentialMissing), so downstream of that a ""
// return means the MCPServer is genuinely unauthenticated.
//
// The runner's descriptor and setup-identity both call this, so they always
// agree on the credential name.
func CredentialNameForServer(srv *spiceboxv1alpha1.MCPServer) string {
	if srv == nil {
		return ""
	}
	return srv.Spec.Auth.Credential
}

// ProviderIDFromToolkits resolves a credential name to its provider id by
// walking the embedded toolkit catalog's sensitive env vars. The credential
// name is the env var's `credential` field, falling back to its `name`
// verbatim — the same derivation passthrough.Required uses. Returns "" when
// no toolkit declares the credential. No cluster I/O.
func ProviderIDFromToolkits(credName string) string {
	for _, tk := range embeddedtoolkits.All() {
		for _, e := range tk.Env.Allowed {
			if !e.Sensitive {
				continue
			}
			name := e.Credential
			if name == "" {
				name = e.Name
			}
			if name == credName {
				return e.Provider
			}
		}
	}
	return ""
}

// ProviderForCredential resolves the provider-catalog entry backing a
// credential name, so a caller can validate a pasted/typed token against the
// provider's declared format (provider.ValidateToken) BEFORE storing it.
// Resolution order:
//
//  1. the embedded toolkit catalog (a toolkit env var's provider: field), then
//  2. the cluster's MCPServers (spec.auth.provider).
//
// Returns (nil, false) when the credential resolves to no provider id, or to
// an id that isn't a known builtin provider. Callers MUST treat that as "no
// declared format" and stay permissive — never block a credential whose
// backing we can't resolve. A List error during the MCPServer lookup is
// likewise treated as "not found": validation must never be the step that
// fails a credential because the cluster read hiccuped.
func ProviderForCredential(ctx context.Context, c client.Client, credName string) (*provider.Provider, bool) {
	if id := ProviderIDFromToolkits(credName); id != "" {
		if p, ok := provider.ByID(id); ok {
			return p, true
		}
	}
	if c != nil {
		if srv, err := LookupMCPServerByCredential(ctx, c, credName); err == nil && srv != nil {
			if id := srv.Spec.Auth.Provider; id != "" {
				if p, ok := provider.ByID(id); ok {
					return p, true
				}
			}
		}
	}
	return nil, false
}

// ProviderLabel returns the user-facing display name for the MCPServer's
// provider — used as the button label suffix ("Connect <label>") in the
// credential_request Slack render.
//
// Resolution order:
//  1. Spec.Auth.Provider — the canonical provider key (e.g. "linear").
//  2. metadata.name — fallback when Provider is empty.
//  3. "" — only when srv is nil (caller decides how to label).
func ProviderLabel(srv *spiceboxv1alpha1.MCPServer) string {
	if srv == nil {
		return ""
	}
	if srv.Spec.Auth.Provider != "" {
		return srv.Spec.Auth.Provider
	}
	return srv.Name
}

// LinkedService is one resolved credential: the machine-form credential
// name the caller asked about, and the user-facing provider label it
// resolved to.
//
// The pair is the unit because resolution is LOSSY in two ways a bare label list
// hides: a credential with no backing MCPServer or an empty ProviderLabel is
// dropped, and two credentials sharing a label collapse to one. So an input list
// and a returned label list have different lengths in a different order —
// indexing them as co-indexed panicked channelsd (no recovery on that path) for
// every user who opened the App Home tab, on a userPassthrough AgentClass whose
// credentials came from a ToolBundle and so had no MCPServer at all.
//
// Anything needing a label and its credential together must carry LinkedService,
// never two slices and a comment asserting they line up.
type LinkedService struct {
	// CredentialName is the credential-catalog name, as supplied by the
	// caller. It is NOT display text: it composes URLs (identityd's
	// /icon/<credName>) and keys credential lookups.
	CredentialName string

	// Label is the user-facing provider label — display text, and the only
	// half of the pair a renderer may put in front of a user.
	Label string
}

// ResolveLinkedServices intersects the supplied credential names (typically a
// UserIdentity's linked-credential list) with the cluster's MCPServers and
// returns the surviving (credential, label) pairs. It is what shows a
// userPassthrough approver EXACTLY which services they are about to share by
// proxy.
//
// Filtering rules:
//   - No backing MCPServer → skipped; never surface a credential the joining
//     agent cannot actually use.
//   - classCredentials, when non-nil, narrows further to what the joining
//     session's AgentClass references. nil skips that filter.
//   - Deduped BY LABEL (first credential to claim a label keeps it) and sorted
//     by label for stable rendering across reconciles. An empty result is
//     meaningful — nothing linked that this session would use — and the caller's
//     warning falls back to generic phrasing.
//
// A List error returns nil plus the wrapped error; callers treat it as "nothing
// resolved" (degraded UX), not fatal.
func ResolveLinkedServices(
	ctx context.Context,
	c client.Client,
	credentialNames []string,
	classCredentials map[string]struct{},
) ([]LinkedService, error) {
	if len(credentialNames) == 0 {
		return nil, nil
	}
	var list spiceboxv1alpha1.MCPServerList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list MCPServers: %w", err)
	}
	// Index MCPServers by the credential name they serve. Same name-
	// resolution rules as LookupMCPServerByCredential — both via
	// CredentialNameForServer.
	byCred := make(map[string]*spiceboxv1alpha1.MCPServer, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]
		name := CredentialNameForServer(m)
		if _, taken := byCred[name]; !taken {
			byCred[name] = m
		}
	}
	seen := map[string]struct{}{}
	var out []LinkedService
	for _, credName := range credentialNames {
		if classCredentials != nil {
			if _, used := classCredentials[credName]; !used {
				continue
			}
		}
		srv := byCred[credName]
		if srv == nil {
			continue
		}
		label := ProviderLabel(srv)
		if label == "" {
			continue
		}
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, LinkedService{CredentialName: credName, Label: label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// ResolveLinkedServiceLabels is ResolveLinkedServices projected to its
// labels, for the callers that render a list of service names and nothing
// else. It is a projection rather than a second implementation so the
// filtering, dedup and ordering rules have exactly one definition.
func ResolveLinkedServiceLabels(
	ctx context.Context,
	c client.Client,
	credentialNames []string,
	classCredentials map[string]struct{},
) ([]string, error) {
	services, err := ResolveLinkedServices(ctx, c, credentialNames, classCredentials)
	if err != nil {
		return nil, err
	}
	return LinkedServiceLabels(services), nil
}

// LinkedServiceLabels projects the label half of a resolved pair list,
// preserving order. Returns nil for an empty input so an "unresolved"
// result stays distinguishable from an empty-but-allocated one.
func LinkedServiceLabels(services []LinkedService) []string {
	if len(services) == 0 {
		return nil
	}
	labels := make([]string, len(services))
	for i, s := range services {
		labels[i] = s.Label
	}
	return labels
}
