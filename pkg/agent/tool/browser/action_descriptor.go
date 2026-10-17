package browser

// action_descriptor.go renders a short, human-readable sentence describing
// what a pending browser acting call (click/type/press/select/submit) will
// DO, for the approval card's "What" line. It replaces the wire-level phrase
// the card used to show ("Click the element with ref e13 on httpbin.org") —
// correct but meaningless to an approver, who cannot resolve a ref against a
// live page — with what the element IS, built from the SAME ActionFacts the
// consequence judge already extracts for classification (actionfacts.go):
// the target's Role and accessible Text, and whether it is a form submit
// control. No extra DOM read is spent on this: stampWebAction (tools.go)
// calls humanActionDescriptor right after the actionFacts() call it already
// makes for every acting call a judge is wired for.
//
// CREDENTIAL SAFETY: ActionFacts.Text is already protected at the SOURCE —
// actionFacts no longer falls through to el.value and blanks Text outright for a
// credential-filled field (actionfacts.go) — so a typed secret should never
// reach this renderer. This function keeps a belt-and-suspenders posture on top
// of that: for op=="type" it NEVER reads Text at all, naming only the FIELD by
// its role ("Type into a text field"), never a value. Every other op's Text is a
// UI LABEL (a button caption, a link's visible text, a radio's label) — content
// the PAGE AUTHORED for display, not something the agent or a human typed — and
// is safe to show.
import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

// humanActionDescriptor renders ONE short sentence for facts. It is a pure
// function of ActionFacts — it never touches the network or the live page —
// so it degrades gracefully: a facts value with every field empty (the
// fail-closed branch of stampWebAction, when actionFacts() itself errored)
// still produces a sensible generic phrase from Op/Host alone, never an
// empty string.
func humanActionDescriptor(f ActionFacts) string {
	host := displayHost(f.Host)
	switch {
	case f.Op == "submit" || f.Submits:
		// The COMMIT. Name the control's own label when known ("Submit order"
		// is the button's caption, not a typed value — safe), else just say
		// what is structurally happening.
		if f.Text != "" {
			return fmt.Sprintf("Submit the form by clicking %q", f.Text)
		}
		return fmt.Sprintf("Submit the form on %s", host)

	case f.Op == "type":
		// NEVER f.Text here — see the credential-safety note above. Name only
		// the field's role.
		return fmt.Sprintf("Type into the %s on %s", humanRole(f.Role), host)

	case f.Op == "select":
		// The "select" tool op picks an option on a native <select>; its facts
		// carry the SELECT ELEMENT's own role/text (not the chosen option —
		// that is a separate call argument this function never sees), so this
		// names the control being changed, not a value.
		if f.Text != "" {
			return fmt.Sprintf("Change the %q dropdown on %s", f.Text, host)
		}
		return fmt.Sprintf("Change a dropdown selection on %s", host)

	case f.Op == "click" && judge.IsSelectionRole(f.Role):
		// Radio/checkbox/option/menuitemradio/menuitemcheckbox: the accessible
		// text IS the option's label ("Medium", "Extra cheese"), authored by
		// the page, not typed by anyone.
		if f.Text != "" {
			return fmt.Sprintf("Select %q", f.Text)
		}
		return fmt.Sprintf("Select a %s option on %s", humanRole(f.Role), host)

	case f.Op == "click" && f.Role == "link":
		if f.Text != "" {
			return fmt.Sprintf("Click the %q link", f.Text)
		}
		return fmt.Sprintf("Follow a link on %s", host)

	case f.Op == "click" && f.Role == "tab":
		if f.Text != "" {
			return fmt.Sprintf("Switch to the %q tab", f.Text)
		}
		return fmt.Sprintf("Switch tabs on %s", host)

	case f.Op == "press":
		if f.Key != "" {
			// stampPress always sets Key, so a press card names the actual key —
			// "Press Enter on example.com" reads far more clearly than a generic
			// "Press a key", and tells the approver whether it is a committing
			// key (Enter/Space) or a benign one.
			return fmt.Sprintf("Press %s on %s", f.Key, host)
		}
		if f.Text != "" {
			return fmt.Sprintf("Press a key while focused on %q (%s)", f.Text, humanRole(f.Role))
		}
		return fmt.Sprintf("Press a key on %s", host)

	default:
		// Generic click (a plain button, or a role this function does not
		// special-case) and the fail-closed-fact-read case (every field but Op
		// and Host empty) both land here.
		if f.Text != "" {
			if f.Role != "" {
				return fmt.Sprintf("Click %q (%s)", f.Text, humanRole(f.Role))
			}
			return fmt.Sprintf("Click %q", f.Text)
		}
		if f.Role != "" {
			return fmt.Sprintf("Click a %s element on %s", humanRole(f.Role), host)
		}
		if f.Op != "" {
			return fmt.Sprintf("%s on %s", strings.ToUpper(f.Op[:1])+f.Op[1:], host)
		}
		return fmt.Sprintf("Act on %s", host)
	}
}

// humanRole turns an AX role into the plain noun an approver reads naturally
// ("textbox" -> "text field"); anything not listed is shown as-is, which is
// already readable for the common cases (button, link, radio, checkbox,
// option, tab).
func humanRole(role string) string {
	switch role {
	case "textbox", "searchbox", "combobox":
		return "text field"
	case "":
		return "element"
	default:
		return role
	}
}

// displayHost falls back to "the page" when the live host is unavailable
// (about:blank, or the fail-closed fact-read branch) rather than showing an
// empty string mid-sentence.
func displayHost(host string) string {
	if host == "" {
		return "the page"
	}
	return host
}
