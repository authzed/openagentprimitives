package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// maxRequestBody bounds a chat POST body — a chat message, interrupt or
// decision payload is always small text, never a bulk upload.
const maxRequestBody = 64 << 10

// maxWSClientFrame bounds a single frame read from the chat websocket client.
// The chat ws is push-only — the browser only ever sends close/pong control
// frames, never data — so a tiny bound is ample and stops a misbehaving or
// hostile client from streaming an unbounded frame into the read buffer.
const maxWSClientFrame = 1024

// pathSessionKey builds the sessionKey a session-scoped route's {ns}/{name}
// URL wildcards name. Every handler here reads the session from the URL, never
// from a body field: the URL is what the route's Authorize closure (chat.go)
// gated, so a body naming a DIFFERENT session is unrepresentable rather than
// merely unused.
func pathSessionKey(r *http.Request) sessionKey {
	return sessionKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}
}

// --- GET /sessions/api/{ns}/{name}/detail ----------------------------------

// modelInfo names the LLM a session runs on. Both fields are empty when
// neither effectiveSettings nor the AgentClass spec resolved one.
type modelInfo struct {
	// Provider is the llm.Provider registry key (anthropic, openai, …).
	Provider string `json:"provider"`
	// Name is the provider's own model id, not a display label.
	Name string `json:"name"`
}

// sessionDetail is the GET .../detail info-panel payload: the same model +
// budget fields Slack's "Show settings" surface shows, plus owner, created and
// phase. Budget fields are zero/empty when unresolved and unset alike — the
// panel cannot distinguish the two.
type sessionDetail struct {
	// SessionID is the AgentSession name; its namespace is the request's.
	SessionID string `json:"sessionId"`
	// AgentClass is the class this conversation runs, in the same namespace.
	AgentClass string `json:"agentClass"`
	// Model is the resolved LLM, or zero when nothing resolved one.
	Model modelInfo `json:"model"`
	// MaxTokens is the session token budget; 0 means unset or unresolved.
	MaxTokens int64 `json:"maxTokens"`
	// MaxTurns is the session turn budget; 0 means unset or unresolved.
	MaxTurns int32 `json:"maxTurns"`
	// MaxDuration is the wall-clock budget, already rendered ("none" if unset).
	MaxDuration string `json:"maxDuration"`
	// Owner is the conversation's canonical STARTER, not the reading subject.
	Owner string `json:"owner"`
	// CreatedAt is the AgentSession's creation timestamp.
	CreatedAt time.Time `json:"createdAt"`
	// Phase is the AgentSession's lifecycle phase; empty before first status.
	Phase string `json:"phase"`

	// Notices are the user-facing statements this session's status implies —
	// "starting up", "waiting for capacity", "paused, retry to continue".
	//
	// Sent because channelsd, which publishes these to a channel, deliberately
	// skips every CLIENT-HOSTED session (clientHostedHere) and `browser` is one:
	// webd surfaces that transport, not channelsd. Without this a webchat user
	// saw none of it — the transcript stopped and the explanation sat in a
	// condition nobody rendered.
	Notices []sessionnotice.Notice `json:"notices,omitempty"`
}

// sessionDetailHandler returns the session-info panel fields for the URL's
// session. Authorized BEFORE the k8s read, so a subject with no standing gets
// 404/403/503 and never the session's settings.
//
// authorizeRead, not authorize: the panel READS durable state, so it must keep
// working for a conversation with no live entry — a finished one, or any one
// after a webd restart — without re-attaching it.
func sessionDetailHandler(d Deps, reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			writeError(d.Logger(), w, http.StatusUnauthorized, "not authenticated")
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			writeError(d.Logger(), w, http.StatusBadRequest, "session namespace and name are required")
			return
		}
		owner, err := reg.authorizeRead(r.Context(), key, subject)
		if err != nil {
			status, msg := mapSubmitError(d.Logger(), err)
			writeError(d.Logger(), w, status, msg)
			return
		}

		var sess spiceboxv1alpha1.AgentSession
		if err := d.K8s().Get(r.Context(), client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &sess); err != nil {
			d.Logger().Info("chat: get AgentSession for detail failed", "session", key.String(), "err", err.Error())
			writeError(d.Logger(), w, http.StatusNotFound, "chat session not found")
			return
		}

		detail := sessionDetail{
			SessionID:  key.Name,
			AgentClass: sess.Spec.Class,
			Owner:      owner,
			CreatedAt:  sess.CreationTimestamp.Time,
			Phase:      sess.Status.Phase,
			Notices:    sessionnotice.Derive(&sess),
		}
		fillModelAndBudget(r.Context(), d, &sess, &detail)
		writeJSON(d.Logger(), w, http.StatusOK, detail)
	})
}

// fillModelAndBudget populates detail's model + budget, preferring the
// session's resolved effectiveSettings and falling back to the AgentClass
// spec. Best-effort: a failed fetch is logged, not fatal, and the panel still
// renders whatever resolved. The fallback Get uses sess.Namespace because
// AgentClass is namespace-scoped alongside the session referencing it.
func fillModelAndBudget(ctx context.Context, d Deps, sess *spiceboxv1alpha1.AgentSession, detail *sessionDetail) {
	if eff := sess.Status.EffectiveSettings; eff != nil {
		detail.Model = modelInfo{Provider: eff.Model.Provider, Name: eff.Model.Name}
		detail.MaxTokens = eff.Budget.MaxTokens
		detail.MaxTurns = eff.Budget.MaxTurns
		detail.MaxDuration = durationString(eff.Budget.MaxDuration.Duration)
	}
	if detail.Model.Name != "" && detail.MaxTurns != 0 {
		return // fully resolved from effectiveSettings
	}
	// Effective settings not stamped yet (or partial) — fall back to the class
	// spec. Both spec.model and spec.budget are optional pointers (a class may
	// inherit them from a higher tier), so guard each before dereferencing.
	var ac spiceboxv1alpha1.AgentClass
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err != nil {
		d.Logger().Info("chat: get AgentClass for detail fallback failed", "class", sess.Spec.Class, "err", err.Error())
		return
	}
	if detail.Model.Name == "" && ac.Spec.Model != nil {
		detail.Model = modelInfo{Provider: ac.Spec.Model.Provider, Name: ac.Spec.Model.Name}
	}
	if b := ac.Spec.Budget; b != nil {
		if detail.MaxTurns == 0 {
			detail.MaxTurns = b.MaxTurns
		}
		if detail.MaxTokens == 0 {
			detail.MaxTokens = b.MaxTokens
		}
		if detail.MaxDuration == "" {
			detail.MaxDuration = durationString(b.MaxDuration.Duration)
		}
	}
}

// durationString renders a budget duration, collapsing zero to "none" (matches
// the Slack settings modal's rendering of an unset max-duration).
func durationString(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return d.String()
}

// --- GET /sessions/api/{ns}/{name}/messages --------------------------------

type messagesResponse struct {
	// Timeline is the conversation's ordered history; empty means no turns yet,
	// never a failed read (a failure is a non-200 with an errorResponse).
	Timeline []timelineEntry `json:"timeline"`
}

// sessionMessagesHandler replays a chat session's prior turns so a resumed
// conversation renders its history, reading operator memory through webd's
// read-only token. It never auto-creates a session, and with no memory access
// it fails closed with a logged 501 rather than a silent empty transcript.
//
// authorizeRead, not authorize: the transcript is durable and outlives both
// the in-memory entry and the process, so a FINISHED conversation — listed as
// ended in the sidebar, its entry already dropped by the phase watcher — must
// still replay rather than 404 into a blank panel.
func sessionMessagesHandler(d Deps, reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			writeError(d.Logger(), w, http.StatusUnauthorized, "not authenticated")
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			writeError(d.Logger(), w, http.StatusBadRequest, "session namespace and name are required")
			return
		}
		if _, err := reg.authorizeRead(r.Context(), key, subject); err != nil {
			status, msg := mapSubmitError(d.Logger(), err)
			writeError(d.Logger(), w, status, msg)
			return
		}
		items, err := readTranscript(r.Context(), d, key.Namespace, key.Name)
		if err != nil {
			if errors.Is(err, ErrNoMemoryAccess) {
				d.Logger().Info("chat: transcript replay unavailable (no webd memory token); resuming live-attach-only", "session", key.String())
				writeError(d.Logger(), w, http.StatusNotImplemented, "transcript replay is unavailable on this server; the conversation will resume live")
				return
			}
			d.Logger().Info("chat: read transcript failed", "session", key.String(), "err", err.Error())
			writeError(d.Logger(), w, http.StatusInternalServerError, "failed to load conversation history")
			return
		}
		writeJSON(d.Logger(), w, http.StatusOK, messagesResponse{Timeline: items})
	})
}

// --- POST /sessions/api/{ns}/{name}/message --------------------------------

type messageRequest struct {
	// Text is the user's message; empty is rejected with 400.
	Text string `json:"text"`
	// RequestID is the client's idempotency key, reused across the chat's Retry
	// so a reply lost to a transient timeout can't double-post (channelsd dedups
	// on it). Optional — a missing id disables dedup for that send.
	RequestID string `json:"requestId,omitempty"`
}

type messageResponse struct {
	// SessionID echoes the AgentSession name the message was routed into.
	SessionID string `json:"sessionId"`
	// Routed is true when the message reached the running agent.
	Routed bool `json:"routed"`
	// Ended is true when the conversation had no active session to route to.
	Ended bool `json:"ended"`
	// Notice is a severity-styled card for the user; nil when there is nothing
	// to say beyond Routed/Ended.
	Notice *channelevents.NoticeWire `json:"notice,omitempty"`
}

// messageHandler routes text into the URL's EXISTING conversation. It creates
// nothing: a POST naming a session that does not exist answers 404 like any
// other unknown session (see mapSubmitError). Creation belongs to the
// authorized start routes, which check standing to start a session for a
// class — a check this route has no input for.
func messageHandler(reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := reg.deps.Logger()
		if !webui.TrustedOriginMatch(r, reg.deps.TrustedOrigin()) {
			writeError(log, w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			// Unreachable in practice (AuthAuthenticated guarantees a subject),
			// but fail closed rather than proceed with an empty subject.
			writeError(log, w, http.StatusUnauthorized, "not authenticated")
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			writeError(log, w, http.StatusBadRequest, "session namespace and name are required")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req messageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(log, w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" {
			writeError(log, w, http.StatusBadRequest, "text is required")
			return
		}

		dec, err := reg.SubmitMessage(r.Context(), key, subject, text, req.RequestID)
		if err != nil {
			status, msg := mapSubmitError(log, err)
			writeError(log, w, status, msg)
			return
		}
		writeJSON(log, w, http.StatusOK, messageResponse{
			SessionID: key.Name,
			Routed:    dec.Outcome == channelkinds.OutcomeRouted,
			Ended:     dec.Outcome == channelkinds.OutcomeNoActiveSession,
			Notice:    dec.Notice.ToWire(),
		})
	})
}

// --- POST /sessions/api/{ns}/{name}/interrupt ------------------------------

type interruptRequest struct {
	// RequestID is the client's idempotency key; required, so a retried
	// interrupt cannot cancel a second, later turn.
	RequestID string `json:"requestId"`
}

type interruptResponse struct {
	// OK is true once the interrupt was published; it does not mean the agent
	// has stopped, only that the request is in flight.
	OK bool `json:"ok"`
}

// interruptHandler requests a mid-turn interrupt of an in-flight agent turn.
// Same shape as messageHandler: subject-from-context auth, bounded body,
// gated via Registry.SubmitInterrupt, same error mapping.
func interruptHandler(reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := reg.deps.Logger()
		if !webui.TrustedOriginMatch(r, reg.deps.TrustedOrigin()) {
			writeError(log, w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			writeError(log, w, http.StatusUnauthorized, "not authenticated")
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			writeError(log, w, http.StatusBadRequest, "session namespace and name are required")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req interruptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(log, w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.RequestID == "" {
			writeError(log, w, http.StatusBadRequest, "requestId is required")
			return
		}

		if err := reg.SubmitInterrupt(r.Context(), key, subject, req.RequestID); err != nil {
			status, msg := mapSubmitError(log, err)
			writeError(log, w, status, msg)
			return
		}
		writeJSON(log, w, http.StatusOK, interruptResponse{OK: true})
	})
}

// --- POST /sessions/api/{ns}/{name}/decision -------------------------------

type decisionRequest struct {
	// Category is the channelinteractions.Category being decided; required.
	Category string `json:"category"`
	// RequestRef identifies WHICH pending prompt this answers; required, so a
	// stale card cannot decide a newer prompt.
	RequestRef string `json:"requestRef"`
	// ActionID is the button the user chose, from the prompt's own action set.
	ActionID string `json:"actionId"`
}

type decisionResponse struct {
	// OK is true once the decision was published, not once it was applied.
	OK bool `json:"ok"`
}

// decisionHandler submits a decision on a pending interaction (identity_choice
// and any other decision-kind category — see channelinteractions.Category).
// Same shape as interruptHandler: subject-from-context auth, CSRF origin
// guard, bounded body, gated via Registry.SubmitDecision.
func decisionHandler(reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := reg.deps.Logger()
		if !webui.TrustedOriginMatch(r, reg.deps.TrustedOrigin()) {
			writeError(log, w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			writeError(log, w, http.StatusUnauthorized, "not authenticated")
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			writeError(log, w, http.StatusBadRequest, "session namespace and name are required")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req decisionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(log, w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Category == "" {
			writeError(log, w, http.StatusBadRequest, "category is required")
			return
		}
		if req.RequestRef == "" {
			writeError(log, w, http.StatusBadRequest, "requestRef is required")
			return
		}
		if req.ActionID == "" {
			writeError(log, w, http.StatusBadRequest, "actionId is required")
			return
		}

		if err := reg.SubmitDecision(r.Context(), key, subject, req.Category, req.RequestRef, req.ActionID); err != nil {
			status, msg := mapSubmitError(log, err)
			writeError(log, w, status, msg)
			return
		}
		writeJSON(log, w, http.StatusOK, decisionResponse{OK: true})
	})
}

// The CSRF guard for this package's three mutating POST handlers is
// webui.TrustedOriginMatch — the shared helper every browser-only mutating
// route uses, so all of them fail closed identically on a blank trusted
// origin. A MISSING Origin header is refused too: these routes are driven only
// by the shell's own pages, so there is no non-browser caller to exempt.
//
// The route-level Authorize (chat.go) is not a CSRF defense — it proves
// standing, not that the request came from our own page — so every mutating
// POST keeps this check too.

// mapSubmitError maps a submit/authorize error to an HTTP status + a
// client-safe message. Typed branches carry safe, actionable text; the default
// 500 returns a GENERIC message and logs the raw error, so an internal
// k8s/NATS failure stays diagnosable without leaking detail to the caller.
//
// ErrAuthzUnavailable maps to 503, never 403: a CheckInteract that itself
// failed is indeterminate, not a denial. Its text is a fixed sentinel — the
// raw SpiceDB error was already logged with context by checkInteract — so
// returning err.Error() here leaks no internal cause.
func mapSubmitError(log logr.Logger, err error) (int, string) {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, ErrSessionUnavailable):
		// 503, never 404: the read failed, which is not evidence the session is
		// gone. See the sentinel's own doc for the failure this separates.
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, ErrAuthzUnavailable):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, ErrTooManySessionsForSubject):
		// 429, not 503: the viewer is over their own limit and closing a
		// session fixes it. Distinct from the row below, which they can do
		// nothing about.
		return http.StatusTooManyRequests, err.Error()
	case errors.Is(err, ErrServerAtCapacity):
		return http.StatusServiceUnavailable, err.Error()
	default:
		log.Info("chat: submit message failed", "err", err.Error())
		return http.StatusInternalServerError, "failed to send a message"
	}
}

// --- GET /sessions/api/{ns}/{name}/ws --------------------------------------

// wsHandler upgrades an authenticated request to a websocket and streams the
// session's render events to it. Push-only: the browser sends messages via
// POST .../message; the only thing read from the client connection is used
// to detect close (mirrors artifactview's liveHandler read pump).
func wsHandler(d Deps, reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := webui.SubjectFromContext(r.Context())
		if subject == "" {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		key := pathSessionKey(r)
		if key.Namespace == "" || key.Name == "" {
			http.Error(w, "session namespace and name are required", http.StatusBadRequest)
			return
		}
		// Re-check BEFORE the upgrade: this also resolves (and rehydrates) the
		// live entry the sink attaches to, so an unauthorized caller never
		// reaches an upgraded connection.
		if _, err := reg.authorize(r.Context(), key, subject); err != nil {
			status, msg := mapSubmitError(d.Logger(), err)
			http.Error(w, msg, status)
			return
		}

		upgrader := websocket.Upgrader{
			// Fail closed via the shared helper: an empty TrustedOrigin (the
			// external-URL ConfigMap not yet populated) must vouch for nothing,
			// not match a client that also sends no Origin.
			CheckOrigin: func(req *http.Request) bool {
				return webui.TrustedOriginMatch(req, d.TrustedOrigin())
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote an error response to the client.
			return
		}

		ref := browser.SessionRef{Namespace: key.Namespace, Name: key.Name}
		sink := newWSSink(conn, d.Logger(), ref)
		entry, err := reg.Attach(r.Context(), key, subject, sink)
		if err != nil {
			// Torn down (or vanished) between the pre-upgrade authorize check
			// and the attach. The socket is already upgraded, so log the reason
			// before closing rather than drop it silently. Close the SINK (not
			// just the conn): newWSSink already started the writer goroutine, and
			// only sink.Close() stops it (closing the raw conn leaves the writer
			// blocked on its frames channel — a goroutine leak).
			d.Logger().Info("chat ws: attach after upgrade failed; closing", "session", key.String(), "err", err.Error())
			sink.Close()
			return
		}
		// On return (client disconnect, teardown-driven close, or a failed
		// write) detach from the fan-out AND close the sink — closing the sink
		// stops its dedicated writer goroutine (idempotent with a teardown that
		// already closed it), so a browser that just walks away can never leak
		// that goroutine.
		defer func() {
			reg.Detach(entry, sink)
			sink.Close()
		}()

		// Ask channelsd to re-surface whatever prompt the session is parked on.
		// A prompt sent before this tab connected went out as a one-shot live
		// publish that dedup never repeats, so without this the tab shows a
		// parked session with nothing on screen — and the only other trigger is
		// an inbound message the user waiting on the prompt never sends.
		// Ordering matters: attach first, ask second, or the re-published prompt
		// races the sink that must receive it.
		//
		// Best-effort: a failed nudge costs a card, failing the upgrade would
		// cost the whole conversation. Logged, never swallowed.
		if err := reg.RequestResurface(r.Context(), key, subject); err != nil {
			d.Logger().Info("chat ws: resurface request failed; a prompt this session is parked on may not appear until the next message",
				"session", key.String(), "err", err.Error())
		}

		// The chat ws is push-only: the client only ever sends close/pong
		// control frames, never data. Bound the read side so a misbehaving or
		// hostile client can't stream an unbounded frame into the read buffer.
		conn.SetReadLimit(maxWSClientFrame)

		// Half-open detection: the server ping below pairs with a read deadline
		// the client's pong extends. A client that vanished without a TCP FIN
		// sends no pong, so the deadline fires and the read pump detaches —
		// instead of lingering and pinning the idle-reap clock, which pauses
		// while any sink is attached. pongWait must exceed the ping interval so
		// a healthy pong always lands first; the floor keeps a test that shrinks
		// the ping to milliseconds from tripping the deadline before its first
		// round-trip completes.
		pongWait := 2 * reg.wsPing()
		if pongWait < 2*time.Second {
			pongWait = 2 * time.Second
		}
		if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			d.Logger().Info("chat ws: set initial read deadline failed; closing", "session", key.String(), "err", err.Error())
			return
		}
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})

		// Keepalive: an intermediary drops a connection idle through a slow
		// runner cold start or a long turn, and the sink has no replay, so the
		// reply emitted during the gap is lost until a full reload. The
		// goroutine stops via stopPing when the read pump below returns.
		stopPing := make(chan struct{})
		defer close(stopPing)
		go func() {
			t := time.NewTicker(reg.wsPing())
			defer t.Stop()
			for {
				select {
				case <-stopPing:
					return
				case <-t.C:
					if !sink.ping() {
						return // connection dead; the read pump unblocks + detaches
					}
				}
			}
		}()

		// Read pump: drains client frames (used only to detect close) until
		// the connection errors or is closed by the server side (wsSink.Close
		// on a dead write, or Registry.teardown on session end).
		for {
			if _, _, rerr := conn.ReadMessage(); rerr != nil {
				return
			}
		}
	})
}

// --- shared response helpers ----------------------------------------------

type errorResponse struct {
	// Error is the client-safe reason; internal causes stay in the logs.
	Error string `json:"error"`
}

func writeJSON(log logr.Logger, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line + headers are already committed, so the response
		// can't change now — a log is the only correct surface for a broken
		// encode (a dead client connection, or an unencodable payload).
		log.Info("chat: encode JSON response failed", "status", status, "err", err.Error())
	}
}

func writeError(log logr.Logger, w http.ResponseWriter, status int, msg string) {
	writeJSON(log, w, status, errorResponse{Error: msg})
}
