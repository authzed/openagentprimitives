package wizardkeys

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// SeededHas reports whether a flag answered key at all, INCLUDING with an
// empty value.
//
// Presence, not non-emptiness, and the reason it is not spelled
// `SeededAnswer(...) != ""`: `--answer bot-token=` is an answer the operator
// gave — "I have no token" — and reading it as unanswered would put the
// question back in front of them, or send an operator who already holds a
// credential off to mint a second one.
//
// A nil WizardInput.Seeded reads as nothing answered, which is what it means.
func SeededHas(in channelkinds.WizardInput, key string) bool {
	_, ok := in.Seeded[key]
	return ok
}

// SeededAnswer reads what a --answer / --name flag supplied before the run,
// trimmed, or "" when nothing did.
func SeededAnswer(in channelkinds.WizardInput, key string) string {
	if !SeededHas(in, key) {
		return ""
	}
	return strings.TrimSpace(in.Seeded[key])
}
