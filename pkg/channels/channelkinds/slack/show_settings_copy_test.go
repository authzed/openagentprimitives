package slack

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modalInternalOpsVocabulary mirrors the audience rule enforced for notice copy
// in pkg/channels/channelinteractions/categories: a chat surface is read by the person
// who was talking to the agent, not by someone with cluster access, so internal
// object names and daemon names are noise they cannot act on. That guard scans
// notice publishers by path and never reaches this Slack modal, which is how
// the raw-error leak below survived.
var modalInternalOpsVocabulary = []string{
	"kubectl",
	"spicedb",
	"crd",
	"reconcile",
	"configmap",
	"status.conditions",
	"agentsessions.",
	"channelsd",
}

func assertNoInternalOps(t *testing.T, text, what string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, banned := range modalInternalOpsVocabulary {
		assert.NotContains(t, lower, banned,
			"%s must not put %q in front of a chat reader", what, banned)
	}
}

// TestSettingsModalCopy_CarriesNoOperatorDetail is the regression guard for a
// user-copy leak: settingsDegradedBlocks interpolated err.Error() — the raw
// controller-runtime failure, e.g. `agentsessions.agentprimitives.authzed.com
// "x" not found` — straight into a views.open modal body. The operator detail
// belongs in the log line the caller already writes; the modal reader needs to
// know only that the settings could not be shown.
func TestSettingsModalCopy_CarriesNoOperatorDetail(t *testing.T) {
	getErr := errors.New(`agentsessions.agentprimitives.authzed.com "my-session" not found`)

	t.Run("fetch failed: modal explains without quoting the cluster error", func(t *testing.T) {
		txt := blocksText(settingsDegradedBlocks("demo/my-session", getErr))
		require.NotEmpty(t, txt, "the degraded modal must still say something")
		assert.NotContains(t, txt, getErr.Error(),
			"the raw lookup error must stay in the log, not the modal")
		assertNoInternalOps(t, txt, "the degraded settings modal")
	})

	t.Run("no client or bad ref: modal explains without naming the daemon", func(t *testing.T) {
		txt := blocksText(settingsDegradedBlocks("demo/my-session", nil))
		require.NotEmpty(t, txt)
		assertNoInternalOps(t, txt, "the degraded settings modal")
	})

	t.Run("settings not resolved yet: modal explains without naming the reconciler", func(t *testing.T) {
		txt := blocksText(renderSettingsModalBlocks("demo/my-session", nil))
		require.NotEmpty(t, txt)
		assertNoInternalOps(t, txt, "the unresolved-settings modal")
	})
}
