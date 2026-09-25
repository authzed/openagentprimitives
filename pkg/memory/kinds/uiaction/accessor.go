package uiaction

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Content is one action record's body: the whole of what a browser learns
// about a call it made.
type Content struct {
	// RequestID is webd's correlation key for the call this record tracks.
	RequestID string `json:"requestId"`
	// Action is the DECLARED action name, never the underlying tool name.
	Action string `json:"action"`
	// State is the lifecycle point reached so far; see IsTerminal.
	State State `json:"state"`
	// Message is browser-safe human copy — never a diagnostic or an id.
	Message string `json:"message,omitempty"`
	// UpdatedAt is when this record was last rewritten.
	UpdatedAt time.Time `json:"updatedAt"`

	// Requester is the canonical user id of the viewer who clicked, WITHOUT
	// the SpiceDB "user:" prefix (see RequesterKey). It is the filter that
	// makes a shared session safe to render: webd surfaces a record only to
	// the viewer who created it, and strips this field before anything reaches
	// a browser, so two people watching one session do not see each other's
	// in-flight buttons.
	Requester string `json:"requester"`

	// ApprovalAddressedToViewer is true only once an approval ask has been
	// published AND the resolved approver set contains Requester. It drives the
	// chrome auto-reveal: approver IS the viewer => reveal; someone else => no
	// reveal. False while unknown, which fails toward NOT revealing — a viewer
	// can always toggle chrome open, and revealing on every approval in a
	// shared session is chrome that cries wolf.
	ApprovalAddressedToViewer bool `json:"approvalAddressedToViewer,omitempty"`
}

// EntryID is the deterministic memory Entry.ID for a request: one record per
// request ID, so every write for the same ID lands at the same key and
// REPLACES whatever was there — which is why this Kind is mutable rather
// than append-only (see Kind.Retention's doc comment).
func EntryID(requestID string) string {
	return IDPrefix + requestID
}

// RequesterKey normalizes a SpiceDB subject ("user:<canonical>") to the bare
// canonical form Content.Requester stores. Defined ONCE and called by BOTH the
// runner (which gets the prefixed form on the wire) and webd, so the two cannot
// disagree about whether a record belongs to the connected viewer — a
// hand-rolled TrimPrefix at either call site is how that filter silently starts
// matching nothing. A subject with no "user:" prefix comes back unchanged.
func RequesterKey(spiceDBSubject string) string {
	if canon, err := identity.Subject(spiceDBSubject).CanonicalUserID(); err == nil {
		return canon.String()
	}
	return spiceDBSubject
}

// Record writes (or replaces) one action record. Errors are RETURNED, never
// swallowed: a lost record is a control that never re-enables.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("uiaction.Record: marshal %q: %w", c.RequestID, err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        EntryID(c.RequestID),
		CreatedAt: c.UpdatedAt,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("uiaction.Record: put %q: %w", c.RequestID, err)
	}
	return nil
}

// ByRequestID returns one record, or (zero, false, nil) when no record has
// been written yet for requestID — a reader that arrives before the first
// write (or after the record expired) gets "not found", not an error.
func ByRequestID(ctx context.Context, m memory.Memory, scope memory.Scope, requestID string) (Content, bool, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{KindName},
		IDs:   []string{EntryID(requestID)},
	})
	if err != nil {
		return Content{}, false, fmt.Errorf("uiaction.ByRequestID: query %q: %w", requestID, err)
	}
	if len(res.Entries) == 0 {
		return Content{}, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return Content{}, false, fmt.Errorf("uiaction.ByRequestID: unmarshal %q: %w", res.Entries[0].ID, err)
	}
	return c, true, nil
}

// List returns every action record in scope whose Requester matches, newest
// first, bounded by limit (0 = unbounded). The Requester filter is applied
// HERE, after the Query, so no caller can forget it and leak one viewer's
// in-flight action to another in a shared session. Query.Limit is deliberately
// left unset before that filter runs, or other requesters' records could
// exhaust it before this Kind's own filter ever saw them.
func List(ctx context.Context, m memory.Memory, scope memory.Scope, requester string, limit int) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{KindName},
		OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
	})
	if err != nil {
		return nil, fmt.Errorf("uiaction.List: query: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			return nil, fmt.Errorf("uiaction.List: unmarshal %q: %w", e.ID, err)
		}
		if c.Requester != requester {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
