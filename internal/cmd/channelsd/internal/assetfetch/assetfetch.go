// Package assetfetch is channelsd's HTTP client for the operator's
// /artifact-bundle/{ns}/{sess}/{render}/bundle route. It returns
// channelkinds.AssetBytes directly so a *Fetcher satisfies the
// channelkinds.AssetFetcher interface without an adapter.
//
// The bundle route (not the plain /artifact/…/output route) is used
// deliberately: it is a smart passthrough (see
// pkg/memory/httpsrv/bundle.go's serveBundle) that returns the primary's
// raw bytes unchanged UNLESS it is an html primary with resolvable
// `artifact:HANDLE` references, in which case it returns a self-contained
// ZIP instead. Fetch itself carries no branching — it always hits the one
// route and passes through whatever Content-Type/Content-Disposition come
// back, raw or zipped.
//
// Auth: every request carries the channelsd system token via
// Authorization: Bearer <token>. The operator validates against
// IsChannelsdToken and additionally enforces ownerRef-based scoping on
// the resolved ArtifactRender CR.
package assetfetch

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Config configures a Fetcher. OperatorURL is the operator-memory base URL
// (e.g. "http://operator-memory.spicebox-system.svc:8080"); the /artifact/
// path is appended internally. Token is the channelsd-system bearer token.
// HTTP is optional; nil falls back to http.DefaultClient.
type Config struct {
	OperatorURL string
	Token       string
	HTTP        *http.Client
}

// Fetcher resolves an attachment ref (ns, sess, render) to its bytes by
// calling the operator's /artifact/ route.
type Fetcher struct {
	cfg Config
}

// New constructs a Fetcher. Callers should supply a non-empty OperatorURL +
// Token; an empty token will produce 401s at request time.
func New(cfg Config) *Fetcher {
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	return &Fetcher{cfg: cfg}
}

// Fetch retrieves the bytes for the given (ns, sess, render) tuple via the
// operator's bundle route. The returned channelkinds.AssetBytes carries the
// body, the server's Content-Type (text/html, image/png, application/zip,
// …), and the filename parsed from Content-Disposition (when present).
// Non-200 responses are surfaced as errors with up to 4 KiB of the response
// body included for diagnostics.
func (f *Fetcher) Fetch(ctx context.Context, ns, sess, render string) (channelkinds.AssetBytes, error) {
	url := fmt.Sprintf("%s/artifact-bundle/%s/%s/%s/bundle", f.cfg.OperatorURL, ns, sess, render)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return channelkinds.AssetBytes{}, err
	}
	req.Header.Set("Authorization", "Bearer "+f.cfg.Token)
	resp, err := f.cfg.HTTP.Do(req)
	if err != nil {
		return channelkinds.AssetBytes{}, fmt.Errorf("artifact fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return channelkinds.AssetBytes{}, fmt.Errorf("artifact fetch %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return channelkinds.AssetBytes{}, fmt.Errorf("artifact fetch %s: read body: %w", url, err)
	}
	cd := resp.Header.Get("Content-Disposition")
	_, params, _ := mime.ParseMediaType(cd)
	return channelkinds.AssetBytes{
		Bytes:    body,
		MIME:     resp.Header.Get("Content-Type"),
		Filename: params["filename"],
	}, nil
}
