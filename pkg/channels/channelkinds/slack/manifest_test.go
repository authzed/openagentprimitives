package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fullManifest is the manifest for the default bot name and every feature the
// vocabulary defines. The wizard never renders this combination — it asks which
// capabilities the agent needs and renders only their scopes — but the widest
// feature set exercises in one pass the parts of the TEMPLATE that no feature
// set varies: the agent_view surface, the restart shortcut, interactivity, the
// Home tab and the event subscriptions.
func fullManifest() string { return appManifestFor(defaultBotDisplayName, channelfeatures.All()) }

// TestAppManifestParses verifies the embedded manifest is valid YAML.
// (Slack's manifest schema lives in their docs; we don't try to validate
// against it here — just ensure we're not shipping a typo'd YAML.)
func TestAppManifestParses(t *testing.T) {
	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(fullManifest()), &got),
		"manifest is invalid YAML")
	assert.NotNil(t, got["display_information"], "missing display_information")
	assert.NotNil(t, got["oauth_config"], "missing oauth_config")
	assert.NotNil(t, got["settings"], "missing settings")
}

func TestAppManifestRequiredScopes(t *testing.T) {
	required := []string{
		"app_mentions:read",
		"chat:write",
		"channels:history",
		"groups:history",
		"im:history",
		"im:write",
		"users:read.email",
		"assistant:write",
		// The user directory, which the history path needs as much as the
		// mention lookup does: bots.info is the only source of a readable name
		// for a webhook-posted message (no username, no bot_profile), and
		// without users:read every alert in a backfilled thread is attributed
		// to a raw bot id.
		"users:read",
	}
	for _, scope := range required {
		assert.Containsf(t, fullManifest(), scope, "manifest missing required scope: %q", scope)
	}
}

// TestAppManifestGrantsChatWriteCustomize pins the bot scope an ATTENDED
// agent-builder delegation child needs to post through the root binding's
// Slack bot with a per-message display name/icon (plan 4a §13). It is folded
// into MessageDelivery — the feature every Channel already requests — rather
// than a new wizard toggle, since any root binding may end up hosting an
// attended child and the manifest the wizard emits must carry the scope
// unconditionally for that to work.
func TestAppManifestGrantsChatWriteCustomize(t *testing.T) {
	assert.Contains(t, fullManifest(), "chat:write.customize",
		"manifest missing chat:write.customize — needed for an attended delegation child "+
			"to post through the root bot with a per-message display name/icon")
}

func TestAppManifestRestartShortcut(t *testing.T) {
	// Restart-from-here message-action shortcut must be declared so
	// Slack delivers message_action callbacks to the listener.
	assert.Contains(t, fullManifest(), "ap_restart_from_here",
		"manifest missing restart message-action callback_id")
	assert.Contains(t, fullManifest(), "type: message",
		"restart shortcut must be type=message")
}

func TestAppManifestInteractivityEnabled(t *testing.T) {
	// Shortcuts + view_submission require interactivity:is_enabled.
	assert.Contains(t, fullManifest(), "is_enabled: true",
		"interactivity must be enabled for restart-from-here shortcuts")
}

func TestAppManifestFor_DerivesDisplayName(t *testing.T) {
	m := appManifestFor("helpdesk-bot", channelfeatures.All())
	assert.Contains(t, m, "name: helpdesk-bot", "app display name must be derived")
	assert.Contains(t, m, "display_name: helpdesk-bot", "bot display name must be derived")
	assert.NotContains(t, m, "agentprimitives-bot", "default name must be replaced")
	// Still a complete, valid manifest.
	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(m), &got), "templated manifest is invalid YAML")
	assert.Contains(t, m, "app_mentions:read", "scopes must survive templating")
	assert.Contains(t, m, "ap_restart_from_here", "shortcut must survive templating")
}

func TestAppManifestFor_EmptyFallsBackToDefault(t *testing.T) {
	assert.Contains(t, appManifestFor("", channelfeatures.All()), "name: agentprimitives-bot",
		"empty display name falls back to the product default")
}

// manifestBotScopes unmarshals a manifest's oauth_config.scopes.bot list.
// yaml.Unmarshal (sigs.k8s.io/yaml) round-trips through encoding/json, so
// the struct is tagged with json, not yaml.
func manifestBotScopes(t *testing.T, manifest string) []string {
	t.Helper()
	var doc struct {
		OauthConfig struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &doc), "manifest must be valid YAML")
	return doc.OauthConfig.Scopes.Bot
}

// TestAppManifestScopeBlockIsGeneratedFromFeatures replaces the old
// TestRequiredScopes_MirrorManifest hand-maintained-mirror check: with only
// one scope list left (FeatureSupport, features.go), there is nothing left to
// mirror against. Instead this asserts the generation property directly, by
// equality rather than containment: the manifest's oauth_config.scopes.bot
// list equals channelkinds.ScopesFor's output for the given feature set,
// element-for-element and in order. Containment alone only closes the "omit
// a needed scope" direction — a botScopesYAML that appended some unrelated
// hardcoded extra scope would still pass a Contains-only check. Equality
// closes the "request a scope the feature set doesn't need" direction too,
// and is what makes a narrower feature set provably produce a strictly
// narrower manifest rather than just "still contains the narrow set,
// plus who knows what else."
func TestAppManifestScopeBlockIsGeneratedFromFeatures(t *testing.T) {
	k := &Kind{}

	full := appManifestFor("demo-bot", channelfeatures.All())
	assert.Equal(t, channelkinds.ScopesFor(k, channelfeatures.All()), manifestBotScopes(t, full),
		"manifest's bot scopes must equal ScopesFor(k, channelfeatures.All()) exactly")

	// StatusIndicator's only scope (features.go) is assistant:write —
	// chat:write belongs to MessageDelivery, a different feature.
	narrow := appManifestFor("demo-bot", []channelfeatures.Feature{channelfeatures.StatusIndicator})
	assert.Equal(t,
		channelkinds.ScopesFor(k, []channelfeatures.Feature{channelfeatures.StatusIndicator}),
		manifestBotScopes(t, narrow),
		"manifest's bot scopes must equal ScopesFor(k, features) exactly for a narrowed feature set too")
}

// TestAppManifestScopeBlockIsStableAcrossCalls guards determinism:
// channelkinds.ScopesFor sorts its output, so two calls with identical
// inputs must render byte-identical YAML. A regression here (e.g. iterating
// a map without sorting) would make the manifest churn between prints and
// silently break TestAppManifestScopeBlockIsGeneratedFromFeatures-style
// substring assertions in flaky ways.
func TestAppManifestScopeBlockIsStableAcrossCalls(t *testing.T) {
	a := appManifestFor("demo-bot", channelfeatures.All())
	b := appManifestFor("demo-bot", channelfeatures.All())
	assert.Equal(t, a, b, "manifest generation must be deterministic — no map iteration order")
}

func TestAppManifestEventSubscriptions(t *testing.T) {
	required := []string{
		"app_mention",
		"message.channels",
		"message.groups",
		"message.im",
		"app_home_opened",
	}
	for _, ev := range required {
		// Each event appears exactly once as a list item.
		assert.Containsf(t, fullManifest(), "- "+ev+"\n",
			"manifest missing event subscription: %q", ev)
	}
}

// TestAppManifestOmitsUnwiredMembershipEvents is the revert half of the
// whole-branch review's Major 4: member_joined_channel/member_left_channel
// would feed a RelationshipSource's Invalidator
// (pkg/controllers/relationshipsource/invalidate.go), but nothing constructs
// one or calls OnWake yet — see appManifestFor's own doc. Subscribing every
// per-agent app to these events ahead of a consumer would only cost a busy
// workspace a permanent stream of unconsumed "events_api inner" logs for a
// feature operators cannot benefit from.
func TestAppManifestOmitsUnwiredMembershipEvents(t *testing.T) {
	assert.NotContains(t, fullManifest(), "member_joined_channel",
		"the Invalidator this event would feed is unwired; land the subscription WITH its consumer")
	assert.NotContains(t, fullManifest(), "member_left_channel",
		"the Invalidator this event would feed is unwired; land the subscription WITH its consumer")
}

func TestAppManifestUsesAgentView(t *testing.T) {
	assert.Contains(t, fullManifest(), "agent_view:", "manifest must declare agent_view")
	assert.Contains(t, fullManifest(), "agent_description:", "agent_view needs agent_description")
	assert.NotContains(t, fullManifest(), "assistant_view", "assistant_view must be gone")
	assert.NotContains(t, fullManifest(), "assistant_description", "assistant_description must be gone")
	assert.NotContains(t, fullManifest(), "assistant_thread_started",
		"assistant_thread_started does not fire under agent_view; drop the subscription")
	assert.NotContains(t, fullManifest(), "assistant_thread_context_changed",
		"assistant_thread_context_changed is assistant_view-only")
	assert.Contains(t, fullManifest(), "assistant:write", "assistant:write scope must remain (setStatus)")
}

func TestAppManifestEnablesHomeTab(t *testing.T) {
	// The App Home tab hosts the per-agent "Manage connections" button
	// (userPassthrough agents). It must be enabled and the app_home_opened
	// event subscribed, or Slack never delivers AppHomeOpenedEvent and the
	// views.publish path stays dead.
	assert.Contains(t, fullManifest(), "home_tab_enabled: true",
		"home tab must be enabled for the Manage-connections button")
	assert.Contains(t, fullManifest(), "- app_home_opened\n",
		"app_home_opened must be subscribed so views.publish is exercised")
}

// directorySyncFeatures is the feature set the credential-setup flow generates
// a manifest for: a Slack app that exists to issue one bot token for a
// directory sync, and does nothing else.
var directorySyncFeatures = []channelfeatures.Feature{channelfeatures.DirectorySync}

// botTokenManifest is that manifest, for a made-up identity name.
func botTokenManifest() string {
	return BotTokenAppManifestFor("demo-directory-bot", directorySyncFeatures)
}

// TestBotTokenAppManifestForDirectorySync is the literal pin on what an
// operator's generated app will request, and it is deliberately NOT expressed
// as "equals ScopesFor(DirectorySync)" — that form follows the declaration
// wherever it goes, so it stays green for a declaration that has quietly lost
// a scope. These four names are the claim.
//
// users:read.email is the one this test exists for. Slack does not refuse
// users.info without it; it answers and omits profile.email, so an app created
// from a manifest missing it installs cleanly, syncs, and writes nothing —
// every member dropped as a join miss. Generating the app is what turns that
// from an operator's typo into something this repo ships, which is why the
// scope is asserted here as well as in
// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed.
func TestBotTokenAppManifestForDirectorySync(t *testing.T) {
	scopes := manifestBotScopes(t, botTokenManifest())
	assert.ElementsMatch(t,
		[]string{"channels:read", "groups:read", "users:read", "users:read.email"}, scopes,
		"the generated directory-sync app must request exactly the four scopes the sync calls with")
	require.NotEmpty(t, scopes,
		"a manifest with an empty bot scope list is one Slack refuses; the feature set declared none")
}

// TestBotTokenAppManifestScopeBlockIsGeneratedFromFeatures is the other
// direction: whatever the declaration says, the manifest says the same, by
// equality rather than containment — so a renderer that appended a scope of
// its own fails here even while the literal pin above passes.
func TestBotTokenAppManifestScopeBlockIsGeneratedFromFeatures(t *testing.T) {
	assert.Equal(t,
		channelkinds.ScopesFor(&Kind{}, directorySyncFeatures),
		manifestBotScopes(t, botTokenManifest()),
		"the bot scope block must equal ScopesFor(k, features) exactly")
}

// TestBotTokenAppManifestSubscribesToNothing is why this manifest is not
// appManifestFor with the listener parts trimmed. Slack validates bot_events
// against the scopes the same manifest requests, and the channel template's
// message.* subscriptions want history scopes a directory sync does not
// declare — that document would be refused as invalid_manifest. A manifest
// with no events cannot be inconsistent with its own scopes, and this is what
// stops one being added back without the scopes that would make it valid.
func TestBotTokenAppManifestSubscribesToNothing(t *testing.T) {
	m := botTokenManifest()
	for _, absent := range []string{
		"event_subscriptions", "bot_events", "socket_mode_enabled",
		"app_home", "agent_view", "shortcuts", "interactivity",
	} {
		assert.NotContainsf(t, m, absent,
			"a token-only app declares no %s; it has no listener to deliver to", absent)
	}
}

// TestBotTokenAppManifestParses keeps the same floor the channel manifest has:
// valid YAML with the two stanzas Slack requires to mint a bot token.
func TestBotTokenAppManifestParses(t *testing.T) {
	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(botTokenManifest()), &got), "manifest is invalid YAML")
	assert.NotNil(t, got["display_information"], "missing display_information")
	assert.NotNil(t, got["features"], "missing features.bot_user; no bot user means no bot token")
	assert.NotNil(t, got["oauth_config"], "missing oauth_config")
}

func TestBotTokenAppManifestNaming(t *testing.T) {
	named := botTokenManifest()
	assert.Contains(t, named, "name: demo-directory-bot", "app display name must be derived")
	assert.Contains(t, named, "display_name: demo-directory-bot", "bot display name must be derived")
	assert.Contains(t, BotTokenAppManifestFor("", directorySyncFeatures), "name: agentprimitives-bot",
		"empty display name falls back to the product default")
}

// TestBotTokenAppManifestFitsTheNoteWidth holds the same line-width rule the
// channel manifest has, and for the same reason: this is rendered on a huh
// note that hard-wraps at the form's width, and a wrapped YAML line is one
// Slack's import rejects — with nothing in the render to say so.
func TestBotTokenAppManifestFitsTheNoteWidth(t *testing.T) {
	assert.Empty(t, tui.RailedNoteBudget().Overflows(botTokenManifest()),
		"every manifest line must fit the note budget whole")
}

// manifestDisplayNames pulls the two names a manifest carries:
// display_information.name and features.bot_user.display_name. They are the
// same value today and are read separately so a template that stopped
// deriving one of them from the other fails here.
func manifestDisplayNames(t *testing.T, manifest string) (appName, botName string) {
	t.Helper()
	var doc struct {
		DisplayInformation struct {
			Name string `json:"name"`
		} `json:"display_information"`
		Features struct {
			BotUser struct {
				DisplayName string `json:"display_name"`
			} `json:"bot_user"`
		} `json:"features"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &doc), "manifest must be valid YAML")
	return doc.DisplayInformation.Name, doc.Features.BotUser.DisplayName
}

// TestManifestDisplayNameIsSanitized is the fix for a rejection an operator
// would have had no way to diagnose.
//
// Both templates used to interpolate the caller's string raw, and the callers
// pass AgentClass and AgentIdentity names — validated as DNS-1123 subdomains,
// which permit periods and 253 characters. Slack permits neither: a dot in
// bot_user.display_name and a display_information.name past 35 characters are
// each refused as invalid_manifest, with no stated cause on the paste path and
// no recovery on the provisioning one (appprovision/errors.go). An identity
// named "acme.slack-directory-for-the-platform-team-prod" is entirely ordinary
// and trips both at once.
//
// Driven through the two REAL templates rather than through
// manifestDisplayName alone: the defect was a template interpolating around
// the helper, so a test of the helper by itself would have passed throughout.
func TestManifestDisplayNameIsSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a dotted identity name: folded, because Slack refuses a dot in a bot display name",
			in:   "acme.slack-directory",
			want: "acme-slack-directory",
		},
		{
			name: "past Slack's 35-character cap: cut to it, and not left ending on a fold's hyphen",
			in:   "acme.slack-directory-for-the-platform-team-prod",
			want: "acme-slack-directory-for-the-platfo",
		},
		{
			name: "empty: the product default, which is what having no agent to name it after means",
			in:   "",
			want: defaultBotDisplayName,
		},
		{
			name: "nothing but separators: also the default, since the fold leaves nothing to name it",
			in:   "...---...",
			want: defaultBotDisplayName,
		},
		{
			name: "an already-valid name passes through untouched",
			in:   "helpdesk-bot",
			want: "helpdesk-bot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for template, manifest := range map[string]string{
				"appManifestFor":         appManifestFor(tc.in, channelfeatures.All()),
				"BotTokenAppManifestFor": BotTokenAppManifestFor(tc.in, directorySyncFeatures),
			} {
				appName, botName := manifestDisplayNames(t, manifest)
				assert.Equalf(t, tc.want, appName, "%s: display_information.name", template)
				assert.Equalf(t, tc.want, botName, "%s: features.bot_user.display_name", template)
				assert.LessOrEqualf(t, len(appName), manifestNameLimit,
					"%s: Slack refuses a name past %d characters", template, manifestNameLimit)
				assert.NotContainsf(t, appName, ".", "%s: Slack refuses a dot in the bot display name", template)
			}
		})
	}
}
