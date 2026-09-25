// Package overview computes the admin dashboard's Overview payload: token
// rollups by model and by AgentClass, a 24-hour hourly token time-series, and
// the top-line KPI scalars. It draws from sources already in admind — the live
// session snapshot (for model/class attribution and active counts) and a
// bounded cross-scope scan of `turn` + `authz_decision` memory entries (for the
// time-series and denial/tool-call counts).
//
// Like pkg/web/admind/audit, this package deliberately does NOT import its parent
// admind package: it depends on narrow seams (a memory querier interface and a
// snapshot function) so the parent can wire it without an import cycle and so
// tests can stub both sides. A short in-process cache makes Overview a
// "last computed within TTL" read rather than a per-request full scan.
package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

const (
	pageSize   = 500
	maxScan    = 20000
	defaultTTL = 10 * time.Second
	buckets    = 24
)

// MemQuerier is the narrow memory seam the aggregator needs — *memory.Local
// satisfies it. Kept minimal so tests can stub it.
type MemQuerier interface {
	QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error)
}

// LiveSession is the minimal projection of a live AgentSession the overview
// aggregator needs. The admind parent adapts its richer SessionState (which
// carries the resolved model, agent class, active flag, and per-session token
// counters) into this shape via the Snapshot function — keeping this package
// free of the parent import.
type LiveSession struct {
	Model        string
	Class        string
	Active       bool
	InputTokens  int64
	OutputTokens int64
	// PendingApprovals is the session's count of pending approvals across all
	// kinds (tool-call + leakage + content-inspection). Summed into the
	// ApprovalsPending KPI.
	PendingApprovals int
	// ByModel is the session's per-served-model token breakdown (projected from
	// admind.SessionState.ByModel), each bucket's Model already the uniform
	// <provider>/<model> display id. EMPTY for a session that has not ended
	// yet, in which case the rollup below attributes InputTokens/OutputTokens
	// under the single Model field above — EITHER the buckets OR the session
	// totals, never both, so nothing is double-counted.
	ByModel []ModelBucket
}

// ModelBucket mirrors v1alpha1.ModelCostBucket's token counts without pulling
// in the CRD import — this package deliberately stays free of it (see the
// package doc). Only tokens are needed here; the cost breakdown lives in
// pkg/web/admind/budget.go, which already imports v1alpha1.
type ModelBucket struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// ModelTokens is one per-model token rollup.
type ModelTokens struct {
	// Model is the uniform "<provider>/<model>" display id.
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	// Sessions counts how many live sessions contributed to this row.
	Sessions int `json:"sessions"`
}

// ClassTokens is one per-AgentClass token rollup.
type ClassTokens struct {
	Class        string `json:"class"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	// Sessions counts how many live sessions contributed to this row.
	Sessions int `json:"sessions"`
}

// HourBucket is one hour of the 24-hour token time-series.
type HourBucket struct {
	// HourStartUnix is the bucket's start, in Unix seconds UTC.
	HourStartUnix int64 `json:"hourStartUnix"`
	InputTokens   int64 `json:"inputTokens"`
	OutputTokens  int64 `json:"outputTokens"`
}

// KPIs are the dashboard's top-line scalars.
type KPIs struct {
	// ActiveSessions counts live sessions currently mid-turn.
	ActiveSessions int `json:"activeSessions"`
	// TokensToday is input+output across today's buckets.
	TokensToday int64 `json:"tokensToday"`
	// ToolCalls24h counts tool calls in the scanned 24-hour window.
	ToolCalls24h int `json:"toolCalls24h"`
	// Denials24h counts authz denials in the same window.
	Denials24h int `json:"denials24h"`
	// ApprovalsPending sums live sessions' pending approvals.
	ApprovalsPending int `json:"approvalsPending"`
}

// Overview is the full admin Overview payload.
type Overview struct {
	// ByModel is the per-model rollup, ordered for display.
	ByModel []ModelTokens `json:"byModel"`
	// ByAgentClass is the per-class rollup, ordered for display.
	ByAgentClass []ClassTokens `json:"byAgentClass"`
	// Series24h is a FIXED 24-hour window, oldest first; empty hours are zeroed
	// buckets rather than gaps.
	Series24h [buckets]HourBucket `json:"series24h"`
	// KPIs are the top-line scalars.
	KPIs KPIs `json:"kpis"`
	// ComputedAt is when this snapshot was built; it may be up to TTL stale.
	ComputedAt time.Time `json:"computedAt"`
	// Truncated reports that the scan hit maxScan before exhausting matches,
	// so the counts below are floors rather than totals.
	Truncated bool `json:"truncated"`
}

// Engine computes (and briefly caches) the Overview. Mem and Snapshot are the
// two seams; Logger is required. Now and TTL are optional overrides (defaults:
// time.Now and 10s) used by tests. All exported fields are set at construction
// by the parent admind; the unexported cache state is owned by Overview.
type Engine struct {
	Mem      MemQuerier
	Snapshot func() []LiveSession
	Logger   logr.Logger
	TTL      time.Duration
	Now      func() time.Time

	mu       sync.Mutex
	cached   *Overview
	cachedAt time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) ttl() time.Duration {
	if e.TTL > 0 {
		return e.TTL
	}
	return defaultTTL
}

// Overview returns the dashboard payload, recomputing only when the cache is
// empty or older than the TTL. Concurrent callers serialize on the same
// compute (no thundering herd): the first computes, the rest read the cache.
func (e *Engine) Overview(ctx context.Context) (Overview, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cached != nil && e.now().Sub(e.cachedAt) < e.ttl() {
		return *e.cached, nil
	}
	ov, err := e.compute(ctx)
	if err != nil {
		return Overview{}, err
	}
	e.cached = &ov
	e.cachedAt = e.now()
	return ov, nil
}

func (e *Engine) compute(ctx context.Context) (Overview, error) {
	now := e.now().UTC()
	currentHour := now.Truncate(time.Hour)
	windowStart := currentHour.Add(-time.Duration(buckets-1) * time.Hour)
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	var ov Overview
	ov.ComputedAt = now
	for i := 0; i < buckets; i++ {
		ov.Series24h[i].HourStartUnix = windowStart.Add(time.Duration(i) * time.Hour).Unix()
	}

	// --- live snapshot: by-model / by-class rollups + active count ---
	byModel := map[string]*ModelTokens{}
	byClass := map[string]*ClassTokens{}
	active := 0
	if e.Snapshot != nil {
		for _, s := range e.Snapshot() {
			if s.Active {
				active++
			}
			ov.KPIs.ApprovalsPending += s.PendingApprovals

			// A session WITH per-served-model buckets attributes its tokens to
			// EACH bucket's own Model, not the session's blended one, so a
			// session that fell back or auto-routed shows where the tokens
			// actually went rather than one total under "openrouter/auto". A
			// session with NO buckets attributes its own tokens under its own
			// Model. Exactly one branch runs per session, so nothing is
			// double-counted.
			if len(s.ByModel) > 0 {
				for _, b := range s.ByModel {
					mt := byModel[b.Model]
					if mt == nil {
						mt = &ModelTokens{Model: b.Model}
						byModel[b.Model] = mt
					}
					mt.InputTokens += b.InputTokens
					mt.OutputTokens += b.OutputTokens
					mt.Sessions++
				}
			} else {
				mt := byModel[s.Model]
				if mt == nil {
					mt = &ModelTokens{Model: s.Model}
					byModel[s.Model] = mt
				}
				mt.InputTokens += s.InputTokens
				mt.OutputTokens += s.OutputTokens
				mt.Sessions++
			}

			ct := byClass[s.Class]
			if ct == nil {
				ct = &ClassTokens{Class: s.Class}
				byClass[s.Class] = ct
			}
			ct.InputTokens += s.InputTokens
			ct.OutputTokens += s.OutputTokens
			ct.Sessions++
		}
	}
	ov.KPIs.ActiveSessions = active

	// --- bounded cross-scope scan: time-series + denial/tool-call KPIs ---
	scanned := 0
	// authz_decision is spelled out because pkg/memory/kinds/authzdecision
	// exports no name constant to reference; turn does.
	kinds := []string{turn.KindName, "authz_decision"}
scan:
	for offset := 0; ; offset += pageSize {
		res, err := e.Mem.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: kinds,
			Since: &windowStart, OrderDesc: true, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return Overview{}, fmt.Errorf("overview: cross-scope query: %w", err)
		}
		for _, ent := range res.Entries {
			scanned++
			switch ent.Kind {
			case turn.KindName:
				e.foldTurn(ent, windowStart, startOfToday, &ov)
			case "authz_decision":
				if e.isDenial(ent) {
					ov.KPIs.Denials24h++
				}
			}
			if scanned >= maxScan {
				e.Logger.Info("overview: scan bound hit; aggregation truncated", "maxScan", maxScan)
				ov.Truncated = true
				break scan
			}
		}
		if len(res.Entries) < pageSize {
			break
		}
	}

	ov.ByModel = sortedModels(byModel)
	ov.ByAgentClass = sortedClasses(byClass)
	return ov, nil
}

// foldTurn folds one turn entry into the time-series, tokens-today, and
// tool-call KPI. A malformed turn is logged and skipped — one bad entry must
// not break the whole aggregation.
func (e *Engine) foldTurn(ent memory.Entry, windowStart, startOfToday time.Time, ov *Overview) {
	var turn memory.Turn
	if err := json.Unmarshal(ent.Content, &turn); err != nil {
		e.Logger.Info("overview: malformed turn entry skipped",
			"scope", ent.Scope.ID, "entry", ent.ID, "err", err.Error())
		return
	}
	at := turn.CreatedAt
	if at.IsZero() {
		at = ent.CreatedAt
	}
	at = at.UTC()
	if turn.Usage != nil {
		if idx := int(at.Sub(windowStart) / time.Hour); idx >= 0 && idx < buckets {
			ov.Series24h[idx].InputTokens += turn.Usage.InputTokens
			ov.Series24h[idx].OutputTokens += turn.Usage.OutputTokens
		}
		if !at.Before(startOfToday) {
			ov.KPIs.TokensToday += turn.Usage.InputTokens + turn.Usage.OutputTokens
		}
	}
	for _, blk := range turn.Content {
		if blk.Type == "tool_use" && blk.ToolUse != nil {
			ov.KPIs.ToolCalls24h++
		}
	}
}

// isDenial reports whether an authz_decision entry is a denial. Mirrors the
// audit engine's denial test (denied + read_denied). Malformed entries are
// logged and treated as non-denials.
func (e *Engine) isDenial(ent memory.Entry) bool {
	var c struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(ent.Content, &c); err != nil {
		e.Logger.Info("overview: malformed authz_decision entry skipped",
			"scope", ent.Scope.ID, "entry", ent.ID, "err", err.Error())
		return false
	}
	return c.Outcome == "denied" || c.Outcome == "read_denied"
}

// sortedModels emits rows newest-token-heavy first, name-tie-broken, for a
// stable dashboard order.
func sortedModels(m map[string]*ModelTokens) []ModelTokens {
	out := make([]ModelTokens, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].InputTokens+out[i].OutputTokens, out[j].InputTokens+out[j].OutputTokens
		if ti != tj {
			return ti > tj
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func sortedClasses(m map[string]*ClassTokens) []ClassTokens {
	out := make([]ClassTokens, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].InputTokens+out[i].OutputTokens, out[j].InputTokens+out[j].OutputTokens
		if ti != tj {
			return ti > tj
		}
		return out[i].Class < out[j].Class
	})
	return out
}
