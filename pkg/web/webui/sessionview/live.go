package sessionview

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/livemirror"
)

// liveWriteTimeout bounds each websocket write so a stuck client cannot pin
// the mirror goroutine forever — mirrors artifactview's liveWriteTimeout.
const liveWriteTimeout = 10 * time.Second

// maxLiveClientFrame bounds a single frame read from the session-view
// websocket client. The socket is push-only — the browser sends only
// close/pong control frames, never data — so anything approaching this size is
// broken or hostile. gorilla's default read limit is UNLIMITED, so without
// this a single client can grow webd's read buffer without bound: an OOM of
// one shared pod, triggerable by any subject holding agentsession#interact.
// Same value and reasoning as artifactview's maxLiveClientFrame and the chat
// socket's maxWSClientFrame.
const maxLiveClientFrame = 1024

// liveKinds is the outbound envelope kind set the session-view mirror streams
// live: user-facing messages (both directions), plan updates, notifications,
// and turn activity — the conversational + status surface a resumed history
// replays, kept live. No artifact-revision polling, unlike artifactview: this
// page has no artifact.
var liveKinds = []channelevents.Kind{
	channelevents.KindUserMessage,
	channelevents.KindUserEcho,
	channelevents.KindPlanUpdate,
	channelevents.KindNotification,
	channelevents.KindTurnActivity,
	channelevents.KindWidgetOffer,
}

// liveMessage is one server→client frame.
type liveMessage struct {
	// Type is "snapshot" (History set) or "event" (Event set).
	Type string `json:"type"`
	// History is the initial replay; empty is a legitimate empty transcript,
	// and also what a failed replay degrades to (runLiveMirror logs it).
	History []livemirror.TimelineEntry `json:"history,omitempty"`
	// Event is one live outbound envelope.
	Event *channelevents.Envelope `json:"event,omitempty"`
}

// liveHandler authorizes a session-view websocket request (CheckInteract,
// BEFORE the upgrade — the same gate the shell page uses) and then pushes a
// history snapshot followed by live outbound envelopes.
func liveHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")
		subject := webui.SubjectFromContext(ctx)

		ok, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Error(err, "sessionview live: CheckInteract errored; returning 500",
				"ns", ns, "name", name, "subject", subject)
			http.Error(w, "authorization error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "you do not have access to this session", http.StatusForbidden)
			return
		}

		// Authorized. Upgrade. Only the trusted shell origin may open the socket.
		upgrader := websocket.Upgrader{
			// Fail closed via the shared helper (empty trusted origin matches nothing).
			CheckOrigin: func(req *http.Request) bool {
				return webui.TrustedOriginMatch(req, d.TrustedOrigin())
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote an error response to the client.
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxLiveClientFrame)

		// The read pump drains client frames (control and data alike) and
		// cancels the context the moment the client closes or errors, so
		// runLiveMirror exits promptly and no goroutine leaks.
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer cancel()
			for {
				if _, _, rerr := conn.ReadMessage(); rerr != nil {
					return
				}
			}
		}()

		runLiveMirror(wctx, conn, d, ns, name)
	})
}

// runLiveMirror writes the initial history snapshot, then streams live
// outbound envelopes until ctx is cancelled (client gone) or a write fails.
func runLiveMirror(ctx context.Context, conn *websocket.Conn, d Deps, ns, name string) {
	history, err := livemirror.ReadHistory(ctx, d.OperatorURL(), d.MemoryToken(), ns, name, d.Logger())
	if err != nil {
		// Best-effort: log + send an empty snapshot rather than killing the conn.
		// The live stream below still works even if history replay failed.
		d.Logger().Error(err, "sessionview live: ReadHistory failed; sending empty snapshot", "ns", ns, "name", name)
		history = livemirror.History{}
	}
	if werr := writeLiveMessage(conn, liveMessage{Type: "snapshot", History: history.Timeline}); werr != nil {
		return
	}

	// A nil NATS connection or a subscribe failure degrades to "no live
	// updates" (an always-nil channel never fires in the select below) rather
	// than failing the whole socket — the history snapshot the client already
	// has is still useful on its own.
	var eventCh <-chan channelevents.Envelope
	if nc := d.NATS(); nc != nil {
		ch, werr := livemirror.WatchOutbound(ctx, nc, ns, name, liveKinds...)
		if werr != nil {
			d.Logger().Error(werr, "sessionview live: WatchOutbound failed; live updates disabled", "ns", ns, "name", name)
		} else {
			eventCh = ch
		}
	} else {
		d.Logger().Info("sessionview live: NATS not configured; live updates disabled", "ns", ns, "name", name)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-eventCh:
			if !ok {
				eventCh = nil // watcher closed; stop selecting on it
				continue
			}
			e := env
			if e.Kind == channelevents.KindWidgetOffer {
				// The raw envelope carries only the artifact identity, and the
				// browser holds no signing key, so the /mcpui-host URL is minted
				// here at relay time.
				e = mintWidgetOfferHostURL(d, ns, name, e)
			}
			if werr := writeLiveMessage(conn, liveMessage{Type: "event", Event: &e}); werr != nil {
				return // client gone
			}
		}
	}
}

// widgetOfferClientPayload is what the browser receives for a live
// widget_offer event: channelevents.WidgetOfferPayload plus a freshly-minted
// /mcpui-host URL, rewritten at relay time by mintWidgetOfferHostURL because
// the browser cannot mint its own content-capability token.
type widgetOfferClientPayload struct {
	// ArtifactID identifies the offered widget within this session.
	ArtifactID string `json:"artifactId"`
	// Tool is the MCP tool whose call produced the widget.
	Tool string `json:"tool,omitempty"`
	// RendererKind is the channelassets renderer that produced it.
	RendererKind string `json:"rendererKind,omitempty"`
	// HostURL is the framable /mcpui-host address; empty means the mint failed
	// and the client skips this widget.
	HostURL string `json:"hostUrl"`
}

// mintWidgetOfferHostURL rewrites a widget_offer envelope's payload to carry
// a freshly-minted /mcpui-host URL. A malformed payload or a mint failure is
// logged and the envelope is forwarded WITH AN EMPTY HostURL (best-effort):
// the client's handleFrame skips a widget it has no URL for rather than
// crashing, and the durable status.activeWidgets bootstrap (page.go) still
// surfaces the widget on the next page load regardless.
func mintWidgetOfferHostURL(d Deps, ns, name string, env channelevents.Envelope) channelevents.Envelope {
	var p channelevents.WidgetOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		d.Logger().Error(err, "sessionview live: widget_offer payload unmarshal failed; forwarding without hostUrl",
			"ns", ns, "name", name)
		return env
	}
	// Same fail-closed gate the bootstrap path applies (sandboxOrigin):
	// without a usable sandbox origin, a minted hostUrl would frame
	// MCP-server-authored widget HTML in the TRUSTED origin. Forwarding with an
	// empty HostURL is the supported degradation — the client skips a widget it
	// has no URL for.
	base := sandboxOrigin(d)
	if base == "" {
		d.Logger().Info("sessionview live: sandbox base URL is not an http(s) origin; forwarding widget_offer without hostUrl",
			"ns", ns, "name", name, "artifactID", p.ArtifactID, "sandboxBaseURL", d.SandboxBaseURL())
		return env
	}
	ct, err := d.SignWidgetToken(ns, name, p.ArtifactID)
	if err != nil {
		d.Logger().Error(err, "sessionview live: SignWidgetToken failed; forwarding without hostUrl",
			"ns", ns, "name", name, "artifactID", p.ArtifactID)
		return env
	}
	out, err := json.Marshal(widgetOfferClientPayload{
		ArtifactID:   p.ArtifactID,
		Tool:         p.Tool,
		RendererKind: p.RendererKind,
		HostURL:      base + "/mcpui-host?ct=" + ct,
	})
	if err != nil {
		d.Logger().Error(err, "sessionview live: marshal widget_offer client payload failed; forwarding without hostUrl",
			"ns", ns, "name", name, "artifactID", p.ArtifactID)
		return env
	}
	env.Payload = out
	return env
}

// writeLiveMessage writes one frame under a deadline. A non-nil return means
// the connection is dead and the caller should stop.
func writeLiveMessage(conn *websocket.Conn, msg liveMessage) error {
	if err := conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(msg)
}
