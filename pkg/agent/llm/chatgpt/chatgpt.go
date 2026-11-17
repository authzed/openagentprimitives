// Package chatgpt implements the public Responses API contract for ChatGPT
// plan usage. This provider never sends subscription credentials to an
// OPENAI_BASE_URL override or to the Chat Completions endpoint.
package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type Provider struct{ client oa.Client }

func New(token string) (*Provider, error) {
	if token == "" {
		return nil, fmt.Errorf("chatgpt: access token required")
	}
	return &Provider{client: oa.NewClient(option.WithAPIKey(token), option.WithBaseURL("https://api.openai.com/v1/"), option.WithMaxRetries(0))}, nil
}
func (*Provider) Name() string           { return "chatgpt" }
func (*Provider) SupportedFromEnv() bool { return false }

// Subscription usage has no truthful per-token dollar price.
func (*Provider) Pricing(string) (llm.ModelPricing, bool) { return llm.ModelPricing{}, false }
func (*Provider) Capabilities(string) llm.CapabilitySet   { return llm.NewCapabilitySet() }
func (*Provider) NativeInputMIMEs(string) llm.MIMESet     { return llm.MIMESet{} }

func init() {
	providers.RegisterRotating("chatgpt", func(token string) (llm.Provider, error) { return New(token) })
}

func requestBody(req llm.Request) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("chatgpt: model required")
	}
	input := []any{}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("chatgpt: unsupported message role %q", m.Role)
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				input = append(input, map[string]any{"role": m.Role, "content": b.Text})
			case "tool_use":
				if b.ToolUse == nil || !json.Valid(b.ToolUse.Input) {
					return nil, fmt.Errorf("chatgpt: invalid tool call")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": b.ToolUse.ID, "name": b.ToolUse.Name, "namespace": "oap", "arguments": string(b.ToolUse.Input)})
			case "tool_result":
				if b.ToolResult == nil {
					return nil, fmt.Errorf("chatgpt: missing tool result")
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": b.ToolResult.ToolUseID, "output": b.ToolResult.Content})
			case "image":
				input = append(input, map[string]any{"role": m.Role, "content": []any{map[string]any{"type": "input_image", "image_url": "data:" + b.MIME + ";base64," + base64.StdEncoding.EncodeToString(b.Data)}}})
			default:
				return nil, fmt.Errorf("chatgpt: unsupported content block %q", b.Type)
			}
		}
	}
	instructions := make([]string, 0, len(req.System))
	for _, b := range req.System {
		instructions = append(instructions, b.Text)
	}
	body := map[string]any{"model": req.Model, "instructions": strings.Join(instructions, "\n\n"), "input": input, "store": false, "stream": true}
	if len(req.Tools) > 0 {
		tools := []any{}
		for _, t := range req.Tools {
			if t.ServerType != "" {
				return nil, fmt.Errorf("chatgpt: hosted tool %q is unsupported", t.Name)
			}
			if !json.Valid(t.InputSchema) {
				return nil, fmt.Errorf("chatgpt: invalid schema for tool %q", t.Name)
			}
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema, "strict": false})
		}
		body["tools"] = []any{map[string]any{"type": "namespace", "name": "oap", "description": "Tools authorized by Open Agent Primitives", "tools": tools}}
	}
	return body, nil
}

type item struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}
type event struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Item        item   `json:"item"`
	Response    struct {
		Model  string `json:"model"`
		Output []item `json:"output"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
		Usage struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Details struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (p *Provider) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := requestBody(req)
	if err != nil {
		return llm.Response{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return llm.Response{}, fmt.Errorf("chatgpt: encode request: %w", err)
	}
	stream := p.client.Responses.NewStreaming(ctx, responses.ResponseNewParams{}, option.WithRequestBody("application/json", encoded))
	defer func() {
		if err := stream.Close(); err != nil {
			log.Printf("chatgpt: close response stream: %v", err)
		}
	}()
	emit := func(e llm.StreamEvent) {
		if req.OnEvent != nil {
			req.OnEvent(e)
		}
	}
	// Reasoning items occupy Responses output indices but are not public
	// content blocks. Keep neutral stream indices aligned with the transcript.
	indices := map[int]int{}
	completedItems := map[int]item{}
	blockIndex := func(output int) int {
		if i, ok := indices[output]; ok {
			return i
		}
		i := len(indices)
		indices[output] = i
		return i
	}
	for stream.Next() {
		var ev event
		if err = json.Unmarshal([]byte(stream.Current().RawJSON()), &ev); err != nil {
			return llm.Response{}, fmt.Errorf("chatgpt: malformed response event")
		}
		switch ev.Type {
		case "response.output_text.delta":
			emit(llm.StreamEvent{Type: llm.StreamEventTextDelta, BlockIndex: blockIndex(ev.OutputIndex), Text: ev.Delta})
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				emit(llm.StreamEvent{Type: llm.StreamEventToolUseStart, BlockIndex: blockIndex(ev.OutputIndex), ToolUseID: ev.Item.CallID, ToolName: ev.Item.Name})
			}
		case "response.function_call_arguments.delta":
			emit(llm.StreamEvent{Type: llm.StreamEventToolUseDeltaArgs, BlockIndex: blockIndex(ev.OutputIndex), JSONFragment: ev.Delta})
		case "response.output_item.done":
			completedItems[ev.OutputIndex] = ev.Item
			if ev.Item.Type == "function_call" {
				emit(llm.StreamEvent{Type: llm.StreamEventToolUseStop, BlockIndex: blockIndex(ev.OutputIndex)})
			}
		case "response.failed", "response.incomplete", "error":
			code := ""
			if ev.Response.Error != nil {
				code = ev.Response.Error.Code
			}
			return llm.Response{}, fmt.Errorf("chatgpt: %s (%s); check ChatGPT usage or sign in again", ev.Type, code)
		case "response.completed":
			out := llm.Response{Model: ev.Response.Model, StopReason: "end_turn", Usage: llm.Usage{InputTokens: ev.Response.Usage.Input - ev.Response.Usage.Details.Cached, CacheReadTokens: ev.Response.Usage.Details.Cached, OutputTokens: ev.Response.Usage.Output}}
			// Subscription streams can omit function calls from the terminal
			// response.output, even though output_item.done delivered them.
			// Finalized stream items are authoritative. Commit them only after
			// response.completed so a later failure cannot execute partial tools.
			output := ev.Response.Output
			if len(completedItems) > 0 {
				order := make([]int, 0, len(completedItems))
				for index := range completedItems {
					order = append(order, index)
				}
				slices.Sort(order)
				output = make([]item, 0, len(order))
				for _, index := range order {
					output = append(output, completedItems[index])
				}
			}
			for _, it := range output {
				switch it.Type {
				case "message":
					for _, b := range it.Content {
						switch b.Type {
						case "output_text":
							out.Content = append(out.Content, llm.ContentBlock{Type: "text", Text: b.Text})
						case "refusal":
							out.Content = append(out.Content, llm.ContentBlock{Type: "text", Text: b.Refusal})
						}
					}
				case "function_call":
					if (it.Namespace != "" && it.Namespace != "oap") || !json.Valid([]byte(it.Arguments)) {
						return llm.Response{}, fmt.Errorf("chatgpt: invalid function call in completed response")
					}
					out.Content = append(out.Content, llm.ContentBlock{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: it.CallID, Name: it.Name, Input: json.RawMessage(it.Arguments)}})
					out.StopReason = "tool_use"
				case "reasoning": // The neutral transcript stores text and tool calls.
				default:
					return llm.Response{}, fmt.Errorf("chatgpt: unsupported output item %q", it.Type)
				}
			}
			if len(out.Content) == 0 {
				return llm.Response{}, fmt.Errorf("chatgpt: completed response contained no text or tool calls")
			}
			emit(llm.StreamEvent{Type: llm.StreamEventUsage, Usage: &out.Usage})
			emit(llm.StreamEvent{Type: llm.StreamEventStop, StopReason: out.StopReason})
			return out, nil
		}
	}
	if err = stream.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("chatgpt: Responses request failed: %w", err)
	}
	return llm.Response{}, fmt.Errorf("chatgpt: stream ended before response.completed")
}
