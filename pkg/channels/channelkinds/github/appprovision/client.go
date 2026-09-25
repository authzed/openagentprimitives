// pkg/channels/channelkinds/github/appprovision/client.go
//
// The exchange leg of GitHub's App-manifest flow (see manifest.go): swap the
// one-time `code` GitHub redirected back with for the created App's identity
// and secrets. Modeled on
// pkg/channels/channelkinds/slack/appprovision/client.go — same option-func
// shape, same "never log secret bytes" discipline.
package appprovision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// defaultBaseURL is GitHub's API host. Overridden by WithBaseURL in tests.
const defaultBaseURL = "https://api.github.com"

// defaultTimeout bounds one call. Long enough for a slow network, short
// enough that a wizard watching a terminal has not concluded the tool is
// wedged.
const defaultTimeout = 30 * time.Second

// maxResponseBodyBytes bounds how much of the conversions response this
// client will read, so a misbehaving or malicious server can't force
// unbounded memory growth.
const maxResponseBodyBytes = 1 << 20 // 1 MiB

// endpointConversions names the exchange call in error messages. It is the
// endpoint SHAPE, with the literal "{code}" placeholder, not the concrete
// request: the real URL embeds the one-time exchange code as a path segment,
// and that URL — code included — must never appear in a returned error. See
// Convert's doc for why an innocent-looking %w around an *http.Client.Do or
// http.NewRequestWithContext error would leak it (both wrap *url.Error,
// whose Error() string is "POST <full-url>: <cause>").
const endpointConversions = "/app-manifests/{code}/conversions"

// Conversion is the App identity and secrets minted by exchanging a
// manifest-flow code. PEM and WebhookSecret are secret material and are
// wrapped in sensitive.SensitiveValue — the same type
// pkg/platform/identity/credkind/githubapp uses for this App's minted
// installation tokens — so that a stray %v, a structured-log line built from
// this struct, or a json.Marshal of it redacts to "[REDACTED]" instead of
// dumping the private key in cleartext. The PEM in particular mints
// installation tokens indefinitely, making it the highest-value secret in
// this whole feature; callers reach the real bytes only via
// UnderlyingValue(), called as late as possible, at the point they are
// written into a k8s Secret.
//
// AppID and Slug are identifiers, not secrets, and are deliberately left as
// plain strings — wrapping them would make the type meaningless.
type Conversion struct {
	// AppID is the GitHub App's numeric id, stringified — the same value the
	// githubApp credkind expects under the "app-id" Secret key.
	AppID string
	// Slug is the App's URL-safe name.
	Slug string
	// PEM is the App's RS256 private key, PEM-encoded. Never logged — see
	// the type doc above.
	PEM sensitive.SensitiveValue
	// WebhookSecret is the shared secret GitHub generated for this App's
	// webhook HMAC. Never logged — see the type doc above.
	WebhookSecret sensitive.SensitiveValue
}

// HTTPClient talks to the real GitHub API.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// Option configures an HTTPClient.
type Option func(*HTTPClient)

// WithBaseURL points the client at a stand-in GitHub.
func WithBaseURL(u string) Option {
	return func(c *HTTPClient) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// NewHTTPClient returns a client for the real GitHub API unless told
// otherwise.
func NewHTTPClient(opts ...Option) *HTTPClient {
	c := &HTTPClient{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// conversionResponse is the shape GitHub's app-manifests/{code}/conversions
// endpoint returns. Decoded into an unexported struct rather than a map so a
// missing field is a Go zero value we check explicitly, not a silent nil.
type conversionResponse struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

// Convert exchanges the one-time code GitHub's App-manifest flow redirected
// back with for the created App's identity and secrets. This is the only
// call in the flow — GitHub both created the App and, because hook_attributes
// was in the manifest (see manifest.go), registered its webhook, in the same
// operation that minted this code. Convert just reads back what was minted.
//
// code is single-use and short-lived. It is NEVER placed in a returned
// error, by design: it appears only in the request URL's path, and any
// implementation that wraps an error from http.NewRequestWithContext or
// (*http.Client).Do with %w would embed that URL — code included — into the
// resulting error's message, because both return *url.Error, whose Error()
// method renders the full request URL. Every error path below is built from
// literals and status codes only, never from the request URL or the
// response body, so the code (and PEM/WebhookSecret, once decoded) can never
// surface in an error string.
func (c *HTTPClient) Convert(ctx context.Context, code string) (*Conversion, error) {
	url := c.baseURL + "/app-manifests/" + code + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		// Deliberately NOT %w-wrapped: err here is a *url.Error whose Error()
		// string embeds the URL we just built, code included.
		return nil, fmt.Errorf("github %s: build request failed", endpointConversions)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := c.http.Do(req)
	if err != nil {
		// Same reasoning as above: (*http.Client).Do also returns *url.Error.
		return nil, fmt.Errorf("github %s: call failed", endpointConversions)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("github %s: read response: %w", endpointConversions, err)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		// Deliberately excludes the response body: GitHub's error body is not
		// secret today, but nothing guarantees it stays that way, and the
		// status code is enough for a caller to decide "expired code" vs.
		// "our manifest was rejected".
		return nil, fmt.Errorf("github %s: status %d", endpointConversions, res.StatusCode)
	}

	var resp conversionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("github %s: decode response: %w", endpointConversions, err)
	}
	if resp.ID == 0 || resp.PEM == "" || resp.WebhookSecret == "" {
		return nil, fmt.Errorf("github %s: response missing app id, pem, or webhook secret", endpointConversions)
	}

	return &Conversion{
		AppID:         strconv.FormatInt(resp.ID, 10),
		Slug:          resp.Slug,
		PEM:           sensitive.NewSensitiveValue([]byte(resp.PEM)),
		WebhookSecret: sensitive.NewSensitiveValue([]byte(resp.WebhookSecret)),
	}, nil
}
