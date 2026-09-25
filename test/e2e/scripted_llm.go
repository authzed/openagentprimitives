//go:build e2e

// Package e2e is the in-process end-to-end test framework for
// agentprimitives.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// TB is the subset of *testing.T the harness needs. Lets tests swap
// in a recording double for assertions about harness-side failures.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Matcher inspects an llm.Request and reports whether this rule
// applies. Pure function; no side effects.
type Matcher func(req llm.Request) bool

// ReplyPart is one block in the assistant's response. Multiple parts
// in a single Reply assemble into one llm.Response.
type ReplyPart struct {
	kind     string // "text" | "tool_use" | "respond_to_user" | "end_turn" | "refusal"
	text     string
	toolName string
	toolArgs map[string]any
}

// Rule is one (matcher → reply) pair. Consumed on first match unless
// Repeating is true.
//
// Exactly one of Reply, ReplyFn, or ReplyErr is set on a given Rule.
// ReplyFn lets a test compute the reply parts at match time — useful
// when a later tool_use needs an id surfaced by an earlier tool_result
// (e.g. the operation_id minted by new_operation, which downstream
// sandbox tool_use args must echo back).
type Rule struct {
	Match   Matcher
	Reply   []ReplyPart
	ReplyFn func() []ReplyPart
	// ReplyErr, when non-nil, causes Send to return (Response{}, ReplyErr)
	// on match. Mutually exclusive with Reply/ReplyFn — last-set wins.
	ReplyErr  error
	Repeating bool
	Consumed  bool
}

// ScriptedLLM is the test-facing LLM provider. Satisfies llm.Provider.
type ScriptedLLM struct {
	t     TB
	mu    sync.Mutex
	rules []*Rule
	seen  []llm.Request
	// nativeMIMEs is what NativeInputMIMEs reports, for every model id.
	// Guarded by mu: it is written from the test goroutine
	// (SetNativeInputMIMEs) and read from the runner's own goroutine (the
	// attachment-hydration pass, once per request).
	nativeMIMEs llm.MIMESet
}

// NewScriptedLLM constructs a ScriptedLLM bound to t. Failures during
// Send (no matching rule, etc.) call t.Fatalf with a structured dump.
func NewScriptedLLM(t TB) *ScriptedLLM {
	return &ScriptedLLM{t: t}
}

// Name implements llm.Provider.
func (*ScriptedLLM) Name() string { return "test" }

// SupportedFromEnv implements llm.Provider. Always true.
func (*ScriptedLLM) SupportedFromEnv() bool { return true }

// Pricing implements llm.Provider. The scripted LLM reports a single flat price
// for any model so e2e cost-estimation scenarios are deterministic.
func (*ScriptedLLM) Pricing(model string) (llm.ModelPricing, bool) {
	return llm.ModelPricing{
		InputPerMTok: 1, OutputPerMTok: 1,
		CacheCreationPerMTok: 1, CacheReadPerMTok: 1, Currency: "USD",
	}, true
}

// Capabilities implements llm.Provider. The scripted LLM has no model-level
// capability data of its own; it always reports an empty set.
func (*ScriptedLLM) Capabilities(string) llm.CapabilitySet {
	return llm.NewCapabilitySet()
}

// NativeInputMIMEs implements llm.Provider. Reports whatever
// SetNativeInputMIMEs configured, for every model id. The default is nil —
// no native input support — so e2e scenarios assert on the
// reference/manifest path unless they explicitly opt in.
func (s *ScriptedLLM) NativeInputMIMEs(string) llm.MIMESet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeMIMEs
}

// SetNativeInputMIMEs declares which MIME types this provider accepts as
// native message content, mirroring fake.Provider.SetNativeInputMIMEs. A
// scenario that wants an inbound attachment to reach the provider as a native
// block must call this — the default (nil) degrades every attachment to
// reference form no matter what the rest of the pipeline does.
//
// Call before driving the inbound message that carries the attachment; the
// hydration pass reads this once per request, on the runner's goroutine.
func (s *ScriptedLLM) SetNativeInputMIMEs(set llm.MIMESet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nativeMIMEs = set
}

// syntheticUsage is the fixed per-Send token usage ScriptedLLM reports on
// every response. There is no real model behind this provider to report real
// token counts, but the runner's cumulative-usage tracking (pkg/agent/runner's
// Progress/Loop snapshot) and the post-session cost reporter (pkg/agent/postsession/cost)
// multiply cumulative usage by Pricing — without a non-zero usage figure every
// e2e cost-estimation scenario would compute a cost of exactly 0 regardless of
// pricing, making "amountMicroUSD > 0" unassertable. Fixed and small so
// multi-turn scenarios stay well under any fixture's budget.maxTokens ceiling.
var syntheticUsage = llm.Usage{InputTokens: 100, OutputTokens: 50}

// Send walks the rule list in registration order and returns the
// first match's assembled response. On no-match, calls t.Fatalf
// with a structured dump.
func (s *ScriptedLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	for _, r := range s.rules {
		if r.Consumed && !r.Repeating {
			continue
		}
		if r.Match(req) {
			if !r.Repeating {
				r.Consumed = true
			}
			if r.ReplyErr != nil {
				return llm.Response{}, r.ReplyErr
			}
			parts := r.Reply
			if r.ReplyFn != nil {
				parts = r.ReplyFn()
			}
			resp := assembleResponse(parts)
			resp.Usage = syntheticUsage
			return resp, nil
		}
	}
	s.t.Fatalf("ScriptedLLM.Send: no rule matched request:\n%s\n\nrules pending: %d\nrequests served so far: %d",
		describeRequest(req), s.pendingCountLocked(), len(s.seen))
	return llm.Response{}, nil
}

// Requests returns a copy of every Send the LLM observed.
func (s *ScriptedLLM) Requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Request, len(s.seen))
	copy(out, s.seen)
	return out
}

// AssertAllRulesConsumed fails the test if any non-Repeating rule
// hasn't matched.
//
// This SAMPLES; it does not wait. The conversation it asserts about runs on
// the runner's goroutine, so a caller must first wait on a settle point that
// proves the turn loop finished — e2e.WaitForSessionIdle is the general one.
// Gating it on a channelsd-side signal instead (an inbound turn appearing in
// memory, say) asserts nothing: channelsd durably records the inbound turn
// BEFORE the runner for that session exists, so the sample can land before the
// first Send. The "requests served" count below is what distinguishes that
// (0 served — nothing ran yet) from a genuine mismatch (n served, none
// matching), which is the whole reason it is in the message.
func (s *ScriptedLLM) AssertAllRulesConsumed() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rules {
		if !r.Consumed && !r.Repeating {
			s.t.Fatalf("ScriptedLLM: rule #%d of %d was never matched (requests served: %d)%s",
				i, len(s.rules), len(s.seen), s.describeSeenLocked())
		}
	}
}

// describeSeenLocked renders every request the LLM actually served, for the
// AssertAllRulesConsumed diagnostic. Caller holds s.mu.
func (s *ScriptedLLM) describeSeenLocked() string {
	if len(s.seen) == 0 {
		return "\n  (the scripted LLM was never called — the turn loop had not reached it yet; " +
			"did the test wait for a settle point, e.g. e2e.WaitForSessionIdle?)"
	}
	var b strings.Builder
	for i, req := range s.seen {
		fmt.Fprintf(&b, "\n  request #%d:\n%s", i, describeRequest(req))
	}
	return b.String()
}

func (s *ScriptedLLM) pendingCountLocked() int {
	n := 0
	for _, r := range s.rules {
		if !r.Consumed {
			n++
		}
	}
	return n
}

// On is the low-level rule registration entry point.
func (s *ScriptedLLM) On(m Matcher) *RuleBuilder {
	r := &Rule{Match: m}
	s.mu.Lock()
	s.rules = append(s.rules, r)
	s.mu.Unlock()
	return &RuleBuilder{r: r}
}

// OnUserMessage is shorthand: matches when the latest user-role
// message contains a text ContentBlock whose Text contains the
// substring `textContains`.
func (s *ScriptedLLM) OnUserMessage(textContains string) *RuleBuilder {
	return s.On(matchUserText(textContains))
}

// OnToolResult is shorthand: matches when the latest tool_result
// ContentBlock corresponds (via ToolUseID lookup) to a tool_use
// for `toolName`, AND the predicate applied to the result's Content
// (parsed as JSON into `any`) returns true. `predicate` may be nil
// (matches any result).
func (s *ScriptedLLM) OnToolResult(toolName string, predicate func(any) bool) *RuleBuilder {
	return s.On(matchToolResult(toolName, predicate))
}

// RuleBuilder is the fluent return from On / OnUserMessage / OnToolResult.
type RuleBuilder struct{ r *Rule }

// Reply sets the response parts and returns the builder for chaining
// (.Repeating()).
func (b *RuleBuilder) Reply(parts ...ReplyPart) *RuleBuilder {
	b.r.Reply = parts
	return b
}

// ReplyFn registers a closure that returns the reply parts at match
// time. Use when an earlier rule's predicate captured a value the next
// reply needs to echo back (e.g. an operation_id minted by
// new_operation that a downstream sandbox tool_use must reference).
//
// Calling ReplyFn AFTER Reply, or vice-versa, overrides the prior
// setting — only the most recently-set source is used.
func (b *RuleBuilder) ReplyFn(fn func() []ReplyPart) *RuleBuilder {
	b.r.ReplyFn = fn
	b.r.Reply = nil
	return b
}

// ReplyErr registers an error to return from Send on match. Use for
// provider-error E2E scenarios. Calling ReplyErr after Reply/ReplyFn
// (or vice versa) overrides the prior setting — only the most
// recently-set source is used.
func (b *RuleBuilder) ReplyErr(err error) *RuleBuilder {
	b.r.ReplyErr = err
	b.r.Reply = nil
	b.r.ReplyFn = nil
	return b
}

// Repeating marks the rule as match-many. By default rules are
// consumed on first match.
func (b *RuleBuilder) Repeating() *RuleBuilder {
	b.r.Repeating = true
	return b
}

// ─── matchers (internal) ───

// matchUserText: find the most recent user-role message, look for a
// text ContentBlock containing `contains`.
func matchUserText(contains string) Matcher {
	return func(req llm.Request) bool {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role != "user" {
				continue
			}
			// Skip user messages that are ONLY tool_result blocks —
			// the "latest user text" we want to match against is the
			// human-typed text, not a tool result.
			var hasText bool
			for _, c := range m.Content {
				if c.Type == "text" {
					hasText = true
					if strings.Contains(c.Text, contains) {
						return true
					}
				}
			}
			if hasText {
				return false // had text but didn't match
			}
			// All content was tool_result; keep walking back.
		}
		return false
	}
}

// matchToolResult: find the most recent tool_result, resolve its
// ToolUseID back to the tool name via earlier assistant messages,
// then apply the predicate to the parsed result Content.
func matchToolResult(name string, predicate func(any) bool) Matcher {
	return func(req llm.Request) bool {
		// 1. Find latest tool_result ContentBlock.
		var latest *llm.ToolResultBlock
		for i := len(req.Messages) - 1; i >= 0 && latest == nil; i-- {
			m := req.Messages[i]
			for j := len(m.Content) - 1; j >= 0; j-- {
				c := m.Content[j]
				if c.Type == "tool_result" && c.ToolResult != nil {
					latest = c.ToolResult
					break
				}
			}
		}
		if latest == nil {
			return false
		}
		// 2. Walk earlier messages to find the matching tool_use ID.
		var toolName string
		for _, m := range req.Messages {
			if m.Role != "assistant" {
				continue
			}
			for _, c := range m.Content {
				if c.Type == "tool_use" && c.ToolUse != nil && c.ToolUse.ID == latest.ToolUseID {
					toolName = c.ToolUse.Name
				}
			}
		}
		if toolName != name {
			return false
		}
		// 3. Apply predicate to the result's Content (parsed as JSON).
		if predicate == nil {
			return true
		}
		var parsed any
		// The runner wraps every tool_result Content in untrusted-data
		// delimiters (pkg/agent/runner wrapUntrustedToolOutput); strip them
		// so the predicate sees the raw JSON payload the tool returned.
		content := unwrapUntrustedToolOutput(latest.Content)
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			// Treat unparseable as the raw string for predicate purposes.
			parsed = content
		}
		return predicate(parsed)
	}
}

// unwrapUntrustedToolOutput strips the
//
//	<untrusted-tool-output nonce="…">␤ … ␤</untrusted-tool-output nonce="…">
//
// envelope that pkg/agent/runner wraps every tool_result Content in before
// feeding it to the LLM (wrapUntrustedToolOutput — a prompt-injection
// defence). matchToolResult needs the raw tool payload — the JSON the tool
// actually returned — to json.Unmarshal it for predicates. Returns s
// unchanged when it carries no such envelope.
func unwrapUntrustedToolOutput(s string) string {
	// Element name mirrors untrustedToolOutputTag in
	// pkg/agent/runner/loop.go. The nonce attribute is per-result and
	// unknown here, so match on the element-name prefix and slice between
	// the open tag's '>' and the final closing tag.
	const openPrefix = "<untrusted-tool-output"
	const closeTag = "</untrusted-tool-output"
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, openPrefix) {
		return s
	}
	openEnd := strings.IndexByte(t, '>')
	closeStart := strings.LastIndex(t, closeTag)
	if openEnd < 0 || closeStart < 0 || closeStart <= openEnd {
		return s
	}
	return strings.TrimSpace(t[openEnd+1 : closeStart])
}

// ─── response assembly ───

func assembleResponse(parts []ReplyPart) llm.Response {
	resp := llm.Response{StopReason: "end_turn"}
	for i, p := range parts {
		switch p.kind {
		case "text":
			resp.Content = append(resp.Content, llm.ContentBlock{
				Type: "text", Text: p.text,
			})
		case "tool_use":
			input, _ := json.Marshal(p.toolArgs)
			resp.Content = append(resp.Content, llm.ContentBlock{
				Type: "tool_use",
				ToolUse: &llm.ToolUseBlock{
					ID:    fmt.Sprintf("toolu_%d", i),
					Name:  p.toolName,
					Input: input,
				},
			})
			resp.StopReason = "tool_use"
		case "respond_to_user":
			args := map[string]any{"text": p.text}
			input, _ := json.Marshal(args)
			resp.Content = append(resp.Content,
				llm.ContentBlock{Type: "text", Text: p.text},
				llm.ContentBlock{
					Type: "tool_use",
					ToolUse: &llm.ToolUseBlock{
						ID:    fmt.Sprintf("toolu_%d", i),
						Name:  "respond_to_user",
						Input: input,
					},
				},
			)
			resp.StopReason = "tool_use"
		case "end_turn":
			// Already set.
		case "refusal":
			resp.StopReason = "refusal"
		}
	}
	return resp
}

// describeRequest renders the latest message in a request for the
// "no rule matched" diagnostic. Walks back to find the latest message
// the matcher would have looked at.
func describeRequest(req llm.Request) string {
	if len(req.Messages) == 0 {
		return "(empty request)"
	}
	last := req.Messages[len(req.Messages)-1]
	var b strings.Builder
	fmt.Fprintf(&b, "  role: %s", last.Role)
	for _, c := range last.Content {
		fmt.Fprintf(&b, "\n  %s: ", c.Type)
		switch c.Type {
		case "text":
			fmt.Fprintf(&b, "%q", c.Text)
		case "tool_use":
			if c.ToolUse != nil {
				fmt.Fprintf(&b, "name=%s id=%s input=%s",
					c.ToolUse.Name, c.ToolUse.ID, string(c.ToolUse.Input))
			}
		case "tool_result":
			if c.ToolResult != nil {
				fmt.Fprintf(&b, "tool_use_id=%s isErr=%v content=%q",
					c.ToolResult.ToolUseID, c.ToolResult.IsError, c.ToolResult.Content)
			}
		}
	}
	return b.String()
}

// Compile-time check.
var _ llm.Provider = (*ScriptedLLM)(nil)
