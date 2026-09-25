package registry

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
)

// ValidateCredential is the single entry point for "is this credential
// declaration legal here?", answering all four questions an identity
// controller has to ask, in the order that produces the most useful message.
//
// It exists because those four questions were open-coded identically in the
// AgentIdentity and UserIdentity reconcilers, and a rule added to one would
// not reach the other — which is how the SessionUserIdentity scope ended up
// with no enforcement point at all (the fix is to call this from that path
// too).
//
//  1. the type is registered (fail-closed on unknown AND empty — see Get)
//  2. the type may be declared on this scope
//  3. the type's own block is present and well-formed (Kind.ValidateSpec)
//  4. no OTHER type's block is set (ValidateExclusive)
//
// Every message is already prefixed with credentials[<name>], because it is
// surfaced verbatim on a SpecInvalid condition and the reader needs to know
// which credential it is about.
func ValidateCredential(c spiceboxv1alpha1.AgentCredential, scope credkind.Scope) error {
	k, err := Get(c.Type)
	if err != nil {
		return fmt.Errorf("credentials[%s]: %w", c.Name, err)
	}
	if !credkind.ValidOnScope(k, scope) {
		return fmt.Errorf("credentials[%s]: type=%s is not valid here (valid on: %v)",
			c.Name, c.Type, k.ValidOn())
	}
	// ValidateSpec's messages already carry the credentials[<name>] prefix and
	// name the offending field, so they pass through verbatim.
	if err := k.ValidateSpec(c); err != nil {
		return err
	}
	return ValidateExclusive(c)
}

// ValidateExclusive reports a union block set on c that does not belong to c's
// declared type.
//
// The rule is asked of the registry, off each Kind's HasBlock, rather than
// written into each kind as a list of its siblings. That older shape was
// O(n^2) coupling in the package built to remove it: adding githubApp as a
// fourth type required editing the other three to reject it, all three were
// missed, and each therefore accepted a credential declaring type=static while
// carrying a githubApp block. Nothing downstream re-checks — the broker
// dispatches purely on Type — so the stray block was inert but the CR was
// accepted as valid, and the operator's mistake was never reported.
//
// A fifth kind now needs no edit here at all: it is covered the moment it
// registers.
func ValidateExclusive(c spiceboxv1alpha1.AgentCredential) error {
	var foreign []string
	for _, k := range All() {
		if k.Type() == c.Type {
			continue
		}
		if k.HasBlock(c) {
			foreign = append(foreign, k.Type())
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	// All() is sorted by type, so the rendering is deterministic — a status
	// message that reordered between reconciles would rewrite the CR's
	// condition for no reason.
	noun := "block"
	if len(foreign) > 1 {
		noun = "blocks"
	}
	return fmt.Errorf("credentials[%s]: type=%s must not set the %s %s",
		c.Name, c.Type, strings.Join(foreign, ", "), noun)
}
