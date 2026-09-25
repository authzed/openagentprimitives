// pkg/channels/channelkinds/slack/interaction_inertness_test.go
//
// Markup inertness for the IN-THREAD interaction card, the sibling of
// monitoring_test.go's coverage for the monitoring surface. Both surfaces
// render publisher text as mrkdwn, so both must render it inert; closing one
// and leaving the other open just moves the hole.
//
// The concrete card this protects is credential_update's: its Lead IS a
// provider's raw HTTP response body, its Body IS the agent's own sentence, and
// one of its Fields names an upstream-chosen tool -- and the card's single
// action is a credential-ENTRY button. A `<url|label>` reaching the card's
// mrkdwn section renders as a genuine-looking link one line from that button.
//
// Two mechanisms hold, and the tests below pin each: Body / NextStep / Fields
// are ESCAPED (escapePublisherPayload), and the Lead is inert STRUCTURALLY on
// the CARD because its sink there is a rich_text element Slack renders
// literally.
//
// Structural inertness covers the card and nothing else. The same Lead reaches
// two sinks Slack DOES parse -- the message's plain-text `text` field and the
// degraded mrkdwn section -- and each escapes it itself; those are pinned in
// interaction_lead_text_test.go. Reading this file as "the Lead is inert
// everywhere" is what left both of them open.
package slack

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// lureLink is the payload an agent (or a hostile upstream) would use to forge
// a platform-authored action.
const lureLink = "<https://attacker.example.invalid/update|Update credential>"

// lureLinkVisible is what a reader must still be able to read after the sweep,
// and the form every "inert is not deleted" assertion in this package makes.
//
// It is a SUBSTRING rather than the whole escaped span on purpose. Asserting
// "&lt;https://…|Update credential&gt;" verbatim measures the ESCAPER'S
// REPRESENTATION, not whether the reader can still see what was claimed — so it
// breaks the moment a second, independent guard also touches the string, which
// is exactly what happened when defuseBareLinks joined escapeSlackText: the
// escaped span is now additionally code-spanned around its URL run
// ("&lt;`https://…|Update` credential&gt;"), which renders the same characters
// and reads the same way. App Home's own guard was corrected to this shape when
// it took the sweep first (f3395f15); these are its siblings.
const lureLinkVisible = "attacker.example.invalid/update"

// interactionSectionText renders p and returns the container's body section
// text -- the one mrkdwn surface on the card, carrying Body, NextStep and
// Fields. The Lead is NOT here: it rides in the rich_text_title, which is
// covered by TestBuildInteractionRequestBlocks_LeadIsInertByStructure.
func interactionSectionText(t *testing.T, p channelevents.InteractionRequestPayload) string {
	t.Helper()
	c := firstContainer(t, buildInteractionRequestBlocks(p, "default/sess-inert"))
	for _, child := range c.ChildBlocks {
		if section, ok := child.(*slackapi.SectionBlock); ok && section.Text != nil {
			return section.Text.Text
		}
	}
	require.Fail(t, "no section child carrying the body")
	return ""
}

// interactionLeadElement renders p and returns the rich_text_title element
// carrying the Lead.
func interactionLeadElement(t *testing.T, p channelevents.InteractionRequestPayload) *slackapi.RichTextSectionTextElement {
	t.Helper()
	els := titleElements(t, firstContainer(t, buildInteractionRequestBlocks(p, "default/sess-inert")))
	require.Len(t, els, 3, "chip, spacer, lead")
	lead, ok := els[2].(*slackapi.RichTextSectionTextElement)
	require.True(t, ok, "the lead element must be a rich_text text element, got %T", els[2])
	return lead
}

// credentialUpdateShapedPayload mirrors the card
// pkg/channels/channelsd/pipeline/credential_update.go publishes in-thread: a platform
// verdict Lead, an attributed agent Body, label/value fields, and exactly one
// link action.
func credentialUpdateShapedPayload(mutate func(*channelevents.InteractionRequestPayload)) channelevents.InteractionRequestPayload {
	p := channelevents.InteractionRequestPayload{
		Category:   "credential_update",
		RequestRef: "ref-inert",
		Lead:       "The provider rejected this token: 401 Unauthorized",
		Body:       "The agent said: my push kept failing so I assumed the token expired",
		Fields: []channelevents.InteractionField{
			{Label: "Identity", Value: "AgentIdentity demo-agent-identity"},
			{Label: "Credential", Value: "github-pat"},
			{Label: "Tool", Value: "github__create_issue"},
		},
		Actions: []channelevents.InteractionAction{{
			ID: "update_credential", Label: "Update credential",
			Kind: channelevents.ActionKindLink, URL: "https://agent.example.invalid/link?d=x&sig=y",
		}},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ADMIN"},
		},
	}
	mutate(&p)
	return p
}

// TestBuildInteractionRequestBlocks_RendersInjectedMarkupInert is the in-thread
// half of the injection guard. Every publisher-supplied slot that reaches the
// card's mrkdwn section must render a forged link as literal characters.
//
// Mutation sensitivity: drop any one of escapePublisherPayload's
// interpolations and exactly the matching row fails.
func TestBuildInteractionRequestBlocks_RendersInjectedMarkupInert(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*channelevents.InteractionRequestPayload)
	}{
		{
			name: "agent-authored Body cannot forge a link",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Body = "The agent said: click here " + lureLink
			},
		},
		{
			name: "a publisher NextStep cannot forge a link",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.NextStep = "Do it here: " + lureLink
			},
		},
		{
			name: "an upstream-chosen field VALUE cannot forge a link",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Fields[2].Value = lureLink
			},
		},
		{
			name: "a field LABEL cannot forge a link",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Fields[0].Label = lureLink
			},
		},
		{
			name: "a channel-wide ping cannot be smuggled in",
			mutate: func(p *channelevents.InteractionRequestPayload) {
				p.Body = "The agent said: <!channel> everyone look"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interactionSectionText(t, credentialUpdateShapedPayload(tc.mutate))
			assert.NotContains(t, got, lureLink,
				"the raw <url|label> must never survive -- Slack renders it as a hyperlink one line from the real action button")
			assert.NotContains(t, got, "<https://",
				"no publisher-supplied slot may open a Slack link span")
			assert.NotContains(t, got, "<!channel>",
				"no publisher-supplied slot may open a channel-wide ping")
		})
	}
}

// TestBuildInteractionRequestBlocks_InjectedMarkupStaysVisible: inert is not
// the same as deleted. A reader must still SEE what was attempted, or the
// escaping hides an attack instead of defusing it.
func TestBuildInteractionRequestBlocks_InjectedMarkupStaysVisible(t *testing.T) {
	got := interactionSectionText(t, credentialUpdateShapedPayload(func(p *channelevents.InteractionRequestPayload) {
		p.Body = lureLink
	}))
	assert.Contains(t, got, lureLinkVisible,
		"the attempt must render as the literal characters a human can read")
	assert.Contains(t, got, "&lt;", "...including the angle bracket the escape defanged")
}

// TestBuildInteractionRequestBlocks_LeadIsInertByStructure covers the one
// publisher slot escapePublisherPayload deliberately leaves alone. The Lead's
// sink ON THE CARD is the container's rich_text_title, whose text elements
// Slack renders as literal characters rather than parsing as mrkdwn -- so a
// forged link there cannot become a link, and escaping it would instead show a
// reader "&amp;" for an ordinary ampersand in a title.
//
// The guard is therefore structural, and this test is what holds it: the Lead
// must reach a rich_text element and must NOT reach the mrkdwn section. It says
// nothing about the Lead's OTHER sinks -- see this file's header.
func TestBuildInteractionRequestBlocks_LeadIsInertByStructure(t *testing.T) {
	p := credentialUpdateShapedPayload(func(p *channelevents.InteractionRequestPayload) {
		p.Lead = lureLink
	})

	lead := interactionLeadElement(t, p)
	assert.Equal(t, lureLink, lead.Text,
		"the lead rides a rich_text element verbatim, where Slack does not parse mrkdwn")
	require.NotNil(t, lead.Style)
	assert.True(t, lead.Style.Bold, "the lead's emphasis is a rich_text style, not `*` markup a publisher could close")

	assert.NotContains(t, interactionSectionText(t, p), lureLink,
		"the lead must never reach the mrkdwn section, where the same string WOULD render as a hyperlink")
}

// TestBuildInteractionRequestBlocks_KeepsPublisherMarkupAndItsOwnStructure is
// the false-positive guard: escaping must not have become a blanket that
// mangles real cards. Publishers deliberately compose backticks, `*` and `_`
// (toolApprovalFields backtick-wraps the tool name; the
// "_(no justification provided)_" fallback italicizes), and the renderer's own
// bullets + bolded labels are ITS markup, not the publisher's.
func TestBuildInteractionRequestBlocks_KeepsPublisherMarkupAndItsOwnStructure(t *testing.T) {
	got := interactionSectionText(t, credentialUpdateShapedPayload(func(p *channelevents.InteractionRequestPayload) {
		p.Body = "_(no justification provided)_"
		p.Fields = []channelevents.InteractionField{
			{Label: "Tool", Value: "`git_push`"},
			{Label: "Permission", Value: "`write`"},
		}
	}))
	assert.Contains(t, got, "_(no justification provided)_", "publisher italics must stay live")
	assert.Contains(t, got, "• *Tool*: `git_push`", "publisher code spans and our bullet/label markup must both survive")
	assert.Contains(t, got, "• *Permission*: `write`")
}

// TestBuildInteractionRequestBlocks_ResolvedMentionsStayLive is the OTHER
// false-positive guard, and the reason field Values are not escaped
// unconditionally. resolveFieldMentions replaces a Mentions field's Value with
// `<@U…>` markup THIS kind composed; escaping it again would render
// "&lt;@U123&gt;" and info_leakage's "Would share with" row -- whose whole
// invariant is that the data owner sees exactly WHO -- would name nobody
// clickably.
func TestBuildInteractionRequestBlocks_ResolvedMentionsStayLive(t *testing.T) {
	got := interactionSectionText(t, credentialUpdateShapedPayload(func(p *channelevents.InteractionRequestPayload) {
		// The shape resolveFieldMentions produces: Value already carries the
		// composed mention, and Mentions is non-empty.
		p.Fields = []channelevents.InteractionField{{
			Label:    "Would share with",
			Value:    "<@U_OWNER>",
			Mentions: []channelevents.ExternalIdentity{{Kind: "slack", Subject: identity.Subject("user:owner")}},
		}}
	}))
	assert.Contains(t, got, "<@U_OWNER>",
		"a resolved mention is markup this kind composed and must render live")
	assert.NotContains(t, got, "&lt;@U_OWNER&gt;",
		"double-escaping a resolved mention would stop the data owner seeing who is named")
}

// TestResolveFieldMentions_EscapesTheFallbackItEmbeds closes the seam the
// exemption above opens: for a Mentions field escapePublisherPayload must not
// escape, so this function is the last place its publisher-supplied fallback
// text can be made inert. A mention that fails to resolve falls back to that
// text -- and an unescaped fallback would reach the card live, through the one
// Value the escaper deliberately leaves alone.
func TestResolveFieldMentions_EscapesTheFallbackItEmbeds(t *testing.T) {
	fields := []channelevents.InteractionField{{
		Label: "Would share with",
		Value: lureLink,
		// Kind != "slack" takes the fallback path with no API call.
		Mentions: []channelevents.ExternalIdentity{{Kind: "teams", Subject: identity.Subject("user:elsewhere")}},
	}}
	out := resolveFieldMentions(context.Background(), nil, fields, logr.Discard(), "ref-inert")
	require.Len(t, out, 1)
	assert.NotContains(t, out[0].Value, lureLink,
		"an unresolved mention's fallback is publisher text and must be inert before it reaches the renderer")
	assert.Contains(t, out[0].Value, lureLinkVisible,
		"...and still visible")
}
