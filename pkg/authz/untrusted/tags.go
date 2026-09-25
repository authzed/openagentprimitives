// Package untrusted holds the shared tag names for nonce-delimited untrusted-
// content envelopes, so the producer (the envelope builder) and the consumers
// (the runner's strip + the agent prompt) can never drift apart — a drift would
// silently let untrusted content escape its boundary.
package untrusted

// AnnotationsTag is the element name wrapping untrusted DOM context captured
// from a browser annotation. MUST stay in sync across the envelope builder,
// the runner strip, and the agent prompt — hence this single constant.
const AnnotationsTag = "untrusted-annotations"

// WidgetActionTag is the element name wrapping an untrusted MCP-UI widget
// action (tool name/params, prompt, link URL, intent, notify message — all
// widget/MCP-server-authored). MUST stay in sync across the envelope builder,
// the runner strip, and the agent prompt (wired by a follow-up task) — hence
// this single constant, same convention as AnnotationsTag.
const WidgetActionTag = "untrusted-widget-action"

// AttachmentTag is the element name wrapping a native attachment content
// block (an image or document a user attached). Unlike tool output, the
// payload is not text and cannot be wrapped inline, so the markers are
// emitted as sibling text blocks bracketing the native block within the same
// user message. MUST stay in sync across the hydration pass and the agent
// prompt — hence this single constant, same convention as AnnotationsTag.
const AttachmentTag = "untrusted-attachment"

// ProfileTag is the element name wrapping user-profile detail injected into
// the agent's per-turn context. Profile text (title, status, display name) is
// set by the user about themselves, so it is attacker-controlled — and unlike
// tool output, it arrives without the agent ever choosing to fetch it. Same
// single-constant convention as AnnotationsTag.
const ProfileTag = "untrusted-profile"
