// Package idpscreens holds the questions more than one identity-provider kind
// asks — "which client did you register, what is its secret, and who may sign in
// with it?" — described once instead of once per kind.
//
// The screen mechanics live in pkg/cli/tui; this package is the idp vocabulary on
// top of them. A question lands here when a second kind needs it; questions
// specific to one provider (a generic OIDC issuer, a local password) stay in that
// kind's own package.
package idpscreens

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// State keys the shared questions answer. Stable: they are the vocabulary a
// caller seeds to answer a question ahead of the run, and the names a refusal
// uses to say which answer is missing.
const (
	KeyClientID     = "client-id"
	KeyClientSecret = "client-secret"
	KeyAccess       = "access"
	KeyDomains      = "domains"
)

// The two sign-in policies a ClusterIdentityProvider can express.
const (
	AccessDomains = "domains"
	AccessAny     = "any"
)

// callbackLabel names the redirect URI to the user, in the words their
// provider's console uses.
const callbackLabel = "Redirect URI"

// CallbackAddress is the redirect URI a kind shows with its first question.
//
// An address rather than rendered text because two callers need the same answer
// about it: the kind, which offers it alongside the question, and the CLI, which
// must print it OUTSIDE the form when a note cannot carry it whole. It is the one
// thing the user has to carry to their provider's console, so the summary line a
// Question records on overflow is true but too late. Both callers ask
// tui.Address.Fits, so they cannot disagree about which addresses fit.
func CallbackAddress(callbackURL string) tui.Address {
	return tui.Address{Label: callbackLabel, URL: callbackURL}
}

// NewClientSecret returns the client-secret question, which every kind backed
// by a registered OAuth client asks identically.
//
// secretExists makes the answer optional: a stored secret is kept by leaving
// the field blank, which is recorded as an explicit empty answer so that "keep
// what is there" stays distinguishable from "the input ran out".
func NewClientSecret(secretExists bool) *tui.Question {
	title := "Client secret"
	if secretExists {
		title = "Client secret (leave blank to keep the stored one)"
	}
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:         "client-secret",
			Label:      "Secret",
			Key:        KeyClientSecret,
			Title:      title,
			NoteLabel:  "Client secret",
			NoteValue:  SecretUpdated,
			NoteAbsent: SecretKept,
		},
		Optional: secretExists,
	})
}

// SecretKept is the summary line for a credential the user left blank, which on a
// re-setup means the stored one stands. A NoteAbsent rather than something
// NoteValue derives: the only alternative for an empty credential is a masker,
// and every masker here returns "****" for an empty string, reporting a redacted
// secret where there is none.
const SecretKept = "kept the stored one"

// SecretUpdated is the summary line for a credential the user supplied. It
// describes the outcome and never returns the value: masking is not enough, since
// the project's masker preserves the last four characters, which for a password
// is four characters of the password.
func SecretUpdated(string) string { return "updated" }

// NewAccess returns the sign-in policy question, which every kind backed by a
// remote issuer asks identically.
//
// Default — not the option order — is what an unanswered run lands on, since this
// question always supplies one. It resolves to allow-any in exactly one case: a
// re-setup of a provider that is ALREADY allow-any, where silence retains a policy
// someone chose before rather than granting a new one. A first setup always lands
// on the restrictive option, so "anyone may sign in" can only be reached by
// picking it.
func NewAccess(existingAny bool, existingDomains []string) *tui.Question {
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        "access",
			Label:     "Access",
			Key:       KeyAccess,
			Title:     "Who may sign in to this cluster?",
			NoteLabel: "Who may sign in",
			NoteValue: func(v string) string {
				if v == AccessAny {
					return "any account the provider authenticates"
				}
				return "only the listed email domains"
			},
		},
		Options: []tui.Choice{
			{Label: "Only accounts in email domains I list", Value: AccessDomains},
			{Label: "Any account the provider authenticates", Value: AccessAny},
		},
		Default: func() string {
			if existingAny && len(existingDomains) == 0 {
				return AccessAny
			}
			return AccessDomains
		},
	})
}

// NewDomains returns the allowed-email-domains question, asked only when the
// access question chose to restrict by domain.
func NewDomains(existing []string) *tui.Question {
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        "domains",
			Label:     "Domains",
			Key:       KeyDomains,
			Title:     "Allowed email domains (comma-separated)",
			NoteLabel: "Allowed domains",
			// Nothing to list when any account may sign in. Not merely cosmetic:
			// without this the run would demand a domain list it is about to ignore.
			Skip: func(st *tui.State) bool {
				return st.Get(KeyAccess) == AccessAny
			},
		},
		Default: func() string { return strings.Join(existing, ",") },
		Check: func(_ *tui.State, v string) error {
			if len(ParseDomains(v)) == 0 {
				return fmt.Errorf("list at least one email domain, or choose %q at the previous question",
					"Any account the provider authenticates")
			}
			return nil
		},
	})
}

// ParseDomains splits a comma-separated answer into domains, trimming each and
// dropping blanks — so "a, b" is what someone separating a list actually types,
// and a trailing comma is harmless.
func ParseDomains(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if d := strings.TrimSpace(part); d != "" {
			out = append(out, d)
		}
	}
	return out
}
