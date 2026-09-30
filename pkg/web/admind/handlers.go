package admind

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
	"github.com/authzed/openagentprimitives/pkg/web/admind/overview"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *Admind) handleSessionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.agg.Snapshot())
}

func (a *Admind) handleToolCalls(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.agg.ToolCalls())
}

func (a *Admind) handleApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.agg.Approvals())
}

type sessionDetail struct {
	SessionState
	RecentEvents []RecentEvent `json:"recentEvents"`
	// Bundles is one entry per st.BundleSessions, enriched with that bundle's
	// live sandbox backend. Omitted (nil) for a session with no resolved
	// bundles, rather than an empty slice, so the UI can tell "no bundles" from
	// "bundles present but none of them read".
	Bundles []BundleSandbox `json:"bundles,omitempty"`
}

func (a *Admind) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	st, recent, ok := a.agg.Get(ns, name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("session %s/%s not tracked", ns, name))
		return
	}
	var bundles []BundleSandbox
	if len(st.BundleSessions) > 0 {
		bundles = a.bundleSandboxes(r.Context(), ns, st.BundleSessions)
	}
	writeJSON(w, http.StatusOK, sessionDetail{SessionState: st, RecentEvents: recent, Bundles: bundles})
}

// BundleSandbox surfaces one AgentSession bundle's sandbox backend. The
// aggregator's SessionState carries only what AgentSession.status itself
// records per bundle (Name/SpiceboxSessionName/AgentIdentity) — sandbox
// kind/ref/prewarmed/phase live only on that bundle's own SpiceboxSession
// CR, one level down, so bundleSandboxes fetches it live per bundle. This is
// intentionally per-bundle rather than a single session-level value: a
// session's bundles can legitimately resolve to different sandbox kinds, and
// collapsing them would manufacture a second, lossy source of truth for a
// fact that can genuinely differ bundle-to-bundle.
type BundleSandbox struct {
	Name                string `json:"name"`                    // the tool bundle's name
	SpiceboxSessionName string `json:"spiceboxSessionName"`     // the SpiceboxSession the bundle resolved to
	AgentIdentity       string `json:"agentIdentity,omitempty"` // the identity the bundle's tools run as
	// SandboxKind/SandboxRef/Prewarmed are empty/false when the bundle's
	// SpiceboxSession has not yet been assigned a sandbox, or could not be
	// read (see bundleSandboxes).
	SandboxKind string `json:"sandboxKind,omitempty"`
	SandboxRef  string `json:"sandboxRef,omitempty"`
	// Prewarmed is carried through only when true — the same "don't show a
	// scary false" rule sandboxCell applies in cmd/oap/internal/sandboxcmd: a
	// cold sandbox on a backend that cannot pre-warm at all should not read as
	// a warning.
	Prewarmed bool `json:"prewarmed,omitempty"`
	// Phase/Reason are derived from the bundle's SpiceboxSession conditions —
	// Failed / Ready / NotReady / Unknown — mirroring the READY column `oap
	// sandbox list` already computes from the same Ready condition.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// WorkspaceMode is the bundle's SpiceboxSession.spec.workspace.mode ("shared"
	// when the AgentSession controller resolved a workspace StorageClass and
	// wired an RWX claim across bundles, "isolated" — pod-local /work only —
	// otherwise). A sibling of SandboxKind: same live Get, same "why can't my
	// tools see each other's files" diagnostic value. Empty when the bundle's
	// SpiceboxSession could not be read (see bundleSandboxes).
	WorkspaceMode string `json:"workspaceMode,omitempty"`
}

// bundleSandboxes fetches each bundle's SpiceboxSession, live, at
// session-detail request time — the one new K8s read this task adds. It is
// bounded by len(bundles) (a session's bundle count, not the cluster's
// session count) and only runs when a human opens one session's detail page,
// mirroring sessionStartedBy's existing live-Get-on-detail-view pattern
// above. Best-effort per bundle: a SpiceboxSession already gone (session
// ended, GC'd) yields blank sandbox fields rather than failing the whole
// response; a non-NotFound read failure is logged and also degrades to blank
// fields, matching sessionStartedBy's error handling.
func (a *Admind) bundleSandboxes(ctx context.Context, ns string, bundles []spiceboxv1alpha1.ResolvedBundle) []BundleSandbox {
	out := make([]BundleSandbox, 0, len(bundles))
	for _, b := range bundles {
		bs := BundleSandbox{Name: b.Name, SpiceboxSessionName: b.SpiceboxSessionName, AgentIdentity: b.AgentIdentity}
		var sbx spiceboxv1alpha1.SpiceboxSession
		err := a.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: b.SpiceboxSessionName}, &sbx)
		switch {
		case err == nil:
			if h := sbx.Status.Sandbox; h != nil {
				bs.SandboxKind, bs.SandboxRef, bs.Prewarmed = h.Kind, h.Ref, h.Prewarmed
			}
			bs.Phase, bs.Reason = sandboxSessionPhase(sbx.Status.Conditions)
			bs.WorkspaceMode = string(sbx.Spec.Workspace.Mode)
		case apierrors.IsNotFound(err):
			// Normal: the bundle's SpiceboxSession has not been created yet, or
			// was already GC'd after the session ended. Phase is stamped Gone
			// rather than left blank, which sandboxSessionPhase would otherwise
			// render as "Unknown" ("freshly created, not yet reconciled") — the
			// opposite of what actually happened, and the shape a reader would
			// wait on rather than move past.
			bs.Phase = "Gone"
		default:
			a.cfg.Logger.Info("admind: bundle SpiceboxSession lookup failed; sandbox omitted",
				"namespace", ns, "spiceboxSession", b.SpiceboxSessionName, "err", err.Error())
		}
		out = append(out, bs)
	}
	return out
}

// sandboxSessionPhase derives a coarse phase label + reason from a
// SpiceboxSession's conditions: Failed wins (terminal), else Ready/NotReady
// from the Ready condition, else "Unknown" when neither condition has been
// set yet (freshly created, not yet reconciled).
func sandboxSessionPhase(conds []metav1.Condition) (phase, reason string) {
	if c := meta.FindStatusCondition(conds, spiceboxv1alpha1.SpiceboxSessionConditionFailed); c != nil && c.Status == metav1.ConditionTrue {
		return "Failed", c.Reason
	}
	if c := meta.FindStatusCondition(conds, spiceboxv1alpha1.SpiceboxSessionConditionReady); c != nil {
		if c.Status == metav1.ConditionTrue {
			return "Ready", c.Reason
		}
		return "NotReady", c.Reason
	}
	return "Unknown", ""
}

// maxSessionLogEntries caps the full-logs payload. The session transcript is
// the append-only turn Kind; a session with more turns than this is truncated
// to the most recent maxSessionLogEntries (returned oldest-first) with
// truncated=true so the UI can surface "showing the latest N".
const maxSessionLogEntries = 2000

// sessionLogEntry is one row of the full-logs view: the turn's identity, a
// decoded flat content summary (backward-compatible), the STRUCTURED content
// blocks the UI renders as color-coded roles / JSON-view tool calls, and the
// subject that authored it (actor).
type sessionLogEntry struct {
	Kind      string    `json:"kind"`           // the memory kind this row was read from
	ID        string    `json:"id"`             // the memory entry's id
	Index     int       `json:"index"`          // the turn's position in the transcript
	Role      string    `json:"role,omitempty"` // who spoke: user / assistant / tool / system
	CreatedAt time.Time `json:"createdAt"`      // when the turn was written
	// Content is the flattened one-string summary (text + "→ tool(input)" +
	// "← result" lines). Retained verbatim for backward-compatibility with the
	// pre-structured UI; new UI reads Blocks.
	Content string `json:"content"`
	// Actor is the subject that authored this turn. It is the turn's persisted
	// per-turn author when present; otherwise, for a legacy author-less
	// human turn, it falls back to the session's started-by canonical subject
	// with ActorInferred=true. Empty for agent/tool/system turns and for
	// sessions with no started-by annotation.
	Actor string `json:"actor,omitempty"`
	// ActorInferred is true only when Actor was filled from the session-level
	// started-by fallback (a legacy turn written before per-turn authorship),
	// not from the turn's own author. The UI labels these "session initiator".
	ActorInferred bool `json:"actorInferred,omitempty"`
	// Blocks is the structured, render-ready decomposition of the turn's
	// content: typed text / tool_use (name + raw JSON input) / tool_result
	// (raw output + isError) blocks. nil for a malformed turn (Content carries
	// the raw payload instead).
	Blocks []logBlock `json:"blocks,omitempty"`
}

// logBlock is one structured content block for the Full Logs viewer. Only the
// fields relevant to Type are populated; tool input/output stay as raw JSON
// (json.RawMessage) so the UI renders them in a JSON view instead of a
// pre-stringified blob.
type logBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"
	// Text is set for Type=="text".
	Text string `json:"text,omitempty"`
	// Name is the tool name for Type=="tool_use".
	Name string `json:"name,omitempty"`
	// Input is the structured tool input for Type=="tool_use": the tool's raw
	// JSON arguments, unmodified.
	Input json.RawMessage `json:"input,omitempty"`
	// Content is the tool output for Type=="tool_result": the raw result as
	// JSON when it parses, otherwise a JSON string. Always valid JSON.
	Content json.RawMessage `json:"content,omitempty"`
	// IsError flags a failed tool call for Type=="tool_result".
	IsError bool `json:"isError,omitempty"`
}

type sessionLogsResponse struct {
	Entries []sessionLogEntry `json:"entries"` // oldest-first
	// Truncated is true when older turns were dropped to fit the entry cap.
	Truncated bool `json:"truncated"`
}

// handleSessionLogs returns the session's FULL transcript from the memory
// facade — richer than the aggregator's capped RecentEvents ring. It reads the
// append-only turn Kind for the session scope, orders it chronologically, and
// caps at maxSessionLogEntries (keeping the most recent, returned oldest-first).
// An untracked or empty session is a normal 200 with an empty list, never a 404
// — audit/transcript surfaces must not conflate "no turns yet" with "gone".
func (a *Admind) handleSessionLogs(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	// The route is gated on platform#view_sessions (admind.go), so an admin who
	// reaches here is authorized to read any session's transcript; mint a system
	// approval to clear the memory read door (admind is a trusted platform reader,
	// like webd).
	ctx := memory.WithSystemApproval(r.Context(), "operator:admind")
	// Newest-first with Limit=cap+1 both takes the recent tail and tells us
	// more existed than we returned (truncation).
	res, err := a.cfg.Mem.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{turn.KindName},
		OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
		Limit:   maxSessionLogEntries + 1,
	})
	if err != nil {
		a.cfg.Logger.Info("admind: session logs query failed", "session", scope.ID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "session logs query failed: "+err.Error())
		return
	}
	entries := res.Entries
	truncated := false
	if len(entries) > maxSessionLogEntries {
		truncated = true
		entries = entries[:maxSessionLogEntries]
	}
	// Present chronologically (oldest-first) and deterministically regardless of
	// backend ordering / equal timestamps.
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})
	// The session's started-by is only the fallback for legacy author-less
	// turns (looked up once). Best-effort: a gone/annotation-less session
	// yields "".
	startedBy := a.sessionStartedBy(r.Context(), ns, name)
	rows := make([]sessionLogEntry, 0, len(entries))
	for _, e := range entries {
		row := sessionLogEntry{Kind: e.Kind, ID: e.ID, CreatedAt: e.CreatedAt}
		if tn, derr := turn.EntryToTurn(e); derr == nil {
			row.Index = tn.Index
			row.Role = tn.Role
			row.Content = summarizeTurnContent(tn.Content)
			row.Blocks = turnContentBlocks(tn.Content)
			switch {
			case tn.Author != "":
				// True per-turn author (multiplayer-correct).
				row.Actor = tn.Author.String()
			case startedBy != "" && isRequesterRole(tn.Role):
				// Legacy author-less human turn: fall back to the session
				// initiator, flagged so the UI labels it as inferred.
				row.Actor = startedBy
				row.ActorInferred = true
			}
		} else {
			// A malformed turn must not sink the whole log — surface the raw
			// payload and log so an operator can locate the bad entry.
			a.cfg.Logger.Info("admind: session log decode failed; returning raw content",
				"session", scope.ID, "entry", e.ID, "err", derr.Error())
			row.Content = string(e.Content)
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, sessionLogsResponse{Entries: rows, Truncated: truncated})
}

// summarizeTurnContent renders a turn's content blocks into one render-ready
// string: user/assistant text verbatim, tool_use as "→ <name>(<input>)", and
// tool_result as "← <content>" (prefixed "[error]" when the tool failed).
// Unknown block types are surfaced by type so nothing silently disappears.
func summarizeTurnContent(blocks []memory.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			if b.ToolUse != nil {
				parts = append(parts, fmt.Sprintf("→ %s(%s)", b.ToolUse.Name, string(b.ToolUse.Input)))
			}
		case "tool_result":
			if b.ToolResult != nil {
				prefix := "← "
				if b.ToolResult.IsError {
					prefix = "← [error] "
				}
				parts = append(parts, prefix+b.ToolResult.Content)
			}
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// turnContentBlocks walks the same content blocks as summarizeTurnContent but
// preserves structure for the UI: text → {text}; tool_use → {name, input} with
// the tool's raw JSON arguments intact; tool_result → {content, isError} with
// the raw output as JSON (or a JSON string). Unknown block types are surfaced
// by type so nothing silently disappears. Returns nil for an empty turn.
func turnContentBlocks(blocks []memory.ContentBlock) []logBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]logBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, logBlock{Type: "text", Text: b.Text})
		case "tool_use":
			if b.ToolUse == nil {
				continue
			}
			lb := logBlock{Type: "tool_use", Name: b.ToolUse.Name}
			if len(b.ToolUse.Input) > 0 {
				lb.Input = rawJSONOrString(string(b.ToolUse.Input))
			}
			out = append(out, lb)
		case "tool_result":
			if b.ToolResult == nil {
				continue
			}
			out = append(out, logBlock{
				Type:    "tool_result",
				Content: rawJSONOrString(b.ToolResult.Content),
				IsError: b.ToolResult.IsError,
			})
		default:
			out = append(out, logBlock{Type: b.Type})
		}
	}
	return out
}

// rawJSONOrString returns s as raw JSON when it already parses as JSON,
// otherwise as a JSON-encoded string. Either way the result is always valid
// JSON, so the UI can attempt a JSON-view render and fall back to text.
func rawJSONOrString(s string) json.RawMessage {
	if s != "" && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	// Marshaling a string never fails; the result is a quoted JSON string.
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

// isRequesterRole reports whether a turn role is a human-authored request
// (user/inbox) — the turns the session's started-by subject is attributed to.
// Assistant, system_note, and tool turns are the agent's own output.
func isRequesterRole(role string) bool {
	return role == "user" || role == "inbox"
}

// sessionStartedBy returns the session's creating-user canonical subject from
// the started-by annotation channelsd stamps at creation. Best-effort: a
// gone session (audit outlives sessions), a kubectl-driven session with no
// annotation, or a Get failure all yield "" — a non-NotFound error is logged,
// never fatal, so the logs view still renders.
func (a *Admind) sessionStartedBy(ctx context.Context, ns, name string) string {
	var s spiceboxv1alpha1.AgentSession
	if err := a.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
		if !apierrors.IsNotFound(err) {
			a.cfg.Logger.Info("admind: session started-by lookup failed",
				"session", ns+"/"+name, "err", err.Error())
		}
		return ""
	}
	return spiceboxv1alpha1.StartedBySubject(&s).String()
}

func (a *Admind) handleSessionKill(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	obj := &spiceboxv1alpha1.AgentSession{}
	obj.Namespace, obj.Name = ns, name
	subject := r.Header.Get("X-Admin-Subject")
	if err := a.cfg.K8s.Delete(r.Context(), obj); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "session already gone")
			return
		}
		a.cfg.Logger.Info("admind: session kill failed", "session", ns+"/"+name, "subject", subject, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	a.cfg.Logger.Info("admind: session killed by admin", "session", ns+"/"+name, "subject", subject)
	w.WriteHeader(http.StatusNoContent)
}

const heartbeatInterval = 5 * time.Second

func (a *Admind) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported by server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeFrame := func(fr SSEFrame) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", fr.Event, fr.Data)
		fl.Flush()
	}

	ch, cancel := a.agg.Subscribe()
	defer cancel()

	// Initial snapshot so the client never renders from nothing.
	for _, st := range a.agg.Snapshot() {
		data, err := json.Marshal(st)
		if err != nil {
			a.cfg.Logger.Info("admind: marshal snapshot frame", "err", err.Error())
			continue
		}
		writeFrame(SSEFrame{Event: "session", Data: data})
	}

	hb := time.NewTicker(heartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case fr := <-ch:
			writeFrame(fr)
		case <-hb.C:
			writeFrame(SSEFrame{Event: "heartbeat", Data: []byte(`{}`)})
		}
	}
}

func (a *Admind) handleAuditQuery(w http.ResponseWriter, r *http.Request) {
	var req audit.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	resp, err := a.engine.Query(r.Context(), req)
	if err != nil {
		a.cfg.Logger.Info("admind: audit query failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "audit query failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Admind) handleAuditFacets(w http.ResponseWriter, r *http.Request) {
	var req audit.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	resp, err := a.engine.Facets(r.Context(), req)
	if err != nil {
		a.cfg.Logger.Info("admind: audit facets failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "audit facets failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// budgetInfo is the Overview's spend panel. EstimatedCostUSD is derived from
// the per-model token rollup via the cost price map; Estimated is always true
// (list-price estimates, never a metered bill). TokenCeiling is nil until the
// cluster-budget wiring lands — the UI then shows raw spend, not a percentage.
type budgetInfo struct {
	TokensSpent      int64    `json:"tokensSpent"`
	TokenCeiling     *int64   `json:"tokenCeiling,omitempty"`
	EstimatedCostUSD cost.USD `json:"estimatedCostUSD"`
	Estimated        bool     `json:"estimated"`
}

// overviewResponse is the Overview payload plus the cost-derived budget. The
// embedded overview.Overview promotes its fields (byModel, byAgentClass,
// series24h, kpis, computedAt, truncated) to the top level of the JSON object.
type overviewResponse struct {
	overview.Overview
	Budget budgetInfo `json:"budget"`
}

func (a *Admind) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := a.overview.Overview(r.Context())
	if err != nil {
		a.cfg.Logger.Info("admind: overview compute failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "overview failed: "+err.Error())
		return
	}
	prices := a.effectivePrices(r.Context())
	var estCost cost.USD
	var tokensSpent int64
	for _, m := range ov.ByModel {
		// m.Model is a uniform "<provider>/<model>" display id (a bucket's own
		// Model, or the session's blended one); Estimate needs the bare id its
		// price tables are keyed by.
		estCost += prices.Estimate(bareModel(m.Model), m.InputTokens, m.OutputTokens)
		tokensSpent += m.InputTokens + m.OutputTokens
	}
	// Inner interactive-toolkit spend (e.g. a passthrough `claude` sub-run) is
	// not token-based, so it never appears in ov.ByModel. Add it over the SAME
	// session scope that produced ov.ByModel — the overview engine's own live
	// input is liveSessionSnapshot(a.agg), so a.agg.Snapshot() is exactly that
	// set (no double-count). Without this the headline spend understates a
	// sub-agent session by the full tool cost (see the codebot case).
	for _, s := range a.agg.Snapshot() {
		estCost += toolCostUSD(s.ByTool)
	}
	writeJSON(w, http.StatusOK, overviewResponse{
		Overview: ov,
		Budget:   budgetInfo{TokensSpent: tokensSpent, EstimatedCostUSD: estCost, Estimated: true},
	})
}

func (a *Admind) handleHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := a.health.Snapshot(r.Context())
	if err != nil {
		a.cfg.Logger.Info("admind: health snapshot failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "health snapshot failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleConfigResource dispatches GET /admin/v1/config/{resource} onto the
// registered Projector for that URL slug. Unknown slug → 404 (T29 registers
// the real projectors; an unregistered cluster correctly 404s here). The
// cached client a.cfg.K8s is correct: every config CRD is operator-watched.
func (a *Admind) handleConfigResource(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	p, ok := config.Get(resource)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown config resource: "+resource)
		return
	}
	rows, err := p.List(r.Context(), a.cfg.K8s)
	if err != nil {
		a.cfg.Logger.Info("admind: config list failed", "resource", resource, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "config list failed: "+err.Error())
		return
	}
	if rows == nil {
		// Some projectors build their slice with `var rows []ResourceRow` and
		// never append when the CRD list is empty, leaving it nil. A nil slice
		// marshals to JSON `null`, and the admin UI iterates the response
		// directly (Array.prototype methods) — `null` throws client-side. Coerce
		// here so every resource, present and future, is guaranteed `[]`.
		rows = []config.ResourceRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleConfigDetail dispatches GET /admin/v1/config/{resource}/{id...} onto
// the registered DetailProjector for that URL slug. The trailing {id...}
// wildcard captures the resource id: "<ns>/<name>" for a namespaced resource,
// a bare "<name>" for a cluster-scoped one. Unknown slug → 404; a CR that does
// not exist → 404 (the projector returns a nil detail); a read failure → 500.
func (a *Admind) handleConfigDetail(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	id := r.PathValue("id")
	p, ok := config.GetDetail(resource)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown config resource: "+resource)
		return
	}
	ns, name := splitConfigID(id)
	if name == "" {
		writeJSONError(w, http.StatusNotFound, "missing resource id")
		return
	}
	detail, err := p.Detail(r.Context(), a.cfg.K8s, ns, name)
	if err != nil {
		a.cfg.Logger.Info("admind: config detail failed", "resource", resource, "id", id, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "config detail failed: "+err.Error())
		return
	}
	if detail == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", resource, id))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// splitConfigID parses the {id...} detail path segment. A namespaced id is
// "<ns>/<name>"; a cluster-scoped id is a bare "<name>" (ns == ""). Only the
// FIRST slash is a separator — a name never contains one, but a namespace is a
// single DNS label so this is unambiguous.
func splitConfigID(id string) (ns, name string) {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

func (a *Admind) handleAuditEntities(w http.ResponseWriter, r *http.Request) {
	axis := r.PathValue("axis")
	switch axis {
	case "agents", "sessions", "tools", "users":
		// valid
	default:
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown entity axis %q (want agents|sessions|tools|users)", axis))
		return
	}
	rows, err := a.engine.Entities(r.Context(), axis, audit.QueryRequest{})
	if err != nil {
		a.cfg.Logger.Info("admind: audit entities failed", "axis", axis, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "audit entities failed: "+err.Error())
		return
	}
	if axis == "sessions" {
		a.enrichSessionRows(r, rows)
	}
	writeJSON(w, http.StatusOK, rows)
}

// enrichSessionRows joins each "sessions"-axis EntityRow (keyed "ns/name")
// against the live aggregator snapshot (status + tokens + estimated cost) and
// the started-by annotation. A session present in the audit scan but absent
// from the live aggregator (GC'd — audit outlives sessions) simply keeps its
// bare events/denied counts: no status, no tokens. Starter lookup is
// best-effort — a List error degrades to no startedBy (logged), never failing
// the request.
func (a *Admind) enrichSessionRows(r *http.Request, rows []audit.EntityRow) {
	states := map[string]SessionState{}
	for _, s := range a.agg.Snapshot() {
		states[s.Namespace+"/"+s.Name] = s
	}
	meta, err := a.sessionMeta(r.Context())
	if err != nil {
		a.cfg.Logger.Info("admind: audit sessions meta lookup failed; startedBy/startedAt omitted",
			"err", err.Error())
	}
	prices := a.effectivePrices(r.Context())
	for i := range rows {
		key := rows[i].Key
		if st, ok := states[key]; ok {
			rows[i].Status = st.Phase
			rows[i].InputTokens = st.InputTokens
			rows[i].OutputTokens = st.OutputTokens
			// st.Model is the uniform "<provider>/<model>" display id; strip the
			// provider prefix before the bare-keyed price lookup.
			rows[i].EstimatedCostUSD = prices.Estimate(bareModel(st.Model), st.InputTokens, st.OutputTokens) + toolCostUSD(st.ByTool)
		}
		if m, ok := meta[key]; ok {
			rows[i].StartedBy = m.StartedBy
			rows[i].StartedAt = m.StartedAt
		}
	}
}
