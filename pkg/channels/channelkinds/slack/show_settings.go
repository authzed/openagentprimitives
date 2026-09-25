// pkg/channels/channelkinds/slack/show_settings.go
//
// "Show settings" button + modal: makes the agent's resolved effectiveSettings
// reachable from Slack with a single click.
//
// Flow:
//
//  1. sender.Send appends a "Show settings" action block on every outgoing
//     user_message. The button value encodes the session ref (ns/name) using
//     encodeApprovalButtonValue(discShowSettings, ...). No status fetch at
//     send time — the button only needs the ref.
//
//  2. listener.onInteraction routes discShowSettings clicks here.
//     handleShowSettingsAction decodes the session ref, fetches the
//     AgentSession via l.deps.K8sClient, and opens a modal rendered by
//     renderSettingsModalBlocks.
//
//  3. renderSettingsModalBlocks is a pure function (no client) so it can be
//     unit-tested directly. A nil eff argument renders a "not yet resolved"
//     placeholder.
package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	// showSettingsActionID is the block_action id for the "Show settings"
	// button posted alongside agent messages. The listener routes clicks
	// to handleShowSettingsAction which opens the settings modal.
	showSettingsActionID = "show_settings"
)

// settingsButton builds a "Show settings" button block element. sessRef is
// "namespace/name" for the session; it is encoded into the button value so the
// modal handler can fetch fresh status on click without storing any server-side
// cache. The value is compact JSON under Slack's 2000-char cap.
func settingsButton(sessRef string) slackapi.BlockElement {
	return slackapi.NewButtonBlockElement(
		showSettingsActionID,
		encodeApprovalButtonValue(discShowSettings, "", "", sessRef),
		slackapi.NewTextBlockObject("plain_text", "Show settings", true, false),
	)
}

// renderSettingsModalBlocks builds the modal body from a resolved
// EffectiveSettings. Pure function — no client calls, directly unit-testable.
//
//   - sessRef ("namespace/name") is surfaced as a footer context line so the
//     modal always identifies which session it describes — matching the
//     degraded modal, which already shows it.
//   - nil eff → "settings not yet resolved" section (still with the sessRef
//     footer).
//   - Budget dimensions whose Provenance["budget.<dim>"] == "clamped" are
//     annotated with " _(clamped)_" so the user can see what was adjusted.
//   - AllowedToolkits / AllowedMCP are shown when non-empty.
func renderSettingsModalBlocks(sessRef string, eff *spiceboxv1alpha1.EffectiveSettings) []slackapi.Block {
	if eff == nil {
		return append([]slackapi.Block{
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject("mrkdwn",
					"_Settings for this session aren't ready yet. Try again in a moment._",
					false, false),
				nil, nil,
			),
		}, sessionRefContextBlocks(sessRef)...)
	}

	var blocks []slackapi.Block

	// --- Model ---
	{
		var modelLine string
		if eff.Model.Name != "" {
			// Span-safe for the same reason as joinInlineCode below: the
			// backticks are this renderer's, and a catalog entry carrying one of
			// its own would close the span and render the rest as live mrkdwn.
			modelLine = fmt.Sprintf("`%s/%s`",
				inertSpanValue(eff.Model.Provider), inertSpanValue(eff.Model.Name))
		} else {
			modelLine = "_not resolved_"
		}
		provModel := eff.Provenance["model"]
		if provModel != "" && provModel != "clamped" {
			modelLine += fmt.Sprintf(" (from %s tier)", provModel)
		}
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				"*Model*\n"+modelLine,
				false, false),
			nil, nil,
		))
	}

	// --- Budget ---
	{
		var lines []string

		turns := fmt.Sprintf("Max turns: `%d`", eff.Budget.MaxTurns)
		if eff.Provenance["budget.maxTurns"] == "clamped" {
			turns += " _(clamped)_"
		}
		lines = append(lines, turns)

		tokens := fmt.Sprintf("Max tokens: `%d`", eff.Budget.MaxTokens)
		if eff.Provenance["budget.maxTokens"] == "clamped" {
			tokens += " _(clamped)_"
		}
		lines = append(lines, tokens)

		dur := eff.Budget.MaxDuration.Duration.String()
		if eff.Budget.MaxDuration.Duration == 0 {
			dur = "none"
		}
		durLine := fmt.Sprintf("Max duration: `%s`", dur)
		if eff.Provenance["budget.maxDuration"] == "clamped" {
			durLine += " _(clamped)_"
		}
		lines = append(lines, durLine)

		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				"*Budget*\n"+strings.Join(lines, "\n"),
				false, false),
			nil, nil,
		))
	}

	// --- Allowlists (only when constrained) ---
	if len(eff.AllowedToolkits) > 0 {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				"Allowed toolkits: "+joinInlineCode(eff.AllowedToolkits),
				false, false),
		))
	}
	if len(eff.AllowedMCP) > 0 {
		names := make([]string, 0, len(eff.AllowedMCP))
		for _, m := range eff.AllowedMCP {
			names = append(names, m.Name)
		}
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				"Allowed MCP servers: "+joinInlineCode(names),
				false, false),
		))
	}

	// --- Provenance footer (non-clamped tiers are informational) ---
	if prov := eff.Provenance; len(prov) > 0 {
		blocks = append(blocks, slackapi.NewDividerBlock())
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				"_Settings resolved via the 4-tier chain: session → class → namespace → cluster._",
				false, false),
		))
	}

	// --- Session footer — always identify which session this describes. ---
	blocks = append(blocks, sessionRefContextBlocks(sessRef)...)

	return blocks
}

// sessionRefContextBlocks renders the "Session: `ns/name`" context line shown
// in the settings modals so the user can always tell which session they're
// looking at. Returns nil for an empty ref so callers can unconditionally
// append the result.
func sessionRefContextBlocks(sessRef string) []slackapi.Block {
	if sessRef == "" {
		return nil
	}
	return []slackapi.Block{
		slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Session: `%s`", sessRef),
				false, false),
		),
	}
}

// handleShowSettingsAction recognises a "Show settings" block_action click,
// fetches the AgentSession's current effectiveSettings, and opens a modal.
// Returns (true, nil) on a recognised click; (false, nil) when no matching
// action is present.
//
// On session-fetch error the modal still opens with a degraded "settings
// unavailable" body — the click is never silently swallowed.
func (l *slackListener) handleShowSettingsAction(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	logger := log.FromContext(ctx)
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID != showSettingsActionID {
			continue
		}
		v, ok := decodeApprovalButtonValue(a.Value)
		if !ok || v.V != discShowSettings {
			continue
		}

		// Fetch the AgentSession to get the latest effectiveSettings.
		var modalBlocks []slackapi.Block
		if l.deps.K8sClient != nil && v.S != "" {
			ns, name, cut := strings.Cut(v.S, "/")
			if cut && ns != "" && name != "" {
				var as spiceboxv1alpha1.AgentSession
				if err := l.deps.K8sClient.Get(ctx, types.NamespacedName{
					Namespace: ns, Name: name,
				}, &as); err != nil {
					logger.Info("show_settings: get session failed; opening degraded modal",
						"sessRef", v.S, "clicker", cb.User.ID, "err", err.Error())
					modalBlocks = settingsDegradedBlocks(v.S, err)
				} else {
					modalBlocks = renderSettingsModalBlocks(v.S, as.Status.EffectiveSettings)
				}
			}
		}
		if modalBlocks == nil {
			// K8sClient unwired or malformed sessRef — degraded.
			logger.Info("show_settings: no K8sClient or invalid sessRef; opening degraded modal",
				"sessRef", v.S, "clicker", cb.User.ID)
			modalBlocks = settingsDegradedBlocks(v.S, nil)
		}

		view := slackapi.ModalViewRequest{
			Type:   slackapi.VTModal,
			Title:  slackapi.NewTextBlockObject("plain_text", "Agent settings", true, false),
			Close:  slackapi.NewTextBlockObject("plain_text", "Close", true, false),
			Blocks: slackapi.Blocks{BlockSet: modalBlocks},
		}
		if _, err := l.api.OpenViewContext(ctx, cb.TriggerID, view); err != nil {
			logger.Info("show_settings: views.open failed",
				"sessRef", v.S, "triggerID", cb.TriggerID, "err", err.Error())
			return true, fmt.Errorf("show_settings: views.open: %w", err)
		}
		logger.Info("show_settings: modal opened", "sessRef", v.S, "clicker", cb.User.ID)
		return true, nil
	}
	return false, nil
}

// settingsDegradedBlocks returns a fallback modal body for when the session
// can't be fetched. err may be nil (K8sClient unwired / bad ref).
//
// err selects WHICH sentence the reader gets; its text never appears in the
// modal. A lookup failure's message is operator copy — it names API groups and
// resources the reader cannot act on — and the caller has already logged it
// with the session ref and the clicker. What the reader needs is whether to
// retry now or go find a fresher message.
func settingsDegradedBlocks(sessRef string, err error) []slackapi.Block {
	detail := "This button may be from an older message. Try again from a recent one."
	if err != nil {
		detail = "Couldn't load this session just now. Try again in a moment."
	}
	return append([]slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				"_Settings are currently unavailable._\n"+detail,
				false, false),
			nil, nil,
		),
	}, sessionRefContextBlocks(sessRef)...)
}

// joinInlineCode renders a string slice as comma-separated "`item`" elements.
//
// Each item is made span-safe first. The backtick this function adds is what
// makes the item inert — Slack parses neither a `<url|label>` span nor a
// `<!channel>` ping inside a code span — so an item carrying a backtick of its
// own closes that span early and renders everything after it as live mrkdwn,
// with no escape anywhere on this path to fall back on. inertSpanValue, not
// inertProse: a defused URL already carries a span of its own, and wrapping
// that again nests the delimiters — an empty span, then a live URL.
//
// Lower exposure than the decision surfaces — the items are toolkit and
// MCPServer names off a Settings CR, which takes admin-tier write RBAC, and
// this modal carries no decision control. It is the same delimiter contract
// either way, and a contract that holds only for the values nobody hostile can
// reach is not one.
func joinInlineCode(items []string) string {
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, "`"+inertSpanValue(s)+"`")
	}
	return strings.Join(parts, ", ")
}

// isSettingsClamped reports whether the EffectiveSettings record shows a
// budget clamp via a Provenance "budget.*"="clamped" entry. (The
// SettingsAccepted condition reason SettingsClamped is an equivalent signal,
// but the stamped Provenance is what this path reads.)
func isSettingsClamped(eff *spiceboxv1alpha1.EffectiveSettings) bool {
	if eff == nil {
		return false
	}
	for k, v := range eff.Provenance {
		if strings.HasPrefix(k, "budget.") && v == "clamped" {
			return true
		}
	}
	return false
}
