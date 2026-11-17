package subjectresolve

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// namePrefix is the literal reference form this resolver claims.
const namePrefix = "name:"

// noUserDirectoryReason answers a name reference on a deployment that wired
// no Env.UserIdentities: there is nothing to match a name against.
const noUserDirectoryReason = "name references are not available on this deployment"

// nameResolver resolves "name:<full name>" to the ONE platform user whose
// recorded name matches it — the displayName or realName of a channel identity
// that carries a channel-vouched email — compared case-, accent- and
// whitespace-insensitively. It exists for references whose only join key is a
// person's name: a pull request author whose commit email is a personal
// address the platform has never seen, while the same person is known under
// their work address.
//
// The name being matched is NOT verified: it is profile text the person set
// themselves (Slack's real_name and display name are user-editable), so
// anyone can claim anyone's name. What bounds that is which records count. A
// channel identity is a candidate only when it carries an Email, which a
// channel kind sets only for an account it vouches for (Slack: a full member
// of the installed workspace, never a guest, shared-channel stranger or
// foreign-workspace user). Guests and outsiders are therefore neither matches
// nor ambiguity votes: they cannot capture a name, and they cannot veto a
// real member's match by colliding with it. spec.displayName is not matched
// at all — channelsd copies it from the first inbound's display name for
// every sender, vouched or not. The residual risk is a vouched member of an
// installed workspace renaming themselves to someone else's name; that is
// accountable inside an org this deployment already trusts with email.
//
// A name is a weaker join than an email, so the resolver refuses rather than
// guesses: zero matches and more than one distinct matching user are both
// unresolved, and a single-word name is rejected outright, because single
// words are handles ("alex", "chris") that collide across people far more often
// than full names do. The cost is that a person with a single legal name
// (a mononym) cannot be reached by name; they are reached by email instead.
//
// Resolutions stay SubjectProven == false, like the email resolver's: a name
// match picks a real user, but it does not prove the reference IS that user.
type nameResolver struct{}

func (nameResolver) Usage() (string, string) {
	return "name:<full name>", "the one verified platform user whose channel profile carries this full name (first and last); refused when no user or several users match"
}

func (nameResolver) TryResolve(ctx context.Context, ref string, env Env) (Resolution, bool, error) {
	raw, ok := strings.CutPrefix(ref, namePrefix)
	if !ok {
		return Resolution{}, false, nil
	}
	want := normalizeName(raw)
	if len(strings.Fields(want)) < 2 {
		return Resolution{Reason: fmt.Sprintf("%q is not a full name; a name reference needs at least a first and last name", truncateRef(raw))}, true, nil
	}
	if env.UserIdentities == nil {
		return Resolution{Reason: noUserDirectoryReason}, true, nil
	}
	users, err := env.UserIdentities(ctx)
	if err != nil {
		return Resolution{}, true, err
	}

	matched := map[string]bool{}
	for i := range users {
		if !userHasName(&users[i], want) {
			continue
		}
		canon, cerr := identity.Subject(users[i].Spec.Subject).CanonicalUserID()
		if cerr != nil {
			// A UserIdentity whose subject is not a user: subject cannot be a
			// preference target; skip it rather than fail the whole lookup.
			continue
		}
		matched[canon.String()] = true
	}
	switch len(matched) {
	case 0:
		return Resolution{Reason: fmt.Sprintf("no platform user is recorded under the name %q", truncateRef(raw))}, true, nil
	case 1:
		for subject := range matched {
			return Resolution{Subject: subject}, true, nil
		}
	}
	return Resolution{Reason: fmt.Sprintf("%d platform users are recorded under the name %q; a name reference must match exactly one", len(matched), truncateRef(raw))}, true, nil
}

// userHasName reports whether a name recorded on one of ui's vouched channel
// identities normalizes to want. An identity without an Email is one the
// channel did not vouch for, so its self-set name is not a candidate.
func userHasName(ui *spiceboxv1alpha1.UserIdentity, want string) bool {
	for _, ci := range ui.Status.ChannelIdentities {
		if ci.Email == "" {
			continue
		}
		if normalizeName(ci.DisplayName) == want || normalizeName(ci.RealName) == want {
			return true
		}
	}
	return false
}

// normalizeName folds case, strips combining marks ("Núñez" → "nunez") and
// collapses runs of whitespace, so the same name typed into GitHub and into
// Slack compares equal.
func normalizeName(s string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	stripped, _, err := transform.String(t, s)
	if err != nil {
		// These transformers do not fail on in-memory strings; if one ever
		// does, compare the unstripped name, which can only miss a match.
		stripped = s
	}
	return strings.Join(strings.Fields(cases.Fold().String(stripped)), " ")
}
