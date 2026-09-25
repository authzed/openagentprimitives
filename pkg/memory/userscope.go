package memory

import (
	"fmt"
	"strings"
)

// ScopeKindUser is the per-user scope kind (see the Scope doc in entry.go —
// reserved there since the framework's inception, first used by the
// user_preference kind).
const ScopeKindUser = "user"

// UserScope returns the scope holding one user's cross-session data, keyed
// by the bare canonical user id (base64url). Refuses ids that could collide
// with session ("<ns>/<name>") or resource ("<type>:<id>") scope IDs — the
// capability doors key on scope.ID alone, so disjointness is load-bearing
// (see resourcescope.go's ResourceScope for the same rule on the other
// non-session scope kind).
func UserScope(canonicalID string) (Scope, error) {
	if canonicalID == "" {
		return Scope{}, fmt.Errorf("user scope: empty canonical id")
	}
	if strings.ContainsAny(canonicalID, "/:") {
		return Scope{}, fmt.Errorf("user scope: canonical id %q contains a reserved separator", canonicalID)
	}
	return Scope{Kind: ScopeKindUser, ID: canonicalID}, nil
}
