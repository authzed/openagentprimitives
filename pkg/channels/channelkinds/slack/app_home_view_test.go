// pkg/channels/channelkinds/slack/app_home_view_test.go
//
// Tests for the App Home Block Kit view builder. The renderer is
// pure (no cluster access), so unit tests can drive the full
// rendering matrix directly.
package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
)

// linkedServices builds the (credential, label) pairs a card carries for the
// services the user has linked — the shape passthroughcatalog's resolver
// returns, one credential per surviving label.
func linkedServices(labels ...string) []passthroughcatalog.LinkedService {
	out := make([]passthroughcatalog.LinkedService, 0, len(labels))
	for _, l := range labels {
		out = append(out, passthroughcatalog.LinkedService{
			CredentialName: strings.ToLower(l) + "-token",
			Label:          l,
		})
	}
	return out
}

// TestBuildAppHomeView_EmptyState pins the no-agent placeholder: a nil Agent
// (the class couldn't be loaded) renders an explanatory page, not a blank tab,
// and carries the OAP branding rather than the old "agentprimitives" name.
func TestBuildAppHomeView_EmptyState(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{})
	require.Equal(t, "home", string(view.Type))
	blocks := view.Blocks.BlockSet
	require.GreaterOrEqual(t, len(blocks), 2, "header + explanatory section minimum")

	body := mustMarshal(t, blocks)
	assert.Contains(t, body, "isn't available right now",
		"empty state needs an explanatory message — a blank home is confusing")
	assert.Contains(t, body, "Open Agent Primitives",
		"the branding footer names the product: OAP / Open Agent Primitives")
	assert.NotContains(t, body, "agentprimitives hub",
		"no generic hub title on this surface")
}

// TestBuildAppHomeView_OperatorIdentityMode pins the operator-default
// rendering: identity badge says "operator account", no connections
// section, no Manage button.
func TestBuildAppHomeView_OperatorIdentityMode(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{
		Agent: &appHomeAgent{
			Name:         "triage",
			DisplayName:  "Triage Bot",
			Description:  "Routes bug reports to the right team",
			IdentityMode: "", // operator default
		},
	})
	body := mustMarshal(t, view.Blocks.BlockSet)
	assert.Contains(t, body, "Triage Bot", "DisplayName rendered as the hero name")
	assert.Contains(t, body, "Routes bug reports", "Description rendered")
	assert.Contains(t, body, "Uses an operator account",
		"operator-identity badge must appear")
	assert.NotContains(t, body, "Uses YOUR account",
		"don't show the passthrough badge for operator-mode agents")
	assert.NotContains(t, body, "Manage connections",
		"operator-mode agents don't surface the Manage button — there's nothing for the user to manage")
	assert.NotContains(t, body, "Your connections",
		"operator-mode agents don't surface connection state")
}

// TestBuildAppHomeView_Passthrough_AllLinked pins the happy passthrough
// rendering: badge + ✅/✅ connections + Manage button.
func TestBuildAppHomeView_Passthrough_AllLinked(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{
		Agent: &appHomeAgent{
			Name:                     "pm-for-user",
			DisplayName:              "Product Manager",
			Description:              "Helps with project mgmt",
			IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
			LinkedServices:           linkedServices("GitHub", "Linear"),
			AllRequiredServiceLabels: []string{"GitHub", "Linear"},
			PortalLinkURL:            "https://identityd.example.org/my/accounts?d=eyJ&sig=abc",
		},
	})
	body := mustMarshal(t, view.Blocks.BlockSet)
	assert.Contains(t, body, "Uses YOUR account",
		"passthrough badge must appear when identityMode=userPassthrough")
	assert.Contains(t, body, "Your connections",
		"connections section must be present when there are required services")
	assert.Contains(t, body, "white_check_mark",
		"each linked service must render with a checkmark glyph")
	assert.Contains(t, body, "Manage connections",
		"Manage button must be present when PortalLinkURL is set")
	assert.Contains(t, body, "https://identityd.example.org/my/accounts",
		"Manage button URL must point at /my/accounts")
}

// TestBuildAppHomeView_Passthrough_PartialLinked pins the actionable case:
// some services connected, some not. The user needs to clearly see WHICH are
// still missing so they can click Manage and fix it.
func TestBuildAppHomeView_Passthrough_PartialLinked(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{
		Agent: &appHomeAgent{
			Name:                     "pm-for-user",
			DisplayName:              "Product Manager",
			IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
			LinkedServices:           linkedServices("Linear"),     // only Linear
			AllRequiredServiceLabels: []string{"GitHub", "Linear"}, // needs both
			PortalLinkURL:            "https://identityd.example.org/my/accounts?d=x&sig=y",
		},
	})
	body := mustMarshal(t, view.Blocks.BlockSet)
	assert.Contains(t, body, "white_check_mark",
		"connected service rendered with ✓")
	assert.Contains(t, body, "Linear",
		"connected service name must appear")
	assert.Contains(t, body, "heavy_multiplication_x",
		"missing service rendered with ✗")
	assert.Contains(t, body, "GitHub",
		"missing service name must appear")
	assert.Contains(t, body, "not linked yet",
		"missing service annotated so the user knows what to action")
}

// TestBuildAppHomeView_Passthrough_NothingLinked pins the fresh-user case:
// the agent needs accounts but the user hasn't linked any. The "Nothing linked
// yet — tap Manage" CTA matters most for this case (it's the primary funnel
// into setup).
func TestBuildAppHomeView_Passthrough_NothingLinked(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{
		Agent: &appHomeAgent{
			Name:                     "pm-for-user",
			DisplayName:              "Product Manager",
			IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
			LinkedServices:           nil, // nothing linked
			AllRequiredServiceLabels: []string{"GitHub", "Linear"},
			PortalLinkURL:            "https://identityd.example.org/my/accounts?d=x&sig=y",
		},
	})
	body := mustMarshal(t, view.Blocks.BlockSet)
	assert.Contains(t, body, "Nothing linked yet",
		"primary CTA for fresh users")
	assert.Contains(t, body, "Manage connections",
		"setup funnel — the button MUST be present so the user can act")
	assert.Contains(t, body, "GitHub",
		"required services still listed so the user knows what's needed")
	assert.Contains(t, body, "Linear",
		"required services still listed")
}

// TestBuildAppHomeView_Passthrough_NoPortalLink pins the degradation path:
// when the channelsd signer is unavailable, the minter returns "" and we omit
// the Manage button rather than rendering a broken link. The user sees the
// connection state but can't act from Home — better than a 404.
func TestBuildAppHomeView_Passthrough_NoPortalLink(t *testing.T) {
	view := buildAppHomeView(appHomeViewInput{
		Agent: &appHomeAgent{
			Name:                     "pm-for-user",
			IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
			LinkedServices:           linkedServices("Linear"),
			AllRequiredServiceLabels: []string{"Linear"},
			PortalLinkURL:            "", // signer unavailable
		},
	})
	body := mustMarshal(t, view.Blocks.BlockSet)
	assert.Contains(t, body, "Uses YOUR account",
		"badge still appears even without the portal link")
	assert.Contains(t, body, "white_check_mark",
		"connection state still rendered")
	assert.NotContains(t, body, "Manage connections",
		"Manage button must be OMITTED (not rendered with empty URL) when no portal link is available")
}

// TestConnectionsBlock_NoRequiredNoLinked covers the "honest silence" path:
// when we couldn't determine BOTH the required and linked sets, we render
// nothing rather than guess. Saying "this agent doesn't need any of your
// accounts" is wrong for the common case where credentialNamesForClass reads
// only credentialRemap and misses the direct Spec.Auth.Credential path.
func TestConnectionsBlock_NoRequiredNoLinked(t *testing.T) {
	got := connectionsBlock(nil, nil).String()
	assert.Equal(t, "", got,
		"empty required + empty linked must render nothing — not 'doesn't need anything', which has been wrong every time we've checked")
}

// TestConnectionsBlock_LinkedNoRequired covers the lookup-miss path: the user
// has linked accounts, but our credential-set computation for this specific
// agent didn't resolve any required services. Don't claim the agent doesn't
// need them — say what we know (the user has these linked) and move on.
func TestConnectionsBlock_LinkedNoRequired(t *testing.T) {
	got := connectionsBlock(inertAppHomeTexts([]string{"Linear", "GitHub"}), nil).String()
	assert.Contains(t, got, "Linear", "user's linked services must be named")
	assert.Contains(t, got, "GitHub", "user's linked services must be named")
	assert.NotContains(t, got, "doesn't need",
		"don't claim the agent doesn't need anything — we don't actually know that")
}

// mustMarshal is a tiny helper: render the block slice to JSON so
// assertions can substring-match. The view builder produces real
// slack-go block types; JSON gives stable inspection without
// duplicating the SDK's internal tree walks.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
