//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// createCheckRun posts one check run to the fixture and returns the id it
// assigned, exercising the same handler the github kind's own client reaches.
func createCheckRun(t *testing.T, g *FakeGitHub, sha string) int64 {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": "demo-reviewbot", "head_sha": sha, "status": "in_progress"})
	require.NoError(t, err)

	resp, err := http.Post(g.URL()+"/repos/demo-org/platform/check-runs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var out struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.ID
}

// TestFakeGitHub_SeededIDsAreWhatTheProviderMinted is the replay half of the
// trigger-status stand-in, and the one M17 (ignoring the seeded minter) walked
// through before it existed.
//
// A captured session's recorded reply names the id GitHub chose, and the step's
// own divergence check was derived from those exact bytes. A fixture minting 1
// where the recording says 99044729080 fails a run that took the identical path
// — so the number the provider chose is seeded, and everything around it stays
// real.
func TestFakeGitHub_SeededIDsAreWhatTheProviderMinted(t *testing.T) {
	t.Run("the recorded ids are handed back in mint order", func(t *testing.T) {
		g := newFakeGitHub(t)
		seq := bt.NewMintedIDSequence(
			map[string][]string{bt.FamilyTriggerStatus: {"99044729080", "99044729081"}},
			func(msg string) { t.Errorf("%s", msg) },
		)
		g.SetCheckRunIDMinter(seq.Minter(bt.FamilyTriggerStatus))

		assert.Equal(t, int64(99044729080), createCheckRun(t, g, "sha-one"))
		assert.Equal(t, int64(99044729081), createCheckRun(t, g, "sha-two"))
		assert.Empty(t, seq.Unused(), "both recorded ids were drawn")
	})

	t.Run("an unseeded fixture keeps its own counter", func(t *testing.T) {
		g := newFakeGitHub(t)
		seq := bt.NewMintedIDSequence(nil, func(msg string) { t.Errorf("%s", msg) })
		// nil minter — the compatibility path every authored bundle takes, and
		// what keeps the 39 bronze bundles working unchanged.
		g.SetCheckRunIDMinter(seq.Minter(bt.FamilyTriggerStatus))

		assert.Equal(t, int64(1), createCheckRun(t, g, "sha-one"),
			"with nothing pinned the fixture mints its own, exactly as before")
	})

	t.Run("a run minting more than was recorded is reported, never served zero", func(t *testing.T) {
		g := newFakeGitHub(t)
		var reported []string
		seq := bt.NewMintedIDSequence(
			map[string][]string{bt.FamilyTriggerStatus: {"99044729080"}},
			func(msg string) { reported = append(reported, msg) },
		)
		g.SetCheckRunIDMinter(seq.Minter(bt.FamilyTriggerStatus))

		require.Equal(t, int64(99044729080), createCheckRun(t, g, "sha-one"))

		second := createCheckRun(t, g, "sha-two")
		assert.NotZero(t, second,
			"the kind patches a run BY ID, so a fixture handing back 0 would let a broken "+
				"round trip pass — the exhaustion is reported instead")
		assert.NotEmpty(t, reported,
			"exhaustion has to reach the test: the run opened more statuses than the captured "+
				"session did, and every recorded argument downstream now addresses a different run")
	})
}

// TestFakeGitHub_PreSeededConclusionDoesNotConsumeTheSequence pins the one place
// the seeded minter is deliberately NOT used.
//
// SetCheckRunConclusion stands for a run some EARLIER delivery left behind, not
// one the session under test opened. Drawing from the sequence there would eat
// an id the run's own create is going to need and shift every later one by a
// position — the exact failure the ordering contract exists to prevent.
func TestFakeGitHub_PreSeededConclusionDoesNotConsumeTheSequence(t *testing.T) {
	g := newFakeGitHub(t)
	seq := bt.NewMintedIDSequence(
		map[string][]string{bt.FamilyTriggerStatus: {"99044729080"}},
		func(msg string) { t.Errorf("%s", msg) },
	)
	g.SetCheckRunIDMinter(seq.Minter(bt.FamilyTriggerStatus))

	g.SetCheckRunConclusion("sha-old", "reviewbot", "success")

	assert.Equal(t, int64(99044729080), createCheckRun(t, g, "sha-new"),
		"the session's own create still gets the FIRST recorded id")
}
