package slack

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"

// Compile-time proof that *fakeslack.Client satisfies the injected client
// interfaces. If a method signature drifts, this fails to build.
var (
	_ slackClient          = (*fakeslack.Client)(nil)
	_ historyClient        = (*fakeslack.Client)(nil)
	_ attachmentInfoClient = (*fakeslack.Client)(nil)
)
