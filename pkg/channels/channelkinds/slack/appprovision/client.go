// pkg/channels/channelkinds/slack/appprovision/client.go
//
// The Slack app-configuration API surface `oap channel create --kind slack`
// needs: create an app from the manifest the wizard generated, then install it
// to the workspace without a browser round trip.
//
// apps.manifest.create is documented. apps.developerInstall is NOT: it is the
// endpoint the open-source Slack CLI uses, and it is the only known source of
// an app-level (xapp-) token, which Socket Mode requires. If Slack changes it,
// the wizard's manual path is what every user falls back to — which is why that
// path stays rather than becoming dead code.
package appprovision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// defaultBaseURL is Slack's API host. Overridden by WithBaseURL in tests.
const defaultBaseURL = "https://slack.com"

// defaultTimeout bounds one call. Long enough for a slow network, short enough
// that a user watching a wizard screen has not concluded the tool is wedged.
const defaultTimeout = 30 * time.Second

const (
	methodCreate  = "apps.manifest.create"
	methodInstall = "apps.developerInstall"
)

// CreateResult is what Slack returns for a newly created app.
type CreateResult struct {
	AppID             string
	ClientID          string
	ClientSecret      string
	SigningSecret     string
	VerificationToken string
	OAuthAuthorizeURL string
}

// InstallResult is what Slack returns for a workspace install. BotToken and
// AppLevelToken are the two credentials a Slack Channel needs.
type InstallResult struct {
	AppID         string
	BotToken      string
	AppLevelToken string
	UserToken     string
}

// Client is the app-configuration surface the wizard consumes. An interface so
// the wizard's tests can drive both halves without a network.
type Client interface {
	Create(ctx context.Context, token, manifestJSON string) (CreateResult, error)
	Install(ctx context.Context, token, appID string, botScopes []string) (InstallResult, error)
}

// HTTPClient talks to the real Slack API.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// Option configures an HTTPClient.
type Option func(*HTTPClient)

// WithBaseURL points the client at a stand-in Slack.
func WithBaseURL(u string) Option {
	return func(c *HTTPClient) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// NewHTTPClient returns a client for the real Slack API unless told otherwise.
//
// The transport is not an Option. There is exactly one caller — Kind.Wizard —
// and the timeout it would want is the one built in here; a second knob with
// no turner is API that has to be kept working without ever being exercised.
// The seam a test needs is the Client interface, which the wizard consumes,
// and WithBaseURL, which this package's own tests use.
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

// ManifestJSON converts the wizard's YAML manifest to the JSON the API takes.
//
// Converting rather than generating a second time is what keeps the manifest a
// user is shown and the manifest Slack is sent the same document. appManifestFor
// renders YAML under a hard column budget for the wizard's note card, and a
// second JSON generator would be a second place to forget a field.
func ManifestJSON(manifestYAML string) (string, error) {
	raw, err := yaml.YAMLToJSON([]byte(manifestYAML))
	if err != nil {
		return "", fmt.Errorf("convert the app manifest to JSON: %w", err)
	}
	return string(raw), nil
}

// Create makes an app from a manifest. It does NOT install it: the returned
// app exists in Slack with no tokens until Install runs.
func (c *HTTPClient) Create(ctx context.Context, token, manifestJSON string) (CreateResult, error) {
	body, err := json.Marshal(struct {
		Manifest json.RawMessage `json:"manifest"`
	}{Manifest: json.RawMessage(manifestJSON)})
	if err != nil {
		return CreateResult{}, fmt.Errorf("build the %s request: %w", methodCreate, err)
	}

	var resp struct {
		slackEnvelope
		// AppID identifies the created app; every later call is keyed by it.
		AppID string `json:"app_id"`
		// Credentials are minted once, at creation, and never re-served.
		Credentials struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			// SigningSecret verifies inbound request signatures from Slack.
			SigningSecret string `json:"signing_secret"`
			// VerificationToken is Slack's deprecated request check; unused here.
			VerificationToken string `json:"verification_token"`
		} `json:"credentials"`
		// OAuthAuthorizeURL is where a user installs the app by hand.
		OAuthAuthorizeURL string `json:"oauth_authorize_url"`
	}
	if err := c.post(ctx, methodCreate, token, body, &resp); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		AppID:             resp.AppID,
		ClientID:          resp.Credentials.ClientID,
		ClientSecret:      resp.Credentials.ClientSecret,
		SigningSecret:     resp.Credentials.SigningSecret,
		VerificationToken: resp.Credentials.VerificationToken,
		OAuthAuthorizeURL: resp.OAuthAuthorizeURL,
	}, nil
}

// Install installs an already-created app to the workspace the configuration
// token belongs to, granting botScopes, and returns the tokens it minted.
func (c *HTTPClient) Install(ctx context.Context, token, appID string, botScopes []string) (InstallResult, error) {
	body, err := json.Marshal(struct {
		AppID string `json:"app_id"`
		// BotScopes are the scopes to grant; omitted when empty, which installs
		// with whatever the manifest already declares.
		BotScopes []string `json:"bot_scopes,omitempty"`
	}{AppID: appID, BotScopes: botScopes})
	if err != nil {
		return InstallResult{}, fmt.Errorf("build the %s request: %w", methodInstall, err)
	}

	var resp struct {
		slackEnvelope
		AppID string `json:"app_id"`
		// APIAccessTokens are the tokens this install minted. Each may be empty
		// when the manifest declares no scopes of that class.
		APIAccessTokens struct {
			// Bot is the xoxb- token the listener and senders authenticate with.
			Bot string `json:"bot"`
			// AppLevel is the xapp- token socket mode connects with.
			AppLevel string `json:"app_level"`
			// User is the xoxp- token for calls made as the installing user.
			User string `json:"user"`
		} `json:"api_access_tokens"`
	}
	if err := c.post(ctx, methodInstall, token, body, &resp); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{
		AppID:         resp.AppID,
		BotToken:      resp.APIAccessTokens.Bot,
		AppLevelToken: resp.APIAccessTokens.AppLevel,
		UserToken:     resp.APIAccessTokens.User,
	}, nil
}

// slackEnvelope is the ok/error pair every Slack method answers with.
type slackEnvelope struct {
	// OK false means the call failed even though HTTP returned 2xx.
	OK bool `json:"ok"`
	// Error is Slack's machine-readable failure code; empty when OK.
	Error string `json:"error"`
}

// okErrorOf lets post read the envelope out of any response shape that embeds
// it, without post needing to know the rest of the shape.
type okErrorOf interface{ okError() (bool, string) }

func (e slackEnvelope) okError() (bool, string) { return e.OK, e.Error }

// post sends one JSON call and decodes it into out.
//
// A non-2xx status, an undecodable body and an ok:false envelope are all
// errors: reading any of them as success would hand the caller a zero-valued
// result that looks like a token it does not have.
func (c *HTTPClient) post(ctx context.Context, method, token string, body []byte, out any) error {
	url := c.baseURL + "/api/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call slack %s: %w", method, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("slack %s returned HTTP %d", method, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("decode the slack %s response: %w", method, err)
	}
	env, ok := out.(okErrorOf)
	if !ok {
		return fmt.Errorf("decode the slack %s response: no ok/error envelope", method)
	}
	if okFlag, code := env.okError(); !okFlag {
		return &APIError{Method: method, Code: code}
	}
	return nil
}
