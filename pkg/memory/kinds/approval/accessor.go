package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// requestIDTagPrefix tags request entries with the channel-visible
// request id — the only key a Show Details button click carries — so
// channelsd can look details up after its in-process cache is gone.
const requestIDTagPrefix = "request:"

// RecordRequest persists an approval request. Tag kind:request so
// ByToolCall can distinguish from outcome entries; when a RequestID is
// present, also tag request:<id> for RequestByID.
func RecordRequest(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string, r Request) error {
	tags := []string{"kind:request"}
	if r.RequestID != "" {
		tags = append(tags, requestIDTagPrefix+r.RequestID)
	}
	return record(ctx, m, scope, toolUseID, tags, r)
}

// RecordOutcome persists the approver's decision.
func RecordOutcome(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string, o Outcome) error {
	return record(ctx, m, scope, toolUseID, []string{"kind:outcome"}, o)
}

// The for_tool_call link carries an empty Kind: a tool_use block id is
// not a memory entry id, so claiming Kind:"turn" would fail Memory.Put's
// link-prefix validation once the turn Kind is registered.
func record(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string, tags []string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("approval.record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        memory.NewID(Kind{}),
		CreatedAt: time.Now().UTC(),
		Links: []memory.Link{
			{Relation: "for_tool_call", ID: toolUseID},
		},
		Tags:    tags,
		Content: raw,
	})
	return err
}

// Pair groups the request + outcome for one toolUseID.
type Pair struct {
	Request *Request
	Outcome *Outcome
}

// ByToolCall returns the request/outcome pair (either field may be
// nil if not yet recorded).
func ByToolCall(ctx context.Context, m memory.Memory, scope memory.Scope, toolUseID string) (Pair, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		LinkedTo: []memory.LinkFilter{{Relation: "for_tool_call", ID: toolUseID}},
	})
	if err != nil {
		return Pair{}, err
	}
	var out Pair
	for _, e := range res.Entries {
		// An undecodable row is SKIPPED: approval is append-only, so it can
		// never be deleted, and failing the read would wedge every approval
		// lookup in the scope forever. The skip is announced, not silent — a
		// vanished outcome leaves the tool call waiting on an answer that was
		// recorded and cannot be read, and an operator needs something to find.
		switch {
		case hasTag(e.Tags, "kind:request"):
			var r Request
			if err := json.Unmarshal(e.Content, &r); err != nil {
				undecodable.Skipped("approval.ByToolCall(request)", scope, e, err)
			} else {
				out.Request = &r
			}
		case hasTag(e.Tags, "kind:outcome"):
			var o Outcome
			if err := json.Unmarshal(e.Content, &o); err != nil {
				undecodable.Skipped("approval.ByToolCall(outcome)", scope, e, err)
			} else {
				out.Outcome = &o
			}
		}
	}
	return out, nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// RequestByID returns the request entry tagged request:<requestID>, or
// (nil, nil) when no such entry exists (requests recorded before payload
// persistence, or an unknown id). Only a query/decode failure is an error.
func RequestByID(ctx context.Context, m memory.Memory, scope memory.Scope, requestID string) (*Request, error) {
	if requestID == "" {
		return nil, nil
	}
	res, err := m.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{Kind{}.Name()},
		Tags: []string{"kind:request", requestIDTagPrefix + requestID},
	})
	if err != nil {
		return nil, fmt.Errorf("approval.RequestByID: %w", err)
	}
	for _, e := range res.Entries {
		var r Request
		if err := json.Unmarshal(e.Content, &r); err != nil {
			return nil, fmt.Errorf("approval.RequestByID: decode %s: %w", e.ID, err)
		}
		return &r, nil
	}
	return nil, nil
}
