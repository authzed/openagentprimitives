package spicedb

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// CheckHold reports whether canonicalID may freeze the named session for
// human review.
//
// Callers are human-facing: the CLI's session-hold command and any admin
// surface. The denial-streak tripper does NOT call this — it runs in the
// operator with no user subject, and an automated containment control that
// required a human's permission to fire would not be one.
func (c *Client) CheckHold(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "hold", canonicalID, consistencyFor(fullyConsistent), "hold")
}
