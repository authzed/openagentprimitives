package slack

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

var _ relsync.Configurable = (*SyncKind)(nil)

// ConfigScreens returns no screens: this kind syncs whatever channels and
// members its bot token reaches (SyncKind's own doc), so there is nothing
// beyond credential and endpoint to ask. Returning nil rather than a screen
// with no real question is the deliberate "asks nothing" case
// relsync.Configurable's own doc calls out — not a placeholder left unwired.
func (k *SyncKind) ConfigScreens(prior relsync.ExistingConfig) []relsync.ConfigScreen {
	return nil
}

// BuildConfig always returns nil, nil: there are no answers to turn into
// spec.config because ConfigScreens never asks any.
func (k *SyncKind) BuildConfig(answers map[string]string) (json.RawMessage, error) {
	return nil, nil
}

// NeedsEndpoint reports false: Slack's API host is fixed, not a per-install
// address the operator supplies.
func (k *SyncKind) NeedsEndpoint() bool {
	return false
}

var _ relsync.CredentialSetup = (*SyncKind)(nil)

const (
	// slackSetupFlowName is the builtins.Flow that mints this kind's
	// credential: a bot token from the operator's own Slack app.
	slackSetupFlowName = "slack-bot-token"

	// slackSetupIntent is the prose that flow narrows its suggested scopes
	// from. It says "read" and never "write" or "post": this kind only calls
	// conversations.list/.info/.members, users.info and auth.test, so a token
	// carrying chat:write would be broader than anything this sync does with
	// it.
	slackSetupIntent = "read-only access to workspace channels and their members for directory sync"
)

// SetupFlow names the flow an operator with no Slack credential yet is
// walked through, and the intent that keeps its scope recommendation to the
// read scopes this kind's API calls actually need.
func (k *SyncKind) SetupFlow() (flow, intent string) {
	return slackSetupFlowName, slackSetupIntent
}

// SetupScopes returns nil: slack-bot-token derives its own recommendation
// from the calls THIS sync makes (see that flow's scopeHint, and
// relsync_scopes_test.go's relsyncMethodBotScopes, which pin the same four
// read scopes against a driven run). Restating them here would be a second
// hand-maintained copy of a list that is already correct and already tested,
// and the copy is the one that would rot.
func (k *SyncKind) SetupScopes() []string { return nil }
