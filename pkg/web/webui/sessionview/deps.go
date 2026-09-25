package sessionview

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
)

// Deps is the session-view page's dependency interface. internal/cmd/webd's concrete
// deps value implements it; the WebUI casts webui.Deps to this in Routes(). A
// cast failure (nil, or a webd umbrella missing one of these collaborators)
// means the session-view routes fail closed — see sessionview.go's Routes.
type Deps interface {
	// CheckInteract is the ONLY authorization gate this page uses — the same
	// agentsession#interact check pkg/web/webui/interact's POST handler gates a
	// send on. NEVER CheckArtifactView/CheckView: those are a strict superset
	// (parent->interact + parent->artifact_org_view + platform->view_audit)
	// that would admit platform admins — and, on an org-visible session, any
	// authenticated user — into a session-scoped view as if they were a
	// participant. An
	// error is fail-closed 500 (page, and ws pre-upgrade), never a denial.
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	// OperatorURL and MemoryToken back livemirror.ReadHistory's replay of the
	// session's durable transcript turns — the same operator memory API
	// pkg/web/webui/chat's Deps exposes for its own replay.
	OperatorURL() string
	MemoryToken() string
	// NATS is the shared connection livemirror.WatchOutbound subscribes on for
	// live outbound envelopes. nil means NATS is not configured and the live
	// handler degrades to history-only rather than panicking on a nil
	// subscriber. A concrete *nats.Conn, so the nil check is a plain pointer
	// comparison, never the typed-nil-interface trap.
	NATS() *nats.Conn
	// TrustedOrigin gates the websocket upgrade's Origin header (CSRF),
	// mirroring artifactview's liveHandler and chat's wsHandler.
	TrustedOrigin() string
	Logger() logr.Logger

	// SandboxBaseURL is the sandbox-origin base URL that /mcpui-content +
	// /mcpui-host are served from, and that the trusted page's CSP frame-src is
	// scoped to (sessionview.go's FramesSandbox:true). Mirrors
	// artifactview.Deps.SandboxBaseURL.
	SandboxBaseURL() string

	// ActiveWidgets reads the session's AgentSession.status.activeWidgets — the
	// MCP-UI widgets the runner has durably persisted so far. A Get failure is
	// returned so the caller can log and degrade to "no widgets" rather than
	// silently showing none forever.
	ActiveWidgets(ctx context.Context, ns, sess string) ([]WidgetRef, error)

	// SignWidgetToken mints the content-capability token gating /mcpui-content
	// + /mcpui-host for one widget artifact (Kind=widget). Distinct from
	// artifactview.Deps.SignContentToken's Kind, so a token minted for one
	// route can never be replayed at the other.
	SignWidgetToken(ns, sess, artifactID string) (string, error)
	// VerifyWidgetToken verifies a token minted by SignWidgetToken. A bad or
	// expired token, or one minted for a different route (content/asset),
	// is returned as an error — the caller responds 403.
	VerifyWidgetToken(token string) (ns, sess, artifactID string, err error)

	// FetchWidget fetches a persisted MCP-UI widget's raw HTML bytes VERBATIM —
	// no sanitize, no ServeTransform, because safety comes entirely from the
	// sandbox framing, never from transforming the widget's own bytes (see
	// pkg/channels/channelassets/mcpui) — plus its per-widget CSP metadata.
	// Callers MUST treat a nil WidgetMeta.CSP as "use the restrictive
	// default", never as an error.
	FetchWidget(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error)
}

// WidgetRef mirrors v1alpha1.WidgetRef, kept as a package-local DTO so this
// package stays free of the apis/v1alpha1 import; internal/cmd/webd's concrete Deps
// impl does the mapping.
type WidgetRef struct {
	// ArtifactID identifies the persisted widget within the session's scope.
	ArtifactID string
	// Tool is the MCP tool whose call produced the widget.
	Tool string
	// RendererKind is the channelassets renderer that persisted it.
	RendererKind string
}

// WidgetCSPMeta mirrors a widget's `_meta.ui.csp` (mcp-ui / MCP Apps
// protocol): the domains it declares it needs beyond the same-origin defaults.
// See buildWidgetCSP for the directive mapping.
type WidgetCSPMeta struct {
	// ConnectDomains are the origins the widget may fetch/XHR/WebSocket to.
	ConnectDomains []string
	// ResourceDomains are the origins it loads scripts, styles and images from.
	ResourceDomains []string
	// FrameDomains are the origins it may itself frame.
	FrameDomains []string
}

// WidgetMeta carries a widget's CSP-relevant metadata.
type WidgetMeta struct {
	// CSP is nil when the widget declared no `_meta.ui.csp` (or its revision
	// predates CSP persistence); buildWidgetCSP then applies its restrictive
	// default policy.
	CSP *WidgetCSPMeta
}
