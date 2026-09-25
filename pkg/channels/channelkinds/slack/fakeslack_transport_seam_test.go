package slack

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// Compile-time proof that *fakeslack.Client also backs the LISTENER's Slack
// Web API surface, and that *fakeslack.SocketSource backs its socket-mode
// event source. fakeslack_seam_test.go proves the SENDER-side interfaces
// (slackClient, historyClient); this file is what proves listenerAPIClient
// and socketSource. If either signature drifts, this fails to build.
var (
	_ listenerAPIClient = (*fakeslack.Client)(nil)
	_ socketSource      = (*fakeslack.SocketSource)(nil)
)
