package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/mcpfront"
)

func TestValidateConfigAccessTokenLifetime(t *testing.T) {
	cases := []struct {
		name     string
		lifetime time.Duration
		wantErr  bool
	}{
		{name: "zero (an empty or mistyped env value): rejected", lifetime: 0, wantErr: true},
		{name: "negative: rejected", lifetime: -time.Minute, wantErr: true},
		{name: "ten years: rejected as over the maximum", lifetime: 87600 * time.Hour, wantErr: true},
		{name: "just over the maximum: rejected", lifetime: mcpfront.MaxLifetime + time.Nanosecond, wantErr: true},
		{name: "the maximum itself: accepted", lifetime: mcpfront.MaxLifetime},
		{name: "the flag default (2160h): accepted", lifetime: 2160 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(&config{accessTokenLifetime: tc.lifetime})
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--accesstoken-lifetime", "the error must name the flag")
			assert.Contains(t, err.Error(), "WEBD_ACCESSTOKEN_LIFETIME", "and the env var that usually sets it")
		})
	}
}

// TestRunRefusesInvalidLifetimeBeforeStarting: the check is wired into run,
// ahead of everything that touches the cluster — a bad value never serves.
func TestRunRefusesInvalidLifetimeBeforeStarting(t *testing.T) {
	err := run(context.Background(), &config{accessTokenLifetime: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--accesstoken-lifetime")
}
