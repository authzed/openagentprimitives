package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// namerClient is the users.info half of fakeSlackClient, seeded by id rather
// than by email: renderSlackEntities resolves the id form a message carries,
// where every other resolver in this package starts from a canonical.
func namerClient(byID map[string]string) *fakeSlackClient {
	f := &fakeSlackClient{userInfo: map[string]*slackapi.User{}}
	for id, name := range byID {
		f.userInfo[id] = &slackapi.User{ID: id, RealName: name}
	}
	return f
}

// TestRenderSlackEntities is the display half of messagecontent.go's ingest
// encoding: flattenRichTextSection writes Slack's own wire form for a mention
// into MessageText, and this reads it back out for a human.
//
// Every case pins the same property from a different angle — the output is
// PLAIN TEXT, never markup. A resolved name is "@Dana Whitfield", not
// "<@U_DANA>": the string lands in an untrusted region, and re-emitting live
// mention syntax there would let anyone whose text we quote ping a person, or
// a channel, from inside an approval card.
func TestRenderSlackEntities(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		users map[string]string
		want  string
	}{
		{
			name:  "user mention resolves to a plain-text name",
			in:    "<@U_BOT>\ncan you share the performance numbers?",
			users: map[string]string{"U_BOT": "sre-bot"},
			want:  "@sre-bot\ncan you share the performance numbers?",
		},
		{
			name:  "unresolvable user degrades to the bare id, still inert",
			in:    "ask <@U_GHOST> about it",
			users: nil,
			want:  "ask @U_GHOST about it",
		},
		{
			name:  "the legacy piped form prefers the label Slack already cached",
			in:    "<@U_DANA|dana> hi",
			users: nil,
			want:  "@dana hi",
		},
		{
			name:  "a channel ref carries its own label — no lookup needed",
			in:    "see <#C_INC|incidents>",
			users: nil,
			want:  "see #incidents",
		},
		{
			name:  "broadcasts render as the word, never as a live ping",
			in:    "<!here> and <!channel>",
			users: nil,
			want:  "@here and @channel",
		},
		{
			name:  "repeated mentions of one user cost one lookup",
			in:    "<@U_DANA> and <@U_DANA>",
			users: map[string]string{"U_DANA": "Dana Whitfield"},
			want:  "@Dana Whitfield and @Dana Whitfield",
		},
		{
			name:  "text with no entities is returned untouched",
			in:    "just a sentence with an email a@b.example",
			users: nil,
			want:  "just a sentence with an email a@b.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := namerClient(tc.users)
			got := renderSlackEntities(context.Background(), c, tc.in, logr.Discard(), "ref-1")
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "<@", "the output must never carry live mention markup")
		})
	}
}

// TestRenderSlackEntities_OneLookupPerDistinctUser: the memoisation is not a
// nicety. A card's excerpt is untrusted text, so the token count is chosen by
// whoever wrote the message — without a cache, N repeats of one id are N
// users.info calls against the workspace's rate limit.
func TestRenderSlackEntities_OneLookupPerDistinctUser(t *testing.T) {
	c := namerClient(map[string]string{"U_A": "Ana", "U_B": "Bo"})
	in := strings.Repeat("<@U_A> <@U_B> ", 20)

	got := renderSlackEntities(context.Background(), c, in, logr.Discard(), "ref-2")

	assert.Equal(t, strings.Repeat("@Ana @Bo ", 20), got)
	assert.Equal(t, 2, len(c.userInfoCalls), "one users.info call per DISTINCT id, however often it repeats")
}

// TestRenderSlackEntities_LookupBudget caps how many DISTINCT ids one string
// may resolve. The excerpt is attacker-influenceable — a tool result, a
// quoted message — so an unbounded fan-out turns one card into hundreds of
// users.info calls. Over the cap a token degrades to its bare id, the same
// place a failed lookup lands: the budget bounds the API cost, it never
// weakens the inertness invariant.
func TestRenderSlackEntities_LookupBudget(t *testing.T) {
	users := map[string]string{}
	var b strings.Builder
	for i := 0; i < mentionLookupBudget+5; i++ {
		id := "U_" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		users[id] = "name" + id
		b.WriteString("<@" + id + "> ")
	}
	c := namerClient(users)

	got := renderSlackEntities(context.Background(), c, b.String(), logr.Discard(), "ref-3")

	assert.LessOrEqual(t, len(c.userInfoCalls), mentionLookupBudget,
		"the budget bounds users.info calls per string")
	assert.NotContains(t, got, "<@",
		"tokens past the budget degrade to the bare id — never back to live markup")
}

// TestInteractionSender_Request_ExcerptMentionsRenderAsNames is the wiring
// proof: a join card's Excerpt reaches the approver naming a person, not a
// user id.
//
// It asserts over the SENDER'S OWN COMPOSITION — resolveExcerptMentions feeding
// buildInteractionRequestBlocks, in that order — rather than over the blocks the
// fake client received, because slack-go's UnsafeApplyMsgOptions drops blocks
// and neither fake in this package can see them (fakeslack/README.md). Send()
// is still driven, and c.userInfoCalls is what proves the resolution happened
// inside it rather than only in this test.
func TestInteractionSender_Request_ExcerptMentionsRenderAsNames(t *testing.T) {
	const (
		ownerEmail = "owner@corp.example"
		ownerID    = "U_OWNER"
		botID      = "U0BL99R25K3"
	)
	c := fakeClientWithUser(ownerEmail, ownerID)
	c.userInfo = map[string]*slackapi.User{botID: {ID: botID, RealName: "sre-bot"}}
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category:   "permission_request",
		RequestRef: "req-join-1",
		Lead:       "Session join request",
		Body:       "🔔 Someone is asking to talk to your *sre-bot* agent session.",
		Excerpt: &channelevents.InteractionExcerpt{
			Label:   "Their message",
			Content: "<@" + botID + ">\ncan you share the performance numbers for each of the permissions system?",
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", Subject: identity.Subject("user:" + emailCanonical(ownerEmail))}},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), sessionWithChannel("C_JOIN", "1700000000.000010"), env)
	require.NoError(t, err, "Send must succeed")
	assert.Contains(t, c.userInfoCalls, botID,
		"sendRequest itself must resolve the excerpt's mention — not just this test")

	// Which SURFACE the prompt landed on is deliberately not asserted here.
	// Several tests in this package re-Register "permission_request" into the
	// process-wide category registry with a surface of their own, so a
	// surface assertion in this file passes or fails on test ORDER. The claim
	// under test is category-independent anyway: an excerpt is resolved
	// wherever it is delivered.

	// The card the approver sees, composed exactly as sendRequest composes it.
	p.Excerpt = resolveExcerptMentions(context.Background(), c, p.Excerpt, logr.Discard(), p.RequestRef)
	text := concatBlockText(buildInteractionRequestBlocks(p, "default/s1"))

	assert.Contains(t, text, "@sre-bot", "the approver reads a name, not a user id")
	assert.NotContains(t, text, botID, "the raw user id must not survive into the card")
	assert.NotContains(t, text, "<@", "and never as live mention markup — the excerpt is untrusted")
	assert.Contains(t, text, "performance numbers", "the rest of the message is untouched")
}

// TestResolveMetaagentMentions covers which SLOTS of a scope-approval payload
// get decoded, and the one that must not.
//
// Requester is the exception, and it is not a style choice: every metaagent
// renderer wraps it in "<@…>" markup of its own, so decoding it to a name
// would leave "@<@Dana Whitfield>"-shaped nonsense where a live, clickable
// mention belongs. The other five all land in a region rendered as prose or a
// blockquote, where an id names nobody.
func TestResolveMetaagentMentions(t *testing.T) {
	c := namerClient(map[string]string{
		"U_BOT":  "sre-bot",
		"U_DANA": "Dana Whitfield",
	})
	in := scope.MetaagentApprovalPayload{
		Requester:       "U_DANA",
		Verbatim:        "<@U_BOT> please page <@U_DANA>",
		ApproverSummary: "grants read on the repo <@U_BOT> named",
		SkippedExplain:  "skipped the write <@U_DANA> asked for",
		CaveatExplain:   "expires when <@U_BOT> stops",
		CleanedTask:     "page <@U_DANA>",
	}

	got := resolveMetaagentMentions(context.Background(), c, in, logr.Discard())

	assert.Equal(t, "U_DANA", got.Requester,
		"Requester stays a raw id — the renderers compose <@…> around it themselves")
	assert.Equal(t, "@sre-bot please page @Dana Whitfield", got.Verbatim)
	assert.Equal(t, "grants read on the repo @sre-bot named", got.ApproverSummary)
	assert.Equal(t, "skipped the write @Dana Whitfield asked for", got.SkippedExplain)
	assert.Equal(t, "expires when @sre-bot stops", got.CaveatExplain)
	assert.Equal(t, "page @Dana Whitfield", got.CleanedTask)

	assert.Len(t, c.userInfoCalls, 2,
		"one lookup per distinct id ACROSS the payload — the cache spans its slots, not each slot alone")
}

// TestMetaagentScopeApprovalSender_ResolvesMentionsOnceForEverySink: the
// decode happens in the SENDER, upstream of both the card and the ref cache,
// so all three renderers of this payload inherit it — the card, its
// notification preview, and (through the cache) Show Details and the
// permanent cold-start resolution record.
//
// Doing it per-renderer instead would mean a users.info round trip on every
// Show Details CLICK, and three places to keep in step. The tradeoff is that
// a Show Details served after a channelsd restart falls back to authzd's
// durable record, which still holds the raw ids — a degraded rendering of the
// same text, which is what a cache miss already gives that path.
func TestMetaagentScopeApprovalSender_ResolvesMentionsOnceForEverySink(t *testing.T) {
	fc := &fakeSlackClient{userInfo: map[string]*slackapi.User{
		"U_BOT": {ID: "U_BOT", RealName: "sre-bot"},
	}}
	refs := newMetaagentApprovalRefCache()
	s := &metaagentScopeApprovalSender{client: fc, refs: refs}

	_, err := s.Send(context.Background(),
		metaagentSession(map[string]string{"channel_id": "C9", "thread_ts": "222.2"}, nil),
		approvalEnvelope(t, scope.MetaagentApprovalPayload{
			RequestID:       "req-mentions",
			Requester:       "U_ASKER",
			Verbatim:        "<@U_BOT> can you page the on-call?",
			ApproverSummary: "grants paging",
			CleanedTask:     "page the on-call",
			ColdStart:       true,
		}))
	require.NoError(t, err)

	// Sink 1: the card's blockquoted verbatim region.
	require.Len(t, fc.postEphemeralCalls, 0, "no starter annotation — posts in-thread")
	require.Len(t, fc.postMessageCalls, 1)
	card := concatBlockText(buildMetaagentScopeApprovalBlocks(scope.MetaagentApprovalPayload{
		Requester: "U_ASKER", Verbatim: "<@U_BOT> can you page the on-call?", ColdStart: true,
	}, "default/sess1"))
	assert.Contains(t, card, "&lt;@U_BOT&gt;",
		"the PURE builder is unchanged: handed an UNRESOLVED payload it still escapes the "+
			"raw token into the blockquote — which is precisely what the approver used to read")

	// Sink 2: the ref cache, which Show Details and the resolution record read.
	got, ok := refs.get("req-mentions")
	require.True(t, ok, "the posted approval must be recorded")
	assert.Equal(t, "@sre-bot can you page the on-call?", got.Verbatim,
		"the cache holds what was PUBLISHED, so a later Show Details reads the same names the card showed")
	assert.Equal(t, "U_ASKER", got.Requester, "the requester id the click handler mentions with is untouched")
	assert.Contains(t, fc.userInfoCalls, "U_BOT")
}
