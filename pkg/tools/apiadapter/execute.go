package apiadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// newHTTPClient is the SSRF-guarded client factory. Production is
// safehttp.Client(), which refuses loopback, link-local, private and
// cluster-internal destinations — the same guard every other URL-following
// path here uses. Tests swap it to reach an httptest server, which the real
// guard would (correctly) refuse.
var newHTTPClient = safehttp.Client

// guardHost is the resolved-host SSRF pre-check, applied after URL
// construction in buildRequest: defense-in-depth for a Config that never
// went through Validate (NewExecutor does not force it), where an
// unvalidated Path can move the URL's authority boundary. The dial-time
// guard inside newHTTPClient() remains the authoritative backstop; this
// refuses earlier, with a clearer message. Tests swap it alongside
// newHTTPClient to reach loopback httptest servers.
var guardHost = safehttp.GuardHost

// DefaultMaxResponseBytes caps how much of an upstream response is read into a
// tool result. An adapter is a tool surface for a model, not a proxy: an
// unbounded body would blow the turn's context long before it was useful.
const DefaultMaxResponseBytes = 256 << 10

// Result is one executed call.
type Result struct {
	Status      int
	Body        []byte
	ContentType string
}

// Executor performs a config's operations. Construct one per config; it is
// safe for concurrent use.
type Executor struct {
	cfg        Config
	credential string
	client     *http.Client
	// MaxResponseBytes caps how much of an upstream response Call reads (see
	// DefaultMaxResponseBytes). Set before the first Call; not synchronized.
	MaxResponseBytes int64
}

// NewExecutor binds a validated config to the credential its auth scheme names.
// The credential is passed in rather than read from the environment so the
// engine stays pure and testable; the binary reads cfg.Auth.EnvVar.
func NewExecutor(cfg Config, credential string) *Executor {
	c := newHTTPClient()
	// Go strips Authorization/Cookie on a cross-host redirect but copies
	// every other header verbatim, and for auth.type "query" the credential
	// is IN the URL the Referer carries — so an open-redirect on the
	// legitimate host can walk the credential to an attacker origin.
	// Same-host-only keeps ordinary same-host redirects (a trailing-slash
	// 301, say) working. Wrap (never replace) CheckRedirect so the
	// dial-time SSRF guard newHTTPClient() already installed still runs.
	if u, err := url.Parse(cfg.BaseURL); err == nil { // Validate normally guarantees this; guard anyway
		base, prev := u.Hostname(), c.CheckRedirect
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), base) {
				return fmt.Errorf("apiadapter: refusing redirect off %q to %q: the credential would travel", base, req.URL.Hostname())
			}
			if prev != nil {
				return prev(req, via)
			}
			return nil
		}
	}
	return &Executor{cfg: cfg, credential: credential, client: c, MaxResponseBytes: DefaultMaxResponseBytes}
}

// redact removes the credential from an error's text before it can reach a
// tool result. url.Error embeds the full request URL, and for query auth the
// credential is IN that URL; Go's own redaction covers only userinfo. This
// flattens the error chain (errors.New, not %w) — deliberate: preserving
// %w would let errors.As/Is reconstruct the original text, and a chain that
// can be unwrapped back to the credential defeats the point.
func (e *Executor) redact(err error) error {
	if e.credential == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), e.credential, "[redacted]"))
}

// Call executes one named operation with the supplied arguments. Errors are
// returned for an unknown operation, a missing required argument, a bad URL, a
// refused destination, or a transport failure. A non-2xx upstream response is
// NOT an error — it comes back in Result.Status so the caller can report the
// upstream's own words.
func (e *Executor) Call(ctx context.Context, opName string, args map[string]any) (Result, error) {
	op, ok := e.cfg.Operation(opName)
	if !ok {
		return Result{}, fmt.Errorf("apiadapter: unknown operation %q", opName)
	}
	req, err := e.buildRequest(ctx, op, args)
	if err != nil {
		return Result{}, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return Result{}, e.redact(fmt.Errorf("apiadapter: %s: %w", op.Name, err))
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so a truncated body can be told apart from
	// one that landed exactly at the cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, e.MaxResponseBytes+1))
	if err != nil {
		return Result{}, e.redact(fmt.Errorf("apiadapter: %s: read response: %w", op.Name, err))
	}
	if int64(len(body)) > e.MaxResponseBytes {
		// A silent truncation hands the model broken JSON it cannot explain.
		// Cut back to the cap and say so, so the model can tell the response
		// was cut rather than the upstream API returning garbage.
		body = append(body[:e.MaxResponseBytes:e.MaxResponseBytes], fmt.Sprintf("\n[truncated at %d bytes]", e.MaxResponseBytes)...)
	}
	return Result{Status: resp.StatusCode, Body: body, ContentType: resp.Header.Get("Content-Type")}, nil
}

// buildRequest binds args into the operation's request: path placeholders,
// query values, headers, and a JSON body, then injects the credential.
func (e *Executor) buildRequest(ctx context.Context, op Operation, args map[string]any) (*http.Request, error) {
	path := op.Path
	query := url.Values{}
	header := http.Header{}
	bodyFields := map[string]any{}

	for _, p := range op.Params {
		v, present := args[p.Name]
		if !present || v == nil {
			if p.Required {
				return nil, fmt.Errorf("apiadapter: %s: missing required argument %q", op.Name, p.Name)
			}
			continue
		}
		switch p.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(scalarString(v)))
		case "query":
			query.Set(p.Name, scalarString(v))
		case "header":
			header.Set(p.Name, scalarString(v))
		case "body":
			bodyFields[p.Name] = v
		}
	}

	target, err := url.Parse(strings.TrimRight(e.cfg.BaseURL, "/") + path)
	if err != nil {
		return nil, fmt.Errorf("apiadapter: %s: build url: %w", op.Name, err)
	}
	// Re-guard the RESOLVED host: for a config that went through Validate,
	// a path param can never move it (PathEscape plus the "{seg}" binding
	// rule keep it inside the path), but NewExecutor does not force
	// Validate — an unvalidated op.Path missing its leading "/" can parse
	// as userinfo+host, moving the authority boundary the baseURL check
	// never saw. The dial-time guard inside newHTTPClient() is still the
	// authoritative backstop; this refuses earlier, with a clearer message.
	if err := guardHost(target.Hostname()); err != nil {
		return nil, fmt.Errorf("apiadapter: %s: %w", op.Name, err)
	}

	var body io.Reader
	if len(bodyFields) > 0 {
		b, err := json.Marshal(bodyFields)
		if err != nil {
			return nil, fmt.Errorf("apiadapter: %s: encode body: %w", op.Name, err)
		}
		body = bytes.NewReader(b)
		header.Set("Content-Type", "application/json")
	}

	if err := e.injectAuth(&query, header); err != nil {
		return nil, fmt.Errorf("apiadapter: %s: %w", op.Name, err)
	}
	target.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, op.Method, target.String(), body)
	if err != nil {
		// target.String() — which for auth.type "query" now carries the
		// credential (injectAuth ran just above) — is embedded verbatim in
		// this error by net/http/net/url.
		return nil, e.redact(fmt.Errorf("apiadapter: %s: new request: %w", op.Name, err))
	}
	req.Header = header
	return req, nil
}

// injectAuth applies the config's single auth scheme to the outgoing request.
func (e *Executor) injectAuth(query *url.Values, header http.Header) error {
	if e.cfg.Auth.Type == "none" {
		return nil
	}
	if e.credential == "" {
		return fmt.Errorf("credential is empty (auth.envVar %q was unset)", e.cfg.Auth.EnvVar)
	}
	switch e.cfg.Auth.Type {
	case "bearer":
		header.Set("Authorization", "Bearer "+e.credential)
	case "header":
		header.Set(e.cfg.Auth.Name, e.credential)
	case "basic":
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(e.credential)))
	case "query":
		query.Set(e.cfg.Auth.Name, e.credential)
	}
	return nil
}

// scalarString renders a JSON scalar for a wire position that takes text.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode as float64; render integers without a ".0".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
