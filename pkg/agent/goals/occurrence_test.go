package goals

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunReadDependenciesRetainsOnlyItsOwnSessionInPrivateDomain(t *testing.T) {
	own := Source{ResourceType: "agentsession", ResourceID: "team/root", Permission: "unknown_provenance"}
	other := Source{ResourceType: "agentsession", ResourceID: "team/another", Permission: "view_memory"}
	o := Occurrence{Domain: Domain{Namespace: "team", Owner: "owner", ClassUID: "class"}, SessionName: "root", SessionUID: "bound-root-uid", Proposal: &RunProposal{Sources: []Source{own, other, own}}}
	require.ElementsMatch(t, []Source{{ResourceType: "agent_goal_domain", ResourceID: o.Domain.ID(), Permission: "view_memory"}, other}, o.ReadDependencies())
	require.Equal(t, []Source{own, other, own}, o.Proposal.Sources)
	o.SessionUID = ""
	require.ElementsMatch(t, []Source{own, other}, o.ReadDependencies(), "an unbound name must never establish ownership")
}
