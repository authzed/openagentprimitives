package main

import (
	"context"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory/pttagmint"
)

// pttagSpiceDBAdapter combines the SubjectLookup and RelationshipWriter
// roles from one *spicedb.Client, now that WriteRelationships is guarded
// via (*spicedb.Client).Writer rather than exposed directly on *Client.
// LookupSubjects is promoted from the embedded *spicedb.Client (its own
// typed signature — see pttagmint.SubjectLookup); WriteRelationships is
// explicit, forwarding to the RelWriter bound at construction. Keeping both
// roles on one adapter value (assigned to both Minter.Subjects and
// Minter.Rels at the same call site) preserves the "one client, two
// interfaces" property newPtTagMinter's doc records: a caller cannot derive
// readers from one datastore and write tuples to another.
type pttagSpiceDBAdapter struct {
	*spicedb.Client
	writer spicedb.RelWriter
}

// newPtTagSpiceDBAdapter binds cl's LookupSubjects role and a RelWriter
// (bound to pttagmint.Source) for the WriteRelationships role, both over
// the same underlying connection.
func newPtTagSpiceDBAdapter(cl *spicedb.Client) pttagSpiceDBAdapter {
	return pttagSpiceDBAdapter{Client: cl, writer: cl.Writer(pttagmint.Source)}
}

// WriteRelationships satisfies pttagmint.RelationshipWriter over the bound
// RelWriter, guarded by relsource.CheckWrite.
func (a pttagSpiceDBAdapter) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	return a.writer.WriteRelationships(ctx, req)
}

// newPtTagMinter builds the component that answers POST /memory/_pttag_mint.
//
// Extracted from the handler-option list so the wiring is reachable by a test.
// The route it enables spent its whole life defined and unreachable —
// WithPtTagMinter existed, the client existed, the minter existed, and nothing
// ever called the option — so "is it actually enabled" is the question worth
// being able to ask in a test rather than by reading main().
//
// WHY THE OPERATOR AND NOWHERE ELSE. Minting derives a tag's reader set
// server-side from the resources the request names. That needs a SpiceDB
// subject lookup, a relationship write, and the COMPONENT memory credential;
// all three exist only in this process. The security property is that a
// SESSION can never author a tag: direct_reader GRANTS disclosure, so a session
// able to write one could name an audience its source never authorized. The
// runner says what a call TOUCHED; this answers who may SEE it, and there is
// deliberately no request field for an audience.
//
// subjects/rels is the same value (a pttagSpiceDBAdapter binding one
// *spicedb.Client) for both roles — one connection, two interfaces, so a
// caller cannot accidentally derive readers from one datastore and write
// tuples to another. It is non-nil by construction at every call site in
// this binary (SpiceDB client failure exits before this runs), so assigning
// it into these interface fields cannot produce a typed-nil interface — the
// hazard agentidentity's PlatformLinker wiring records.
func newPtTagMinter(spicedbClient ptTagSpiceDB, mem pttagmint.MemoryWriter) *pttagmint.Minter {
	return &pttagmint.Minter{
		Subjects: spicedbClient,
		Rels:     spicedbClient,
		Mem:      mem,
	}
}

// ptTagSpiceDB is the pair of SpiceDB capabilities minting needs, named as one
// type because they must come from one client. Splitting them into two
// parameters would let a future edit pass a reader from one place and a writer
// from another, which is how a tag's audience and its tuples end up describing
// different things.
type ptTagSpiceDB interface {
	pttagmint.SubjectLookup
	pttagmint.RelationshipWriter
}
