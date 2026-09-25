package tool

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// What the viewer is told. The copy for a broken definition has to differ from
// the copy for a refusal, because the two ask different things of the reader:
// a refusal is answered by requesting access, and a broken definition is
// answered by waiting for someone to fix it. Telling the second reader the
// first thing sends them to ask for a permission that cannot help.
func TestErrorForStatusTellsAViewerWhichKindOfFailureThisIs(t *testing.T) {
	cases := []struct {
		name     string
		resp     channelevents.AppToolCallResponse
		contains string
		// absent is copy that would mislead this particular reader.
		absent []string
	}{
		{
			name:     "misconfigured: names it as the agent's configuration, not the viewer's access",
			resp:     channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusMisconfigured},
			contains: "configuration",
			absent:   []string{"access", "permission"},
		},
		{
			name:     "denied: still an access answer",
			resp:     channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusDenied},
			contains: "access",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := errorForStatus(tc.resp)
			require.Error(t, err)
			got := strings.ToLower(err.Error())
			assert.Contains(t, got, tc.contains)
			for _, a := range tc.absent {
				assert.NotContains(t, got, a, "this reader cannot act on that framing")
			}
		})
	}
}

// The runner's Message on a misconfigured response is the CEL failure itself.
// Rendering it would put an expression the viewer did not write, and cannot
// see the source of, into a dashboard — the no-internal-ops-in-user-messages
// rule, and useless besides. The real text goes to the monitoring channel.
func TestErrorForStatusNeverLeaksTheRuleToTheViewer(t *testing.T) {
	err := errorForStatus(channelevents.AppToolCallResponse{
		Status:  channelevents.AppToolCallStatusMisconfigured,
		Message: `mcp trust: validator error: constraints[1] eval: type conversion error from 'string' to 'google.protobuf.Timestamp'`,
	})
	require.Error(t, err)
	for _, leak := range []string{"constraints[", "cel", "timestamp", "google.protobuf", "validator"} {
		assert.NotContains(t, strings.ToLower(err.Error()), leak,
			"viewer copy must not quote the spec's internals")
	}
}

// ViewerMessage is the channel for copy a reply site authored FOR a viewer.
// No site authors it for this status — the text there is diagnostic — so the
// misconfigured arm must ignore the field entirely rather than treat a
// populated one as permission to render it.
func TestErrorForStatusIgnoresViewerMessageOnMisconfigured(t *testing.T) {
	err := errorForStatus(channelevents.AppToolCallResponse{
		Status:        channelevents.AppToolCallStatusMisconfigured,
		ViewerMessage: "constraints[1] blew up",
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "constraints[1]")
	assert.Contains(t, strings.ToLower(err.Error()), "configuration")
}
