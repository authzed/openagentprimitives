package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

func TestPtWrapResult(t *testing.T) {
	ctx, led := provenance.WithTagLedger(context.Background())
	led.RecordMinted("use-tagged", "pt_1")

	// tagged + non-error → pt-untrusted envelope carrying the minted id.
	env, handled := ptWrapResult(ctx, "use-tagged", "hello", false)
	assert.True(t, handled, "a tagged, non-error result is wrapped")
	regions, covered := toolenvelope.PtRegions(env)
	assert.True(t, covered, "the wrapped result is one covered region")
	require.Len(t, regions, 1)
	assert.Equal(t, "pt_1", regions[0].ID)
	assert.Equal(t, "hello", regions[0].Content)

	// errored result → not handled: never bind the datum id to swapped error text.
	_, handled = ptWrapResult(ctx, "use-tagged", "denied", true)
	assert.False(t, handled)

	// no minted tag → not handled (falls back to the plain untrusted wrap).
	_, handled = ptWrapResult(ctx, "use-untagged", "hello", false)
	assert.False(t, handled)

	// no ledger → not handled.
	_, handled = ptWrapResult(context.Background(), "use-tagged", "hello", false)
	assert.False(t, handled)
}
