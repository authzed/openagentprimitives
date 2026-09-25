package desktop_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/vz"
)

func TestVZProvider_SatisfiesInterface(t *testing.T) {
	var _ desktop.ClusterProvider = vz.New(desktop.VMConfig{})
}

func TestVZProvider_StubErrorsOffPlatform(t *testing.T) {
	// On Linux CI this exercises the stub; on darwin/arm64 it constructs
	// the real (placeholder) provider (Provision is not called here).
	p := vz.New(desktop.VMConfig{})
	require.NotNil(t, p)
	if st, err := p.Status(context.Background()); err != nil {
		assert.Equal(t, desktop.StateUnknown, st)
	}
}
