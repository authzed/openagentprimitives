package httpclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ResolvePtTags asks the operator for the CONTENT behind a set of tags, in the
// named scope, returning only the tags the caller is entitled to receive
// (pt_tag:<id>#access). It backs derive_tag's validator (reading the source
// tags it derives from) and a subagent's bound data slot (a child reading its
// parent's bound datum).
//
// A REQUEST, like MintPtTag/VerifyPtTags: pt_tag_content is component-read, so
// this session-credentialed caller cannot read it directly. The operator holds
// the credential AND does the entitlement check, so a caller can never resolve
// bytes for a tag it lacks access to.
func (c *Client) ResolvePtTags(ctx context.Context, scope memory.Scope, tagIDs []string) ([]memory.PtTagContent, error) {
	ns, name := scopePath(scope)
	var out memory.PtTagResolveResponse
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/memory/_pttag_resolve/%s/%s", ns, name),
		memory.PtTagResolveRequest{TagIDs: tagIDs}, &out); err != nil {
		return nil, err
	}
	return out.Contents, nil
}
