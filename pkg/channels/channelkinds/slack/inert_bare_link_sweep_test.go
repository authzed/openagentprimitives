// pkg/channels/channelkinds/slack/inert_bare_link_sweep_test.go
//
// The bare-URL half of the forged-action guard, on the DECISION surfaces.
//
// escapeSlackText closes `<url|label>` and nothing else. Slack auto-links a
// bare "https://…" in any mrkdwn text object with no markup involved, so a
// surface guarded by the escape alone still renders a genuine-looking link one
// line from a real Approve / Deny button. defuseBareLinks closed that on App
// Home (f3395f15, b3c01bf1); these are its siblings, ranked by how close the
// untrusted string sits to a control the reader is about to click.
//
// Two properties are asserted per surface and both matter:
//
//   - no live link, measured through liveLinkIn — Slack's rendering rule, not
//     the defuser's implementation, and applied PER TEXT OBJECT (mrkdwnSinks);
//   - the text is still READABLE. Inert is not deleted; an approver who cannot
//     see what was asked is an approver deciding blind.
package slack

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// bareLureHost is the visible remnant every row asserts survives the sweep. It
// is a substring of bareLureURL, so "still readable" is checked independently
// of "not live".
const bareLureHost = "attacker.example.invalid/connect-your-account"

// sweepSessionRef is on every fixture payload on purpose: it makes the cards
// render their provenance footer, which composes backticks of its OWN
// ("session `ns/name`"). A guard written against a footerless card is a guard
// against half the string a reader gets.
var sweepSessionRef = channelevents.SessionRef{Namespace: "agents", Name: "demo"}

// mrkdwnSinks returns every mrkdwn string a reader sees as its OWN element.
//
// Per text object, NOT concatenated, and that distinction is the whole point:
// Slack parses each text object independently, so concatenating them lets one
// block's backticks pair with another's. Every card here composes a footer with
// backticks of its own, so a concatenating oracle pairs the footer's opening
// backtick with the BODY's dangling one and reports a live link as safely
// spanned — which is not a rendering any reader can get. It hid a real cap
// defect on the Show-Details modal until this helper was written.
func mrkdwnSinks(blocks []slackapi.Block) []string {
	var out []string
	add := func(t *slackapi.TextBlockObject) {
		if t != nil && t.Type == slackapi.MarkdownType {
			out = append(out, t.Text)
		}
	}
	for _, b := range blocks {
		switch v := b.(type) {
		case containerBlock:
			// The title is a rich_text element Slack renders literally, so it is
			// not a mrkdwn sink; the children are.
			out = append(out, mrkdwnSinks(v.ChildBlocks)...)
		case *slackapi.SectionBlock:
			add(v.Text)
		case *slackapi.HeaderBlock:
			add(v.Text)
		case *slackapi.ContextBlock:
			for _, e := range v.ContextElements.Elements {
				if t, ok := e.(*slackapi.TextBlockObject); ok {
					add(t)
				}
			}
		}
	}
	return out
}

// assertNoLiveLink runs Slack's rendering rule over each sink separately.
func assertNoLiveLink(t *testing.T, sinks []string, msg string) {
	t.Helper()
	require.NotEmpty(t, sinks, "the surface must render at least one mrkdwn sink, or the guard asserts nothing")
	for i, s := range sinks {
		assert.Empty(t, liveLinkIn(s), "%s (sink %d)", msg, i)
	}
}

// mentionFieldCardSinks renders a card whose one Field carries value and
// mentionCount non-slack Mentions.
//
// A non-slack Mention is nothing this renderer can turn into a `<@…>`, so the
// publisher's display text stands in — the path where the fallback IS what the
// reader reads. mentionCount selects which of resolveFieldMentions' two
// interpolations runs: equal to the comma-split count, the positional one;
// otherwise the whole-Value one.
func mentionFieldCardSinks(t *testing.T, value string, mentionCount int) []string {
	t.Helper()
	mentions := make([]channelevents.ExternalIdentity, mentionCount)
	for i := range mentions {
		mentions[i] = channelevents.ExternalIdentity{
			Kind: "bento", ExternalID: identity.RawExternalID("who-" + string(rune('a'+i))),
		}
	}
	fields := resolveFieldMentions(t.Context(), &fakeSlackClient{},
		[]channelevents.InteractionField{{Label: "Would share with", Value: value, Mentions: mentions}},
		logr.Discard(), "req-sweep")
	return mrkdwnSinks(buildInteractionRequestBlocks(channelevents.InteractionRequestPayload{
		AgentSessionRef: sweepSessionRef, Lead: "Share this?", Fields: fields,
	}, "agents/demo"))
}

// decisionSurface is one rendering path that puts an untrusted string in front
// of a reader who is about to decide something. render takes the lure and
// returns each mrkdwn text object the reader sees.
type decisionSurface struct {
	name   string
	render func(t *testing.T, lure string) []string
}

// decisionSurfaces is the swept set: the surfaces where an attacker-controlled
// string sits beside a real decision control.
//
// Deliberately NOT exhaustive over the package. The agent's own reply
// (sender.go / stream_delta_sink.go's pl.Text) is excluded on purpose and has
// its own guard below — the agent's message IS the message, and a user who
// asks for a link must get a link.
func decisionSurfaces() []decisionSurface {
	return []decisionSurface{
		{
			// The credential-update card's Body IS the agent's own sentence and
			// the provider's raw HTTP response body; the card carries the real
			// credential-entry button.
			name: "the pending card's Body, beside its Approve/Deny buttons",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildInteractionRequestBlocks(
					channelevents.InteractionRequestPayload{
						AgentSessionRef: sweepSessionRef,
						Lead:            "Approve this tool call?",
						Body:            "The agent explains: " + lure,
						Actions: []channelevents.InteractionAction{
							{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
						},
					}, "agents/demo"))
			},
		},
		{
			name: "the pending card's NextStep line",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildInteractionRequestBlocks(
					channelevents.InteractionRequestPayload{
						AgentSessionRef: sweepSessionRef,
						Lead:            "Approve this tool call?", NextStep: "Next: " + lure,
					}, "agents/demo"))
			},
		},
		{
			// tool_approval's "Why" row is the summarizer LLM's prose and its
			// "What" row is composed over the model's own tool-call arguments.
			name: "a Field value on the pending card",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildInteractionRequestBlocks(
					channelevents.InteractionRequestPayload{
						AgentSessionRef: sweepSessionRef,
						Lead:            "Approve this tool call?",
						Fields: []channelevents.InteractionField{
							{Label: "Why", Value: "because " + lure},
						},
					}, "agents/demo"))
			},
		},
		{
			// The resolved card replays the CACHED wire payload, so a body the
			// pending card made inert must not come back live after the click.
			name: "the resolved card, which replays the cached request payload",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				req := &channelevents.InteractionRequestPayload{
					AgentSessionRef: sweepSessionRef,
					Lead:            "Approve this tool call?", Body: "The agent explains: " + lure,
				}
				return mrkdwnSinks(buildInteractionAppliedBlocks(
					channelevents.InteractionAppliedPayload{
						AgentSessionRef: sweepSessionRef, Outcome: "approved",
					}, req))
			},
		},
		{
			// The verdict is composed into the SAME text object as the swept
			// cached Body, and sendDecisionApplied writes these blocks to the
			// PUBLIC note as well as the prompt. On queued_messages the reason is
			// the RUNNER's, arriving verbatim on .out.interrupt_applied.
			name: "the resolved card's verdict reason, which also rewrites the public note",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildInteractionAppliedBlocks(
					channelevents.InteractionAppliedPayload{
						AgentSessionRef: sweepSessionRef,
						Outcome:         "denied", Reason: "the runner said " + lure,
					}, nil))
			},
		},
		{
			// resolveFieldMentions owns the LAST sweep of a mention-carrying
			// field's publisher text: escapePublisherPayload skips those fields
			// wholesale so it does not re-escape the `<@U…>` composed here.
			//
			// POSITIONAL leg: the comma-split fallbacks line up 1:1 with the
			// mentions, so each recipient gets its own slice of the Value.
			name: "a mention-field's positional display fallback, the last sweep that field gets",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mentionFieldCardSinks(t, "Dana Okafor "+lure, 1)
			},
		},
		{
			// NON-POSITIONAL leg: the counts do not line up, so every recipient
			// falls back to the WHOLE Value. It is a SECOND interpolation of the
			// same string, and a row exercising only the positional leg leaves it
			// unswept — which is exactly what the mutation check caught.
			name: "...and its whole-Value fallback, when the counts do not line up",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mentionFieldCardSinks(t, "Dana Okafor and Sam Ito "+lure, 2)
			},
		},
		{
			// Verbatim is the requester's raw Slack message; the four
			// summary/explain slots are the composer LLM's prose about it. The
			// card carries Approve / Deny / Run-without-scope.
			name: "the metaagent scope-approval card, beside Approve/Deny/Run-without-scope",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-sweep", Requester: "U_ASKER",
						Verbatim: "please widen scope " + lure,
					}, "agents/demo"))
			},
		},
		{
			name: "the metaagent card's approver summary",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-sweep", Requester: "U_ASKER",
						Verbatim: "please widen scope", ApproverSummary: "adds read on " + lure,
					}, "agents/demo"))
			},
		},
		{
			name: "the metaagent card's heads-up warnings",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-sweep", Requester: "U_ASKER",
						Verbatim: "please widen scope", CaveatExplain: "but note " + lure,
					}, "agents/demo"))
			},
		},
		{
			name: "the metaagent Show-Details re-render, from the ref cache",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return []string{renderMetaagentShowDetailsText(MetaagentApprovalRef{
					RequestID: "req-sweep", Requester: "U_ASKER",
					Verbatim: "please widen scope " + lure,
				})}
			},
		},
		{
			// The summarizer LLM's one-liner over agent-controlled tool
			// arguments, in a PUBLIC channel post styled as the platform's own
			// "Approval pending" card. Both of its sinks parse markup.
			name: "the public approval-pending note, in a public channel",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				body := publicNoteText("Waiting to run: "+lure, "<@U_APPROVER>")
				return append(mrkdwnSinks(publicNoteBlocks(body)), publicNoteNotifyText(body))
			},
		},
		{
			// The Show-Details modal is the ground-truth view an approver opens
			// BEFORE clicking Approve. ToolDescription is an upstream MCP
			// server's tool description.
			name: "the Show-Details modal's tool description, read before Approve",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ToolDescription: "Creates an issue. " + lure,
				})
			},
		},
		{
			name: "the Show-Details modal's justification, the summarizer's prose",
			render: func(t *testing.T, lure string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					Justification: "The agent needs this because " + lure,
				})
			},
		},
	}
}

// detailsModalSinks renders the Show-Details modal for d, per text object.
func detailsModalSinks(d channelevents.ToolApprovalDetails) []string {
	return mrkdwnSinks(renderInteractionDetailsModal(d, "req-details-sweep", "agents/demo"))
}

// TestDecisionSurfaces_ABareURLDoesNotAutoLink is the sweep's guard. Every row
// puts the lure in one slot of one surface, so a slot dropped from the sweep
// fails exactly one row.
func TestDecisionSurfaces_ABareURLDoesNotAutoLink(t *testing.T) {
	for _, s := range decisionSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			sinks := s.render(t, bareLureURL)
			assertNoLiveLink(t, sinks,
				"a bare URL needs no markup to become a link: Slack auto-links it, and this surface sits beside a real control")
			assert.Contains(t, strings.Join(sinks, "\n"), bareLureHost,
				"inert is not deleted — the reader must still see what was claimed")
		})
	}
}

// TestDecisionSurfaces_ABareURLBehindAStrayBacktickDoesNotAutoLink is the
// evasion row. A code span is what makes the URL inert, and an author-supplied
// backtick pairs with the opening one the defuser adds, closing the span early
// and leaving the URL live with a stray backtick in front of it. A defuser that
// wraps without neutralizing what it wraps around defends against nothing an
// author who has read its source cannot step around.
func TestDecisionSurfaces_ABareURLBehindAStrayBacktickDoesNotAutoLink(t *testing.T) {
	for _, s := range decisionSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			assertNoLiveLink(t, s.render(t, "` "+bareLureURL),
				"an author-supplied backtick must not be able to close the span the sweep adds")
		})
	}
}

// TestDecisionSurfaces_SweepRunsExactlyOnce pins the property neither half of
// the sweep gives for free: escapeSlackText is single-pass but NOT idempotent
// ("&" → "&amp;" → "&amp;amp;"), and defuseBareLinks is not either — a second
// pass neutralizes the backticks the first one added, leaving the URL OUTSIDE
// the span and live again.
//
// Both failure modes are visible in one string: a double escape shows
// "&amp;lt;", and a double defuse shows a live link.
func TestDecisionSurfaces_SweepRunsExactlyOnce(t *testing.T) {
	for _, s := range decisionSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			sinks := s.render(t, "<b> & "+bareLureURL)
			joined := strings.Join(sinks, "\n")
			assert.NotContains(t, joined, "&amp;lt;", "the escape must run exactly once")
			assert.NotContains(t, joined, "&amp;amp;", "...on the ampersand too")
			assertNoLiveLink(t, sinks, "and the defuse exactly once")
		})
	}
}

// TestSweptSlotsKeepALegitimateCodeFence is the false-positive guard, and the
// reason the defuser is fence-aware rather than neutralizing every backtick it
// finds.
//
// Two swept slots legitimately carry a ``` fence today and are documented as
// preserving it: provider_error_retry composes its failure message into a fence
// in Body (internal/cmd/channelsd/session_watcher.go), and the metaagent card's
// blockquoted Verbatim deliberately does NOT rewrite ``` because mangling a
// code fence in the very request the approver has to read accurately buys
// nothing — a "> " quote has no fence to break.
//
// Text inside a fence is already unlinkifiable, so skipping it costs the guard
// nothing — as long as the fence is still TERMINATED when the reader gets it,
// which is the one thing this test does not establish and
// TestOrphanedFenceCannotReLivenASweptURL does.
func TestSweptSlotsKeepALegitimateCodeFence(t *testing.T) {
	fenced := fenceDelimiter + "\nprovider said: retry via " + bareLureURL + "\n" + fenceDelimiter +
		"\nClick Retry to try again."

	cases := []struct {
		name   string
		render func() []string
	}{
		{
			name: "provider_error_retry's fenced failure message in Body survives, beside a real Retry button",
			render: func() []string {
				return mrkdwnSinks(buildInteractionRequestBlocks(
					channelevents.InteractionRequestPayload{
						AgentSessionRef: sweepSessionRef,
						Lead:            "⚠️ Agent failed", Body: fenced,
						Actions: []channelevents.InteractionAction{
							{ID: "retry", Label: "Retry", Kind: channelevents.ActionKindDecision},
						},
					}, "agents/demo"))
			},
		},
		{
			name: "the metaagent card's blockquoted Verbatim keeps the requester's fence",
			render: func() []string {
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-fence", Requester: "U_ASKER", Verbatim: fenced,
					}, "agents/demo"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(tc.render(), "\n")
			assert.Contains(t, joined, fenceDelimiter,
				"a fence the publisher composed must survive the sweep, not render as ʼʼʼ")
			assert.NotContains(t, joined, strings.Repeat(inertApostrophe, 3),
				"...which is what blanket backtick neutralization would show the reader")
			assert.Contains(t, joined, bareLureHost,
				"and the fenced URL is still readable, just not clickable — a fence is already inert")
		})
	}
}

// TestSweptSlotsSurviveTheRuneCapWithoutSplittingASpan is the cap half, the
// second defect b3c01bf1 found on App Home and the one that arrives WITH the
// sweep rather than before it.
//
// The sweep emits PAIRED delimiters, and the metaagent card caps every region
// at 2800 runes AFTER it. A cut that lands between the two backticks drops the
// closing one; an unpaired backtick opens no span, and the URL prefix left in
// front of the ellipsis still resolves because the attacker owns the whole
// host — so the region renders a live link again.
func TestSweptSlotsSurviveTheRuneCapWithoutSplittingASpan(t *testing.T) {
	// One whitespace-free run long enough to straddle the 2800-rune region cap
	// on its own, with prose in front of it as the lure.
	lure := "Approve here: " + bareLureURL + "/" + strings.Repeat("a", 3000)

	assertNoLiveLink(t, mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
		scope.MetaagentApprovalPayload{
			RequestID: "req-cap", Requester: "U_ASKER", Verbatim: lure,
		}, "agents/demo")),
		"the cap must not leave the URL outside the span the sweep put it in")
}

// TestDetailsModalProseCapCannotSplitTheDefusedSpan is the same cap defect on
// the Show-Details modal, whose two prose sections cap at 2800 through
// summarizeForSlackSection. ToolDescription is an upstream MCP server's tool
// description and carries no length bound, so one whitespace-free run
// straddling the cap is the server's to choose — on the ground-truth view an
// approver reads before clicking Approve.
func TestDetailsModalProseCapCannotSplitTheDefusedSpan(t *testing.T) {
	// Deliberately one LINE: summarizeForSlackSection takes the first line
	// before it caps, so a multi-line fixture would never reach the cap.
	long := "Creates an issue. See " + bareLureURL + "/" + strings.Repeat("a", 3000)

	for _, tc := range []struct {
		name    string
		details channelevents.ToolApprovalDetails
	}{
		{"an upstream tool description", channelevents.ToolApprovalDetails{ToolDescription: long}},
		{"the summarizer's justification", channelevents.ToolApprovalDetails{Justification: long}},
	} {
		t.Run(tc.name+" cannot re-liven its URL by straddling the cap", func(t *testing.T) {
			sinks := detailsModalSinks(tc.details)
			assertNoLiveLink(t, sinks, "the cap must not drop the closing backtick the sweep added")
			for i, text := range sinks {
				assert.LessOrEqual(t, len([]rune(text)), slackTextObjectLimit,
					"sink %d must still fit Slack's limit: re-closing the span is paid out of the budget, not added to it", i)
			}
		})
	}
}

// TestOrphanedFenceCannotReLivenASweptURL is the fence-awareness's one
// remaining assumption, made into a guard.
//
// defuseBareLinks skips a FENCED region on the strength of one property: Slack
// renders a fence's content literally, so a URL in there was never a link. That
// property belongs to a fence with BOTH delimiters. Every post-sweep transform
// that removes characters can delete the closing one, and what it leaves is a
// region the sweep passed over — deliberately, unswept — with nothing left
// holding it inert:
//
//   - summarizeForSlackSection's FIRST-LINE STRIP, on the Show-Details modal.
//     An upstream MCP server's tool description carries the fence; the strip
//     cuts at the newline inside it. capInertRunes never even looked, because
//     the string was under the cap and it returned on `capped == s`.
//   - the RUNE CAP itself, on the metaagent card. Verbatim is the requester's
//     raw Slack message, unbounded upstream, so the requester chooses both the
//     content and where the 2800-rune cut lands — on the card carrying Approve
//     / Deny / Run-without-scope. Here capInertRunes did look and saw nothing:
//     the orphaned tail is unswept, so its backticks are the author's, and any
//     even number of them reads as "no span left open".
//   - the same cap on App Home, whose Description is AgentClass spec and whose
//     real buttons link the user's credentials.
//
// The fixtures put the URL INSIDE the fence, which is the only place this can
// bite: a URL outside one was swept in the ordinary way.
func TestOrphanedFenceCannotReLivenASweptURL(t *testing.T) {
	// Long enough that a 200- or 2800-rune cap lands inside the fence, leaving
	// the closing delimiter on the far side of the cut.
	fencedRun := func(pad int) string {
		return fenceDelimiter + "\nsee " + bareLureURL + "/" + strings.Repeat("a", pad) + "\n" + fenceDelimiter
	}

	cases := []struct {
		name   string
		render func(t *testing.T) []string
	}{
		{
			// The strip, not the cap: this description is far UNDER 2800 runes,
			// so the cap's early `capped == s` return is the whole exposure.
			name: "the Show-Details modal, where the first-line strip drops the closing fence",
			render: func(t *testing.T) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ToolDescription: "Creates an issue. " + fenceDelimiter + bareLureURL + "\nrest" + fenceDelimiter,
				})
			},
		},
		{
			// The shape the fence-blind oracle could not see: one stray backtick
			// AFTER the URL pairs with the fence's third delimiter, so a
			// whole-string pairing reports the live URL as safely spanned.
			name: "...and again with a stray backtick after the URL, which whole-string pairing hid",
			render: func(t *testing.T) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ToolDescription: "Creates an issue. " + fenceDelimiter + "cfg `k` " + bareLureURL + " `\nrest" + fenceDelimiter,
				})
			},
		},
		{
			name: "the metaagent card's Verbatim, where the 2800-rune cap drops it, beside Approve/Deny",
			render: func(t *testing.T) []string {
				t.Helper()
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-orphan", Requester: "U_ASKER", Verbatim: fencedRun(3000),
					}, "agents/demo"))
			},
		},
		{
			name: "the metaagent card's approver summary, capped the same way",
			render: func(t *testing.T) []string {
				t.Helper()
				return mrkdwnSinks(buildMetaagentScopeApprovalBlocks(
					scope.MetaagentApprovalPayload{
						RequestID: "req-orphan", Requester: "U_ASKER",
						Verbatim: "please widen scope", ApproverSummary: fencedRun(3000),
					}, "agents/demo"))
			},
		},
		{
			name: "App Home's description section, on the page whose real button links the user's credentials",
			render: func(t *testing.T) []string {
				t.Helper()
				return []string{appHomePageText(appHomeAgent{
					Name: "triage", DisplayName: "Triage", Description: fencedRun(400),
				}).Description}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sinks := tc.render(t)
			assertNoLiveLink(t, sinks,
				"a transform that deletes the closing fence leaves the region the sweep skipped with nothing holding it inert")
			assert.Contains(t, strings.Join(sinks, "\n"), "attacker.example.invalid",
				"inert is not deleted — the reader must still see what was claimed")
		})
	}
}

// TestCapInertRunesIsIdempotent pins the property this package has now shipped
// two escapers without: neither half of inertProse survives a second pass, and
// a REPAIR that does not survive one is the same bug in a new place. Re-running
// the cap over its own output is the shape a future renderer reaches for when it
// caps a composed line whose parts were already capped.
func TestCapInertRunesIsIdempotent(t *testing.T) {
	cases := []struct {
		name  string
		swept string
	}{
		{
			name:  "a cut inside the span the sweep added",
			swept: inertProse("see " + bareLureURL + "/" + strings.Repeat("a", 400)),
		},
		{
			name:  "a cut inside a fence the sweep skipped",
			swept: inertProse(fenceDelimiter + "\nsee " + bareLureURL + "/" + strings.Repeat("a", 400) + "\n" + fenceDelimiter),
		},
		{
			// What the first-line strip hands the cap: short enough that no
			// truncation happens at all, so the repair is the only thing acting.
			name: "a fence the strip orphaned before the cap ever ran",
			swept: strings.SplitN(
				inertProse("lede "+fenceDelimiter+bareLureURL+"\nrest"+fenceDelimiter), "\n", 2)[0],
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+": re-capping its own output changes nothing", func(t *testing.T) {
			const budget = 200
			once := capInertRunes(tc.swept, budget)
			assert.Equal(t, once, capInertRunes(once, budget),
				"a second pass must not undo the first — that is how defuseBareLinks re-livens a URL")
			assert.Empty(t, liveLinkIn(once), "and the once-capped string is inert to begin with")
			assert.LessOrEqual(t, len([]rune(once)), budget, "every repair is paid out of the budget")
		})
	}
}

// TestCapInertRunes_MeasuresParityOnTheTailRegionOnly is the cap's contract
// stated at the DELIMITER level, one layer below the rendering rule liveLinkIn
// models — it says the cap leaves no pair half-written, whatever that pair
// would have rendered as.
//
// A code FENCE contributes three backticks, so whole-string SPAN parity flips
// on it: an unterminated fence plus one span the cut left hanging reads EVEN —
// "nothing to repair" — while the span is still open. Only the region after the
// last fence delimiter, where defuseUnfenced's pairs actually live, answers the
// question the span check is asking, and it only answers it once the fence
// check has run. The requester types the Verbatim on the metaagent card, so an
// unbalanced fence is theirs to choose.
func TestCapInertRunes_MeasuresParityOnTheTailRegionOnly(t *testing.T) {
	run := "see " + bareLureURL + "/" + strings.Repeat("a", 400)

	cases := []struct {
		name  string
		swept string
	}{
		{
			name:  "no fence: the cut inside a span is re-closed",
			swept: inertProse(run),
		},
		{
			name:  "balanced fence ahead of the run: still re-closed",
			swept: inertProse(fenceDelimiter + "\nx\n" + fenceDelimiter + "\n" + run),
		},
		{
			// The sweep DID sweep this tail (an odd delimiter count fails
			// closed), so the cut lands inside a span it added — and the fence
			// it sits under is closed on the way out, which is what makes the
			// whole-string parity that would have read "nothing to repair"
			// stop mattering.
			name:  "unterminated fence ahead of the run: both delimiters closed, where whole-string parity would close neither",
			swept: inertProse(fenceDelimiter + "\n" + run),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const budget = 200
			capped := capInertRunes(tc.swept, budget)

			require.NotEqual(t, tc.swept, capped, "the fixture must actually be truncated")
			assert.LessOrEqual(t, len([]rune(capped)), budget,
				"re-closing a delimiter is paid out of the budget, not added to it")

			assert.Zero(t, strings.Count(capped, fenceDelimiter)%2,
				"a fence the cut orphaned must be closed, or the region it opened renders unswept")
			parts := strings.Split(capped, fenceDelimiter)
			assert.Zero(t, strings.Count(parts[len(parts)-1], "`")%2,
				"the region where the sweep's spans live must end with every one of them closed")
		})
	}
}

// TestComposedSpanValuesCannotBreakOut is the OTHER defect class: a renderer
// that wraps a value in its OWN `%s` span while the sweep that produced the
// value deliberately leaves backticks live. One backtick in the value closes
// the span early and everything after it renders as live mrkdwn.
//
// The details modal is the sharp case — ResourceID is bound from the model's
// own tool-call arguments, and the modal is the ground-truth view an approver
// reads BEFORE clicking Approve.
func TestComposedSpanValuesCannotBreakOut(t *testing.T) {
	cases := []struct {
		name   string
		render func(t *testing.T, value string) []string
	}{
		{
			name: "the details modal's resourceID, bound from the model's own tool-call arguments",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ResourceType: "repo", ResourceID: v, Permission: "write",
				})
			},
		},
		{
			name: "...its resourceType",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ResourceType: v, ResourceID: "org/x", Permission: "write",
				})
			},
		},
		{
			name: "...its permission",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{
					ResourceType: "repo", ResourceID: "org/x", Permission: v,
				})
			},
		},
		{
			name: "...and its footer's state impact",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{StateImpact: v})
			},
		},
		{
			name: "...and its footer's args hash",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return detailsModalSinks(channelevents.ToolApprovalDetails{ArgsHash: v})
			},
		},
		{
			name: "the settings modal's allowed-toolkit names",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return mrkdwnSinks(renderSettingsModalBlocks("agents/demo",
					&spiceboxv1alpha1.EffectiveSettings{AllowedToolkits: []string{v}}))
			},
		},
		{
			name: "the settings modal's allowed MCP server names",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return mrkdwnSinks(renderSettingsModalBlocks("agents/demo",
					&spiceboxv1alpha1.EffectiveSettings{
						AllowedMCP: []spiceboxv1alpha1.AllowedMCPServer{{Name: v}},
					}))
			},
		},
		{
			name: "the settings modal's resolved model name",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return mrkdwnSinks(renderSettingsModalBlocks("agents/demo",
					&spiceboxv1alpha1.EffectiveSettings{
						Model: spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: v},
					}))
			},
		},
		{
			name: "the tool-session header's tool name",
			render: func(t *testing.T, v string) []string {
				t.Helper()
				return mrkdwnSinks(renderToolSessionBlocks(toolSessionView{toolName: v}))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The value closes the renderer's span, then forges an action in
			// BOTH the forms Slack renders as a link.
			sinks := tc.render(t, "safe`  "+lureLink+"  "+bareLureURL)
			assert.NotContains(t, strings.Join(sinks, "\n"), lureLink,
				"a value must not be able to close the span its renderer wrapped it in and forge a masked link")
			assertNoLiveLink(t, sinks, "...nor a bare one")
		})
	}
}

// TestToolSessionHeaderReasonIsInert covers the tool-session header's OTHER
// half. reason is the agent's `_reason` for the dispatching tool call — the
// model's own prose — and it is interpolated into a mrkdwn section with no
// escape at all, so it can open a channel-wide ping as easily as a link.
func TestToolSessionHeaderReasonIsInert(t *testing.T) {
	sinks := mrkdwnSinks(renderToolSessionBlocks(toolSessionView{
		toolName: "claude",
		reason:   "write the README <!channel> " + lureLink + " " + bareLureURL,
	}))
	joined := strings.Join(sinks, "\n")

	assert.NotContains(t, joined, "<!channel>", "model prose must not open a channel-wide ping")
	assert.NotContains(t, joined, lureLink, "...nor a forged platform action")
	assertNoLiveLink(t, sinks, "...nor a bare auto-linked URL")
	assert.Contains(t, joined, "&lt;!channel&gt;", "and the attempt stays visible, not deleted")
}

// TestAgentReplyTextStaysLive is the constraint the sweep must NOT break, kept
// next to it so a future widening trips over it.
//
// The agent's own reply is deliberately live: sender.go's slackifyText even
// UN-escapes entities on that path, because the agent's message IS the message
// and a user who asks the agent for a link must get one. It is a different
// payload on a different sink from every row above, and no sweep belongs on it.
func TestAgentReplyTextStaysLive(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}

	const reply = "Here is the runbook: https://docs.example.invalid/runbook"
	_, err := s.Send(t.Context(), sess, userMessageEnvelope(t, reply))
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "expected 1 postMessage call")

	posted := renderTextFromOpts(t, c.postMessageCalls[0].options)
	assert.Equal(t, reply, posted,
		"the agent's own reply must reach Slack verbatim — no escape, no code span around its URL")
}

// TestInProcessNoticeKeepsItsComposedLink is the second false-positive guard.
// The in-process notice path (noticeMsgOptions → buildInteractionBlocks, NOT
// buildInteractionRequestBlocks) composes real `<url|label>` links of the
// kind's own making — the fork-root "continued from an earlier conversation"
// link is the live example. Those must keep working: the sweep belongs at the
// WIRE boundary, where publisher text can still be told apart from markup this
// kind composed.
func TestInProcessNoticeKeepsItsComposedLink(t *testing.T) {
	joined := strings.Join(mrkdwnSinks(buildInteractionBlocks(
		channelevents.InteractionRequestPayload{
			Lead: "This conversation continues in a new thread",
			Body: "Follow it in <https://slack.example/archives/C1/p123|the new thread>.",
		},
		channelinteractions.Category{Tone: channelinteractions.ToneRoutine}, "")), "\n")

	assert.Contains(t, joined, "<https://slack.example/archives/C1/p123|the new thread>",
		"a link this kind composed for its own in-process notice must stay live")
}
