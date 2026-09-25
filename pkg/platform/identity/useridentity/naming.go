// Package useridentity persists credentials on the cluster-scoped UserIdentity
// CRD and its master credential Secrets, and holds the naming helpers every
// component derives those names with.
package useridentity

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// NameForSubject returns the deterministic, DNS-safe metadata.name for the
// UserIdentity of the given canonical SpiceDB subject. The operator and identityd
// both compute it, so either can Get the UserIdentity by name without a
// list/filter. A canonical subject can exceed the 63-char DNS-label limit and is
// not lowercase, so the name is a hash rather than the subject itself; the full
// subject is stored in spec.subject.
func NameForSubject(subject identity.Subject) string {
	sum := sha256.Sum256([]byte(subject.String()))
	return "u-" + hex.EncodeToString(sum[:])[:48]
}

// MasterSecretName returns the name of the Secret holding one credential's
// material, in the agentprimitives-identities namespace.
func MasterSecretName(userIdentityName, credentialName string) string {
	return userIdentityName + "-" + credentialName
}

// IdPIdentitySecretName returns the deterministic name of the Secret holding the
// user's captured IdP refresh token (the federation subject-token source), in
// IdentitiesNamespace. Derived from the same subject hash as the UserIdentity, so
// any component can locate it.
func IdPIdentitySecretName(subject string) string {
	return NameForSubject(identity.Subject(subject)) + "-idp-identity"
}
