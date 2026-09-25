package httpclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// VerifyPtTags asks the operator to bind the (id, content) regions a caller
// parsed from an outbound payload against the bytes the platform stored for
// those ids, returning which ids bound and whether every region did.
//
// A REQUEST, like MintPtTag, and for the same reason. pt_tag_content is
// component-read (SessionReadable false), so a session bearer cannot read it to
// compare for itself; and even if it could, letting the caller do the compare
// would defeat the check — a caller could pair a witnessed wide id with
// fabricated bytes and declare a match. The operator holds the stored bytes and
// does the comparison, returning only ids, never content.
func (c *Client) VerifyPtTags(ctx context.Context, scope memory.Scope, regions []memory.PtTagRegion) (memory.PtTagVerifyResponse, error) {
	ns, name := scopePath(scope)
	var out memory.PtTagVerifyResponse
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/memory/_pttag_verify/%s/%s", ns, name),
		memory.PtTagVerifyRequest{Regions: regions}, &out); err != nil {
		return memory.PtTagVerifyResponse{}, err
	}
	return out, nil
}
