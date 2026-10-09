package slack

import (
	"context"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// slackEntityRe matches Slack's own wire encoding for the things a message can
// reference. Slack sends these forms in event text and renders them back as
// names in its client; anywhere we quote that text ourselves, the encoding
// leaks through as an opaque id unless something decodes it.
//
// The four groups, in the order the alternation tries them:
//
//	<@U0123>          user, id only
//	<@U0123|dana>     user, with the label Slack cached at send time
//	<#C0123|general>  channel (the label is always present in practice)
//	<!here>           broadcast — here / channel / everyone
//
// Deliberately NOT matched: `<https://…|label>` links. Those are the bare-link
// sweep's business (defuseBareLinks), and rewriting a URL's angle-bracket form
// on the way to a reader is a security-relevant transform that belongs with the
// rest of the link handling, not here.
var slackEntityRe = regexp.MustCompile(`<([@#!])([^>|]+)(?:\|([^>]*))?>`)

// mentionLookupBudget caps how many DISTINCT user ids one string may resolve.
//
// The strings this runs over are untrusted — a quoted message, a tool result —
// so the token count is chosen by whoever wrote the text, not by us. Without a
// cap, one card renders into an unbounded burst of users.info calls against a
// workspace-wide rate limit, and the reader gets nothing extra for it: a
// message naming more than a couple of dozen people is not one whose ids the
// approver is reading individually. Past the cap the token keeps its raw form,
// which is exactly how it renders today.
const mentionLookupBudget = 24

// userNamer is the users.info half of slackClient, named separately so the
// helpers below state the ONE capability they need.
type userNamer interface {
	GetUserInfoContext(ctx context.Context, user string, _ ...slackapi.GetUserInfoOption) (*slackapi.User, error)
}

// renderSlackEntities rewrites Slack's wire encoding for mentions, channel
// refs and broadcasts into the PLAIN TEXT a human expects to read. It is the
// display-side inverse of flattenRichTextSection (messagecontent.go), which
// writes those same forms into MessageText at ingest so the id — not a name
// that can change — is what travels through the pipeline.
//
// Two properties make it safe to run over untrusted text, and both are the
// reason it does not simply hand the string back to Slack to render:
//
//   - The output is INERT, UNCONDITIONALLY. "@Dana Whitfield", never
//     "<@U_DANA>". Re-emitting live mention syntax would let anyone whose
//     message we quote ping a person — or, via <!channel>, everyone — from
//     inside a card they do not control. The invariant holds on the failure
//     paths too: an id this cannot resolve degrades to "@U_DANA", NOT to the
//     token it arrived as. A helper whose output is inert only when its
//     lookups succeed is one a caller has to sweep, and the next caller will
//     forget.
//   - It runs BEFORE the caller's inertness sweep, never after. A display name
//     is workspace-settable text, so it can carry a backtick or a fence; the
//     sweep that neutralizes those (inertExcerpt) must get the last word. In
//     the sender this holds by construction — resolution happens there, the
//     sweep happens inside the pure block builder it calls.
//
// A lookup that fails degrades to the bare id rather than inventing a name or
// dropping the reference: the reader still sees that SOMEONE was mentioned, and
// by which id, which is no less than the encoded form told them. The failure is
// logged, because a systematically missing users:read scope otherwise shows up
// only as cards full of raw ids with nothing saying why.
func renderSlackEntities(ctx context.Context, cli userNamer, s string, logger logr.Logger, requestRef string) string {
	return newMentionResolver(cli, logger, requestRef).render(ctx, s)
}

// mentionResolver carries the lookup cache and budget across every string ONE
// card is built from. Sharing it matters twice over: a person named in two
// slots of the same payload costs one users.info call rather than one per slot,
// and the budget bounds the CARD, which is the unit a reader and a rate limit
// both care about — a per-string cap is no cap at all when the caller has five
// strings.
//
// Not safe for concurrent use, and does not need to be: a resolver belongs to
// one render.
type mentionResolver struct {
	cli    userNamer
	logger logr.Logger
	ref    string
	// names memoises id → name, negative results INCLUDED: an id that cannot be
	// resolved must not be retried once per occurrence. Same idiom as the
	// history reader's author resolution.
	names map[string]string
}

func newMentionResolver(cli userNamer, logger logr.Logger, requestRef string) *mentionResolver {
	return &mentionResolver{cli: cli, logger: logger, ref: requestRef, names: map[string]string{}}
}

func (r *mentionResolver) render(ctx context.Context, s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	return slackEntityRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := slackEntityRe.FindStringSubmatch(tok)
		sigil, body, label := m[1], m[2], m[3]
		switch sigil {
		case "!":
			// here / channel / everyone, and the user-group form
			// "subteam^S123|@team". The label is the group's own handle when
			// Slack supplies one; the body is the word itself for a broadcast,
			// so both render without a lookup.
			if label != "" {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@" + body
		case "#":
			// A channel ref carries its name inline, so this costs no API call.
			// Without one there is nothing to resolve — conversations.info is a
			// different scope, and a channel id in a quoted message is not
			// worth acquiring it.
			if label == "" {
				return "#" + body
			}
			return "#" + strings.TrimPrefix(label, "#")
		}
		// A user. The piped form already carries the label Slack cached when
		// the message was sent; prefer it and skip the lookup entirely.
		if label != "" {
			return "@" + strings.TrimPrefix(label, "@")
		}
		name, ok := r.names[body]
		if !ok {
			if len(r.names) >= mentionLookupBudget {
				return "@" + body
			}
			name = lookupUserName(ctx, r.cli, body, r.logger, r.ref)
			r.names[body] = name // cached even when empty — see above
		}
		if name == "" {
			return "@" + body
		}
		return "@" + name
	})
}

// lookupUserName resolves one Slack user id to the most human name the profile
// offers, or "" when it cannot — which the caller reads as "fall back to the
// bare id". RealName over Name because that is the order the history reader
// already attributes messages in, and two surfaces naming the same person
// differently is worse than either name.
func lookupUserName(ctx context.Context, cli userNamer, userID string, logger logr.Logger, requestRef string) string {
	if cli == nil {
		return ""
	}
	u, err := cli.GetUserInfoContext(ctx, userID)
	if err != nil {
		logger.Info("slack: users.info failed; leaving the mention as its raw id",
			"requestRef", requestRef, "user", userID, "err", err.Error())
		return ""
	}
	if u == nil {
		return ""
	}
	if u.RealName != "" {
		return u.RealName
	}
	return u.Name
}

// resolveExcerptMentions returns a copy of an Excerpt with Slack's entity
// encoding decoded to readable text in BOTH its fields — the label is
// publisher-authored today, but the contract makes no distinction between the
// two and neither does the renderer.
//
// The Excerpt is the only slot this runs over, and that boundary is the point:
// it is the one place the wire contract lets channel-native, untrusted text
// travel. Body and Fields are publisher prose from a channel-agnostic pipeline
// that is forbidden from composing "<@…>" in the first place, so decoding them
// would only give a publisher a way to smuggle a mention through a slot the
// renderer promises is inert.
//
// nil in, nil out — a card with nothing quoted needs no lookups.
func resolveExcerptMentions(ctx context.Context, cli userNamer, ex *channelevents.InteractionExcerpt, logger logr.Logger, requestRef string) *channelevents.InteractionExcerpt {
	if ex == nil {
		return nil
	}
	r := newMentionResolver(cli, logger, requestRef)
	out := *ex
	out.Label = r.render(ctx, out.Label)
	out.Content = r.render(ctx, out.Content)
	return &out
}

// resolveMetaagentMentions decodes the scope-approval payload's untrusted text
// slots — the requester's own message and the composer's prose about it — so
// an approver reads names instead of ids.
//
// Requester is the deliberate exception. It is a raw user id that EVERY
// metaagent renderer wraps in "<@…>" markup of its own (the card's region-1
// prefix, the notification preview, the resolution record), which is the one
// place in this payload a live, clickable mention is wanted. Decoding it would
// put a name inside that markup and kill the mention. This is the same slot
// escapeMetaagentApproval singles out, for the same structural reason.
//
// The live path runs this ONCE, in the sender, before the card is built and
// before the ref cache is written — never inside the three renderers. That is
// one users.info round trip per prompt rather than one per Show Details click,
// and it makes the cache hold what was actually published.
//
// The restart path runs it a second time, at the other end: authzd's durable
// request record predates any Slack rendering, so resolveMetaagentApprovalRef
// decodes what it rehydrates (refWithResolvedMentions). Two call sites is the
// price of one card reading the same way either side of a channelsd restart.
func resolveMetaagentMentions(ctx context.Context, cli userNamer, p scope.MetaagentApprovalPayload, logger logr.Logger) scope.MetaagentApprovalPayload {
	r := newMentionResolver(cli, logger, p.RequestID)
	p.Verbatim = r.render(ctx, p.Verbatim)
	p.ApproverSummary = r.render(ctx, p.ApproverSummary)
	p.SkippedExplain = r.render(ctx, p.SkippedExplain)
	p.CaveatExplain = r.render(ctx, p.CaveatExplain)
	p.CleanedTask = r.render(ctx, p.CleanedTask)
	return p
}
