package icons

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Discoverer fetches favicons from a service's site URL: HTML first (parse
// <link rel="icon"> candidates, rank by format then size, fetch the best),
// falling back to <siteURL>/favicon.ico.
//
// It follows an operator-supplied URL, so HTTPClient MUST be
// pkg/x/safehttp.Client in production — that is what rejects private IPs and
// non-HTTP schemes before any request leaves. Only tests substitute a plain
// http.Client, against httptest servers.
type Discoverer struct {
	HTTPClient   *http.Client
	MaxBodyBytes int64 // HTML body cap (default 1 MiB)
	MaxIconBytes int64 // icon body cap (default 256 KiB)
}

// Default body caps, applied when the corresponding field is unset.
const (
	defaultMaxBodyBytes = 1 << 20
	defaultMaxIconBytes = 256 << 10
)

// bodyCap and iconCap resolve the effective caps WITHOUT writing to the
// receiver. One *Discoverer is shared by the icon resolver across concurrent
// requests, so defaulting in place would be a data race on every request that
// left the field unset — which is every request, since webd sets neither.
func (d *Discoverer) bodyCap() int64 {
	if d.MaxBodyBytes > 0 {
		return d.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

func (d *Discoverer) iconCap() int64 {
	if d.MaxIconBytes > 0 {
		return d.MaxIconBytes
	}
	return defaultMaxIconBytes
}

// Discover walks the HTML-first → favicon.ico fallback chain. Returns
// an Entry with Negative=false on success. Returns an error if no path
// yielded a usable icon — the caller falls back to FallbackSVG.
func (d *Discoverer) Discover(ctx context.Context, siteURL string) (Entry, error) {
	// 1. Fetch HTML root.
	if e, err := d.fromHTML(ctx, siteURL); err == nil {
		return e, nil
	}
	// 2. /favicon.ico fallback.
	if e, err := d.fromFaviconIco(ctx, siteURL); err == nil {
		return e, nil
	}
	return Entry{}, errors.New("icons: discovery exhausted (HTML + favicon.ico both failed)")
}

func (d *Discoverer) fromHTML(ctx context.Context, siteURL string) (Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, siteURL, nil)
	if err != nil {
		return Entry{}, err
	}
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return Entry{}, fmt.Errorf("fetch HTML: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Entry{}, fmt.Errorf("HTML root status %d", resp.StatusCode)
	}
	body := io.LimitReader(resp.Body, d.bodyCap())

	candidates, baseHref, err := parseIconLinks(body)
	if err != nil {
		return Entry{}, fmt.Errorf("parse HTML: %w", err)
	}
	if len(candidates) == 0 {
		return Entry{}, errors.New("no <link rel=\"icon\"> tags found")
	}

	// Resolve relative hrefs against <base href> if present, else siteURL.
	baseURL, err := url.Parse(siteURL)
	if err != nil {
		return Entry{}, fmt.Errorf("parse siteURL: %w", err)
	}
	if baseHref != "" {
		if parsed, err := url.Parse(baseHref); err == nil {
			baseURL = baseURL.ResolveReference(parsed)
		}
	}
	for i := range candidates {
		if abs, err := url.Parse(candidates[i].Href); err == nil {
			candidates[i].AbsURL = baseURL.ResolveReference(abs).String()
		}
	}

	// Rank candidates: format tier (lower = better), then size desc.
	sortCandidates(candidates)

	// Try each in rank order; first successful fetch wins.
	for _, c := range candidates {
		e, err := d.fetchIcon(ctx, c.AbsURL)
		if err != nil {
			continue
		}
		return e, nil
	}
	return Entry{}, errors.New("no candidate fetch succeeded")
}

func (d *Discoverer) fromFaviconIco(ctx context.Context, siteURL string) (Entry, error) {
	u, err := url.Parse(siteURL)
	if err != nil {
		return Entry{}, err
	}
	u.Path = "/favicon.ico"
	u.RawQuery = ""
	u.Fragment = ""
	return d.fetchIcon(ctx, u.String())
}

func (d *Discoverer) fetchIcon(ctx context.Context, iconURL string) (Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return Entry{}, err
	}
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return Entry{}, fmt.Errorf("fetch icon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Entry{}, fmt.Errorf("icon status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	ct = strings.SplitN(ct, ";", 2)[0]
	ct = strings.TrimSpace(strings.ToLower(ct))
	if !isAllowedImageType(ct) {
		return Entry{}, fmt.Errorf("disallowed content-type %q", ct)
	}
	body := io.LimitReader(resp.Body, d.iconCap()+1)
	raw, err := io.ReadAll(body)
	if err != nil {
		return Entry{}, fmt.Errorf("read icon body: %w", err)
	}
	if int64(len(raw)) > d.iconCap() {
		return Entry{}, fmt.Errorf("icon body exceeds %d byte cap", d.iconCap())
	}
	return Entry{Bytes: raw, ContentType: ct}, nil
}

// iconCandidate is one parsed <link rel="icon"> entry.
type iconCandidate struct {
	Rel      string
	Type     string
	Href     string
	AbsURL   string
	SizesMax int // largest dimension declared in sizes="WxH"; 0 if absent
}

// parseIconLinks walks the HTML head looking for icon <link> tags.
// Returns the candidates and the optional <base href> value.
func parseIconLinks(r io.Reader) ([]iconCandidate, string, error) {
	z := html.NewTokenizer(r)
	var out []iconCandidate
	var baseHref string
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return out, baseHref, nil
			}
			return out, baseHref, z.Err()
		case html.SelfClosingTagToken, html.StartTagToken:
			tn, _ := z.TagName()
			tag := string(tn)
			switch tag {
			case "base":
				attrs := readAttrs(z)
				if h, ok := attrs["href"]; ok {
					baseHref = h
				}
			case "link":
				attrs := readAttrs(z)
				rel := strings.ToLower(strings.TrimSpace(attrs["rel"]))
				if !isIconRel(rel) {
					continue
				}
				href := attrs["href"]
				if href == "" {
					continue
				}
				out = append(out, iconCandidate{
					Rel:      rel,
					Type:     strings.ToLower(strings.TrimSpace(attrs["type"])),
					Href:     href,
					SizesMax: parseSizesMax(attrs["sizes"]),
				})
			case "body":
				// <link> tags must be in <head>; bail early when body starts.
				return out, baseHref, nil
			}
		}
	}
}

func readAttrs(z *html.Tokenizer) map[string]string {
	attrs := map[string]string{}
	for {
		k, v, more := z.TagAttr()
		attrs[strings.ToLower(string(k))] = string(v)
		if !more {
			return attrs
		}
	}
}

func isIconRel(rel string) bool {
	// rel may contain multiple values, e.g. "shortcut icon" or "icon mask-icon".
	for _, r := range strings.Fields(rel) {
		switch r {
		case "icon", "shortcut", "apple-touch-icon", "mask-icon", "apple-touch-icon-precomposed":
			return true
		}
	}
	return false
}

func parseSizesMax(s string) int {
	// sizes="64x64" or "16x16 32x32 any". Return the largest declared dimension.
	s = strings.ToLower(s)
	max := 0
	for _, tok := range strings.Fields(s) {
		if tok == "any" {
			// Vector / "any size"; treat as very large.
			if max < 1024 {
				max = 1024
			}
			continue
		}
		parts := strings.SplitN(tok, "x", 2)
		if len(parts) != 2 {
			continue
		}
		w, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		h, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		if w > max {
			max = w
		}
		if h > max {
			max = h
		}
	}
	return max
}

// sortCandidates ranks candidates: format tier then size desc.
func sortCandidates(c []iconCandidate) {
	// Simple O(n^2) insertion sort — typical n is 2-6.
	for i := 1; i < len(c); i++ {
		j := i
		for j > 0 && betterCandidate(c[j], c[j-1]) {
			c[j-1], c[j] = c[j], c[j-1]
			j--
		}
	}
}

func betterCandidate(a, b iconCandidate) bool {
	ta, tb := formatTier(a), formatTier(b)
	if ta != tb {
		return ta < tb
	}
	return a.SizesMax > b.SizesMax
}

// formatTier ranks candidates; LOWER is preferred.
//
// SVG used to rank FIRST, which meant an attacker offering both an SVG and a
// PNG won deterministically. It is now ranked last: isAllowedImageType refuses
// upstream SVG outright, and a candidate that reaches here without a type (a
// bare `mask-icon` rel) must not out-rank an honest PNG.
func formatTier(c iconCandidate) int {
	if c.Type == "image/png" {
		return 2
	}
	if strings.Contains(c.Rel, "apple-touch-icon") {
		return 3
	}
	if c.Type == "image/x-icon" || c.Type == "image/vnd.microsoft.icon" {
		return 4
	}
	if c.Type == "image/webp" {
		return 5
	}
	if c.Type == "image/svg+xml" || strings.Contains(c.Rel, "mask-icon") {
		return 9 // never served from upstream; ranked last so it can never win
	}
	if c.Type == "image/jpeg" || c.Type == "image/jpg" {
		return 6
	}
	// Unknown / missing type — rank below everything declared but
	// above nothing.
	return 7
}

// isAllowedImageType gates what may be FETCHED FROM UPSTREAM and served back.
//
// SVG is deliberately absent, and it is the only removal from the obvious list.
// These bytes are written to the browser with the upstream's own Content-Type,
// on the origin that holds idd_session -- and SVG is the one image format that
// is also a script host. Anyone who can create an MCPServer pointing at a
// siteURL they control (an ordinary tenant action) could therefore serve script
// that runs same-origin with a signed-in admin the moment that admin loaded the
// icon. Every other accepted type is raster and cannot carry script.
//
// This does NOT affect the generated fallback, which is an SVG this package
// authors itself (resolver.go) and never fetches; the response headers in
// NewHandler are the second, independent half that makes even that inert.
func isAllowedImageType(ct string) bool {
	switch ct {
	case "image/png", "image/x-icon", "image/vnd.microsoft.icon",
		"image/webp", "image/jpeg", "image/jpg", "image/gif":
		return true
	}
	return false
}
