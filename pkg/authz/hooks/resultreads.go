package hooks

import (
	"errors"
	"fmt"
)

// ErrUnattributableResource marks a declaration's refusal to attribute a result
// it READ successfully — as opposed to a result it could not read at all.
//
// The distinction decides whether the isError escape hatch applies. A failed
// tool call usually carries something that is not the tool's envelope, and
// there is genuinely nothing to gate in that; but isError is written by the
// UPSTREAM, so letting it excuse every resolver error would mean a result that
// parsed fine and named a scope the declaration refused could be waved through
// by the other side setting a flag. That is the failure mode the comment above
// InfoLeakRead.Eval's `errored` computation records as a past production bug,
// and a bare error at this boundary is how a second instance of it grows.
//
// A declaration wraps this (with %w) when it is refusing what it understood;
// everything else it returns is read as "that was not my envelope".
var ErrUnattributableResource = errors.New("result names a resource this declaration cannot attribute")

// The plural, result-derived half of a read declaration.
//
// A single-resource decl says "this tool reads ONE object, and here is the arg
// or result field naming it". That describes every CRD-declared tool and
// nothing about a memory search, which reads the session's own scope plus every
// resource pool the session reached through a slot grant — heterogeneous in
// type (`customer:alpha`, `repo:x`), so no ResourceType + field-path pair names
// them.
//
// Extending the existing declaration keeps ONE tagging path rather than opening
// a second door beside it: the read hook still decides what a call touched, and
// still mints and taints from that answer. What changes is that the answer may
// name several objects, each with the slice of the result that came from it.

// ToolReadResource is one resource a tool call read, paired with the part of
// the result that came from it.
type ToolReadResource struct {
	Type string
	ID   string
	// Permission the resource's AUDIENCE is expanded under — whoever holds it
	// on this object is who may see this datum. It is a property of the
	// RESOURCE, not of how the session reached it: a session holding a
	// `(customer, read)` slot grant reads a pool whose readers are whoever
	// holds view_memory on that customer. Minting under the slot's permission
	// would derive the wrong subject set, and nothing downstream would notice.
	Permission string
	// Content is the part of the result this resource contributed, in the
	// tool's own result format.
	//
	// It is stored behind the tag, and the platform places it in a child's
	// context when the datum is bound into a data slot — so it must be THIS
	// resource's bytes and no others'. A tag for one pool carrying the whole
	// result would hand every other pool's entries to this pool's audience.
	Content string
}

// resolveResultReads runs a plural declaration over a tool result.
//
// Shared by both PostToolCall hooks — info_leak_read, which taints and mints,
// and info_leak_audience, which gates the same result against the channel the
// same turn — so the two cannot come to different conclusions about what a call
// read. Parsing the result twice in two places is how they would.
//
// Returns (nil, nil) when the decl declares no plural reads, which is the
// caller's signal to take its single-resource path.
func resolveResultReads(decl *ToolReadsDecl, result string) ([]ToolReadResource, error) {
	if decl == nil || decl.ResultResources == nil {
		return nil, nil
	}
	refs, err := decl.ResultResources(result)
	if err != nil {
		return nil, err
	}
	for i, r := range refs {
		// The minter refuses an incomplete ref too, but it would refuse it
		// having already been handed a datum, and its error names neither the
		// tool nor the call. Fail here, where both are known.
		if r.Type == "" || r.ID == "" || r.Permission == "" {
			// Marked: the declaration read the result and produced a ref this
			// gate cannot use, which is a refusal, not an unreadable envelope.
			return nil, fmt.Errorf("result resource %d needs type, id and permission (got %q/%q/%q): %w",
				i, r.Type, r.ID, r.Permission, ErrUnattributableResource)
		}
	}
	return refs, nil
}
