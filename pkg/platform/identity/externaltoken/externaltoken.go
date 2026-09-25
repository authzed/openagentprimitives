// Package externaltoken derives the SpiceDB object id and value hash used by the
// externaltoken token-use authorization grant. Writer (operator) and checkers
// (runner, ToolCall reconciler) MUST import this one package so their derivations
// are byte-identical; a fork would silently fail every check closed.
package externaltoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	ObjectType              = "externaltoken"
	RelationAuthorizedToken = "authorized_token"
	PermissionUseToken      = "use_token"
	CaveatTokenValueMatches = "token_value_matches"
	CaveatArgAuthorizedHash = "authorized_value_hash"
	CaveatArgPresentedHash  = "presented_value_hash"
)

// AuthorizedTokenGrant is a current grant read back from SpiceDB. An empty
// AuthorizedValueHash means an identity-only (uncaveated / federated) grant.
type AuthorizedTokenGrant struct {
	CredID              string
	AuthorizedValueHash string
}

// CredID is the stable externaltoken object id for a credential, derived from its
// full source coordinates (not secret). The full tuple is required: co-located
// static creds share Namespace/Name under different Keys, and federated creds
// distinguish on Resource (their Key is empty).
func CredID(src spiceboxv1alpha1.CredentialSource) string {
	joined := strings.Join([]string{src.Type, src.Namespace, src.Name, src.Key, src.Resource}, "/")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

// ValueHash is the per-session-keyed HMAC of a token value, mirroring
// grants.ArgsHash's HMAC-SHA256/hex idiom.
func ValueHash(sessionKey []byte, value string) string {
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
