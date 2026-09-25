//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Reply builders.

// Text returns a plain-text reply part.
func Text(s string) ReplyPart {
	return ReplyPart{kind: "text", text: s}
}

// ToolUse returns a tool_use reply part. The LLM will emit one
// tool_use block; the runner will dispatch the tool, get the result,
// and feed it back as the next request.
func ToolUse(name string, args map[string]any) ReplyPart {
	if args == nil {
		args = map[string]any{}
	}
	return ReplyPart{kind: "tool_use", toolName: name, toolArgs: args}
}

// RespondToUser is the agent's exit-turn idiom: emit text + call
// the respond_to_user tool. Used to end a turn with a final reply.
func RespondToUser(text string) ReplyPart {
	return ReplyPart{kind: "respond_to_user", text: text}
}

// EndTurn marks the response as stop_reason=end_turn with no
// tool_use. The runner will treat this as "agent has nothing more
// to do".
func EndTurn() ReplyPart {
	return ReplyPart{kind: "end_turn"}
}

// Refusal forces the response's stop_reason to "refusal" (a provider
// content-policy block). Compose it after other parts: alone it yields empty
// content; after a ToolUse it keeps the tool_use block but still refuses.
func Refusal() ReplyPart {
	return ReplyPart{kind: "refusal"}
}

// Tool-result predicates for OnToolResult's second arg.
//
// The predicates operate on the JSON-parsed `any` form of the
// tool_result.Content string. If the content isn't valid JSON, the
// predicate receives the raw string.

// AnyResult matches any tool_result payload.
//
// Beware what this hides on an authorization-gated tool: a DENIED call
// still produces a tool_result — an error one — and AnyResult matches it
// as happily as the real payload, so the script replies with its canned
// success text and ExpectAgentReply passes. A gated tool whose scenario
// asserts the call SUCCEEDED must use ResultContains (or ResultMatches)
// instead; that is how the post-approval re-check denial went unnoticed
// across 19 of 20 verbose runs.
func AnyResult() func(any) bool { return func(any) bool { return true } }

// ResultContains matches when the tool_result payload, rendered back to
// JSON (or used as-is when it is a bare string), contains every one of
// subs. It is the de-blinded alternative to AnyResult: an error result
// will not contain the payload's substrings, so no rule matches and
// ScriptedLLM.Send fails the test with its full request dump rather than
// replying with canned success text.
func ResultContains(subs ...string) func(any) bool {
	return func(got any) bool {
		hay, ok := got.(string)
		if !ok {
			raw, err := json.Marshal(got)
			if err != nil {
				hay = fmt.Sprint(got)
			} else {
				hay = string(raw)
			}
		}
		for _, s := range subs {
			if !strings.Contains(hay, s) {
				return false
			}
		}
		return true
	}
}

// ResultEquals matches when the parsed payload deep-equals `want`.
func ResultEquals(want any) func(any) bool {
	return func(got any) bool {
		return reflect.DeepEqual(got, want)
	}
}

// ResultMatches lets the test inspect the parsed payload with arbitrary
// logic. The predicate returns true to accept the match.
func ResultMatches(predicate func(any) bool) func(any) bool {
	if predicate == nil {
		return AnyResult()
	}
	return predicate
}
