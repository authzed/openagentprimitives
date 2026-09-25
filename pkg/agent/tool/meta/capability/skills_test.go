package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillsActiveOnlyWithBodies(t *testing.T) {
	c, ok := Lookup("skills")
	require.True(t, ok)
	assert.True(t, c.DefaultOn(), "skills is default-on")
	assert.False(t, c.Infrastructural(), "skills is not infrastructural")

	// No resolved skill bodies → nothing to load, inactive (not a skip).
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: RunnerEnv{SkillBodies: nil}})
	assert.Nil(t, skip)
	assert.Empty(t, tools, "no skill bodies → no load_skill")

	// With resolved bodies → load_skill. SkillBodies is the real
	// map[string]string type meta.NewLoadSkill consumes (canonical name → body).
	tools, skip = c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: RunnerEnv{
		SkillBodies: map[string]string{"acme//greet@v1": "# Greet\nSay hi."},
	}})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"load_skill"}, toolNames(tools))
}
