package registry

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SecretNameFor answers "which Secret does this credential's value live in"
// through the registry — the shared implementation of the discriminated-union
// dispatch that five call sites used to hand-roll as
//
//	switch {
//	case cred.Static != nil:    …
//	case cred.OAuth != nil:     …
//	case cred.Federated != nil: …
//	}
//
// That shape is a type switch on the credential-type enum spelled with no
// string literal, so the AST guard in credkind/guard_test.go that catches
// `c.Type == "static"` cannot see it — see that file's Guard 6, which catches
// this shape structurally instead.
//
// Three distinct outcomes carry three distinct meanings, and callers must not
// conflate them:
//   - An UNREGISTERED type returns an error. This is a wiring bug (the
//     binary is missing its blank import of the type's kind package under
//     credkind/imports, or an equivalent registration), not a shape the
//     credential can legitimately take — the caller decides whether to log
//     and skip the one credential or fail the whole operation.
//   - A registered type with NO backing Secret (federated: minted per
//     resolve, nothing stored) returns ("", nil). This is normal, not an
//     error — there is nothing to watch, adopt, or pin.
//   - Otherwise returns (ref.Name, nil).
//
// This helper answers only the NAME. A caller that also needs the Secret's
// KEY (a static credential's single named key, used to prompt for or
// key-check exactly that value) should call Get(cred.Type) and Kind.SecretRef
// directly instead, the same registry dispatch with the fuller answer.
func SecretNameFor(cred spiceboxv1alpha1.AgentCredential) (string, error) {
	k, err := Get(cred.Type)
	if err != nil {
		return "", err
	}
	ref := k.SecretRef(cred)
	if ref == nil {
		return "", nil
	}
	return ref.Name, nil
}
