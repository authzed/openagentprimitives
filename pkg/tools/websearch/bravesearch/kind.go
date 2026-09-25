// Package bravesearch implements websearch.Backend against the Brave Search
// API (api.search.brave.com/res/v1/web/search): a documented HTTP API,
// authenticated by a single subscription-token header, whose terms permit
// programmatic (non-browser) access — the three properties this package's
// backend was chosen for. "fake" (pkg/tools/websearch) remains the one
// tests reach for; this is the one production selects.
package bravesearch

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
)

// KindName is this backend's registry key.
const KindName = "bravesearch"

func init() { registry.Register(Backend{}) }

// Backend is the registrable descriptor. It is a stateless singleton;
// everything stateful (the HTTP client, the credential) lives on the
// Provider New returns — the same split sandboxkinds.Kind/Runtime and
// channelkinds.Kind/Listener use.
type Backend struct{}

func (Backend) Name() string { return KindName }

// New constructs a live Provider from deps. Fails closed: a Backend with no
// HTTPClient or no APIKey cannot make a request the SSRF guard or the
// upstream API would accept, so it refuses to construct rather than
// returning a Provider that would fail on first use.
func (Backend) New(deps websearch.Deps) (websearch.Provider, error) {
	if deps.HTTPClient == nil {
		return nil, fmt.Errorf("bravesearch: HTTPClient is required")
	}
	if deps.APIKey == "" {
		return nil, fmt.Errorf("bravesearch: APIKey is required")
	}
	return &Provider{httpClient: deps.HTTPClient, apiKey: deps.APIKey}, nil
}

var _ websearch.Backend = Backend{}
