package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestIdPScreen_DefaultsToCurrentThenNone(t *testing.T) {
	kinds := []string{"google", "oidc", "password"}
	st := tui.NewState()
	g, err := newIdPScreen(kinds, DetectedSettings{IdPKind: "google"}).Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.NotNil(t, g) // not seeded -> presents

	st2 := tui.NewState()
	st2.Set(keyIdP, "google") // accept-all seed
	g2, err := newIdPScreen(kinds, DetectedSettings{IdPKind: "google"}).Prepare(context.Background(), st2)
	require.NoError(t, err)
	assert.Nil(t, g2)
	require.NoError(t, newIdPScreen(kinds, DetectedSettings{IdPKind: "google"}).Apply(context.Background(), st2))
	assert.Equal(t, "google", st2.Get(keyIdP))
}
