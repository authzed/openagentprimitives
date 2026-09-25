package httpclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// MintPtTag asks the operator to derive a datum's audience and record its
// provenance tag, returning the tag id.
//
// A REQUEST, not a write, and the distinction is the whole point. pt_tag is
// ComponentWritten precisely so a session credential cannot author one: a tag's
// reader set GRANTS disclosure, so a session able to write its own could name
// an audience its source never authorized and then disclose to it legitimately,
// because the tag would be checked and would say yes.
//
// This sends what the tool call TOUCHED; the operator decides who may see it.
// There is deliberately no field here for an audience, and adding one would
// undo the reason the route exists.
func (c *Client) MintPtTag(ctx context.Context, scope memory.Scope, req memory.PtTagMintRequest) (string, error) {
	ns, name := scopePath(scope)
	var out memory.PtTagMintResponse
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/memory/_pttag_mint/%s/%s", ns, name), req, &out); err != nil {
		return "", err
	}
	return out.TagID, nil
}
