// pkg/channels/channelkinds/slack/metaagent_inert_test.go
//
// The metaagent scope-approval card is this package's highest-exposure
// untrusted-text surface after the public note, and until now it had no
// escaping at all: Verbatim is the user's own Slack message, and the three
// *Explain / ApproverSummary fields are an LLM's prose about it. The card is
// posted NON-ephemerally into the thread, one line above the real Approve /
// Deny buttons.
package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// metaagentLure is a scope-approval payload whose every publisher-supplied
// slot carries forged markup.
//
// Verbatim is `stripMetaagentMention(ev.Text, …)` — the raw text of a Slack
// message the requester typed, with only the bot mention removed
// (metaagent_listener.go). ApproverSummary / SkippedExplain / CaveatExplain /
// CleanedTask are the composer LLM's output over that same text. So every one
// of them is chosen, directly or by prompt, by the person asking for the scope
// change — and rendered to the person deciding whether to grant it.
func metaagentLure() scope.MetaagentApprovalPayload {
	return scope.MetaagentApprovalPayload{
		RequestID:       "req-lure",
		Requester:       "U_ASKER",
		Verbatim:        "please widen scope " + lureLink,
		ApproverSummary: "adds read on X " + lureLink,
		SkippedExplain:  "skipped one thing " + lureLink,
		CaveatExplain:   "with a caveat " + lureLink,
		CleanedTask:     "summarize the board " + lureLink,
	}
}

// TestBuildMetaagentScopeApprovalBlocks_PublisherTextIsInert is the guard for
// the card itself. Every region is a MarkdownType section, so a
// `<url|label>` in any of them renders as a REAL hyperlink on a card the
// reader is being asked to trust enough to click Approve on.
func TestBuildMetaagentScopeApprovalBlocks_PublisherTextIsInert(t *testing.T) {
	cases := []struct {
		name      string
		coldStart bool
	}{
		{name: "mid-session card: no region renders forged markup", coldStart: false},
		{name: "cold-start card: the cleaned-task region does not either", coldStart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl := metaagentLure()
			pl.ColdStart = tc.coldStart
			text := extractBlockText(buildMetaagentScopeApprovalBlocks(pl, "default/sess1"))

			assert.NotContains(t, text, lureLink,
				"no publisher-supplied region may render a forged action beside the real buttons")
			assert.Contains(t, text, lureLinkVisible,
				"inert is not deleted — the approver must see what was actually asked for")
		})
	}
}

// TestBuildMetaagentScopeApprovalBlocks_RequesterMentionStaysLive is the
// order guard and the false-positive guard in one. The `<@U…>` prefix on the
// mid-session card is markup THIS kind composes, and it is the only thing
// telling the approver who asked. Escaping the composed region instead of the
// publisher's text would render "&lt;@U_ASKER&gt;" and the card would stop
// naming anyone.
func TestBuildMetaagentScopeApprovalBlocks_RequesterMentionStaysLive(t *testing.T) {
	text := extractBlockText(buildMetaagentScopeApprovalBlocks(metaagentLure(), "default/sess1"))

	assert.Contains(t, text, "<@U_ASKER>", "the kind's own requester mention must stay clickable")
	assert.NotContains(t, text, "&lt;@U_ASKER&gt;",
		"the escape must run BEFORE the mention is composed, not after")
}

// TestMetaagentScopeApprovalSender_NotificationPreviewIsInert covers the
// card's OTHER sink, which is a separate code path from the blocks and was
// missed by every escaping sweep for the same reason the Lead sinks were: the
// preview is a plain-text field, and MsgOptionText(_, false) means slack-go
// does not escape it either. It is what a push notification and the channel
// list show.
func TestMetaagentScopeApprovalSender_NotificationPreviewIsInert(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &metaagentScopeApprovalSender{client: fc, refs: newMetaagentApprovalRefCache()}

	_, err := s.Send(context.Background(),
		metaagentSession(map[string]string{"channel_id": "C1", "thread_ts": "111.1"}, nil),
		approvalEnvelope(t, metaagentLure()))
	require.NoError(t, err, "Send")
	require.Len(t, fc.postMessageCalls, 1)

	preview := renderTextFromOpts(t, fc.postMessageCalls[0].options)
	assert.NotContains(t, preview, lureLink, "the notification preview must not render forged markup")
	assert.Contains(t, preview, "<@U_ASKER>", "the kind's own requester mention stays live in the preview")
}

// TestRenderMetaagentShowDetailsText_IsInert covers the Show Details
// ephemeral, which re-renders the SAME fields from the ref cache (or, after a
// restart, from the durable metaagent_audit record) through a second,
// independent renderer. A card made inert whose "Show Details" is not is the
// shape of defect this package keeps finding: one escaped rendering beside one
// live one, for the same payload.
func TestRenderMetaagentShowDetailsText_IsInert(t *testing.T) {
	pl := metaagentLure()
	got := renderMetaagentShowDetailsText(MetaagentApprovalRef{
		RequestID:       pl.RequestID,
		Requester:       pl.Requester,
		Verbatim:        pl.Verbatim,
		ApproverSummary: pl.ApproverSummary,
		CleanedTask:     pl.CleanedTask,
	})

	assert.NotContains(t, got, lureLink, "Show Details must not re-liven what the card made inert")
	assert.Contains(t, got, "<@U_ASKER>", "the kind's own 'Requested by' mention stays live")
}

// TestRenderMetaagentColdStartResolution_IsInert covers the PERMANENT record.
// A cold-start card is ephemeral to the starter, so this in-thread message is
// the only channel-visible trace of what was decided — it outlives the card,
// and it quotes the request text back verbatim.
func TestRenderMetaagentColdStartResolution_IsInert(t *testing.T) {
	pl := metaagentLure()
	ref := MetaagentApprovalRef{
		RequestID:       pl.RequestID,
		Requester:       pl.Requester,
		Verbatim:        pl.Verbatim,
		ApproverSummary: pl.ApproverSummary,
		CleanedTask:     pl.CleanedTask,
		ColdStart:       true,
	}
	for _, decision := range []string{
		MetaagentDecisionApproveCleaned,
		MetaagentDecisionApproveOriginal,
		MetaagentDecisionDeny,
	} {
		t.Run(decision+": the permanent record carries no live markup", func(t *testing.T) {
			got := renderMetaagentColdStartResolution(ref, decision)
			assert.NotContains(t, got, lureLink)
		})
	}
}

// TestMetaagentNoticeSender_BodyIsInert. Unlike the card, nothing
// attacker-controlled reaches this body TODAY — every publisher composes it
// from an in-process literal. It is escaped for the same per-sink reason the
// notice Lead is (inert.go): the sink is mrkdwn, escaping it is lossless, and
// a body slot on a wire payload that ANY publisher can fill should not depend
// on all of them continuing to pass literals.
func TestMetaagentNoticeSender_BodyIsInert(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &metaagentNoticeSender{client: fc}

	_, err := s.Send(context.Background(),
		metaagentSession(map[string]string{"channel_id": "C1"}, nil),
		noticeEnvelope(t, channelkinds.MetaagentNoticePayload{
			Requester: "U_ASKER", Body: "scope denied " + lureLink,
		}))
	require.NoError(t, err, "Send")
	require.Len(t, fc.postEphemeralCalls, 1)

	assert.NotContains(t, renderTextFromOpts(t, fc.postEphemeralCalls[0].options), lureLink)
}
