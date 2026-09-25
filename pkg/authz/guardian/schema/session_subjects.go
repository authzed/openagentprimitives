package schema

import (
	"fmt"
	"sort"
	"strings"
)

// SubjectTypesFor reports the SpiceDB object types the COMPOSED agentsession
// block admits as subjects of the named relation: the scaffold's own vocabulary
// (`relation participant: user | group#member` yields user and group) unioned
// with every channel-kind link type in links (`slack_channel#member` yields
// slack_channel).
//
// It exists so a gate that must accept exactly what the live schema accepts can
// ASK rather than carry its own list. A hand-maintained list beside a gate does
// not track the registry: the fork reconciler's held `user` and `group` alone
// and refused slack_channel#member as an agentsession participant long after the
// composer began unioning it into the very relation being written.
//
// The union is computed by appendLinks — the composer's own function, the one
// that produces the live relation line — rather than by a second implementation
// of the same rule, so the answer cannot drift from the text it describes.
//
// links are the raw declarations (registry.SessionRelationLinks); the "#relation"
// half names the link's own relation and is not part of the subject type.
//
// Fail-closed on a question the scaffold cannot answer. An unknown relation, or
// one that holds no subjects (started_by, which the composer never unions links
// into), is an error rather than a partial answer: reporting "just the links"
// would drop the base vocabulary, and reporting the links for a relation that
// never receives them would claim an admissibility the live schema does not
// grant.
//
// Pure: no I/O, no registry access. The caller supplies both the scaffold text
// and the links, which is what lets a test compose a fixture and compare.
func SubjectTypesFor(scaffold, relation string, links ...string) ([]string, error) {
	if !isSessionSubjectRelation("relation " + relation + ":") {
		return nil, fmt.Errorf("agentsession relation %q holds no subjects; channel-kind links are never unioned into it (subject-bearing: %s)",
			relation, strings.Join(sessionSubjectRelations, ", "))
	}
	prefix := "relation " + relation + ":"
	for _, line := range strings.Split(scaffold, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		return subjectTypesOfLine(appendLinks(trimmed, links), prefix), nil
	}
	return nil, fmt.Errorf("agentsession relation %q is not declared in the schema", relation)
}

// subjectTypesOfLine splits a `relation x: a | b#c` line into its distinct
// subject object types, dropping each entry's "#relation" half. Sorted and
// de-duplicated so the result is stable and a caller may binary-search or
// compare it directly.
func subjectTypesOfLine(relationLine, prefix string) []string {
	var types []string
	seen := map[string]bool{}
	for _, part := range strings.Split(strings.TrimPrefix(relationLine, prefix), "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// A caveated entry ("externaltoken with token_value_matches") names the
		// same object type as its uncaveated sibling; the caveat is a condition
		// on the tuple, not a different subject type.
		typ, _, _ := strings.Cut(part, "#")
		typ, _, _ = strings.Cut(typ, " ")
		if typ == "" || seen[typ] {
			continue
		}
		seen[typ] = true
		types = append(types, typ)
	}
	sort.Strings(types)
	return types
}
