package secretout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxPublishErrBody bounds how much of a non-2xx response body Publish reads
// into its returned error. The operator's error bodies (secretoutsrv's 4xx/5xx
// http.Error text) are short structural messages, never the secret value —
// generous enough that a real message is never truncated, small enough to
// bound memory against a misbehaving/malicious endpoint.
const maxPublishErrBody = 4 * 1024

// Publisher sends a captured secret value to the operator so it lands in the
// per-session Secret. Implemented over the operator HTTP endpoint.
// On success the value is persisted in the Secret and the caller should
// record the handle→secretName mapping on AgentSession.status.
type Publisher interface {
	Publish(ctx context.Context, name string, value []byte, handle string) error
}

// HTTPPublisher is the production Publisher: it POSTs to the operator's
// /secret-output/{ns}/{name} endpoint using the per-session bearer token.
// It reuses the same operator base URL and token that the memory httpclient uses
// (memURL + memToken from internal/cmd/runner startup) — no extra credential is required.
type HTTPPublisher struct {
	// OperatorURL is the base URL of the operator HTTP server
	// (e.g. "http://operator.agentprimitives.svc:8080"). Must NOT have a trailing slash.
	OperatorURL string
	// Namespace / SessionName identify the AgentSession in the URL path and must
	// match the session the per-session token was issued for.
	Namespace   string
	SessionName string
	// Token is the per-session bearer token. The same token the runner uses for
	// memory operations — the operator validates that it authorises the
	// {Namespace}/{SessionName} session.
	Token string

	// http is the underlying client. nil uses a default client with a 15s timeout.
	http *http.Client
}

// NewHTTPPublisher constructs a Publisher ready to POST to the operator.
// operatorURL, ns, name and token are the same values internal/cmd/runner uses for
// the memory httpclient.
func NewHTTPPublisher(operatorURL, ns, name, token string) *HTTPPublisher {
	return &HTTPPublisher{
		OperatorURL: operatorURL,
		Namespace:   ns,
		SessionName: name,
		Token:       token,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

// secretOutputRequest mirrors secretoutsrv.SecretOutputRequest without
// importing that package (which lives in the operator, not the runner).
type secretOutputRequest struct {
	// Name is the per-session Secret key the value lands under.
	Name string `json:"name"`
	// Value is the raw secret as a JSON string — not base64; the operator
	// stores the bytes verbatim.
	Value string `json:"value"`
	// Handle is the opaque store handle the LLM sees in place of Value.
	Handle string `json:"handle"`
}

// Publish POSTs {name, value, handle} to the operator's
// /secret-output/{ns}/{name} endpoint. The value is sent as a JSON string
// (base64-free: the operator stores raw bytes). On non-2xx the value is
// never included in the returned error — call sites must treat any error as
// "did not land" and surface a safe capture-failed message to the LLM.
func (p *HTTPPublisher) Publish(ctx context.Context, name string, value []byte, handle string) error {
	body, err := json.Marshal(secretOutputRequest{
		Name:   name,
		Value:  string(value),
		Handle: handle,
	})
	if err != nil {
		// json.Marshal of a plain struct with string fields should never fail;
		// treat it as an internal error without echoing the value.
		return fmt.Errorf("secret-output publish %q: marshal request: %w", name, err)
	}

	endpoint, err := url.JoinPath(p.OperatorURL, "secret-output", p.Namespace, p.SessionName)
	if err != nil {
		return fmt.Errorf("secret-output publish %q: build URL: %w", name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("secret-output publish %q: build request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)

	cli := p.http
	if cli == nil {
		cli = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("secret-output publish %q: HTTP POST: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the (bounded) response body for the error message so the caller
		// sees the operator's actual reason — e.g. secretoutsrv's write-once
		// 409 text — in full rather than a truncated snippet. This is the
		// operator's own structural error text, never the request body (which
		// holds the secret value), so there is no leak risk in surfacing it.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxPublishErrBody))
		return fmt.Errorf("secret-output publish %q: operator returned %d: %s", name, resp.StatusCode, string(errBody))
	}
	return nil
}

// NoopPublisher is a Publisher that always succeeds without doing anything.
// Useful in tests and local-mode sessions where there is no operator endpoint.
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ string, _ []byte, _ string) error { return nil }

var _ Publisher = (*HTTPPublisher)(nil)
var _ Publisher = NoopPublisher{}
