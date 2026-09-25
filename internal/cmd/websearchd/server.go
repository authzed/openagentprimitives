// server.go turns one websearch.SearchExecutor into two MCP tools — search
// and fetch. This file never reads an environment variable — main.go owns
// the env contract and hands this Server an already-constructed executor and
// an already-opened (or nil) artifact store, exactly as
// internal/cmd/apiadapter's server.go takes an already-parsed Config and an
// already-resolved credential.
//
// fetch is the entire security surface of this daemon (see the design doc's
// §3 R4 and §4): every check below runs in the stated order, and each one
// exists to close a specific gap the one before it leaves open.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// fetchTextCapBytes is DERIVED from, not restated from, the prompt-injection
// classifier's own inspect limit (pkg/authz/contentguard.MaxInspectBytes).
// Design R4: the classifier only ever inspects the first MaxInspectBytes of
// a tool result — and what it inspects (contentguard.Capped.Inspect's
// Subject.Result) is this handler's WHOLE marshaled fetchResponse, the same
// bytes the model receives verbatim on a Pass, not the Text field alone. So
// this constant bounds the SERIALIZED response — url, content_type, text,
// truncated, artifact_ref together — never Text in isolation.
//
// The bound is enforced ONCE, at boundedResult (jsonResult and toolErr both
// route through it) — not here, and not per field. handleFetch's own
// Text-vs-envelope budgeting below still exists and still matters (it is
// what makes the cap fire rarely instead of on every oversized fetch), but
// it is hygiene, not the guarantee: round 1 of this fix capped Text alone,
// round 2 capped Text against the measured envelope, and both were
// per-field reasoning that a later field the earlier arithmetic never
// priced in went on to defeat (round 2's own envelope — driven by a
// Content-Type header that stayed within the allowlist after parsing while
// still being tens of kilobytes on the wire — blew the cap before Text held
// a single byte). boundedResult is what makes "no result this daemon
// returns exceeds fetchTextCapBytes" true regardless of which field, or
// which future tool, produced the overage. See its doc comment.
//
// Referencing the constant, rather than restating "32 * 1024" here, is what
// keeps this bound in step with the classifier's own limit if that limit
// ever moves; a hardcoded copy would silently drift the moment the two
// diverge. And the bound this constant enforces is unconditional — it holds
// whether or not any inspector is even wired up — but "every byte the model
// saw from fetch was CLASSIFIED" is not: a promptinjection inspector is
// opt-in per cluster, so that stronger claim holds only where an admin has
// configured one. This constant, and boundedResult, guarantee the size
// bound either way; they cannot guarantee an inspector exists to use it.
const fetchTextCapBytes = contentguard.MaxInspectBytes

// fetchURLCapBytes bounds the URL fetchResponse echoes back. fetched.URL is
// the FINAL url the executor's HTTP client landed on — after any redirects
// — so it is exactly as attacker-influenced as the Content-Type header
// was: nothing here asked for it to be short, and a redirect chain ending
// on a server-chosen query string isn't bounded by anything the model
// requested. This is hygiene, same as echoing the parsed mediaType instead
// of the raw header below — it exists to keep the envelope small enough
// that Text still gets a useful budget, and to reduce how often
// boundedResult's fallback has to fire, not because a URL needs its own
// cap-and-classify story the way Text does.
const fetchURLCapBytes = 4096

// urlTruncationMarker is appended to a URL boundedURL had to cut, so the
// model can tell "this is the whole final URL" from "this was longer and
// got cut" — the same reason fetchDescription documents what Truncated
// means for Text, applied to this field too.
const urlTruncationMarker = "...[truncated]"

// boundedURL returns u unchanged when it already fits fetchURLCapBytes, or
// a rune-safe prefix of it plus urlTruncationMarker, sized so the result
// (marker included) never exceeds fetchURLCapBytes.
func boundedURL(u string) string {
	if len(u) <= fetchURLCapBytes {
		return u
	}
	return truncateRunes(u, fetchURLCapBytes-len(urlTruncationMarker)) + urlTruncationMarker
}

// allowedFetchContentTypes is the set of base media types (charset and other
// parameters stripped by mime.ParseMediaType before this lookup) fetch will
// read as text. Everything else — images, video, arbitrary binaries,
// application/pdf (no extraction here; see the design doc's §7 "out of
// scope") — is refused outright rather than handed to the model as noise a
// text-oriented cap and text-oriented classifier were never built for.
var allowedFetchContentTypes = map[string]bool{
	"text/html":        true,
	"text/plain":       true,
	"text/markdown":    true,
	"application/json": true,
	"application/xml":  true,
	"text/xml":         true,
}

// guardHost is the fast, legible pre-dial SSRF refusal (safehttp.GuardHost).
// Package-level so tests can disarm it to reach an httptest loopback server
// — GuardHost correctly refuses 127.0.0.1 on principle, same as production
// would refuse a real internal address. The dial-time guard inside the
// executor's own injected HTTPClient (safehttp.Client() in production,
// wired in main.go — never here) remains the authoritative backstop
// regardless of this var; this is the same two-layer shape
// pkg/tools/apiadapter's execute.go uses for its own guardHost var.
var guardHost = safehttp.GuardHost

// Server serves search and fetch as MCP tools over one
// websearch.SearchExecutor.
type Server struct {
	exec websearch.SearchExecutor
	// store is nil when no artifact store is configured — a true nil
	// interface (CLAUDE.md's nil-interface rule), never a typed-nil pointer.
	store artifactstore.Store
}

// NewServer binds exec (the registry-selected provider's Search/Fetch) and
// an optional artifact store for fetch's over-cap spillover.
func NewServer(exec websearch.SearchExecutor, store artifactstore.Store) *Server {
	return &Server{exec: exec, store: store}
}

// Register adds the search and fetch tools.
func (s *Server) Register(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name:        "search",
		Description: searchDescription,
		InputSchema: searchInputSchema,
	}, s.handleSearch)
	mcpSrv.AddTool(&mcp.Tool{
		Name:        "fetch",
		Description: fetchDescription,
		InputSchema: fetchInputSchema,
	}, s.handleFetch)
}

// searchDescription tells the model this is the cheap, safe half: titles,
// URLs and snippets only, no page bodies — fetch is the tool that reads a
// page.
const searchDescription = "Search the web for results matching the query. Returns only titles, URLs, " +
	"and short snippets — no page bodies. Call fetch on a result's url to read its content."

var searchInputSchema = json.RawMessage(`{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"query": {"type": "string", "description": "Search query"},
		"limit": {"type": "integer", "minimum": 1, "maximum": 20, "description": "Max results to return (provider default/cap applies above 20)"}
	},
	"required": ["query"]
}`)

// searchResult mirrors websearch.SearchResult's fields under this tool's own
// wire names, rather than marshaling the internal type directly, so this
// daemon's MCP-facing JSON shape can't drift silently if that internal type
// ever grows an unrelated field.
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

func (s *Server) handleSearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := decodeArgs(req, &args); err != nil {
		return toolErr("search: decode arguments: %v", err), nil
	}
	if args.Query == "" {
		return toolErr("search: query is required"), nil
	}

	results, err := s.exec.Search(ctx, args.Query, websearch.SearchOpts{MaxResults: args.Limit})
	if err != nil {
		return toolErr("search: %v", err), nil
	}

	out := searchResponse{Results: make([]searchResult, 0, len(results))}
	for _, r := range results {
		out.Results = append(out.Results, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return jsonResult(out)
}

// fetchDescription is the whole reason CLAUDE.md's "say in the tool's
// description what truncated means" instruction exists: a model that cannot
// tell "this is all of it" from "this is the first part" reasons wrongly
// about both — the same failure mode query_memory/search_memory's own
// Truncated field docs guard against.
const fetchDescription = "Fetch the text content of a single https URL. http:// URLs are refused outright and " +
	"unconditionally, regardless of the current egress policy — not because plain HTTP can never reach anywhere " +
	"from here, but so the refusal never depends on which configuration happens to leave port 80 open today. " +
	"Only text-shaped content is read (html, plain text, markdown, json, xml); anything else is refused. HTML " +
	"is reduced to plain text before it is measured against the size cap. `truncated` tells you which of two " +
	"things happened: false means `text` IS the entire page; true means the page continued past roughly 32KB " +
	"of extracted text and `text` holds only the beginning. When `truncated` is true, `artifact_ref` — if " +
	"present — is a handle to the COMPLETE text; read the rest through your own artifact-reading tool " +
	"(if you have one) rather than treating the excerpt as the whole page. When `truncated` is true and no " +
	"`artifact_ref` is present, only the excerpt is available."

var fetchInputSchema = json.RawMessage(`{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"url": {"type": "string", "description": "https URL to fetch"}
	},
	"required": ["url"]
}`)

// fetchResponse is this tool's wire shape: {url, contentType, text,
// truncated, artifactRef?} per the design doc's §4.
type fetchResponse struct {
	URL string `json:"url"`
	// ContentType is the PARSED base media type — mime.ParseMediaType's
	// first return value, with charset/other parameters stripped — never
	// the raw Content-Type header. handleFetch already computes this value
	// to check the allowlist; echoing it here instead of fetched.ContentType
	// verbatim is what keeps this field small even when the fetched
	// server's own header wasn't (a hostile server can pad a header with an
	// arbitrarily large parameter and still land on an allowlisted base
	// type once parsed).
	ContentType string `json:"content_type"`
	Text        string `json:"text"`
	// Truncated — see fetchDescription; documented once there for the model
	// and repeated here only as a pointer to it, so the two can't drift.
	Truncated bool `json:"truncated"`
	// ArtifactRef, present only when Truncated and an artifact store is
	// configured, is a handle to the COMPLETE extracted text.
	ArtifactRef string `json:"artifact_ref,omitempty"`
}

func (s *Server) handleFetch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := decodeArgs(req, &args); err != nil {
		return toolErr("fetch: decode arguments: %v", err), nil
	}
	if args.URL == "" {
		return toolErr("fetch: url is required"), nil
	}

	// 1. Scheme. Verified against pkg/controllers/agentsession/netpol.go
	// before writing this: a session's RUNNER pod egress rule
	// (BuildRunnerNetworkPolicy) opens only TCP 443/6443, never 80, while a
	// separate-pod SIDECAR's own allowlist-mode rule
	// (cosidecar.AllowlistEgressRules, what this daemon runs under per the
	// design doc's R5 isolation) happens to open 80 as well — so whether
	// plain HTTP reaches anywhere at all depends on which pod this process
	// runs in and which network mode its SidecarToolbox is given, not on
	// anything this handler can see. That is exactly the mode-asymmetry
	// CLAUDE.md warns about: relying on it would let http:// work in
	// whichever configuration happens to leave 80 open today and simply
	// hang the moment that egress shape tightens (a stricter cluster
	// policy, a future narrowing of the sidecar rule) — with nothing here
	// to say why. Refusing http:// outright removes the dependency instead
	// of inheriting it.
	u, err := url.Parse(args.URL)
	if err != nil {
		return toolErr("fetch: invalid url %q: %v", args.URL, err), nil
	}
	if u.Scheme != "https" {
		return toolErr("fetch: only https urls are supported (got %q); http:// is not reliably reachable from this platform's egress policy and would hang rather than fail", u.Scheme), nil
	}

	// 2. A fast, legible pre-dial refusal — no DNS lookup, no dial attempt,
	// just a clear message naming the blocked host before any network cost
	// is spent on it.
	if err := guardHost(u.Hostname()); err != nil {
		return toolErr("fetch: %v", err), nil
	}

	// 3. The actual dial, through the executor's own HTTPClient
	// (safehttp.Client() in production — see main.go's run()). This is the
	// authoritative guard: it resolves the host, checks every resolved IP,
	// and dials the vetted IP directly — closing the DNS-rebinding gap
	// guardHost's static, pre-dial check above cannot.
	fetched, err := s.exec.Fetch(ctx, args.URL)
	if err != nil {
		return toolErr("fetch: %v", err), nil
	}

	// 4. Content-type allowlist. mediaType (never fetched.ContentType, the
	// raw header) is what fetchResponse.ContentType echoes back on success
	// and what any deny message below names — see that field's doc
	// comment for why the raw header never appears in either place. The
	// parse error is kept, not discarded: a deny needs to say which of two
	// different things happened — "this header could not be parsed at all"
	// names a malformed upstream response, "this parsed fine but isn't on
	// the allowlist" names one this handler simply won't read — and
	// collapsing both into one message hides that distinction from whoever
	// is debugging a refused fetch.
	mediaType, _, mtErr := mime.ParseMediaType(fetched.ContentType)
	switch {
	case mtErr != nil:
		return toolErr("fetch: content type header could not be parsed: %v (accepted: text/html, text/plain, text/markdown, application/json, application/xml, text/xml)", mtErr), nil
	case !allowedFetchContentTypes[mediaType]:
		return toolErr("fetch: unsupported content type %q (accepted: text/html, text/plain, text/markdown, application/json, application/xml, text/xml)", mediaType), nil
	}

	// 5. HTML reduced to text BEFORE measuring — see htmltotext.go's doc
	// comment for why, and for the library choice.
	text := fetched.Body
	if mediaType == "text/html" {
		text = htmlToText(text)
	}

	resp := fetchResponse{URL: boundedURL(fetched.URL), ContentType: mediaType}

	// 6. The cap bounds the SERIALIZED response, not the reduced text in
	// isolation — see fetchTextCapBytes's doc comment for why. Try the
	// response with the FULL text first; only fall back to truncating when
	// ITS marshaled size overruns the cap, and only then by measuring what
	// actually fits, never by assuming a fixed margin (a margin has to be
	// sized for a worst case and silently under-protects the moment a field
	// is added to fetchResponse).
	untruncated := resp
	untruncated.Text = text
	fullBytes, err := json.Marshal(untruncated)
	if err != nil {
		return toolErr("fetch: marshal result: %v", err), nil
	}
	if len(fullBytes) <= fetchTextCapBytes {
		resp.Text = text
		return jsonResult(resp)
	}

	resp.Truncated = true
	if s.store != nil {
		key := "websearchd/fetch/" + uuid.New().String() + ".txt"
		ref, perr := s.store.Put(ctx, key, strings.NewReader(text))
		if perr != nil {
			// Not fatal to the call — the excerpt is still useful output —
			// but never silently dropped, per CLAUDE.md's no-silent-errors
			// rule: an operator needs to be able to grep for this.
			log.Printf("ap-websearchd: store full fetch body for %s failed: %v", fetched.URL, perr)
		} else {
			resp.ArtifactRef = string(ref)
		}
	}

	// Measure the envelope EXACTLY as this call will return it: url,
	// truncated (now true) and artifact_ref (now set, if a store took the
	// spill) already at their real values for THIS call, Text still at its
	// zero value "" — so resp itself, right now, IS that envelope, and the
	// overhead below is this call's own overhead (this URL's length, this
	// artifact_ref's length), never an assumed one.
	overhead, err := json.Marshal(resp)
	if err != nil {
		return toolErr("fetch: marshal result: %v", err), nil
	}
	// json.Marshal("") contributes 2 bytes (`""`) already counted in
	// len(overhead); growing Text from "" to its real value only ADDS
	// len(json.Marshal(text)) - 2 marshaled bytes on top of that, so the
	// budget available for the quoted, escaped text is the remaining room
	// plus those 2 bytes handed back.
	budget := fetchTextCapBytes - len(overhead) + len(`""`)
	resp.Text = capToMarshaledBudget(text, budget)
	return jsonResult(resp)
}

// capToMarshaledBudget returns the longest rune-safe prefix of text whose
// MARSHALED (JSON-quoted, escaped) form is at most budget bytes — never the
// longest prefix by raw byte or rune count. Escaping only ever grows a
// string: a quote or backslash becomes two bytes, a control character or (by
// encoding/json's default HTML-safe escaping) '<', '>', '&' becomes a
// six-byte \u00XX sequence. Measuring raw length and assuming escaping is
// free would under-count for exactly the content an attacker gets to choose
// — the same gap this whole fix closes one level up.
//
// Binary search walks RUNE boundaries, not byte offsets: a candidate cut
// point has to land where a rune starts anyway (never mid-rune), and a
// prefix's marshaled length is monotonically non-decreasing as more runes
// are added, so bisecting over rune count is exact rather than approximate.
func capToMarshaledBudget(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if quoted, _ := json.Marshal(text); len(quoted) <= budget {
		return text
	}

	offsets := runeOffsets(text)
	lo, hi, best := 0, len(offsets)-1, 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if quoted, _ := json.Marshal(text[:offsets[mid]]); len(quoted) <= budget {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return text[:offsets[best]]
}

// runeOffsets returns the byte offset of the start of every rune in s, plus
// a final entry equal to len(s) — so offsets[i] is where the i-th rune
// begins, offsets[n] (n = the rune count) is s's own byte length, and
// text[:offsets[k]] is always exactly the first k runes of text, never a
// partial one.
func runeOffsets(s string) []int {
	offsets := make([]int, 0, len(s)+1)
	for i := range s { // ranging over a string yields each rune's start byte index
		offsets = append(offsets, i)
	}
	return append(offsets, len(s))
}

// truncateRunes returns the longest prefix of s whose byte length is at
// most max, cut at a rune boundary — reusing runeOffsets' rune-safety
// (never split a multi-byte character) for a plain raw-byte bound, as
// opposed to capToMarshaledBudget's bound on a MARSHALED length. boundedURL
// is the one caller today.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	offsets := runeOffsets(s)
	cut := 0
	for _, o := range offsets {
		if o > max {
			break
		}
		cut = o
	}
	return s[:cut]
}

// boundedResult is the CHOKE POINT every tool result this daemon returns —
// a success via jsonResult, a failure via toolErr — passes through last,
// and the one place "no marshaled result leaves this daemon over
// fetchTextCapBytes" is enforced. Not per field, and not per tool: round 1
// of this fix capped Text; round 2 capped Text against the measured
// envelope; both were per-field reasoning, and both were beaten by a field
// the earlier arithmetic never priced in (round 2's own envelope blew the
// cap before Text held a single byte, driven by a Content-Type header that
// stayed within the allowlist after parsing while still being tens of
// kilobytes on the wire). A new field on fetchResponse, or a new tool's own
// response type added to this file later, inherits this bound for free by
// returning through jsonResult/toolErr — it does not get its own budget
// arithmetic, and does not need one.
//
// toolErr routes through here too, deliberately: contentguard's
// prompt-injection inspector has no IsError skip, so an oversized error
// result is exactly as capable of carrying unscanned bytes past the
// classifier as an oversized success result would be.
//
// On overflow the ENTIRE result is replaced by a small fixed message —
// never a truncated prefix of the oversized text — because that message's
// size does not depend on anything the caller computed; trusting a
// caller's own arithmetic (a measured envelope, an assumed margin) is
// exactly what rounds 1 and 2 both did and both got wrong. This path is
// the backstop, not the primary defense: handleFetch's own Text-vs-envelope
// budget, the mediaType echo, and boundedURL above all exist to keep this
// from firing in practice; see fetchTextCapBytes' doc comment.
//
// The bound this function enforces — no result over fetchTextCapBytes
// bytes — holds unconditionally. Whether every byte a model receives was
// also INSPECTED by a classifier is a stronger claim that holds only where
// a cluster has a promptinjection inspector configured — it is opt-in, not
// every cluster runs one — which this daemon has no way to observe.
func boundedResult(text string, isError bool) *mcp.CallToolResult {
	if len(text) > fetchTextCapBytes {
		text = fmt.Sprintf("result exceeded the %d-byte output cap and was rejected before reaching the model", fetchTextCapBytes)
		isError = true
	}
	return &mcp.CallToolResult{
		IsError: isError,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// jsonResult marshals v as this tool call's successful text result, then
// routes it through boundedResult — see that function's doc comment for
// why the size bound lives there rather than here.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return toolErr("marshal result: %v", err), nil
	}
	return boundedResult(string(b), false), nil
}

// decodeArgs decodes req's raw MCP tool-call arguments into out. This
// daemon is a LEAF MCP server: the operation_id/_reason/args envelope a
// SidecarToolbox-sourced tool sees on the LLM-facing schema is unwrapped by
// the runner's MCP dispatcher before the call ever reaches here (mirrors
// internal/cmd/apiadapter's decodeArgs, word for word, for the same reason).
// A nil Params or empty Arguments (a tool called with no args at all) leaves
// out at its zero value rather than erroring, mirroring encoding/json's own
// treatment of a missing field.
func decodeArgs(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, out)
}

// toolErr builds a plain-text IsError CallToolResult. Every tool-call
// failure in this binary returns one of these, never a bare Go error to the
// transport — per CLAUDE.md's no-silent-errors rule. Routed through
// boundedResult like every other result this daemon returns (see that
// function's doc comment) — a formatted error message can itself be
// attacker-sized (a deny path can echo a fetched header or a caller-chosen
// URL), so this is not exempt from the same bound jsonResult enforces.
func toolErr(format string, args ...any) *mcp.CallToolResult {
	return boundedResult(fmt.Sprintf(format, args...), true)
}
