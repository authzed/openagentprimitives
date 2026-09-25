package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestDetectedScreen_DefaultByExistence(t *testing.T) {
	// existing -> presents, default accept
	g, err := newDetectedScreen(DetectedSettings{Existing: true, ACMEEmail: "demo@example.test"}).Prepare(context.Background(), tui.NewState())
	require.NoError(t, err)
	assert.NotNil(t, g)

	// seeded choice short-circuits and records it
	st := tui.NewState()
	st.Set(keyDetected, detectedAccept)
	scr := newDetectedScreen(DetectedSettings{Existing: true})
	g2, err := scr.Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g2)
	require.NoError(t, scr.Apply(context.Background(), st))
	assert.Equal(t, detectedAccept, st.Get(keyDetected))
}
