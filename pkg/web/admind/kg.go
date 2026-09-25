package admind

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// kgDefaultLimit bounds search/related result counts when ?limit= is absent or
// unparseable.
const kgDefaultLimit = 50

// kgUnavailable is the degraded payload returned when no KGProvider is wired
// (graphiti / KG off). It is a 200, NEVER a 500 — the Knowledge panel renders a
// clean "not configured" state from available:false.
type kgUnavailable struct {
	// Available is always false here; it is what the panel branches on.
	Available bool `json:"available"`
	// Note is operator-facing copy naming the missing configuration.
	Note string `json:"note"`
}

// kgFactsResponse / kgEntityResponse / kgEntitiesResponse / kgCommunitiesResponse
// are the per-action success envelopes. All carry available:true; a provider
// error mid-request surfaces in Error on a 200 so the panel degrades rather
// than blanking. Slices are never nil, so the frontend can map unconditionally.
type kgFactsResponse struct {
	// Available is always true on this envelope — see kgUnavailable for false.
	Available bool `json:"available"`
	// Facts is empty, never nil, when the search matched nothing.
	Facts []memory.KGFact `json:"facts"`
	// Error is a provider failure on an otherwise-200 response; empty on success.
	Error string `json:"error,omitempty"`
}

type kgEntityResponse struct {
	// Available is always true on this envelope.
	Available bool `json:"available"`
	// Entity is nil for BOTH not-found and a provider error; Error distinguishes.
	Entity *memory.KGEntity `json:"entity"`
	// Error is a provider failure on an otherwise-200 response.
	Error string `json:"error,omitempty"`
}

type kgEntitiesResponse struct {
	// Available is always true on this envelope.
	Available bool `json:"available"`
	// Entities is empty, never nil, when nothing related was found.
	Entities []memory.KGEntity `json:"entities"`
	// Error is a provider failure on an otherwise-200 response.
	Error string `json:"error,omitempty"`
}

type kgCommunitiesResponse struct {
	// Available is always true on this envelope.
	Available bool `json:"available"`
	// Communities is empty, never nil, when the graph has no clusters yet.
	Communities []memory.KGCommunity `json:"communities"`
	// Error is a provider failure on an otherwise-200 response.
	Error string `json:"error,omitempty"`
}

// handleKG serves GET /admin/v1/kg/{action} — a read-only proxy to the
// operator's KGProvider. ADMIN-ONLY (the route is gated on view_audit). When no
// KGProvider is configured the panel degrades (available:false, 200). A provider
// error is logged AND surfaced to the caller as available:true+error rather than
// a 500: a degraded Knowledge panel is more useful than a blank error page.
func (a *Admind) handleKG(w http.ResponseWriter, r *http.Request) {
	if a.cfg.KG == nil {
		writeJSON(w, http.StatusOK, kgUnavailable{
			Available: false,
			Note:      "knowledge graph not configured (set --graphiti-endpoint)",
		})
		return
	}

	ctx := r.Context()
	q := r.URL.Query()
	action := r.PathValue("action")

	switch action {
	case "search":
		facts, err := a.cfg.KG.SearchFacts(ctx, strings.TrimSpace(q.Get("q")), kgLimit(q.Get("limit")))
		a.writeKGFacts(w, "search", facts, err)

	case "entity":
		entity, err := a.cfg.KG.GetEntity(ctx, strings.TrimSpace(q.Get("uuid")))
		resp := kgEntityResponse{Available: true, Entity: entity}
		if err != nil {
			a.cfg.Logger.Info("admind: kg entity failed", "err", err.Error())
			resp.Entity = nil // nil/not-found and errors both render as a null entity
			resp.Error = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)

	case "facts":
		facts, err := a.cfg.KG.EntityFacts(ctx, strings.TrimSpace(q.Get("uuid")))
		a.writeKGFacts(w, "facts", facts, err)

	case "related":
		entities, err := a.cfg.KG.RelatedEntities(ctx, strings.TrimSpace(q.Get("uuid")), kgLimit(q.Get("limit")))
		resp := kgEntitiesResponse{Available: true, Entities: entities}
		if resp.Entities == nil {
			resp.Entities = []memory.KGEntity{}
		}
		if err != nil {
			a.cfg.Logger.Info("admind: kg related failed", "err", err.Error())
			resp.Entities = []memory.KGEntity{}
			resp.Error = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)

	case "communities":
		communities, err := a.cfg.KG.Communities(ctx, strings.TrimSpace(q.Get("group")))
		resp := kgCommunitiesResponse{Available: true, Communities: communities}
		if resp.Communities == nil {
			resp.Communities = []memory.KGCommunity{}
		}
		if err != nil {
			a.cfg.Logger.Info("admind: kg communities failed", "err", err.Error())
			resp.Communities = []memory.KGCommunity{}
			resp.Error = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		writeJSONError(w, http.StatusNotFound, "unknown kg action: "+action)
	}
}

// writeKGFacts is the shared success/degrade writer for the two fact-returning
// actions (search, facts). Facts is always a non-nil slice; a provider error is
// logged and surfaced in Error rather than turning into a 500.
func (a *Admind) writeKGFacts(w http.ResponseWriter, action string, facts []memory.KGFact, err error) {
	resp := kgFactsResponse{Available: true, Facts: facts}
	if resp.Facts == nil {
		resp.Facts = []memory.KGFact{}
	}
	if err != nil {
		a.cfg.Logger.Info("admind: kg "+action+" failed", "err", err.Error())
		resp.Facts = []memory.KGFact{}
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// kgLimit parses ?limit=, falling back to kgDefaultLimit when absent, malformed,
// or non-positive.
func kgLimit(raw string) int {
	if raw == "" {
		return kgDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return kgDefaultLimit
	}
	return n
}
