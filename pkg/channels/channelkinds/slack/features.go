package slack

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// oauthScopesURL is where every scope in this file is granted.
const oauthScopesURL = "https://api.slack.com/apps → your app → 'OAuth & Permissions' → Bot Token Scopes (add, then 'Reinstall to Workspace')"

// FeatureSupport is Slack's single source of truth for what each channel
// feature costs. scopes.go's missingScopes and manifest.go's botScopesYAML
// both derive their required-scope list from this map via
// channelkinds.ScopesFor, rather than maintaining their own copies —
// TestFeatureSupportScopeUnionMatchesKnownScopes (features_test.go) pins the
// resulting union against a golden list so a scope silently dropping out of
// this map (e.g. deleting a channelfeatures entry) still fails a test (see
// scopes_test.go's TestFakeGrantsEveryRequiredScope doc comment for the
// precedent: the same class of drift already bit fakeslack's granted-scope
// header once, when this list was still duplicated by hand).
func (*Kind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return map[channelfeatures.Feature]channelkinds.FeatureRequirement{
		channelfeatures.MessageDelivery: {
			// chat:write.customize lets a post override the bot's display name
			// and icon per-message (chat.postMessage's username/icon_url/
			// icon_emoji params) rather than always posting as the app's own
			// identity. It rides along with MessageDelivery rather than a new
			// feature toggle: any root Slack binding may end up hosting an
			// ATTENDED agent-builder delegation child (plan 4a §13), which posts
			// through the root's own bot but under its own name/icon so a
			// transcript reads as the child speaking, not the root. That is a
			// property of message delivery itself, not something the wizard's
			// per-capability picker should ask about separately.
			Scopes:   []string{"app_mentions:read", "chat:write", "chat:write.customize", "commands", "im:write"},
			Setup:    oauthScopesURL,
			Degrades: "the bot cannot post or receive messages at all — total outage, not a degradation",
		},
		channelfeatures.StatusIndicator: {
			Scopes:   []string{"assistant:write"},
			Setup:    oauthScopesURL + "; also toggle 'Agent or Assistant' ON under the app's 'Agents & AI Apps' tab, and have a workspace admin enable 'Agents & AI Apps'",
			Degrades: "the native 'is thinking…' indicator never appears; the bot still replies normally",
		},
		// Both history features end in resolveAuthors (history.go), so both pay
		// for the user directory as well as the messages: users.info and
		// bots.info each need users:read, and users:read.email is what makes
		// users.info return the profile email at all. Declaring them here rather
		// than leaning on UserLookup is not redundancy — a Channel whose agent
		// has history on and mention_lookup off requests exactly this set, and
		// without the email half every backfilled participant is keyed by a
		// synthetic subject instead of their own identity.
		channelfeatures.ThreadHistory: {
			Scopes:   []string{"channels:history", "groups:history", "im:history", "users:read", "users:read.email"},
			Setup:    oauthScopesURL,
			Degrades: "the agent cannot read the conversation it is replying in, so it loses context between turns",
		},
		channelfeatures.ChannelHistory: {
			Scopes:   []string{"channels:history", "groups:history", "users:read", "users:read.email"},
			Setup:    oauthScopesURL,
			Degrades: "read_channel_history returns nothing, so the agent cannot see messages outside its own thread",
		},
		channelfeatures.UserLookup: {
			Scopes:   []string{"users:read", "users:read.email"},
			Setup:    oauthScopesURL,
			Degrades: "the agent cannot resolve a name or email to a mentionable user, so @-mentions it composes will not link",
		},
		channelfeatures.AttachmentsOutbound: {
			Scopes:   []string{"files:write"},
			Setup:    oauthScopesURL,
			Degrades: "artifact attachments are dropped with a 'delivery failed' footer; the text reply still lands",
		},
		channelfeatures.AttachmentsInbound: {
			Scopes:   []string{"files:read"},
			Setup:    oauthScopesURL,
			Degrades: "every file a user attaches reports 'could not be retrieved' — permanently, since the gap never resolves itself",
		},
		channelfeatures.CredentialPortal: {
			// app_home_opened is an EVENT subscription, not a scope, so no
			// reinstall or re-consent is needed to enable it.
			Setup:    "https://api.slack.com/apps → your app → 'App Manifest': set features.app_home.home_tab_enabled: true and add app_home_opened under settings.event_subscriptions.bot_events",
			Degrades: "the Home tab shows no 'Manage connections' button, so userPassthrough agents have no self-service credential fix",
		},
		channelfeatures.SessionViews: {
			Setup:    "no extra Slack permission required",
			Degrades: "nothing on the Slack side",
		},
		// relsync_kind.go's SyncKind is what this feature exists for, and this
		// is the WHOLE call set it makes, not the half of it that enumerates:
		// ListScopes calls conversations.list requesting BOTH public_channel
		// and private_channel in one call, and FetchScope calls
		// conversations.info/conversations.members per channel, public or
		// private, then users.info per member. Neither channel scope is
		// optional depending on which channel types this workspace happens to
		// have — the same code path must work for either, so both are
		// declared together.
		//
		// users:read and users:read.email are the member half, and they are
		// declared HERE rather than left to arrive from the history features.
		// They did arrive that way, which is why this list was short of them
		// for a while and nothing went red: mention_lookup, thread_history and
		// channel_history all declare users:read and are all default-on, so a
		// scopes-valid check on an ordinary AgentClass saw them. An AgentClass
		// that turns those three off and turns directory_sync on gets exactly
		// what this entry asks for and nothing else — a bot token, and a
		// generated app manifest (manifest.go's BotTokenAppManifestFor), built
		// from this line alone.
		//
		// users:read.email is the load-bearing half of that pair. Slack does
		// not refuse users.info without it; it answers and OMITS profile.email
		// — and FetchScope keys the identity join on the email, so every
		// member resolves to nobody, is dropped as a join miss, and a sync
		// that wrote NOTHING reports success.
		//
		// TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed
		// (relsync_scopes_test.go) drives ListScopes/FetchScope for real and
		// pins this the same way TestHistoryFeaturesDeclareTheScopesTheirCallsNeed
		// (history_scopes_test.go) pins the history path — this repo shipped
		// the "manifest requests neither scope the sync path needs" bug
		// once; the guard exists so the next relsync method added without a
		// matching scope fails a test, not a cluster.
		channelfeatures.DirectorySync: {
			Scopes: []string{"channels:read", "groups:read", "users:read", "users:read.email"},
			Setup:  oauthScopesURL,
			Degrades: "a RelationshipSource using this app's bot token cannot enumerate this workspace at all: " +
				"conversations.list, conversations.info and conversations.members all fail with missing_scope, " +
				"and users.info either fails or returns members with no email to join on, " +
				"so the periodic Slack directory sync writes nothing to SpiceDB — see pkg/platform/relsync",
		},
	}
}
