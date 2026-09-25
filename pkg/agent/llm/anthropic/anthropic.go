// Package anthropic adapts the official Anthropic Go SDK to the llm.Provider
// interface. Cacheable system blocks, tool definitions, and message content
// blocks emit cache_control: { type: "ephemeral" }; the rest pass through
// unmarked.
//
// The API key lives in the SDK client and is never logged: the SDK's debug
// logging is off by default and this adapter does not turn it on.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

type Provider struct {
	client *sdk.Client
}

// New constructs a Provider with the given API key. Panics on an empty key,
// which would otherwise silently fall back to the SDK's env-var lookup.
func New(apiKey string) *Provider {
	if apiKey == "" {
		panic("anthropic.New: apiKey must be non-empty")
	}
	c := sdk.NewClient(option.WithAPIKey(apiKey))
	return &Provider{client: &c}
}

// DefaultModelID is the model id used when the caller leaves Request.Model
// unset.
const DefaultModelID = "claude-opus-4-7"

// envAPIKey is the single env var this provider reads.
const envAPIKey = "ANTHROPIC_API_KEY"

func (*Provider) Name() string { return "anthropic" }

// SupportedFromEnv reports whether ANTHROPIC_API_KEY is set, so a startup-time
// selector can choose this provider without constructing the client (which
// panics on an empty key).
func (*Provider) SupportedFromEnv() bool {
	return os.Getenv(envAPIKey) != ""
}

// NewFromEnv builds a Provider from ANTHROPIC_API_KEY, erroring when it is
// unset. Prefer it over pairing os.Getenv + New at the call site.
func NewFromEnv() (*Provider, error) {
	key := os.Getenv(envAPIKey)
	if key == "" {
		return nil, fmt.Errorf("anthropic: %s not set", envAPIKey)
	}
	return New(key), nil
}

// SDKClient returns the underlying anthropic-sdk-go client, so the runner can
// build a Files-API client without re-reading the API key.
func (p *Provider) SDKClient() *sdk.Client { return p.client }

// Send translates req → SDK Messages call → response.
//
// It always uses the SDK's streaming endpoint (Messages.NewStreaming +
// (*Message).Accumulate), even when Request.OnEvent is nil, because the
// non-streaming endpoint refuses MaxTokens above the model's per-call
// inference-time budget (≈21k for most models, ≈8k for Opus 4 variants) and
// streaming has no such cap. Accumulate rebuilds exactly the Message the
// non-streaming endpoint would have returned, so the final llm.Response is
// identical either way; OnEvent only adds live deltas.
//
// IMPORTANT: the field names below reference the anthropic-sdk-go v1.x API. If
// the SDK is upgraded, re-check these mappings against its messages.go/tools.go.
func (p *Provider) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	if req.Model == "" {
		return llm.Response{}, fmt.Errorf("anthropic: model is required")
	}

	params, err := BuildParams(req)
	if err != nil {
		return llm.Response{}, err
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var acc sdk.Message
	for stream.Next() {
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return llm.Response{}, fmt.Errorf("anthropic: stream accumulate: %w", err)
		}
		if req.OnEvent != nil {
			for _, neutral := range translateStreamEvent(ev) {
				req.OnEvent(neutral)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("anthropic: stream: %w", err)
	}
	return translateResponse(&acc), nil
}

// applyMessageCacheControl stamps an ephemeral cache_control breakpoint onto
// whichever variant of u is populated. The runner marks at most two message
// blocks per request (Loop.markCacheBreakpoints); the system block and last tool
// definition take the other two of Anthropic's four-breakpoint budget.
//
// The switch covers every block type BuildParams can emit, and all six SDK param
// types carry a CacheControl field, so no case skips or approximates.
func applyMessageCacheControl(u *sdk.ContentBlockParamUnion) {
	cc := sdk.NewCacheControlEphemeralParam()
	switch {
	case u.OfText != nil:
		u.OfText.CacheControl = cc
	case u.OfToolUse != nil:
		u.OfToolUse.CacheControl = cc
	case u.OfToolResult != nil:
		u.OfToolResult.CacheControl = cc
	case u.OfContainerUpload != nil:
		u.OfContainerUpload.CacheControl = cc
	case u.OfDocument != nil:
		u.OfDocument.CacheControl = cc
	case u.OfImage != nil:
		u.OfImage.CacheControl = cc
	}
}

// BuildParams translates a provider-neutral llm.Request into the SDK's
// MessageNewParams. Cacheable system blocks, tool defs, and message content
// blocks emit CacheControl: sdk.CacheControlEphemeralParam{} on the
// corresponding param.
// Exported for tests in package anthropic_test.
func BuildParams(req llm.Request) (sdk.MessageNewParams, error) {
	// System blocks.
	system := make([]sdk.TextBlockParam, 0, len(req.System))
	for _, s := range req.System {
		b := sdk.TextBlockParam{Text: s.Text}
		if s.Cacheable {
			b.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		system = append(system, b)
	}

	// Tool definitions. ToolInputSchemaParam already supplies the
	// {"type":"object","properties":...,"required":...} envelope, so extract the
	// inner properties/required rather than passing the whole schema (which would
	// nest a schema-in-a-schema and fail Anthropic's validation). Other top-level
	// keys ride along in ExtraFields so they surface at the top level.
	tools := make([]sdk.ToolUnionParam, 0, len(req.Tools))
	for _, td := range req.Tools {
		// Server tools (web_search, web_fetch, etc.) are identified by
		// ServerType and run by the API; emit the matching SDK variant
		// rather than a custom ToolParam. Description / InputSchema are
		// ignored here — the type identifier carries the schema.
		if td.ServerType != "" {
			var tu sdk.ToolUnionParam
			switch td.ServerType {
			case "web_search_20250305":
				tu = sdk.ToolUnionParam{OfWebSearchTool20250305: &sdk.WebSearchTool20250305Param{}}
			case "web_fetch_20250910":
				tu = sdk.ToolUnionParam{OfWebFetchTool20250910: &sdk.WebFetchTool20250910Param{}}
			case "code_execution_20260120":
				tu = sdk.ToolUnionParam{OfCodeExecutionTool20260120: &sdk.CodeExecutionTool20260120Param{}}
			default:
				return sdk.MessageNewParams{}, fmt.Errorf("anthropic: unknown server tool type %q (custom-tool path expects ServerType to be empty)", td.ServerType)
			}
			tools = append(tools, tu)
			continue
		}
		var raw map[string]any
		if len(td.InputSchema) > 0 {
			if err := json.Unmarshal(td.InputSchema, &raw); err != nil {
				return sdk.MessageNewParams{}, fmt.Errorf("anthropic: tool %q: bad input schema: %w", td.Name, err)
			}
		}
		innerProps := raw["properties"]
		var required []string
		if r, ok := raw["required"].([]any); ok {
			required = make([]string, 0, len(r))
			for _, v := range r {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		}
		extra := map[string]any{}
		for k, v := range raw {
			switch k {
			case "type", "properties", "required":
				// Already handled by the SDK's typed fields above.
			default:
				extra[k] = v
			}
		}
		tp := sdk.ToolParam{
			Name:        td.Name,
			Description: sdk.String(td.Description),
			InputSchema: sdk.ToolInputSchemaParam{
				Properties:  innerProps,
				Required:    required,
				ExtraFields: extra,
			},
		}
		if td.Cacheable {
			tp.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		tools = append(tools, sdk.ToolUnionParam{OfTool: &tp})
	}

	// Messages.
	messages := make([]sdk.MessageParam, 0, len(req.Messages))
	for _, msg := range req.Messages {
		var role sdk.MessageParamRole
		switch msg.Role {
		case "user":
			role = sdk.MessageParamRoleUser
		case "assistant":
			role = sdk.MessageParamRoleAssistant
		default:
			return sdk.MessageNewParams{}, fmt.Errorf("anthropic: unknown message role %q", msg.Role)
		}

		content := make([]sdk.ContentBlockParamUnion, 0, len(msg.Content))
		for _, cb := range msg.Content {
			switch cb.Type {
			case "text":
				content = append(content, sdk.ContentBlockParamUnion{
					OfText: &sdk.TextBlockParam{Text: cb.Text},
				})
			case "tool_use":
				if cb.ToolUse == nil {
					return sdk.MessageNewParams{}, fmt.Errorf("anthropic: tool_use block missing ToolUse field")
				}
				// Input is `any` in the SDK param; pass the raw JSON directly so
				// the SDK marshals it verbatim.
				var inputAny any
				if err := json.Unmarshal(cb.ToolUse.Input, &inputAny); err != nil {
					return sdk.MessageNewParams{}, fmt.Errorf("anthropic: tool_use input: %w", err)
				}
				content = append(content, sdk.ContentBlockParamUnion{
					OfToolUse: &sdk.ToolUseBlockParam{
						ID:    cb.ToolUse.ID,
						Name:  cb.ToolUse.Name,
						Input: inputAny,
					},
				})
			case "tool_result":
				if cb.ToolResult == nil {
					return sdk.MessageNewParams{}, fmt.Errorf("anthropic: tool_result block missing ToolResult field")
				}
				trp := sdk.ToolResultBlockParam{
					ToolUseID: cb.ToolResult.ToolUseID,
					Content: []sdk.ToolResultBlockParamContentUnion{
						{OfText: &sdk.TextBlockParam{Text: cb.ToolResult.Content}},
					},
				}
				if cb.ToolResult.IsError {
					trp.IsError = sdk.Bool(true)
				}
				content = append(content, sdk.ContentBlockParamUnion{
					OfToolResult: &trp,
				})
			case "container_upload":
				content = append(content, sdk.NewContainerUploadBlock(cb.FileID))
			case "document":
				// Always a base64 PDF: the SDK's document source union has no generic
				// "base64 + arbitrary media_type" variant (Base64PDFSourceParam.MediaType
				// is a fixed constant). Because this case emits exactly one wire shape,
				// models.anthropicNativeInput may map only application/pdf to
				// NativeBlockDocument; TestOnlyPDFMapsToNativeBlockDocument keeps a second
				// document MIME from ever arriving here mislabeled as a PDF.
				content = append(content, sdk.ContentBlockParamUnion{
					OfDocument: &sdk.DocumentBlockParam{
						Source: sdk.DocumentBlockParamSourceUnion{
							OfBase64: &sdk.Base64PDFSourceParam{
								Data: base64.StdEncoding.EncodeToString(cb.Data),
							},
						},
					},
				})
			case "image":
				content = append(content, sdk.ContentBlockParamUnion{
					OfImage: &sdk.ImageBlockParam{
						Source: sdk.ImageBlockParamSourceUnion{
							OfBase64: &sdk.Base64ImageSourceParam{
								Data:      base64.StdEncoding.EncodeToString(cb.Data),
								MediaType: sdk.Base64ImageSourceMediaType(cb.MIME),
							},
						},
					},
				})
			default:
				return sdk.MessageNewParams{}, fmt.Errorf("anthropic: unknown content block type %q", cb.Type)
			}

			if cb.Cacheable && len(content) > 0 {
				applyMessageCacheControl(&content[len(content)-1])
			}
		}
		messages = append(messages, sdk.MessageParam{
			Role:    role,
			Content: content,
		})
	}

	params := sdk.MessageNewParams{
		Model:     req.Model,
		MaxTokens: int64(req.MaxTokens),
		System:    system,
		Tools:     tools,
		Messages:  messages,
	}
	if req.UserID != "" {
		params.Metadata = sdk.MetadataParam{UserID: sdk.String(req.UserID)}
	}
	return params, nil
}

// translateResponse converts an SDK Message into a provider-neutral llm.Response.
//
// Usage fields CacheCreationInputTokens and CacheReadInputTokens are present in
// sdk.Usage as of v1.38.0 and are mapped to the corresponding llm.Usage fields.
func translateResponse(out *sdk.Message) llm.Response {
	content := make([]llm.ContentBlock, 0, len(out.Content))
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			tb := block.AsText()
			content = append(content, llm.ContentBlock{
				Type: "text",
				Text: tb.Text,
			})
		case "tool_use":
			tu := block.AsToolUse()
			content = append(content, llm.ContentBlock{
				Type: "tool_use",
				ToolUse: &llm.ToolUseBlock{
					ID:    tu.ID,
					Name:  tu.Name,
					Input: tu.Input,
				},
			})
			// All other block types (thinking, server_tool_use, etc.) are ignored;
			// we only surface text and tool_use to the provider-neutral layer.
		}
	}

	return llm.Response{
		Content:    content,
		StopReason: string(out.StopReason),
		Usage: llm.Usage{
			InputTokens:         out.Usage.InputTokens,
			OutputTokens:        out.Usage.OutputTokens,
			CacheCreationTokens: out.Usage.CacheCreationInputTokens,
			CacheReadTokens:     out.Usage.CacheReadInputTokens,
		},
	}
}

// Compile-time interface check.
var _ llm.Provider = (*Provider)(nil)
