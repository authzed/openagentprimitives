package interact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_UserMessageKindIsRegistered(t *testing.T) {
	_, ok := Get("user_message")
	require.True(t, ok, "user_message must be registered")
	assert.Contains(t, Names(), "user_message")
}

func TestViaSub(t *testing.T) {
	ab, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")
	assert.Equal(t, "annotations", ab.ViaSub(), "annotation_batch occupies the /annotations sub-URN")

	um, ok := Get("user_message")
	require.True(t, ok, "user_message must be registered")
	assert.Equal(t, "", um.ViaSub(), "a plain message has no sub-facet")
}
