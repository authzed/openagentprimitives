// pkg/platform/identityd/suggested.go — cluster-wide suggested-credential
// aggregator for the standing portal.
//
// The credential set comes from the canonical resolver
// (pkg/platform/identity/passthrough), the same one the operator parker, the
// Slack Home view and the channelsd DM publisher use — a local re-derivation
// would drop the toolkit-backed credentials the resolver includes.
package identityd

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
)

// suggestedCredentialsForUser returns the deduplicated, sorted set of
// credential names referenced by any AgentClass with
// identityMode=userPassthrough cluster-wide, minus the user's already-linked
// set. Labels come from the matching MCPServer.spec.auth.provider; toolkit-
// backed credentials have no such field and fall back to a blank label the
// caller renders as the bare credential name.
//
// Best-effort: RequiredBestEffort logs and skips an individual unresolvable ref
// (a dangling MCPServer or toolkit), so one broken ref never blanks out the
// credentials that did resolve — for its own AgentClass or any other.
//
// Uncached and cluster-wide — go through
// Server.suggestedCredentialsForUserCached on request paths.
func suggestedCredentialsForUser(
	ctx context.Context,
	c client.Client,
	alreadyLinked map[string]bool,
	logger logr.Logger,
) ([]portalCredential, error) {
	// 1. List every AgentClass cluster-wide.
	var classes spiceboxv1alpha1.AgentClassList
	if err := c.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("list AgentClasses: %w", err)
	}

	// 2. Pre-resolve MCPServer provider labels and auth types once. Toolkit-
	//    backed credentials have no MCPServer entry, so they end up with an
	//    empty Label and NeedsRefresh=false.
	labels := portalLabelMap(ctx, c, logger)
	authTypes := portalAuthTypeMap(ctx, c, logger)

	// 3. Walk every passthrough AgentClass through the shared resolver, so MCP
	//    and toolkit credentials are both surfaced.
	seen := map[string]bool{}
	out := []portalCredential{}

	for i := range classes.Items {
		ac := &classes.Items[i]
		if ac.Spec.IdentityMode != spiceboxv1alpha1.IdentityModeUserPassthrough {
			continue
		}
		creds := passthrough.RequiredBestEffort(ctx, c, ac, logger)
		for _, credName := range creds {
			if alreadyLinked[credName] || seen[credName] {
				continue
			}
			seen[credName] = true
			out = append(out, portalCredential{
				Name:         credName,
				Label:        labels[credName],
				NeedsRefresh: authTypes[credName] == "oauth",
				IconURL:      portalIconURL(credName),
			})
		}
	}

	// 4. Stable-sort by name for deterministic rendering.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
