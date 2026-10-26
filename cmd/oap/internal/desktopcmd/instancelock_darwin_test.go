//go:build darwin && arm64

package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireInstanceLock(t *testing.T) {
	dir := t.TempDir()

	first, err := acquireInstanceLock(dir)
	require.NoError(t, err, "first acquire must succeed")
	t.Cleanup(func() { _ = first.Close() })

	second, err := acquireInstanceLock(dir)
	require.ErrorIs(t, err, errAlreadyRunning, "a second open fd must not get the lock while the first holds it")
	assert.Nil(t, second)

	require.NoError(t, first.Close())
	third, err := acquireInstanceLock(dir)
	require.NoError(t, err, "the lock is free again once the holder closes it")
	assert.NoError(t, third.Close())
}
