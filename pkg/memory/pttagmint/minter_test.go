package pttagmint_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/pttagmint"
)

// fakeSubjects stands in for *spicedb.Client's LookupSubjects.
//
// Its byRef values are BARE canonical ids ("tim"), never "user:tim", because
// that is what the real method returns — it hardcodes SubjectObjectType "user"
// and hands back raw SubjectObjectIds, leaving callers to re-prefix. Seeding
// this fake with pre-prefixed subjects is not a harmless shorthand: it is the
// exact shape that let the minter ship writing tuples it could not parse,
// green under a fake that answered in a form production never produces.
type fakeSubjects struct {
	byRef map[string][]string
	err   error
	asked []string
}

func (f *fakeSubjects) LookupSubjects(_ context.Context, ref string) ([]string, error) {
	f.asked = append(f.asked, ref)
	if f.err != nil {
		return nil, f.err
	}
	return f.byRef[ref], nil
}

type fakeRels struct {
	updates []*v1.RelationshipUpdate
	err     error
}

func (f *fakeRels) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.updates = append(f.updates, req.Updates...)
	return &v1.WriteRelationshipsResponse{}, nil
}

type fakeMem struct {
	puts []memory.Entry
	err  error
}

func (f *fakeMem) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	if f.err != nil {
		return memory.Entry{}, f.err
	}
	f.puts = append(f.puts, e)
	return e, nil
}

func newMinter(subs *fakeSubjects, rels *fakeRels, mem *fakeMem) *pttagmint.Minter {
	return &pttagmint.Minter{
		Subjects: subs, Rels: rels, Mem: mem,
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
	}
}

var testScope = memory.Scope{Kind: "session", ID: "ns/sess"}

// readersOf pulls the direct_reader subjects out of the written tuples — the
// audience as SpiceDB will actually answer it, rather than as the record
// describes it.
func readersOf(t *testing.T, rels *fakeRels) []string {
	t.Helper()
	var out []string
	for _, u := range rels.updates {
		if u.Relationship.Relation == "direct_reader" {
			out = append(out, u.Relationship.Subject.Object.ObjectType+":"+u.Relationship.Subject.Object.ObjectId)
		}
	}
	return out
}

// TestTheAudienceIsDERIVED_NotTaken is the security property this whole
// package exists for.
//
// The request names RESOURCES. The reader set comes from LookupSubjects over
// those resources, and there is no field through which a caller could state an
// audience — so a compromised or injected session cannot mint itself a wide
// tag and then disclose to it legitimately.
func TestTheAudienceIsDERIVED_NotTaken(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{
		"doc:d1#viewer": {"tim", "fred", "sam"},
	}}
	rels, mem := &fakeRels{}, &fakeMem{}

	id, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		ToolUseID: "toolu_1",
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	assert.Equal(t, []string{"doc:d1#viewer"}, subs.asked,
		"the audience must be looked up from the resource, never accepted from the caller")
	assert.ElementsMatch(t, []string{"user:tim", "user:fred", "user:sam"}, readersOf(t, rels))
}

// TestBareLookupSubjectsAreTypedBeforeTheTupleWrite pins the boundary that
// broke: LookupSubjects answers with raw canonical ids, the tuple write needs
// "<type>:<id>", and the minter is what bridges them.
//
// Named as its own case rather than left implicit in the assertions above,
// because the failure it guards is total and silent-shaped. Every mint against
// a real SpiceDB returned "subject … is not in <type>:<id> form", so no tag
// ever got tuples — a datum whose provenance falls back to the session-wide
// taint set looks, from anywhere but the log, like a system with no findings.
// It survived a full unit suite because the fake answered in a form production
// never produces; an e2e bundle is what surfaced it.
func TestBareLookupSubjectsAreTypedBeforeTheTupleWrite(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{
		// The canonical encoding is base64url of an email — no colon in it,
		// which is precisely why the un-prefixed form could not parse.
		"doc:d1#viewer": {"YXVkaXRvci0xQGV4YW1wbGUuY29t"},
	}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		ToolUseID: "toolu_bare",
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
	})
	require.NoError(t, err, "a bare canonical id must not fail the tuple write")
	assert.Equal(t, []string{"user:YXVkaXRvci0xQGV4YW1wbGUuY29t"}, readersOf(t, rels),
		"the subject type is supplied by the minter, not guessed by the parser")
}

// TestReadersAcrossResourcesAreIntersected applies the lattice's own rule
// WITHIN one datum: content assembled from two resources is visible only to
// whoever may see both.
//
// A union here would let a call touching a public resource and a restricted
// one produce a datum readable by the public one's whole audience.
func TestReadersAcrossResourcesAreIntersected(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{
		"doc:d1#viewer": {"tim", "fred", "sam"},
		"doc:d2#viewer": {"tim", "sarah"},
	}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{
			{Type: "doc", ID: "d1", Permission: "viewer"},
			{Type: "doc", ID: "d2", Permission: "viewer"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"user:tim"}, readersOf(t, rels),
		"only the reader authorized on BOTH resources survives")
}

// TestALookupFailureRefusesTheMint pins the direction of a partial failure.
//
// An audience that could not be computed is UNKNOWN, not empty. Recording an
// empty one would look like a deliberate "nobody may see this", and recording
// a partial one would silently widen or narrow the datum against a set nobody
// derived. Neither is acceptable, so the mint fails and the caller decides.
func TestALookupFailureRefusesTheMint(t *testing.T) {
	subs := &fakeSubjects{err: errors.New("spicedb unavailable")}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
	})
	require.Error(t, err)
	assert.Empty(t, rels.updates, "nothing may be written on a derivation we could not complete")
	assert.Empty(t, mem.puts)
}

// TestTuplesAreWrittenBeforeTheRecord pins the failure ordering.
//
// A tag whose tuples exist but whose record is missing is enforceable and
// merely under-audited. The reverse — a record with no tuples — is a tag that
// LOOKS accounted for while `reader` resolves it to nobody, so every
// disclosure of that datum is refused with nothing explaining why.
func TestTuplesAreWrittenBeforeTheRecord(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}}
	rels := &fakeRels{err: errors.New("spicedb write failed")}
	mem := &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
	})
	require.Error(t, err)
	assert.Empty(t, mem.puts,
		"a record must not outlive a failed tuple write, or the tag reads as accounted for while resolving to nobody")
}

// TestADerivedMintTakesNoReadersOfItsOwn keeps the leaf-XOR-derived invariant
// intact across the wire: a derived tag's audience is the intersection its
// sources define, resolved in SpiceDB, and it must carry no direct_reader
// tuples that would union past that.
func TestADerivedMintTakesNoReadersOfItsOwn(t *testing.T) {
	subs := &fakeSubjects{}
	rels, mem := &fakeRels{}, &fakeMem{}

	id, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		DerivedFrom: []string{"ptt-a", "ptt-b"},
	})
	require.NoError(t, err)
	assert.Empty(t, subs.asked, "a derived mint resolves through its sources; it looks nothing up")
	assert.Empty(t, readersOf(t, rels),
		"a direct_reader on a derived tag would union past the intersection its sources define")

	var sources []string
	for _, u := range rels.updates {
		if u.Relationship.Relation == "derived_from" {
			sources = append(sources, u.Relationship.Subject.Object.ObjectId)
		}
	}
	assert.ElementsMatch(t, []string{"ptt-a", "ptt-b"}, sources)

	require.Len(t, mem.puts, 1)
	var rec pttag.TagRecord
	require.NoError(t, json.Unmarshal(mem.puts[0].Content, &rec))
	assert.Equal(t, pttag.KindDerived, rec.Kind)
	assert.Equal(t, string(id), rec.ID)
}

// TestAnUndisclosableLeafIsRecorded pins that "nobody may read this" is a real
// answer rather than an error. A resource with no authorized subjects yields a
// datum with no audience, and every disclosure of it is refused — the correct
// direction.
func TestAnUndisclosableLeafIsRecorded(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{"doc:secret#viewer": nil}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "secret", Permission: "viewer"}},
	})
	require.NoError(t, err, "a resource nobody can read is a fact, not a malformed request")
	assert.Empty(t, readersOf(t, rels))
	require.Len(t, mem.puts, 1)
}

// TestMintRequiresASigningFacadeAgainstAVerifier is the regression for the
// SECOND live-only failure: the operator handed the minter its RAW, unsigned
// memory facade (memLocal) instead of its operator-signing one (opSigned). Every
// mint then passed the per-kind write door (fix above) only to hit the
// append-only verifier — pt_tag / pt_tag_content are append-only — and fail with
//
//	pttagmint: recording tag "ptt-…": append-only kind: signed provenance
//	required: pt_tag/ptt-…
//
// so per-datum egress silently ran coarse forever. The in-process e2e harness
// signs its minter's facade, so this only failed on a real cluster.
//
// Here a facade with verify-on-write enabled (as the operator's is) shows both
// sides: the unsigned facade is refused, the operator-signing facade is accepted.
func TestMintRequiresASigningFacadeAgainstAVerifier(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x07
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: "system:operator", KeyID: signer.KeyID()}: pub}
	local := memory.NewLocal(inmem.NewBackend(),
		memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))

	minterOver := func(mem pttagmint.MemoryWriter) *pttagmint.Minter {
		return &pttagmint.Minter{
			Subjects: &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}},
			Rels:     &fakeRels{},
			Mem:      mem,
			Now:      func() time.Time { return time.Unix(0, 0).UTC() },
		}
	}
	req := memory.PtTagMintRequest{
		ToolUseID: "toolu_sign",
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
		Content:   "the datum",
		MIME:      "text/plain",
	}

	// The component read capability the operator's own writes carry — the
	// SigningMemory reads the chain head to compute seq/prevHash, and the
	// verifier-backed facade gates that read. In production the router attaches a
	// bearer approval for the scope; here the system approval stands in.
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("unsigned facade: refused (signed provenance required)", func(t *testing.T) {
		_, err := minterOver(local).MintPtTag(ctx, testScope, req)
		require.Error(t, err)
		assert.ErrorIs(t, err, memory.ErrProvenanceRequired,
			"an append-only pt_tag write through an UNSIGNED facade must be refused by the verifier — the live failure this pins")
	})

	t.Run("operator-signing facade: accepted", func(t *testing.T) {
		signed := provenance.NewSigningMemory(local, signer)
		id, err := minterOver(signed).MintPtTag(ctx, testScope, req)
		require.NoError(t, err, "the minter must succeed when handed the operator's SIGNING facade (opSigned)")
		assert.NotEmpty(t, id)
	})
}

func TestAMalformedResourceRefIsRefused(t *testing.T) {
	subs := &fakeSubjects{}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1"}}, // no permission
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission")
	assert.Empty(t, rels.updates)
}

// TestMintUnderASessionTokenContextSucceeds is the regression for a failure
// that shipped GREEN and only surfaced on a real enforcing cluster.
//
// The runner asks the operator to mint; the operator's memory HTTP door stamps
// the caller's per-session bearer onto the request context
// (memory.WithTokenSession, httpsrv.go:321). That context then flowed unchanged
// into the component's pt_tag and pt_tag_content Puts — and the per-kind write
// door keys on the token's PRESENCE, not on which facade holds the pen, so it
// refused both as "component-written". Live, every mint failed with
//
//	pttagmint: recording tag "ptt-…": memory: kind is not session-writable:
//	"pt_tag" is component-written
//
// and per-datum egress silently fell back to the coarse floor forever.
//
// The tag IS the operator's own authored write: it DERIVES the audience and a
// session can never name it, which is the whole reason this package exists
// rather than the runner writing the record. So the session token is
// authentication for the HTTP hop, never authorship of the tag, and the
// component must hand off (WithoutTokenSession) before it writes.
//
// The other cases here use fakeMem, which always allows — exactly the blind
// spot that let this ship. This one writes through a REAL memory.Local so the
// write door actually runs. pt_tag and pt_tag_content register via their kinds'
// init(), imported transitively through pttagmint.
func TestMintUnderASessionTokenContextSucceeds(t *testing.T) {
	cases := []struct {
		name string
		req  memory.PtTagMintRequest
	}{
		{
			name: "leaf tag, audience derived from a resource",
			req: memory.PtTagMintRequest{
				ToolUseID: "toolu_leaf",
				Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
				Content:   "the datum the tag governs",
				MIME:      "text/plain",
			},
		},
		{
			name: "derived tag, audience is its DerivedFrom chain",
			req: memory.PtTagMintRequest{
				ToolUseID:   "toolu_derived",
				DerivedFrom: []string{"ptt-source-a", "ptt-source-b"},
				Content:     "content assembled from two source data",
				MIME:        "text/plain",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs := &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}}
			rels := &fakeRels{}
			mem := memory.NewLocal(inmem.NewBackend())
			minter := &pttagmint.Minter{
				Subjects: subs, Rels: rels, Mem: mem,
				Now: func() time.Time { return time.Unix(0, 0).UTC() },
			}

			// Exactly what the memory HTTP door stamps onto a per-session
			// bearer's request before it reaches the minter.
			ctx := memory.WithTokenSession(context.Background(),
				memory.NamespacedName{Namespace: "ns", Name: "sess"})

			id, err := minter.MintPtTag(ctx, testScope, tc.req)
			require.NoError(t, err,
				"the operator's component mint must not be refused by the per-kind write door merely because the request authenticated on a session token")
			require.NotEmpty(t, id)

			// The record actually landed in the enforcing facade — proving the
			// door let the component through, not that the write was skipped.
			// Read back component-side (no token) to isolate the write door
			// from the read door.
			got, ok, gerr := mem.Get(context.Background(), testScope, pttag.KindName, id)
			require.NoError(t, gerr)
			require.True(t, ok, "the pt_tag record must be readable back")
			assert.Equal(t, id, got.ID)
		})
	}
}
