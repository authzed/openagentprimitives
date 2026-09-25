package pttagcontent

import (
	"context"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// ResolveEntitled returns the content for the requested tag ids that are BOTH
// present in recs AND pass the entitled check, in request order. A tag absent
// from the scope, or one the caller is not entitled to, is OMITTED — never
// returned — which is the filter that keeps the resolve route from becoming a
// read-back channel for a component-read kind. An entitlement-check error fails
// the whole call (fail closed): a partial content set is worse than an error,
// because a caller placing a child's data slots cannot tell a withheld datum
// from a check that broke.
//
// Kept here, a pure function over records the CALLER read, so the operator's
// _pttag_resolve route and the in-process e2e harness gate identically — a
// divergence would make the harness green under a disclosure production forbids.
func ResolveEntitled(recs []ContentRecord, tagIDs []string, entitled func(tagID string) (bool, error)) ([]memory.PtTagContent, error) {
	stored := make(map[string]ContentRecord, len(recs))
	for _, r := range recs {
		stored[r.TagID] = r
	}
	var out []memory.PtTagContent
	for _, id := range tagIDs {
		rec, present := stored[id]
		if !present {
			continue
		}
		ok, err := entitled(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, memory.PtTagContent{TagID: id, Content: rec.Content, MIME: rec.MIME})
	}
	return out, nil
}

// Bind content-binds a set of claimed (id, content) regions against the stored
// content records, returning which ids matched and whether every region did.
//
// The comparison is the load-bearing half of per-datum egress: a region whose
// bytes do not match what the platform stored for its claimed id is fabricated
// or mislabelled, and its presence marks the whole payload unbound so the caller
// falls to the coarse floor rather than honour a partial forge. Kept here, a
// pure function over records the CALLER read, so the operator's verify route and
// the in-process e2e harness bind identically — a divergence between them would
// make the harness green under a comparison production does not perform.
//
// It returns only ids, never content: the caller already holds the bytes it
// asked about, and echoing the stored bytes would make this a read-back channel
// for the very content pt_tag_content keeps out of a session's reach.
func Bind(recs []ContentRecord, regions []memory.PtTagRegion) memory.PtTagVerifyResponse {
	stored := make(map[string]string, len(recs))
	for _, r := range recs {
		stored[r.TagID] = strings.TrimSpace(r.Content)
	}
	resp := memory.PtTagVerifyResponse{AllBound: true}
	for _, reg := range regions {
		want, ok := stored[reg.ID]
		if !ok || want != reg.Content {
			resp.AllBound = false
			continue
		}
		resp.Verified = append(resp.Verified, reg.ID)
	}
	return resp
}

// List returns every stored datum in scope (oldest-first).
//
// PLATFORM-side only. The Kind's SessionReadable is false, so this never
// answers a model's query_memory; it exists for the paths that resolve a tag
// to its bytes — placing a bound data slot into a child's context, or
// answering an audit. A caller reaching for this is asserting it is one of
// those, and the read door still refuses a token-originated request.
//
// Read-only, like pttag's: there is no Record here because content is written
// by the MINTER, in the same call that writes the tag governing it. A shorter
// path would let a caller store bytes under a tag whose audience it did not
// derive.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]ContentRecord, error) {
	return auditaccessor.List[ContentRecord](ctx, m, scope, Kind{}, "pttagcontent.List")
}
