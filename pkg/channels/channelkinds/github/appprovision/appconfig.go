// pkg/channels/channelkinds/github/appprovision/appconfig.go
//
// The read half of App provisioning: look up what GitHub currently has on
// file for an already-created App, so a caller can detect drift against what
// this cluster expects. See ReadAppConfig's doc for where the read-only line
// sits, and which single field webhookupdate.go is allowed to write.
package appprovision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

// endpointApp names this call in error messages.
const endpointApp = "/app"

// ReadAppConfigParams names the App and carries the private key needed to
// sign the App-level JWT this read requires. GitHub authenticates "read the
// authenticated app" as the App itself (a JWT signed with its own private
// key), not as one of its installations — the same auth leg
// githubapp.SignAppJWT already performs for minting an installation token,
// reused here rather than re-implemented.
type ReadAppConfigParams struct {
	// AppID is the GitHub App's numeric id, stringified, used as the JWT
	// issuer.
	AppID string
	// PrivateKeyPEM is the App's RS256 private key in PEM form. Never
	// logged.
	PrivateKeyPEM []byte
}

// AppConfig is what ReadAppConfig reads back about an already-provisioned
// App. Today this is only the webhook URL GitHub currently has registered —
// the one field the drift check (pkg/controllers/channel) needs — but it is
// its own type rather than a bare string so a future caller needing another
// read-only field (the App's permission set, say) has somewhere to add it.
type AppConfig struct {
	// WebhookURL is GitHub's hook_attributes.url for this App: the URL
	// GitHub will actually POST pull_request deliveries to.
	WebhookURL string
}

// ReadAppConfig reads back the App's current configuration — importantly,
// the webhook URL GitHub has on file for it — so a caller can detect drift
// against where this cluster actually serves. This call is READ ONLY.
//
// The rest of an App's configuration is read-only for this package too. The
// single exception is UpdateWebhookURL (webhookupdate.go), which writes
// exactly one field — the webhook URL — and only for an App the caller has
// established this tool provisioned, identified by the provenance marker the
// wizard stamps on the Channel. That is the whole of the write surface.
//
// The line sits there because repointing the webhook of an App a human
// registered by hand is an outward-facing change to a resource this cluster
// does not own. For an unmarked App, drift is reported for a human to act on
// (see pkg/controllers/channel's WebhookURLDrift condition) and never
// auto-corrected by calling back into GitHub.
func (c *HTTPClient) ReadAppConfig(ctx context.Context, p ReadAppConfigParams) (*AppConfig, error) {
	appJWT, err := githubapp.SignAppJWT(p.AppID, p.PrivateKeyPEM, time.Now())
	if err != nil {
		return nil, fmt.Errorf("github %s: %w", endpointApp, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpointApp, nil)
	if err != nil {
		return nil, fmt.Errorf("github %s: build request failed: %w", endpointApp, err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s: call failed: %w", endpointApp, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("github %s: read response: %w", endpointApp, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("github %s: status %d", endpointApp, res.StatusCode)
	}

	// Reuses manifestDoc's hookAttrs shape (manifest.go): the same
	// hook_attributes.url field this App was created with is what GitHub
	// reports back here.
	var resp struct {
		HookAttrs hookAttrs `json:"hook_attributes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("github %s: decode response: %w", endpointApp, err)
	}

	return &AppConfig{WebhookURL: resp.HookAttrs.URL}, nil
}
