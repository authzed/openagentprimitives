package categories

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// OutcomeLabel is what a surface shows for a RESOLVED interaction: the
// category's own OutcomeText when it supplied one, else the outcome's human
// headline — with identity_choice's raw action id swapped for its friendly
// label on the way past.
//
// This is the single answer to "what do I show when a prompt resolves", so the
// surfaces that ask it cannot drift apart. A new category with its own
// labelling rule gets a row here, never a per-surface copy.
//
// OutcomeText is category-overloaded — identity_choice puts a load-bearing
// action id in it, credential_link a credential name — which is why the
// identity_choice swap is gated on category rather than applied to any value
// that happens to match. The wire value is never rewritten at the source
// (internal/cmd/runner reads it straight back out as the identity gate's decision
// action); this is presentation only, applied at render time.
//
// The web chat is the one surface this cannot absorb, its renderer being
// TypeScript. pkg/web/webui/chat/ui/InteractionCard.tsx mirrors these steps in
// the same order against its own copies of the two label tables — the
// identity_choice swap, the TrimSpace, then the headline fallback. Keep them in
// sync.
func OutcomeLabel(category, outcomeText, outcome string) string {
	if category == IdentityChoice {
		if label, ok := IdentityChoiceOutcomeLabel(outcomeText); ok {
			return label
		}
	}
	if t := strings.TrimSpace(outcomeText); t != "" {
		return t
	}
	return channelevents.OutcomeHeadline(outcome)
}
