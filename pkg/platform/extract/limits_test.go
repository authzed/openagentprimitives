package extract_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

func TestParseLimitsOmittedFieldsTakeTheCeiling(t *testing.T) {
	got, err := extract.ParseLimits([]byte(`{"archives":{"maxMembers":32}}`), extract.DefaultLimits)
	require.NoError(t, err)

	assert.Equal(t, 32, got.MaxMembers, "the named field is honored")
	assert.Equal(t, extract.DefaultLimits.MaxUncompressedTotal, got.MaxUncompressedTotal,
		"an omitted field takes the ceiling, NEVER zero — zero reads as unlimited to any `n > lim` check")
	assert.Equal(t, extract.DefaultLimits.MaxMemberBytes, got.MaxMemberBytes)
	assert.Equal(t, extract.DefaultLimits.MaxRatio, got.MaxRatio)
	assert.Equal(t, extract.DefaultLimits.MaxWallClock, got.MaxWallClock,
		"wall clock protects the pod, not the class, and is not configurable")
}

func TestParseLimitsEmptyAndAbsentConfigTakeTheCeiling(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"archives":{}}`, `null`} {
		got, err := extract.ParseLimits([]byte(raw), extract.DefaultLimits)
		require.NoError(t, err, "raw %q", raw)
		assert.Equal(t, extract.DefaultLimits, got, "raw %q must yield the ceiling unchanged", raw)
	}
}

// The rule the whole feature turns on. A class may TIGHTEN a bound; it must
// never loosen one past what the deployment will serve, or an AgentClass edit
// raises the ceiling on a shared extractord.
func TestParseLimitsClampsRatherThanSubstitutes(t *testing.T) {
	got, err := extract.ParseLimits([]byte(`{"archives":{
		"maxUncompressedTotal": 1099511627776,
		"maxMemberBytes": 1099511627776,
		"maxMembers": 100000,
		"maxRatio": 100000
	}}`), extract.DefaultLimits)
	require.NoError(t, err)

	assert.Equal(t, extract.DefaultLimits.MaxUncompressedTotal, got.MaxUncompressedTotal)
	assert.Equal(t, extract.DefaultLimits.MaxMemberBytes, got.MaxMemberBytes)
	assert.Equal(t, extract.DefaultLimits.MaxMembers, got.MaxMembers)
	assert.Equal(t, extract.DefaultLimits.MaxRatio, got.MaxRatio)
}

func TestParseLimitsTightensWhenAsked(t *testing.T) {
	got, err := extract.ParseLimits([]byte(`{"archives":{
		"maxUncompressedTotal": 1048576,
		"maxMemberBytes": 524288,
		"maxMembers": 8,
		"maxRatio": 10
	}}`), extract.DefaultLimits)
	require.NoError(t, err)

	assert.Equal(t, int64(1<<20), got.MaxUncompressedTotal)
	assert.Equal(t, int64(512<<10), got.MaxMemberBytes)
	assert.Equal(t, 8, got.MaxMembers)
	assert.Equal(t, 10, got.MaxRatio)
}

// A non-positive value is a config error, not "unlimited" and not "default":
// silently reading 0 as either would be the fail-open this type exists to
// prevent, and silently reading it as the ceiling would hide a typo.
func TestParseLimitsRejectsNonPositiveValues(t *testing.T) {
	for _, raw := range []string{
		`{"archives":{"maxMembers":0}}`,
		`{"archives":{"maxMembers":-1}}`,
		`{"archives":{"maxUncompressedTotal":0}}`,
		`{"archives":{"maxRatio":-5}}`,
	} {
		_, err := extract.ParseLimits([]byte(raw), extract.DefaultLimits)
		require.Error(t, err, "raw %q must be rejected", raw)
	}
}

func TestParseLimitsRejectsMalformedJSON(t *testing.T) {
	_, err := extract.ParseLimits([]byte(`{"archives":{"maxMembers":"lots"}}`), extract.DefaultLimits)
	require.Error(t, err, "a malformed value must fail closed at apply time, not silently at upload time")
}

// A ceiling with a zero field would let a config value through unclamped.
func TestParseLimitsRefusesAZeroCeiling(t *testing.T) {
	_, err := extract.ParseLimits([]byte(`{"archives":{"maxMembers":10}}`),
		extract.Limits{MaxMembers: 0, MaxWallClock: time.Second})
	require.Error(t, err, "a zero ceiling cannot clamp anything and must be refused rather than trusted")
}
