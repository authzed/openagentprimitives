// Identity attenuation for delegated children.
//
// The rule is MONOTONIC: a child's identity mode may equal or narrow its
// parent's, never widen it. The trap this closes is the one every published
// delegation guidance names first — a child minted from a parent's service
// identity but LABELLED with a user's identity manufactures authority the user
// never granted, and the audit trail then attributes it to that user. With
// monotonicity an agent-identity parent has nothing to launder, so the case is
// unrepresentable rather than merely refused.
package subagentrequest

import (
	"fmt"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// CheckMonotonicIdentity refuses a child identity that widens its parent's.
// Modes are the RESOLVED effective modes, never spec.identityMode.
//
// The function takes plain strings rather than a resolved-mode type, so an
// unrecognized value means a caller bug (e.g., passing spec.identityMode
// instead of the resolved mode). The safe answer to a caller bug in an
// authority check is refusal.
func CheckMonotonicIdentity(parentMode, childMode, parentStarter, childStarter string) error {
	// Validate both modes are recognized constants; fail closed on caller bugs.
	if parentMode != v1.IdentityModeAgent && parentMode != v1.IdentityModeUserPassthrough {
		return fmt.Errorf("unrecognized parent identity mode %q", parentMode)
	}
	if childMode != v1.IdentityModeAgent && childMode != v1.IdentityModeUserPassthrough {
		return fmt.Errorf("unrecognized child identity mode %q", childMode)
	}

	if childMode != v1.IdentityModeUserPassthrough {
		return nil // narrowing to (or staying at) agent identity is always fine
	}
	if parentMode != v1.IdentityModeUserPassthrough {
		return fmt.Errorf(
			"a child cannot widen its parent's identity: parent runs as %q, child asks for %q",
			parentMode, childMode)
	}
	if childStarter == "" {
		return fmt.Errorf("a userPassthrough child has no starter subject to act as")
	}
	if childStarter != parentStarter {
		return fmt.Errorf(
			"a userPassthrough child must act as the same human as its parent: parent %q, child %q (different starter)",
			parentStarter, childStarter)
	}
	return nil
}
