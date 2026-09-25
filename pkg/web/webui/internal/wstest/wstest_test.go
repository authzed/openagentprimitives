package wstest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceFactor is a build-tag constant, so the arithmetic every case below
// asserts has to be written in terms of it rather than a literal: the same
// test file runs under `go test` and `go test -race`, and only one of those
// two builds sees a factor of 3.
func scaled(base time.Duration, env float64) time.Duration {
	return time.Duration(float64(base) * float64(raceFactor) * env)
}

func TestScale_NoEnvVar_AppliesOnlyTheRaceFactor(t *testing.T) {
	t.Setenv(TimeoutScaleEnv, "")
	assert.Equal(t, scaled(2*time.Second, 1), Scale(2*time.Second))
	assert.Equal(t, scaled(3*time.Second, 1), Scale(3*time.Second))
}

func TestScale_PositiveEnvVar_MultipliesOnTopOfTheRaceFactor(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want float64
	}{
		{name: "integer 4: deadline is 4x the race-scaled base", env: "4", want: 4},
		{name: "fractional 1.5: deadline is 1.5x the race-scaled base", env: "1.5", want: 1.5},
		{name: "1: deadline is the race-scaled base unchanged", env: "1", want: 1},
		{name: "below 1 but positive: shortens, because the operator asked", env: "0.5", want: 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TimeoutScaleEnv, tc.env)
			assert.Equal(t, scaled(2*time.Second, tc.want), Scale(2*time.Second))
		})
	}
}

func TestScale_UnusableEnvVar_IsIgnoredRatherThanApplied(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{name: "not a number: ignored, base keeps the race factor only", env: "slow"},
		{name: "zero: ignored, because a zero deadline is already expired", env: "0"},
		{name: "negative: ignored, because a past deadline fails every read", env: "-2"},
		{name: "empty: treated as unset", env: ""},
		{name: "trailing unit: ignored, this is a multiplier and not a duration", env: "4s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TimeoutScaleEnv, tc.env)
			assert.Equal(t, scaled(2*time.Second, 1), Scale(2*time.Second))
		})
	}
}

func TestScale_NonPositiveBase_IsReturnedUntouched(t *testing.T) {
	t.Setenv(TimeoutScaleEnv, "4")
	assert.Zero(t, Scale(0), "a zero duration means 'no deadline' to net.Conn; scaling it must not invent one")
	assert.Equal(t, -1*time.Second, Scale(-1*time.Second), "a negative base is already-expired by construction")
}

func TestDeadline_IsNowPlusTheScaledBase(t *testing.T) {
	t.Setenv(TimeoutScaleEnv, "2")
	base := 2 * time.Second
	before := time.Now()
	got := Deadline(base)
	after := time.Now()

	want := scaled(base, 2)
	require.False(t, got.Before(before.Add(want)), "Deadline must be at least now+scaled when measured before the call")
	assert.False(t, got.After(after.Add(want)), "Deadline must be at most now+scaled when measured after the call")
}
