// pkg/channels/channelkinds/slack/listener_client.go
//
// listenerAPIClient extracts the Slack Web API surface the listener calls on
// its `api` field into an interface, so tests (and, in a later task, the
// fakeslack simulator) can inject a fake without a real bot token. Production
// still constructs the real *slackapi.Client via newSlackAPIClientFull.
package slack

import (
	"context"

	slackapi "github.com/slack-go/slack"
	corev1 "k8s.io/api/core/v1"
)

// listenerAPIClient is the Slack Web API surface the listener calls directly
// on l.api. Extracted as an interface so tests can inject a fake (fakeslack)
// without a real bot token. The concrete *slackapi.Client satisfies it; see
// the compile-time proof below. The method set here is exactly what
// `grep -n "l\.api\." pkg/channels/channelkinds/slack/*.go | grep -v _test` finds —
// keep it in sync if a new call site is added.
type listenerAPIClient interface {
	// AuthTestContext resolves the bot's own user/team ID at Start.
	AuthTestContext(ctx context.Context) (*slackapi.AuthTestResponse, error)
	// SetAssistantThreadsStatusContext drives the "Thinking…" AI-assistant
	// thread status indicator.
	SetAssistantThreadsStatusContext(ctx context.Context, params slackapi.AssistantThreadsSetStatusParameters) error
	// PostMessageContext posts join notices, starter messages, and other
	// listener-originated chat.postMessage calls.
	PostMessageContext(ctx context.Context, channelID string, options ...slackapi.MsgOption) (string, string, error)
	// PostEphemeralContext posts the metaagent scope-approval ephemeral.
	PostEphemeralContext(ctx context.Context, channelID, userID string, options ...slackapi.MsgOption) (string, error)
	// GetUserInfoContext resolves a Slack user's profile (email, team, name)
	// for identity resolution and the App Home tab.
	GetUserInfoContext(ctx context.Context, user string, _ ...slackapi.GetUserInfoOption) (*slackapi.User, error)
	// OpenViewContext opens a modal (Show Details, Show settings, etc.).
	OpenViewContext(ctx context.Context, triggerID string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error)
	// OpenView is the non-context views.open, used by the "Restart from
	// here" shortcut's modal + error-modal paths.
	OpenView(triggerID string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error)
	// PublishViewContext publishes the App Home tab.
	PublishViewContext(ctx context.Context, req slackapi.PublishViewContextRequest) (*slackapi.ViewResponse, error)
}

// compile-time proof the real client still satisfies the interface.
var _ listenerAPIClient = (*slackapi.Client)(nil)

// newSlackAPIClientFull returns a Slack Web API client configured with both
// the bot-token and the app-level token (required by socketmode.New), as the
// listenerAPIClient interface. The Sender's newSlackAPIClient only needs the
// bot-token, so we can't reuse it directly. Returns a true nil interface
// (never a typed-nil pointer) when either token is missing.
func newSlackAPIClientFull(sec *corev1.Secret) listenerAPIClient {
	if testClientOverride != nil {
		return testClientOverride
	}
	if sec == nil {
		return nil
	}
	bot := sec.Data[SecretKeyBotToken]
	app := sec.Data[SecretKeyAppToken]
	if len(bot) == 0 || len(app) == 0 {
		return nil
	}
	return slackapi.New(string(bot), slackapi.OptionAppLevelToken(string(app)))
}
