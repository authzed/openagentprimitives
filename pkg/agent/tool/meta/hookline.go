package meta

import (
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// HookLine is the one-line description of a hook the agent reads in two
// places — update_view's `hook` property description and the "Your page"
// prompt section — so the two can never describe the same region differently.
// The intent is the author's own words with its whitespace collapsed to
// single spaces so the line stays one line; an intent that is only whitespace
// is absent, and an absent intent says so, because a region with no
// instruction is a fact the agent should see rather than a blank it fills
// with a guess.
func HookLine(h uicomponents.Hook) string {
	// Fields+Join, not TrimSpace: the result is ONE bullet in the "Your page"
	// section and one line in update_view's `hook` description, and an intent
	// carrying a newline would split the bullet in both.
	intent := strings.Join(strings.Fields(h.Intent), " ")
	if intent == "" {
		intent = "no instruction from the author"
	}
	allowed := "any registered component"
	if !slices.Contains(h.AllowedComponents, uicomponents.AllowAll) {
		allowed = strings.Join(h.AllowedComponents, ", ")
	}
	return h.Name + ": " + intent + " (allowed: " + allowed + ")"
}
