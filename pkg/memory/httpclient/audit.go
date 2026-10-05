package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"k8s.io/apimachinery/pkg/util/validation"
)

// QueryAudit reads the complete administrative audit export. It requires the
// operator's administrative credential, and never falls back to a filtered
// session query when that endpoint is absent or access is denied.
func (c *Client) QueryAudit(ctx context.Context, scope memory.Scope) (memory.QueryResult, error) {
	var result memory.QueryResult
	ns, name, ok := strings.Cut(scope.ID, "/")
	if scope.Kind != "session" || !ok || len(validation.IsDNS1123Label(ns)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return result, fmt.Errorf("audit export requires a session scope")
	}
	err := c.do(ctx, http.MethodGet, "/audit/"+ns+"/"+name, nil, &result)
	return result, err
}
