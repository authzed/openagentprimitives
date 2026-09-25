package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/bravesearch"
)

// fakeExecutor is a package-local test double for websearch.SearchExecutor —
// NOT websearch.FakeProvider (pkg/tools/websearch/fake.go), which implements
// the ClientTools()/agenttool.Tool half of websearch.Provider (the
// in-runner-meta-tool shape design R1/R2 supersede for this daemon), not
// SearchExecutor. Tests own their fixtures (CLAUDE.md's test conventions).
type fakeExecutor struct {
	searchFn func(ctx context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error)
	fetchFn  func(ctx context.Context, url string) (*websearch.FetchResult, error)
}

func (f *fakeExecutor) Search(ctx context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error) {
	if f.searchFn == nil {
		return nil, errors.New("fakeExecutor: searchFn not set")
	}
	return f.searchFn(ctx, query, opts)
}

func (f *fakeExecutor) Fetch(ctx context.Context, url string) (*websearch.FetchResult, error) {
	if f.fetchFn == nil {
		return nil, errors.New("fakeExecutor: fetchFn not set")
	}
	return f.fetchFn(ctx, url)
}

var _ websearch.SearchExecutor = (*fakeExecutor)(nil)

// failingPutStore embeds a nil artifactstore.Store and overrides only Put,
// so a test can prove a failed spill degrades gracefully without needing a
// full fake implementing every Store method. Any method other than Put
// called on this in a test is a test bug, not production behavior — the
// embedded nil interface panics loudly if that ever happens.
type failingPutStore struct{ artifactstore.Store }

func (failingPutStore) Put(context.Context, string, io.Reader) (artifactstore.Ref, error) {
	return "", errors.New("boom: store unavailable")
}

func rawArgs(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func callTool(ctx context.Context, handler mcp.ToolHandler, args json.RawMessage) (*mcp.CallToolResult, error) {
	return handler(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}})
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "content block must be text")
	return tc.Text
}

// --- search --------------------------------------------------------------

func TestHandleSearch_HappyPath(t *testing.T) {
	var sawQuery string
	var sawOpts websearch.SearchOpts
	exec := &fakeExecutor{searchFn: func(_ context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error) {
		sawQuery, sawOpts = query, opts
		return []websearch.SearchResult{
			{Title: "First", URL: "https://example.test/1", Snippet: "s1"},
			{Title: "Second", URL: "https://example.test/2", Snippet: "s2"},
		}, nil
	}}
	s := NewServer(exec, nil)

	res, err := callTool(context.Background(), s.handleSearch, rawArgs(t, map[string]any{"query": "golang", "limit": 5}))
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))

	assert.Equal(t, "golang", sawQuery)
	assert.Equal(t, 5, sawOpts.MaxResults)

	var got searchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	require.Len(t, got.Results, 2)
	assert.Equal(t, searchResult{Title: "First", URL: "https://example.test/1", Snippet: "s1"}, got.Results[0])
	assert.Equal(t, searchResult{Title: "Second", URL: "https://example.test/2", Snippet: "s2"}, got.Results[1])
}

func TestHandleSearch_ErrorPaths(t *testing.T) {
	cases := []struct {
		name       string
		args       map[string]any
		searchFn   func(context.Context, string, websearch.SearchOpts) ([]websearch.SearchResult, error)
		wantErrSub string
	}{
		{
			// searchFn deliberately left nil: if a regression ever let this
			// call through, fakeExecutor.Search's own nil-guard reports it
			// clearly rather than this reaching for a real network call.
			name:       "missing query: refused before Search is called",
			args:       map[string]any{},
			wantErrSub: "query is required",
		},
		{
			name: "executor error: surfaced verbatim",
			args: map[string]any{"query": "q"},
			searchFn: func(context.Context, string, websearch.SearchOpts) ([]websearch.SearchResult, error) {
				return nil, errors.New("upstream unavailable")
			},
			wantErrSub: "upstream unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(&fakeExecutor{searchFn: tc.searchFn}, nil)
			res, err := callTool(context.Background(), s.handleSearch, rawArgs(t, tc.args))
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Contains(t, resultText(t, res), tc.wantErrSub)
		})
	}
}

// --- fetch: ordering (scheme -> guardHost -> dial) ------------------------

// mustNotFetch fails the test if Fetch is ever invoked — used to prove a
// refusal happened BEFORE any call reached the executor (and so before any
// network dial in production).
func mustNotFetch(t *testing.T) func(context.Context, string) (*websearch.FetchResult, error) {
	return func(context.Context, string) (*websearch.FetchResult, error) {
		t.Fatal("Fetch must not be called")
		return nil, nil
	}
}

func TestHandleFetch_RejectsNonHTTPSScheme(t *testing.T) {
	cases := []string{"http://example.test/page", "ftp://example.test/page", "file:///etc/passwd"}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			s := NewServer(&fakeExecutor{fetchFn: mustNotFetch(t)}, nil)
			res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": u}))
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Contains(t, resultText(t, res), "only https urls are supported")
		})
	}
}

func TestHandleFetch_RejectsInvalidURL(t *testing.T) {
	s := NewServer(&fakeExecutor{fetchFn: mustNotFetch(t)}, nil)
	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://%zz"}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "invalid url")
}

// TestHandleFetch_GuardHostRejectsBeforeDial pins that the REAL (unswapped)
// guardHost fires for a blocked host, and that it does so before the
// executor is ever consulted — a network-free proof of ordering step 2 in
// handleFetch, complementing TestHandleFetch_RealSSRFGuardBlocksLoopback
// which proves step 3 (the dial-time guard) below.
func TestHandleFetch_GuardHostRejectsBeforeDial(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "cloud metadata IP literal", url: "https://169.254.169.254/latest/meta-data"},
		{name: "cloud metadata hostname", url: "https://metadata.google.internal/x"},
		{name: "loopback", url: "https://127.0.0.1/x"},
		{name: "in-cluster service name", url: "https://foo.svc.cluster.local/x"},
		{name: "bare dotless hostname", url: "https://internalhost/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(&fakeExecutor{fetchFn: mustNotFetch(t)}, nil)
			res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": tc.url}))
			require.NoError(t, err)
			require.True(t, res.IsError)
			assert.Contains(t, resultText(t, res), "safehttp", "guardHost's own error must be surfaced verbatim")
		})
	}
}

// --- fetch: content-type allowlist ----------------------------------------

func TestHandleFetch_ContentTypeAllowlist(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		wantAllowed bool
		// wantErrSub distinguishes the two ways a content type is refused:
		// "could not be parsed" (a malformed header) from "unsupported
		// content type" (parsed fine, just not on the allowlist) — see
		// handleFetch's step 4 for why a deny needs to say which.
		wantErrSub string
	}{
		{name: "text/html allowed", contentType: "text/html; charset=utf-8", wantAllowed: true},
		{name: "text/plain allowed", contentType: "text/plain", wantAllowed: true},
		{name: "application/json allowed", contentType: "application/json", wantAllowed: true},
		{name: "text/markdown allowed", contentType: "text/markdown", wantAllowed: true},
		{name: "image refused", contentType: "image/png", wantAllowed: false, wantErrSub: `unsupported content type "image/png"`},
		{name: "octet-stream refused", contentType: "application/octet-stream", wantAllowed: false, wantErrSub: `unsupported content type "application/octet-stream"`},
		{name: "empty content type refused: malformed, not merely unsupported", contentType: "", wantAllowed: false, wantErrSub: "content type header could not be parsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
				return &websearch.FetchResult{URL: u, ContentType: tc.contentType, Body: "hello"}, nil
			}}, nil)
			res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
			require.NoError(t, err)
			if tc.wantAllowed {
				require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))
			} else {
				require.True(t, res.IsError)
				assert.Contains(t, resultText(t, res), tc.wantErrSub)
			}
		})
	}
}

// TestHandleFetch_ContentTypeEchoesParsedMediaTypeNotRawHeader proves the
// success envelope's content_type is the PARSED base media type, not the
// fetched server's raw header — the fix for BLOCKER-1/MAJOR-2's root cause:
// a raw header is attacker-controlled and unbounded (a hostile server can
// pad it with an oversized parameter and still land on an allowlisted base
// type once parsed), so echoing it verbatim would carry that padding into
// the envelope regardless of anything Text does.
func TestHandleFetch_ContentTypeEchoesParsedMediaTypeNotRawHeader(t *testing.T) {
	hostileHeader := "text/html; charset=utf-8; padding=" + strings.Repeat("A", 5000)
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: hostileHeader, Body: "hello"}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.Equal(t, "text/html", got.ContentType, "content_type must be the PARSED base type, never the raw padded header")
}

// --- fetch: HTML reduced to text before the cap ---------------------------

func TestHandleFetch_HTMLReducedToTextBeforeCap(t *testing.T) {
	html := "<html><body><h1>Title</h1><p>First paragraph.</p><p>Second with <b>bold</b>.</p></body></html>"
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/html", Body: html}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.NotContains(t, got.Text, "<p>", "tags must not survive text reduction")
	assert.NotContains(t, got.Text, "<html>")
	assert.Contains(t, got.Text, "Title")
	assert.Contains(t, got.Text, "First paragraph.")
	assert.Contains(t, got.Text, "Second with")
	// Adjacent block elements must not run into one unbroken word.
	assert.NotContains(t, got.Text, "paragraph.Second", "block elements must be separated, not glued together")
	assert.False(t, got.Truncated)
}

func TestHandleFetch_NonHTMLTextPassesThroughUnreduced(t *testing.T) {
	body := "plain body text, no markup here"
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: body}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.Equal(t, body, got.Text)
}

// --- fetch: the cap, truncation, and artifact spillover -------------------

func TestHandleFetch_UnderCap_NotTruncated(t *testing.T) {
	body := strings.Repeat("a", 100)
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: body}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.False(t, got.Truncated)
	assert.Empty(t, got.ArtifactRef)
	assert.Equal(t, body, got.Text)
}

// TestHandleFetch_OverCap_NoStore_TruncatesWithoutArtifactRef proves the
// WHOLE marshaled response — not Text in isolation — never exceeds
// fetchTextCapBytes (itself derived from contentguard.MaxInspectBytes, not
// restated), and that with no artifact store configured, truncated is still
// reported honestly with no artifact_ref field (omitempty leaves it
// entirely absent from the JSON).
//
// The bound is on the SERIALIZED envelope (see fetchTextCapBytes' own doc
// comment): got.Text is necessarily shorter than fetchTextCapBytes by
// however many bytes url/content_type/truncated/quoting consume, so this no
// longer asserts got.Text's own length equals the cap — that was true only
// under the superseded raw-text cap.
func TestHandleFetch_OverCap_NoStore_TruncatesWithoutArtifactRef(t *testing.T) {
	body := strings.Repeat("x", fetchTextCapBytes+500)
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: body}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := resultText(t, res)
	assert.NotContains(t, text, `"artifact_ref"`, "omitempty must drop artifact_ref entirely when store is nil")
	assert.LessOrEqual(t, len(text), fetchTextCapBytes,
		"the WHOLE marshaled response — the same bytes the model receives verbatim — must never exceed the classifier's own inspect window")

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	assert.True(t, got.Truncated)
	assert.Less(t, len(got.Text), len(body), "must actually have truncated, not returned the whole body")
	assert.Empty(t, got.ArtifactRef)
}

// TestHandleFetch_OverCap_WithStore_SpillsCompleteTextAsArtifact proves the
// artifact path: an in-memory store receives the COMPLETE (untruncated)
// text, and the returned artifact_ref resolves back to it.
func TestHandleFetch_OverCap_WithStore_SpillsCompleteTextAsArtifact(t *testing.T) {
	full := strings.Repeat("y", fetchTextCapBytes+1234)
	store := blob.NewMem()
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: full}, nil
	}}, store)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := resultText(t, res)
	assert.LessOrEqual(t, len(text), fetchTextCapBytes,
		"the WHOLE marshaled response must never exceed the classifier's own inspect window")

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	require.True(t, got.Truncated)
	require.NotEmpty(t, got.ArtifactRef)
	assert.Less(t, len(got.Text), len(full), "must actually have truncated, not returned the whole body")

	rc, err := store.Get(context.Background(), artifactstore.Ref(got.ArtifactRef))
	require.NoError(t, err)
	defer rc.Close()
	stored, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, full, string(stored), "the artifact must hold the COMPLETE text, not the excerpt")
}

// TestHandleFetch_ArtifactPutFailure_DegradesGracefully proves a failed
// spill never fails the call outright: the excerpt is still useful output,
// and the failure is logged (CLAUDE.md's no-silent-errors rule) rather than
// silently dropped.
func TestHandleFetch_ArtifactPutFailure_DegradesGracefully(t *testing.T) {
	body := strings.Repeat("z", fetchTextCapBytes+10)
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: body}, nil
	}}, failingPutStore{})

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "a failed spill must not fail the tool call")

	text := resultText(t, res)
	assert.LessOrEqual(t, len(text), contentguard.MaxInspectBytes,
		"the WHOLE marshaled response must stay within the classifier's inspect window even on a degraded spill")

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	assert.True(t, got.Truncated)
	assert.Empty(t, got.ArtifactRef, "no ref when the store write failed")
}

// TestHandleFetch_TruncationStaysOnRuneBoundary proves capToMarshaledBudget
// never splits a multi-byte UTF-8 rune, even when the natural cut point
// would land mid-character.
func TestHandleFetch_TruncationStaysOnRuneBoundary(t *testing.T) {
	// A 3-byte rune (é as a precomposed codepoint, or any non-ASCII rune)
	// repeated so the cap boundary is highly likely to land inside one.
	body := strings.Repeat("€", fetchTextCapBytes) // "€" is 3 bytes in UTF-8
	s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
		return &websearch.FetchResult{URL: u, ContentType: "text/plain", Body: body}, nil
	}}, nil)

	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := resultText(t, res)
	assert.LessOrEqual(t, len(text), contentguard.MaxInspectBytes,
		"the WHOLE marshaled response must stay within the classifier's inspect window")

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	assert.True(t, got.Truncated)
	assert.True(t, utf8.ValidString(got.Text), "truncation must never split a multi-byte rune")
	assert.Less(t, len(got.Text), fetchTextCapBytes,
		"Text must be sized for what fits ALONGSIDE url/content_type/truncated, not the full cap")
}

// TestHandleFetch_ResponseNeverExceedsInspectCap is the range test the fix
// brief for this BLOCKER calls for explicitly: a single fixture would have
// passed the OLD, buggy text-only cap just as easily as it passes the fix —
// Text sized right at fetchTextCapBytes, with the REST of the envelope
// pushing the marshaled total past contentguard.MaxInspectBytes, is exactly
// the shape that slipped through before. Every case below checks the ACTUAL
// bytes handleFetch hands back over the wire — the same bytes
// contentguard.Capped.Inspect caps before the prompt-injection classifier
// runs, and what the model receives verbatim on a Pass — never exceed that
// window, whatever combination of url length, artifact_ref presence, and
// JSON-escaping-heavy content produced them.
func TestHandleFetch_ResponseNeverExceedsInspectCap(t *testing.T) {
	cases := []struct {
		name string
		url  string
		body string
		// contentType, when empty, defaults to "text/plain". Every case
		// before the two MINOR-4 cases below used a short, fixed content
		// type and a url capped at 4000 chars — which is exactly why none
		// of them caught BLOCKER-1: no case here previously exercised an
		// ENVELOPE that alone (url + content_type, Text still empty)
		// exceeds the cap.
		contentType   string
		store         artifactstore.Store
		wantTruncated bool
	}{
		{
			name:          "short url, small body, no artifact ref: untruncated",
			url:           "https://example.test/p",
			body:          "hello, world",
			wantTruncated: false,
		},
		{
			name:          "short url, large body, no store: truncated, no artifact_ref",
			url:           "https://example.test/p",
			body:          strings.Repeat("a", fetchTextCapBytes+5000),
			wantTruncated: true,
		},
		{
			name:          "long url, large body: truncated, the url's own length eats into the text budget too",
			url:           "https://example.test/" + strings.Repeat("p", 4000),
			body:          strings.Repeat("b", fetchTextCapBytes+5000),
			wantTruncated: true,
		},
		{
			name:          "large body with an artifact store configured: truncated, artifact_ref present and counted",
			url:           "https://example.test/p",
			body:          strings.Repeat("c", fetchTextCapBytes+5000),
			store:         blob.NewMem(),
			wantTruncated: true,
		},
		{
			// The exact bug this fix closes: raw text length sitting right at
			// the OLD (buggy) cap looked untruncated to a text-only length
			// check, but every "<" marshals to a 6-byte < escape (Go's
			// default HTML-safe JSON escaping), so the actual wire response
			// this would produce is roughly 6x the cap unless the cap
			// measures the MARSHALED size.
			name:          "text at the old cap's raw length but escape-heavy ('<'): still truncated once marshaled size is measured",
			url:           "https://example.test/p",
			body:          strings.Repeat("<", fetchTextCapBytes),
			wantTruncated: true,
		},
		{
			// The other expansion CLAUDE.md's fix brief calls out by name:
			// quotes and backslashes each cost 1 raw byte but 2 marshaled
			// bytes.
			name:          "text at the old cap's raw length but escape-heavy (quotes/backslashes): still truncated once marshaled size is measured",
			url:           "https://example.test/p",
			body:          strings.Repeat(`"\`, fetchTextCapBytes/2),
			wantTruncated: true,
		},
		{
			// MINOR-4 (the case the fix brief calls for by name): a
			// Content-Type header padded via a parameter to tens of
			// kilobytes still parses (mime.ParseMediaType strips
			// parameters) to the allowlisted base type "text/html" — so it
			// PASSES the allowlist check — but if the raw header were
			// echoed back verbatim (as it was before this fix), the
			// envelope alone would exceed fetchTextCapBytes before a
			// single byte of Text was added: no case above exercises this,
			// because every one of them uses a short, fixed content type.
			// A small body proves the point cleanly — untruncated is only
			// possible at all if the ECHOED content_type stayed small.
			name:          "hostile-sized Content-Type header (allowlisted after parsing), small body: envelope alone must not blow the cap before Text holds a byte",
			url:           "https://example.test/p",
			body:          "hello, small body",
			contentType:   "text/html; padding=" + strings.Repeat("A", 40000),
			wantTruncated: false,
		},
		{
			// The other half MINOR-4 asks for: a very long FINAL url (what
			// fetched.URL actually is — the executor's landing url after
			// any redirects, not merely the requested one) large enough
			// that echoing it raw would alone exceed the cap, same shape
			// as the Content-Type case above but for the url field.
			name:          "very long final url (far past any previous case), large body: url alone must not blow the cap either",
			url:           "https://example.test/" + strings.Repeat("q", 100000),
			body:          strings.Repeat("d", fetchTextCapBytes+5000),
			wantTruncated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := tc.contentType
			if ct == "" {
				ct = "text/plain"
			}
			s := NewServer(&fakeExecutor{fetchFn: func(_ context.Context, u string) (*websearch.FetchResult, error) {
				return &websearch.FetchResult{URL: u, ContentType: ct, Body: tc.body}, nil
			}}, tc.store)

			res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": tc.url}))
			require.NoError(t, err)
			require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))

			text := resultText(t, res)
			assert.LessOrEqual(t, len(text), contentguard.MaxInspectBytes,
				"the WHOLE marshaled response — what the classifier inspects and the model receives — must stay within the cap")

			var got fetchResponse
			require.NoError(t, json.Unmarshal([]byte(text), &got))
			assert.Equal(t, tc.wantTruncated, got.Truncated)
			if tc.store != nil && tc.wantTruncated {
				assert.NotEmpty(t, got.ArtifactRef, "a configured store must have received the spill")
			}
		})
	}
}

func TestHandleFetch_MissingURL(t *testing.T) {
	s := NewServer(&fakeExecutor{fetchFn: mustNotFetch(t)}, nil)
	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "url is required")
}

func TestHandleFetch_ExecutorError(t *testing.T) {
	s := NewServer(&fakeExecutor{fetchFn: func(context.Context, string) (*websearch.FetchResult, error) {
		return nil, errors.New("upstream: connection refused")
	}}, nil)
	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": "https://example.test/page"}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "connection refused")
}

// --- end-to-end against the REAL bravesearch provider ---------------------

// TestHandleFetch_RealProviderEndToEnd wires an actual bravesearch.Provider
// (Task 1's registered production backend) — not a fake — against an
// httptest server, disarming ONLY guardHost (an httptest server is loopback,
// which guardHost correctly refuses on principle) so the daemon's real
// composition (guardHost pre-check -> executor.Fetch, which dials through
// the INJECTED HTTPClient) is exercised end to end exactly as main.go wires
// it, mirroring the two-layer test shape
// pkg/tools/apiadapter/execute_test.go uses for the same reason.
func TestHandleFetch_RealProviderEndToEnd(t *testing.T) {
	orig := guardHost
	guardHost = func(string) error { return nil }
	t.Cleanup(func() { guardHost = orig })

	// NewTLSServer (not NewServer): handleFetch refuses a non-https scheme
	// before guardHost or the executor ever run, so the fixture must offer
	// https for guardHost's disarming to be the thing this test actually
	// exercises.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<p>Hello from a real provider.</p>"))
	}))
	t.Cleanup(srv.Close)

	provider, err := bravesearch.Backend{}.New(websearch.Deps{HTTPClient: srv.Client(), APIKey: "k"})
	require.NoError(t, err)
	exec, ok := provider.(websearch.SearchExecutor)
	require.True(t, ok, "bravesearch.Provider must implement websearch.SearchExecutor")

	s := NewServer(exec, nil)
	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": srv.URL + "/page"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected IsError: %s", resultText(t, res))

	var got fetchResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
	assert.Contains(t, got.Text, "Hello from a real provider.")
	assert.False(t, got.Truncated)
}

// TestHandleFetch_RealSSRFGuardBlocksLoopback proves the OPPOSITE of the
// test above: with guardHost left real (unswapped), a loopback destination
// is refused before the executor's dial — this is what makes the swap in
// TestHandleFetch_RealProviderEndToEnd a deliberate test-only bypass rather
// than a gap in production.
func TestHandleFetch_RealSSRFGuardBlocksLoopback(t *testing.T) {
	// NewTLSServer for the same reason as TestHandleFetch_RealProviderEndToEnd:
	// this must fail at guardHost (an https URL to a loopback host), not at
	// the earlier scheme check, or the assertion below would be proving the
	// wrong layer.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the guarded request must never reach the server")
	}))
	t.Cleanup(srv.Close)

	provider, err := bravesearch.Backend{}.New(websearch.Deps{HTTPClient: srv.Client(), APIKey: "k"})
	require.NoError(t, err)
	exec := provider.(websearch.SearchExecutor)

	s := NewServer(exec, nil)
	res, err := callTool(context.Background(), s.handleFetch, rawArgs(t, map[string]any{"url": srv.URL + "/page"}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "safehttp")
}

// --- Register: the surface a real MCP client sees --------------------------

func TestRegister_AdvertisesSearchAndFetch(t *testing.T) {
	s := NewServer(&fakeExecutor{}, nil)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	s.Register(mcpSrv)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/mcp", HTTPClient: httpSrv.Client()}, nil)
	require.NoError(t, err)
	defer mcpSess.Close()

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	descs := map[string]string{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		descs[tl.Name] = tl.Description
	}
	assert.ElementsMatch(t, []string{"search", "fetch"}, names)
	assert.Contains(t, descs["fetch"], "truncated", "fetch's description must teach the model what truncated means")
	assert.Contains(t, descs["fetch"], "https", "fetch's description must say it is https-only")
}

// --- helpers ---------------------------------------------------------------

func TestToolErr(t *testing.T) {
	res := toolErr("fetch: %s", "boom")
	require.True(t, res.IsError)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "fetch: boom", text.Text)
}

func TestDecodeArgs(t *testing.T) {
	var args map[string]any

	require.NoError(t, decodeArgs(nil, &args))
	assert.Nil(t, args)

	require.NoError(t, decodeArgs(&mcp.CallToolRequest{}, &args))
	assert.Nil(t, args)

	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{"url":"https://x"}`)}}
	require.NoError(t, decodeArgs(req, &args))
	assert.Equal(t, map[string]any{"url": "https://x"}, args)
}

// TestCapToMarshaledBudget replaces the old TestTruncateAtRuneBoundary: the
// helper it exercised was renamed/re-scoped from a raw-byte cap to a
// MARSHALED-byte budget (see capToMarshaledBudget's own doc comment and
// fetchTextCapBytes' — the fix that made the classifier's inspect-window
// invariant hold against the SERIALIZED response, not the raw text alone).
// The old assertions asserted the OLD (raw-byte) semantics and no longer
// describe the current function, so they are replaced here rather than
// patched — CLAUDE.md's "comments describe current code" rule applies to
// tests as much as production code.
func TestCapToMarshaledBudget(t *testing.T) {
	assert.Equal(t, "hello", capToMarshaledBudget("hello", 100), "under the budget: unchanged")
	assert.Equal(t, "", capToMarshaledBudget("hello", 0), "non-positive budget: empty")
	assert.Equal(t, "", capToMarshaledBudget("hello", -1), "negative budget: empty")

	// `"he"` marshals to exactly 4 bytes (2 quotes + 2 ASCII chars): a budget
	// of exactly that keeps it whole, and one byte less drops a character —
	// proving the budget is measured against the MARSHALED (quoted) form,
	// not the raw byte count (a raw-byte budget of 4 would fit "hell").
	assert.Equal(t, "he", capToMarshaledBudget("hello", 4))
	assert.Equal(t, "h", capToMarshaledBudget("hello", 3))

	// A multi-byte rune must never be split, even when the byte budget would
	// otherwise land mid-character.
	multi := "a€b" // 'a'(1) + '€'(3 bytes) + 'b'(1) = 5 raw bytes
	got := capToMarshaledBudget(multi, 5)
	assert.True(t, utf8.ValidString(got), "must never cut mid-rune")
	quoted, err := json.Marshal(got)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(quoted), 5, "result must fit the marshaled budget")
}
