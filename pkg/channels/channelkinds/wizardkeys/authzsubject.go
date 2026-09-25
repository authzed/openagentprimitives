package wizardkeys

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// KeyAuthzSubject is the answer key the shared subject question lands under.
// Stable: a caller seeding answers from flags for a non-interactive run
// addresses the question by it.
const KeyAuthzSubject = "authzsubject"

// AuthzSubjectPrompt is the question text, exported so that both kinds asking
// it declare the SAME prompt by referencing this constant instead of retyping
// the string — a second, independently-typed copy is exactly the drift this
// package exists to prevent.
const AuthzSubjectPrompt = "SpiceDB authzSubject"

// ValidateAuthzSubject refuses an answer that is not a well-formed SpiceDB
// service subject.
//
// It is the question every NON-ATTRIBUTABLE kind must ask: a kind whose
// Kind.UserAttributable() is false has no per-user identity to derive a
// subject from, so the Channel must declare one, and the channel controller
// refuses the Channel outright when it does not (see pkg/controllers/channel's
// validate). bento and github are the two today.
//
// Leaving it out, or getting its shape wrong, is not a cosmetic omission: an
// invalid subject makes the AgentClass this Channel binds to Valid=False —
// taking down its paired output Channel too — and, past that gate, would have
// the inbound pipeline refuse the delivery (pipeline.go runs the same
// authz.ValidateSubject at delivery time).
//
// It lives HERE, on the contract's own shared key, rather than in each kind's
// Result, because a channel wizard's questions carry no validation a client
// evaluates — channelkinds.ValidateInputs refuses a Question.Validation
// outright, since nothing on this side would run it. So a kind's Result is the
// only place the check can happen, and one implementation is what keeps the
// two kinds refusing the same malformed subject with the same words.
func ValidateAuthzSubject(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("%s: a subject is required", KeyAuthzSubject)
	}
	if err := authz.ValidateSubject(v, authz.SubjectService); err != nil {
		return fmt.Errorf(`%s: %w (expected "service:<name>", e.g. "service:review-bot")`, KeyAuthzSubject, err)
	}
	return nil
}
