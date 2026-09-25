package admind

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	memoryPageSize     = 500
	memoryMaxScan      = 20000
	memorySearchLimit  = 100
	memoryEntriesLimit = 200 // recent entries returned for a ?kind= drill-in
)

// memoryKindRollup is one per-Kind row of the Memory browser's rollup table.
type memoryKindRollup struct {
	Kind       string `json:"kind"`       // the memory kind being rolled up
	Entries    int    `json:"entries"`    // entries of this kind across every scope scanned
	Scopes     int    `json:"scopes"`     // distinct scope count
	LastWrite  string `json:"lastWrite"`  // max CreatedAt, RFC3339, "" when none
	AppendOnly bool   `json:"appendOnly"` // the kind refuses edits and per-entry deletes
}

// memoryEntryRow is one entry, returned for a ?kind= drill-in or a ?q= search.
type memoryEntryRow struct {
	Kind    string  `json:"kind"`
	Scope   string  `json:"scope"` // scope id, e.g. "ns/name"
	ID      string  `json:"id"`
	Created string  `json:"created"`         // RFC3339
	Score   float64 `json:"score,omitempty"` // search relevance; 0/absent for rollup drill-ins
}

// memoryResponse is the Memory browser payload: per-Kind rollups, an optional
// entry list (?kind= recent entries or ?q= search hits), and a truncation flag.
type memoryResponse struct {
	Rollups   []memoryKindRollup `json:"rollups"`
	Entries   []memoryEntryRow   `json:"entries,omitempty"`
	Truncated bool               `json:"truncated"`
}

// handleMemory serves GET /admin/v1/memory. Without ?q= it returns per-Kind
// rollups across every session scope (a bounded cross-scope scan grouped by
// Kind); ?kind= additionally returns that Kind's recent entries. With ?q= it
// returns ranked search hits instead. ADMIN-ONLY — the route is gated on the
// view_audit platform permission; the cross-scope query bypasses per-entry
// authz by design.
func (a *Admind) handleMemory(w http.ResponseWriter, r *http.Request) {
	// The route is gated on platform#view_audit (admind.go); an authorized admin
	// may browse memory cross-session, so mint a system approval to clear the
	// memory doors — the ranked cross-scope Search now fails closed on empty
	// scopes, so this is required for the ?q= branch.
	ctx := memory.WithSystemApproval(r.Context(), "operator:admind")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kindFilter := strings.TrimSpace(r.URL.Query().Get("kind"))

	if q != "" {
		a.memorySearch(ctx, w, q, kindFilter)
		return
	}

	kinds := registeredMemoryKindNames()
	if len(kinds) == 0 {
		// No Kinds registered (no blank imports): nothing to roll up.
		writeJSON(w, http.StatusOK, memoryResponse{Rollups: []memoryKindRollup{}})
		return
	}

	type kindAgg struct {
		entries int
		scopes  map[string]struct{}
		last    time.Time
	}
	aggs := map[string]*kindAgg{}
	scanned := 0
	truncated := false
scan:
	for offset := 0; ; offset += memoryPageSize {
		res, err := a.cfg.Mem.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: kinds,
			OrderDesc: true, Limit: memoryPageSize, Offset: offset,
		})
		if err != nil {
			a.cfg.Logger.Info("admind: memory rollup query failed", "err", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "memory query failed: "+err.Error())
			return
		}
		for _, e := range res.Entries {
			scanned++
			g := aggs[e.Kind]
			if g == nil {
				g = &kindAgg{scopes: map[string]struct{}{}}
				aggs[e.Kind] = g
			}
			g.entries++
			g.scopes[e.Scope.ID] = struct{}{}
			if e.CreatedAt.After(g.last) {
				g.last = e.CreatedAt
			}
			if scanned >= memoryMaxScan {
				a.cfg.Logger.Info("admind: memory rollup scan bound hit; result truncated", "maxScan", memoryMaxScan)
				truncated = true
				break scan
			}
		}
		if len(res.Entries) < memoryPageSize {
			break
		}
	}

	rollups := make([]memoryKindRollup, 0, len(aggs))
	for kind, g := range aggs {
		last := ""
		if !g.last.IsZero() {
			last = g.last.UTC().Format(time.RFC3339)
		}
		rollups = append(rollups, memoryKindRollup{
			Kind:       kind,
			Entries:    g.entries,
			Scopes:     len(g.scopes),
			LastWrite:  last,
			AppendOnly: memory.KindAppendOnly(kind),
		})
	}
	sort.Slice(rollups, func(i, j int) bool {
		if rollups[i].Entries != rollups[j].Entries {
			return rollups[i].Entries > rollups[j].Entries
		}
		return rollups[i].Kind < rollups[j].Kind
	})

	resp := memoryResponse{Rollups: rollups, Truncated: truncated}
	if kindFilter != "" {
		entries, err := a.recentMemoryEntries(ctx, kindFilter)
		if err != nil {
			a.cfg.Logger.Info("admind: memory kind entries query failed", "kind", kindFilter, "err", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "memory kind query failed: "+err.Error())
			return
		}
		resp.Entries = entries
	}
	writeJSON(w, http.StatusOK, resp)
}

// recentMemoryEntries returns the newest entries of a single Kind across all
// session scopes (the ?kind= drill-in).
func (a *Admind) recentMemoryEntries(ctx context.Context, kind string) ([]memoryEntryRow, error) {
	res, err := a.cfg.Mem.QueryAllScopes(ctx, memory.CrossScopeQuery{
		ScopeKind: "session", Kinds: []string{kind},
		OrderDesc: true, Limit: memoryEntriesLimit,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]memoryEntryRow, 0, len(res.Entries))
	for _, e := range res.Entries {
		rows = append(rows, memoryEntryRow{
			Kind: e.Kind, Scope: e.Scope.ID, ID: e.ID,
			Created: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return rows, nil
}

// memorySearch serves the ?q= branch: a ranked cross-scope search. An empty
// Scopes set means "all scopes" for the text/vector providers (postgres,
// graphiti). When no searcher is configured the facade returns
// ErrNoSearchProviders, which we surface as an empty result (200) rather than a
// 500 — search is an optional capability, not a failure.
func (a *Admind) memorySearch(ctx context.Context, w http.ResponseWriter, q, kindFilter string) {
	req := memory.SearchRequest{Text: q, Limit: memorySearchLimit}
	if kindFilter != "" {
		req.Kinds = []string{kindFilter}
	}
	res, err := a.cfg.Mem.Search(ctx, req)
	if err != nil {
		if errors.Is(err, memory.ErrNoSearchProviders) {
			writeJSON(w, http.StatusOK, memoryResponse{Rollups: []memoryKindRollup{}, Entries: []memoryEntryRow{}})
			return
		}
		a.cfg.Logger.Info("admind: memory search failed", "q", q, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "memory search failed: "+err.Error())
		return
	}
	entries := make([]memoryEntryRow, 0, len(res.Entries))
	for _, se := range res.Entries {
		entries = append(entries, memoryEntryRow{
			Kind: se.Entry.Kind, Scope: se.Entry.Scope.ID, ID: se.Entry.ID,
			Created: se.Entry.CreatedAt.UTC().Format(time.RFC3339),
			Score:   se.Score,
		})
	}
	writeJSON(w, http.StatusOK, memoryResponse{Rollups: []memoryKindRollup{}, Entries: entries})
}

// registeredMemoryKindNames returns every registered Kind name — the kind set
// the cross-scope rollup scan queries (QueryAllScopes requires a non-empty
// Kinds list; the results are grouped back by entry Kind).
func registeredMemoryKindNames() []string {
	ks := memory.RegisteredKinds()
	names := make([]string, 0, len(ks))
	for _, k := range ks {
		names = append(names, k.Name())
	}
	return names
}
