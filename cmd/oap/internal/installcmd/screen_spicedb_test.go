package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestSpiceDBScreen_DefaultAndSeed(t *testing.T) {
	// Seeded (accept-all): short-circuits, records the seeded value.
	st := tui.NewState()
	st.SetBool(keyExternalSpiceDB, true)
	scr := newSpiceDBScreen(DetectedSettings{})
	g, err := scr.Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g, "seeded key must short-circuit (nil group)")
	require.NoError(t, scr.Apply(context.Background(), st))
	assert.True(t, st.Bool(keyExternalSpiceDB))

	// Not seeded: presents a group (non-nil), default reflects detection.
	st2 := tui.NewState()
	g2, err := newSpiceDBScreen(DetectedSettings{ExternalSpiceDB: true}).Prepare(context.Background(), st2)
	require.NoError(t, err)
	assert.NotNil(t, g2)
}
