package uicomponents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// validateAgentLinkProps decodes propsJSON as the JSON object a declaration's
// props map would hold, and validates it as a single ap:agentlink node —
// mirroring propsNode/Validate from validate_props_test.go. It does NOT call
// installFixtureVocabulary: that helper resets the registry down to the
// fixture-only types (ap:stack/ap:box/ap:leaf), which would make ap:agentlink
// itself unknown. ap:agentlink is real, production-registered vocabulary
// (components.go's init), so it is already present in the package-global
// registry this test package imports.
func validateAgentLinkProps(t *testing.T, propsJSON string) error {
	t.Helper()
	var props map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(propsJSON), &props))
	return uicomponents.Validate(propsNode("ap:agentlink", props), uicomponents.DefaultOptions())
}

func TestValidate_AgentLink(t *testing.T) {
	cases := []struct {
		name    string
		props   string
		wantErr string // substring; "" = valid
	}{
		{name: "well-formed", props: `{"namespace":"ws-abc","agentClass":"demo-agent","label":"Try it","prompt":"hello"}`},
		{name: "prompt optional", props: `{"namespace":"ws-abc","agentClass":"demo-agent","label":"Try it"}`},
		{name: "href is not a prop", props: `{"namespace":"ws-abc","agentClass":"demo-agent","label":"x","href":"/admin"}`, wantErr: `unknown prop "href"`},
		{name: "namespace must be a label", props: `{"namespace":"Not A Label","agentClass":"demo-agent","label":"x"}`, wantErr: "namespace"},
		{name: "agentClass must be a label", props: `{"namespace":"ws-abc","agentClass":"../x","label":"x"}`, wantErr: "agentClass"},
		{name: "label required", props: `{"namespace":"ws-abc","agentClass":"demo-agent"}`, wantErr: "label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentLinkProps(t, tc.props)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidate_AgentLink_EmptyPropsStillChecked pins the empty-props early
// return in validateProps: an ap:agentlink node with NO props at all must
// still fail on the required Label, not silently validate clean the way an
// optional-everything component does.
func TestValidate_AgentLink_EmptyPropsStillChecked(t *testing.T) {
	err := validateAgentLinkProps(t, `{}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "label")
}
