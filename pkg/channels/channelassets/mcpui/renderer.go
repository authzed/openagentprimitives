// Package mcpui is the identity renderer for MCP-UI / MCP Apps interactive
// widgets (tool.Result.UIResource — a ui:// resource an MCP tool returns
// instead of plain content). Unlike every other channelassets.Renderer, mcpui
// does NOT sanitize or transform its input: the widget HTML is UNTRUSTED,
// MCP-server-authored, and MUST run its own JavaScript, because that is how it
// implements the postMessage bridge the protocol requires. A bluemonday pass
// or a fixed CSP <meta> injection (what the html kind does) would break every
// widget — the widget is SUPPOSED to execute script.
//
// Safety is NOT delivered by this renderer and must never be added here. It
// comes entirely from WHERE the persisted bytes are served: the sandbox-origin
// route in pkg/web/webui/sessionview, which frames the content in an iframe
// sandbox="allow-scripts" (deliberately WITHOUT allow-same-origin) under a
// per-widget Content-Security-Policy built from the resource's own
// _meta.ui.csp. That is the OPPOSITE isolation posture from the generic
// artifact live-view host (pkg/web/webui/artifactview), whose inner frame is
// sandbox="allow-same-origin" WITHOUT allow-scripts, on the assumption that
// every renderer's OUTPUT is already script-inert.
//
// This renderer exists ONLY so a widget's bytes can ride the ArtifactRender CR
// → operator → artifactstore persistence pipeline (the CR-create+poll+finalize
// dance in pkg/agent/tool/meta/artifact_prepare.go, mirrored by the runner's
// applyUIResource in pkg/agent/runner/loop_uiresource.go). It is deliberately
// kept OUT of the agent-facing artifact_prepare surface:
//
//   - SupportsLiveView() is false, so artifact_offer_view — the tool that mints
//     a browser live-view link — refuses any mcpui-kind artifact outright.
//   - Delivery() is BundledOnly, so a path that did reach the generic
//     artifactview page would look for a PreviewComposer-generated preview
//     child (mcpui implements none) rather than framing the raw bytes.
//   - MOST IMPORTANTLY: AgentSelectable() is false (below), so the agent's own
//     production menu (AvailableAssetKinds) skips this kind no matter which
//     process it is registered in — that is the real gate. Only
//     applyUIResource creates ArtifactRender{Kind:"mcpui"} CRs, straight from
//     an MCP tool's structured UIResource field, never through the
//     artifact_prepare meta tool. This package is still blank-imported ONLY by
//     internal/cmd/operator, the artifactrender controller's registry.ByKind
//     dispatch target for that CR, and NOT by internal/cmd/runner or
//     internal/cmd/webd the way css/html/image/svg are — defence in depth, a
//     second reason an agent process can never reach kind="mcpui", not the
//     reason. The blank-import site in internal/cmd/operator/main.go carries
//     the same note.
package mcpui

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

const (
	// Generous next to a typical widget document (well under 100 KiB): identity
	// passthrough does no transformation work, so there is no processing-cost
	// reason to cap tighter. Purely a sanity ceiling.
	maxInput  = 1 << 20 // 1 MiB
	maxOutput = 1 << 20 // 1 MiB
)

// Renderer is the identity mcpui renderer. Stateless; safe to share across
// goroutines.
type Renderer struct{}

// New constructs the mcpui renderer.
func New() *Renderer { return &Renderer{} }

func (Renderer) Kind() string { return "mcpui" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}

// Delivery is BundledOnly — see the package doc. mcpui content must never be
// the top-level document of the generic artifact live-view.
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryBundledOnly }

func (Renderer) MaxInputSize() int64   { return maxInput }
func (Renderer) MaxOutputSize() int64  { return maxOutput }
func (Renderer) OutputMIMEs() []string { return []string{"text/html"} }
func (Renderer) InputMIMEs() []string  { return []string{"text/html"} }

// SupportsLiveView is false: mcpui widgets surface through the session-view
// page's own status.activeWidgets + widget_offer signal, never through
// artifact_offer_view or the generic artifact live-view. Returning false here
// is what makes artifact_offer_view refuse an mcpui artifact outright.
func (Renderer) SupportsLiveView() bool { return false }

// Operator-only: the runner creates a widget render itself (loop_uiresource);
// an agent never picks this kind.
func (Renderer) AgentSelectable() bool { return false }

// Instructions is empty: mcpui is never meant to be agent-selectable via
// artifact_prepare (see the package doc), so there is no agent-facing
// authoring guidance to give.
func (Renderer) Instructions() string { return "" }

// Render is the identity transform: output bytes equal input bytes,
// verbatim. NO sanitize (bluemonday), NO CSP <meta> injection — see the
// package doc for why. The only hard error is oversized input; ctx
// cancellation is respected like every other renderer in this package tree.
func (r *Renderer) Render(ctx context.Context, in channelassets.Input) (channelassets.Output, error) {
	if int64(len(in.Payload)) > maxInput {
		return channelassets.Output{}, fmt.Errorf("input %d bytes > max %d", len(in.Payload), maxInput)
	}
	if err := ctx.Err(); err != nil {
		return channelassets.Output{}, err
	}
	filename := in.Filename
	if filename == "" {
		filename = "widget.html"
	}
	return channelassets.Output{
		Bytes:    in.Payload,
		MIME:     "text/html",
		Filename: filename,
		AltText:  in.AltText,
	}, nil
}

// ServeTransform is a no-op: mcpui content is served exactly as rendered
// (identity through the whole pipeline). Safety is never delivered through
// this hook — see the package doc for where it actually lives.
func (Renderer) ServeTransform(content []byte) []byte { return content }
