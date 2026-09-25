package provenance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagLedger_RecordsAndReadsBack(t *testing.T) {
	ctx, led := WithTagLedger(context.Background())
	require.NotNil(t, led)
	require.Same(t, led, TagLedgerFrom(ctx), "the ledger in ctx is the one returned")

	led.RecordMinted("use-1", "pt_abc")
	got, ok := led.MintedFor("use-1")
	assert.True(t, ok)
	assert.Equal(t, "pt_abc", got)

	_, ok = led.MintedFor("use-unknown")
	assert.False(t, ok, "unknown use id reads back not-ok")
}

func TestTagLedgerFrom_absentIsNil(t *testing.T) {
	assert.Nil(t, TagLedgerFrom(context.Background()), "no ledger opened → nil, callers must nil-check")
}
