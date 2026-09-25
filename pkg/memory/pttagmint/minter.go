// Package pttagmint derives a datum's audience and records its provenance tag.
//
// It is the component half of the split the pt_tag write door creates. The
// runner knows WHICH tool call carried WHICH resource into context, and only
// the runner knows it; but a session credential must never author a tag,
// because a tag's direct_reader set GRANTS disclosure and a session that could
// write one could name an audience its source never authorized.
//
// So the runner asks and this answers. The request names RESOURCES; the reader
// set is derived here, by LookupSubjects, from the resources themselves. There
// is deliberately no path by which a caller can state an audience — that is
// the entire security property, and it is why this package exists rather than
// the runner simply writing the record.
package pttagmint

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

// Source identifies this package's relationship write — the derived
// reader-set tuple that makes a minted tag's audience enforceable, not just
// recorded. Claims exactly the four pt_tag relations writeTuples below ever
// TOUCHes; this package writes no others and does no deletes.
//
// Declared exactly once here; every wiring site (the operator, the e2e
// in-process runner factory) references this var rather than retyping the
// name, so the claim can never drift from what this writer actually
// presents.
var Source = relsource.Source{
	Name: "pttagmint",
	Claims: []string{
		"pt_tag#session",
		"pt_tag#direct_reader",
		"pt_tag#derived_from",
		"pt_tag#untrusted_origin",
	},
}

func init() {
	relsource.Register(Source)
}

// SubjectLookup expands a subject-set expression ("doc:d1#viewer") to the
// concrete subjects it contains.
type SubjectLookup interface {
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
}

// RelationshipWriter writes the tuples that make a tag's audience answerable.
//
// The memory record alone decides nothing: `reader` is a SpiceDB permission,
// so the tuples ARE the authorization input and the record is its durable
// account. Both are written here, by the same component, from the same derived
// reader set — writing one without the other would leave a tag that is either
// unenforceable or unauditable.
type RelationshipWriter interface {
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)
}

// MemoryWriter is the COMPONENT memory credential — the operator's in-process
// facade, not a session bearer, or the per-kind write door would refuse it.
type MemoryWriter interface {
	Put(ctx context.Context, e memory.Entry) (memory.Entry, error)
}

// Minter implements httpsrv.PtTagMinter.
type Minter struct {
	Subjects SubjectLookup
	Rels     RelationshipWriter
	Mem      MemoryWriter
	// Now is injectable so a test can assert the stamped time. Nil means
	// time.Now.
	Now func() time.Time
}

func (m *Minter) now() time.Time {
	if m.Now == nil {
		return time.Now().UTC()
	}
	return m.Now().UTC()
}

// MintPtTag derives the audience, writes the tuples, and records the tag.
//
// Ordering is deliberate: TUPLES FIRST, then the record. A tag whose tuples
// exist but whose record is missing is enforceable and merely under-audited —
// checks answer correctly and the gap is visible. The reverse is worse: a
// record with no tuples is a tag that LOOKS accounted for while `reader`
// resolves it to nobody, so every disclosure of that datum is refused with no
// indication why. Fail toward the recoverable direction.
func (m *Minter) MintPtTag(ctx context.Context, scope memory.Scope, req memory.PtTagMintRequest) (string, error) {
	if m.Subjects == nil || m.Rels == nil || m.Mem == nil {
		return "", fmt.Errorf("pttagmint: minter is not fully wired")
	}
	// Authorship hand-off + component authority. The runner authenticates the
	// mint request on its per-session bearer, and the memory HTTP door stamps
	// that token onto ctx (memory.WithTokenSession) with a WriteMemory-only
	// bearer approval. But the tag is NOT the session's write: the audience is
	// DERIVED here, by LookupSubjects over the resources, and a session can
	// never name it — that is the whole reason pt_tag is component-written. So
	// the token is authentication for the hop, not authorship of the record.
	//
	//   - WithoutTokenSession: drop the token before the pt_tag / pt_tag_content
	//     Puts, or the per-kind write door — which keys on the token's presence,
	//     not on which facade holds the pen — refuses the component its own kind.
	//   - WithSystemApproval: the append-only Put (through the operator's signing
	//     facade) SEEDS the provenance chain on the first write into a scope,
	//     which SCANS every append-only kind for the publisher's head — including
	//     ones like `approval` that need memory:READ, which the bearer's
	//     WriteMemory approval does not grant. Seeding the operator's own chain is
	//     a component read, so approve it as one; otherwise a mint into a
	//     not-yet-seeded scope fails ("scanning append-only kind ... missing
	//     capability approval"). Leaf mints escaped this only when a prior
	//     in-process write had already seeded the scope; a derived mint hit it.
	//
	// Both are no-ops in the pure in-process operator path (no token, and its own
	// ctx is already system-approved).
	ctx = memory.WithSystemApproval(memory.WithoutTokenSession(ctx), "pttag_mint")
	id := provenance.TagID(memory.NewID(pttag.Kind{}))

	var (
		tag provenance.Tag
		err error
	)
	switch {
	case len(req.DerivedFrom) > 0:
		sources := make([]provenance.TagID, 0, len(req.DerivedFrom))
		for _, s := range req.DerivedFrom {
			sources = append(sources, provenance.TagID(s))
		}
		tag, err = provenance.NewDerived(id, sources)
	default:
		var readers []string
		readers, err = m.deriveReaders(ctx, req.Resources)
		if err != nil {
			return "", err
		}
		tag, err = provenance.NewLeaf(id, readers, req.UntrustedOrigin)
	}
	if err != nil {
		return "", fmt.Errorf("pttagmint: composing tag: %w", err)
	}

	if err := m.writeTuples(ctx, scope, tag); err != nil {
		return "", err
	}
	// Sources only for a leaf: a derived tag's provenance is its DerivedFrom,
	// and naming resources there would claim a derivation the schema does not
	// make.
	var sources []string
	if len(req.DerivedFrom) == 0 {
		for _, res := range req.Resources {
			sources = append(sources, res.Type+":"+res.ID)
		}
	}
	rec := pttag.FromTag(tag, sources, req.ToolUseID, m.now())
	content, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("pttagmint: encoding tag record %q: %w", id, err)
	}
	if _, err := m.Mem.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      pttag.KindName,
		ID:        string(id),
		CreatedAt: rec.MintedAt,
		Content:   content,
	}); err != nil {
		return "", fmt.Errorf("pttagmint: recording tag %q: %w", id, err)
	}
	if err := m.writeContent(ctx, scope, id, req); err != nil {
		return "", err
	}
	return string(id), nil
}

// writeContent stores the datum behind the tag that governs it.
//
// AFTER the tag record, deliberately. The tag is what a disclosure decision
// consults; content stored first would be bytes with no audience governing them
// if the tag write then failed. This order fails the other way — a tag with no
// content, which is a complete provenance record that simply cannot be handed
// onward — and that is the degradation to prefer.
//
// A mint with no content is the norm rather than an error: only calls whose
// result is worth passing to a child need storing, and every existing caller
// that just wants provenance passes none.
func (m *Minter) writeContent(ctx context.Context, scope memory.Scope, id provenance.TagID, req memory.PtTagMintRequest) error {
	if req.Content == "" {
		return nil
	}
	rec := pttagcontent.ContentRecord{
		TagID:    string(id),
		Content:  req.Content,
		MIME:     req.MIME,
		StoredAt: m.now(),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("pttagmint: encoding content for tag %q: %w", id, err)
	}
	if _, err := m.Mem.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      pttagcontent.KindName,
		ID:        memory.NewID(pttagcontent.Kind{}),
		CreatedAt: rec.StoredAt,
		Content:   body,
	}); err != nil {
		return fmt.Errorf("pttagmint: storing content for tag %q: %w", id, err)
	}
	return nil
}

// deriveReaders expands every resource the call touched and INTERSECTS them.
//
// The intersection is the same rule the schema applies between tags, applied
// here between the resources of a single datum: a datum assembled from two
// resources is visible only to whoever may see both. Taking a union would let
// a tool call that touched a public resource and a restricted one produce a
// datum readable by the public resource's whole audience.
//
// An empty result is a legitimate answer — a datum nobody may see — and is
// recorded as such rather than treated as an error. NewLeaf accepts it, and
// the disclosure it produces is a refusal, which is the correct direction.
func (m *Minter) deriveReaders(ctx context.Context, resources []memory.PtTagResourceRef) ([]string, error) {
	sets := make([][]string, 0, len(resources))
	for _, res := range resources {
		if res.Type == "" || res.ID == "" || res.Permission == "" {
			return nil, fmt.Errorf("pttagmint: resource ref needs type, id and permission (got %q/%q/%q)", res.Type, res.ID, res.Permission)
		}
		ref := res.Type + ":" + res.ID + "#" + res.Permission
		subs, err := m.Subjects.LookupSubjects(ctx, ref)
		if err != nil {
			// Never fall back to "no restriction" on a lookup failure: an
			// audience we could not compute is unknown, not empty, and an
			// unknown one must not be recorded as though it had been derived.
			return nil, fmt.Errorf("pttagmint: expanding %q: %w", ref, err)
		}
		// Re-prefix. LookupSubjects answers with RAW subject object ids —
		// "<canonicalID>", never "user:<canonicalID>" — and states that
		// callers needing the typed form re-prefix. It is always `user`
		// because that method hardcodes SubjectObjectType "user".
		//
		// Doing this here rather than letting parseSubject default the type
		// keeps the guess out of the parser: a parser that supplies a missing
		// type cannot tell a bare canonical id from a genuinely malformed
		// subject, and would write tuples for both.
		typed := make([]string, 0, len(subs))
		for _, s := range subs {
			typed = append(typed, "user:"+s)
		}
		sets = append(sets, typed)
	}
	return provenance.IntersectAudiences(sets), nil
}

// writeTuples records the tag's shape in SpiceDB, where `reader` is answered.
func (m *Minter) writeTuples(ctx context.Context, scope memory.Scope, tag provenance.Tag) error {
	obj := &v1.ObjectReference{ObjectType: "pt_tag", ObjectId: string(tag.ID())}
	updates := []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: obj, Relation: "session",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
				ObjectType: "agentsession", ObjectId: scope.ID,
			}},
		},
	}}
	for _, r := range tag.DirectReaders() {
		sub, err := parseSubject(r)
		if err != nil {
			return fmt.Errorf("pttagmint: tag %q: %w", tag.ID(), err)
		}
		updates = append(updates, &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: obj, Relation: "direct_reader", Subject: sub,
			},
		})
	}
	for _, src := range tag.DerivedFrom() {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: obj, Relation: "derived_from",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
					ObjectType: "pt_tag", ObjectId: string(src),
				}},
			},
		})
	}
	if tag.UntrustedOrigin() {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: obj, Relation: "untrusted_origin", Subject: &v1.SubjectReference{Object: obj},
			},
		})
	}
	if _, err := m.Rels.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return fmt.Errorf("pttagmint: writing tuples for tag %q: %w", tag.ID(), err)
	}
	return nil
}

// parseSubject splits a "type:id" subject produced by LookupSubjects.
//
// Refuses anything it cannot split rather than guessing a type. A malformed
// subject silently dropped would narrow the audience without saying so, which
// looks exactly like a correct restrictive answer.
func parseSubject(s string) (*v1.SubjectReference, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && i > 0 && i+1 < len(s) {
			return &v1.SubjectReference{Object: &v1.ObjectReference{
				ObjectType: s[:i], ObjectId: s[i+1:],
			}}, nil
		}
	}
	return nil, fmt.Errorf("subject %q is not in <type>:<id> form", s)
}
