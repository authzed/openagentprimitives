package openaicompat

import (
	"encoding/json"
	"fmt"
	"strings"

	oa "github.com/openai/openai-go/v3"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// BuildParams translates a neutral llm.Request into OpenAI Chat Completions
// params.
//
// Cacheable hints are deliberately ignored. OpenAI performs automatic prompt
// caching with no client-side marker, so there is nothing to translate the hint
// into — and emitting an Anthropic-shaped cache_control here would be a
// malformed request rather than a no-op. Locked by
// TestBuildParams_CacheableHintIsInert.
//
// Server tools are rejected (OpenAI has no equivalent) rather than silently
// dropped.
func BuildParams(req llm.Request) (oa.ChatCompletionNewParams, error) {
	if req.Model == "" {
		return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: model is required")
	}

	var messages []oa.ChatCompletionMessageParamUnion

	// System blocks → one joined system message.
	if len(req.System) > 0 {
		parts := make([]string, 0, len(req.System))
		for _, s := range req.System {
			parts = append(parts, s.Text)
		}
		messages = append(messages, oa.SystemMessage(strings.Join(parts, "\n\n")))
	}

	// Conversation messages.
	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			// A neutral user message may carry text and/or tool_result blocks, in
			// any order. Adjacent text blocks join with "\n\n" (matching the system
			// block separator) into a single user message; a tool_result flushes
			// any accumulated text first so the emitted OpenAI messages follow the
			// neutral Content encounter order rather than grouping all text last.
			var textParts []string
			flushText := func() {
				if len(textParts) == 0 {
					return
				}
				messages = append(messages, oa.UserMessage(strings.Join(textParts, "\n\n")))
				textParts = nil
			}
			for _, cb := range msg.Content {
				switch cb.Type {
				case "text":
					textParts = append(textParts, cb.Text)
				case "tool_result":
					if cb.ToolResult == nil {
						return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool_result block missing ToolResult")
					}
					flushText()
					messages = append(messages, oa.ToolMessage(cb.ToolResult.Content, cb.ToolResult.ToolUseID))
				case "document", "image":
					// A native block reached a provider whose adapter cannot
					// emit one. That means a model row declared a MIME its
					// adapter does not implement — the two must move together.
					// Fail loudly here rather than dropping the file: silently
					// omitting it would leave the agent answering about a
					// document it never received.
					return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: native %q content blocks are not supported by this provider; "+
						"remove the MIME from the model's NativeInputMIMEs row", cb.Type)
				default:
					return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: unsupported user content block %q", cb.Type)
				}
			}
			flushText()
		case "assistant":
			// Text → assistant content; tool_use → assistant tool_calls. Adjacent
			// text blocks join with "\n\n", matching the system/user separator.
			var textParts []string
			var toolCalls []oa.ChatCompletionMessageToolCallUnionParam
			for _, cb := range msg.Content {
				switch cb.Type {
				case "text":
					textParts = append(textParts, cb.Text)
				case "tool_use":
					if cb.ToolUse == nil {
						return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool_use block missing ToolUse")
					}
					toolCalls = append(toolCalls, oa.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &oa.ChatCompletionMessageFunctionToolCallParam{
							ID: cb.ToolUse.ID,
							Function: oa.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      cb.ToolUse.Name,
								Arguments: string(cb.ToolUse.Input),
							},
						},
					})
				default:
					return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: unsupported assistant content block %q", cb.Type)
				}
			}
			asst := oa.ChatCompletionAssistantMessageParam{ToolCalls: toolCalls}
			if len(textParts) > 0 {
				asst.Content.OfString = oa.String(strings.Join(textParts, "\n\n"))
			}
			messages = append(messages, oa.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		default:
			return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: unknown message role %q", msg.Role)
		}
	}

	// Tools. Server tools are rejected; custom tools pass the full JSON schema
	// as `parameters` (OpenAI wants the whole schema object, unlike Anthropic).
	var tools []oa.ChatCompletionToolUnionParam
	for _, td := range req.Tools {
		if td.ServerType != "" {
			return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: server tool %q not supported", td.Name)
		}
		var schema map[string]any
		if len(td.InputSchema) > 0 {
			if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
				return oa.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool %q bad input schema: %w", td.Name, err)
			}
		}
		tools = append(tools, oa.ChatCompletionFunctionTool(oa.FunctionDefinitionParam{
			Name:        td.Name,
			Description: oa.String(td.Description),
			Parameters:  oa.FunctionParameters(schema),
		}))
	}

	params := oa.ChatCompletionNewParams{
		Model:               oa.ChatModel(req.Model),
		Messages:            messages,
		Tools:               tools,
		MaxCompletionTokens: oa.Int(int64(req.MaxTokens)),
		StreamOptions:       oa.ChatCompletionStreamOptionsParam{IncludeUsage: oa.Bool(true)},
	}
	if req.UserID != "" {
		params.User = oa.String(req.UserID)
	}
	return params, nil
}

// finishReasonToStop maps an OpenAI finish_reason to a neutral StopReason
// (Response.HasToolUses keys off the tool_use blocks, not this string, but the
// runner records it for logging/budget).
func finishReasonToStop(fr string) string {
	switch fr {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		return fr
	}
}

// TranslateResponse converts an OpenAI ChatCompletion into a neutral Response.
// An empty Choices list is an error, not a silently-returned zero-value
// Response — the caller must know something went wrong rather than treat an
// empty response as a valid no-op turn.
func TranslateResponse(cc *oa.ChatCompletion) (llm.Response, error) {
	if len(cc.Choices) == 0 {
		return llm.Response{}, fmt.Errorf("openaicompat: response had no choices")
	}
	choice := cc.Choices[0]
	msg := choice.Message

	var content []llm.ContentBlock
	if msg.Content != "" {
		content = append(content, llm.ContentBlock{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		content = append(content, llm.ContentBlock{
			Type: "tool_use",
			ToolUse: &llm.ToolUseBlock{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			},
		})
	}

	return llm.Response{
		Content:    content,
		StopReason: finishReasonToStop(choice.FinishReason),
		Model:      cc.Model,
		Usage: llm.Usage{
			InputTokens:  cc.Usage.PromptTokens,
			OutputTokens: cc.Usage.CompletionTokens,
		},
	}, nil
}
