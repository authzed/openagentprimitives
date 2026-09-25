package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slackBotTokenScopes is a checked-in transcription of the bot-token scopes
// Slack's scope reference (https://docs.slack.dev/reference/scopes) defines.
//
// WHAT IT PROVES: that a scope name in features.go was copied off Slack's list
// rather than inferred from a method name. Slack does not tolerate a scope it
// does not define — apps.manifest.create rejects the entire manifest, and
// adding one by hand in the app's OAuth settings is refused as illegal — so an
// invented name is not a cosmetic error. It makes every manifest this tool
// generates unimportable and every app it offers to create for a user fail at
// the API.
//
// WHAT IT DOES NOT PROVE: that a declared scope is the RIGHT one for the API
// call it is meant to cover — TestHistoryFeaturesDeclareTheScopesTheirCallsNeed
// is the check for that — nor that this list is complete or current. It is
// hand-maintained: a test cannot call Slack, so a transcription of the
// reference is the closest available stand-in for asking Slack itself, and it
// is only as good as the last person to update it.
//
// MAINTAINING IT: when this test fails on a scope you believe is real, confirm
// it on the scope reference page — not on a method's doc prose, and never from
// the method's name — then add it here in exactly that spelling. Never add a
// name on the grounds that our code already uses it: that circularity is the
// one thing this list exists to break.
var slackBotTokenScopes = []string{
	"app_mentions:read",
	"assistant:write",
	"bookmarks:read",
	"bookmarks:write",
	"calls:read",
	"calls:write",
	"canvases:read",
	"canvases:write",
	"channels:history",
	"channels:join",
	"channels:manage",
	"channels:read",
	"channels:write.invites",
	"channels:write.topic",
	"chat:write",
	"chat:write.customize",
	"chat:write.public",
	"commands",
	"conversations.connect:manage",
	"conversations.connect:read",
	"conversations.connect:write",
	"dnd:read",
	"emoji:read",
	"files:read",
	"files:write",
	"groups:history",
	"groups:read",
	"groups:write",
	"groups:write.invites",
	"groups:write.topic",
	"im:history",
	"im:read",
	"im:write",
	"im:write.invites",
	"im:write.topic",
	"incoming-webhook",
	"links.embed:write",
	"links:read",
	"links:write",
	"metadata.message:read",
	"mpim:history",
	"mpim:read",
	"mpim:write",
	"mpim:write.invites",
	"mpim:write.topic",
	"pins:read",
	"pins:write",
	"reactions:read",
	"reactions:write",
	"reminders:read",
	"reminders:write",
	"remote_files:read",
	"remote_files:share",
	"remote_files:write",
	"team.billing:read",
	"team.preferences:read",
	"team:read",
	"triggers:read",
	"triggers:write",
	"usergroups:read",
	"usergroups:write",
	"users.profile:read",
	"users:read",
	"users:read.email",
	"users:write",
	"workflow.steps:execute",
}

// TestFeatureSupportDeclaresOnlyScopesSlackDefines checks every scope in
// features.go against slackBotTokenScopes.
//
// This is the seed-level check the golden union lists cannot be: every one of
// them was written by copying whatever FeatureSupport already said, so all of
// them agreed with "bots:read" — a name that reads like a scope, is not one,
// and made Slack reject the generated manifest outright.
func TestFeatureSupportDeclaresOnlyScopesSlackDefines(t *testing.T) {
	defined := make(map[string]bool, len(slackBotTokenScopes))
	for _, s := range slackBotTokenScopes {
		defined[s] = true
	}
	require.Len(t, defined, len(slackBotTokenScopes),
		"slackBotTokenScopes lists a scope twice; the reference has no duplicates")

	for feature, req := range (&Kind{}).FeatureSupport() {
		for _, scope := range req.Scopes {
			assert.Truef(t, defined[scope],
				"feature %q declares %q, which is not a scope Slack defines; Slack rejects a "+
					"manifest naming an undefined scope, so no app can be created from ours. If %q "+
					"really is on https://docs.slack.dev/reference/scopes, add it to "+
					"slackBotTokenScopes", feature, scope, scope)
		}
	}
}
