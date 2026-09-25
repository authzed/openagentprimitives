// Package channelassets defines the renderer plug-in surface for
// agent-produced visual artifacts. Renderers register at init() time via
// pkg/channels/channelassets/registry; the artifactrender controller
// dispatches to the registered kind at reconcile time.
//
// Deliberately free of Kubernetes API dependencies, so renderer plug-ins stay
// unit-testable in isolation.
package channelassets

import (
	"context"
	"errors"
	"fmt"
)

// ErrMalformedPayload marks a Render failure caused by a structurally broken
// input payload (truncated mid-tag, or fully entity-encoded) rather than an
// internal renderer fault. The artifactrender controller maps it to
// ReasonArtifactRenderMalformedInput; callers use errors.Is to detect it.
var ErrMalformedPayload = errors.New("channelassets: malformed payload")

// Renderer is the per-kind plug-in. One impl per renderer Kind.
type Renderer interface {
	// Kind matches ArtifactRender.spec.kind. Pattern: lowercase,
	// dash-separated. Examples: "html", "slide-deck", "chart-bar".
	Kind() string

	// ExecutionMode declares where Render runs: InProcess inside the
	// operator's artifactrender controller, or PodSpawn in a controller-spawned
	// renderer Pod it watches to completion. Every registered kind is InProcess;
	// PodSpawn is reserved for a heavy renderer (Chromium for HTML→PDF).
	ExecutionMode() ExecutionMode

	// Delivery declares whether this kind may be served as a top-level
	// browser document. Standalone kinds are previewed by serving their
	// rendered bytes directly; BundledOnly kinds are NEVER the top-level
	// document — their preview is composed as HTML and rendered by the
	// html kind. BundledOnly does NOT block channel file delivery (that is
	// governed by asset capability + sanitization).
	Delivery() DeliveryMode

	// MaxInputSize is the per-renderer cap on input bytes. The operator
	// also enforces an absolute 10 MiB cap regardless.
	MaxInputSize() int64

	// MaxOutputSize is the per-renderer cap on rendered output bytes.
	// Operator's absolute 10 MiB cap also applies.
	MaxOutputSize() int64

	// OutputMIMEs lists the MIME types Render may produce (most kinds: one).
	// None may be a denied MIME unless Delivery()==BundledOnly; registry
	// panics otherwise.
	OutputMIMEs() []string

	// InputMIMEs lists accepted input MIMEs. Informational — bytes are
	// bytes — but used for capability negotiation and schema generation
	// on the runner side.
	InputMIMEs() []string

	// SupportsLiveView reports whether this kind's output can be shown in the
	// live-view, a browser viewer that re-renders on each new revision. False
	// also makes artifact_offer_view refuse the kind outright.
	SupportsLiveView() bool

	// AgentSelectable reports whether an agent may PRODUCE this kind through
	// artifact_prepare. False for a kind only the platform creates renders of
	// — the workshop draft (oap) and the MCP-UI widget (mcpui) — which the
	// operator still renders and every route still serves, but which never
	// appears on the agent's menu (AvailableAssetKinds skips it).
	AgentSelectable() bool

	// Instructions returns this kind's payload-authoring guidance for the
	// agent (the html kind's allowed-vs-stripped rules, say). It lives with
	// the kind, never in the runner's generic prompt, so a new kind brings its
	// own. "" when the kind needs no guidance.
	Instructions() string

	// Render is called by the artifactrender controller (InProcess mode)
	// or by the spawned renderer Pod's main (PodSpawn mode). The call
	// shape is identical. Ctx carries the reconcile / pod deadline;
	// renderers must respect cancellation.
	Render(ctx context.Context, in Input) (Output, error)

	// ServeTransform post-processes this kind's rendered bytes for live-view
	// framing. Most kinds serve inert content as-is (a no-op). Called
	// unconditionally by the content handler so it stays type-agnostic.
	ServeTransform(content []byte) []byte
}

// ExecutionMode is the closed-enum location for Render.
type ExecutionMode int

const (
	ExecutionModeInProcess ExecutionMode = iota
	ExecutionModePodSpawn
)

// String renders a stable label for status messages and logs.
func (m ExecutionMode) String() string {
	switch m {
	case ExecutionModeInProcess:
		return "InProcess"
	case ExecutionModePodSpawn:
		return "PodSpawn"
	default:
		return "Unknown"
	}
}

// DeliveryMode is the closed-enum delivery posture for a renderer kind.
type DeliveryMode int

const (
	DeliveryStandalone DeliveryMode = iota
	DeliveryBundledOnly
)

func (d DeliveryMode) String() string {
	switch d {
	case DeliveryStandalone:
		return "Standalone"
	case DeliveryBundledOnly:
		return "BundledOnly"
	default:
		return "Unknown"
	}
}

// Input is what the controller hands to Render.
type Input struct {
	// Payload is the raw bytes, already inflated from inline base64 or
	// fetched via artifactstore.Get when the CR used PayloadRef.
	Payload []byte

	// Filename is a delivery hint (the renderer may rewrite — e.g. the
	// HTML renderer ensures a .html extension).
	Filename string

	// AltText for accessibility / channel-side preview.
	AltText string
}

// Warning is a single, structured sanitizer/transform finding. It is the
// agent feedback loop: the agent reads these to learn exactly what was
// removed and adjust the source before re-rendering.
//
// Action semantics:
//   - "unwrapped": a disallowed TAG was dropped but its children kept, so any
//     class/id hook ON that tag is gone (the layout class no longer matches).
//   - "removed": a disallowed element AND its content were dropped.
//   - "stripped": an attribute or a CSS construct was removed.
//   - "kept": content was left in place but flagged (e.g. CSS that could not be
//     parsed and was emitted as-is, backstopped by the CSP).
type Warning struct {
	Kind   string `json:"kind"`           // which surface was touched: "tag" | "attr" | "css"
	Name   string `json:"name"`           // the element / attribute / CSS construct by name
	Action string `json:"action"`         // what happened to it: "unwrapped" | "removed" | "stripped" | "kept"
	Count  int    `json:"count"`          // occurrences affected in this payload; always >= 1
	Note   string `json:"note,omitempty"` // hint on how to fix the source; empty when the action speaks for itself
}

// String renders a Warning as a single stable human-readable line.
func (w Warning) String() string {
	var s string
	switch w.Kind {
	case "tag":
		s = fmt.Sprintf("%s %d <%s> tag(s)", w.Action, w.Count, w.Name)
	case "attr":
		s = fmt.Sprintf("%s %s from %d element(s)", w.Action, w.Name, w.Count)
	case "css":
		s = fmt.Sprintf("%s %d CSS %s", w.Action, w.Count, w.Name)
	default:
		s = fmt.Sprintf("%s %d %s", w.Action, w.Count, w.Name)
	}
	if w.Note != "" {
		s += ": " + w.Note
	}
	return s
}

// Output is what Render returns.
type Output struct {
	Bytes    []byte
	MIME     string    // typically one of Renderer.OutputMIMEs(); rare per-render variation allowed
	Filename string    // possibly rewritten; defaults to Input.Filename
	AltText  string    // possibly enriched; defaults to Input.AltText
	Warnings []Warning // structured sanitizer/transform findings — agent feedback loop
}

// deniedOutputMIMEs is the hard deny-list, applied both at registry
// registration and at wildcard capability matching, so those two can never
// disagree about what may be served as a top-level browser document.
//
// image/svg+xml — SVG can carry <script>, <foreignObject> wrapping arbitrary
// HTML, and xlink:href to remote resources, so even sanitized SVG is a known
// bypass surface. Denied means: no Standalone renderer may emit it (registry
// panics), and an "asset:image/*" wildcard never matches it. A BundledOnly
// renderer may still produce it (the svg kind does), and a channel kind may
// still carry it by naming "asset:image/svg+xml" explicitly — both are
// deliberate opt-ins, not wildcard reach.
var deniedOutputMIMEs = map[string]struct{}{
	"image/svg+xml": {},
}

// IsDeniedOutputMIME reports whether mime is in the hard deny-list.
// Used by the registry (panic on registration), by the capability
// matcher (wildcard does not match denied), and by tests.
func IsDeniedOutputMIME(mime string) bool {
	_, ok := deniedOutputMIMEs[mime]
	return ok
}

// RefRewriter is implemented by renderer kinds whose content can embed
// artifact:HANDLE references to secondary artifacts — html's img[src]/
// link[href] today. Callers (the bundler in pkg/memory/httpsrv/bundle.go, the
// live-view resolver in pkg/web/webui/artifactview/rewrite.go) never parse the
// content themselves; a kind with no reference concept simply does not
// implement this, which callers read as "nothing to rewrite", not an error.
// Implementations are PURE — no store or K8s access; resolve is the caller's
// authz'd hook.
type RefRewriter interface {
	// RewriteRefs rewrites each artifact:HANDLE reference in content via
	// resolve(handle) -> (replacement, keep). keep==false DROPS the reference
	// rather than failing the document; everything else is untouched. resolve
	// is memoized per distinct handle (both the replacement and the keep
	// decision), because a caller's resolve may have side effects — the
	// bundler fetches and records secondary bytes on first resolution.
	//
	// Returns the rewritten content plus the ordered, de-duplicated handles
	// that were kept, which the caller uses as the dependency set to bundle.
	// No refs returns (content, nil). Content that fails to parse is likewise
	// returned unchanged with a nil list — the degrade-don't-fail posture
	// every renderer transform here uses.
	RewriteRefs(content []byte, resolve func(handle string) (replacement string, keep bool)) (out []byte, resolved []string)
}

// PreviewComposer is implemented by BundledOnly kinds. PreviewHTML composes a
// complete HTML preview document for content (the kind's already-rendered
// bytes). The html kind sanitizes + CSPs the result before the live-viewer
// serves it, so the returned HTML must use only constructs html allows — no
// <script>; inline <style>/class/style= and same-document #fragment links are
// fine. gen is a runner-supplied secondary-LLM callback for kinds wanting
// generated sample markup; it is nil-safe, and a nil gen or a gen error means
// "use a deterministic fallback".
type PreviewComposer interface {
	PreviewHTML(ctx context.Context, content []byte, gen MarkupGenerator) ([]byte, error)
}

// MarkupGenerator returns model-generated HTML body markup for the given
// instruction. The caller sanitizes the result before use.
type MarkupGenerator func(ctx context.Context, instruction string) (string, error)
