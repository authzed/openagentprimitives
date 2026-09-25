package provenance

import "sort"

// Disclosing data whose authorized readers are R to an audience A is safe iff
// A is a SUBSET of R: everyone who will see it must already be allowed to.
//
// ONE predicate, deliberately, because there is more than one egress. A
// channel's audience is who reads the channel; a tool call's audience is who
// can read the destination resource, since once the data is there they can.
// Same rule, different source for A. Two implementations of one safety rule is
// how the two answers diverge under a later edit, and the divergence would be
// silent — each site would look correct on its own.

// Unauthorized returns the members of audience that readers does not contain,
// sorted. Empty means the disclosure is safe.
//
// It returns WHO rather than a bool because the refusal is worth explaining: a
// gate that says only "denied" leaves an operator unable to tell a
// misconfigured channel from a genuinely sensitive document, and both look
// identical in a log.
//
// An EMPTY audience is vacuously safe: there is nobody to leak to. That is the
// one place in this package where an empty set means yes, and it is sound for
// the same reason the others mean no — the question is "is anyone present who
// should not be", and with nobody present the answer is no. Callers must not
// use an empty audience to mean "unknown"; an audience that could not be
// resolved is not an empty one, and passing it here would read as safe.
func Unauthorized(audience, readers []string) []string {
	if len(audience) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(readers))
	for _, r := range readers {
		allowed[r] = struct{}{}
	}
	var bad []string
	seen := make(map[string]struct{}, len(audience))
	for _, a := range audience {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		if _, ok := allowed[a]; !ok {
			bad = append(bad, a)
		}
	}
	sort.Strings(bad)
	return bad
}

// MayDisclose reports whether every member of audience is an authorized
// reader. The bool form of Unauthorized, for call sites that do not render the
// reason.
func MayDisclose(audience, readers []string) bool {
	return len(Unauthorized(audience, readers)) == 0
}
