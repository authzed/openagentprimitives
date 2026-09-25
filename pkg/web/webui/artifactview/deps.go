package artifactview

import (
	"context"

	"github.com/go-logr/logr"
)

// Deps is the artifact viewer's dependency interface. internal/cmd/webd's concrete deps
// value implements it; the WebUI casts webui.Deps to this.
type Deps interface {
	VerifyLink(raw string) (artifactID, sessionRef, backLink string, err error)
	CheckView(ctx context.Context, artifactID, subject string) (bool, error)
	// CheckInteract is agentsession#interact — session MEMBERSHIP, which
	// CheckView is a strict superset of (`view = parent->interact +
	// platform->view_audit`, pkg/authz/spicedb/schema/schema.zed). It gates the
	// SESSION-scoped mirrors only (WatchSessionStatus / WatchMessages): holding
	// view_audit lets a platform admin look at any artifact, but that is not a
	// licence to read the session's conversation, plan and notifications as if
	// they were a participant. The revision stream stays gated on CheckView
	// alone, so the admin live-view keeps working and only the mirrors go dark.
	// An error is fail-closed (mirrors off), never read as a grant.
	CheckInteract(ctx context.Context, ns, sess, subject string) (bool, error)
	// ArtifactMeta returns the artifact head's display name + description (the
	// agent-supplied title/summary). Both empty (with nil error) when the head
	// has no name/description — the shell falls back to the artifact id.
	ArtifactMeta(ctx context.Context, ns, sess, artifactID string) (name, description string, err error)
	// ChannelKind returns the AgentSession's input-channel kind (e.g. "slack"),
	// used to pick the thread-link icon in the live-view toolbar. Returns ""
	// (with nil error) when the kind is unknown.
	ChannelKind(ctx context.Context, ns, sess string) (string, error)
	// SessionViews returns the granted session_views.interactions for the
	// session's AgentClass — the interaction kinds a browser view of this session
	// may submit (e.g. "user_message"). Nil when the capability is absent,
	// inactive, or malformed, and the shell then renders a read-only viewer.
	// UX ONLY: the real gate is the /interact endpoint's own session_views check,
	// which this list cannot bypass — it only avoids rendering a control the
	// server would refuse. Read from the AgentClass's spec (never status), so a
	// capability revocation is reflected immediately.
	SessionViews(ctx context.Context, ns, sess string) []string
	ResolveRender(ctx context.Context, ns, sess, artifactID string) (renderName string, err error)
	// RenderKind resolves renderName's ArtifactRender.spec.kind, which
	// rewriteArtifactRefs uses to dispatch generically to the kind's registered
	// channelassets.RefRewriter. A non-nil error means the kind itself could not
	// be resolved (e.g. a K8s lookup failure); the caller logs it and serves the
	// content untransformed rather than failing the request.
	RenderKind(ctx context.Context, ns, sess, renderName string) (string, error)
	// ContentRender returns the render whose bytes should be FRAMED for this
	// artifact, and ready=false when there is no servable content yet (a
	// bundled-only artifact whose preview is still generating). For a normal
	// (standalone) artifact this is its newest render; for a bundled-only kind
	// (svg/css) it is the internal html preview child's render — its raw bytes
	// are never framed.
	ContentRender(ctx context.Context, ns, sess, artifactID string) (renderName string, ready bool, err error)
	// PreviewChildRender returns the internal html preview child's render for a
	// SPECIFIC source revision, ready=false when that revision's preview hasn't
	// been generated yet. Used to frame an older bundled-only revision's preview
	// when it is pinned in the viewer.
	PreviewChildRender(ctx context.Context, ns, sess, revID string) (renderName string, ready bool, err error)
	// RenderIsBundledOnly reports whether the artifact's kind is delivered
	// bundled-only (svg/css) — i.e. its raw render bytes must never be framed,
	// only its internal html preview child. The revision handler uses this to
	// decide whether pinning an OLDER revision (which has no generated preview)
	// may serve the render directly (standalone) or must show the generating
	// state (bundled-only). A lookup failure or unknown kind reports false (the
	// safe default for standalone kinds; bundled-only kinds are an explicit set).
	RenderIsBundledOnly(ctx context.Context, ns, sess, artifactID string) bool
	SignContentToken(ns, sess, renderName, artifactID string) (string, error)
	VerifyContentToken(token string) (ns, sess, renderName string, err error)
	// ResolveAssetURL resolves an `artifact:HANDLE` reference in a primary's
	// live-view HTML to a token-gated same-origin asset URL
	// ("/artifacts/a/?ct=..."), SCOPED to the primary's own (ns, sess): the handle
	// is looked up only within that session's artifact store, never globally, so
	// it can never resolve to another session's secondary. ok=false with a nil
	// error means the handle does not resolve within (ns, sess) — unknown, or
	// living only in a different session — and the caller must drop the
	// referencing attribute rather than treat it as fatal. A non-nil error means
	// resolution itself failed; also non-fatal, but worth logging.
	ResolveAssetURL(ctx context.Context, ns, sess, handle string) (url string, ok bool, err error)
	// VerifyAssetToken verifies a token minted by ResolveAssetURL and returns
	// the secondary's own (ns, sess, renderName). It rejects a token minted
	// for the /content route (see contenttoken.Claims.Kind) — a content token
	// can never be replayed here.
	VerifyAssetToken(token string) (ns, sess, renderName string, err error)
	FetchRender(ctx context.Context, ns, sess, renderName string) ([]byte, string, error)
	// FetchRenderBundle is FetchRender's sibling for the direct-download path
	// ONLY — never contentHandler/the live-view, which must keep getting raw,
	// inert bytes via FetchRender. It hits the operator's smart-passthrough bundle
	// route (pkg/memory/httpsrv's serveBundle) on the same (ns, sess, renderName)
	// triple, returning a self-contained ZIP whenever the primary is html with at
	// least one resolvable `artifact:HANDLE` reference and the same bytes+MIME
	// FetchRender would have otherwise. The caller need not know which it got: it
	// always sets Content-Type/Content-Disposition from the returned mime.
	FetchRenderBundle(ctx context.Context, ns, sess, renderName string) (data []byte, mime string, err error)
	// ServeTransform applies the artifact kind's live-view serve transform to the
	// fetched render bytes, dispatching to the kind's registered renderer.
	// Returns content unchanged for kinds with no transform (all kinds today —
	// the artifact is served inert). The content handler calls this
	// unconditionally so it stays type-agnostic.
	ServeTransform(ctx context.Context, ns, sess, renderName string, content []byte) []byte
	ListRevisions(ctx context.Context, ns, sess, artifactID string) ([]RevisionMeta, error)
	// WatchSessionStatus streams the session's presentation status (active/paused
	// + cause, the plan, the agent's latest status message). The channel delivers
	// an initial snapshot seeded from the AgentSession phase, then an updated one
	// on each out.plan_update / out.notification event, and closes when ctx is
	// cancelled. A nil channel with a nil error means status streaming is
	// unavailable (NATS not wired) and the live-view degrades to revisions-only.
	WatchSessionStatus(ctx context.Context, ns, sess string) (<-chan StatusSnapshot, error)
	// WatchMessages streams the session's OUTBOUND conversation for the chat
	// mirror: the agent's replies (respond_to_user) and the user's own sends
	// (user_echo). Read-only — webd subscribes, writes nothing, and holds no
	// signing key. A nil channel with a nil error means messaging is unavailable
	// (NATS not wired), degrading to no chat stream like WatchSessionStatus.
	WatchMessages(ctx context.Context, ns, sess string) (<-chan MirrorMessage, error)
	TrustedOrigin() string
	SandboxBaseURL() string
	Logger() logr.Logger
}

// StatusSnapshot mirrors the session's presentation status for the live-view.
type StatusSnapshot struct {
	Phase         string           `json:"phase"`         // AgentSession phase, e.g. "Running"
	Paused        bool             `json:"paused"`        // agent is waiting on a human
	PauseCause    string           `json:"pauseCause"`    // why it paused; "" when not paused
	Plan          []PlanStatusItem `json:"plan"`          // ordered steps; empty when the session has no plan
	StatusMessage string           `json:"statusMessage"` // agent's latest one-line progress note
}

// MirrorMessage is one chat line for the live-view chat panel.
type MirrorMessage struct {
	Role   string `json:"role"`          // "agent" | "user"
	Text   string `json:"text"`          // the message body as sent
	Author string `json:"author"`        // display string; "" when unknown
	Via    string `json:"via,omitempty"` // surface the message came in through; omitted when native
	At     string `json:"at"`            // RFC3339, stamped by webd on receive
}

// PlanStatusItem is one plan step shown in the live-view status panel.
type PlanStatusItem struct {
	Label  string `json:"label"`  // step text as the agent wrote it
	Status string `json:"status"` // pending | in_progress | done | error
}

// RevisionMeta is one revision of an artifact.
type RevisionMeta struct {
	Seq               int      // 1-based position, oldest first
	RevisionID        string   // opaque id; the ?rev value for pinning
	RenderName        string   // ArtifactRender CR name — server-side only, never sent to a browser
	ChangeDescription string   // agent's summary of this revision; may be empty
	CreatedAt         string   // RFC3339
	Tags              []string // agent-supplied labels; empty when untagged
	Filename          string   // the revision's suggested download name; "" when the renderer offered none
	Size              int64    // rendered bytes
	MIME              string   // rendered content type
}
