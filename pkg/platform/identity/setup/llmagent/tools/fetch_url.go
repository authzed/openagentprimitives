package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

const FetchURLName = "fetch_url"
const FetchURLSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "url": { "type": "string", "description": "http or https URL to fetch (read-only)" }
  },
  "required": ["url"]
}`

// FetchURLClient is the seam, and it is SSRF-guarded: the dialer refuses
// private/loopback/link-local destinations and re-validates redirects, so an
// LLM-emitted URL cannot reach cloud metadata or in-cluster addresses. Tests may
// override it.
var FetchURLClient = safehttp.Client()

const fetchMaxBytes = 256 * 1024

func FetchURLRun(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	u, err := url.Parse(args.URL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("fetch_url: only http/https allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ap-setup/1.0")
	resp, err := FetchURLClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch_url: GET %s: %w", args.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("fetch_url: %s returned %d", args.URL, resp.StatusCode)
	}
	return string(body), nil
}
