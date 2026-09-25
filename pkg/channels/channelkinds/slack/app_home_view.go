// pkg/channels/channelkinds/slack/app_home_view.go
//
// Block Kit view builder for the Slack App Home tab. Each OAP agent is a
// SEPARATE Slack app (its own bot token → its own Socket Mode connection →
// its own listener), so the Home tab a user opens belongs to exactly ONE
// agent — the one this listener fronts (l.deps.Channel.Spec.AgentClass). The
// view is therefore a focused page ABOUT that single agent, not a directory of
// every agent in the workspace.
//
// Layout (single agent, full-width — no boxed card):
//
//	*<DisplayName>*                                    ← section (hero name)
//	🤖 Uses an operator account · has its own creds    ← context (identity badge)
//	<Description>                                       ← section
//	──────────────────────────────
//	*Your connections*                                 ← section (passthrough only)
//	✅ GitHub — linked
//	❌ Linear — not linked yet
//	[ Manage connections ]                             ← actions
//	──────────────────────────────
//	*Preferences — <class>*                            ← preferences (self-submitting)
//	[ … ]
//	──────────────────────────────
//	💡 Tag this agent in any channel…                  ← context (footer)
//	Powered by OAP · Open Agent Primitives             ← context (branding)
//
// Re-publish cadence: every app_home_opened event triggers a fresh
// views.publish (no cache). Slack's per-app per-user view IS the cache; our
// own layer would only introduce staleness.
//
// Slack constraints honored: one agent's page is well under the 100-block
// view cap even with many preference keys, so there is no truncation loop.
package slack

import (
	"strings"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// appHomeViewInput is what internal/cmd/channelsd assembles before calling the
// view builder. The handler reads:
//   - Agent: the single AgentClass this listener's Channel is bound to, resolved
//     for the CURRENT user (linked/required services, preferences, portal link).
//     nil when the class could not be loaded — the view then renders the
//     empty state rather than a blank tab.
//   - ExternalBaseURL: identityd's externally reachable URL used to compose the
//     per-credential icon image URL for the connections section. Empty → icon
//     omitted; the section renders text-only.
type appHomeViewInput struct {
	Agent           *appHomeAgent
	ExternalBaseURL string
}

// appHomeAgent is the single agent this Home tab is about.
type appHomeAgent struct {
	Name        string // metadata.name — stable identifier
	DisplayName string // Spec.DisplayName or Name fallback — the hero title
	Description string // Spec.Description (when present)

	// IdentityMode: "" (operator-default) or "userPassthrough".
	IdentityMode string

	// LinkedServices lists the services the CURRENT user has linked AND that
	// this AgentClass uses, each carrying its user-facing provider label and
	// the credential name that resolved to it.
	//
	// ONE slice of pairs, not a label slice beside a credential-name slice:
	// the section reads a label and its credential together (the connections
	// lines and the icon URL), and the resolver that produces them drops
	// credentials with no backing MCPServer, dedups by label and sorts — so
	// two parallel slices are neither equal-length nor co-indexed, which is
	// what made the icon slot panic. See passthroughcatalog.LinkedService.
	LinkedServices []passthroughcatalog.LinkedService

	// AllRequiredServiceLabels lists every service THIS AgentClass uses (union
	// of provider labels resolved from its MCPServers). Drives the "needs X
	// but not yet linked" half of the connections section.
	AllRequiredServiceLabels []string

	// PortalLinkURL: pre-minted /my/accounts deep-link for THIS user. Empty →
	// omit the Manage button (signer unavailable).
	PortalLinkURL string

	// ClassRef is "<namespace>/<name>" — the ref renderPreferenceBlocks and the
	// per-class Save button's action_id encode (the decode contract in
	// app_home_pref_interaction.go). Set only when this class is in scope for a
	// preferences section (see Preferences / PreferencesLoadFailed); "" otherwise.
	ClassRef string

	// Preferences carries the CURRENT user's resolved values for this class's
	// declared userPreferences — populated only when the user has INTERACTED
	// with this class (LookupPersonalizableClassRefs) and the class declares at
	// least one. Outside either condition this stays nil and renders no section.
	Preferences []preferences.Resolved

	// PreferencesLoadFailed is true when this class WAS in scope for a
	// preferences section (interacted + declares keys) but resolving the user's
	// values failed, or no preferences client was configured. The page then
	// renders a "couldn't load" notice instead of silently showing nothing,
	// which would be indistinguishable from "this agent has no preferences".
	PreferencesLoadFailed bool
}

// buildAppHomeView produces the slackapi.HomeTabViewRequest payload passed to
// views.publish. Exactly one agent, or the empty state.
func buildAppHomeView(in appHomeViewInput) slackapi.HomeTabViewRequest {
	var blocks []slackapi.Block
	if in.Agent == nil {
		blocks = emptyStateBlocks()
	} else {
		blocks = agentPageBlocks(*in.Agent, in.ExternalBaseURL)
	}
	return slackapi.HomeTabViewRequest{
		Type:   slackapi.VTHomeTab,
		Blocks: slackapi.Blocks{BlockSet: blocks},
	}
}

// emptyStateBlocks renders when the bound AgentClass could not be loaded (an
// unbound Channel, a missing class, a failed lookup). A blank Home tab makes
// users wonder if the app is broken; this says what happened without claiming
// something false.
func emptyStateBlocks() []slackapi.Block {
	return []slackapi.Block{
		slackapi.NewHeaderBlock(slackapi.NewTextBlockObject(
			slackapi.PlainTextType, "Nothing to show here yet", false, false)),
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				"This agent isn't available right now. Try reopening this tab, or check with your workspace admin that it's configured.",
				false, false),
			nil, nil),
		brandingFooterBlock(),
	}
}

// agentPageBlocks builds the focused page for the one agent this Home tab is
// about. Full-width section / context / divider blocks — a page, not a card in
// a list.
//
// Everything below reads FINISHED text: appHomePageText owns normalize → sweep
// → cap for every AgentClass-supplied slot, and this function transforms
// nothing it is handed. That split is the fix for two separate injection
// escapes, both of which were a transform standing next to the sweep rather
// than inside its pipeline — see appHomePageText and inertText.
func agentPageBlocks(a appHomeAgent, externalBaseURL string) []slackapi.Block {
	slots := appHomePageText(a)

	// Hero name (bold) + identity badge subtitle. The name is swept and lands
	// in a MarkdownType block, so a class-supplied DisplayName cannot forge a
	// link here.
	blocks := []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "*"+slots.Title+"*", false, false),
			nil, nil),
		slackapi.NewContextBlock("identity_"+a.Name,
			slackapi.NewTextBlockObject(slackapi.MarkdownType, slots.Badge, false, false)),
	}

	// Description (both modes). The emptiness test is on the FINISHED text: a
	// description of only whitespace or a lone italic marker normalizes to "",
	// and Slack rejects a text object with an empty string outright
	// (invalid_blocks), taking the whole view down with it.
	if slots.Description != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, slots.Description, false, false),
			nil, nil))
	}

	// Connections + Manage button (passthrough only — operator-mode agents
	// have nothing for the user to link).
	if a.IdentityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		blocks = append(blocks, slackapi.NewDividerBlock())
		if slots.Connections != "" {
			// Icon: the first linked service's favicon as a right-aligned
			// accessory, when externalBaseURL is configured and a credential is
			// linked. Slack fetches it server-side; empty URL means no icon. The
			// URL and its alt text come off the SAME pair — appHomePageText reads
			// both off one LinkedService — so there is no second slice whose
			// length this guard has to also be right about.
			var accessory *slackapi.Accessory
			if externalBaseURL != "" && slots.IconCredential != "" {
				if iconURL := passthrough.IconURL(externalBaseURL, slots.IconCredential); iconURL != "" {
					accessory = slackapi.NewAccessory(slackapi.NewImageBlockElement(iconURL, slots.IconAlt))
				}
			}
			blocks = append(blocks, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType, slots.Connections, false, false),
				nil, accessory))
		}
		if a.PortalLinkURL != "" {
			btn := slackapi.NewButtonBlockElement(
				"home:manage_connections:"+a.Name,
				a.Name,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Manage connections", false, false),
			).WithURL(a.PortalLinkURL)
			blocks = append(blocks, slackapi.NewActionBlock("manage_actions:"+a.Name, btn))
		}
	}

	// Preferences. PreferencesLoadFailed and a real Preferences list are
	// mutually exclusive by construction (app_home.go sets exactly one), so
	// this is an if/else, not two independent checks.
	switch {
	case a.PreferencesLoadFailed:
		blocks = append(blocks,
			slackapi.NewDividerBlock(),
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType,
					":warning: couldn't load your preferences for this agent — try reopening this tab.",
					false, false),
				nil, nil,
				slackapi.SectionBlockOptionBlockID("pref_load_failed:"+a.ClassRef)),
		)
	case len(a.Preferences) > 0:
		blocks = append(blocks, slackapi.NewDividerBlock())
		blocks = append(blocks, renderPreferenceBlocks(a.ClassRef, a.Preferences)...)
		// Bool/enum keys self-submit via dispatch_action on change; free-text
		// keys don't, so a Save button is only needed when at least one is present.
		if hasFreeTextPreference(a.Preferences) {
			saveBtn := slackapi.NewButtonBlockElement(
				"home:pref_save:"+a.ClassRef,
				a.ClassRef,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Save preferences", false, false),
			)
			blocks = append(blocks, slackapi.NewActionBlock("pref_save_actions:"+a.ClassRef, saveBtn))
		}
	}

	// Footer: how to use, plus OAP branding.
	blocks = append(blocks,
		slackapi.NewDividerBlock(),
		slackapi.NewContextBlock("home_footer",
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				":bulb: Tag this agent in any channel where the bot is invited to start a conversation. Anything you say there runs as you — your linked accounts power what the agent does on your behalf.",
				false, false),
		),
		brandingFooterBlock(),
	)
	return blocks
}

// brandingFooterBlock is the one place the product name appears on this
// surface — muted, at the very bottom, so the page stays about the agent.
func brandingFooterBlock() slackapi.Block {
	return slackapi.NewContextBlock("home_branding",
		slackapi.NewTextBlockObject(slackapi.MarkdownType,
			"Powered by *OAP* · Open Agent Primitives", false, false),
	)
}

// identityBadgeShort is the compact identity line used as the hero subtitle.
func identityBadgeShort(identityMode string) string {
	if identityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		return ":bust_in_silhouette: *Uses YOUR account* — calls services as you."
	}
	return ":robot_face: *Uses an operator account* — has its own credentials."
}

// appHomePageSlots is the FINISHED text for the page — normalized, swept and
// capped, ready to hand to Block Kit unchanged. agentPageBlocks reads it and
// transforms nothing.
//
// The struct exists so the pipeline lives in ONE function body instead of being
// spread across the renderer, where a later step could get between two earlier
// ones. Both of the escapes this surface has shipped were exactly that: an
// unwrap that ran after the sweep and stripped the character the sweep had
// tripped over, and a rune cap that ran after the sweep and cut the code span
// it had added in half.
type appHomePageSlots struct {
	// Title is the swept+capped DisplayName, wrapped in bold by the renderer.
	Title string
	// Badge is the identity line — this package's own sentence, capped.
	Badge string
	// Description is the full swept description (no rune budget — a full-width
	// section has room the capped slots do not).
	Description string
	// Connections is the per-service ✅/❌ block (passthrough only), swept
	// labels composed with this package's own glyphs. "" when there is nothing
	// honest to say.
	Connections string
	// IconCredential composes the identityd icon URL and is deliberately RAW —
	// its sink is a URL, not mrkdwn, and escaping it would corrupt the fetch.
	// IconAlt is the same LinkedService's label, swept; the two are read off one
	// pair so no length guard has to be right about a second slice.
	IconCredential string
	IconAlt        string
}

// appHomeTitleRunes is Slack's practical budget for the hero name. Named
// because slot() has to spend one of its runes on re-closing a truncated code
// span.
const appHomeTitleRunes = 150

// appHomePageText runs this surface's whole text pipeline, in the one order
// that is correct: NORMALIZE (trim, unwrap italic markers) → SWEEP (escape +
// defuse bare links) → COMPOSE (this package's own glyphs and emphasis) → CAP.
//
// Each step's position is forced by its own argument:
//
//   - NORMALIZE before SWEEP: normalizing removes characters, and removing one
//     from swept text un-decides what the sweep decided — a stripped leading
//     "_" turns a string the sweep passed over into a live bare URL.
//   - CAP after SWEEP: the sweep only ever expands (one "&" becomes five
//     characters, each defused link gains two backticks), so capping first lets
//     a swept title overflow the slot Slack enforces. Same ordering
//     escapeToolApprovalDetails establishes for the details modal.
//   - therefore the CAP must know the sweep's delimiters, since it is cutting a
//     string with PAIRS in it. That is slot(), not truncateRunes.
//
// inertText holds all three: the normalizers take a string and the sweep
// returns an inertText, so nothing downstream typechecks against a normalizer,
// and the only exits are String() and the delimiter-aware slot().
//
// These are not the platform's words. DisplayName and Description are
// AgentClass.Spec fields supplied by whatever .oap bundle was installed or by
// any principal with AgentClass write RBAC, and the service labels resolve from
// the class's own MCPServers. They render one line above the REAL "Manage
// connections" button — this is where a user goes to link credentials, so a
// forged "Connect your account" link is worth more here than anywhere else this
// package draws.
//
// SWEPT, and why each is text:
//   - DisplayName — the hero name. The empty-string fallback to Name is
//     resolved HERE so the fallback cannot slip past the sweep and reach the
//     title unescaped.
//   - Description — the description section. Swept ONCE, because the sweep is
//     not idempotent.
//   - LinkedServices[].Label / AllRequiredServiceLabels — the ✅/❌ connections
//     lines. Swept per label, before connectionsBlock composes its own markup
//     around them; the two lists are compared against each other in there, and
//     sweeping both keeps that comparison intact.
//
// NOT swept, deliberately:
//   - Name is an IDENTIFIER, not text: it composes the "identity_<name>" block
//     id and the "home:manage_connections:<name>" action id, and rides as the
//     button's value. Escaping it would change what a click carries. Its only
//     path to a text slot is the DisplayName fallback above.
//   - PortalLinkURL is a URL slot (WithURL), and LinkedServices[].CredentialName
//     composes an image URL — neither is mrkdwn, and escaping either would
//     corrupt a link the user is meant to follow.
//   - identityBadgeShort is this package's own sentence. It is capped with a
//     plain truncateRunes because it carries no code span for a cut to split.
//
// The caller's appHomeAgent is read, never written: every swept value lands in
// a new slice or a local.
func appHomePageText(a appHomeAgent) appHomePageSlots {
	display := a.DisplayName
	if display == "" {
		display = a.Name
	}
	// unwrapItalicMarkers BEFORE the sweep. It removes characters, and removing
	// a character from swept text is how a URL the sweep had already decided
	// about becomes live again.
	description := inertAppHomeText(unwrapItalicMarkers(a.Description))

	linked := make([]inertText, 0, len(a.LinkedServices))
	for _, s := range a.LinkedServices {
		linked = append(linked, inertAppHomeText(s.Label))
	}

	slots := appHomePageSlots{
		Title:       inertAppHomeText(display).slot(appHomeTitleRunes),
		Badge:       truncateRunes(identityBadgeShort(a.IdentityMode), appHomeTitleRunes),
		Description: description.String(),
	}
	if a.IdentityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		slots.Connections = connectionsBlock(linked, inertAppHomeTexts(a.AllRequiredServiceLabels)).String()
	}
	if len(a.LinkedServices) > 0 {
		slots.IconCredential = a.LinkedServices[0].CredentialName
		slots.IconAlt = linked[0].String()
	}
	return slots
}

// unwrapItalicMarkers trims whitespace and drops ONE leading and ONE trailing
// underscore, so a description that arrived already wrapped in Slack's italic
// markers (a common shape from YAML-folded manifests: "_Routes bug reports…_")
// does not render as double italics.
//
// It is COSMETIC, and its name says so — stripping an underscore is not a
// security control; appHomePageText is. Naming it as though it sanitized is how
// a reader auditing this surface's mrkdwn sinks mistook it for a guard.
//
// Being cosmetic does not stop it from breaking one. Run at a Description sink
// AFTER the sweep, "_https://attacker.example/connect-your-account" has its "_"
// stripped here and renders as a live bare URL one line above the real
// credential-linking button. It therefore has exactly ONE caller, inside
// appHomePageText and AHEAD of the sweep, which is where a character-removing
// transform on this surface belongs.
func unwrapItalicMarkers(s string) string {
	s = strings.TrimSpace(s)
	// Common YAML-folded multi-line descriptions start with a stray underscore
	// from `description: |_…`. Trim a single leading and trailing underscore so
	// we don't render *double* italics.
	s = strings.TrimPrefix(s, "_")
	s = strings.TrimSuffix(s, "_")
	return s
}

// connectionsBlock renders the per-service ✅/❌ summary of the user's linked
// services FOR THIS AGENT, one line per service.
//
// Returns "" when the agent has no required services AND the user has nothing
// linked relevant to it — better to render nothing than a misleading "doesn't
// need anything" line. Empty required + empty linked happens transiently when
// the operator hasn't reconciled yet OR when our credential-name lookup misses.
//
// It takes and returns inertText: the labels arrive already swept, and what it
// composes AROUND them — the glyphs, "*Your connections*", the CTA — is this
// package's own live markup, which sweeping the composed block would render as
// literal asterisks. The two sets are matched on the swept values, so both sides
// have to have been swept the same way for the comparison to hold.
func connectionsBlock(linked, required []inertText) inertText {
	if len(required) == 0 && len(linked) == 0 {
		// Honest silence: we couldn't determine required services. Don't claim
		// "doesn't need anything" because that's frequently wrong.
		return composeInert("")
	}
	if len(required) == 0 {
		// Linked services exist but we couldn't tie them to this specific
		// agent's class. Still informative: name what the user has linked.
		lines := make([]string, 0, len(linked))
		for _, l := range linked {
			lines = append(lines, ":white_check_mark: "+l.String()+" — linked")
		}
		return composeInert("*Your connections*\n" + strings.Join(lines, "\n"))
	}
	linkedSet := make(map[inertText]struct{}, len(linked))
	for _, l := range linked {
		linkedSet[l] = struct{}{}
	}
	lines := make([]string, 0, len(required))
	for _, svc := range required {
		if _, ok := linkedSet[svc]; ok {
			lines = append(lines, ":white_check_mark: "+svc.String()+" — linked")
		} else {
			lines = append(lines, ":heavy_multiplication_x: "+svc.String()+" — not linked yet")
		}
	}
	body := "*Your connections*\n" + strings.Join(lines, "\n")
	if len(linked) == 0 {
		body += "\n_Nothing linked yet — tap *Manage connections* below to link them._"
	}
	return composeInert(body)
}
