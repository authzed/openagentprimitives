package slack

import (
	"strings"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlackKind_SchemaContributor(t *testing.T) {
	var k *Kind = &Kind{}
	var c channelkinds.SchemaContributor = k
	frag := c.SpiceDBSchemaFragment()
	require.NotNil(t, frag)
	require.NotEmpty(t, frag.RawZed)

	for _, def := range []string{
		"definition slack_workspace",
		"definition slack_user",
		"definition slack_bot",
		"definition slack_channel",
		"definition slack_usergroup",
	} {
		assert.Truef(t, strings.Contains(frag.RawZed, def),
			"missing %q in RawZed:\n%s", def, frag.RawZed)
	}

	// `agent` and `string` are base-scaffold-owned
	// (pkg/authz/spicedb/schema/schema.zed) precisely so this fragment (and
	// any other kind's) can reference them without a composition conflict —
	// they must NOT appear as definitions here, but the relations that
	// reference them must still be present.
	assert.NotContains(t, frag.RawZed, "definition agent",
		"the agent sentinel type is base-scaffold-owned; this fragment must not redeclare it")
	assert.NotContains(t, frag.RawZed, "definition string",
		"the relhash sentinel type is base-scaffold-owned; this fragment must not redeclare it")
	assert.Contains(t, frag.RawZed, "relation user: user | agent")
	assert.Contains(t, frag.RawZed, "relation relhash: string")

	// Audience-resolver lookup uses LookupSubjects(slack_channel:<id>, view),
	// so the view permission must be present.
	assert.Contains(t, frag.RawZed, "permission view = member + workspace->member")
}
