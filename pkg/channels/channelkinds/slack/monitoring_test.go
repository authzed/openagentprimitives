package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func monitoringChannel(channelID string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:  "slack",
			Role:  spiceboxv1alpha1.ChannelRoleMonitoring,
			Slack: &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
	if channelID != "" {
		ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
	}
	return ch
}

func TestRenderMonitoringText(t *testing.T) {
	cases := []struct {
		name string
		ev   channelevents.MonitoringEvent
		want []string // substrings that must all be present
	}{
		{
			name: "error/failed: red circle, source, reason, hint",
			ev: channelevents.MonitoringEvent{
				Level: channelevents.MonitoringLevelError, Category: "credential",
				Transition: channelevents.MonitoringTransitionFailed,
				Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
				Condition:  "Refresh", Reason: "TokenEndpointError", Summary: "401 invalid_grant",
				Hint: "re-run `oap identity refresh github-bot`",
			},
			want: []string{":red_circle:", "AgentIdentity", "default/github-bot", "Refresh", "TokenEndpointError", "401 invalid_grant", "oap identity refresh github-bot"},
		},
		{
			name: "recovered: green circle",
			ev: channelevents.MonitoringEvent{
				Level: channelevents.MonitoringLevelError, Category: "credential",
				Transition: channelevents.MonitoringTransitionRecovered,
				Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
				Condition:  "Refresh", Reason: "RefreshSucceeded",
			},
			want: []string{":large_green_circle:", "recovered", "RefreshSucceeded"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMonitoringText(tc.ev)
			for _, sub := range tc.want {
				assert.Contains(t, got, sub)
			}
		})
	}
}

// TestBuildMonitoringBlocks_ContainerShape pins that a monitoring event now
// renders as the shared container card — tone chip + bold-code source ref in
// the title, the condition/reason/summary/hint in a section, and the machine
// metadata in a muted context footer — not the old flat mrkdwn line.
func TestBuildMonitoringBlocks_ContainerShape(t *testing.T) {
	ev := channelevents.MonitoringEvent{
		Level: channelevents.MonitoringLevelWarning, Category: "reconcile",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "MCPServer", Namespace: "default", Name: "hubspot-companies"},
		Condition:  "Valid", Reason: "WildcardLeafNotRouted",
		Summary: "tools[search_crm_objects].permissionVariants[0].check: reachable through the wildcard relation",
		Hint:    "set routeViaSessionGrant: true",
	}

	blocks := buildMonitoringBlocks(ev)

	// Exactly one top-level block, and it is the container.
	require.Len(t, blocks, 1, "a monitoring event renders as a single container block")
	cont, ok := blocks[0].(containerBlock)
	require.True(t, ok, "the top-level block must be the container, got %T", blocks[0])
	require.NoError(t, cont.validate(), "the container must be one Slack accepts")

	// Title: chip + bold-code source ref.
	require.NotNil(t, cont.RichTextTitle, "the card leads with a rich_text_title")
	title := richTextPlain(cont.RichTextTitle)
	assert.Contains(t, title, ":large_orange_circle:", "warning tone draws the orange circle chip")
	assert.Contains(t, title, "MCPServer default/hubspot-companies", "the source ref is the lead")

	// Body section: condition, reason, summary, hint — all present, section-tier.
	text := concatBlockText(blocks)
	assert.Contains(t, text, "*Valid*: WildcardLeafNotRouted", "condition and reason lead the body")
	assert.Contains(t, text, "> tools[search_crm_objects]", "the summary rides a blockquote")
	assert.Contains(t, text, "_set routeViaSessionGrant: true_", "the hint is italicised")

	// Muted footer carries the machine metadata the chip and body do not.
	last, ok := cont.ChildBlocks[len(cont.ChildBlocks)-1].(*slackapi.ContextBlock)
	require.True(t, ok, "the final child is the muted context footer, got %T", cont.ChildBlocks[len(cont.ChildBlocks)-1])
	footer := ""
	for _, e := range last.ContextElements.Elements {
		if tb, ok := e.(*slackapi.TextBlockObject); ok {
			footer += tb.Text
		}
	}
	assert.Equal(t, "warning · failed · reconcile", footer, "footer is the muted level · transition · category line")
}

// TestBuildMonitoringBlocks_Recovered pins the good-news path: a recovered
// event outranks its level and draws the green circle, so a red level does not
// paint a recovery red.
func TestBuildMonitoringBlocks_Recovered(t *testing.T) {
	blocks := buildMonitoringBlocks(channelevents.MonitoringEvent{
		Level: channelevents.MonitoringLevelError, Category: "credential",
		Transition: channelevents.MonitoringTransitionRecovered,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh", Reason: "RefreshSucceeded",
	})
	require.Len(t, blocks, 1)
	cont := blocks[0].(containerBlock)
	assert.Contains(t, richTextPlain(cont.RichTextTitle), ":large_green_circle:",
		"a recovery is good news and draws green even at error level")
}

// TestBuildMonitoringBlocks_ClusterScopedSource: a source with no namespace
// renders "kind name", never a dangling "kind /name".
func TestBuildMonitoringBlocks_ClusterScopedSource(t *testing.T) {
	blocks := buildMonitoringBlocks(channelevents.MonitoringEvent{
		Level: channelevents.MonitoringLevelWarning, Category: "reconcile",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "SpiceboxClass", Name: "cluster-defaults"},
		Condition:  "Valid",
	})
	title := richTextPlain(blocks[0].(containerBlock).RichTextTitle)
	assert.Contains(t, title, "SpiceboxClass cluster-defaults")
	assert.NotContains(t, title, "SpiceboxClass /cluster-defaults", "no dangling slash for a cluster-scoped source")
}

// TestBuildMonitoringBlocks_RendersInjectedMarkupInert is the container-side
// twin of TestRenderMonitoringText_RendersInjectedMarkupInert: the card must
// not become a new injection sink. The source ref rides a rich_text element
// (Slack renders it literally), and every mrkdwn field is escaped, so a lure in
// any publisher-supplied field survives as visible characters, never live
// markup.
func TestBuildMonitoringBlocks_RendersInjectedMarkupInert(t *testing.T) {
	const lure = "<https://attacker.example.invalid/update|Update credential>"
	cases := []struct {
		name string
		ev   channelevents.MonitoringEvent
	}{
		{"Summary", channelevents.MonitoringEvent{
			Level: channelevents.MonitoringLevelError, Category: "credential",
			Transition: channelevents.MonitoringTransitionFailed,
			Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
			Condition:  "CredentialUpdatePending", Summary: "The agent said: " + lure,
		}},
		{"Reason", channelevents.MonitoringEvent{
			Level: channelevents.MonitoringLevelError, Category: "credential",
			Transition: channelevents.MonitoringTransitionFailed,
			Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
			Condition:  "CredentialUpdatePending", Reason: lure,
		}},
		{"Category", channelevents.MonitoringEvent{
			Level: channelevents.MonitoringLevelError, Category: lure,
			Transition: channelevents.MonitoringTransitionFailed,
			Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
			Condition:  "CredentialUpdatePending",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := blocksJSON(t, buildMonitoringBlocks(tc.ev))
			assert.NotContains(t, raw, "<https://",
				"no publisher-supplied field may open a Slack link span in the card")
			assert.Contains(t, raw, "attacker.example.invalid",
				"the attempt must still be visible as inert characters")
		})
	}
}

func TestMonitoringSender_SendMonitoring(t *testing.T) {
	ev := channelevents.MonitoringEvent{
		Level: channelevents.MonitoringLevelError, Category: "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh", Reason: "TokenEndpointError",
	}

	t.Run("happy path: posts the container card to the configured channel ID", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &monitoringSender{deps: channelkinds.Deps{Channel: monitoringChannel("C_MON")}, client: fc}
		require.NoError(t, s.SendMonitoring(context.Background(), ev))
		require.Len(t, fc.postMessageCalls, 1)
		assert.Equal(t, "C_MON", fc.postMessageCalls[0].channelID)
		// The wire payload carries the container block, not just the flat text —
		// a regression back to MsgOptionText-only would drop the card silently.
		assert.Contains(t, msgOptionBlocksJSON(t, fc.postMessageCalls[0].options), `"type":"container"`,
			"SendMonitoring must post the container card as the primary rendering")
	})

	t.Run("missing channel ID: returns error, posts nothing", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &monitoringSender{deps: channelkinds.Deps{Channel: monitoringChannel("")}, client: fc}
		err := s.SendMonitoring(context.Background(), ev)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "channelId")
		assert.Empty(t, fc.postMessageCalls)
	})

	t.Run("nil client (bot-token missing): returns error", func(t *testing.T) {
		s := &monitoringSender{deps: channelkinds.Deps{Channel: monitoringChannel("C_MON")}, client: nil}
		assert.Error(t, s.SendMonitoring(context.Background(), ev))
	})
}

func TestKind_NewMonitoringSender_NotNil(t *testing.T) {
	s := (&Kind{}).NewMonitoringSender(channelkinds.Deps{Channel: monitoringChannel("C_MON")})
	assert.NotNil(t, s, "slack kind must return a MonitoringSender")
}

// TestKind_SupportsMonitoringMatchesNewMonitoringSender pins the two halves of
// the one fact channelkinds.Kind splits across two methods. Callers without a
// resolved Secret (the credential-update watcher's deliverability pre-check)
// can only see SupportsMonitoring, and they decide whether an admin is
// reachable AT ALL from it -- so a kind whose two answers disagree makes that
// watcher claim a delivery nobody received.
func TestKind_SupportsMonitoringMatchesNewMonitoringSender(t *testing.T) {
	k := &Kind{}
	assert.True(t, k.SupportsMonitoring(), "slack does deliver monitoring events")
	assert.NotNil(t, k.NewMonitoringSender(channelkinds.Deps{Channel: monitoringChannel("C_MON")}),
		"SupportsMonitoring()==true obliges NewMonitoringSender to return a real sender")
}

// TestRenderMonitoringText_RendersInjectedMarkupInert is the review's
// Important 4. A MonitoringEvent's Summary carries text the platform did not
// author -- a provider's raw HTTP response, an upstream MCP server's tool
// name, and (for the credential-update card) the AGENT's own explanation.
// SendMonitoring posts with MsgOptionText(..., false), i.e. mrkdwn is
// INTERPRETED, so unescaped `<url|label>` renders as a real hyperlink inside
// the blockquote -- one line above the genuine remediation hint, on the
// channel that is now a shared-bot-token entry surface.
//
// Mutation sensitivity: drop escapeMonitoringText from the Summary
// interpolation and the first assertion fails immediately.
func TestRenderMonitoringText_RendersInjectedMarkupInert(t *testing.T) {
	const lure = "<https://attacker.example.invalid/update|Update credential>"

	cases := []struct {
		name string
		ev   channelevents.MonitoringEvent
	}{
		{
			name: "agent-authored Summary cannot forge a link",
			ev: channelevents.MonitoringEvent{
				Level: channelevents.MonitoringLevelError, Category: "credential",
				Transition: channelevents.MonitoringTransitionFailed,
				Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
				Condition:  "CredentialUpdatePending",
				Summary:    "The agent said: " + lure,
			},
		},
		{
			name: "provider-derived Reason cannot forge a link",
			ev: channelevents.MonitoringEvent{
				Level: channelevents.MonitoringLevelError, Category: "credential",
				Transition: channelevents.MonitoringTransitionFailed,
				Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
				Condition:  "CredentialUpdatePending",
				Reason:     lure,
			},
		},
		{
			name: "an upstream-chosen Source.Name cannot forge a link",
			ev: channelevents.MonitoringEvent{
				Level: channelevents.MonitoringLevelError, Category: "credential",
				Transition: channelevents.MonitoringTransitionFailed,
				Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: lure},
				Condition:  "CredentialUpdatePending",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMonitoringText(tc.ev)
			assert.NotContains(t, got, lure,
				"the raw <url|label> must never survive into the posted text -- Slack renders it as a hyperlink")
			assert.NotContains(t, got, "<https://",
				"no publisher-supplied field may open a Slack link span")
			assert.Contains(t, got, "&lt;https://attacker.example.invalid/update|Update credential&gt;",
				"it must still be VISIBLE, as the literal characters, so a reader sees what was attempted")
		})
	}
}

// TestEscapeSlackText pins the escape set AND the single-pass property that
// makes it order-independent: a strings.Replacer never re-scans its own
// output, so "&lt;" becomes "&amp;lt;" rather than "&amp;amp;lt;". A
// hand-rolled ReplaceAll chain only behaves this way when "&" happens to run
// first, which is why this package has exactly one escaper.
//
// It also pins what is deliberately NOT escaped: publishers compose backticks,
// `*` and `_` on purpose (toolApprovalFields backtick-wraps the tool name),
// and escaping those would mangle real cards for no security gain.
func TestEscapeSlackText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"link syntax: both angle brackets escaped", "<a|b>", "&lt;a|b&gt;"},
		{"channel-wide ping cannot survive", "<!channel>", "&lt;!channel&gt;"},
		{"bare ampersand escaped once", "tom & jerry", "tom &amp; jerry"},
		{"single pass: a pre-existing entity is escaped, never re-scanned", "&lt;", "&amp;lt;"},
		{"deliberate publisher markup is left live", "Tool `git_push` is _required_ and *bold*", "Tool `git_push` is _required_ and *bold*"},
		{"plain text untouched", "401 Unauthorized -- Bad credentials", "401 Unauthorized -- Bad credentials"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, escapeSlackText(tc.in))
		})
	}
}

// TestInertExcerptUsesTheSharedEscaper: the excerpt path adds fence-breaking
// protection on top of the shared escaper, and must not have drifted into a
// second implementation of the escape itself.
func TestInertExcerptUsesTheSharedEscaper(t *testing.T) {
	assert.Equal(t, escapeSlackText("<a|b>"), inertExcerpt("<a|b>"),
		"with no code fence to break, inertExcerpt must equal the shared escaper exactly")
	assert.NotContains(t, inertExcerpt("```\n<!here>"), "```",
		"...while still neutralizing fence breakers, which is the only thing it adds")
}

// TestRenderMonitoringText_KeepsItsOwnStructuralMarkup: escaping must not eat
// the markup renderMonitoringText itself writes, or every monitoring post
// degrades to one unreadable line.
func TestRenderMonitoringText_KeepsItsOwnStructuralMarkup(t *testing.T) {
	got := renderMonitoringText(channelevents.MonitoringEvent{
		Level: channelevents.MonitoringLevelError, Category: "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "demo-bot"},
		Condition:  "Refresh",
		Summary:    "first line\nsecond line",
		Hint:       "do the thing",
	})
	assert.Contains(t, got, "\n> first line", "the blockquote prefix this function writes must survive")
	assert.Contains(t, got, "\n> second line", "including on continuation lines")
	assert.Contains(t, got, "_do the thing_", "the italic hint wrapper is ours, not the publisher's")
	assert.Contains(t, got, "`AgentIdentity default/demo-bot`", "the code span is ours too")
}
