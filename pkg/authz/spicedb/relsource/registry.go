package relsource

import (
	"fmt"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-global registry of relation claims. It is never reset
// between tests. Sources registered by one test stay in the registry for
// subsequent tests. This is intentional: production claims register into
// this registry from init() in each writer's own package, and resetting it
// would erase those, breaking any test that ran after. See
// pkg/tools/sandboxkinds/registry for the same pattern and rationale.
var reg = kindregistry.New[Source]("relsource", func(s Source) string { return s.Name })

// Register declares src as a writer and the relations it claims, keyed by
// src.Name. Panics on an empty or duplicate Name — both are programmer
// errors caught at registration, matching every other kind registry in the
// repo.
//
// Register does not itself detect two DIFFERENT sources claiming the same
// relation; that check happens lazily, the first time a claim is consulted
// (see relationIndex), so the resulting panic can name both sources rather
// than just whichever registered second.
func Register(src Source) {
	checkSubjectIdentityClaims(src)
	reg.Register(src)
	invalidateIndex()
}

// checkSubjectIdentityClaims panics when a source declares a
// SubjectIdentityClaim that cannot possibly match, which is the one failure
// mode that field exists to prevent and the one a test can never observe from
// results: an unmatched probe returns nothing, exactly like a person with no
// links.
//
// Three shapes are provably no-ops:
//
//   - a claim not in Claims. A source can only declare the shape of tuples it
//     writes, and it writes only what it claims. An entry outside Claims is a
//     typo or a copy-paste from another source, and either way the reader would
//     probe a relation this source never produces.
//   - the hash sentinel. Its subject is a string digest by construction (see
//     IsSentinelRelation), so a user-subject filter can never match it.
//   - the display-name label. Its subject is an encoded name on the same
//     permission-less `string` type (see IsLabelRelation), so a user-subject
//     filter can never match it either. Refused for the same reason and in the
//     same breath as the sentinel, rather than left to be noticed later: the
//     two are the only relations a directory sync writes whose subject is not
//     an identity, and a reader that treats a NAME as a person's linked
//     identity would put a row on the console attributing a channel's name to
//     whoever was being looked at.
//
// Panicking at registration matches how the registry already treats an empty or
// duplicate Name, and a malformed claim in buildIndex: a programmer error caught
// where it is written rather than in an incident.
func checkSubjectIdentityClaims(src Source) {
	owned := make(map[string]bool, len(src.Claims))
	for _, c := range src.Claims {
		owned[c] = true
	}
	for _, c := range src.SubjectIdentityClaims {
		if !owned[c] {
			panic(fmt.Sprintf("relsource: source %q declares SubjectIdentityClaims %q that is not one of its Claims", src.Name, c))
		}
		_, relation, ok := SplitClaim(c)
		if !ok {
			continue
		}
		if IsSentinelRelation(relation) {
			panic(fmt.Sprintf("relsource: source %q declares the hash sentinel %q as a SubjectIdentityClaim; its subject is a string digest, so probing it can only ever return nothing", src.Name, c))
		}
		if IsLabelRelation(relation) {
			panic(fmt.Sprintf("relsource: source %q declares the display-name label %q as a SubjectIdentityClaim; its subject is an encoded name on the permission-less `string` type, so probing it can only ever return nothing", src.Name, c))
		}
	}
}

// All returns every registered Source, sorted by name. Exported for tests
// that need to sanity-check the whole registry (e.g. that every claim is
// well-formed) rather than one Source at a time.
func All() []Source {
	return reg.All()
}

// index is the built relation-ownership table.
type index struct {
	// owner maps "resourceType#relation" to the name of the source that
	// claims it.
	owner map[string]string
	// byType maps a resource type to every relation claimed on it, for the
	// CheckDeleteFilter could-match rule.
	byType map[string][]string
}

var (
	idxMu sync.Mutex
	idx   *index
)

// invalidateIndex discards the cached index so the next check rebuilds it
// from current registrations. Called on every Register so a claim
// registered after the first check is still picked up — the index is
// "lazily built on first check" in the sense that it is rebuilt on demand,
// not that it is built exactly once for the life of the process.
func invalidateIndex() {
	idxMu.Lock()
	defer idxMu.Unlock()
	idx = nil
}

// buildIndex constructs an index from an explicit set of sources, checking
// for conflicting claims.
//
// Panics if two different sources claim the same relation: ambiguous
// ownership is a bug in the claims, detectable at this point, and resolving
// it silently (e.g. last-registration-wins) would pick a winner nobody
// chose and make the loser's refusals look like a guard malfunction.
//
// Also panics if a source declares a malformed claim with no '#' separator:
// that is a programmer error and must be caught at the point the claim is
// declared.
func buildIndex(srcs []Source) *index {
	built := &index{
		owner:  map[string]string{},
		byType: map[string][]string{},
	}
	for _, src := range srcs {
		for _, claim := range src.Claims {
			resourceType, relation, ok := SplitClaim(claim)
			if !ok {
				panic(fmt.Sprintf("relsource: source %q has malformed claim %q, want \"type#relation\"", src.Name, claim))
			}
			key := claimKey(resourceType, relation)
			if existing, taken := built.owner[key]; taken && existing != src.Name {
				panic(fmt.Sprintf("relsource: %s and %s both claim %s", existing, src.Name, key))
			}
			built.owner[key] = src.Name
			built.byType[resourceType] = append(built.byType[resourceType], relation)
		}
	}
	return built
}

// relationIndex returns the cached index, building it from every registered
// Source on first use after invalidation.
func relationIndex() *index {
	idxMu.Lock()
	defer idxMu.Unlock()
	if idx != nil {
		return idx
	}
	built := buildIndex(reg.All())
	idx = built
	return idx
}

// SplitClaim parses a "definition#relation" claim into its two halves. ok is
// false for a malformed claim. Exported so consumers that walk Source.Claims
// parse them with the same function the registry itself uses, rather than a
// second copy of the split.
func SplitClaim(claim string) (resourceType, relation string, ok bool) {
	resourceType, relation, ok = strings.Cut(claim, "#")
	return resourceType, relation, ok
}

// claimKey is the canonical index key for a resource type and relation.
func claimKey(resourceType, relation string) string {
	return resourceType + "#" + relation
}
