package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// webHostTransforms is the per-type chain the AgentClass publishes for web_host
// (SlotTransformsOf), passed into UnattendedBrowserSlotBindings exactly as the
// runner cold start passes runner.SlotTransformsOf(&class). It escapes a dotted
// host into a legal SpiceDB object id.
var webHostTransforms = map[string][]string{browserschema.WebHostType: browserschema.WebHostValueTransforms()}

// TestUnattendedBrowserSlotBindings covers C8's required cases: an unattended
// allowlist site (with a bound identity) binds, an attended (Unattended=false)
// site does not, a blocklist toolbox refuses an unattended site even if one
// somehow sets the flag, and -- symmetric with the blocklist backstop -- an
// allowlist toolbox with NO bound identity refuses it too. The CRD's own
// XValidation rule should already prevent both bad combinations from existing;
// these are the defensive fail-closed backstops.
func TestUnattendedBrowserSlotBindings(t *testing.T) {
	cases := []struct {
		name      string
		toolboxes []spiceboxv1alpha1.ResolvedBrowserToolbox
		wantHosts []string
		wantEmpty bool
	}{
		{
			name: "unattended host under allowlist with identity binds (escaped id)",
			toolboxes: []spiceboxv1alpha1.ResolvedBrowserToolbox{{
				Name: "browser",
				Spec: spiceboxv1alpha1.BrowserToolboxSpec{
					Access: spiceboxv1alpha1.BrowserAccess{
						Mode:     "allowlist",
						Identity: "web-creds",
						Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "aa.com", Unattended: true}},
					},
				},
			}},
			// The grant is written on the ESCAPED host (dots are illegal in a
			// SpiceDB object id), not the raw "aa.com".
			wantHosts: []string{"aa=2Ecom"},
		},
		{
			name: "attended (unattended=false) host does not bind",
			toolboxes: []spiceboxv1alpha1.ResolvedBrowserToolbox{{
				Name: "browser",
				Spec: spiceboxv1alpha1.BrowserToolboxSpec{
					Access: spiceboxv1alpha1.BrowserAccess{
						Mode:     "allowlist",
						Identity: "web-creds",
						Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "bb.com"}},
					},
				},
			}},
			wantEmpty: true,
		},
		{
			name: "unattended host under blocklist is refused (defensive)",
			toolboxes: []spiceboxv1alpha1.ResolvedBrowserToolbox{{
				Name: "browser",
				Spec: spiceboxv1alpha1.BrowserToolboxSpec{
					Access: spiceboxv1alpha1.BrowserAccess{
						Mode:     "blocklist",
						Identity: "web-creds",
						Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "aa.com", Unattended: true}},
					},
				},
			}},
			wantEmpty: true,
		},
		{
			name: "unattended host under allowlist WITHOUT identity is refused (defensive)",
			toolboxes: []spiceboxv1alpha1.ResolvedBrowserToolbox{{
				Name: "browser",
				Spec: spiceboxv1alpha1.BrowserToolboxSpec{
					Access: spiceboxv1alpha1.BrowserAccess{
						Mode:  "allowlist",
						Sites: []spiceboxv1alpha1.BrowserSite{{Host: "aa.com", Unattended: true}},
					},
				},
			}},
			wantEmpty: true,
		},
		{
			name: "mixed: one unattended, one attended, in one toolbox",
			toolboxes: []spiceboxv1alpha1.ResolvedBrowserToolbox{{
				Name: "browser",
				Spec: spiceboxv1alpha1.BrowserToolboxSpec{
					Access: spiceboxv1alpha1.BrowserAccess{
						Mode:     "allowlist",
						Identity: "web-creds",
						Sites: []spiceboxv1alpha1.BrowserSite{
							{Host: "aa.com", Unattended: true},
							{Host: "bb.com"},
						},
					},
				},
			}},
			wantHosts: []string{"aa=2Ecom"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnattendedBrowserSlotBindings(tc.toolboxes, webHostTransforms)
			if tc.wantEmpty {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, len(tc.wantHosts))
			for i, h := range tc.wantHosts {
				assert.Equal(t, "web_host", got[i].ResourceType)
				assert.Equal(t, "act", got[i].Permission)
				assert.Equal(t, h, got[i].ResourceID.String())
			}
		})
	}
}

// TestUnattendedBrowserSlotBindings_IgnoresEffectiveSites proves Unattended is
// read only from the EXPLICIT spec sites, never from identity-derived
// EffectiveSites entries -- an identity-catalog host is never unattended, so
// even an EffectiveSites entry that happens to carry the same host as an
// unattended explicit site must not cause a double count, and an
// EffectiveSites-only host (Source "identity", no matching explicit site)
// must never bind.
func TestUnattendedBrowserSlotBindings_IgnoresEffectiveSites(t *testing.T) {
	toolboxes := []spiceboxv1alpha1.ResolvedBrowserToolbox{{
		Name: "browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode:     "allowlist",
				Identity: "web-creds",
				Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "aa.com", Unattended: true}},
			},
		},
		EffectiveSites: []spiceboxv1alpha1.BrowserEffectiveSite{
			{Host: "aa.com", Source: "explicit"},
			{Host: "identity-only.com", Source: "identity"},
		},
	}}
	got := UnattendedBrowserSlotBindings(toolboxes, webHostTransforms)
	require.Len(t, got, 1)
	assert.Equal(t, "aa=2Ecom", got[0].ResourceID.String(), "the grant is written on the escaped host")
}

// TestBrowserHostIdentityLabels covers the card-display matching: an
// EffectiveSite host whose host matches a catalog login's Hosts pattern
// (suffix-by-label or exact) resolves to that login's Label; a host with no
// match is simply absent.
func TestBrowserHostIdentityLabels(t *testing.T) {
	toolboxes := []spiceboxv1alpha1.ResolvedBrowserToolbox{{
		Name: "browser",
		EffectiveSites: []spiceboxv1alpha1.BrowserEffectiveSite{
			{Host: "aa.com"},
			{Host: "sub.bb.com"},
			{Host: "cc.com"},
		},
		Catalog: []spiceboxv1alpha1.BrowserLoginRef{
			{ID: "l1", Label: "Acme SSO", Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "aa.com"}}},
			{ID: "l2", Label: "Beta Portal", Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "bb.com"}}},
		},
	}}
	got := BrowserHostIdentityLabels(toolboxes)
	assert.Equal(t, "Acme SSO", got["aa.com"])
	assert.Equal(t, "Beta Portal", got["sub.bb.com"], "non-exact login host matches a subdomain, mirroring browserpolicy.Match")
	_, ok := got["cc.com"]
	assert.False(t, ok, "a host with no matching login must be absent, not a guessed label")
}
