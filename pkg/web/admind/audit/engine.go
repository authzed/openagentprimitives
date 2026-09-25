package audit

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
)

// CrossScopeQuerier is the narrow memory seam the engine needs — *memory.Local
// satisfies it. Kept minimal so tests can stub it.
type CrossScopeQuerier interface {
	QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error)
}

// Engine answers admin audit queries: it pages entries out of memory,
// maps them through the registered mappers, applies event-level filters,
// and aggregates. Post-mapping filtering means a worst case scan of
// maxScan entries per request — bounded, logged, surfaced as Truncated.
type Engine struct {
	Mem          CrossScopeQuerier
	ResolveClass func(ctx context.Context, ns, name string) string // "" when unknown/deleted
	Logger       logr.Logger
}

const (
	pageSize     = 500
	maxScan      = 20000
	defaultLimit = 100
	maxLimit     = 1000
)

// QueryRequest is the wire shape POSTed by the UI (via webd's proxy). Every
// field is a filter; an absent or empty one narrows nothing.
type QueryRequest struct {
	// Since and Until bound the time range; nil means unbounded on that side.
	Since *time.Time `json:"since,omitempty"`
	Until *time.Time `json:"until,omitempty"`
	// EventKinds narrows to these Event.Kind values; empty means all kinds.
	EventKinds       []string `json:"kinds,omitempty"`
	SessionNamespace string   `json:"sessionNamespace,omitempty"`
	SessionName      string   `json:"sessionName,omitempty"`
	AgentClass       string   `json:"agentClass,omitempty"`
	Tool             string   `json:"tool,omitempty"`
	// Actor is matched against the canonical user subject.
	Actor   string `json:"actor,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	// Limit caps the page; 0 takes the engine's own default.
	Limit int `json:"limit,omitempty"`
	// Offset is the page start, counted after filtering.
	Offset int `json:"offset,omitempty"`
}

type QueryResponse struct {
	// Events is this page, newest first; empty means no matches.
	Events []Event `json:"events"`
	// HasMore reports that another page exists past Offset+len(Events).
	HasMore bool `json:"hasMore"`
	// Truncated reports the scan hit its bound before exhausting matches, so
	// HasMore and any counts are floors rather than totals.
	Truncated bool `json:"truncated"`
}

type FacetsResponse struct {
	// Counts maps facet name → value → count, over the facets kind, outcome,
	// tool, actor, agentClass, and session ("ns/name").
	Counts map[string]map[string]int `json:"counts"`
	// Truncated reports the scan hit its bound, so every count is a floor.
	Truncated bool `json:"truncated"`
}

type EntityRow struct {
	// Key is axis-dependent: a class name, "ns/name", a tool, or an actor.
	Key string `json:"key"`
	// Events is this key's total matching events in the window.
	Events int `json:"events"`
	// Denied is the subset whose outcome was a denial.
	Denied int `json:"denied"`

	// The fields below are populated ONLY for the "sessions" axis, by the
	// handler joining each row's "ns/name" Key against the live aggregator
	// snapshot (status + tokens) and the started-by annotation. They are
	// omitted for the agents/tools/users axes, and empty even on the sessions
	// axis when a row's session has been GC'd out of the live aggregator
	// (audit outlives sessions) — the row then carries just events/denied.
	Status           string     `json:"status,omitempty"`           // live phase (Running, Succeeded, …)
	InputTokens      int64      `json:"inputTokens,omitempty"`      // tokens as last reported by the runner
	OutputTokens     int64      `json:"outputTokens,omitempty"`     // tokens as last reported by the runner
	EstimatedCostUSD cost.USD   `json:"estimatedCostUSD,omitempty"` // list-price estimate, never a metered bill
	StartedBy        string     `json:"startedBy,omitempty"`        // canonical subject that started the session
	StartedAt        *time.Time `json:"startedAt,omitempty"`        // session start (status.startedAt, else CR creation time)
}

func (r QueryRequest) matches(ev Event) bool {
	if r.SessionNamespace != "" && ev.SessionNamespace != r.SessionNamespace {
		return false
	}
	if r.SessionName != "" && ev.SessionName != r.SessionName {
		return false
	}
	if r.AgentClass != "" && ev.AgentClass != r.AgentClass {
		return false
	}
	if r.Tool != "" && ev.Tool != r.Tool {
		return false
	}
	if r.Actor != "" && ev.Actor != r.Actor {
		return false
	}
	if r.Outcome != "" && ev.Outcome != r.Outcome {
		return false
	}
	if len(r.EventKinds) > 0 && !slices.Contains(r.EventKinds, ev.Kind) {
		return false
	}
	return true
}

// iterate maps+filters entries newest-first, calling fn per matching event
// until fn returns false or the scan bound is hit. Returns truncated=true
// when the bound was hit with entries remaining.
func (g *Engine) iterate(ctx context.Context, req QueryRequest, fn func(Event) bool) (truncated bool, err error) {
	memKinds := MemoryKindsForEventKinds(req.EventKinds)
	if len(memKinds) == 0 {
		return false, nil
	}
	classCache := map[string]string{}
	scanned := 0
	for offset := 0; ; offset += pageSize {
		res, err := g.Mem.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: memKinds,
			Since: req.Since, Until: req.Until,
			OrderDesc: true, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return false, fmt.Errorf("audit: cross-scope query: %w", err)
		}
		for _, e := range res.Entries {
			scanned++
			m, ok := MapperFor(e.Kind)
			if !ok {
				continue
			}
			evs, err := m.Map(e)
			if err != nil {
				// One malformed entry must not break the whole query —
				// log with locating context and keep going.
				g.Logger.Info("audit: mapper failed; skipping entry",
					"memoryKind", e.Kind, "scope", e.Scope.ID, "entry", e.ID, "err", err.Error())
				continue
			}
			for _, ev := range evs {
				key := ev.SessionNamespace + "/" + ev.SessionName
				cls, ok := classCache[key]
				if !ok {
					cls = g.ResolveClass(ctx, ev.SessionNamespace, ev.SessionName)
					classCache[key] = cls
				}
				ev.AgentClass = cls
				if !req.matches(ev) {
					continue
				}
				if !fn(ev) {
					return false, nil
				}
			}
			if scanned >= maxScan {
				g.Logger.Info("audit: scan bound hit; result truncated", "maxScan", maxScan)
				return true, nil
			}
		}
		if len(res.Entries) < pageSize {
			return false, nil
		}
	}
}

func (g *Engine) Query(ctx context.Context, req QueryRequest) (QueryResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	var resp QueryResponse
	skipped := 0
	truncated, err := g.iterate(ctx, req, func(ev Event) bool {
		if skipped < req.Offset {
			skipped++
			return true
		}
		if len(resp.Events) < limit {
			resp.Events = append(resp.Events, ev)
			return true
		}
		resp.HasMore = true
		return false // limit+1 seen — stop
	})
	if err != nil {
		return QueryResponse{}, err
	}
	resp.Truncated = truncated
	if resp.Events == nil {
		resp.Events = []Event{}
	}
	return resp, nil
}

func (g *Engine) Facets(ctx context.Context, req QueryRequest) (FacetsResponse, error) {
	resp := FacetsResponse{Counts: map[string]map[string]int{
		"kind": {}, "outcome": {}, "tool": {}, "actor": {}, "agentClass": {}, "session": {},
	}}
	bump := func(facet, val string) {
		if val == "" {
			return
		}
		resp.Counts[facet][val]++
	}
	truncated, err := g.iterate(ctx, req, func(ev Event) bool {
		bump("kind", ev.Kind)
		bump("outcome", ev.Outcome)
		bump("tool", ev.Tool)
		bump("actor", ev.Actor)
		bump("agentClass", ev.AgentClass)
		bump("session", ev.SessionNamespace+"/"+ev.SessionName)
		return true
	})
	if err != nil {
		return FacetsResponse{}, err
	}
	resp.Truncated = truncated
	return resp, nil
}

// Entities aggregates per-entity rollups for one axis: "agents",
// "sessions", "tools", or "users".
func (g *Engine) Entities(ctx context.Context, axis string, req QueryRequest) ([]EntityRow, error) {
	keyFn := map[string]func(Event) string{
		"agents":   func(ev Event) string { return ev.AgentClass },
		"sessions": func(ev Event) string { return ev.SessionNamespace + "/" + ev.SessionName },
		"tools":    func(ev Event) string { return ev.Tool },
		"users":    func(ev Event) string { return ev.Actor },
	}[axis]
	if keyFn == nil {
		return nil, fmt.Errorf("audit: unknown entity axis %q (want agents|sessions|tools|users)", axis)
	}
	rows := map[string]*EntityRow{}
	if _, err := g.iterate(ctx, req, func(ev Event) bool {
		k := keyFn(ev)
		if k == "" {
			return true
		}
		r, ok := rows[k]
		if !ok {
			r = &EntityRow{Key: k}
			rows[k] = r
		}
		r.Events++
		if ev.Outcome == "denied" || ev.Outcome == "read_denied" {
			r.Denied++
		}
		return true
	}); err != nil {
		return nil, err
	}
	out := make([]EntityRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Events != out[j].Events {
			return out[i].Events > out[j].Events
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}
