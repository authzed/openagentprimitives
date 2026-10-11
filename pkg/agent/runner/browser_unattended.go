package runner

import (
	"log/slog"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// UnattendedBrowserSlotBindings derives the web_host slot grants an operator's
// BrowserSite.Unattended declaration (C8) authorizes UNCONDITIONALLY, with no
// human card: for every resolved BrowserToolbox, every EXPLICIT site
// (Spec.Access.Sites -- identity-catalog hosts are never unattended, so
// EffectiveSites is deliberately not read here) that sets Unattended under
// allowlist mode contributes one binding on its canonical host.
//
// A blocklist toolbox -- or one with no bound identity -- is excluded even if a
// site somehow set Unattended under it (defense in depth -- the CRD's
// field-scoped XValidation rule on BrowserToolbox.Spec already refuses BOTH
// combinations at admission, and the CLI's ValidateFile mirrors it, so reaching
// this function should never happen; these are the fail-closed backstops if it
// ever does). The identity backstop is symmetric with the mode one on purpose:
// an unattended grant signs in as the bound identity's login, so without an
// identity there is nothing to sign in as. A host that does
// not canonicalize is skipped and logged rather than aborting every other
// binding in the batch -- the same "drop and say why" shape
// BindClassDefaults's own default-derivation failures use.
//
// The caller (internal/cmd/runner's session cold start) writes the result
// through engine.Engine.GrantUnattendedSlots -- see that method's doc for why
// this must NOT go through BindClassDefaults.
//
// transforms is the per-type value-transform chain the AgentClass published on
// status.resolvedSlots (runner.SlotTransformsOf) -- the SAME source
// BindClassDefaults and the plan-gate approval bind derive their object ids
// from, so this cold-start grant cannot mint a different id than the acting
// tool's Check or an approved card would. For web_host that chain is
// spicedb_escape: a dotted host is not a legal SpiceDB object id, so writing the
// grant on the raw host would fail WriteRelationships (or, if it somehow landed,
// never match the escaped id the Check resolves). This is the ONE grant-write
// site that bypasses the tool-check derivation, so it must apply the same
// transform explicitly.
func UnattendedBrowserSlotBindings(toolboxes []spiceboxv1alpha1.ResolvedBrowserToolbox, transforms map[string][]string) []authz.SlotBinding {
	var out []authz.SlotBinding
	for _, rt := range toolboxes {
		if rt.Spec.Access.Mode != browserpolicy.ModeAllowlist {
			for _, site := range rt.Spec.Access.Sites {
				if site.Unattended {
					slog.Info("browser toolbox: unattended site under a non-allowlist mode; refusing the implicit grant",
						"browser", rt.Name, "host", site.Host, "mode", rt.Spec.Access.Mode)
				}
			}
			continue
		}
		if rt.Spec.Access.Identity == "" {
			for _, site := range rt.Spec.Access.Sites {
				if site.Unattended {
					slog.Info("browser toolbox: unattended site without a bound identity; refusing the implicit grant",
						"browser", rt.Name, "host", site.Host)
				}
			}
			continue
		}
		for _, site := range rt.Spec.Access.Sites {
			if !site.Unattended {
				continue
			}
			host, err := browserpolicy.CanonicalHost(site.Host)
			if err != nil {
				slog.Info("browser toolbox: unattended site's host could not be canonicalized; skipped",
					"browser", rt.Name, "host", site.Host, "err", err.Error())
				continue
			}
			// Escape the host into a legal SpiceDB object id via the class's
			// published web_host chain (spicedb_escape) -- dots are illegal in a
			// SpiceDB object id, and this is the same chain the acting tool's
			// Check and an approved plan-gate card derive their id from, so a
			// grant written here matches the id a later acting Check resolves.
			id, err := authz.NewObjectID(host, transforms[browserschema.WebHostType])
			if err != nil {
				slog.Info("browser toolbox: unattended site's host could not become an object id; skipped",
					"browser", rt.Name, "host", host, "err", err.Error())
				continue
			}
			out = append(out, authz.SlotBinding{
				ResourceType: browserschema.WebHostType,
				ResourceID:   id,
				Permission:   browserschema.ActPermission,
			})
		}
	}
	return out
}

// BrowserHostIdentityLabels maps every resolved browser toolbox's known host
// (EffectiveSites -- explicit sites plus any identity-derived ones) to the
// bound identity's login whose Hosts pattern matches it, using the SAME
// suffix-by-label/exact matching browserpolicy.Match applies to egress. A
// host matched by more than one login keeps the first match; a host matched
// by none is simply absent, and the card then names the host with no
// signing-in clause (see plangate.CardInput.WebHostIdentityLabels).
//
// Feeds Loop.PlanGateWebHostIdentityLabels, which is display-only and never
// an input to any authorization decision: naming the WRONG identity on a
// card would be a readability bug, not a security one, because the grant
// itself (GrantUnattendedSlots / an approved plan-gate card) never reads this
// map.
func BrowserHostIdentityLabels(toolboxes []spiceboxv1alpha1.ResolvedBrowserToolbox) map[string]string {
	out := map[string]string{}
	for _, rt := range toolboxes {
		for _, site := range rt.EffectiveSites {
			if _, ok := out[site.Host]; ok {
				continue
			}
			if label, ok := matchingLoginLabel(rt.Catalog, site.Host); ok {
				out[site.Host] = label
			}
		}
	}
	return out
}

// matchingLoginLabel returns the Label of the first catalog entry whose Hosts
// matches host, using browserpolicy's own equality-or-suffix-by-label rule so
// this never disagrees with what the egress gate itself considers a match.
func matchingLoginLabel(catalog []spiceboxv1alpha1.BrowserLoginRef, host string) (string, bool) {
	for _, login := range catalog {
		if login.Label == "" {
			continue
		}
		for _, h := range login.Hosts {
			if browserpolicy.Match(browserpolicy.Site{Host: h.Host, Exact: h.Exact}, host) {
				return login.Label, true
			}
		}
	}
	return "", false
}
