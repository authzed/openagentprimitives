package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFineGrainedInfoLeakageIdentity(t *testing.T) {
	c := fineGrainedInfoLeakageCapability{}
	assert.Equal(t, "fine_grained_info_leakage", c.Name())
	assert.False(t, c.DefaultOn(),
		"per-datum tracking costs a LookupSubjects per tool call plus durable storage per datum, and changes what the disclosure gate permits — it must be asked for")
	assert.False(t, c.Infrastructural(),
		"an operator must be able to turn this off; it is not part of the always-on core")
}

func TestFineGrainedInfoLeakageParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "absent config is legal", raw: ``},
		{name: "empty object is legal", raw: `{}`},
		{name: "enabled:false alone is legal and still parses", raw: `{"enabled":false}`},
		{name: "unknown config key is rejected", raw: `{"enabeld":true}`, wantErr: "unknown field"},
		{name: "wrong-typed enabled value is rejected", raw: `{"enabled":"yes"}`, wantErr: "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			_, err := fineGrainedInfoLeakageCapability{}.ParseConfig(raw)
			if tc.wantErr != "" {
				require.Error(t, err,
					"a bad config must surface as a CapabilitiesValid condition, never fall back to a silent default")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestFineGrainedInfoLeakageOffersNoTools pins that this capability is a
// behavioural switch, not a tool grant.
//
// Returning (nil, nil) rather than a SkipReason is deliberate and matches
// user_profile: nothing is wrong, there is simply no tool to contribute, and a
// SkipReason would log a warning on every session that enables it.
func TestFineGrainedInfoLeakageOffersNoTools(t *testing.T) {
	tools, skip := fineGrainedInfoLeakageCapability{}.Offer(OfferContext{})
	assert.Empty(t, tools, "this capability gates hook behaviour; it contributes no meta tools")
	assert.Nil(t, skip, "no tool is not a skip — a SkipReason here would warn on every session")
}

// TestFineGrainedInfoLeakageIsOffByDefault is the regression that stops
// per-datum provenance from becoming an accidental global behaviour change.
//
// Every existing AgentClass omits this key. If absence ever resolved to active,
// each of them would silently start paying a LookupSubjects per tool call and
// writing a durable tag per datum, and their disclosure gate would begin
// answering from a different set — with nothing in any diff to say so.
//
// Asserted through ActiveWithConfig rather than by reading the map directly,
// because that is the single function every consumer must use; a test that
// inspected spec.capabilities itself could pass while the function consumers
// actually call disagreed.
func TestFineGrainedInfoLeakageIsOffByDefault(t *testing.T) {
	const name = "fine_grained_info_leakage"

	_, active, err := ActiveWithConfig(classWithCaps(t, "", ""), name)
	require.NoError(t, err)
	assert.False(t, active, "a class that does not name the capability must get today's coarse behaviour, unchanged")

	_, active, err = ActiveWithConfig(classWithCaps(t, "artifacts", `{}`), name)
	require.NoError(t, err)
	assert.False(t, active, "declaring some OTHER capability must not enable this one")

	_, active, err = ActiveWithConfig(classWithCaps(t, name, `{}`), name)
	require.NoError(t, err)
	assert.True(t, active, "a class that asks for it must get it, or the capability is unreachable")

	_, active, err = ActiveWithConfig(classWithCaps(t, name, `{"enabled":false}`), name)
	require.NoError(t, err)
	assert.False(t, active, "an explicit enabled:false must turn it off, not merely parse")
}
