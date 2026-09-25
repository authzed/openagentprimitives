package artifactview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// liveFakeDeps implements Deps via per-behavior func fields for the websocket
// handler tests. Only the fields the live handler exercises need defaults; the
// rest stay nil and would panic if called (none are on the live path).
type liveFakeDeps struct {
	verifyLink       func(raw string) (string, string, string, error)
	checkView        func(ctx context.Context, artifactID, subject string) (bool, error)
	signContentToken func(ns, sess, renderName, artifactID string) (string, error)
	sandboxBaseURL   func() string
	trustedOrigin    func() string
	checkInteract    func(ctx context.Context, ns, sess, subject string) (bool, error)
	listRevisions    func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error)
	contentRender    func(ctx context.Context, ns, sess, artifactID string) (string, bool, error)
	watchStatus      func(ctx context.Context, ns, sess string) (<-chan StatusSnapshot, error)
	watchMessages    func(ctx context.Context, ns, sess string) (<-chan MirrorMessage, error)
	logger           logr.Logger
}

func (f *liveFakeDeps) VerifyLink(raw string) (string, string, string, error) {
	return f.verifyLink(raw)
}
func (f *liveFakeDeps) CheckView(ctx context.Context, artifactID, subject string) (bool, error) {
	return f.checkView(ctx, artifactID, subject)
}
func (f *liveFakeDeps) ResolveRender(ctx context.Context, ns, sess, artifactID string) (string, error) {
	return "", nil
}

// CheckInteract defaults to "a session participant" — the ordinary viewer every
// other test in this file models. A test exercising the platform-admin case
// (artifact#view via view_audit, no agentsession#interact) sets f.checkInteract.
func (f *liveFakeDeps) CheckInteract(ctx context.Context, ns, sess, subject string) (bool, error) {
	if f.checkInteract == nil {
		return true, nil
	}
	return f.checkInteract(ctx, ns, sess, subject)
}

// ContentRender defaults to the standalone behavior: frame the newest revision's
// render (from listRevisions), ready=true. A test exercising the bundled-only
// generating→ready transition sets f.contentRender directly.
func (f *liveFakeDeps) ContentRender(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
	if f.contentRender != nil {
		return f.contentRender(ctx, ns, sess, artifactID)
	}
	revs, err := f.listRevisions(ctx, ns, sess, artifactID)
	if err != nil {
		return "", false, err
	}
	if len(revs) == 0 {
		return "", false, nil
	}
	return revs[len(revs)-1].RenderName, true, nil
}

// RenderIsBundledOnly is not on the live ws path; default to standalone.
func (f *liveFakeDeps) RenderIsBundledOnly(ctx context.Context, ns, sess, artifactID string) bool {
	return false
}

// PreviewChildRender is not on the live ws path; default to "not generated".
func (f *liveFakeDeps) PreviewChildRender(ctx context.Context, ns, sess, revID string) (string, bool, error) {
	return "", false, nil
}
func (f *liveFakeDeps) ArtifactMeta(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
	return "", "", nil
}
func (f *liveFakeDeps) ChannelKind(ctx context.Context, ns, sess string) (string, error) {
	return "", nil
}
func (f *liveFakeDeps) SessionViews(ctx context.Context, ns, sess string) []string {
	return nil // the live-socket tests don't exercise session_views
}
func (f *liveFakeDeps) SignContentToken(ns, sess, renderName, artifactID string) (string, error) {
	return f.signContentToken(ns, sess, renderName, artifactID)
}
func (f *liveFakeDeps) VerifyContentToken(token string) (string, string, string, error) {
	return "", "", "", nil
}

// ResolveAssetURL / VerifyAssetToken are not on the live ws path (asset refs
// are resolved from /content, not the status/chat socket); harmless stubs.
func (f *liveFakeDeps) ResolveAssetURL(ctx context.Context, ns, sess, handle string) (string, bool, error) {
	return "", false, nil
}

// RenderKind: this suite exercises the websocket status/plan stream, never
// the content-serving asset-ref rewrite path — a harmless stub.
func (f *liveFakeDeps) RenderKind(ctx context.Context, ns, sess, renderName string) (string, error) {
	return "", nil
}
func (f *liveFakeDeps) VerifyAssetToken(token string) (string, string, string, error) {
	return "", "", "", nil
}
func (f *liveFakeDeps) FetchRender(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return nil, "", nil
}

// FetchRenderBundle: this suite exercises the websocket status/plan stream,
// never the download path — a harmless stub matching FetchRender.
func (f *liveFakeDeps) FetchRenderBundle(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return nil, "", nil
}
func (f *liveFakeDeps) ListRevisions(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
	return f.listRevisions(ctx, ns, sess, artifactID)
}
func (f *liveFakeDeps) ServeTransform(ctx context.Context, ns, sess, renderName string, content []byte) []byte {
	return content
}
func (f *liveFakeDeps) WatchSessionStatus(ctx context.Context, ns, sess string) (<-chan StatusSnapshot, error) {
	if f.watchStatus == nil {
		return nil, nil
	}
	return f.watchStatus(ctx, ns, sess)
}
func (f *liveFakeDeps) WatchMessages(ctx context.Context, ns, sess string) (<-chan MirrorMessage, error) {
	if f.watchMessages == nil {
		return nil, nil // most live-socket tests in this file don't exercise the chat mirror stream
	}
	return f.watchMessages(ctx, ns, sess)
}
func (f *liveFakeDeps) TrustedOrigin() string  { return f.trustedOrigin() }
func (f *liveFakeDeps) SandboxBaseURL() string { return f.sandboxBaseURL() }
func (f *liveFakeDeps) Logger() logr.Logger    { return f.logger }

// liveTestAV returns a happy-path Deps for the websocket handler. trustedOrigin
// is read through a closure so the test can set it to the test server's URL
// AFTER the server starts (the CheckOrigin gate compares against it). Each test
// overrides only the fields it exercises.
func liveTestAV(trustedOrigin *string) *liveFakeDeps {
	return &liveFakeDeps{
		verifyLink: func(raw string) (string, string, string, error) {
			return "artifact-x", "default/s1", "", nil
		},
		checkView: func(ctx context.Context, artifactID, subject string) (bool, error) {
			return true, nil
		},
		signContentToken: func(ns, sess, renderName, artifactID string) (string, error) {
			return "tok", nil
		},
		sandboxBaseURL: func() string { return "https://sandbox.example" },
		trustedOrigin:  func() string { return *trustedOrigin },
		listRevisions: func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
			return []RevisionMeta{
				{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", ChangeDescription: "first", CreatedAt: "t0", Tags: []string{"latest"}},
			}, nil
		},
	}
}

// dialLive starts srv hosting liveHandler(av), points trustedOrigin at it, and
// dials the ws endpoint with the trusted Origin header. It returns the open
// connection (registered for cleanup) plus the dial response.
func dialLive(t *testing.T, av Deps, trustedOrigin *string) (*websocket.Conn, *http.Response) {
	t.Helper()
	srv := httptest.NewServer(liveHandler(av))
	t.Cleanup(srv.Close)
	*trustedOrigin = srv.URL

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/?d=AA&sig=BB"
	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	require.NoError(t, err, "ws handshake must succeed for happy-path dials")
	t.Cleanup(func() { conn.Close() })
	require.NotNil(t, resp, "dial must yield an HTTP response")
	return conn, resp
}

// readLive reads one frame and unmarshals it into a liveMessage, also returning
// the raw bytes so a test can assert on the exact JSON (e.g. renderName leak).
func readLive(t *testing.T, conn *websocket.Conn) (liveMessage, []byte) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err, "read ws frame")
	var msg liveMessage
	require.NoError(t, json.Unmarshal(raw, &msg), "unmarshal ws frame")
	return msg, raw
}

func TestLive_Snapshot_EmbedsContentURLAndOmitsRenderName(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode, "handshake must succeed")
	require.NotNil(t, conn)

	msg, raw := readLive(t, conn)
	assert.Equal(t, "snapshot", msg.Type)
	assert.Equal(t, 1, msg.Current.Seq)
	assert.Equal(t, "rev-1", msg.Current.RevisionID)
	assert.Contains(t, msg.Current.HostURL, "https://sandbox.example/artifact-host?ct=tok")
	assert.Contains(t, msg.Current.ContentURL, "https://sandbox.example/content?ct=tok")

	// RenderName ("ar-1") must never leak to the browser, in any field.
	assert.NotContains(t, string(raw), "renderName", "client JSON must not carry a renderName key")
	assert.NotContains(t, string(raw), "ar-1", "render CR name must not appear in the client frame")

	require.Len(t, msg.Revisions, 1)
	assert.Equal(t, []string{"latest"}, msg.Revisions[0].Tags)
}

func TestLive_HeadChange_PushesRevisionFrame(t *testing.T) {
	prev := livePollInterval()
	setLivePollInterval(20 * time.Millisecond)
	t.Cleanup(func() { setLivePollInterval(prev) })

	var trusted string
	av := liveTestAV(&trusted)
	// The sidebar grows from 1 to 2 revisions; the content render (change-key)
	// moves from ar-1 to ar-2 in lockstep. listRevisions is stateful, so drive
	// the content render from an independent counter rather than re-listing
	// inside ContentRender (that would double-advance listRevisions' counter).
	var listCalls int32
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
		if atomic.AddInt32(&listCalls, 1) == 1 {
			return []RevisionMeta{
				{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", Tags: []string{"prev"}},
			}, nil
		}
		return []RevisionMeta{
			{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", Tags: []string{"prev"}},
			{Seq: 2, RevisionID: "rev-2", RenderName: "ar-2", Tags: []string{"latest"}},
		}, nil
	}
	var crCalls int32
	av.contentRender = func(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
		if atomic.AddInt32(&crCalls, 1) == 1 {
			return "ar-1", true, nil
		}
		return "ar-2", true, nil
	}

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	assert.Equal(t, 1, snap.Current.Seq)

	upd, raw := readLive(t, conn)
	assert.Equal(t, "revision", upd.Type)
	assert.Equal(t, 2, upd.Current.Seq)
	assert.Equal(t, "rev-2", upd.Current.RevisionID)
	assert.Contains(t, upd.Current.HostURL, "/artifact-host?ct=tok")
	assert.Contains(t, upd.Current.ContentURL, "/content?ct=tok")
	assert.NotContains(t, string(raw), "ar-2", "render CR name must not leak on a revision push")
}

// TestLive_BundledOnlyGenerating_NoCurrentThenPushesWhenReady covers the
// bundled-only (svg/css) flow: while the internal html preview child is still
// generating, ContentRender returns ready=false and the live frame carries NO
// Current (no content URL is minted for the raw svg/css bytes). The sidebar
// still lists the source revision. When the preview child lands (ready=true with
// the child render), the next poll pushes a "revision" frame whose change-key is
// the content render name — even though the SOURCE revision id never changed.
func TestLive_BundledOnlyGenerating_NoCurrentThenPushesWhenReady(t *testing.T) {
	prev := livePollInterval()
	setLivePollInterval(20 * time.Millisecond)
	t.Cleanup(func() { setLivePollInterval(prev) })

	var trusted string
	av := liveTestAV(&trusted)
	// One stable source revision the whole time (a single svg revision).
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
		return []RevisionMeta{
			{Seq: 1, RevisionID: "rev-1", RenderName: "ar-svg-1", Tags: []string{"latest"}},
		}, nil
	}
	// First build: preview child still generating (ready=false). Later: the child
	// render lands.
	var crCalls int32
	av.contentRender = func(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
		if atomic.AddInt32(&crCalls, 1) == 1 {
			return "", false, nil // generating
		}
		return "ar-preview-child", true, nil // child landed
	}

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// Snapshot: sidebar present, but NO Current (preview still generating). The
	// raw svg render name must never appear, nor any content URL.
	snap, snapRaw := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	require.Len(t, snap.Revisions, 1, "sidebar lists the source revision while generating")
	assert.Equal(t, "", snap.Current.HostURL, "no host URL while the preview is generating")
	assert.Equal(t, "", snap.Current.ContentURL, "no content URL while the preview is generating")
	assert.Equal(t, 0, snap.Current.Seq, "no Current while generating")
	assert.NotContains(t, string(snapRaw), "ar-svg-1", "raw svg render must never be framed/leaked")

	// Next poll: child ready → a revision frame with the content URL appears,
	// keyed on the content render name (source rev id is unchanged).
	upd, updRaw := readLive(t, conn)
	assert.Equal(t, "revision", upd.Type)
	assert.Equal(t, 1, upd.Current.Seq, "Current reflects the source revision the user sees")
	assert.Equal(t, "rev-1", upd.Current.RevisionID)
	assert.Contains(t, upd.Current.HostURL, "/artifact-host?ct=tok", "host URL points at the preview child render")
	assert.Contains(t, upd.Current.ContentURL, "/content?ct=tok", "content URL points at the preview child render")
	assert.NotContains(t, string(updRaw), "ar-preview-child", "child render CR name must not leak")
	assert.NotContains(t, string(updRaw), "ar-svg-1", "raw svg render must never leak")
}

func TestLive_StatusFrame_MirrorsSessionStatus(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	av.watchStatus = func(ctx context.Context, ns, sess string) (<-chan StatusSnapshot, error) {
		ch := make(chan StatusSnapshot, 1)
		ch <- StatusSnapshot{
			Paused: true, PauseCause: "awaiting_reply",
			Plan:          []PlanStatusItem{{Label: "Fetch commits", Status: "in_progress"}},
			StatusMessage: "Fetching…",
		}
		return ch, nil
	}

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// Frame 1 is the revision snapshot; the status frame follows from the watch.
	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)

	st, _ := readLive(t, conn)
	require.Equal(t, "status", st.Type)
	require.NotNil(t, st.Status)
	assert.True(t, st.Status.Paused)
	assert.Equal(t, "awaiting_reply", st.Status.PauseCause)
	require.Len(t, st.Status.Plan, 1)
	assert.Equal(t, "Fetch commits", st.Status.Plan[0].Label)
	assert.Equal(t, "Fetching…", st.Status.StatusMessage)
}

// TestRunLivePoller_ForwardsOutboundMessagesAsFrames covers the chat mirror:
// WatchMessages delivers one outbound message (agent reply or user echo), and
// the poller forwards it verbatim as a "message" frame alongside the revision
// snapshot — the browser chat panel (Task 3) renders these.
func TestRunLivePoller_ForwardsOutboundMessagesAsFrames(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	av.watchMessages = func(ctx context.Context, ns, sess string) (<-chan MirrorMessage, error) {
		ch := make(chan MirrorMessage, 1)
		ch <- MirrorMessage{Role: "agent", Text: "done — reworded the hero", At: "2026-07-09T00:00:00Z"}
		close(ch)
		return ch, nil
	}

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// Frame 1 is the revision snapshot; the message frame follows from the watch.
	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)

	msg, _ := readLive(t, conn)
	require.Equal(t, "message", msg.Type)
	require.NotNil(t, msg.Message)
	assert.Equal(t, "agent", msg.Message.Role)
	assert.Contains(t, msg.Message.Text, "reworded the hero")
}

// TestLive_UnsignedParams_AdminPath covers the admin viewer ws path: the client
// forwards window.location.search (artifactId+sessionRef, no d/sig) to
// /artifact-view/ws, so the handler resolves the params directly, never calls
// VerifyLink, and still gates the upgrade on CheckView.
func TestLive_UnsignedParams_AdminPath(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	var verifyLinkCalled bool
	av.verifyLink = func(raw string) (string, string, string, error) {
		verifyLinkCalled = true
		return "", "", "", assert.AnError
	}
	var gotArtifactID string
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		gotArtifactID = artifactID
		return true, nil
	}

	srv := httptest.NewServer(liveHandler(av))
	t.Cleanup(srv.Close)
	trusted = srv.URL

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/?artifactId=artifact-x&sessionRef=default%2Fs1"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	require.NoError(t, err, "unsigned admin params must complete the ws handshake")
	t.Cleanup(func() { conn.Close() })
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	assert.False(t, verifyLinkCalled, "the unsigned path must not call VerifyLink")
	assert.Equal(t, "artifact-x", gotArtifactID, "CheckView runs with the query artifact id")
}

// mirrorRecorder records the (ns, sess) each session-scoped watcher is opened
// on, so a test can assert WHICH session's live data a socket subscribed to —
// the fact the artifact/session binding exists to constrain.
type mirrorRecorder struct {
	mu     sync.Mutex
	opened []string
}

func (m *mirrorRecorder) note(ns, sess string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened = append(m.opened, ns+"/"+sess)
}

func (m *mirrorRecorder) sessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.opened...)
}

// sessionScopedArtifact makes the fake behave like the real artifact store:
// ListRevisions resolves the head inside memory.Scope{ID: ns+"/"+sess} and
// RevisionTree hard-errors ("artifact %q not found") when the id is not in that
// scope. Artifact ids are globally unique (memory.NewID), so a pair that
// resolves here IS the artifact's owning session.
func sessionScopedArtifact(av *liveFakeDeps, ownerNs, ownerSess string) {
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
		if ns != ownerNs || sess != ownerSess {
			return nil, fmt.Errorf("artifacts: artifact %q not found", artifactID)
		}
		return []RevisionMeta{
			{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", ChangeDescription: "first", CreatedAt: "t0", Tags: []string{"latest"}},
		}, nil
	}
}

// recordMirrors wires both session-scoped watchers to record the session they
// were opened on; the message watcher also pushes one line so a leak shows up
// as a real frame on the wire rather than only as a bookkeeping entry.
func recordMirrors(av *liveFakeDeps, rec *mirrorRecorder, line string) {
	av.watchStatus = func(ctx context.Context, ns, sess string) (<-chan StatusSnapshot, error) {
		rec.note(ns, sess)
		return nil, nil
	}
	av.watchMessages = func(ctx context.Context, ns, sess string) (<-chan MirrorMessage, error) {
		rec.note(ns, sess)
		ch := make(chan MirrorMessage, 1)
		ch <- MirrorMessage{Role: "user", Text: line, At: "2026-08-09T00:00:00Z"}
		return ch, nil
	}
}

// TestLive_CrossSessionRef_Denied_NoMirrorOpened pins the artifact→session
// binding. The caller holds artifact:artifact-a (it lives in their own session
// default/session-a, and CheckView passes on it) but presents a DIFFERENT
// session in the unsigned sessionRef param. Nothing about artifact-a authorizes
// default/session-b, so the socket must be refused before the upgrade and no
// session-scoped watcher may open — otherwise the victim session's chat lines,
// plan, notifications and phase stream to a subject with no access to it.
func TestLive_CrossSessionRef_Denied_NoMirrorOpened(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	sessionScopedArtifact(av, "default", "session-a")
	rec := &mirrorRecorder{}
	recordMirrors(av, rec, "victim session private message")

	srv := httptest.NewServer(liveHandler(av))
	t.Cleanup(srv.Close)
	trusted = srv.URL

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/?artifactId=artifact-a&sessionRef=default%2Fsession-b"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	if err == nil {
		// The upgrade was accepted on an unbound (artifact, session) pair. Read
		// what it pushes so the failure names the leak rather than a bare status.
		_, _ = readLive(t, conn) // degraded snapshot (the mismatch was swallowed)
		leak, _ := readLive(t, conn)
		require.Failf(t, "cross-session live socket accepted",
			"handshake succeeded for artifact-a against default/session-b; watchers opened on %v and streamed %q",
			rec.sessions(), leak.Message.Text)
	}
	require.NotNil(t, resp, "dial must yield an HTTP response")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"an artifact that does not live in the requested session must be refused")
	assert.Empty(t, rec.sessions(), "no session-scoped watcher may open on an unbound pair")
}

func TestLive_Denied_HandshakeFailsWith403(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, nil
	}

	srv := httptest.NewServer(liveHandler(av))
	t.Cleanup(srv.Close)
	trusted = srv.URL

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/?d=AA&sig=BB"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{srv.URL}})
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	assert.Error(t, err, "denied request must not complete the ws handshake")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestLive_ViewWithoutInteract_RevisionsFlowMirrorsStayClosed covers the second
// half of the gate. CheckView is a strict superset of session membership
// (`view = parent->interact + parent->artifact_org_view + platform->view_audit`),
// so a platform admin passes it on any artifact — and any user does on an
// org-visible one. Either is a licence to look at the ARTIFACT, not to
// read the session's conversation, plan and notifications as if they were a
// participant — so the revision stream must still flow while both
// session-scoped mirrors stay unopened.
func TestLive_ViewWithoutInteract_RevisionsFlowMirrorsStayClosed(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	sessionScopedArtifact(av, "default", "s1")
	av.checkInteract = func(ctx context.Context, ns, sess, subject string) (bool, error) {
		return false, nil // artifact#view via view_audit, no agentsession#interact
	}
	rec := &mirrorRecorder{}
	recordMirrors(av, rec, "session conversation the admin may not read")

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode,
		"the artifact itself is viewable, so the socket must still open")

	snap, _ := readLive(t, conn)
	assert.Equal(t, "snapshot", snap.Type, "the artifact's own revision stream is unaffected")
	assert.Equal(t, "rev-1", snap.Current.RevisionID)
	assert.Empty(t, rec.sessions(), "neither session-scoped mirror may open without agentsession#interact")
}

// TestLive_CheckInteractError_MirrorsFailClosed pins the error arm: an
// unreachable SpiceDB must not read as a grant of session membership.
func TestLive_CheckInteractError_MirrorsFailClosed(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	av.checkInteract = func(ctx context.Context, ns, sess, subject string) (bool, error) {
		return false, assert.AnError
	}
	rec := &mirrorRecorder{}
	recordMirrors(av, rec, "session conversation")

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	assert.Empty(t, rec.sessions(), "a CheckInteract error must disable the mirrors, never open them")
}

// TestLive_MirrorsScopedToTheResolvedSession is the positive half of the
// binding on the socket: a participant on a BOUND pair does get both mirrors,
// and both are opened on the session the server resolved.
func TestLive_MirrorsScopedToTheResolvedSession(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)
	sessionScopedArtifact(av, "default", "s1")
	rec := &mirrorRecorder{}
	recordMirrors(av, rec, "a line from the artifact's own session")

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	msg, _ := readLive(t, conn)
	require.Equal(t, "message", msg.Type)
	require.NotNil(t, msg.Message)
	assert.Equal(t, "a line from the artifact's own session", msg.Message.Text)
	assert.Equal(t, []string{"default/s1", "default/s1"}, rec.sessions(),
		"both mirrors open on the session the artifact resolves in")
}

// TestLive_OversizeClientFrame_ClosesTheSocket pins the read bound. The live
// socket is push-only — the browser sends only close/pong control frames — so a
// client streaming an unbounded data frame is either broken or hostile and must
// not be able to grow the server's read buffer without limit.
func TestLive_OversizeClientFrame_ClosesTheSocket(t *testing.T) {
	var trusted string
	av := liveTestAV(&trusted)

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type, "the socket is live before the oversize write")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, make([]byte, maxLiveClientFrame+1)))

	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "a frame over the read limit must terminate the socket")
	assert.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig),
		"the server must close with 1009 message-too-big, got %v", err)
}

// TestLive_UnchangedContent_DoesNotRelistRevisions pins the steady-state cost of
// an idle viewer. The change-key IS the content render name, so an unchanged
// ContentRender means the tick has nothing to push — and listing the revisions
// to build a frame that is then discarded would double every idle viewer's
// store traffic for the entire life of the socket.
func TestLive_UnchangedContent_DoesNotRelistRevisions(t *testing.T) {
	prev := livePollInterval()
	setLivePollInterval(5 * time.Millisecond)
	t.Cleanup(func() { setLivePollInterval(prev) })

	var trusted string
	av := liveTestAV(&trusted)
	var listCalls int32
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error) {
		atomic.AddInt32(&listCalls, 1)
		return []RevisionMeta{{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", Tags: []string{"latest"}}}, nil
	}
	av.contentRender = func(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
		return "ar-1", true, nil // never changes
	}

	conn, resp := dialLive(t, av, &trusted)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	snap, _ := readLive(t, conn)
	require.Equal(t, "snapshot", snap.Type)

	// The bind probe listed once; let many poll ticks elapse and require that
	// none of them listed again.
	before := atomic.LoadInt32(&listCalls)
	time.Sleep(120 * time.Millisecond)
	assert.Equal(t, before, atomic.LoadInt32(&listCalls),
		"an unchanged content render must not cost a revision list per tick")
}
