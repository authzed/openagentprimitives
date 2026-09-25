package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopStatefulStorage_TrustsDefault(t *testing.T) {
	dec, err := NoopStatefulStorage{}.Resolve(context.Background(), StatefulParams{})
	require.NoError(t, err)
	assert.Empty(t, dec.ClassName, "noop trusts the cluster default")
	assert.Nil(t, dec.CreateClass)
	assert.False(t, dec.Probe)
	assert.Empty(t, dec.Message)
}
