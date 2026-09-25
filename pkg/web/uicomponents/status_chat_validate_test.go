package uicomponents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckStatus(t *testing.T) {
	cases := []struct {
		name    string
		props   StatusProps
		wantErr string
	}{
		{name: "running with text and an RFC3339 since: valid", props: StatusProps{State: "running", Text: "Running since", Since: "2026-09-13T12:52:00Z"}},
		{name: "idle with text: valid", props: StatusProps{State: "idle", Text: "Not started"}},
		{name: "unknown state: refused", props: StatusProps{State: "busy", Text: "x"}, wantErr: `ap:status: state "busy"`},
		{name: "empty text: refused, a state line says something", props: StatusProps{State: "ended"}, wantErr: "text is required"},
		// The browser draws the moment in the VIEWER's time zone, so it needs
		// a moment, not a clock reading someone else already localized.
		{name: "a wall-clock since: refused, the moment is what the browser localizes", props: StatusProps{State: "running", Text: "Running since", Since: "12:52"}, wantErr: `ap:status: since "12:52" is not an RFC3339 time`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStatus(&tc.props)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCheckChat(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{name: "ns/name: valid", ref: "ws-c829e029cffc/demo-haiku-d44fc350"},
		{name: "no slash: refused", ref: "demo-haiku", wantErr: "sessionRef must be ns/name"},
		{name: "three segments: refused", ref: "a/b/c", wantErr: "sessionRef must be ns/name"},
		{name: "uppercase segment: refused as not a DNS-1123 label", ref: "WS/demo", wantErr: "not a DNS-1123 label"},
		{name: "empty: refused", ref: "", wantErr: "sessionRef must be ns/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChat(&ChatProps{SessionRef: tc.ref})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestAgentLinkEmbedIsAPlainFlag(t *testing.T) {
	// embed changes what a click does, not what the link may point at: the same
	// namespace/agentClass/label checks apply unchanged.
	assert.NoError(t, checkAgentLink(&AgentLinkProps{Namespace: "ws-abc123456789", AgentClass: "demo-haiku", Label: "Start the test", Embed: true}))
	assert.Error(t, checkAgentLink(&AgentLinkProps{Namespace: "Bad", AgentClass: "demo-haiku", Label: "x", Embed: true}))
}
