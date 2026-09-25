package adoptguard

import "k8s.io/apimachinery/pkg/types"

// FixedInfra returns the allowlist predicate for operator-infra objects that
// are never CR-referenced (so never adopted) but the operator must read. Keep
// this tiny + explicit. names is the exhaustive set of allowlisted objects, and
// each entry MUST be spelled from the repo's own constant rather than a literal
// — a typo here silently widens nothing and narrows a read the operator needs.
func FixedInfra(names ...types.NamespacedName) func(types.NamespacedName) bool {
	allowed := make(map[types.NamespacedName]struct{}, len(names))
	for _, n := range names {
		allowed[n] = struct{}{}
	}
	return func(nn types.NamespacedName) bool {
		_, ok := allowed[nn]
		return ok
	}
}
