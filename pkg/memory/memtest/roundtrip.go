// Package memtest provides Backend test doubles for the memory framework.
//
// RoundTripBackend is the reason the package exists: it decorates any Backend
// so entries come back OUT reshaped the way the postgres backend reshapes them
// — content re-emitted from a JSONB column (reordered keys, different
// whitespace), created_at truncated to microseconds by the pgx TIMESTAMPTZ
// encoder, empty tag/link lists mapped back to nil (postgres.normalizeEntry).
//
// Every equivalence / idempotency comparison in the framework has to survive
// that reshaping, and nothing else in the unit tier exercises it — inmem holds
// entries verbatim, sqlite stores content as TEXT and created_at as integer
// nanoseconds — so a comparison correct only on raw bytes is green everywhere a
// developer looks and broken in the mode every remote install runs.
// pkg/memory/postgres proves the same property against a real server (mage
// test:postgres); this proves it with no Docker, in the default suite.
//
// The re-encoding is deliberately NOT a byte-for-byte imitation of postgres's
// JSONB output. Callers may rely only on what a JSONB round trip actually
// guarantees: semantically equal, textually different.
package memtest

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// RoundTripBackend decorates a Backend with the read-side reshaping described
// in the package doc. Writes pass through untouched — like postgres, it is the
// read that reshapes.
type RoundTripBackend struct {
	memory.Backend
}

var _ memory.Backend = (*RoundTripBackend)(nil)

// NewRoundTrip wraps inner. inner must be non-nil.
func NewRoundTrip(inner memory.Backend) *RoundTripBackend {
	if inner == nil {
		panic("memtest.NewRoundTrip: nil Backend")
	}
	return &RoundTripBackend{Backend: inner}
}

func (b *RoundTripBackend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	e, found, err := b.Backend.Get(ctx, scope, kind, id)
	if err != nil || !found {
		return e, found, err
	}
	return Reshape(e), true, nil
}

func (b *RoundTripBackend) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	res, err := b.Backend.Query(ctx, q)
	if err != nil {
		return res, err
	}
	res.Entries = reshapeAll(res.Entries)
	return res, nil
}

func (b *RoundTripBackend) QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	res, err := b.Backend.QueryAllScopes(ctx, q)
	if err != nil {
		return res, err
	}
	res.Entries = reshapeAll(res.Entries)
	return res, nil
}

func reshapeAll(in []memory.Entry) []memory.Entry {
	out := make([]memory.Entry, len(in))
	for i, e := range in {
		out[i] = Reshape(e)
	}
	return out
}

// Reshape returns e as a durable backend would hand it back. Exported so a
// test can assert on the reshaping itself without going through a Backend.
func Reshape(e memory.Entry) memory.Entry {
	e.Content = reshapeJSON(e.Content)
	e.CreatedAt = e.CreatedAt.UTC().Truncate(time.Microsecond)
	if len(e.Tags) == 0 {
		e.Tags = nil
	}
	if len(e.Links) == 0 {
		e.Links = nil
	}
	return e
}

// reshapeJSON re-encodes raw so the bytes differ while the JSON value does
// not: the round trip through interface{} sorts object keys and normalizes
// numbers, and json.Indent then re-inserts structural whitespace using the
// stdlib tokenizer (so string contents are never touched). Invalid JSON is
// returned unchanged — a durable backend would have refused it at write time.
func reshapeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", ""); err != nil {
		return compact
	}
	return json.RawMessage(buf.Bytes())
}
