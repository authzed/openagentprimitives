// pkg/channels/channelkinds/github/appprovision/webhookupdate.go
//
// The one write this package makes against an existing App's configuration:
// repoint its webhook URL. It is deliberately narrow — a single field, on an
// App the caller has already established this tool provisioned. See
// UpdateWebhookURL's doc, and ReadAppConfig's (appconfig.go), for where the
// read-only line now sits.
package appprovision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

// endpointHookConfig names this call in error messages. It is GitHub's
// "update a webhook configuration for an app" endpoint, which acts on the
// App the request's JWT authenticates as — the App is named by the signing
// key, not by a path segment.
const endpointHookConfig = "/app/hook/config"

// UpdateWebhookURLParams names the App, carries the private key needed to
// sign the App-level JWT this write requires, and gives the URL GitHub should
// deliver to from now on. The auth leg is the same one ReadAppConfig
// performs: GitHub authenticates an App-configuration write as the App
// itself, not as one of its installations.
type UpdateWebhookURLParams struct {
	// AppID is the GitHub App's numeric id, stringified, used as the JWT
	// issuer.
	AppID string
	// PrivateKeyPEM is the App's RS256 private key in PEM form. Never
	// logged, and never placed in a returned error.
	PrivateKeyPEM []byte
	// URL is where GitHub should POST this App's deliveries. Must be
	// non-empty: an empty value would clear the App's webhook, silently
	// stopping every delivery.
	URL string
}

// hookConfigUpdate is the PATCH body. Only url is sent: content_type, secret
// and insecure_ssl are omitted, so GitHub leaves each at whatever it already
// has. Sending a zero-valued secret would rotate the App's webhook HMAC and
// break signature verification for every delivery afterwards.
type hookConfigUpdate struct {
	URL string `json:"url"`
}

// UpdateWebhookURL repoints an App's webhook to url. This is the ONLY write
// this package makes against an existing App's configuration, and it touches
// exactly one field.
//
// The caller must have already established that this tool provisioned the
// App — the provenance marker the wizard stamps on the Channel. Repointing
// an App a human registered by hand is an outward-facing change to a
// resource this cluster does not own; that case still gets a drift finding
// for a human to act on (pkg/controllers/channel's WebhookURLDrift
// condition), never this call.
//
// It checks the URL only for emptiness. Whether the address is one GitHub
// could ever deliver to is the CALLER's check — Kind.RepointWebhookURL runs
// channelkinds.UnreachableWebhookURL before reaching this — because this
// package is the HTTP client for GitHub's API and not the place the project's
// rule about reachable addresses lives.
//
// No returned error carries the App JWT or the private key. The JWT travels
// in a header rather than the URL, so unlike Convert (client.go) the
// %w-wrapped *url.Error paths below are safe: the URL they render is the
// literal endpoint, with no credential in it.
func (c *HTTPClient) UpdateWebhookURL(ctx context.Context, p UpdateWebhookURLParams) error {
	if p.URL == "" {
		return fmt.Errorf("github %s: refusing to set an empty webhook url", endpointHookConfig)
	}

	appJWT, err := githubapp.SignAppJWT(p.AppID, p.PrivateKeyPEM, time.Now())
	if err != nil {
		return fmt.Errorf("github %s: %w", endpointHookConfig, err)
	}

	body, err := json.Marshal(hookConfigUpdate{URL: p.URL})
	if err != nil {
		return fmt.Errorf("github %s: encode request: %w", endpointHookConfig, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+endpointHookConfig, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github %s: build request failed: %w", endpointHookConfig, err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github %s: call failed: %w", endpointHookConfig, err)
	}
	defer func() { _ = res.Body.Close() }()

	// Drained (bounded) so the connection can be reused, and discarded: the
	// response echoes the hook config back, and nothing here needs it.
	if _, err := io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBodyBytes)); err != nil {
		return fmt.Errorf("github %s: read response: %w", endpointHookConfig, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// Status only, never the body — the same reasoning as Convert's:
		// GitHub's error body is not secret today and nothing guarantees it
		// stays that way, and the status is what a caller acts on.
		return fmt.Errorf("github %s: status %d", endpointHookConfig, res.StatusCode)
	}
	return nil
}
