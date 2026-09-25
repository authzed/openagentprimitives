package spicedb

import (
	"context"
	"fmt"
	"io"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The three lookups every pt-tag consumer needs, implemented once.
//
// handoff.GradeRequest, hooks.Trifecta and hold.TrifectaTripper each declare
// these as injected functions, and until now NOBODY implemented them — which is
// why all three read as unwired, and why wiring any one of them first would
// only have moved the gap. They live together because they are one question
// asked three ways: who can see this datum, who can see the place it is going,
// and can it be trusted.

// SessionReadTranscriptAudience returns A(session): the users who can read this
// session's transcript, which is what "the audience of a datum bound here"
// means.
//
// It is agentsession#read_transcript rather than #interact deliberately.
// read_transcript arrows UP the lineage
// (`interact + parent + parent->read_transcript`), so a child's audience
// already includes the people accountable for its ancestors — which is the
// fact that makes a handoff's disclosure question answerable without separate
// bookkeeping. The `+ parent` arm admits the parent SESSION itself as a
// subject, not a user, so it never appears here: LookupSubjects below hardcodes
// SubjectObjectType "user", which is exactly right for an audience question —
// a session is not a person a datum can be "disclosed to".
func (c *Client) SessionReadTranscriptAudience(ctx context.Context, sess authz.SessionRef) ([]string, error) {
	subs, err := c.LookupSubjects(ctx,
		fmt.Sprintf("agentsession:%s/%s#read_transcript", sess.Namespace, sess.Name))
	if err != nil {
		return nil, fmt.Errorf("audience of session %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return subs, nil
}

// TagReaders returns R(T): the tag's fully-resolved audience.
//
// RESOLVED, not direct. pt_tag#reader is
// `direct_reader + derived_from.all(reader)` — an INTERSECTION over the
// derivation tree — so SpiceDB has already narrowed a derived tag to the people
// authorized on every source it was assembled from. Reading direct_reader
// instead would return a derived tag's own (empty) set and read as "nobody",
// or, worse, a union somewhere upstream would read as laundering.
func (c *Client) TagReaders(ctx context.Context, tagID string) ([]string, error) {
	subs, err := c.LookupSubjects(ctx, fmt.Sprintf("pt_tag:%s#reader", tagID))
	if err != nil {
		return nil, fmt.Errorf("readers of tag %q: %w", tagID, err)
	}
	return subs, nil
}

// TagCarriesUntrusted answers the INTEGRITY axis: does this datum, or anything
// it was derived from, come from a source that may carry injected content.
//
// It cannot use LookupSubjects, and the reason is structural rather than
// incidental. That method hardcodes SubjectObjectType "user" because every
// other caller asks about people; carries_untrusted resolves to PT_TAGS. The
// minter writes a leaf's self-mark as
// `pt_tag:T#untrusted_origin@pt_tag:T`, so the satisfying subject is a tag —
// T itself for a leaf, or transitively the source that self-marked for a
// derived one.
//
// A CheckPermission cannot substitute either: it would need the satisfying
// source tag as its subject, which is the very thing being looked up.
//
// Non-empty means untrusted. The boolean is deliberately not "is T marked" —
// integrity is a union down the tree, so one untrusted source anywhere taints
// everything below it and a trusted sibling cannot launder it clean.
func (c *Client) TagCarriesUntrusted(ctx context.Context, tagID string) (bool, error) {
	subs, err := c.lookupSubjectsOfType(ctx,
		fmt.Sprintf("pt_tag:%s#carries_untrusted", tagID), "pt_tag")
	if err != nil {
		return false, fmt.Errorf("integrity of tag %q: %w", tagID, err)
	}
	return len(subs) > 0, nil
}

// lookupSubjectsOfType is LookupSubjects with the subject object type named
// rather than assumed.
//
// Kept unexported and separate rather than widening LookupSubjects' signature:
// that method has many callers, all of them asking about users, and its doc
// carries a specific warning about its FullyConsistent choice that those
// callers rely on. This shares the consistency reasoning — these are security
// gates reading tuples the minter may have written moments earlier — without
// disturbing them.
func (c *Client) lookupSubjectsOfType(ctx context.Context, subjectRef, subjectObjectType string) ([]string, error) {
	objType, objID, relation, err := ParseSubject(subjectRef)
	if err != nil {
		return nil, fmt.Errorf("parse subject %q: %w", subjectRef, err)
	}
	stream, err := c.cl.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource:          &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
		Permission:        relation,
		SubjectObjectType: subjectObjectType,
		// FullyConsistent for the same reason LookupSubjects is: a tag minted
		// during this very turn must be visible to the gate that judges the
		// handoff carrying it. Quantized caching here would let a datum be
		// graded against tuples that predate it.
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	if err != nil {
		return nil, fmt.Errorf("lookup subjects: %w", err)
	}
	var out []string
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("lookup subjects recv: %w", err)
		}
		if r.Subject != nil && r.Subject.SubjectObjectId != "" {
			out = append(out, r.Subject.SubjectObjectId)
		}
	}
}
