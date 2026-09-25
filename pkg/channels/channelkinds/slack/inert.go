package slack

import (
	"regexp"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// slackEscaper HTML-escapes Slack's three active characters. It is the ONE
// escaper in this package: every site rendering text it did not author into a
// mrkdwn surface goes through escapeSlackText.
//
// strings.NewReplacer is load-bearing — SINGLE-PASS and ORDER-INDEPENDENT, so
// "&lt;" becomes "&amp;lt;" and not "&amp;amp;lt;". Chained ReplaceAll calls
// are only correct while "&" happens to run first, and break silently the
// moment someone reorders the lines.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeSlackText defangs Slack's three MARKUP characters — `&`, `<`, `>` — so
// the string cannot open a link span (`<url|label>`), a channel-wide ping
// (`<!channel>`, `<!here>`), or a mention (`<@U…>`).
//
// It is HALF the sweep: Slack auto-links a bare "https://…" with no markup at
// all, which escaping cannot touch. See defuseBareLinks for the other half and
// inertProse for the pair — a surface calling this alone has decided a bare URL
// on it is acceptable; the decision surfaces have not.
//
// Backticks, `*` and `_` stay live on purpose: publishers here compose them
// (toolApprovalFields backtick-wraps the tool name, contentInspectionLead bolds
// and code-spans, the "_(no justification provided)_" fallback italicizes), and
// escaping them would mangle real cards for no security gain. Consequence: a
// value a RENDERER wraps in a span of its own needs inertSpanValue, since an
// author backtick would close that span early.
func escapeSlackText(s string) string { return slackEscaper.Replace(s) }

// escapeSlackTexts is escapeSlackText over a slice, returning a NEW slice. The
// escape has to land per entry, BEFORE a caller composes its own markup around
// each one (the join notice's thread participants), or that markup is escaped
// with it. inertAppHomeTexts is the same shape with the full sweep behind it.
func escapeSlackTexts(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = escapeSlackText(s)
	}
	return out
}

// inertApostrophe is what a neutralized backtick becomes: the modifier
// apostrophe, which Slack's mrkdwn gives no meaning to. Shared by inertExcerpt
// (fences) and defuseBareLinks (single backticks) so the two agree on what a
// defanged delimiter looks like.
const inertApostrophe = "ʼ"

// fenceDelimiter is Slack's code-block delimiter. Text inside a fence renders
// literally: no link span, no auto-link, no mention.
const fenceDelimiter = "```"

// neutralizeBackticks defangs every backtick in s, so nothing in it can open or
// close an inline code span.
func neutralizeBackticks(s string) string {
	return strings.ReplaceAll(s, "`", inertApostrophe)
}

// bareLinkRe matches what Slack's own linkifier hyperlinks with no markup
// involved: a scheme-form URL, a mailto:, or a bare "www." host. The scheme is
// matched GENERICALLY rather than as an http/https allowlist — the shape is
// what Slack recognizes, and an allowlist breaks on the first "slack://".
//
// There is deliberately NO leading `\b`: it requires a non-word character
// before the scheme, so "_https://attacker.example/x" has no boundary to match,
// the string takes defuseBareLinks' early exit, and Slack — which has no such
// rule — linkifies it anyway. Anchoring on the scheme run alone costs only
// BREADTH ("Docs-https://x" matches from "Docs-"), which is the right direction
// to fail in: an over-broad match is WRAPPED, a missed one is SKIPPED.
//
// A schemeless bare host ("attacker.example") is deliberately unmatched: Slack
// does not linkify those on this path, and matching them would monospace every
// ordinary sentence containing a filename.
var bareLinkRe = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.\-]{1,31}://|mailto:|www\.)\S*`)

// defuseBareLinks makes a string's URLs unclickable by wrapping each in an
// inline code span, which Slack does not linkify through. It is the half
// escapeSlackText cannot do: escaping & < > stops `<url|label>` — the form
// where the attacker chooses the LABEL — but Slack auto-links a bare
// "https://…" with no markup at all, and escaping is not sufficient even for
// the masked form, since `&lt;https://attacker.example/x|Connect&gt;` opens no
// link SPAN but leaves the URL inside it bare. Inert, not deleted: every
// character survives, and a code span is easier to copy than to misclick.
//
// Backticks are neutralized to inertApostrophe FIRST, or an author-supplied
// backtick could pair with one of the two this function adds and leave the URL
// outside its span. A string with no URL run is returned untouched, so ordinary
// `code` in an ordinary description is unaffected.
//
// It is FENCE-AWARE and has to be. Two swept slots legitimately carry a ```
// fence and document preserving it: provider_error_retry's Body
// (internal/cmd/channelsd/session_watcher.go), and the metaagent card's blockquoted
// Verbatim, where a "> " quote has no fence to break and mangling the
// requester's code buys nothing. Neutralizing every backtick would render both
// as "ʼʼʼ". Skipping fenced regions is free: Slack renders their content
// literally, so a URL in there was never a link.
//
// The fence bookkeeping fails CLOSED. Splitting on the delimiter puts unfenced
// regions at even indices and fenced ones at odd — except that an ODD delimiter
// count leaves the last region unterminated, and that tail is swept as though
// it were outside. Over-sweeping monospaces a URL that did not need it;
// under-sweeping leaves it clickable.
//
// Trailing sentence punctuation is left outside the span so a URL ending a
// sentence does not render with the full stop in monospace.
func defuseBareLinks(s string) string {
	if !bareLinkRe.MatchString(s) {
		return s
	}
	parts := strings.Split(s, fenceDelimiter)
	// parts[len-1] is the tail. When len(parts) is even the delimiter count is
	// odd, so the fence that opened the tail never closes and the tail is swept.
	tailUnterminated := len(parts)%2 == 0
	for i := range parts {
		fenced := i%2 == 1 && !(tailUnterminated && i == len(parts)-1)
		if fenced {
			continue
		}
		parts[i] = defuseUnfenced(parts[i])
	}
	return strings.Join(parts, fenceDelimiter)
}

// defuseUnfenced is defuseBareLinks' body for ONE region known to be outside a
// code fence: defang every backtick, then wrap each URL run in its own span.
//
// Unconditional, with no per-region "has a URL" early exit — this region's
// backticks are exactly the ones that could pair with a span added to a
// NEIGHBOURING region of the same text object.
func defuseUnfenced(s string) string {
	s = neutralizeBackticks(s)
	return bareLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		link := strings.TrimRight(m, ".,;:!?")
		if link == "" {
			return m
		}
		return "`" + link + "`" + m[len(link):]
	})
}

// inertProse makes an untrusted string inert for a FREE mrkdwn region — one the
// renderer wraps in no delimiter of its own: a section body, a blockquote, a
// bullet's value. It is the whole sweep, both halves, and what every decision
// surface in this package renders untrusted text through.
//
// Order matters INSIDE it: escape first, so the entities land inside the code
// span rather than around it, and so a `<url|label>` the escape has already
// defanged still gets its inner URL defused.
//
// Neither half is idempotent — escapeSlackText doubles its own entities, and a
// second defuse neutralizes the backticks the first added, leaving the URL
// outside the span — so every caller must be the ONLY sweep on its string. The
// wire boundaries here (escapePublisherPayload, escapeMetaagentApproval,
// escapeToolApprovalDetails, publicNoteText) each hold that position for
// exactly one path.
func inertProse(s string) string { return defuseBareLinks(escapeSlackText(s)) }

// inertSpanValue makes an untrusted string inert for a slot the RENDERER wraps
// in its own inline code span — `fmt.Sprintf("`%s`", value)` and friends.
//
// It escapes and then defangs backticks, and deliberately does NOT defuse bare
// links. Both halves are load-bearing:
//
//   - Defanging is the point. escapeSlackText leaves backticks live ON PURPOSE,
//     so one backtick in the value closes the renderer's span early and the
//     remainder renders as live mrkdwn — the delimiter added to make the value
//     inert becomes the thing it escapes through.
//   - Defusing would UNDO it: a URL wrapped in a span and then wrapped again
//     gives "“https://…“" — an empty span, then a live URL. The renderer's own
//     span already stops the auto-link.
func inertSpanValue(s string) string { return neutralizeBackticks(escapeSlackText(s)) }

// capInertRunes caps ALREADY-SWEPT mrkdwn at maxRunes and re-closes EITHER
// delimiter pair a character-removing transform can break: an inline code span,
// and the ``` fence the sweep skipped a region on the strength of. Every
// post-sweep rune cap in this package goes through it, and so does the
// Show-Details modal's first-line strip (summarizeForSlackSection), a
// character-removing transform of the same shape.
//
// It exists because the sweep leaves PAIRED delimiters behind and the
// transforms run after it. Each pair has its own failure:
//
//   - a cut between the two backticks the sweep put AROUND a URL drops the
//     closing one. An unpaired backtick opens no span, and a truncated URL still
//     resolves because the attacker owns the host — so the slot renders a live
//     link again.
//   - a cut that drops a closing ``` orphans a fence whose content
//     defuseBareLinks SKIPPED and therefore never swept. "Creates an issue.
//     ```URL\nx```" is fence-skipped, then cut at the newline by the first-line
//     strip: an unterminated fence around a URL nothing defused. This arrives
//     with no truncation at all, which is why neither check may sit behind a
//     `capped == s` early return.
//
// The two repairs are measured differently because the pairs mean different
// things:
//
//   - the FENCE is counted over the whole string; an odd count means the last
//     delimiter opened a region nothing closes, and the repair is to CLOSE it,
//     restoring the premise the sweep relied on (fenced ⇒ literal ⇒ never
//     linkified). Sweeping the orphaned tail instead would leave the fence still
//     orphaned, grow the string by two runes per link with no bound, and rewrite
//     the requester's code content.
//   - the SPAN is counted over the TAIL region after the last fence delimiter,
//     where the sweep's own pairs live. defuseUnfenced defangs every author
//     backtick before adding its own, always two per link, so an odd count there
//     can only be a truncated pair. Fenced backticks are excluded — they come in
//     threes and are not span delimiters. The fence check runs FIRST, which is
//     what makes that reading true.
//
// Every repair is paid OUT OF the budget, not added to it: the closing
// delimiter must fit inside maxRunes or fixing one Slack constraint breaks
// another. Re-capping shorter can land the cut inside the OPENING delimiter
// instead, which balances the string equally well — hence the re-measure.
func capInertRunes(s string, maxRunes int) string {
	capped := truncateRunes(s, maxRunes)
	if fenceLeftOpen(capped) {
		capped = truncateRunes(s, maxRunes-len(fenceDelimiter))
		if fenceLeftOpen(capped) {
			// The closed region is literal, so nothing inside it can be a span
			// delimiter and the span check below has nothing left to find.
			return capped + fenceDelimiter
		}
	}
	if !spanLeftOpen(capped) {
		return capped
	}
	capped = truncateRunes(s, maxRunes-1)
	if spanLeftOpen(capped) {
		capped += "`"
	}
	return capped
}

// fenceLeftOpen reports whether s ends inside a ``` code fence: an ODD number
// of delimiters, so the last one opened a region nothing closes. Same count
// defuseBareLinks fails closed on — read here to TERMINATE the region instead,
// since by now the sweep that skipped it will not run again.
func fenceLeftOpen(s string) bool {
	return strings.Count(s, fenceDelimiter)%2 == 1
}

// spanLeftOpen reports whether s ends inside an inline code span: an odd number
// of backticks in the region after the last fence delimiter (with no fence, the
// whole string). Only meaningful once fenceLeftOpen is false — with a fence left
// open, that trailing region is unterminated FENCE content whose backticks are
// the author's and carry no parity contract.
func spanLeftOpen(s string) bool {
	parts := strings.Split(s, fenceDelimiter)
	return strings.Count(parts[len(parts)-1], "`")%2 == 1
}

// inertText is App Home text that has already been swept. It is the surface's
// ORDERING CONTRACT expressed as a type rather than a comment, because a
// comment failed here twice: once when unwrapItalicMarkers stripped a leading
// "_" the sweep could not see, uncovering a live URL; once when a rune cap cut
// between the two backticks the sweep had added, dropping the closing one.
// Both were a correct guard undone by a transform standing NEXT to it.
//
// Required order: NORMALIZE the raw string (trim, unwrap italics — anything
// that removes characters), then SWEEP, then CAP. The type makes the first and
// last un-invertible:
//
//   - normalizers take a string and the sweep RETURNS an inertText, so a
//     normalizer cannot run after the sweep — it would not typecheck;
//   - a struct, NOT a named string type, so `string(t)` cannot reach the value.
//     The only exits are String() and slot(), and slot() is the
//     delimiter-aware cap.
//
// The type also discourages sweeping twice: neither half is idempotent —
// escapeSlackText doubles its entities, and a second defuse neutralizes the
// backticks the first added, leaving the URL outside a span.
type inertText struct{ s string }

// String returns the swept text for an UNCAPPED mrkdwn sink (the context block
// above the card). Callers with a rune budget want slot instead.
func (t inertText) String() string { return t.s }

// slot caps swept text at maxRunes for a length-enforced card slot and
// re-closes a code span the cut opened. The other producer of an inertText,
// composeInert, composes markup containing no backtick, so the parity
// capInertRunes reads means the same thing here as everywhere else.
func (t inertText) slot(maxRunes int) string { return capInertRunes(t.s, maxRunes) }

// inertAppHomeText is the App Home tab's sweep — inertProse, in the type that
// makes this surface's normalize → sweep → cap order un-invertible.
//
// App Home earns the wrapper because its real buttons link the user's
// credentials, so a forged "Connect your account" link is worth more here than
// anywhere else this kind draws, and its text is AgentClass spec supplied by
// whatever .oap bundle was installed or by any principal with AgentClass write
// RBAC. The decision surfaces — the interaction card, the metaagent card, the
// Show-Details modal, the public note — call inertProse directly.
func inertAppHomeText(s string) inertText { return inertText{s: inertProse(s)} }

// inertAppHomeTexts is inertAppHomeText over a slice, returning a NEW slice so
// the caller's own is untouched — same reason escapeSlackTexts does.
func inertAppHomeTexts(in []string) []inertText {
	if len(in) == 0 {
		return nil
	}
	out := make([]inertText, len(in))
	for i, s := range in {
		out[i] = inertAppHomeText(s)
	}
	return out
}

// composeInert asserts that a line THIS PACKAGE composed is inert — the one
// deliberate hole in the type, with a single caller (connectionsBlock, which
// interleaves already-swept labels with its own "*Your connections*" markup;
// sweeping the composed line would render that markup as literal asterisks).
//
// Precondition, which slot depends on: every VARIABLE part of s is already an
// inertText, and the markup around them contains no backtick — otherwise the
// parity slot reads stops meaning what it thinks it means.
func composeInert(s string) inertText { return inertText{s: s} }

// escapePublisherPayload returns a copy of a wire InteractionRequestPayload
// with every publisher-supplied text slot made inert, so the ONLY live Slack
// link/mention markup in the rendered card is what this kind composes.
//
// It runs at the WIRE BOUNDARY (buildInteractionRequestBlocks) rather than
// inside the shared renderer, because by the time buildInteractionBlocks runs,
// publisher text and kind-composed markup are indistinguishable: the same
// renderer draws the applied card, whose Body carries a verdict naming the
// decider as a real `<@U…>`, and the public note, whose Body carries approver
// mentions. Escaping there would render those as "&lt;@U123&gt;".
//
// The wire contract calls these fields "publisher-authored and trusted", which
// is not something a renderer can rely on for link markup: the credential-update
// card's Lead IS a provider's raw HTTP response body, its Body IS the AGENT's
// own sentence, and one of its Fields names an upstream-chosen tool. Unescaped,
// an agent writing `<https://attacker.example/x|Update credential>` gets a
// genuine-looking hyperlink ONE LINE from the real credential-entry button.
//
// The sweep is inertProse, BOTH halves — a Body saying "see
// https://attacker.example/reconnect" auto-links with every angle bracket
// perfectly escaped. `*`, `_` and backticks outside a link-bearing region stay
// live, because publishers compose those deliberately (see escapeSlackText).
//
// Lead is deliberately NOT escaped here: its sink on the card is noticeTitle's
// rich_text element, which Slack renders literally, so a forged `<url|label>`
// cannot become a link and escaping would show a reader "&amp;" for an ordinary
// ampersand. Lead is therefore the ONE field decided per SINK — escape where the
// surface parses markup, leave raw where it renders characters literally.
// Escaping a mrkdwn sink is lossless (Slack decodes the entities back), so a NEW
// Lead sink must pick a side explicitly. For a WIRE payload today:
//
//   - the card's rich_text title (noticeTitle) — LITERAL, left raw;
//   - the message's plain-text `text` field (notifyOption,
//     interaction_delivery.go) — mrkdwn, escapes itself. This is the
//     notification preview on every delivery, and a PUBLIC channel post on
//     postBroadcast's participants path;
//   - the DM thread title (SetAssistantThreadsTitle, deliverPrompt) — not a
//     markup surface, left raw;
//   - the resolved card's re-render (buildInteractionAppliedBlocks) — Lead to
//     the same literal title, Body/Fields to mrkdwn, which is why that function
//     runs this whole sweep itself rather than trusting the pending card.
//
// buildNoticeFallbackBlocks is NOT on that list: its callers (noticeMsgOptions
// and notice_post.go's two degrade branches) render in-process *notice.Notice
// copy, and sendRequest never degrades to it. That in-process path has five
// mrkdwn Lead sinks of its own and all five escape — two normal renderings
// (buildNoticeFallbackBlocks' section, noticeNotifyText's preview) and three
// degrade branches that post Args().Lead directly (postNoticeEphemeral,
// postNoticeInThread, postNoticeReturningTS). The five must agree: a degraded
// rendering that is MORE live than the one it replaced is the worst of both.
// Each degrade branch escapes at its CALL SITE, not inside postEphemeralText /
// postInThread, whose other callers pass already-inert noticeNotifyText output.
//
// Fields carrying Mentions are the other exception — see below.
//
// Audience.PublicNoteBody is NOT part of this sweep: it never reaches the card.
// Its sinks all parse markup (publicNoteBlocks' mrkdwn section,
// publicNoteNotifyText's escape=false plain-text field, both re-rendered every
// expiry tick), and it has its own escaper, publicNoteText, which must run
// BEFORE withApproverClause appends the live approver mentions — escaping here
// would be too late, since by render time those mentions share the string.
//
// The APPLIED payload is a different type this function never sees. Its two
// untrusted slots — InteractionAppliedPayload's OutcomeText and Reason, both
// RUNNER-controlled on the queued_messages path — are escaped by their own
// sinks, since the verdict line is composed into the body AFTER this sweep:
//
//   - the resolved card's verdict line (appliedVerdictLine) — mrkdwn section;
//     escapes both, then composes the live decider mention;
//   - the two chat.update edits and the response_url copy (sendDecisionApplied)
//     — mrkdwn; escape via notifyOption / escapeSlackText;
//   - the initiator fallback notice — escaped by notifyOption via deliverPrompt,
//     and must NOT be escaped again.
//
// See interactionOutcomeText for why that value stays raw at its source.
func escapePublisherPayload(p channelevents.InteractionRequestPayload) channelevents.InteractionRequestPayload {
	p.Body = inertProse(p.Body)
	p.NextStep = inertProse(p.NextStep)
	if len(p.Fields) == 0 {
		return p
	}
	fields := make([]channelevents.InteractionField, len(p.Fields))
	for i, f := range p.Fields {
		f.Label = inertProse(f.Label)
		// A field carrying Mentions is the exception: resolveFieldMentions has
		// already REPLACED its Value with markup THIS kind composed (`<@U123>`),
		// escaping the publisher's fallback text as it went. Escaping again
		// renders a literal "&lt;@U123&gt;", and info_leakage's "Would share
		// with" row would stop naming anyone clickably.
		if len(f.Mentions) == 0 {
			f.Value = inertProse(f.Value)
		}
		fields[i] = f
	}
	p.Fields = fields
	return p
}

// escapeMetaagentApproval / escapeMetaagentRef are the metaagent card's half of
// the same sweep, over the six publisher-supplied slots the wire payload and the
// cached ref both carry: Requester, Verbatim, ApproverSummary, SkippedExplain,
// CaveatExplain, CleanedTask.
//
// Six, not every field of scope.MetaagentApprovalPayload: the sweep is per-SINK,
// and these are the ones this kind drops into a mrkdwn region. Applied /
// Skipped / Caveats are structured scope values no renderer here turns into
// markup, RequestID is minted by authzd rather than supplied by the requester,
// and ColdStart is a bool. A new payload field only earns a sweep entry when a
// renderer starts emitting it as mrkdwn.
//
// They are a PAIR because the same six values reach three renderers as two
// types: the card (buildMetaagentScopeApprovalBlocks) plus its notification
// preview, Show Details (renderMetaagentShowDetailsText), and the permanent
// cold-start record (renderMetaagentColdStartResolution) — the last two from the
// in-process ref cache or, after a channelsd restart, from authzd's durable
// metaagent_audit record. Each renderer sweeps at its own top, so the six are
// made inert once per SINK; nothing is escaped on the way into the cache, which
// stays the record of what was published (escaping twice doubles the entities).
//
// None of the six is this kind's own text: Verbatim is the requester's raw Slack
// message with the bot mention stripped, and the four explain/summary slots are
// the composer LLM's prose about it. The card is posted NON-ephemerally into the
// session thread with real Approve / Deny / Run-without-scope buttons, so a
// `<url|label>` in any slot renders a forged action one line above genuine ones,
// to the person deciding whether to widen the agent's permissions.
//
// inertProse, not inertExcerpt, even for the two BLOCKQUOTED regions: there is
// no surrounding fence to break — Verbatim and CleanedTask are quoted with "> "
// — and rewriting ``` would mangle a legitimate code fence in the very request
// the approver must read accurately. defuseBareLinks is fence-aware for the same
// reason: a fence the requester typed survives and only the text outside it is
// swept, which costs nothing because Slack never linkifies through a fence.
//
// The bare-URL half matters most here: a "> " blockquote is an ordinary mrkdwn
// region, so "details at https://attacker.example/x" auto-links inside the
// quoted request with no markup for an escape to catch.
//
// Requester takes escapeSlackText alone: it is an id every renderer wraps in
// `<@…>` markup of its own, so an unescaped ">" would close the mention early
// and let the remainder render as markup of the publisher's choosing, while
// defusing would put a code span inside the mention. The escape is a no-op on
// real channel-native ids, and must run BEFORE the `<@…>` is composed — the same
// order property publicNoteText and buildInteractionAppliedBlocks turn on.
func escapeMetaagentApproval(p scope.MetaagentApprovalPayload) scope.MetaagentApprovalPayload {
	p.Requester = escapeSlackText(p.Requester)
	p.Verbatim = inertProse(p.Verbatim)
	p.ApproverSummary = inertProse(p.ApproverSummary)
	p.SkippedExplain = inertProse(p.SkippedExplain)
	p.CaveatExplain = inertProse(p.CaveatExplain)
	p.CleanedTask = inertProse(p.CleanedTask)
	return p
}

// escapeToolApprovalDetails is the Show-Details MODAL's half of the same sweep,
// over all seven slots of the wire ToolApprovalDetails blob.
//
// The modal needs its own sweep because the blob never passes through
// escapePublisherPayload: Details is a json.RawMessage the card renders as a
// BUTTON, and both of the modal's resolution legs hand it over verbatim — the
// in-process delivery cache and the durable memapproval record.
//
// It is not publisher copy: Justification is the summarizer LLM's prose,
// ToolDescription is an UPSTREAM MCP server's (see
// pkg/agent/runner/host_approval.go), and ArgsJSON is the model's own tool-call
// arguments. The same justification string is also a Field.Value on the pending
// card, where escapePublisherPayload makes it inert — leaving it live here would
// put one live rendering beside one escaped one, with the live one on the
// ground-truth view an approver opens BEFORE clicking Approve.
//
// The slots take THREE treatments, one per sink:
//
//   - ToolDescription and Justification are free PROSE in their own sections —
//     inertProse. Escaping alone left a bare URL in an upstream tool description
//     auto-linking on that ground-truth view. The modal composes no live markup
//     of its own, so nothing collides with the defuse.
//   - Permission / ResourceType / ResourceID / StateImpact / ArgsHash land
//     INSIDE a span this renderer composes — inertSpanValue. Backticks are live
//     by design, so one in ResourceID (bound from the MODEL's arguments) closes
//     that span early; defusing instead would nest a span and re-open it.
//   - ArgsJSON alone takes inertExcerpt, because its sink alone is a code FENCE:
//     escaping & < > does not stop a ``` inside an argument value from closing
//     the fence and letting the rest render as live mrkdwn. Same treatment as
//     every other fenced untrusted region (buildInteractionBlocks' and
//     buildNoticeFallbackBlocks' Excerpt), and the opposite of the metaagent
//     card's blockquotes, which have no fence to break.
//
// Called at the TOP of renderInteractionDetailsModal, BEFORE its caps run:
// sweeping after would let 2800 runes of "<" expand to 11200 characters and blow
// Slack's 3000-character section limit, failing the whole modal with an opaque
// invalid_blocks. The caps that follow are delimiter-aware (capInertRunes)
// precisely because this order puts them after a sweep emitting PAIRED
// backticks.
func escapeToolApprovalDetails(d channelevents.ToolApprovalDetails) channelevents.ToolApprovalDetails {
	d.Permission = inertSpanValue(d.Permission)
	d.ResourceType = inertSpanValue(d.ResourceType)
	d.ResourceID = inertSpanValue(d.ResourceID)
	d.StateImpact = inertSpanValue(d.StateImpact)
	d.ArgsHash = inertSpanValue(d.ArgsHash)
	d.ToolDescription = inertProse(d.ToolDescription)
	d.Justification = inertProse(d.Justification)
	d.ArgsJSON = inertExcerpt(d.ArgsJSON)
	return d
}

func escapeMetaagentRef(r MetaagentApprovalRef) MetaagentApprovalRef {
	r.Requester = escapeSlackText(r.Requester)
	r.Verbatim = inertProse(r.Verbatim)
	r.ApproverSummary = inertProse(r.ApproverSummary)
	r.SkippedExplain = inertProse(r.SkippedExplain)
	r.CaveatExplain = inertProse(r.CaveatExplain)
	r.CleanedTask = inertProse(r.CleanedTask)
	return r
}

// inertExcerpt neutralizes Slack-active sequences and code-fence breakers on
// untrusted excerpt content before it is placed inside a code fence. Rules, in
// order: (1) replace ``` with modifier apostrophes so the excerpt cannot break
// the surrounding fence; (2) HTML-escape & < > so mrkdwn never reads them as
// entities, <!channel>/<!here> pings, or tags.
func inertExcerpt(s string) string {
	// Step 1: neutralize backtick triplets so the excerpt can't break the fence.
	s = strings.ReplaceAll(s, fenceDelimiter, strings.Repeat(inertApostrophe, len(fenceDelimiter)))
	// Step 2: HTML-escape & < > (covers <!channel>, <!here>, and arbitrary tags).
	return escapeSlackText(s)
}
