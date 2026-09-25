//go:build e2e

package e2e

import (
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// SlackFake returns the harness's shared fake Slack client. Scenario
// packages live in their own `_test` packages (e.g. slack_dm_threading_test)
// and so cannot reach the unexported slackFake field directly; this
// exported accessor is the seam. Its most important use is SeedUser: the
// real listener's resolveIdentity only trusts a Slack profile email as the
// canonical SpiceDB subject for a full member of the bot's own installed
// workspace (see emailTrusted in pkg/channels/channelkinds/slack/listener.go), so a
// scenario MUST seed the sending user (TeamID matching fakeslack's
// AuthTestContext team "T-test", plus the desired Email) before driving any
// inbound — otherwise the message is attributed to an untrusted synthetic
// identity and authz/session-ownership will not match the fixture's
// expectations.
func (h *Harness) SlackFake() *fakeslack.Client { return h.slackFake }

// SendSlackDM injects a user DM through the REAL slack listener (via the
// shared h.slackFake / h.slackSource installed by Start, see harness.go's
// slack.InstallTestTransport call) and returns the user message's ts — the
// thread anchor a threaded agent reply is expected to land under. Requires
// the harness's Channel to be kind: slack; the fake listener's dispatch loop
// consumes the pushed event exactly as the production socketmode.Client's
// event channel would.
func (h *Harness) SendSlackDM(userID, channelID, text string) (userMsgTS string) {
	h.t.Helper()
	ts, ev := h.slackFake.InjectDM(userID, channelID, text)
	h.slackSource.Push(ev)
	return ts
}

// SendSlackDMWithFiles is SendSlackDM plus one or more file attachments
// (from h.SlackFake().SeedFile) — the real-slack-kind path an inbound
// attachment scenario needs, mirroring SendSlackDM's InjectDM call but
// through InjectDMWithFiles.
func (h *Harness) SendSlackDMWithFiles(userID, channelID, text string, files []slackapi.File) (userMsgTS string) {
	h.t.Helper()
	ts, ev := h.slackFake.InjectDMWithFiles(userID, channelID, text, files)
	h.slackSource.Push(ev)
	return ts
}

// SendSlackMention injects a channel @-mention through the real slack
// listener, mirroring SendSlackDM for the app_mention inbound path. Returns
// the mention's ts.
func (h *Harness) SendSlackMention(userID, channelID, text string) (msgTS string) {
	h.t.Helper()
	ts, ev := h.slackFake.InjectMention(userID, channelID, text)
	h.slackSource.Push(ev)
	return ts
}

// SendSlackMentionInThread injects a channel @-mention that is a reply within
// the thread rooted at threadTS (so it correlates to the same session/thread as
// the root). Used to have a SECOND user summon the bot into a thread another
// user started. Returns the reply mention's ts.
func (h *Harness) SendSlackMentionInThread(userID, channelID, text, threadTS string) (msgTS string) {
	h.t.Helper()
	ts, ev := h.slackFake.InjectMentionInThread(userID, channelID, text, threadTS)
	h.slackSource.Push(ev)
	return ts
}

// SlackTopLevel returns the channel's top-level (non-threaded) messages as
// recorded by the fake Slack client — both the injected inbound DMs/mentions
// and any outbound posts the real sender made with no thread_ts.
func (h *Harness) SlackTopLevel(channelID string) []fakeslack.Message {
	h.t.Helper()
	return h.slackFake.TopLevel(channelID)
}

// ExpectSlackThreadedReply polls until fakeslack shows exactly one reply
// threaded under rootTS in channelID (i.e. the real slack sender posted
// exactly one chat.postMessage with thread_ts == rootTS), or timeout
// elapses. Fatals with a diagnostic (top-level + reply counts) so a miss
// distinguishes "nothing posted yet" from "posted somewhere unexpected."
func (h *Harness) ExpectSlackThreadedReply(t *testing.T, channelID, rootTS string, timeout time.Duration) fakeslack.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r := h.slackFake.Replies(channelID, rootTS); len(r) == 1 {
			return r[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ExpectSlackThreadedReply: no threaded reply under %s in %s within %s (top-level=%d, replies=%d)",
		rootTS, channelID, timeout, len(h.slackFake.TopLevel(channelID)), len(h.slackFake.Replies(channelID, rootTS)))
	return fakeslack.Message{}
}
