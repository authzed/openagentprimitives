// Package summarizer is the isolated "What this tool call will do" LLM
// that lives inside the runner pod and produces the one-sentence
// approver-facing summary shown in the Slack approval prompt.
//
// # Security boundary
//
// This is a SECOND, ISOLATED LLM call — not a method on the primary
// agent LLM. The distinction matters: the primary LLM's output can be
// influenced by indirect prompt injection (a malicious MCP tool result
// arriving on a prior turn, etc.). If the primary LLM authored the
// "What" line, a compromised primary could mislead the approver into
// rubber-stamping an attack. A summarizer that only sees the tool
// schema + args produces a mechanically-derived description of what
// will actually execute — a compromised primary cannot lie to the
// approver through this channel.
//
// Inputs the summarizer SEES (Request below):
//
//   - the tool name
//   - the tool's upstream MCP description (operator-signed, immutable)
//   - the input schema (operator-signed, immutable)
//   - the actual args about to execute
//   - the resource type + id + permission
//
// Inputs it does NOT see:
//
//   - the primary LLM's chat history
//   - the agent's own justification text
//   - any tool outputs (the primary injection surface)
//   - the runner's system prompt
//
// Hardening on top of input scoping (enforced by the implementation):
//
//   - System prompt frames args as USER DATA, NOT instructions
//   - Output constrained to one JSON object: {"summary": "..."}
//   - 30-word cap; truncated on overflow
//   - Zero tools, zero agency, single round-trip
//   - Every string arg value truncated at 200 chars to limit injection
//     surface inside the args themselves
//
// See memory/project_approval_summarizer_llm.md for the threat model.
// A third-LLM cross-check ("Why-vs-What consistency check") is planned.
package summarizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Provider is the contract every summarizer backend implements. One
// round-trip per call; no streaming, no tools, no agency.
type Provider interface {
	// Summarize returns a one-sentence description of what executing
	// the tool call in `req` will do, derived mechanically from the
	// schema + args.
	//
	// Returns the summary plus nil on success. Returns ("", err) on
	// any failure (timeout, rate-limit, transport, malformed model
	// output) — callers fall back to a deterministic
	// "Grant <permission> on <resource>" instead of blocking the
	// approval.
	Summarize(ctx context.Context, req Request) (string, error)

	// SummarizeAnnotations returns a one-sentence summary of the changes a user
	// requested via browser annotations, derived ONLY from the structured
	// annotation fields (never the primary LLM's output/history/tool results).
	// Returns ("", err) on any failure; callers fall back to
	// FallbackAnnotationSummary.
	SummarizeAnnotations(ctx context.Context, req AnnotationRequest) (string, error)

	// Name returns a short identifier for logs ("anthropic", "fake",
	// etc.). Not part of caching or routing.
	Name() string
}

// Request is the input the summarizer LLM is fed. Every field listed
// below is REQUIRED to be safe to render verbatim under the security
// model — operator-signed (tool, schema, description) or immutable
// runtime state (args/resource/permission). The agent's own
// justification is DELIBERATELY NOT a field here; see the package
// docstring.
type Request struct {
	Tool            string
	ToolDescription string
	InputSchemaJSON string
	ArgsJSON        string
	ResourceType    string
	ResourceID      string
	Permission      string
}

// hashKey returns a stable cache key for (tool, args). Two identical
// requests across sessions share a key — the summary is deterministic
// in the tool+args, so a cache hit is safe to reuse.
func (r Request) hashKey() string {
	h := sha256.New()
	h.Write([]byte(r.Tool))
	h.Write([]byte{0})
	h.Write([]byte(r.ArgsJSON))
	return hex.EncodeToString(h.Sum(nil)[:16]) // 16-byte prefix; collision-resistant enough
}

// MaxSummaryWords caps the summary's length when validating model
// output. Slack section text gives us up to 3000 chars; 30 words is
// enough to be informative but tight enough to discourage rambling.
const MaxSummaryWords = 30

// DefaultTimeout caps how long the runner waits for the summarizer
// before giving up. The approval availability MUST NOT couple to
// summarizer availability, so on timeout we fall back to the
// deterministic "Grant <permission> on <resource>" rendered by
// channelsd. 5s is generous for Haiku-class one-shot calls.
const DefaultTimeout = 5 * time.Second

// MaxStringArgChars caps individual string arg values before they're
// fed to the LLM. Args like `notes` or `description` can carry
// arbitrary user text — truncating them limits the injection surface
// inside the args themselves without losing the tool-call's gist.
const MaxStringArgChars = 200

// Cache is a process-local memo for (tool, args) → summary. Entries
// are never evicted by TTL (the inputs are immutable per call), but
// the map is bounded by a soft cap. Safe for concurrent use.
type Cache struct {
	mu  sync.Mutex
	m   map[string]string
	cap int
}

// NewCache constructs a Cache with the given soft cap. Cap ≤ 0 falls
// back to 1024.
func NewCache(cap int) *Cache {
	if cap <= 0 {
		cap = 1024
	}
	return &Cache{m: make(map[string]string, cap), cap: cap}
}

// Get returns the cached summary for req, ok==true on hit.
func (c *Cache) Get(req Request) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[req.hashKey()]
	return v, ok
}

// Put stores summary against req's hash key. Evicts a random entry
// when the cap is hit (simple bounded growth; LRU would be overkill
// for the expected per-session approval volume).
func (c *Cache) Put(req Request, summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.cap {
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[req.hashKey()] = summary
}
