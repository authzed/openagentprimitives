// Package extractordclient is the HTTP client internal/cmd/operator dials extractord
// (internal/cmd/extractord) with — the network counterpart to pkg/platform/extract's MIME
// registry, which extractord wraps as a zero-egress HTTP service. Client
// implements pkg/memory/httpsrv.AttachmentExtractor, so internal/cmd/operator's DI
// wires it directly into httpsrv.WithAttachmentExtractor.
package extractordclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
)

var _ httpsrv.AttachmentExtractor = (*Client)(nil)

// Client dials one extractord instance over HTTP. The zero value is not
// usable; construct with New.
type Client struct {
	endpoint string
	http     *http.Client
}

// defaultTimeout backstops this client independently of whatever context
// Extract's caller passes in: nothing upstream of internal/cmd/operator's inbound-asset
// handler is guaranteed to put a deadline on that ctx, so without this a
// stalled extractord response would leave this Do call — and the operator
// request goroutine handling it — blocked forever. extractord's own
// extraction work is separately capped at 30s (pkg/platform/extract/tabula.DefaultTimeout),
// so this leaves comfortable headroom for network transfer of a file up to
// the operator's own 25 MiB ceiling on a slow link, without being unbounded.
const defaultTimeout = 45 * time.Second

// New builds a Client against endpoint (e.g.
// "http://agentprimitives-extractord.agentprimitives-system.svc:8080").
func New(endpoint string) *Client {
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), http: &http.Client{Timeout: defaultTimeout}}
}

// extractResponse mirrors internal/cmd/extractord/main.go's extractResponse — the
// exact wire shape POST /extract answers with on 200.
type extractResponse struct {
	Text string `json:"text"`
	// Pages is the page/slide count when the format has one, else 0.
	Pages int `json:"pages"`
}

// Extract POSTs body to extractord's /extract route with Content-Type: mime
// and decodes {"text","pages"}. body is streamed straight into the request,
// never buffered whole here.
//
// Maps extractord's status-code contract (internal/cmd/extractord/main.go's
// handleExtract doc) onto httpsrv.AttachmentExtractor's permanent/transient
// split: 415 (no backend claims mime) is the ONLY permanent outcome, mapped
// to httpsrv.ErrAttachmentMIMEUnsupported; every other non-2xx status (413
// too large, 422 unprocessable, 5xx) and every transport-level failure
// (timeout, connection refused, ctx cancellation) is a plain error, which
// callers must treat as transient — never re-worded as "unsupported type".
func (c *Client) Extract(ctx context.Context, mime string, body io.Reader) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/extract", body)
	if err != nil {
		return "", 0, fmt.Errorf("extractordclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", mime)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("extractordclient: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnsupportedMediaType {
		return "", 0, httpsrv.ErrAttachmentMIMEUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		// extractord's own non-2xx bodies are content-free by design (see
		// internal/cmd/extractord/main.go's classifyExtractError — it deliberately never
		// forwards a parser's raw error text, which can embed uploaded file
		// bytes), so including this verbatim in the wrapped error never risks
		// leaking file content. Bounded read: extractord's error bodies are
		// short static strings, but nothing here should trust that unconditionally.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("extractordclient: extractord returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out extractResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("extractordclient: decode response: %w", err)
	}
	return out.Text, out.Pages, nil
}
