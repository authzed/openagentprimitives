// export.go: storeDraft, the sidecar half of the tuple-authorized
// draft-export route (Task 6, pkg/web/workshopdraftsrv). Task 7 wires this
// behind the export_draft tool; this file only provides the client call.
package workshopmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// draftStored is what the operator's draft route answers: the store reference
// and digest the install request uses, and the render handle plus artifact id
// the builder finalizes and attaches.
type draftStored struct {
	ArtifactRef string `json:"artifactRef"`
	Digest      string `json:"digest"`
	Handle      string `json:"handle"`
	ArtifactID  string `json:"artifactId"`
}

// storeDraft POSTs oapBytes to the operator's tuple-authorized draft-export
// route (pkg/web/workshopdraftsrv, POST {OPERATOR_MEMORY_URL}/workshop/draft)
// and returns what it reports back.
//
// A plain http.Client, not pkg/x/safehttp's SSRF-guarded one: that guard
// exists for probe_mcp's target, which is a URL a MODEL supplies as a tool
// argument. OPERATOR_MEMORY_URL is neither model-supplied nor a tool
// argument — it is the same operator address main.go already reaches
// through s.Ops (httpclient.New(memURL, memToken)) via the identical
// operator-injected env vars, an address this process trusts by
// construction.
func (s *Server) storeDraft(ctx context.Context, oapBytes []byte) (draftStored, error) {
	memURL := os.Getenv("OPERATOR_MEMORY_URL")
	bearer := os.Getenv("token")
	if memURL == "" || bearer == "" {
		return draftStored{}, fmt.Errorf("storeDraft: OPERATOR_MEMORY_URL and token (the operator bearer) are both required; got url=%q tokenSet=%v", memURL, bearer != "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(memURL, "/")+"/workshop/draft", bytes.NewReader(oapBytes))
	if err != nil {
		return draftStored{}, fmt.Errorf("storeDraft: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return draftStored{}, fmt.Errorf("storeDraft: POST %s/workshop/draft: %w", memURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return draftStored{}, fmt.Errorf("storeDraft: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The operator's own error text (its 4xx/5xx body) is safe to surface:
		// pkg/web/workshopdraftsrv never echoes the draft bytes, only structural
		// denial reasons.
		return draftStored{}, fmt.Errorf("storeDraft: operator refused the draft store (status %d): %s", resp.StatusCode, string(body))
	}

	var out draftStored
	if err := json.Unmarshal(body, &out); err != nil {
		return draftStored{}, fmt.Errorf("storeDraft: decode response: %w", err)
	}
	return out, nil
}
