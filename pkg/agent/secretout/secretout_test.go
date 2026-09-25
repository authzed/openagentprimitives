package secretout

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStore_PutMintsOpaqueHandle_GetReturnsValue(t *testing.T) {
	s := NewSessionStore("sess-uid-123")
	h, err := s.Put(context.Background(), "kubeconfig", []byte("apiVersion: v1\n..."))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "so-"), "handle is opaque, so- prefixed")
	assert.NotContains(t, h, "apiVersion", "handle never embeds the value")

	got, ok := s.Get(h)
	require.True(t, ok, "handle resolves")
	assert.Equal(t, "kubeconfig", got.Name)
	assert.Equal(t, []byte("apiVersion: v1\n..."), got.Value)
}

func TestSessionStore_GetUnknownHandle(t *testing.T) {
	s := NewSessionStore("sess-uid-123")
	_, ok := s.Get("so-nope")
	assert.False(t, ok, "unknown handle does not resolve")
}

func TestSessionStore_HandlesAreUnique(t *testing.T) {
	s := NewSessionStore("sess-uid-123")
	h1, _ := s.Put(context.Background(), "a", []byte("x"))
	h2, _ := s.Put(context.Background(), "a", []byte("x"))
	assert.NotEqual(t, h1, h2, "each Put mints a fresh handle")
}
