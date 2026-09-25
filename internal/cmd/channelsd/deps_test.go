package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForDeps_SucceedsAfterDepsBecomeReady(t *testing.T) {
	memFails, azFails := 1, 1
	mem := func(context.Context) error {
		if memFails > 0 {
			memFails--
			return errors.New("memory /healthz: HTTP 503")
		}
		return nil
	}
	az := func(context.Context) error {
		if azFails > 0 {
			azFails--
			return errors.New("spicedb not ready")
		}
		return nil
	}
	require.NoError(t, waitForDeps(context.Background(), mem, az, logr.Discard()))
}

func TestWaitForDeps_HonorsContextCancellation(t *testing.T) {
	fail := func(context.Context) error { return errors.New("never ready") }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := waitForDeps(ctx, fail, fail, logr.Discard())
	assert.Error(t, err)
}
