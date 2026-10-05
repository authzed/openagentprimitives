package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

type Backend struct {
	client *Client
}

var _ memory.Backend = (*Backend)(nil)

func NewBackend(client *Client) *Backend { return &Backend{client: client} }

// Capabilities mirrors the postgres backend exactly: SQLite (JSON1 +
// link subqueries) is a strict peer, not a degraded inmem.
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

// linkData is the JSON shape stored in the entry's links column. The tags are
// the STORED key names and must stay byte-identical to the postgres sibling's,
// or links written by one backend are unreadable by the other.
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

func marshalLinks(links []memory.Link) string {
	data := make([]linkData, len(links))
	for i, l := range links {
		data[i] = linkData{l.Relation, l.Scope.Kind, l.Scope.ID, l.Kind, l.ID}
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// unmarshalLinks decodes the stored links blob. As with the postgres sibling,
// a decode error is surfaced rather than collapsed to nil links — silently
// dropping links would corrupt link-traversal queries with no diagnostic.
func unmarshalLinks(raw string) ([]memory.Link, error) {
	if raw == "" {
		return nil, nil
	}
	var data []linkData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("sqlite: unmarshal links: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
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

func marshalTags(tags []string) string {
	if tags == nil {
		tags = []string{}
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

// marshalProvenance encodes the append-only tamper-evidence envelope for
// storage, returning an untyped nil (SQL NULL) for a mutable kind's absent
// envelope. Returns any rather than string so the NULL is distinguishable
// from an empty blob.
func marshalProvenance(p *memory.Provenance) (any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("sqlite: marshal provenance: %w", err)
	}
	return string(b), nil
}

// unmarshalProvenance decodes a stored provenance envelope. A decode error is
// NOT collapsed to a nil envelope: provenance exists for tamper-evidence, and a
// nil one reads back as "unsigned", which the verifier reports as a mere warning
// while silently lowering the re-seeded chain head and the truncation anchor.
// Propagate it so a corrupt blob surfaces instead of being laundered into a
// benign-looking entry.
func unmarshalProvenance(raw string) (*memory.Provenance, error) {
	if raw == "" {
		return nil, nil
	}
	var p memory.Provenance
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("sqlite: unmarshal provenance: %w", err)
	}
	return &p, nil
}

func (b *Backend) Put(ctx context.Context, e memory.Entry) error {
	createdNS := e.CreatedAt.UTC().UnixNano()
	if e.Provenance != nil && !time.Unix(0, createdNS).Equal(e.CreatedAt) {
		// Reject rather than silently overflowing a signed timestamp. The
		// caller can then reseed its chain from the unchanged durable tail.
		return fmt.Errorf("sqlite put %s/%s: signed CreatedAt is outside the nanosecond timestamp range", e.Kind, e.ID)
	}
	tx, err := b.client.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite put begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var content any
	if e.Content != nil {
		content = string(e.Content)
	}
	prov, err := marshalProvenance(e.Provenance)
	if err != nil {
		return fmt.Errorf("sqlite put entry: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_entry (scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, provenance)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (scope_kind, scope_id, kind, entry_id) DO UPDATE SET
			created_at = excluded.created_at,
			tags = excluded.tags,
			content = excluded.content,
			links = excluded.links,
			provenance = excluded.provenance`,
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID,
		createdNS, marshalTags(e.Tags), content, marshalLinks(e.Links), prov)
	if err != nil {
		return fmt.Errorf("sqlite put entry: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind=? AND source_scope_id=? AND source_kind=? AND source_id=?",
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID); err != nil {
		return fmt.Errorf("sqlite put delete old links: %w", err)
	}
	for _, l := range e.Links {
		if _, err = tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO memory_link
				(source_scope_kind, source_scope_id, source_kind, source_id, relation,
				 target_scope_kind, target_scope_id, target_kind, target_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.Scope.Kind, e.Scope.ID, e.Kind, e.ID, l.Relation,
			l.Scope.Kind, l.Scope.ID, l.Kind, l.ID); err != nil {
			return fmt.Errorf("sqlite put link: %w", err)
		}
	}
	return tx.Commit()
}

func (b *Backend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	row := b.client.db.QueryRowContext(ctx,
		selectEntryColumns+" FROM memory_entry WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?",
		scope.Kind, scope.ID, kind, id)
	e, err := scanEntry(row)
	if err == sql.ErrNoRows {
		return memory.Entry{}, false, nil
	}
	if err != nil {
		return memory.Entry{}, false, fmt.Errorf("sqlite get: %w", err)
	}
	return e, true, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(row rowScanner) (memory.Entry, error) {
	var (
		e          memory.Entry
		createdNS  int64
		tagsRaw    string
		contentRaw sql.NullString
		linksRaw   string
		provRaw    sql.NullString
	)
	if err := row.Scan(&e.Scope.Kind, &e.Scope.ID, &e.Kind, &e.ID,
		&createdNS, &tagsRaw, &contentRaw, &linksRaw, &provRaw); err != nil {
		return e, err
	}
	e.CreatedAt = time.Unix(0, createdNS).UTC()

	var tags []string
	if err := json.Unmarshal([]byte(tagsRaw), &tags); err != nil {
		return e, decodeErr(e, fmt.Errorf("sqlite: unmarshal tags: %w", err))
	}
	if len(tags) > 0 {
		e.Tags = tags
	}

	if contentRaw.Valid {
		e.Content = json.RawMessage(contentRaw.String)
	}

	links, err := unmarshalLinks(linksRaw)
	if err != nil {
		return e, decodeErr(e, err)
	}
	e.Links = links

	if e.Provenance, err = unmarshalProvenance(provRaw.String); err != nil {
		return e, decodeErr(e, err)
	}

	return e, nil
}

// decodeErr annotates a per-column decode failure with the entry's identity so
// an operator reading a surfaced error can locate the corrupt row by
// scope/kind/id without source-diving. The Query path has no other place to
// learn which row failed. Mirrors the postgres sibling.
func decodeErr(e memory.Entry, err error) error {
	return fmt.Errorf("scope=%s/%s kind=%s id=%s: %w",
		e.Scope.Kind, e.Scope.ID, e.Kind, e.ID, err)
}

func (b *Backend) Delete(ctx context.Context, scope memory.Scope, kind, id string) error {
	tx, err := b.client.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite delete begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_entry WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?",
		scope.Kind, scope.ID, kind, id); err != nil {
		return fmt.Errorf("sqlite delete entry: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind=? AND source_scope_id=? AND source_kind=? AND source_id=?",
		scope.Kind, scope.ID, kind, id); err != nil {
		return fmt.Errorf("sqlite delete outbound links: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_link WHERE target_scope_kind=? AND target_scope_id=? AND target_kind=? AND target_id=?",
		scope.Kind, scope.ID, kind, id); err != nil {
		return fmt.Errorf("sqlite delete inbound links: %w", err)
	}
	return tx.Commit()
}

func (b *Backend) DeleteScope(ctx context.Context, scope memory.Scope) error {
	tx, err := b.client.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite delete scope begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_entry WHERE scope_kind=? AND scope_id=?", scope.Kind, scope.ID); err != nil {
		return fmt.Errorf("sqlite delete scope entries: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_link WHERE source_scope_kind=? AND source_scope_id=?", scope.Kind, scope.ID); err != nil {
		return fmt.Errorf("sqlite delete scope outbound links: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_link WHERE target_scope_kind=? AND target_scope_id=?", scope.Kind, scope.ID); err != nil {
		return fmt.Errorf("sqlite delete scope inbound links: %w", err)
	}
	return tx.Commit()
}

func (b *Backend) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	sqlStr, args, dropped := buildQuery(q)
	rows, err := b.client.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return memory.QueryResult{}, fmt.Errorf("sqlite query: %w", err)
	}
	defer rows.Close()

	var entries []memory.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return memory.QueryResult{}, fmt.Errorf("sqlite query scan: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return memory.QueryResult{}, fmt.Errorf("sqlite query rows: %w", err)
	}
	return memory.QueryResult{
		Entries:           entries,
		DroppedPredicates: dropped,
		Partial:           len(dropped) > 0,
	}, nil
}

// QueryAllScopes runs the admin cross-scope query. See memory.CrossScopeQuery
// for the authorization contract (admind-only; no per-entry post-filter).
func (b *Backend) QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	sqlStr, args := buildCrossScopeQuery(q)
	rows, err := b.client.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return memory.QueryResult{}, fmt.Errorf("sqlite cross-scope query: %w", err)
	}
	defer rows.Close()

	var entries []memory.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return memory.QueryResult{}, fmt.Errorf("sqlite cross-scope scan: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return memory.QueryResult{}, fmt.Errorf("sqlite cross-scope rows: %w", err)
	}
	return memory.QueryResult{Entries: entries}, nil
}

func (b *Backend) Status(ctx context.Context, scope memory.Scope) (memory.ScopeStatus, error) {
	var exists bool
	err := b.client.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM memory_entry WHERE scope_kind=? AND scope_id=?)",
		scope.Kind, scope.ID).Scan(&exists)
	if err != nil {
		return memory.ScopeStatus{State: memory.StatusUnknown}, fmt.Errorf("sqlite status: %w", err)
	}
	if exists {
		return memory.ScopeStatus{State: memory.StatusLive}, nil
	}
	return memory.ScopeStatus{State: memory.StatusUnknown}, nil
}
