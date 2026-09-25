package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

type Backend struct {
	client *Client
}

var _ memory.Backend = (*Backend)(nil)

func NewBackend(client *Client) *Backend {
	return &Backend{client: client}
}

func (*Backend) Capabilities() memory.Capabilities {
	return memory.Capabilities{
		ContentSchemas: true,
		FieldRange:     true,
		LinkTraversal:  -1,
		ReverseLinks:   true,
		TagFilters:     true,
		TimeRange:      true,
	}
}

// linkData is a memory.Link as denormalized onto the entry's links JSONB
// column. The tags are the STORED key names; changing one orphans every link
// already written.
type linkData struct {
	// Relation is the link's free-form label; empty matches any in a LinkFilter.
	Relation string `json:"relation"`
	// TargetScopeKind is the target's scope kind, always the source's today.
	TargetScopeKind string `json:"target_scope_kind"`
	// TargetScopeID is the target's scope id.
	TargetScopeID string `json:"target_scope_id"`
	// TargetKind is the target entry's memory Kind.
	TargetKind string `json:"target_kind"`
	// TargetID is the target entry's id within its Kind.
	TargetID string `json:"target_id"`
}

func marshalProvenance(p *memory.Provenance) []byte {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return b
}

// unmarshalProvenance decodes a stored provenance envelope. A decode error is
// NOT collapsed to a nil envelope: provenance lives on append-only kinds whose
// whole purpose is tamper-evidence, and a nil envelope reads back as "unsigned"
// — which silently lowers the re-seeded chain head and the truncation anchor.
// The error is propagated up the scan path so the corrupt blob is surfaced, not
// laundered into a benign-looking entry.
func unmarshalProvenance(raw []byte) (*memory.Provenance, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p memory.Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("unmarshal provenance: %w", err)
	}
	return &p, nil
}

func marshalLinks(links []memory.Link) []byte {
	if links == nil {
		return []byte("[]")
	}
	data := make([]linkData, len(links))
	for i, l := range links {
		data[i] = linkData{
			Relation:        l.Relation,
			TargetScopeKind: l.Scope.Kind,
			TargetScopeID:   l.Scope.ID,
			TargetKind:      l.Kind,
			TargetID:        l.ID,
		}
	}
	b, _ := json.Marshal(data)
	return b
}

// unmarshalLinks decodes the stored links blob. As with provenance, a decode
// error is surfaced rather than collapsed to nil links — silently dropping
// links would corrupt link-traversal queries with no diagnostic.
func unmarshalLinks(raw []byte) ([]memory.Link, error) {
	if raw == nil {
		return nil, nil
	}
	var data []linkData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("unmarshal links: %w", err)
	}
	links := make([]memory.Link, len(data))
	for i, d := range data {
		links[i] = memory.Link{
			Relation: d.Relation,
			Scope:    memory.Scope{Kind: d.TargetScopeKind, ID: d.TargetScopeID},
			Kind:     d.TargetKind,
			ID:       d.TargetID,
		}
	}
	return links, nil
}

func (b *Backend) Put(ctx context.Context, e memory.Entry) error {
	tx, err := b.client.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres put begin: %w", err)
	}
	defer tx.Rollback(ctx)

	linksJSON := marshalLinks(e.Links)
	provJSON := marshalProvenance(e.Provenance)
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO memory_entry (scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, provenance)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (scope_kind, scope_id, kind, entry_id) DO UPDATE SET
			created_at = EXCLUDED.created_at,
			tags = EXCLUDED.tags,
			content = EXCLUDED.content,
			links = EXCLUDED.links,
			provenance = EXCLUDED.provenance`,
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID,
		e.CreatedAt, tags, e.Content, linksJSON, provJSON)
	if err != nil {
		return fmt.Errorf("postgres put entry: %w", err)
	}

	_, err = tx.Exec(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind = $1 AND source_scope_id = $2 AND source_kind = $3 AND source_id = $4",
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID)
	if err != nil {
		return fmt.Errorf("postgres put delete old links: %w", err)
	}

	if len(e.Links) > 0 {
		rows := make([][]interface{}, len(e.Links))
		for i, l := range e.Links {
			rows[i] = []interface{}{
				e.Scope.Kind, e.Scope.ID, e.Kind, e.ID,
				l.Relation,
				l.Scope.Kind, l.Scope.ID, l.Kind, l.ID,
			}
		}
		_, err = tx.CopyFrom(ctx,
			pgx.Identifier{"memory_link"},
			[]string{"source_scope_kind", "source_scope_id", "source_kind", "source_id",
				"relation", "target_scope_kind", "target_scope_id", "target_kind", "target_id"},
			pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("postgres put links: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (b *Backend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	row := b.client.Pool().QueryRow(ctx,
		"SELECT scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, provenance FROM memory_entry WHERE scope_kind = $1 AND scope_id = $2 AND kind = $3 AND entry_id = $4",
		scope.Kind, scope.ID, kind, id)

	e, err := scanEntry(row)
	if err == pgx.ErrNoRows {
		return memory.Entry{}, false, nil
	}
	if err != nil {
		return memory.Entry{}, false, fmt.Errorf("postgres get: %w", err)
	}
	return e, true, nil
}

func (b *Backend) Delete(ctx context.Context, scope memory.Scope, kind, id string) error {
	tx, err := b.client.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres delete begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		"DELETE FROM memory_entry WHERE scope_kind = $1 AND scope_id = $2 AND kind = $3 AND entry_id = $4",
		scope.Kind, scope.ID, kind, id)
	if err != nil {
		return fmt.Errorf("postgres delete entry: %w", err)
	}
	_, err = tx.Exec(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind = $1 AND source_scope_id = $2 AND source_kind = $3 AND source_id = $4",
		scope.Kind, scope.ID, kind, id)
	if err != nil {
		return fmt.Errorf("postgres delete outbound links: %w", err)
	}
	_, err = tx.Exec(ctx,
		"DELETE FROM memory_link WHERE target_scope_kind = $1 AND target_scope_id = $2 AND target_kind = $3 AND target_id = $4",
		scope.Kind, scope.ID, kind, id)
	if err != nil {
		return fmt.Errorf("postgres delete inbound links: %w", err)
	}
	return tx.Commit(ctx)
}

func (b *Backend) DeleteScope(ctx context.Context, scope memory.Scope) error {
	tx, err := b.client.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres delete scope begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		"DELETE FROM memory_entry WHERE scope_kind = $1 AND scope_id = $2",
		scope.Kind, scope.ID)
	if err != nil {
		return fmt.Errorf("postgres delete scope entries: %w", err)
	}
	_, err = tx.Exec(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind = $1 AND source_scope_id = $2",
		scope.Kind, scope.ID)
	if err != nil {
		return fmt.Errorf("postgres delete scope outbound links: %w", err)
	}
	_, err = tx.Exec(ctx,
		"DELETE FROM memory_link WHERE target_scope_kind = $1 AND target_scope_id = $2",
		scope.Kind, scope.ID)
	if err != nil {
		return fmt.Errorf("postgres delete scope inbound links: %w", err)
	}
	return tx.Commit(ctx)
}

func (b *Backend) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	sql, args, dropped := buildQuery(q)
	rows, err := b.client.Pool().Query(ctx, sql, args...)
	if err != nil {
		return memory.QueryResult{}, fmt.Errorf("postgres query: %w", err)
	}
	defer rows.Close()

	var entries []memory.Entry
	for rows.Next() {
		e, err := scanEntryFromRows(rows)
		if err != nil {
			return memory.QueryResult{}, fmt.Errorf("postgres query scan: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return memory.QueryResult{}, fmt.Errorf("postgres query rows: %w", err)
	}
	return memory.QueryResult{
		Entries:           entries,
		DroppedPredicates: dropped,
		Partial:           len(dropped) > 0,
	}, nil
}

func (b *Backend) Status(ctx context.Context, scope memory.Scope) (memory.ScopeStatus, error) {
	var exists bool
	err := b.client.Pool().QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM memory_entry WHERE scope_kind = $1 AND scope_id = $2)",
		scope.Kind, scope.ID).Scan(&exists)
	if err != nil {
		return memory.ScopeStatus{State: memory.StatusUnknown}, fmt.Errorf("postgres status: %w", err)
	}
	if exists {
		return memory.ScopeStatus{State: memory.StatusLive}, nil
	}
	return memory.ScopeStatus{State: memory.StatusUnknown}, nil
}

func normalizeEntry(e *memory.Entry) {
	if len(e.Tags) == 0 {
		e.Tags = nil
	}
	if len(e.Links) == 0 {
		e.Links = nil
	}
}

func scanEntry(row pgx.Row) (memory.Entry, error) {
	var e memory.Entry
	var linksRaw []byte
	var provRaw []byte
	err := row.Scan(&e.Scope.Kind, &e.Scope.ID, &e.Kind, &e.ID,
		&e.CreatedAt, &e.Tags, &e.Content, &linksRaw, &provRaw)
	if err != nil {
		return e, err
	}
	if e.Links, err = unmarshalLinks(linksRaw); err != nil {
		return e, decodeErr(e, err)
	}
	if e.Provenance, err = unmarshalProvenance(provRaw); err != nil {
		return e, decodeErr(e, err)
	}
	normalizeEntry(&e)
	return e, nil
}

func scanEntryFromRows(rows pgx.Rows) (memory.Entry, error) {
	var e memory.Entry
	var linksRaw []byte
	var provRaw []byte
	err := rows.Scan(&e.Scope.Kind, &e.Scope.ID, &e.Kind, &e.ID,
		&e.CreatedAt, &e.Tags, &e.Content, &linksRaw, &provRaw)
	if err != nil {
		return e, err
	}
	if e.Links, err = unmarshalLinks(linksRaw); err != nil {
		return e, decodeErr(e, err)
	}
	if e.Provenance, err = unmarshalProvenance(provRaw); err != nil {
		return e, decodeErr(e, err)
	}
	normalizeEntry(&e)
	return e, nil
}

// decodeErr annotates a per-column decode failure with the entry's identity so
// an operator reading a surfaced error (or a logged wrap upstream) can locate
// the corrupt row by scope/kind/id without source-diving.
func decodeErr(e memory.Entry, err error) error {
	return fmt.Errorf("scope=%s/%s kind=%s id=%s: %w",
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID, err)
}

// QueryAllScopes runs the admin cross-scope query. See memory.CrossScopeQuery
// for the authorization contract (admind-only; no per-entry post-filter).
func (b *Backend) QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	sql, args := buildCrossScopeQuery(q)
	rows, err := b.client.Pool().Query(ctx, sql, args...)
	if err != nil {
		return memory.QueryResult{}, fmt.Errorf("postgres: cross-scope query: %w", err)
	}
	defer rows.Close()
	var out []memory.Entry
	for rows.Next() {
		e, err := scanEntryFromRows(rows)
		if err != nil {
			return memory.QueryResult{}, fmt.Errorf("postgres: cross-scope scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return memory.QueryResult{}, fmt.Errorf("postgres: cross-scope rows: %w", err)
	}
	return memory.QueryResult{Entries: out}, nil
}
