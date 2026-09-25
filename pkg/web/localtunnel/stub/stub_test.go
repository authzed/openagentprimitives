package stub

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStub_HappyPath(t *testing.T) {
	tn := &Tunnel{URL: "https://stub.example.test"}
	assert.False(t, tn.Started())
	assert.False(t, tn.Stopped())

	url, err := tn.Start(context.Background(), "http://127.0.0.1:9090")
	require.NoError(t, err)
	assert.Equal(t, "https://stub.example.test", url)
	assert.True(t, tn.Started())
	assert.Equal(t, "http://127.0.0.1:9090", tn.LocalAddr())

	require.NoError(t, tn.Stop())
	assert.True(t, tn.Stopped())
}

func TestStub_StartErr(t *testing.T) {
	tn := &Tunnel{StartErr: errors.New("boom")}
	_, err := tn.Start(context.Background(), "http://127.0.0.1:1234")
	require.Error(t, err)
	assert.True(t, tn.Started(), "Started records the call even when StartErr fires")
}

func TestStub_StopErr(t *testing.T) {
	tn := &Tunnel{StopErr: errors.New("bang")}
	err := tn.Stop()
	require.Error(t, err)
	assert.True(t, tn.Stopped())
}

func TestStub_StopIdempotent(t *testing.T) {
	tn := &Tunnel{URL: "https://x"}
	_, _ = tn.Start(context.Background(), "addr")
	require.NoError(t, tn.Stop())
	require.NoError(t, tn.Stop(), "second Stop is a no-op")
}
