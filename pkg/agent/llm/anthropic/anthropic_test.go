package anthropic_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
)

func TestNewRequiresAPIKey(t *testing.T) {
	assert.Panics(t, func() { _ = anthropic.New("") }, "New must panic on empty API key")
}

func TestNewReturnsProvider(t *testing.T) {
	p := anthropic.New("sk-ant-test")
	assert.NotNil(t, p, "New returned nil")
}

func TestBuildParamsRoundTrip(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-4-7",
		MaxTokens: 100,
		System:    []llm.SystemBlock{{Text: "you are a test agent", Cacheable: true}},
		Tools: []llm.ToolDef{{
			Name:        "agent_complete",
			Description: "finish",
			InputSchema: []byte(`{"type":"object"}`),
			Cacheable:   true,
		}},
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "hello"}},
		}},
	}
	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed")

	assert.Equal(t, "claude-opus-4-7", params.Model, "Model")
	assert.EqualValues(t, 100, params.MaxTokens, "MaxTokens")
	require.Len(t, params.System, 1, "System blocks")
	assert.Equal(t, "you are a test agent", params.System[0].Text, "System[0].Text")
	require.Len(t, params.Tools, 1, "Tools")
	require.Len(t, params.Messages, 1, "Messages")

	// System cache-control wired through. CacheControl is a struct; the
	// simplest check is that the JSON encoding contains "cache_control".
	systemJSON, err := json.Marshal(params.System[0])
	require.NoError(t, err, "marshal system[0]")
	assert.True(t, bytes.Contains(systemJSON, []byte(`"cache_control"`)),
		"system block missing cache_control: %s", systemJSON)

	// Tool cache-control wired through.
	toolJSON, err := json.Marshal(params.Tools[0])
	require.NoError(t, err, "marshal tools[0]")
	assert.True(t, bytes.Contains(toolJSON, []byte(`"cache_control"`)),
		"tool def missing cache_control: %s", toolJSON)
}

// TestBuildParams_StampsUserIDMetadata verifies that a non-empty UserID on
// llm.Request is forwarded to sdk.MessageNewParams.Metadata.UserID, and that
// an empty UserID leaves the field omitted (not valid).
func TestBuildParams_StampsUserIDMetadata(t *testing.T) {
	p, err := anthropic.BuildParams(llm.Request{Model: "claude-opus-4-8", MaxTokens: 16, UserID: "sess-uid-123"})
	require.NoError(t, err)
	assert.True(t, p.Metadata.UserID.Valid(), "UserID set ⇒ metadata.user_id should be valid")
	assert.Equal(t, "sess-uid-123", p.Metadata.UserID.Value)

	p2, err := anthropic.BuildParams(llm.Request{Model: "claude-opus-4-8", MaxTokens: 16})
	require.NoError(t, err)
	assert.Equal(t, sdk.MetadataParam{}, p2.Metadata, "no UserID ⇒ metadata is entirely zero-value (omitted, not null)")
}

// TestBuildParams_ServerTool covers all three server-tool routings:
// known web_search type, known web_fetch type, and an unknown type
// that must error. Shared shape: same base llm.Request, only the
// ServerType (and assertion shape) varies.
func TestBuildParams_ServerTool(t *testing.T) {
	baseReq := func(name, serverType string) llm.Request {
		return llm.Request{
			Model:     "claude-opus-4-7",
			MaxTokens: 100,
			Tools: []llm.ToolDef{{
				Name:       name,
				ServerType: serverType,
			}},
			Messages: []llm.Message{{
				Role:    "user",
				Content: []llm.ContentBlock{{Type: "text", Text: "hi"}},
			}},
		}
	}

	cases := []struct {
		name             string
		toolName         string
		serverType       string
		wantErrSubstring string
		wantWebSearch    bool
		wantWebFetch     bool
	}{
		{
			name:          "web_search server-tool: OfWebSearchTool20250305 set, others nil",
			toolName:      "web_search",
			serverType:    "web_search_20250305",
			wantWebSearch: true,
		},
		{
			name:         "web_fetch server-tool: OfWebFetchTool20250910 set, others nil",
			toolName:     "web_fetch",
			serverType:   "web_fetch_20250910",
			wantWebFetch: true,
		},
		{
			name:             "unknown server-tool type: BuildParams returns error mentioning 'unknown server tool type'",
			toolName:         "bogus",
			serverType:       "bogus",
			wantErrSubstring: "unknown server tool type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := anthropic.BuildParams(baseReq(tc.toolName, tc.serverType))
			if tc.wantErrSubstring != "" {
				require.Error(t, err, "expected error for unknown server tool type")
				assert.Contains(t, err.Error(), tc.wantErrSubstring,
					"error message must mention unknown server tool type")
				return
			}
			require.NoError(t, err, "BuildParams must succeed")
			require.Len(t, params.Tools, 1, "Tools")
			tu := params.Tools[0]
			assert.Nil(t, tu.OfTool, "OfTool must not be set on server-tool ToolUnionParam")
			if tc.wantWebSearch {
				assert.NotNil(t, tu.OfWebSearchTool20250305, "OfWebSearchTool20250305 must be set")
				assert.Nil(t, tu.OfWebFetchTool20250910, "OfWebFetchTool20250910 must not be set on web_search")
			}
			if tc.wantWebFetch {
				assert.NotNil(t, tu.OfWebFetchTool20250910, "OfWebFetchTool20250910 must be set")
				assert.Nil(t, tu.OfWebSearchTool20250305, "OfWebSearchTool20250305 must not be set on web_fetch")
			}
		})
	}
}

// TestBuildParams_CodeExecutionServerTool verifies that a ToolDef with
// ServerType "code_execution_20260120" emits the matching SDK server-tool
// variant — mirroring the web_search / web_fetch routing above.
func TestBuildParams_CodeExecutionServerTool(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Tools: []llm.ToolDef{{
			ServerType: "code_execution_20260120",
		}},
	}
	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed")
	require.Len(t, params.Tools, 1, "Tools")
	assert.NotNil(t, params.Tools[0].OfCodeExecutionTool20260120, "OfCodeExecutionTool20260120 must be set")
}

// TestBuildParams_ContainerUploadBlock verifies that a ContentBlock with
// Type "container_upload" emits sdk.NewContainerUploadBlock(FileID) — the
// input-side counterpart to the model's own code-exec file_id output.
func TestBuildParams_ContainerUploadBlock(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "container_upload", FileID: "file_abc"}},
		}},
	}
	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed")
	require.Len(t, params.Messages, 1, "Messages")
	require.Len(t, params.Messages[0].Content, 1, "Content blocks")
	cb := params.Messages[0].Content[0]
	require.NotNil(t, cb.OfContainerUpload, "OfContainerUpload must be set")
	assert.Equal(t, "file_abc", cb.OfContainerUpload.FileID, "FileID")
}

// TestBuildParamsEmitsNativeBlocks locks the wire shape of the two native
// content block types down to the field the SDK actually requires. A native
// image block must reach the SDK as an image param with base64 source; a
// document block (application/pdf is the only MIME the registry maps to
// NativeBlockDocument — see models.anthropicNativeInput) as a base64 PDF
// document param. Getting either wrong is invisible until a real request
// 400s.
func TestBuildParamsEmitsNativeBlocks(t *testing.T) {
	cases := []struct {
		name  string
		block llm.ContentBlock
		check func(t *testing.T, u sdk.ContentBlockParamUnion)
	}{
		{
			name:  "image/png block: emitted as an image param with base64 source",
			block: llm.ContentBlock{Type: "image", MIME: "image/png", Data: []byte("PNGBYTES")},
			check: func(t *testing.T, u sdk.ContentBlockParamUnion) {
				require.NotNil(t, u.OfImage, "image block must populate OfImage")
				require.NotNil(t, u.OfImage.Source.OfBase64, "image source must be base64")
				assert.Equal(t, sdk.Base64ImageSourceMediaTypeImagePNG, u.OfImage.Source.OfBase64.MediaType, "MediaType")
				assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("PNGBYTES")), u.OfImage.Source.OfBase64.Data, "Data must be base64-encoded")
			},
		},
		{
			name:  "application/pdf block: emitted as a base64 document param",
			block: llm.ContentBlock{Type: "document", MIME: "application/pdf", Data: []byte("%PDF-1.7")},
			check: func(t *testing.T, u sdk.ContentBlockParamUnion) {
				require.NotNil(t, u.OfDocument, "document block must populate OfDocument")
				require.NotNil(t, u.OfDocument.Source.OfBase64, "PDF document source must be base64")
				assert.Nil(t, u.OfDocument.Source.OfText, "PDF document must not populate the plain-text source")
				assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")), u.OfDocument.Source.OfBase64.Data, "Data must be base64-encoded")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := anthropic.BuildParams(llm.Request{
				Model:    "claude-opus-4-8",
				Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{tc.block}}},
			})
			require.NoError(t, err, "BuildParams must accept a native block")
			require.Len(t, params.Messages, 1, "one message in, one message out")
			require.Len(t, params.Messages[0].Content, 1, "one block in, one block out")
			tc.check(t, params.Messages[0].Content[0])
		})
	}
}

// TestBuildParams_NoCodeExecutionWhenAbsent is a byte-identical-to-today
// guard: a plain request with no code_execution ServerType and no
// container_upload block must not set OfCodeExecutionTool20260120 on any
// tool.
func TestBuildParams_NoCodeExecutionWhenAbsent(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Tools: []llm.ToolDef{{
			Name:        "agent_complete",
			Description: "finish",
			InputSchema: []byte(`{"type":"object"}`),
		}},
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "hello"}},
		}},
	}
	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed")
	for _, tu := range params.Tools {
		assert.Nil(t, tu.OfCodeExecutionTool20260120, "OfCodeExecutionTool20260120 must not be set when absent from the request")
	}
}
