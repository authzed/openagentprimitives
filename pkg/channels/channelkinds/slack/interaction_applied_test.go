package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// approvedToolCallRequest is the prompt a tool_approval interaction posts: the
// lead names the ask, the fields carry WHAT is being approved. It is the
// detail a resolved card has to keep — "approved" on its own answers none of
// the questions a reader of the thread has.
func approvedToolCallRequest() channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "agents", Name: "demo"},
		Category:        "tool_approval",
		RequestRef:      "req-tool-1",
		Lead:            "Approve this tool call?",
		Body:            "The agent wants to push to a protected branch.",
		Fields: []channelevents.InteractionField{
			{Label: "Tool", Value: "git_push"},
			{Label: "Repository", Value: "demo-org/demo-repo"},
			{Label: "Branch", Value: "master"},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
	}
}

// approvedToolCallApplied is what the pipe publishes when that prompt is
// approved. NOTE the empty OutcomeText: ApprovalDecisionHandler and
// ToolApprovalDecisionHandler both return a bare
// channelinteractions.Outcome{Result: OutcomeApproved}, so the ONLY thing a
// renderer gets for its outcome line is the wire constant "approved".
func approvedToolCallApplied() channelevents.InteractionAppliedPayload {
	return channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "agents", Name: "demo"},
		Category:        "tool_approval",
		RequestRef:      "req-tool-1",
		Outcome:         channelevents.OutcomeApproved,
		DecidedBy: &channelevents.ExternalIdentity{
			Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_APPROVER"),
		},
	}
}

// A resolved decision must render as the same house container every other
// interaction uses. It was a bare section, so a resolved approval was the one
// message in a thread that spoke no part of the shared vocabulary.
func TestBuildInteractionAppliedBlocks_RendersTheHouseContainer(t *testing.T) {
	req := approvedToolCallRequest()
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)

	require.Len(t, blocks, 1, "a resolved interaction renders as exactly one container block")
	c, ok := blocks[0].(containerBlock)
	require.True(t, ok, "expected containerBlock, got %T — a resolved card is not a bare section", blocks[0])

	require.NoError(t, c.validate(), "the rendered container must be one Slack accepts")
	assert.False(t, c.IsCollapsible)
	assert.True(t, c.HasHeaderDivider)

	require.NotNil(t, c.RichTextTitle, "the resolved card leads with a rich_text_title")
	sec, ok := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
	require.True(t, ok, "expected a RichTextSection title")
	_, isEmoji := sec.Elements[0].(*slackapi.RichTextSectionEmojiElement)
	assert.True(t, isEmoji, "the title must lead with the tone chip, like every other interaction")
}

// The regression itself: an approved tool call kept nothing about the tool
// call. The card has to still say what was approved — which is the BODY and the
// FIELDS, not the lead. The lead is the status line and becomes the outcome on
// resolution (see TitleTracksOutcome…); what was approved ON has to survive
// independently of it.
func TestBuildInteractionAppliedBlocks_KeepsWhatWasApproved(t *testing.T) {
	req := approvedToolCallRequest()
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)

	text := concatBlockText(blocks)
	assert.Contains(t, text, "The agent wants to push to a protected branch.", "the body must survive resolution")
	for _, want := range []string{"Tool", "git_push", "Repository", "demo-org/demo-repo", "Branch", "master"} {
		assert.Contains(t, text, want, "field %q must survive resolution — it IS what was approved", want)
	}
}

// tool_approval leaves OutcomeText empty, so the outcome line fell all the way
// through to the raw wire constant and the whole message became the single
// word "approved" behind a green chip.
func TestBuildInteractionAppliedBlocks_EmptyOutcomeTextDoesNotLeakTheWireConstant(t *testing.T) {
	req := approvedToolCallRequest()
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)

	text := concatBlockText(blocks)
	assert.Contains(t, text, "Approved", "an empty OutcomeText must still render a human outcome")
	assert.NotContains(t, text, "approved",
		"the lowercase wire constant must never reach a reader; it is an enum value, not copy")
}

// A resolved card names who decided it. DecidedBy is on the payload already,
// and "who approved this" is the first question a thread reader asks.
func TestBuildInteractionAppliedBlocks_NamesTheDecider(t *testing.T) {
	req := approvedToolCallRequest()
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)

	assert.Contains(t, concatBlockText(blocks), "<@U_APPROVER>",
		"a Slack decider renders as a native mention (ExternalID is the raw slack id)")
}

// containerTitleText returns the resolved card's title text, EXCLUDING the
// leading tone-chip emoji. The title is where the pending/resolved status word
// lives, so a test that the title tracks the outcome has to read it alone —
// concatBlockText folds the body in, and the body's verdict line ("Approved
// by …") would satisfy an "Approved" assertion even while the title stayed
// stale.
func containerTitleText(t *testing.T, blocks []slackapi.Block) string {
	t.Helper()
	require.Len(t, blocks, 1)
	c, ok := blocks[0].(containerBlock)
	require.True(t, ok, "expected containerBlock, got %T", blocks[0])
	require.NotNil(t, c.RichTextTitle, "the resolved card leads with a rich_text_title")
	sec, ok := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
	require.True(t, ok, "expected a RichTextSection title")
	var b strings.Builder
	for _, el := range sec.Elements {
		if te, ok := el.(*slackapi.RichTextSectionTextElement); ok {
			b.WriteString(te.Text)
		}
	}
	return b.String()
}

// The production regression: a tool_approval PROMPT leads with the status word
// "Approval needed" (host_approval.go toolApprovalLead) and the public NOTE
// leads with "Approval pending". After a decision the tone chip is recomputed
// to resolved-green, but the title kept the cached status word — so a settled
// card read "🟢 Approval needed", the chip and the title drawn from two sources
// and contradicting each other. The applied title must track the OUTCOME, the
// same source the chip does.
func TestBuildInteractionAppliedBlocks_TitleTracksOutcomeNotThePendingStatusWord(t *testing.T) {
	cases := []struct {
		name string
		lead string // the cached request's Lead — a status word in production
	}{
		{name: "prompt lead 'Approval needed' -> title 'Approved'", lead: "Approval needed"},
		{name: "public-note lead 'Approval pending' -> title 'Approved'", lead: "Approval pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := approvedToolCallRequest()
			req.Lead = tc.lead

			title := strings.TrimSpace(containerTitleText(t, buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)))
			assert.Equal(t, "Approved", title,
				"a settled card's title must be the outcome, not the pending status word")
			assert.NotContains(t, title, "pending",
				"the resolved title must not still say the interaction is pending")
			assert.NotContains(t, title, "needed",
				"the resolved title must not still say approval is needed")
		})
	}
}

// The buttons must not survive: the interaction is settled, and a live
// Approve button on a resolved card invites a click that can only be rejected.
func TestBuildInteractionAppliedBlocks_StripsTheActionButtons(t *testing.T) {
	req := approvedToolCallRequest()
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), &req)

	assert.Nil(t, interactionActionBlock(t, blocks), "a resolved card carries no action row")
}

// The delivery store is in-process, so a channelsd restart between prompt and
// decision loses the request. That degrades the DETAIL, never the SHAPE.
func TestBuildInteractionAppliedBlocks_NoCachedRequest_StillRendersTheContainer(t *testing.T) {
	blocks := buildInteractionAppliedBlocks(approvedToolCallApplied(), nil)

	require.Len(t, blocks, 1, "a resolved interaction renders as exactly one container block")
	c, ok := blocks[0].(containerBlock)
	require.True(t, ok, "expected containerBlock, got %T", blocks[0])
	require.NoError(t, c.validate(), "the degraded container must still be one Slack accepts")
	assert.Contains(t, concatBlockText(blocks), "Approved",
		"with no request to describe, the outcome headline carries the card alone")
}

// A denial reads in the degraded tone, not the resolved one, and keeps the
// reason the handler supplied.
func TestBuildInteractionAppliedBlocks_DeniedRendersDegradedToneAndReason(t *testing.T) {
	req := approvedToolCallRequest()
	p := approvedToolCallApplied()
	p.Outcome = channelevents.OutcomeDenied
	p.Reason = "pushing to master needs a change ticket"

	blocks := buildInteractionAppliedBlocks(p, &req)
	require.Len(t, blocks, 1)
	c := blocks[0].(containerBlock)
	sec := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
	emoji, ok := sec.Elements[0].(*slackapi.RichTextSectionEmojiElement)
	require.True(t, ok, "the title must lead with the tone chip")
	assert.Equal(t, toneChip(outcomeTone(channelevents.OutcomeDenied), false), emoji.Name,
		"a denial must not draw the resolved-green chip")

	text := concatBlockText(blocks)
	assert.Contains(t, text, "Denied")
	assert.Contains(t, text, "pushing to master needs a change ticket", "the denial reason must reach the reader")
}

// The store has to hand the request back so the applied edit can rebuild the
// card. Without this the renderer has nothing to describe.
func TestInteractionDeliveryStore_RoundTripsTheRequestPayload(t *testing.T) {
	s := newInteractionDeliveryStore()
	req := approvedToolCallRequest()

	_, ok := s.getRequest("req-tool-1")
	assert.False(t, ok, "no request recorded yet")

	s.recordRequest("req-tool-1", req)
	got, ok := s.getRequest("req-tool-1")
	require.True(t, ok, "a recorded request must come back")
	assert.Equal(t, req.Lead, got.Lead)
	assert.Equal(t, req.Fields, got.Fields)

	s.drop("req-tool-1")
	_, ok = s.getRequest("req-tool-1")
	assert.False(t, ok, "drop must clear the cached request with the rest of the entry")
}

// End-to-end over the sender: a click's response_url edit must carry the
// rebuilt BLOCKS. A body of `{"replace_original":true,"text":…}` with no blocks
// replaces the whole prompt with one line of text.
func TestInteractionSender_Applied_ResponseURLCarriesTheRebuiltCard(t *testing.T) {
	bodies, srv := newResponseURLServer(t)
	defer srv.Close()

	s := &interactionSender{
		client:     &fakeSlackClient{},
		httpClient: responseURLClient(srv),
		delivery:   newInteractionDeliveryStore(),
	}
	s.delivery.recordRequest("req-tool-1", approvedToolCallRequest())

	p := approvedToolCallApplied()
	p.ResponseRef = testResponseURL
	_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "ts"),
		interactionEnvelope(t, channelevents.KindInteractionApplied, p))
	require.NoError(t, err, "Send")

	got := bodies.values()
	require.Len(t, got, 1, "want one response_url POST")

	var body struct {
		ReplaceOriginal bool              `json:"replace_original"`
		Text            string            `json:"text"`
		Blocks          []json.RawMessage `json:"blocks"`
	}
	require.NoError(t, json.Unmarshal([]byte(got[0]), &body), "response_url body must be JSON")
	assert.True(t, body.ReplaceOriginal, "the edit replaces the clicker's prompt")
	require.NotEmpty(t, body.Blocks,
		"the edit must carry blocks; a text-only body is what collapsed the card to one line")
	assert.Contains(t, string(body.Blocks[0]), `"type":"container"`,
		"the replacement is the house container")
	assert.Contains(t, got[0], "git_push", "the edit must still say what was approved")
}

// The recorded-prompt-ref path (a decision resolved without a click — a
// timeout, or an approval applied out-of-band) has to rebuild the same card.
func TestInteractionSender_Applied_RecordedPromptEditCarriesTheRebuiltCard(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}
	s.delivery.record("req-tool-1", deliveryRef{ChannelID: "C01CHAN", TS: "111.222"}, deliveryRef{})
	s.delivery.recordRequest("req-tool-1", approvedToolCallRequest())

	_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "ts"),
		interactionEnvelope(t, channelevents.KindInteractionApplied, approvedToolCallApplied()))
	require.NoError(t, err, "Send")

	require.Len(t, c.updateCalls, 1, "the recorded prompt is edited in place")
	assert.Equal(t, "C01CHAN", c.updateCalls[0].channelID)
	assert.Equal(t, "111.222", c.updateCalls[0].ts)

	blocks := msgOptionBlocksJSON(t, c.updateCalls[0].opts)
	assert.Contains(t, blocks, `"type":"container"`, "the edit is the house container")
	assert.Contains(t, blocks, "git_push", "the edit must still say what was approved")
	assert.NotContains(t, blocks, `"type":"actions"`, "a resolved card carries no buttons")
}

// lureShapedToolCallRequest is the cached prompt for a tool_approval whose
// publisher slots carry a forged action: Body is the summarizer's sentence and
// the Fields carry LLM-supplied argument values, so both are reachable by an
// agent that wants a genuine-looking hyperlink on a platform-authored card.
//
// It is the SAME payload the pending card already neutralised — which is the
// whole point: the resolved card must not un-neutralise it.
func lureShapedToolCallRequest() channelevents.InteractionRequestPayload {
	p := approvedToolCallRequest()
	p.Body = "The agent wants to push to a protected branch. " + lureLink
	p.Fields = append(p.Fields, channelevents.InteractionField{Label: "Why", Value: lureLink})
	return p
}

// TestBuildInteractionAppliedBlocks_PublisherPayloadIsInert is the guard for
// the resolved card. sendRequest caches the UNESCAPED wire payload
// (recordRequest, interaction.go) and this renderer replays it through
// buildInteractionBlocks, whose body section is MarkdownType — so a Body or
// Field that the pending card had already made inert came back to life the
// moment someone clicked Approve or Deny, on all three surfaces the applied
// edit writes (the clicker's response_url copy, the recorded prompt, and the
// PUBLIC note).
func TestBuildInteractionAppliedBlocks_PublisherPayloadIsInert(t *testing.T) {
	req := lureShapedToolCallRequest()
	text := concatBlockText(buildInteractionAppliedBlocks(approvedToolCallApplied(), &req))

	assert.NotContains(t, text, lureLink,
		"a forged <url|label> in the cached Body/Fields must not render live on the RESOLVED card")
	assert.Contains(t, text, lureLinkVisible,
		"inert is not deleted — a reader must still see what was attempted")
}

// The order guard, and the reason the escape cannot simply be applied to the
// composed card: appliedVerdictLine names the decider as a `<@U…>` mention THIS
// kind composed. Escaping after composition would render "&lt;@U_APPROVER&gt;"
// and the resolved card would stop naming who decided.
func TestBuildInteractionAppliedBlocks_DeciderMentionStaysLive(t *testing.T) {
	req := lureShapedToolCallRequest()
	text := concatBlockText(buildInteractionAppliedBlocks(approvedToolCallApplied(), &req))

	assert.Contains(t, text, "<@U_APPROVER>", "the kind's own decider mention must stay clickable")
	assert.NotContains(t, text, "&lt;@U_APPROVER&gt;",
		"the escape must run BEFORE the verdict is composed, not after")
}

// The sender end of the same defect. Asserted through the sender rather than
// the renderer alone because the cache WRITE and the cache READ are in
// different functions, and because the applied edit fans the one rebuilt card
// out to three separate surfaces — including the PUBLIC note, which the whole
// conversation sees rather than the one approver.
//
// The two rows are the sender's two prompt-edit routes: a click carries a
// response_url and that copy is edited (the recorded prompt ref is then
// deliberately skipped — see sendDecisionApplied), while a decision resolved
// without a click falls back to the recorded ref. The public-note edit runs on
// both.
func TestInteractionSender_Applied_EditedSurfacesCarryTheInertPayload(t *testing.T) {
	cases := []struct {
		name         string
		withClick    bool
		wantUpdates  int
		updatesNamed string
	}{
		{
			name:         "clicked: the response_url copy and the public note are both inert",
			withClick:    true,
			wantUpdates:  1,
			updatesNamed: "the public note (the response_url edit stands in for the prompt)",
		},
		{
			name:         "no click (timeout/out-of-band): the recorded prompt and the public note are both inert",
			withClick:    false,
			wantUpdates:  2,
			updatesNamed: "the recorded prompt and the public note",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies, srv := newResponseURLServer(t)
			defer srv.Close()

			c := &fakeSlackClient{}
			s := &interactionSender{client: c, httpClient: responseURLClient(srv), delivery: newInteractionDeliveryStore()}
			s.delivery.record("req-tool-1",
				deliveryRef{ChannelID: "C01CHAN", TS: "111.222"},
				deliveryRef{ChannelID: "C01CHAN", TS: "333.444"})
			s.delivery.recordRequest("req-tool-1", lureShapedToolCallRequest())

			p := approvedToolCallApplied()
			if tc.withClick {
				p.ResponseRef = testResponseURL
			}
			_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "ts"),
				interactionEnvelope(t, channelevents.KindInteractionApplied, p))
			require.NoError(t, err, "Send")

			for i, posted := range bodies.values() {
				assert.NotContains(t, unescapeJSONHTML(t, posted), lureLink,
					"response_url POST %d must carry the inert payload", i)
			}
			require.Len(t, c.updateCalls, tc.wantUpdates, "edited surfaces: %s", tc.updatesNamed)
			for i, call := range c.updateCalls {
				assert.NotContains(t, unescapedBlocksJSON(t, call.opts), lureLink,
					"update call %d must carry the inert payload", i)
			}
		})
	}
}

// unescapeJSONHTML undoes encoding/json's default HTML escaping on a whole
// JSON document, by decoding and re-encoding it with escaping off.
//
// Without it every NotContains below passes VACUOUSLY: json.Marshal writes `<`
// as `<`, so an assertion spelled in the characters Slack actually parses
// ("<url|label>") could never match the raw document whether the payload was
// escaped or not.
func unescapeJSONHTML(t *testing.T, doc string) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(doc), &v), "decode JSON document")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(v), "re-encode without HTML escaping")
	return buf.String()
}

// msgOptionBlocksJSON recovers the marshalled blocks from a captured set of
// MsgOptions.
//
// UnsafeApplyMsgOptions cannot do this: it returns the form VALUES, and
// slack-go encodes blocks later, at request-build time, so "blocks" is simply
// absent from what it hands back. Round-tripping through a real client against
// an httptest server exercises slack-go's own encoder, which is also the thing
// a test asserting on wire shape actually wants to measure.
func msgOptionBlocksJSON(t *testing.T, opts []slackapi.MsgOption) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm(), "parse slack request form")
		got = r.Form.Get("blocks")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cli := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	_, _, _, err := cli.UpdateMessageContext(context.Background(), "C_TEST", "1.1", opts...)
	require.NoError(t, err, "replay MsgOptions through slack-go's encoder")
	return got
}
