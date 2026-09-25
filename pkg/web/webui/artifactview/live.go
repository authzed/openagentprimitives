package artifactview

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// livePollIntervalNs is how often the live handler re-lists revisions to detect
// a new head, in nanoseconds. Stored atomically (not a const) so tests can
// lower it while poller goroutines from prior cases may still be reading it.
var livePollIntervalNs atomic.Int64

func init() { livePollIntervalNs.Store(int64(2 * time.Second)) }

// livePollInterval reads the current poll interval.
func livePollInterval() time.Duration { return time.Duration(livePollIntervalNs.Load()) }

// setLivePollInterval sets the poll interval (test seam).
func setLivePollInterval(d time.Duration) { livePollIntervalNs.Store(int64(d)) }

// liveWriteTimeout bounds each websocket write so a stuck client cannot pin a
// poller goroutine forever.
const liveWriteTimeout = 10 * time.Second

// maxLiveClientFrame bounds a single frame read from the live-view websocket
// client. The socket is push-only — the browser only ever sends close/pong
// control frames, never data — so a tiny bound is ample and stops a misbehaving
// or hostile client from streaming an unbounded frame into the read buffer
// (gorilla's default read limit is unlimited).
const maxLiveClientFrame = 1024

// liveCurrent describes the artifact's current (newest) render: the iframe src
// the client should load.
type liveCurrent struct {
	Seq        int    `json:"seq"`        // 1-based position of the source revision being shown
	RevisionID string `json:"revisionId"` // source revision this render was generated from
	HostURL    string `json:"hostUrl"`    // sandbox-origin host page the shell frames
	ContentURL string `json:"contentUrl"` // raw render URL the host swaps its inner frame to
}

// liveRevisionItem is a client-facing revision entry. It deliberately OMITS
// RenderName: that is a server-side capability input (content tokens are minted
// from it) and must never reach the browser.
type liveRevisionItem struct {
	Seq               int      `json:"seq"`               // 1-based position, oldest first
	RevisionID        string   `json:"revisionId"`        // opaque id; the ?rev value for pinning
	ChangeDescription string   `json:"changeDescription"` // agent's summary of this revision; may be empty
	CreatedAt         string   `json:"createdAt"`         // RFC3339
	Tags              []string `json:"tags"`              // agent-supplied labels; empty when untagged
}

// liveMessage is one server→client frame. Type is "snapshot" (initial
// revisions), "revision" (head changed), "status" (session presentation
// status changed — only Status is set), or "message" (an outbound chat
// mirror line — only Message is set).
type liveMessage struct {
	Type      string             `json:"type"`              // snapshot | revision | status | message
	Current   liveCurrent        `json:"current"`           // zero-valued when nothing is servable yet
	Revisions []liveRevisionItem `json:"revisions"`         // full list each time, not a delta
	Status    *StatusSnapshot    `json:"status,omitempty"`  // nil unless Type=="status"
	Message   *MirrorMessage     `json:"message,omitempty"` // nil unless Type=="message"
}

// liveHandler authorizes a live-view websocket request (bindView: link verify +
// SpiceDB view gate + artifact→session binding, all BEFORE the upgrade) and then
// pushes the initial snapshot plus a "revision" frame whenever the artifact's
// newest revision id changes.
//
// The socket carries TWO streams with two different gates. The artifact's own
// revisions are gated on artifact#view. The SESSION-scoped mirrors (status,
// plan, conversation) are additionally gated on agentsession#interact, and both
// are scoped to the session bindView RESOLVED from the artifact — never to the
// sessionRef the client sent.
func liveHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := webui.SubjectFromContext(r.Context()) // injected by AuthAuthenticated middleware
		// The client forwards window.location.search to the ws URL, so whichever
		// param form the shell was opened with reaches here.
		b, denial := bindView(r.Context(), av, r.URL.Query())
		if denial != nil {
			denial.writeHTTP(w)
			return
		}
		mirrors := b.mirrorsAllowed(r.Context(), av, subject)

		// Authorized. Upgrade. Only the trusted shell origin may open the socket.
		upgrader := websocket.Upgrader{
			// Fail closed via the shared helper (empty trusted origin matches nothing).
			CheckOrigin: func(req *http.Request) bool {
				return webui.TrustedOriginMatch(req, av.TrustedOrigin())
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote an error response to the client.
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxLiveClientFrame)

		// A read pump drains client frames (control + any data) and cancels the
		// context the moment the client closes or errors, so the poll loop below
		// exits promptly and no goroutine leaks.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			defer cancel()
			for {
				if _, _, rerr := conn.ReadMessage(); rerr != nil {
					return
				}
			}
		}()

		runLivePoller(ctx, conn, av, b, mirrors)
	})
}

// runLivePoller writes the initial snapshot, then polls for head changes and
// pushes a "revision" frame each time the newest revision id changes. It
// returns when ctx is cancelled (client gone) or a write fails.
//
// Every session-scoped call takes its (ns, sess) from b — the binding bindView
// RESOLVED from the artifact — so this loop cannot be pointed at a session the
// artifact does not live in. mirrors=false (the subject may view the artifact
// but is not a session participant) leaves the two session mirrors unopened;
// the revision stream is unaffected.
func runLivePoller(ctx context.Context, conn *websocket.Conn, av Deps, b viewBinding, mirrors bool) {
	var snapshot liveMessage
	var lastKey string
	// b.revisions is the binding probe's own list — reuse it for the first frame
	// rather than asking the store the same question twice per connect.
	contentRender, ready, err := av.ContentRender(ctx, b.ns, b.sess, b.artifactID)
	if err == nil {
		snapshot, lastKey, err = buildLiveMessage(av, b, "snapshot", b.revisions, contentRender, ready)
	}
	if err != nil {
		// Tell the client nothing rendered yet rather than killing the conn —
		// the poll loop picks the content up once it appears — but never swallow
		// the reason: a snapshot empty because minting failed looks identical to
		// one empty because the artifact has not rendered yet.
		av.Logger().Error(err, "live-view: initial snapshot failed; sending the empty state and continuing to poll",
			"artifact", b.artifactID, "sessionRef", b.sessionRef())
		snapshot = liveMessage{Type: "snapshot", Revisions: []liveRevisionItem{}}
		lastKey = ""
	}
	if werr := writeLiveMessage(conn, snapshot); werr != nil {
		return
	}

	// Mirror the session's presentation status (active/paused, plan, status
	// message) and its outbound conversation. Both are session-scoped, so both
	// are gated on mirrors. A nil channel (mirrors denied, NATS not wired, or a
	// watch error) is never-ready in select, so the case is inert and the panel
	// stays empty; revisions still flow.
	var statusCh <-chan StatusSnapshot
	var msgCh <-chan MirrorMessage
	if mirrors {
		var serr error
		if statusCh, serr = av.WatchSessionStatus(ctx, b.ns, b.sess); serr != nil {
			av.Logger().Error(serr, "live-view: WatchSessionStatus failed; status panel disabled",
				"artifact", b.artifactID, "sessionRef", b.sessionRef())
			statusCh = nil
		}
		var merr error
		if msgCh, merr = av.WatchMessages(ctx, b.ns, b.sess); merr != nil {
			av.Logger().Error(merr, "live-view: WatchMessages failed; chat mirror disabled",
				"artifact", b.artifactID, "sessionRef", b.sessionRef())
			msgCh = nil
		}
	}

	ticker := time.NewTicker(livePollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Resolve the change-key FIRST and early-out when unchanged: the
			// change-key IS the content render name, so ContentRender alone
			// decides whether this tick has anything to push, and the steady
			// state is every tick but the rare one. Listing revisions first would
			// spend a second store round-trip per tick, forever, on a frame that
			// is then discarded.
			contentRender, ready, cerr := av.ContentRender(ctx, b.ns, b.sess, b.artifactID)
			if cerr != nil {
				// Best-effort: log + keep polling (an operator/memory blip).
				av.Logger().Error(cerr, "live-view: poll iteration failed; will retry",
					"artifact", b.artifactID, "sessionRef", b.sessionRef())
				continue
			}
			if !ready || contentRender == "" || contentRender == lastKey {
				continue // nothing servable yet, or content unchanged
			}
			revs, lerr := av.ListRevisions(ctx, b.ns, b.sess, b.artifactID)
			if lerr != nil {
				av.Logger().Error(lerr, "live-view: poll iteration failed; will retry",
					"artifact", b.artifactID, "sessionRef", b.sessionRef())
				continue
			}
			msg, changeKey, berr := buildLiveMessage(av, b, "revision", revs, contentRender, ready)
			if berr != nil {
				// SignContentToken failed — a real misconfiguration.
				av.Logger().Error(berr, "live-view: poll iteration failed; will retry",
					"artifact", b.artifactID, "sessionRef", b.sessionRef())
				continue
			}
			if changeKey == "" {
				// A ready content render with zero source revisions — impossible
				// in practice, but buildLiveMessage refuses to key on it, so
				// there is nothing to push.
				continue
			}
			if werr := writeLiveMessage(conn, msg); werr != nil {
				return // client gone
			}
			lastKey = changeKey
		case snap, ok := <-statusCh:
			if !ok {
				statusCh = nil // watcher closed; stop selecting on it
				continue
			}
			s := snap
			if werr := writeLiveMessage(conn, liveMessage{Type: "status", Status: &s}); werr != nil {
				return // client gone
			}
		case m, ok := <-msgCh:
			if !ok {
				msgCh = nil // watcher closed; stop selecting on it
				continue
			}
			mm := m
			if werr := writeLiveMessage(conn, liveMessage{Type: "message", Message: &mm}); werr != nil {
				return // client gone
			}
		}
	}
}

// writeLiveMessage writes one frame under a deadline. A non-nil return means
// the connection is dead and the caller should stop.
func writeLiveMessage(conn *websocket.Conn, msg liveMessage) error {
	if err := conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(msg)
}

// buildLiveMessage assembles one frame from an already-resolved revision list
// and content render. It returns the message, a change-key (the content render
// name — "" when nothing is servable yet), and any error (which the caller
// treats as transient). The change-key is the CONTENT render name, not the
// source revision id, so a bundled-only artifact's generating→ready transition
// pushes a frame even when the source revision id is unchanged. RenderName
// never appears in the returned message.
//
// revs and contentRender/ready are the caller's to resolve, so the connect path
// can reuse the binding probe's revision list and the steady-state poll can
// decide from the content render alone whether a list is worth fetching at all.
// All URLs are minted against b's RESOLVED session, never a requested one.
//
// Accepted limitation: only the LATEST source revision has a generated preview
// (artifact_offer_view generates for latest), so pinning an older bundled-only
// revision shows the generating/empty state.
func buildLiveMessage(av Deps, b viewBinding, msgType string, revs []RevisionMeta, contentRender string, ready bool) (liveMessage, string, error) {
	items := make([]liveRevisionItem, 0, len(revs))
	for _, rv := range revs {
		items = append(items, liveRevisionItem{
			Seq:               rv.Seq,
			RevisionID:        rv.RevisionID,
			ChangeDescription: rv.ChangeDescription,
			CreatedAt:         rv.CreatedAt,
			Tags:              rv.Tags,
		})
	}

	if !ready || contentRender == "" || len(revs) == 0 {
		// Nothing servable yet (not rendered, or a bundled-only preview still
		// generating): send the sidebar but no Current, change-key "". The
		// len(revs)==0 guard also keeps the newest lookup below panic-safe —
		// never index into an empty slice from a poller goroutine.
		return liveMessage{Type: msgType, Revisions: items}, "", nil
	}
	hostURL, contentURL, err := urlsFor(av, b.ns, b.sess, contentRender, b.artifactID)
	if err != nil {
		return liveMessage{}, "", err
	}
	// newest is still used for Current.Seq / RevisionID (the SOURCE revision the
	// user is looking at); the change-key is the CONTENT render name.
	newest := revs[len(revs)-1]
	return liveMessage{
		Type: msgType,
		Current: liveCurrent{
			Seq:        newest.Seq,
			RevisionID: newest.RevisionID,
			HostURL:    hostURL,
			ContentURL: contentURL,
		},
		Revisions: items,
	}, contentRender, nil
}
