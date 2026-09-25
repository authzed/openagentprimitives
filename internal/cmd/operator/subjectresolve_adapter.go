package main

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
)

// subjectResolveSpiceDBAdapter adapts *spicedb.Client to
// subjectresolve.RelationReader: UserSubjects("<objType>", "<objID>",
// "<relation>") is exactly the "<type>:<id>#<relation>" shape
// (*spicedb.Client).LookupSubjects already parses, the same call
// LookupSoleIdentitySubjects makes for the narrower sole_user-only case this
// package's own caller (subjectresolve/resource.go) always uses. A thin
// forwarding adapter rather than a second lookup implementation, so a future
// change to how LookupSubjects resolves a relation only has one definition
// to keep correct.
type subjectResolveSpiceDBAdapter struct {
	cl *spicedb.Client
}

// newSubjectResolveAdapter binds cl for the subjectresolve.RelationReader
// role.
//
// On nil-safety, precisely: the returned adapter is a struct VALUE, so it
// can never itself be a typed-nil interface — but for the same reason,
// WithSubjectResolution's reflect guard (which detects only a nil POINTER
// boxed into the interface) cannot see a nil cl hiding inside it. Nothing
// downstream defends this call; what makes it safe is the operator's own
// startup invariant — SpiceDB is REQUIRED, its construction failure exits
// the process before run() ever builds a memHandlerDeps — the same
// invariant newPtTagSpiceDBAdapter's doc and the PlatformLinker wiring
// already record. If that invariant is ever relaxed, this call site (and
// theirs) must grow a real nil check; the guard will not catch it.
func newSubjectResolveAdapter(cl *spicedb.Client) subjectresolve.RelationReader {
	return subjectResolveSpiceDBAdapter{cl: cl}
}

// UserSubjects satisfies subjectresolve.RelationReader by forwarding to
// (*spicedb.Client).LookupSubjects over the "<objType>:<objID>#<relation>"
// subject reference it already parses — no new SpiceDB call shape.
func (a subjectResolveSpiceDBAdapter) UserSubjects(ctx context.Context, objType, objID, relation string) ([]string, error) {
	return a.cl.LookupSubjects(ctx, objType+":"+objID+"#"+relation)
}
