// live.go serves GET /agent-ui/{ns}/{name}/live: a websocket carrying an
// agent-UI action's lifecycle from wherever it settles to whatever browser is
// watching — including one that reconnects AFTER it settled.
//
// Correctness depends on the runner's uiActionRecorder writing the durable
// ui_action memory record BEFORE publishing the ui_action_update envelope, in
// that order, so this route's snapshot-then-stream shape can never diverge
// from a live push.
//
// Modeled on pkg/web/webui/sessionview/live.go, NOT on gateAgentUIPost. That
// ladder is POST-shaped: an Origin HEADER check before any body is read. A
// websocket's CSRF boundary is the Upgrader's own CheckOrigin, checked as part
// of the handshake, so reusing gateAgentUIPost would 403 on the header before
// an Upgrade could ever happen.
package agentui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/livemirror"
)

// maxLiveActions bounds the snapshot frame's Actions slice — uiaction.List's
// own limit param. A session with more settled action records than this
// returns only the newest maxLiveActions: a long-lived, click-happy session
// must never make the FIRST frame this route sends unbounded.
const maxLiveActions = 64

// liveWriteTimeout bounds each websocket write so a stuck client cannot pin
// this goroutine forever — mirrors sessionview's liveWriteTimeout.
const liveWriteTimeout = 10 * time.Second

// maxLiveClientFrame bounds a single frame read from this websocket client.
// The socket is push-only in practice — the only thing a browser may say here
// is a small presence ping — so anything approaching this size is broken or
// hostile. gorilla's default read limit is UNLIMITED and the read pump uses
// ReadMessage (NextReader + io.ReadAll), so without this a single client can
// grow webd's read buffer without bound: an OOM of one shared pod, reachable
// by any subject holding agentsession#interact on any one session.
//
// Same value and reasoning as sessionview's and artifactview's
// maxLiveClientFrame and the chat socket's maxWSClientFrame.
const maxLiveClientFrame = 1024

// liveActionMessage is one frame on the agent-UI live socket.
//
// "snapshot" carries every action record the CONNECTED VIEWER has in this
// session, read from memory at open; "event" carries one transition as it
// happens. Same shape, because they are the same fact from two sources —
// memory is authoritative, NATS is the low-latency copy — so a reconnecting
// browser and a live one converge on identical state. A client that handled
// them differently would reintroduce that divergence.
type liveActionMessage struct {
	// Type discriminates the frame the browser renders.
	Type string `json:"type"` // "snapshot" | "event" | "error"
	// Actions is the open-time snapshot, newest first; present only on
	// "snapshot", where empty genuinely means no records.
	Actions []liveActionEntry `json:"actions,omitempty"`
	// Action is the single transition; present only on "event".
	Action *liveActionEntry `json:"action,omitempty"`
	// Message is browser-safe human copy for Type=="error" ONLY, and never an
	// internal identifier.
	Message string `json:"message,omitempty"`
}

// liveActionEntry is one action's state AS THE BROWSER SEES IT. Note what is
// absent: uiaction.Content.Requester is stripped deliberately — it is a
// canonical SpiceDB subject and the server's own filter input, never something
// a browser needs or may have.
type liveActionEntry struct {
	// RequestID correlates this entry with the POST that started the action.
	RequestID string `json:"requestId"`
	// Action is the declared action name that was invoked.
	Action string `json:"action"`
	// State is the lifecycle state the control renders.
	State string `json:"state"`
	// Message is browser-safe human copy; empty when the state says it all.
	Message string `json:"message,omitempty"`
	// ApprovalAddressedToViewer is true when THIS viewer is the one being
	// asked to approve, so only their tab renders the prompt.
	ApprovalAddressedToViewer bool `json:"approvalAddressedToViewer,omitempty"`
	// UpdatedAt is when the record last changed, for ordering across sources.
	UpdatedAt time.Time `json:"updatedAt"`
}

// liveViewMessage is the "view" arm of this socket's message union, sharing
// the socket and the `type` discriminator with liveActionMessage rather than
// opening a second connection per page.
//
// NOT requester-filtered, unlike the action arm: an action belongs to whoever
// clicked it, but a view-model change is a property of the SESSION and every
// viewer must see it. A view update carries no requester at all, so copying
// the action arm's filter here would suppress the update for everyone.
type liveViewMessage struct {
	// Type is always "view" on this arm.
	Type string `json:"type"` // "view"
	// Hook names the hook whose change triggered the push; diagnostics only —
	// the frame always carries the WHOLE declaration, never a per-hook patch.
	Hook string `json:"hook,omitempty"`
	// Declaration is webd's OWN re-resolved read, never anything carried on
	// the triggering envelope: the push is a trigger, not a document.
	Declaration declarationWire `json:"declaration"`
	// AgentParams is what the agent set this view's controls to, filtered to
	// the parameters the declaration in THIS SAME frame declares. The browser
	// applies it only to controls its viewer has not touched.
	AgentParams map[string]string `json:"agentParams,omitempty"`
}

// liveHandler authorizes an agent-UI live websocket request and then runs
// runActionMirror. Order:
//
//  1. webui.SubjectFromContext empty => 401.
//  2. d.CheckInteract(ns, name, subject) — the SAME agentsession#interact
//     check the POST routes run, and this route's only authorization. An
//     error is NEVER a denial: fail-closed 500, logged. Checked BEFORE the
//     upgrade, so an unauthorized socket never opens.
//  3. Upgrader.CheckOrigin — the CSRF boundary for a GET websocket.
//  4. A read-pump goroutine drains client frames and cancels the context on
//     close/error, so runActionMirror exits promptly and nothing leaks.
//  5. runActionMirror carries the snapshot-then-stream body.
func liveHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")
		session := ns + "/" + name

		// 1. Authentication.
		subject := webui.SubjectFromContext(ctx)
		if subject == "" {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}

		// 2. agentsession#interact — the SAME check gateAgentUIPost runs,
		// inline here because a websocket answers a refusal before the upgrade
		// with plain statuses, not that helper's JSON bodies. An error is
		// fail-closed 500, never a denial.
		ok, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Error(err, "agent-ui live: CheckInteract errored; returning 500",
				"session", session, "subject", subject)
			http.Error(w, "authorization error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not authorized to interact with this session", http.StatusForbidden)
			return
		}

		// 3. Authorized; upgrade. Only the trusted shell origin may open the
		// socket, answered by the same webui.TrustedOriginMatch the POST routes
		// use — so a blank trusted origin fails closed here too, rather than
		// admitting every handshake that sends no Origin.
		trustedOrigin := d.TrustedOrigin()
		upgrader := websocket.Upgrader{
			CheckOrigin: func(req *http.Request) bool {
				return webui.TrustedOriginMatch(req, trustedOrigin)
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote an HTTP error response to the client; still
			// log so the failure is diagnosable rather than silent (no-silent-
			// errors — an upgrade failure is on this route's required-log list).
			d.Logger().Info("agent-ui live: websocket upgrade failed",
				"session", session, "subject", subject, "err", err.Error())
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxLiveClientFrame)

		// 4. The read pump drains client frames and cancels the context the
		// moment the client closes or errors, so runActionMirror exits
		// promptly and no goroutine leaks.
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer cancel()
			// Throttle state is per-SOCKET, so one viewer cannot buy another
			// viewer's budget and a single tab cannot flood the bus.
			var lastPresence time.Time
			for {
				_, data, rerr := conn.ReadMessage()
				if rerr != nil {
					return
				}
				// Unparseable or unrecognized frames are dropped in silence.
				// This direction is not an API surface: the only thing a client
				// may say here is "still watching", and logging every stray
				// frame would hand any browser a write into the operator log.
				var f clientFrame
				if json.Unmarshal(data, &f) != nil || f.Type != clientFrameTypePresence {
					continue
				}
				if time.Since(lastPresence) < presenceRepublishInterval {
					continue
				}
				lastPresence = time.Now()
				publishUIPresence(d, ns, name, subject)
			}
		}()

		runActionMirror(wctx, conn, d, ns, name, subject)
	})
}

// runActionMirror subscribes to the session's ui_action_update NATS subject
// FIRST, then reads the memory snapshot, then streams matching events until
// ctx is cancelled or a write fails.
//
// Subscribe-before-snapshot is load-bearing. Snapshot-then-subscribe has a
// real gap: a record settling between the List call and the WatchOutbound
// registration is invisible to BOTH the snapshot (already queried) and the
// stream (not yet subscribed), lost until the next reconnect. Subscribing
// first captures every settle from that point on, even one racing the snapshot
// read; the cost is a possible SECOND delivery, which sentAsOfSnapshot
// suppresses so the browser sees it exactly once.
func runActionMirror(ctx context.Context, conn *websocket.Conn, d Deps, ns, name, subject string) {
	session := ns + "/" + name
	requester := uiaction.RequesterKey(subject)
	scope := memory.Scope{Kind: "session", ID: session}

	mem := d.Memory()
	if mem == nil {
		// Fail closed LOUDLY: an empty snapshot would read as "you have no
		// pending actions", a claim nothing checked. A nil Memory is a wiring
		// bug, not "no data" — and the view arm needs the same Memory, so
		// there is nothing further this connection could offer anyway.
		d.Logger().Info("agent-ui live: Memory not configured; failing closed", "session", session, "subject", subject)
		_ = writeLiveFrame(conn, liveActionMessage{
			Type:    "error",
			Message: "this view's live updates are unavailable right now",
		})
		return
	}

	// Subscribe FIRST — see the doc comment above. ONE call, both kinds: the
	// view arm needs the same "never miss a settle that races this
	// registration" property the action arm has, and WatchOutbound is variadic
	// over kinds so a second subscription and goroutine is never needed.
	var eventCh <-chan channelevents.Envelope
	if nc := d.NATS(); nc != nil {
		ch, werr := livemirror.WatchOutbound(ctx, nc, ns, name, channelevents.KindUIActionUpdate, channelevents.KindUIViewUpdate)
		if werr != nil {
			d.Logger().Error(werr, "agent-ui live: WatchOutbound failed; live updates disabled (snapshot-only)",
				"session", session, "subject", subject)
		} else {
			eventCh = ch
		}
	} else {
		// A real, logged degradation (see deps.go's NATS doc comment): the
		// memory read below still answers, and a reconnecting browser still
		// learns the terminal state on its next reload.
		d.Logger().Info("agent-ui live: NATS not configured; live updates disabled (snapshot-only)",
			"session", session, "subject", subject)
	}

	actions, err := uiaction.List(ctx, mem, scope, requester, maxLiveActions)
	if err != nil {
		d.Logger().Error(err, "agent-ui live: snapshot read failed", "session", session, "subject", subject)
		_ = writeLiveFrame(conn, liveActionMessage{
			Type:    "error",
			Message: "this view's pending actions could not be loaded",
		})
		return
	}

	// sentAsOfSnapshot remembers each record's (requestID -> state) as the
	// snapshot delivered it, so an event that settled after WatchOutbound
	// registered but also landed in this same List call is recognized as
	// already-delivered rather than replayed as a duplicate frame.
	sentAsOfSnapshot := make(map[string]string, len(actions))
	entries := make([]liveActionEntry, 0, len(actions))
	for _, c := range actions {
		entries = append(entries, toLiveActionEntry(c))
		sentAsOfSnapshot[c.RequestID] = string(c.State)
	}
	if werr := writeLiveFrame(conn, liveActionMessage{Type: "snapshot", Actions: entries}); werr != nil {
		d.Logger().Info("agent-ui live: snapshot write failed; closing",
			"session", session, "subject", subject, "err", werr.Error())
		return
	}

	// The view arm's open-time frame, sent beside the action snapshot. A
	// resolveView failure logs and skips only THIS frame: the socket stays
	// open and keeps delivering action updates.
	if viewMsg, verr := buildLiveViewMessage(ctx, d, ns, name, ""); verr != nil {
		d.Logger().Info("agent-ui live: resolveView failed on connect; view push skipped for this connection",
			"session", session, "subject", subject, "err", verr.Error())
	} else if werr := writeLiveFrame(conn, viewMsg); werr != nil {
		d.Logger().Info("agent-ui live: view snapshot write failed; closing",
			"session", session, "subject", subject, "err", werr.Error())
		return
	}

	for {
		select {
		case <-ctx.Done():
			d.Logger().Info("agent-ui live: context cancelled; closing", "session", session, "subject", subject)
			return
		case env, ok := <-eventCh:
			if !ok {
				eventCh = nil // watcher closed; stop selecting on it
				continue
			}
			switch env.Kind {
			case channelevents.KindUIActionUpdate:
				var payload channelevents.UIActionUpdatePayload
				if uerr := json.Unmarshal(env.Payload, &payload); uerr != nil {
					d.Logger().Info("agent-ui live: malformed ui_action_update payload; skipping",
						"session", session, "subject", subject, "err", uerr.Error())
					continue
				}
				// Filter through uiaction.RequesterKey on BOTH sides: a raw
				// compare between the record's bare canonical Requester and
				// SubjectFromContext's prefixed form is always false, which
				// fails CLOSED — invisible in a single-viewer test — rather
				// than leaking one viewer's action onto another's page.
				if uiaction.RequesterKey(payload.Requester) != requester {
					continue
				}
				if s, ok := sentAsOfSnapshot[payload.RequestID]; ok && s == payload.State {
					// Exact duplicate of what the snapshot already carried — the
					// settle raced the subscribe/snapshot window and landed in both.
					// Suppress rather than replay.
					continue
				}
				sentAsOfSnapshot[payload.RequestID] = payload.State
				entry := liveActionEntry{
					RequestID:                 payload.RequestID,
					Action:                    payload.Action,
					State:                     payload.State,
					Message:                   payload.Message,
					ApprovalAddressedToViewer: payload.ApprovalAddressedToViewer,
					UpdatedAt:                 payload.UpdatedAt,
				}
				if werr := writeLiveFrame(conn, liveActionMessage{Type: "event", Action: &entry}); werr != nil {
					d.Logger().Info("agent-ui live: event write failed; closing",
						"session", session, "subject", subject, "err", werr.Error())
					return
				}
			case channelevents.KindUIViewUpdate:
				// NOT requester-filtered — see liveViewMessage's own doc comment.
				var payload channelevents.UIViewUpdatePayload
				if uerr := json.Unmarshal(env.Payload, &payload); uerr != nil {
					d.Logger().Info("agent-ui live: malformed ui_view_update payload; skipping",
						"session", session, "subject", subject, "err", uerr.Error())
					continue
				}
				viewMsg, verr := buildLiveViewMessage(ctx, d, ns, name, payload.Hook)
				if verr != nil {
					// A resolveView failure here must not tear down a socket that is
					// still delivering action updates — log and skip THIS FRAME only.
					d.Logger().Info("agent-ui live: resolveView failed for a ui_view_update push; skipping this frame",
						"session", session, "subject", subject, "hook", payload.Hook, "err", verr.Error())
					continue
				}
				if werr := writeLiveFrame(conn, viewMsg); werr != nil {
					d.Logger().Info("agent-ui live: view event write failed; closing",
						"session", session, "subject", subject, "err", werr.Error())
					return
				}
			default:
				// WatchOutbound only forwards the two kinds subscribed above; a
				// third kind reaching here would mean this route's own
				// subscription list drifted from its switch. Logged, never
				// silently dropped.
				d.Logger().Info("agent-ui live: unexpected envelope kind on this socket; skipping",
					"session", session, "subject", subject, "kind", string(env.Kind))
			}
		}
	}
}

// buildLiveViewMessage re-resolves the merged view for (ns, name) — the SAME
// resolveView/declarationWireFor path the GET bootstrap and the
// bindings/actions routes use — and wraps it as this route's "view" frame.
// hook is carried for diagnostics only; it never selects what to resolve.
//
// PRECONDITION: the viewer may interact with (ns, name). liveHandler gates it
// once, before the upgrade, for the life of the socket.
func buildLiveViewMessage(ctx context.Context, d Deps, ns, name, hook string) (liveViewMessage, error) {
	doors, v, pe := resolveView(ctx, d, ns, name)
	if pe != nil {
		return liveViewMessage{}, pe
	}
	// The agent's parameter choices ride the same frame as the declaration,
	// re-read here rather than carried on the trigger — same "memory is the
	// source of truth, the envelope is only the push" discipline, and the same
	// security reason: a browser only ever receives values webd read and
	// filtered itself.
	//
	// Sent on EVERY view frame, not only ones a parameter write caused: the
	// frame re-resolves the whole view, and applying a new declaration against
	// the old control values would show a mismatched page.
	//
	// answeredHooksFor is called with the SAME (ns, name) resolveView was, so
	// its scope matches the one mergedViewFor used to resolve v. The UI name
	// rides along for its log line only.
	answered := answeredHooksFor(ctx, d, ns, name, doors.UI.Name, v)
	return liveViewMessage{
		Type: "view", Hook: hook, Declaration: declarationWireFor(v, answered),
		AgentParams: agentParamsFor(ctx, d, ns, name, doors.UI.Name, v.Declaration),
	}, nil
}

// toLiveActionEntry maps a durable uiaction.Content to what the browser
// receives — see liveActionEntry's own doc comment for what is deliberately
// dropped (Requester).
func toLiveActionEntry(c uiaction.Content) liveActionEntry {
	return liveActionEntry{
		RequestID:                 c.RequestID,
		Action:                    c.Action,
		State:                     string(c.State),
		Message:                   c.Message,
		ApprovalAddressedToViewer: c.ApprovalAddressedToViewer,
		UpdatedAt:                 c.UpdatedAt,
	}
}

// writeLiveFrame writes one frame under a deadline; a non-nil return means the
// connection is dead and the caller should stop. Takes `any` so the action and
// view arms — two wire shapes sharing one socket — don't duplicate the
// deadline logic. msg is always a liveActionMessage or a liveViewMessage.
func writeLiveFrame(conn *websocket.Conn, msg any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(msg)
}

// clientFrame is the ONLY thing this route accepts from the browser. The
// socket is otherwise server→client (view and action frames), and it stays
// that way: a client frame may ask the platform to keep serving, and nothing
// else.
type clientFrame struct {
	// Type is the only field read; anything but clientFrameTypePresence is
	// dropped in silence.
	Type string `json:"type"`
}

// clientFrameTypePresence is the one recognized client frame: the viewer's tab
// reports itself visible, focused and recently interacted with.
//
// Those CONDITIONS are evaluated in the browser, deliberately — they are facts
// only the tab holds, and re-deriving them here would mean inventing a weaker
// notion ("the socket is open") that keeps a pod alive for a window somebody
// forgot about. This end's contract is narrower: a frame arrived, so someone
// is still there.
const clientFrameTypePresence = "presence"

// presenceRepublishInterval floors how often one socket may become a heartbeat
// on the bus. The browser sets its own cadence and this does not trust it: a
// client sending presence in a tight loop would turn one tab into a NATS
// flood, and the runner gains nothing from a second "still watching" inside a
// window. Chosen well under the runner's idle TTL, so a well-behaved client's
// frames are never coalesced into uselessness.
const presenceRepublishInterval = 10 * time.Second

// publishUIPresence forwards one presence heartbeat to the session's runner.
//
// Fire-and-forget, NOT a request: an acked heartbeat would let a watching
// browser block on the runner, and the next heartbeat is strictly fresher than
// a retry of this one. The payload is empty — presence means only "a viewer is
// still there", and fields invite reading it as more than a liveness hint.
//
// Errors are logged and dropped: a failed heartbeat degrades to the runner
// idling out on its base TTL, so failing the socket over one trades a small
// degradation for a large one.
func publishUIPresence(d Deps, ns, name, subject string) {
	conn := d.NATS()
	if conn == nil {
		return
	}
	subj := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIPresence)
	if err := conn.Publish(subj, []byte("{}")); err != nil {
		d.Logger().Info("agent-ui live: ui_presence publish failed; this session's runner may idle out while still being watched",
			"session", ns+"/"+name, "subject", subject, "err", err.Error())
	}
}
