// Package sandbox implements the runner-side sandbox tool dispatch:
// synthesizing Tool impls from SpiceboxToolspecs, executing them as
// ToolCall CRs, and fetching their stdout/stderr via the operator's
// /debug/artifact endpoint.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ArtifactClient fetches stored artifact bytes by ref.
type ArtifactClient interface {
	Get(ctx context.Context, ref string) (io.ReadCloser, error)
}

// HTTPArtifactClient is the production impl that hits the operator's
// /debug/artifact endpoint with a bearer token. The operator-side handler
// accepts either the global debug token or a per-session memory token
// scoped to the same session as the ref's path.
type HTTPArtifactClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewHTTPArtifactClient(baseURL, token string) *HTTPArtifactClient {
	return &HTTPArtifactClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *HTTPArtifactClient) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/debug/artifact?ref=%s", c.baseURL, url.QueryEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("artifact: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("artifact: %s status %d", u, resp.StatusCode)
	}
	return resp.Body, nil
}
