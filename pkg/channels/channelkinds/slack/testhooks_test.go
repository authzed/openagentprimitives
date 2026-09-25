package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClient is a minimal listenerAPIClient + slackClient double used only to
// prove InstallTestTransport's plumbing (shared identity across both
// factories + reset), independent of fakeslack's completeness. fakeslack.Client
// now also implements the listener-side methods (AuthTestContext, OpenView,
// PublishViewContext — see fakeslack_transport_seam_test.go), but this local
// stub stays: it isolates the plumbing test from fakeslack's behavior.
// Pointer receiver so identity comparisons (assert.Same) actually prove "the
// same instance", not just "an equal value".
type stubClient struct{}

func (*stubClient) AuthTestContext(context.Context) (*slackapi.AuthTestResponse, error) {
	return &slackapi.AuthTestResponse{}, nil
}
func (*stubClient) SetAssistantThreadsStatusContext(context.Context, slackapi.AssistantThreadsSetStatusParameters) error {
	return nil
}
func (*stubClient) SetAssistantThreadsTitleContext(context.Context, slackapi.AssistantThreadsSetTitleParameters) error {
	return nil
}
func (*stubClient) PostMessageContext(_ context.Context, channelID string, _ ...slackapi.MsgOption) (string, string, error) {
	return channelID, "", nil
}
func (*stubClient) PostEphemeralContext(context.Context, string, string, ...slackapi.MsgOption) (string, error) {
	return "", nil
}
func (*stubClient) GetUserInfoContext(_ context.Context, user string) (*slackapi.User, error) {
	return &slackapi.User{ID: user}, nil
}
func (*stubClient) OpenViewContext(context.Context, string, slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}
func (*stubClient) OpenView(string, slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}
func (*stubClient) PublishViewContext(context.Context, slackapi.PublishViewContextRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}
func (*stubClient) UpdateMessageContext(_ context.Context, channelID, ts string, _ ...slackapi.MsgOption) (string, string, string, error) {
	return channelID, ts, "", nil
}
func (*stubClient) OpenConversationContext(context.Context, *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error) {
	return &slackapi.Channel{}, false, false, nil
}
func (*stubClient) UploadFileContext(context.Context, slackapi.UploadFileParameters) (*slackapi.FileSummary, error) {
	return &slackapi.FileSummary{}, nil
}
func (*stubClient) GetUserByEmailContext(context.Context, string) (*slackapi.User, error) {
	return &slackapi.User{}, nil
}
func (*stubClient) GetUsersContext(context.Context, ...slackapi.GetUsersOption) ([]slackapi.User, error) {
	return nil, nil
}
func (*stubClient) GetPermalinkContext(context.Context, *slackapi.PermalinkParameters) (string, error) {
	return "", nil
}

var (
	_ listenerAPIClient = (*stubClient)(nil)
	_ slackClient       = (*stubClient)(nil)
)

// stubSource is a minimal socketSource double. fakeslack.SocketSource (a
// channel-backed socketSource, see fakeslack_transport_seam_test.go) now
// exists too; this local stub is kept to prove InstallTestTransport wires the
// source through newSocketSource in isolation from fakeslack's behavior.
type stubSource struct{}

func (stubSource) Events() <-chan socketmode.Event        { return nil }
func (stubSource) RunContext(context.Context) error       { return nil }
func (stubSource) Ack(socketmode.Request, ...interface{}) {}

var _ socketSource = stubSource{}

func TestInstallTestTransport_SharesClientAcrossListenerAndSender_ResetRestores(t *testing.T) {
	// Precondition: no override installed anywhere in this test binary yet →
	// both factories fall through to production token-based construction,
	// which returns a true nil interface for a nil Secret.
	require.Nil(t, newSlackAPIClientFull(nil), "precondition: no override -> listener factory nil on nil secret")
	require.Nil(t, newSlackAPIClient(nil), "precondition: no override -> sender factory nil on nil secret")

	fake := &stubClient{}
	source := stubSource{}
	reset := InstallTestTransport(fake, source)
	t.Cleanup(reset)

	gotListener := newSlackAPIClientFull(nil)
	gotSender := newSlackAPIClient(nil)
	assert.Same(t, fake, gotListener, "listener factory must return the installed fake instance")
	assert.Same(t, fake, gotSender, "sender factory must return the SAME installed fake instance, not a copy")
	assert.Equal(t, source, newSocketSource(gotListener), "newSocketSource must return the installed stub source")

	reset()

	assert.Nil(t, newSlackAPIClientFull(nil), "reset must restore production nil-on-nil-secret behavior for the listener factory")
	assert.Nil(t, newSlackAPIClient(nil), "reset must restore production nil-on-nil-secret behavior for the sender factory")
}
