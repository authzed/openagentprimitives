// Package passthrough is the canonical resolver for "which credentials
// does this AgentClass require under identityMode=userPassthrough?"
//
// Single source of truth. Every consumer routes through Required: the
// agentsession reconciler (parks sessions with a missing-credential list), the
// identityd portal (Suggested section + deep-link menu), the Slack Home tab
// (linked services view), and the channelsd DM publisher (transitively, via the
// pre-computed SUI.Status.MissingCredentials).
//
// It lives here, below both the controllers and identityd, so neither has to
// keep its own copy — a partial duplicate that walks only MCPServer refs
// silently drops every toolkit-backed credential.
package passthrough

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

// FederatedTarget is one federated MCPServer the session needs a minted
// credential for. It carries the parameters required to synthesize a
// type=federated AgentCredential at session-projection time — no user-link
// is involved.
type FederatedTarget struct {
	// CredentialName is the catalog name the MCPServer is known by.
	CredentialName string
	// Resource is MCPServer.spec.auth.resource — the ID-JAG audience.
	Resource string
	// ResourceServerURL is MCPServer.spec.server.url — used by the broker
	// for leg-2 token discovery.
	ResourceServerURL string
}

// RequiredFederated walks ac.Spec.MCPServers, GETs each server, and returns
// a FederatedTarget for every server whose spec.auth.type == "federated".
// These targets are NOT included in Required's output — they are synthesized
// as credentials at projection time rather than requiring a user link.
//
// Errors propagate GET failures so callers can requeue.
func RequiredFederated(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) ([]FederatedTarget, error) {
	var out []FederatedTarget
	for _, ref := range ac.Spec.MCPServers {
		var srv spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: ac.Namespace, Name: ref.Ref}, &srv); err != nil {
			return nil, fmt.Errorf("get MCPServer %q: %w", ref.Ref, err)
		}
		if srv.Spec.Auth.Type != "federated" {
			continue
		}
		name := passthroughcatalog.CredentialNameForServer(&srv)
		if mapped, ok := ref.CredentialRemap[name]; ok {
			name = mapped
		}
		out = append(out, FederatedTarget{
			CredentialName:    name,
			Resource:          srv.Spec.Auth.Resource,
			ResourceServerURL: srv.Spec.Server.URL,
		})
	}
	return out, nil
}

// CredRequirement is one credential an AgentClass requires under
// identityMode=userPassthrough, with display metadata resolved by
// precedence: tool field → provider catalog (via ProviderID) → humanized
// credential name (Title) / empty (Description).
type CredRequirement struct {
	Name        string
	Title       string
	Description string
	ProviderID  string
}

// contribution is one tool's claim on a final credential name, carrying
// whatever display metadata that tool declared.
type contribution struct {
	rawName     string // pre-remap declared name
	exact       bool   // declared name already equals the final name (no remap)
	providerID  string
	title       string
	description string
}

// Unauthenticated reports whether an MCPServer's auth stanza names no
// credential a person could ever be asked to link: neither auth.credential
// nor auth.provider, on a server that is not federated. There is no
// metadata.name fallback (passthroughcatalog.CredentialNameForServer), so
// such a server contributes NO credential requirement — Resolve and
// ResolveBestEffort below skip exactly this shape — and every call it makes
// goes out with no auth header.
//
// federated is excluded because such a server's credential is synthesized at
// session-projection time (RequiredFederated), never linked by a person, so
// naming no credential there is correct rather than a mistake.
//
// Exported, and the single statement of the rule, because validate_spec
// (pkg/tools/workshopmcp) refuses this shape under a per-person AgentClass:
// that refusal is only correct while it names exactly the shape this resolver
// drops, so the two read one predicate rather than two copies of it.
func Unauthenticated(spec *spiceboxv1alpha1.MCPServerSpec) bool {
	if spec == nil {
		return false
	}
	return spec.Auth.Type != "federated" && spec.Auth.Credential == "" && spec.Auth.Provider == ""
}

// Resolve enumerates the credentials the AgentClass requires (same set as
// Required) with display metadata. Federated MCP targets are excluded (they
// never require a user link). Errors propagate GET / resolution failures so
// callers can requeue.
func Resolve(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) ([]CredRequirement, error) {
	contribs := map[string][]contribution{}

	for _, ref := range ac.Spec.MCPServers {
		var srv spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: ac.Namespace, Name: ref.Ref}, &srv); err != nil {
			return nil, fmt.Errorf("get MCPServer %q: %w", ref.Ref, err)
		}
		if srv.Spec.Auth.Type == "federated" {
			continue
		}
		if Unauthenticated(&srv.Spec) {
			continue
		}
		raw := passthroughcatalog.CredentialNameForServer(&srv)
		final := raw
		if mapped, ok := ref.CredentialRemap[raw]; ok {
			final = mapped
		}
		recordContribution(contribs, final, contribution{
			rawName:     raw,
			exact:       raw == final,
			providerID:  srv.Spec.Auth.Provider,
			title:       srv.Spec.Auth.Title,
			description: srv.Spec.Auth.Description,
		})
	}

	for _, b := range ac.Spec.ToolBundles {
		metas, err := toolkitCredMetaForBundle(ctx, c, b)
		if err != nil {
			return nil, fmt.Errorf("bundle %q: %w", b.Name, err)
		}
		for _, m := range metas {
			final := m.rawName
			if mapped, ok := b.CredentialRemap[m.rawName]; ok {
				final = mapped
			}
			m.exact = m.rawName == final
			recordContribution(contribs, final, m)
		}
	}

	return finalizeRequirements(contribs), nil
}

// ResolveBestEffort is Resolve's read-only counterpart: per-ref failures
// are logged + skipped; never returns an error.
func ResolveBestEffort(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass, logger logr.Logger) []CredRequirement {
	contribs := map[string][]contribution{}

	for _, ref := range ac.Spec.MCPServers {
		var srv spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: ac.Namespace, Name: ref.Ref}, &srv); err != nil {
			logger.Info("passthrough: skipping dangling MCPServer ref",
				"agentclass", ac.Name, "namespace", ac.Namespace, "ref", ref.Ref, "err", err.Error())
			continue
		}
		if srv.Spec.Auth.Type == "federated" {
			continue
		}
		if Unauthenticated(&srv.Spec) {
			continue
		}
		raw := passthroughcatalog.CredentialNameForServer(&srv)
		final := raw
		if mapped, ok := ref.CredentialRemap[raw]; ok {
			final = mapped
		}
		recordContribution(contribs, final, contribution{
			rawName:     raw,
			exact:       raw == final,
			providerID:  srv.Spec.Auth.Provider,
			title:       srv.Spec.Auth.Title,
			description: srv.Spec.Auth.Description,
		})
	}

	for _, b := range ac.Spec.ToolBundles {
		metas, err := toolkitCredMetaForBundle(ctx, c, b)
		if err != nil {
			logger.Info("passthrough: skipping bundle with unresolvable toolkit ref",
				"agentclass", ac.Name, "namespace", ac.Namespace, "bundle", b.Name, "err", err.Error())
			continue
		}
		for _, m := range metas {
			final := m.rawName
			if mapped, ok := b.CredentialRemap[m.rawName]; ok {
				final = mapped
			}
			m.exact = m.rawName == final
			recordContribution(contribs, final, m)
		}
	}

	return finalizeRequirements(contribs)
}

func recordContribution(m map[string][]contribution, final string, c contribution) {
	if final == "" {
		return
	}
	m[final] = append(m[final], c)
}

// finalizeRequirements resolves each final credential's display metadata
// from its contributions (exact-match contribution preferred, then stable
// by rawName) and returns a sorted requirement list.
func finalizeRequirements(contribs map[string][]contribution) []CredRequirement {
	out := make([]CredRequirement, 0, len(contribs))
	for final, cs := range contribs {
		primary := pickPrimary(cs)
		out = append(out, CredRequirement{
			Name:        final,
			Title:       resolveTitle(final, primary),
			Description: resolveDescription(primary),
			ProviderID:  primary.providerID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pickPrimary(cs []contribution) contribution {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].exact != cs[j].exact {
			return cs[i].exact // exact-match first
		}
		return cs[i].rawName < cs[j].rawName
	})
	return cs[0]
}

func resolveTitle(final string, c contribution) string {
	if c.title != "" {
		return c.title
	}
	if c.providerID != "" {
		if p, ok := provider.ByID(c.providerID); ok && p.Title != "" {
			return p.Title
		}
	}
	return HumanizeCredName(final)
}

func resolveDescription(c contribution) string {
	if c.description != "" {
		return c.description
	}
	if c.providerID != "" {
		if p, ok := provider.ByID(c.providerID); ok {
			return p.Description
		}
	}
	return ""
}

// HumanizeCredName turns a kebab/snake credential name into a title-cased
// label. Empty returns "your account".
func HumanizeCredName(name string) string {
	if name == "" {
		return "your account"
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func requirementNames(reqs []CredRequirement) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	return out // already sorted by Resolve / ResolveBestEffort
}

// Required enumerates the credential NAMES the AgentClass declares it
// needs:
//   - For each MCPServer in ac.Spec.MCPServers (resolved via cluster GET): the
//     credential is srv.Spec.Auth.Credential verbatim (there is no srv.Name
//     fallback — see passthroughcatalog.CredentialNameForServer). Servers with
//     neither Auth.Credential nor Auth.Provider are unauthenticated and skipped
//     — the shape Unauthenticated names, and the one validate_spec refuses
//     under a per-person class.
//     Servers with auth.type=="federated" are ALSO skipped — they contribute a
//     synthesized credential at session-projection time (see RequiredFederated),
//     not a user-linked credential name.
//   - For each ToolBundle in ac.Spec.ToolBundles: each referenced toolkit's
//     sensitive ToolkitEnvVar contributes its declared e.Credential (the env
//     Name verbatim as a last-resort fallback if Credential is empty).
//
// CredentialRemap on the ToolBundle / AgentClassMCPServerRef is applied
// after the per-tool default is computed. The output is sorted +
// deduplicated.
//
// Errors propagate cluster GET / resolution failures so callers can
// requeue (operator) or fall back (read-only views).
func Required(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) ([]string, error) {
	reqs, err := Resolve(ctx, c, ac)
	if err != nil {
		return nil, err
	}
	return requirementNames(reqs), nil
}

// RequiredBestEffort is Required's read-only counterpart: per-ref
// resolution failures (dangling MCPServer ref, dangling toolkit ref)
// are logged via the supplied logger and SKIPPED — the function never
// returns an error and never drops credentials that DID resolve.
//
// Use this for read-only surfaces (identityd's Suggested aggregator,
// the Slack Home tab) where one broken AgentClass should not blank out
// every other listed credential. The operator parker still uses Required
// (strict) because parking a session needs an authoritative list.
func RequiredBestEffort(ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass, logger logr.Logger) []string {
	return requirementNames(ResolveBestEffort(ctx, c, ac, logger))
}

// toolkitCredMetaForBundle returns one contribution per sensitive env var
// reachable from the bundle's toolspecs, carrying the declared credential
// name + display metadata. The returned names are the RAW, pre-remap names.
// The caller (Resolve) applies the bundle's CredentialRemap so two raw names
// can collapse to one final name.
//
// Resolution chain:
//
//	ToolBundle.Toolspecs[]                  (SpiceboxToolspec names)
//	  → SpiceboxToolspec.spec.toolkit.name  (cluster-scoped lookup)
//	    → SpiceboxToolkit.spec.env.allowed[] where Sensitive=true
func toolkitCredMetaForBundle(ctx context.Context, c client.Client, b spiceboxv1alpha1.ToolBundle) ([]contribution, error) {
	seen := map[string]struct{}{}
	var out []contribution
	for _, tsName := range b.Toolspecs {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		if err := c.Get(ctx, client.ObjectKey{Name: tsName}, &ts); err != nil {
			return nil, fmt.Errorf("get SpiceboxToolspec %q: %w", tsName, err)
		}
		tkName := ts.Spec.Toolkit.Name
		if tkName == "" {
			continue
		}
		metas, err := toolkitCredMeta(ctx, c, tkName)
		if err != nil {
			return nil, fmt.Errorf("toolspec %q: %w", tsName, err)
		}
		for _, m := range metas {
			if _, ok := seen[m.rawName]; ok {
				continue
			}
			seen[m.rawName] = struct{}{}
			out = append(out, m)
		}
	}
	return out, nil
}

// toolkitCredMeta returns the display-metadata contributions declared by
// the named toolkit's sensitive env vars. Resolution order: prefer a
// SpiceboxToolkit CR, fall back to the embedded compile-time catalog.
func toolkitCredMeta(ctx context.Context, c client.Client, tkName string) ([]contribution, error) {
	var sbtk spiceboxv1alpha1.SpiceboxToolkit
	switch err := c.Get(ctx, client.ObjectKey{Name: tkName}, &sbtk); {
	case err == nil:
		return sensitiveEnvMeta(sbtk.Spec.Env.Allowed), nil
	case !errors.IsNotFound(err):
		return nil, fmt.Errorf("get SpiceboxToolkit %q: %w", tkName, err)
	}
	for _, tk := range embeddedtoolkits.All() {
		if tk.Name != tkName {
			continue
		}
		envs := make([]spiceboxv1alpha1.ToolkitEnvVar, 0, len(tk.Env.Allowed))
		for _, e := range tk.Env.Allowed {
			envs = append(envs, spiceboxv1alpha1.ToolkitEnvVar{
				Name:        e.Name,
				Title:       e.Title,
				Description: e.Description,
				Sensitive:   e.Sensitive,
				Provider:    e.Provider,
				Credential:  e.Credential,
			})
		}
		return sensitiveEnvMeta(envs), nil
	}
	return nil, fmt.Errorf("toolkit %q not found (no SpiceboxToolkit CR and not in embedded catalog)", tkName)
}

// sensitiveEnvMeta extracts one contribution per sensitive env var, carrying
// the canonical credential name + display metadata (title, description,
// providerID). The Credential field is used as the raw name; if empty the
// env Name is used verbatim (no inference/lowercasing) so the coverage check
// at least names the env rather than dropping it silently.
func sensitiveEnvMeta(envs []spiceboxv1alpha1.ToolkitEnvVar) []contribution {
	var out []contribution
	for _, e := range envs {
		if !e.Sensitive {
			continue
		}
		name := e.Credential
		if name == "" {
			name = e.Name
		}
		out = append(out, contribution{
			rawName:     name,
			providerID:  e.Provider,
			title:       e.Title,
			description: e.Description,
		})
	}
	return out
}
