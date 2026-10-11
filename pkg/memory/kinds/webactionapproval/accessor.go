package webactionapproval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Append persists one web_action approval record, linked to the tool call it
// approved.
//
// The entry ID is DERIVED from (toolUseID, reqID) — not toolUseID alone — so a
// retry of the SAME approval (same ask, re-delivered) is a byte-identical
// re-put — idempotent at the append-only facade — while two DIFFERENT
// approval EVENTS never collide even if they happen to share a toolUseID.
//
// reqID (the ask's own request id, minted fresh per raised card — see
// newRequestID in pkg/agent/runner) is what actually carries the uniqueness
// here, not toolUseID: in production the two already agree (a real LLM API
// mints a globally unique tool_use id per call, same as reqID), but a scripted
// test harness whose single-tool-call-per-turn replies each restart their
// tool_use id numbering from zero can legitimately dispatch two distinct
// mutates — or the SAME mutate re-approved against a changed fingerprint, the
// TOCTOU re-prompt case — under the identical toolUseID. Keying the id on
// reqID as well is what tells those apart rather than silently dropping the
// second write as an ErrAppendOnlyConflict; toolUseID alone would do that
// (and used to, before this was added) because the SAME ask retried keeps its
// reqID too, so a genuine retry of one approval is unaffected.
//
// toolUseID is kept as the Link's value (not reqID): pkg/steelthread/read.go
// cross-references records back to the transcript's tool_use blocks via the
// for_tool_call link, and that correspondence must stay the real id a capture
// can find in the transcript.
func Append(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID, reqID string, rec Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("webactionapproval.Append: marshal: %w", err)
	}
	sum := sha256.Sum256([]byte(toolUseID + "|" + reqID))
	id := Kind{}.IDPrefix() + hex.EncodeToString(sum[:8])
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        id,
		CreatedAt: rec.At,
		// Empty-Kind link, like kinds/approval: a tool_use block id is not a
		// memory entry id, so a Kind here would fail Put's link-prefix check.
		Links:   []memory.Link{{Relation: "for_tool_call", ID: toolUseID}},
		Tags:    []string{"kind:web_action_approval"},
		Content: raw,
	})
	return err
}

// List returns every web_action approval recorded in the scope.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Record, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("webactionapproval.List: %w", err)
	}
	out := make([]Record, 0, len(res.Entries))
	for _, e := range res.Entries {
		var r Record
		if err := json.Unmarshal(e.Content, &r); err != nil {
			return nil, fmt.Errorf("webactionapproval.List: decode %s: %w", e.ID, err)
		}
		out = append(out, r)
	}
	return out, nil
}
