package auditcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestRequireCompleteAuditRead pins the fail-closed direction on the one
// surface where a silent partial is an attestation rather than an
// inconvenience: `oap audit verify` walks a hash chain, and a read that stopped
// at its query limit would leave the chain's tail unexamined while the report
// printed clean and the command exited 0.
//
// Refusing is the only honest answer, because the limit is not a flag the
// reader can raise.
func TestRequireCompleteAuditRead(t *testing.T) {
	t.Run("a truncated read is refused before anything is verified", func(t *testing.T) {
		err := requireCompleteAuditRead(memory.QueryResult{Truncated: true})
		require.ErrorIs(t, err, errAuditTruncated,
			"the sentinel is what makes the exit non-zero and the cause greppable")
		assert.Contains(t, err.Error(), "audit", "the message must say which verification was abandoned")
	})

	t.Run("a complete read verifies normally", func(t *testing.T) {
		assert.NoError(t, requireCompleteAuditRead(memory.QueryResult{}))
	})
}

func TestRequireCompleteAuditReadRejectsPartialExport(t *testing.T) {
	require.ErrorContains(t, requireCompleteAuditRead(memory.QueryResult{Partial: true}), "incomplete audit export")
}
