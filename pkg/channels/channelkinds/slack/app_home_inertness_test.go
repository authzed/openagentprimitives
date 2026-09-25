// pkg/channels/channelkinds/slack/app_home_inertness_test.go
//
// Markup inertness for the App Home tab — the surface whose real buttons link
// the user's CREDENTIALS, which is what makes a forged "Connect your account"
// link rendered one line above them worth more here than anywhere else in this
// package.
//
// The values are AgentClass.Spec.DisplayName / .Description and the provider
// labels resolved from the class's MCPServers. They are supplied by an
// installed .oap bundle or by any principal with AgentClass write RBAC — the
// same third-party trust level this package already declared untrusted for
// skill bodies — and they render into MarkdownType blocks.
//
// What let this survive an enumeration that claimed to cover every mrkdwn sink
// in the package was a helper NAME: sanitizeDescription documented itself as
// scrubbing "for safe rendering" and stripped a leading underscore. A reader
// checking the sinks saw a guard and moved on. The helper is now named for what
// it does.
package slack

import (
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
)

// bareLureURL is the OTHER form of the same forged-action threat, and the one a
// guard built out of escapeSlackText alone does not close: Slack linkifies a
// bare URL in a mrkdwn text object with no `<url|label>` markup involved, so
// escaping & < > leaves it as clickable as the masked form it does close.
//
// It carries no angle brackets on purpose. A row using lureLink can pass on the
// strength of "<" having become "&lt;" — which measures the escaper's
// REPRESENTATION, not whether the surface still renders a link.
const bareLureURL = "https://attacker.example.invalid/connect-your-account"

// liveLinkIn returns the first URL-shaped run in s that Slack would still
// linkify — one that is inside neither a ``` code FENCE nor an inline code
// SPAN, the two regions Slack renders literally and does not linkify through.
//
// Deliberately written from the RENDERING rule rather than by calling the
// production defuser: a guard checked with its own implementation agrees with
// itself no matter what either of them does.
//
// Both regions need BOTH of their delimiters, and that is the whole of the
// rule:
//
//   - a FENCE opened by ``` and never closed protects nothing. Slack makes no
//     promise about how it renders an unterminated fence, so an oracle that
//     credited the guard for one would be grading it against a rendering nobody
//     can guarantee — which is the same fail-closed reading defuseBareLinks
//     takes when it sweeps an unterminated tail "as though it were outside".
//     This helper was FENCE-BLIND, and that is precisely what let a whole
//     defect class ship: a transform that deletes a closing fence leaves the
//     region it opened unswept AND unprotected, and nothing in the suite could
//     see it.
//   - an inline SPAN needs both backticks too. An UNPAIRED backtick opens
//     nothing — Slack renders it as a literal character and linkifies the URL
//     after it — so within each unfenced region the backticks are paired up
//     FIRST and only the text between a pair counts as code. Toggling an "in
//     code" flag on each backtick instead lets a trailing lone backtick swallow
//     the whole remainder, reporting "no live link" for exactly the string a
//     rune cap produces when it cuts between the two backticks the sweep added.
//
// The two nest one way only — a fence's content is literal, so backticks inside
// it are characters, not delimiters — which is why the fence split runs first
// and the pairing runs per surviving region rather than over the whole string.
// Pairing globally lets a fence's three delimiters stand in as span boundaries
// in both directions: it reported a URL inside an orphaned fence as safely
// spanned whenever one stray backtick followed it, and reported a URL inside a
// legitimate fence as live whenever one stray backtick preceded it.
func liveLinkIn(s string) string {
	parts := strings.Split(s, fenceDelimiter)
	// parts[len-1] is the tail. An even part count means an ODD number of
	// delimiters, so the fence that opened the tail never closes: the tail is
	// judged as ordinary mrkdwn, not as code.
	tailUnterminated := len(parts)%2 == 0
	for i, part := range parts {
		if i%2 == 1 && !(tailUnterminated && i == len(parts)-1) {
			continue // inside a terminated fence: literal, never linkified
		}
		if m := liveLinkOutsideSpans(part); m != "" {
			return m
		}
	}
	return ""
}

// liveLinkOutsideSpans is liveLinkIn's body for ONE region known not to be
// inside a terminated fence: the first URL-shaped run that no inline code span
// covers.
func liveLinkOutsideSpans(s string) string {
	var ticks []int
	for i := range s {
		if s[i] == '`' {
			ticks = append(ticks, i)
		}
	}
	inSpan := func(i int) bool {
		for p := 0; p+1 < len(ticks); p += 2 {
			if i > ticks[p] && i < ticks[p+1] {
				return true
			}
		}
		return false
	}
	schemes := []string{"https://", "http://", "mailto:", "www."}
	for i := range s {
		if inSpan(i) {
			continue
		}
		for _, scheme := range schemes {
			if strings.HasPrefix(s[i:], scheme) {
				return strings.Fields(s[i:])[0]
			}
		}
	}
	return ""
}

// TestLiveLinkIn_ModelsSlacksTwoLiteralRegions pins the ORACLE, because every
// inertness guard in this package is only as good as it — and it was wrong.
//
// It graded a whole defect class clean by pairing backticks across the whole
// string with no notion of a fence: a fence's three delimiters stood in as span
// boundaries, in both directions. A test for a helper a test uses looks
// redundant right up until the helper is the reason the suite was green.
func TestLiveLinkIn_ModelsSlacksTwoLiteralRegions(t *testing.T) {
	cases := []struct {
		name string
		text string
		live bool
	}{
		{
			name: "a bare URL in open prose is live",
			text: "see " + bareLureURL,
			live: true,
		},
		{
			name: "...and inert inside a closed inline span",
			text: "see `" + bareLureURL + "`",
		},
		{
			name: "...but live again when the closing backtick is gone: an unpaired one opens nothing",
			text: "see `" + bareLureURL,
			live: true,
		},
		{
			name: "inside a TERMINATED fence it is inert, even with a stray backtick before it",
			text: fenceDelimiter + "\nlet x = `a\nsee " + bareLureURL + "\n" + fenceDelimiter,
		},
		{
			name: "inside an ORPHANED fence it is live: an unterminated fence promises nothing",
			text: "lede " + fenceDelimiter + "see " + bareLureURL,
			live: true,
		},
		{
			name: "...and still live when a stray backtick follows it, which whole-string pairing hid",
			text: "lede " + fenceDelimiter + "cfg `k` " + bareLureURL + " `",
			live: true,
		},
		{
			name: "a URL after a terminated fence is judged on its own region",
			text: fenceDelimiter + "\nx\n" + fenceDelimiter + "\nsee " + bareLureURL,
			live: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := liveLinkIn(tc.text)
			if tc.live {
				assert.NotEmpty(t, got, "Slack linkifies this and the oracle must say so")
				return
			}
			assert.Empty(t, got, "Slack renders this literally and the oracle must not cry wolf")
		})
	}
}

// appHomeText walks the rendered view and returns every mrkdwn string a reader
// sees — card title/subtitle/body plus the section and context blocks around
// them. Marshalling to JSON would not do: encoding/json escapes "<" to "<"
// of its own accord, which would make an unescaped sink look escaped.
func appHomeText(view slackapi.HomeTabViewRequest) string {
	var out string
	add := func(t *slackapi.TextBlockObject) {
		if t != nil {
			out += t.Text + "\n"
		}
	}
	for _, b := range view.Blocks.BlockSet {
		switch v := b.(type) {
		case *slackapi.CardBlock:
			add(v.Title)
			add(v.Subtitle)
			add(v.Body)
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

// TestBuildAppHomeView_ThirdPartyClassTextIsInert puts a forged link in one
// slot per row, so dropping any one field from the sweep fails exactly one row —
// and runs every row against BOTH forms Slack renders as a link: the masked
// `<url|label>` span and a bare URL, which needs no markup at all.
func TestBuildAppHomeView_ThirdPartyClassTextIsInert(t *testing.T) {
	slots := []struct {
		name  string
		agent func(lure string) appHomeAgent
	}{
		{
			name: "a class DisplayName cannot forge a link in the card title",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{
					Name: "triage", DisplayName: "Triage " + lure,
					IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
				}
			},
		},
		{
			name: "a class Description cannot forge a link above the card",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{
					Name: "triage", DisplayName: "Triage", Description: "Routes reports. " + lure,
					IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
				}
			},
		},
		{
			name: "...nor in the operator-mode card body, where the description lands instead",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{
					Name: "triage", DisplayName: "Triage", Description: "Routes reports. " + lure,
				}
			},
		},
		{
			name: "a display name falls back to metadata.name, which is swept too",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{Name: "triage " + lure}
			},
		},
		{
			name: "a linked service label cannot forge a link in the connections line",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{
					Name: "triage", DisplayName: "Triage",
					IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
					LinkedServices:           []passthroughcatalog.LinkedService{{CredentialName: "tracker-token", Label: "Tracker " + lure}},
					AllRequiredServiceLabels: []string{"Tracker " + lure},
				}
			},
		},
		{
			name: "a required-but-unlinked service label cannot forge one either",
			agent: func(lure string) appHomeAgent {
				return appHomeAgent{
					Name: "triage", DisplayName: "Triage",
					IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
					AllRequiredServiceLabels: []string{"Tracker " + lure},
				}
			},
		},
	}
	forms := []struct {
		form string
		lure string
		// visible is the part of the lure the reader must still be able to read
		// after the sweep. Inert is not deleted.
		visible string
	}{
		{form: "masked <url|label>", lure: lureLink, visible: "attacker.example.invalid/update"},
		{form: "bare URL Slack auto-links", lure: bareLureURL, visible: "attacker.example.invalid/connect-your-account"},
		// An unbalanced backtick is the way OUT of a code span: the author's
		// backtick pairs with the opening one the defuser adds, closing the span
		// early and leaving the URL live with a stray backtick after it. A
		// defuser that wraps without neutralizing what it wraps around defends
		// against nothing an author who reads its source cannot step around.
		{form: "bare URL escaping its code span with a stray backtick", lure: "` " + bareLureURL, visible: "attacker.example.invalid/connect-your-account"},
		// A leading WORD character is the way out of a sweep anchored on a word
		// boundary: there is no boundary between "_" (or a digit, or a letter)
		// and the "h" of "https", so the scheme is unmatchable and the whole
		// string takes the defuser's early exit. Slack's linkifier has no such
		// rule — it linkifies the URL either way — and on the Description path
		// unwrapItalicMarkers then strips the "_" back off, so the reader is
		// left with a clean bare URL one line above the real credential button.
		{form: "bare URL a word character hides from a boundary-anchored sweep", lure: "_" + bareLureURL, visible: "attacker.example.invalid/connect-your-account"},
	}
	for _, slot := range slots {
		for _, f := range forms {
			t.Run(slot.name+" ("+f.form+")", func(t *testing.T) {
				a := slot.agent(f.lure)
				got := appHomeText(buildAppHomeView(appHomeViewInput{Agent: &a}))
				assert.Empty(t, liveLinkIn(got),
					"no class-supplied slot may render a clickable link on the surface whose real buttons link the user's credentials")
				assert.NotContains(t, got, lureLink,
					"the masked-link span in particular must never survive verbatim")
				assert.Contains(t, got, f.visible,
					"inert is not deleted — the user must still see what the class claimed")
			})
		}
	}
}

// TestBuildAppHomeView_ItalicUnwrapCannotUncoverALure is the whole-field form
// of the same evasion, and the one that needs no help from the reader: the
// Description is ONLY the lure, so the "_" the sweep tripped over sits at
// position 0 — where unwrapItalicMarkers, which runs AFTER the sweep at both
// Description sinks, strips it. The sweep concluded "this string has no URL in
// it" and a cosmetic transform standing next to it then made that false.
//
// Both identity modes are covered because the description renders the same way
// in each — a section on the page whose real button links the user's
// credentials — and a regression that diverged them must fail here, not slip
// through on the mode the table happens to omit.
func TestBuildAppHomeView_ItalicUnwrapCannotUncoverALure(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "passthrough: the description section renders no live link", mode: spiceboxv1alpha1.IdentityModeUserPassthrough},
		{name: "operator-default: the description section renders no live link", mode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appHomeText(buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{
				Name: "triage", DisplayName: "Triage",
				Description:  "_" + bareLureURL,
				IdentityMode: tc.mode,
			}}))
			assert.Empty(t, liveLinkIn(got),
				"a leading underscore must not carry the URL past the sweep for the unwrap to then expose")
			assert.Contains(t, got, "attacker.example.invalid/connect-your-account",
				"inert is not deleted — the user must still see what the class claimed")
		})
	}
}

// TestBuildAppHomeView_RuneCapCannotSplitTheDefusedSpan covers the OTHER
// adjacent transform: the cap, which the capped hero-name slot applies.
//
// The sweep emits PAIRED delimiters, and truncateRunes runs after it, so a cut
// that lands between the two backticks drops the closing one. An unpaired
// backtick opens no span in Slack, and the URL prefix left in front of the
// ellipsis still resolves — the attacker owns the whole host — so the slot
// renders a live link again. DisplayName carries no CRD length bound, so a
// single whitespace-free URL run straddling the cap is the author's to choose,
// with whatever prose they like in front of it as the lure.
//
// Every section on the page is also checked for balance and inertness: the
// uncapped description section never applies the cap, but it must still be
// swept, so no section anywhere renders a live link.
func TestBuildAppHomeView_RuneCapCannotSplitTheDefusedSpan(t *testing.T) {
	// One whitespace-free run, long enough to straddle the 150-rune hero cap.
	lure := "Connect here: https://attacker.example.invalid/" + strings.Repeat("a", 220)
	view := buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{
		Name: "triage", DisplayName: lure, Description: lure,
	}})

	heroChecked := false
	for _, b := range view.Blocks.BlockSet {
		sec, ok := b.(*slackapi.SectionBlock)
		if !ok || sec.Text == nil {
			continue
		}
		txt := sec.Text.Text
		assert.Empty(t, liveLinkIn(txt),
			"no section may leave a URL outside the span the sweep put it in")
		assert.Zero(t, strings.Count(txt, "`")%2,
			"every section's code spans stay balanced")
		// The hero name is the bold-wrapped, capped slot.
		if strings.HasPrefix(txt, "*") && strings.HasSuffix(txt, "*") {
			name := strings.TrimSuffix(strings.TrimPrefix(txt, "*"), "*")
			assert.LessOrEqual(t, len([]rune(name)), 150,
				"re-closing the span must not push the capped hero name past its slot")
			heroChecked = true
		}
	}
	require.True(t, heroChecked, "the fixture renders a hero-name section")
}

// TestBuildAppHomeView_IsIdempotentAndLeavesItsInputAlone pins two properties
// the sweep needs and neither of which the type system gives for free.
//
// escapeSlackText is single-pass but NOT idempotent ("&" → "&amp;" →
// "&amp;amp;"), and defuseBareLinks is not either (a second pass neutralizes
// the backticks the first one added). A surface that swept twice — because the
// renderer re-published, or because a helper swept a value its caller had
// already swept — would render entity soup, which is a rendering bug rather
// than a security one but is exactly as invisible. Re-publishing the same input
// is the App Home's NORMAL path: every app_home_opened event rebuilds the view.
func TestBuildAppHomeView_IsIdempotentAndLeavesItsInputAlone(t *testing.T) {
	in := appHomeViewInput{ExternalBaseURL: "https://identityd.example.invalid", Agent: &appHomeAgent{
		Name: "triage", DisplayName: "Triage & Co", Description: "Routes reports to " + bareLureURL,
		IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
		LinkedServices:           linkedServices("Tracker & Co"),
		AllRequiredServiceLabels: []string{"Tracker & Co", "Forge"},
	}}
	before := appHomeText(buildAppHomeView(in))
	after := appHomeText(buildAppHomeView(in))

	assert.Equal(t, before, after, "re-publishing the same input must render the same view")
	assert.NotContains(t, before, "&amp;amp;", "one pass of the escape, not two")
	assert.Equal(t, "Triage & Co", in.Agent.DisplayName, "the caller's agent must not be swept in place")
	assert.Equal(t, "Tracker & Co", in.Agent.LinkedServices[0].Label, "nor its label slice, which the caller owns")
	assert.Equal(t, []string{"Tracker & Co", "Forge"}, in.Agent.AllRequiredServiceLabels, "nor its required-label slice")
}

// The escape must not mangle the ordinary card. Everything the platform itself
// composes here — the emoji shortcodes, the bold identity badge, the ✓/✗
// connection glyphs — is written by this package and must stay live, and a
// plain description must render as written.
func TestBuildAppHomeView_PlatformComposedMarkupStaysLive(t *testing.T) {
	got := appHomeText(buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{
		Name: "triage", DisplayName: "Triage Bot",
		Description:              "Routes bug reports to the right team",
		IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
		LinkedServices:           linkedServices("Tracker"),
		AllRequiredServiceLabels: []string{"Tracker", "Forge"},
	}}))

	assert.Contains(t, got, "*Uses YOUR account*", "the identity badge this package writes stays bold")
	assert.Contains(t, got, ":white_check_mark: Tracker", "a linked service keeps its glyph")
	assert.Contains(t, got, ":heavy_multiplication_x: Forge", "an unlinked one keeps its own")
	assert.Contains(t, got, "Triage Bot", "the display name renders as written")
	assert.Contains(t, got, "Routes bug reports to the right team", "so does an ordinary description")
	assert.NotContains(t, got, "&amp;", "nothing here contains an ampersand to double-escape")
}

// Escaping expands (one "&" becomes five characters), so it has to run BEFORE
// the rune cap or a long description could push the card slot past the limit
// Slack enforces — the same ordering escapeToolApprovalDetails establishes for
// the details modal.
//
// That is only half the constraint, and pinning only this half is what let the
// cap split the sweep's code spans for as long as it did:
// TestBuildAppHomeView_RuneCapCannotSplitTheDefusedSpan pins the other half,
// which is that a cap running after the sweep has to know what the sweep added.
func TestBuildAppHomeView_EscapeRunsBeforeTheRuneCap(t *testing.T) {
	long := ""
	for i := 0; i < 400; i++ {
		long += "&"
	}
	view := buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{
		Name: "triage", DisplayName: long, Description: long,
	}})

	heroChecked := false
	for _, b := range view.Blocks.BlockSet {
		sec, ok := b.(*slackapi.SectionBlock)
		if !ok || sec.Text == nil {
			continue
		}
		txt := sec.Text.Text
		if strings.HasPrefix(txt, "*") && strings.HasSuffix(txt, "*") {
			name := strings.TrimSuffix(strings.TrimPrefix(txt, "*"), "*")
			assert.LessOrEqual(t, len([]rune(name)), 150,
				"the hero name stays within the slot AFTER escaping, not before it")
			heroChecked = true
		}
	}
	require.True(t, heroChecked, "the fixture renders a hero-name section")
}
