package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIDTypesStringRoundTrip(t *testing.T) {
	assert.Equal(t, "c2NvcGU", canonicalUnverified("c2NvcGU", "test fixture").String())
	assert.Equal(t, "U0ALICE", RawExternalID("U0ALICE").String())
	assert.Equal(t, "user@example.com", Email("user@example.com").String())
	assert.Equal(t, "slack", Kind("slack").String())
	assert.Equal(t, "T0TEAM", TeamScope("T0TEAM").String())
}

func TestKindConstants(t *testing.T) {
	assert.Equal(t, Kind("slack"), KindSlack)
	assert.Equal(t, Kind("local"), KindLocal)
	assert.Equal(t, Kind("idp"), KindIdP)
	assert.Equal(t, Kind("fake"), KindFake)
	assert.Equal(t, Kind("bento"), KindBento)
}
