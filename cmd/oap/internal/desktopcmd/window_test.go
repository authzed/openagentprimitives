package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWindowCmd_SizeFlagsDefaultToTheSetupWindow: --width/--height exist on
// every platform (the command tree is uniform) and default to the setup
// window's size, so callers that pass neither are unchanged.
func TestWindowCmd_SizeFlagsDefaultToTheSetupWindow(t *testing.T) {
	cmd := NewWindowCmd(nil)
	for flag, want := range map[string]string{"width": "760", "height": "640"} {
		f := cmd.Flags().Lookup(flag)
		require.NotNil(t, f, "--%s flag", flag)
		assert.Equal(t, want, f.DefValue, "--%s default", flag)
	}
}
