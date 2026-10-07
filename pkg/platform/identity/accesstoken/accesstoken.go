// Package accesstoken centralizes every name and derivation shared between
// the schema, the SpiceDB client, the minter, the /mcp middleware, and the
// CLI — the externaltoken precedent: one package so derivations stay
// byte-identical across writer and checker.
package accesstoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	ObjectType           = "accesstoken"
	RelationRoleRead     = "role_read"
	RelationRoleInteract = "role_interact"
	RelationRoleFull     = "role_full"
	RelationScopeClass   = "scope_class"
	PermissionCovers     = "covers"
	PermissionOwner      = "owner"

	RoleRead     = "read"
	RoleInteract = "interact"
	RoleFull     = "full"

	TokenPrefix = "oap_at_"
)

func RoleRelation(role string) (string, error) {
	switch role {
	case RoleRead:
		return RelationRoleRead, nil
	case RoleInteract:
		return RelationRoleInteract, nil
	case RoleFull:
		return RelationRoleFull, nil
	}
	return "", fmt.Errorf("unknown access-token role %q", role)
}

func NewTokenValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token value: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func HashTokenValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func NewTokenID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token id: %w", err)
	}
	return "at-" + hex.EncodeToString(b[:]), nil
}

func MirrorPermissions() []string {
	return []string{"read_transcript", "read", "view", "interact", "approve", "start_session"}
}
