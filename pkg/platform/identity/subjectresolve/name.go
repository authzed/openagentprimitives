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
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// namePrefix is the literal reference form this resolver claims.
const namePrefix = "name:"

// noUserDirectoryReason answers a name reference on a deployment that wired
// no Env.UserIdentities: there is nothing to match a name against.
const noUserDirectoryReason = "name references are not available on this deployment"

// noSessionForNameReason answers a name reference resolved with no session
// context: whether the session's trigger owner is trusted cannot be checked.
const noSessionForNameReason = "name references need a session context to check who opened it"

// untrustedTriggerOwnerReason answers a name reference in a session a trigger
// opened for an owner the provider did not vouch for (for a pull request: an
// author who is not a member, owner or collaborator of the repository).
const untrustedTriggerOwnerReason = "name references are not honored in a session whose trigger owner the provider does not vouch for (for a pull request: an author who is not a member, owner or collaborator)"

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
// In a session a trigger opened, a name reference is honored only when the
// provider vouched for the trigger owner (see untrustedTriggerReason): there
// the name is text the owner chose, and an outside contributor must not be
// able to choose whose settings apply.
//
// Resolutions stay SubjectProven == false, like the email resolver's: a name
// match picks a real user, but it does not prove the reference IS that user.
// They also set ConcealSubject: the subject encodes the matched person's
// email, which a caller that knew only a name must not learn from the answer.
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
	if reason, err := untrustedTriggerReason(ctx, env); err != nil || reason != "" {
		return Resolution{Reason: reason}, true, err
	}
	if env.UserIdentities == nil {
		return Resolution{Reason: noUserDirectoryReason}, true, nil
	}
	users, err := env.UserIdentities(ctx)
	if err != nil {
		return Resolution{}, true, err
	}

	matched := map[string]bool{}
	malformed := 0
	for i := range users {
		if !userHasName(&users[i], want) {
			continue
		}
		canon, cerr := identity.Subject(users[i].Spec.Subject).CanonicalUserID()
		if cerr != nil {
			// A vouched person carries this name but their record's subject
			// is not a user, so it cannot be a preference target. It still
			// counts against uniqueness: skipping it would let it turn an
			// ambiguous name into a "unique" match for someone else.
			log.FromContext(ctx).Info("subjectresolve: name match on a UserIdentity whose subject is not a user; refusing the name reference",
				"userIdentity", users[i].Name, "subject", users[i].Spec.Subject, "err", cerr.Error())
			malformed++
			continue
		}
		matched[canon.String()] = true
	}
	if len(matched) == 1 && malformed == 0 {
		for subject := range matched {
			return Resolution{Subject: subject, ConcealSubject: true}, true, nil
		}
	}
	if len(matched)+malformed > 1 {
		log.FromContext(ctx).Info("subjectresolve: name reference matched more than one platform user; refusing",
			"matchedUsers", len(matched), "malformedRecords", malformed)
	}
	// One reason for "nobody" and "several": a distinct answer, or a count,
	// would let a caller probe the user directory one name at a time.
	return Resolution{Reason: fmt.Sprintf("no single platform user is recorded under the name %q; a name reference must match exactly one", truncateRef(raw))}, true, nil
}

// untrustedTriggerReason refuses a name reference in a session an untrusted
// party opened. In a triggered session the name being looked up is text the
// trigger owner chose (a pull request author's profile name), so honoring it
// would let an outside contributor pick whose settings apply. A session with a
// trigger owner is trusted only when channelsd recorded the provider vouching
// for that owner (AnnotationTriggerOwnerTrusted, from a verified delivery);
// anything else — "false", missing, garbage — refuses. A session no trigger
// opened (no owner annotation) is unaffected. With no session context at all
// there is nothing to check, so that refuses too.
func untrustedTriggerReason(ctx context.Context, env Env) (string, error) {
	if env.SessionAnnotations == nil {
		return noSessionForNameReason, nil
	}
	annotations, err := env.SessionAnnotations(ctx)
	if err != nil {
		return "", err
	}
	if annotations[spiceboxv1alpha1.AnnotationTriggerOwnerSubject] == "" {
		return "", nil
	}
	if annotations[spiceboxv1alpha1.AnnotationTriggerOwnerTrusted] == spiceboxv1alpha1.AnnotationTriggerOwnerTrustedTrue {
		return "", nil
	}
	return untrustedTriggerOwnerReason, nil
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
