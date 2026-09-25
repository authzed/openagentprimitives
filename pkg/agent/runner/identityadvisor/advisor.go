// Package identityadvisor is the isolated "should this turn run as the
// agent identity or the invoking human's identity" LLM used for
// AgentClass.spec.identityMode == "dynamic".
//
// # Security boundary
//
// This is a SECOND, ISOLATED LLM call — not a method on the primary
// agent LLM, and not the same call as the approval summarizer in
// pkg/agent/runner/approval/summarizer. It exists to produce a
// human-reviewable RECOMMENDATION, never a decision: the recommendation
// is always surfaced to a human who explicitly confirms (or overrides)
// it before the session's identity is bound. Because a human is always
// the final authority, this LLM is deliberately allowed to see context
// the summarizer is starved of — the thread transcript, the inbound
// message, participant counts — since a compromised or misleading
// recommendation can, at worst, cause the human to see a bad suggestion
// and reject it. It can never itself grant, escalate, or bind an
// identity.
//
// Contrast with the approval summarizer: that LLM is kept narrow
// (schema + args only, no chat history) because its output can
// influence whether a human approves an already-consequential action.
// This LLM is kept wide (full thread context) because its output is
// merely advisory — the identity bind still requires a separate,
// explicit human confirmation step downstream.
//
// Inputs it sees (Request below) may include UNTRUSTED content — the
// thread transcript and inbound message can carry text authored by any
// participant, including prompt-injection attempts. The system prompt
// frames all such content as USER DATA and instructs the model not to
// follow embedded instructions found within it. Its output is
// constrained to one JSON object: {"mode": "agent"|"userPassthrough",
// "reason": "..."} with a hard cap on reason length — there is no
// mechanism by which this LLM's output can execute a tool, authorize an
// action, or bypass the human-confirmation step.
package identityadvisor

import (
	"context"
	"time"
)

// Provider is the contract every identity-advisor backend implements.
// One round-trip per call; no streaming, no tools, no agency.
type Provider interface {
	// Recommend returns a recommended identity mode for the session
	// about to start, derived from the class-authored prompt plus
	// structural + textual signals about the triggering thread.
	//
	// Returns the recommendation plus nil on success. Returns
	// (Recommendation{}, err) on any failure (timeout, rate-limit,
	// transport, malformed model output) — callers fall back to a
	// deterministic default (e.g. always ask, or the class's static
	// identityMode) rather than blocking session start.
	Recommend(ctx context.Context, req Request) (Recommendation, error)

	// Name returns a short identifier for logs ("anthropic", "fake",
	// etc.).
	Name() string
}

// Request is the input the identity-advisor LLM is fed. Unlike the
// approval summarizer's Request, fields here may carry untrusted,
// human-authored content (ThreadTranscript, InboundText) — see the
// package doc for why that's safe under this LLM's advisory-only
// contract.
type Request struct {
	AgentName        string // display name for the "Run as <agent>" option
	ClassPrompt      string // spec.identityRecommendation.prompt (class-authored)
	ChannelKind      string // "slack" etc.
	IsDirectMessage  bool   // structural signal
	ParticipantCount int    // distinct humans in the thread
	ThreadDepth      int    // messages in the thread
	InitiatingUser   string // who invoked (display/handle)
	ThreadTranscript string // best-effort; may be "" if unavailable
	InboundText      string // the triggering message
}

// Recommendation is the identity-advisor LLM's advisory output. It is
// never applied automatically — the caller always surfaces it to a
// human for explicit confirmation before binding a session identity.
type Recommendation struct {
	// Mode is one of "agent" or "userPassthrough".
	Mode string
	// Reason is a short, human-readable justification (≤ MaxReasonWords
	// words). May reflect untrusted thread content verbatim or in
	// paraphrase — it is advisory text shown to a human, never executed.
	Reason string
}

// MaxReasonWords caps the reason's length when validating model
// output. Kept short so it reads as a one-line justification in a
// confirmation prompt, not a paragraph.
const MaxReasonWords = 30

// DefaultTimeout caps how long the caller waits for the identity
// advisor before giving up. Session start MUST NOT couple to advisor
// availability, so on timeout callers fall back to a deterministic
// default. 5s is generous for Haiku-class one-shot calls.
const DefaultTimeout = 5 * time.Second

// MaxTranscriptChars truncates the thread transcript fed to the model.
// The transcript can grow unbounded over a long-lived thread; this
// keeps the request small and bounds the untrusted-content surface fed
// to the model in any one call.
const MaxTranscriptChars = 6000
