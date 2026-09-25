package relwritesaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// RecordWritten converts the tuples relwrites.Run reports as WRITTEN into
// one Audit entry and records it. Shared by both dispatch paths that call
// relwrites.Run — pkg/agent/tool/mcp and pkg/agent/tool/sandbox — so the two
// can never diverge on what "a tuple landed" gets audited as; source names
// the calling tool (MCPTool.llmName / SandboxTool.Name()).
//
// A no-op — nil error, nothing filed — when written is empty or m is nil.
// Run is partial by construction (one block writes, a sibling marked block
// is refused, both outcomes come back from the same call), so "wrote
// nothing" is a routine outcome, not just the total-failure case: every
// block gated off by its own When, or every tuple refused, must not file an
// empty entry, which would cost a link in the append-only chain and answer
// nothing.
func RecordWritten(ctx context.Context, m memory.Memory, scope memory.Scope, source string, written []relwrites.ResolvedTuple) error {
	if len(written) == 0 || m == nil {
		return nil
	}
	tuples := make([]Tuple, 0, len(written))
	for _, tu := range written {
		tuples = append(tuples, Tuple{Resource: tu.Resource, Relation: tu.Relation, Subject: tu.Subject})
	}
	return Record(ctx, m, scope, "", Audit{Source: source, Tuples: tuples})
}

// Record persists one Audit entry linked to the originating tool call.
// The for_tool_call link carries an empty Kind: a tool_use block id is
// not a memory entry id, so claiming Kind:"turn" would fail Memory.Put's
// link-prefix validation once the turn Kind is registered.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string, a Audit) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("relwritesaudit.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        memory.NewID(Kind{}),
		CreatedAt: time.Now().UTC(),
		Links: []memory.Link{
			{Relation: "for_tool_call", ID: toolUseID},
		},
		Content: raw,
	})
	return err
}

// ByToolCall returns every Audit entry linked to toolUseID.
func ByToolCall(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string) ([]Audit, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		LinkedTo: []memory.LinkFilter{{Relation: "for_tool_call", ID: toolUseID}},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Audit, 0, len(res.Entries))
	for _, e := range res.Entries {
		var a Audit
		if err := json.Unmarshal(e.Content, &a); err != nil {
			// Append-only audit entry that won't decode: log loudly rather
			// than silently drop it — a corrupt/tampered row vanishing from
			// the read result is exactly what the tamper-evident subsystem
			// must surface. Skipping is right here because these rows are a
			// record of writes already made, not an input to a gate.
			undecodable.Skipped("relwritesaudit.ByToolCall", scope, e, err)
			continue
		}
		out = append(out, a)
	}
	return out, nil
}
