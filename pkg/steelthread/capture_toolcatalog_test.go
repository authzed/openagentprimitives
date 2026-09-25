package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// The two halves of this feature meet in Capture: the catalogs are the claim
// (from the records) and the ungated tools are the only exemption from it (from
// the fixture rewrite). Neither is useful alone, and the file the replay reads
// has to carry both or neither.
func TestCapture_PinsTheRecordedCatalogAndTheRewritesOwnExemption(t *testing.T) {
	recs := syntheticRecords(t)
	recs.ToolCatalogs = []toolcatalog.Content{
		{FromTurnIndex: 1, Tools: []string{"respond_to_user"}},
		{FromTurnIndex: 3, Tools: []string{"kube_nodes", "kube_pods", "respond_to_user"}},
	}

	in := captureInput(t)
	in.Fixture = liveFixture(t, withSidecarToolbox)

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	report, _ := steelthread.FormatFindings(findings)
	require.False(t, steelthread.HasHardFinding(findings), "capture must emit:\n%s", report)

	assert.Equal(t, []bt.ToolCatalog{
		{FromTurnIndex: 1, Tools: []string{"respond_to_user"}},
		{FromTurnIndex: 3, Tools: []string{"kube_nodes", "kube_pods", "respond_to_user"}},
	}, got.Bundle.ToolCatalogs)

	// DERIVED from the rewrite that dropped the sidecar's secretInputs, never
	// restated: the exemption cannot outlive the rewrite that earned it.
	assert.Equal(t, []string{"kube_nodes", "kube_pods"}, got.Bundle.ExpectedExtraTools)
}

// TestCapture_RefusesAnOfferedToolTheFixtureWillNotOffer is the wiring test for
// the tool-catalog comparison, driven through Capture because that is the only
// place the recorded catalog and the fixture's own prediction meet.
//
// checkFixtureTools is tested against a hand-built SelfCheckInput and the CLI's
// prediction against a hand-built fixture; neither sees the assignment between
// them, and dropping it leaves both green while the capture goes back to
// emitting a bundle that dies in someone else's suite.
func TestCapture_RefusesAnOfferedToolTheFixtureWillNotOffer(t *testing.T) {
	recs := syntheticRecords(t)
	recs.ToolCatalogs = []toolcatalog.Content{
		// captureInput's fixture prediction offers agent_work_complete and
		// new_operation. The third is the shape of a tool a live channel kind
		// contributed and the fixture's fake kind does not.
		{FromTurnIndex: 1, Tools: []string{"agent_work_complete", "lookup_user_for_mention", "new_operation"}},
	}

	_, findings, err := steelthread.Capture(recs, captureInput(t))
	require.NoError(t, err)

	f := findByCode(t, findings, steelthread.CodeOfferedToolNotProducible)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "lookup_user_for_mention")
	assert.NotContains(t, f.Message, "agent_work_complete",
		"a tool the fixture WILL offer must not be swept into the refusal")
}

// A session recorded before the tool_catalog Kind shipped pins no catalogs, so
// the replay compares no tool sets and there is nothing for an ungated tool to
// be excused FROM. Carrying the list anyway reads as a weakening that is not
// there — and Bundle.Validate refuses the shape outright, which would turn a
// perfectly capturable session into a hard finding.
func TestCapture_NoRecordedCatalogMeansNoExemptionEither(t *testing.T) {
	recs := syntheticRecords(t)
	require.Empty(t, recs.ToolCatalogs, "this case is about a session with no catalog rows")

	in := captureInput(t)
	in.Fixture = liveFixture(t, withSidecarToolbox)

	got, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	report, _ := steelthread.FormatFindings(findings)
	require.False(t, steelthread.HasHardFinding(findings), "capture must still emit:\n%s", report)

	assert.Empty(t, got.Bundle.ToolCatalogs)
	assert.Empty(t, got.Bundle.ExpectedExtraTools,
		"an exemption with no claim to be exempt from is a lie the loader would refuse")
	assert.NoError(t, got.Bundle.Validate())
}
