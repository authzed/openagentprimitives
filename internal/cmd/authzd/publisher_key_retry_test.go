package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRegistrar struct {
	failsLeft int
	calls     int
}

func (f *fakeRegistrar) RegisterPublisherKey(_ context.Context, _ string, _ ed25519.PublicKey) error {
	f.calls++
	if f.failsLeft > 0 {
		f.failsLeft--
		return errors.New("context deadline exceeded")
	}
	return nil
}

func TestRegisterPublisherKeyWithRetry_SucceedsOnceOperatorComesUp(t *testing.T) {
	reg := &fakeRegistrar{failsLeft: 1}
	err := registerPublisherKeyWithRetry(context.Background(), reg, "kid-abc", nil, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 2, reg.calls) // failed once, then succeeded
}

func TestRegisterPublisherKeyWithRetry_ReturnsErrorAfterCeiling(t *testing.T) {
	reg := &fakeRegistrar{failsLeft: 1000}
	err := registerPublisherKeyWithRetry(context.Background(), reg, "kid-abc", nil, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register publisher key")
}
