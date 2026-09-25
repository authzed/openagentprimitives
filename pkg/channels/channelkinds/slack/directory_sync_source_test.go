package slack_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// The hash sentinel rides a relation like any other, so the source must
// claim it. An unclaimed sentinel is writable by anything, and "nothing
// else can forge the sentinel" is exactly the property that makes the
// short-circuit trustworthy.
func TestDirectorySyncSource_ClaimsItsHashSentinels(t *testing.T) {
	claims := slack.DirectorySyncSource.Claims

	for _, want := range []string{
		"slack_channel#relhash",
		"slack_workspace#relhash",
		"slack_usergroup#relhash",
	} {
		assert.Contains(t, claims, want, "the sentinel relation must be claimed, not merely written")
	}
}
