package subjectresolve

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// emailPrefix is the explicit reference form; a bare token containing "@" is
// also accepted (Usage documents both).
const emailPrefix = "email:"

// emailResolver canonicalizes an "email:<addr>" reference (or a bare token
// containing "@") to the SAME canonical user id the rest of the platform
// produces for an email — identity.EmailReference — never a re-derived
// encoding. This is a REFERENCE, not a proven login, matching
// identity.EmailReference's own contract; the resulting canonical id is
// identical to what a verified email would produce (CanonicalUserID does not
// carry the verified bit). Its resolutions accordingly stay SubjectProven ==
// false: any well-formed address yields a subject whether or not a platform
// user exists behind it, so a consumer must apply its own existence bar
// before treating the subject as a real user (see Resolution.SubjectProven).
type emailResolver struct{}

func (emailResolver) Usage() (string, string) {
	return "email:<address>", `the platform user with this email address (a bare address containing "@" is also accepted)`
}

func (emailResolver) TryResolve(ctx context.Context, ref string, env Env) (Resolution, bool, error) {
	candidate, hasPrefix := strings.CutPrefix(ref, emailPrefix)
	if !hasPrefix {
		if !strings.Contains(ref, "@") {
			return Resolution{}, false, nil
		}
		candidate = ref
	}

	addr, err := mail.ParseAddress(strings.TrimSpace(candidate))
	if err != nil || addr.Address == "" {
		return Resolution{Reason: fmt.Sprintf("%q is not a valid email address", truncateRef(candidate))}, true, nil
	}

	canon, err := identity.EmailReference(identity.Email(addr.Address)).Canonical()
	if err != nil {
		return Resolution{}, true, err
	}
	return Resolution{Subject: canon.String()}, true, nil
}
