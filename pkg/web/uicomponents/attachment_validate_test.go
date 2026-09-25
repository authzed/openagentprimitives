package uicomponents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

func validateAttachmentProps(t *testing.T, propsJSON string) error {
	t.Helper()
	var props map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(propsJSON), &props))
	return uicomponents.Validate(propsNode("ap:attachment", props), uicomponents.DefaultOptions())
}

func TestValidate_Attachment(t *testing.T) {
	// The id the artifact service actually mints, so the shape check here
	// cannot drift from the minter without this row going red.
	minted := memory.NewID(memartifact.Kind{})
	cases := []struct {
		name    string
		props   string
		wantErr string // substring; "" = valid
	}{
		{name: "a minted artifact id is accepted", props: `{"artifact":"` + minted + `"}`},
		{name: "with a label", props: `{"artifact":"artifact-0123456789abcdef","label":"Your draft"}`},
		{name: "no artifact fails", props: `{"label":"x"}`, wantErr: "artifact is required"},
		{name: "a render handle is not an artifact id", props: `{"artifact":"ar-sess-live-0a1b2c"}`, wantErr: `artifact "ar-sess-live-0a1b2c" is not an artifact id`},
		{name: "a short id fails", props: `{"artifact":"artifact-0123"}`, wantErr: "is not an artifact id"},
		{name: "uppercase hex fails", props: `{"artifact":"artifact-0123456789ABCDEF"}`, wantErr: "is not an artifact id"},
		{name: "href is not a prop: the shell builds the link", props: `{"artifact":"artifact-0123456789abcdef","href":"/x"}`, wantErr: `unknown prop "href"`},
		{name: "url is not a prop either", props: `{"artifact":"artifact-0123456789abcdef","url":"https://example.invalid"}`, wantErr: `unknown prop "url"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAttachmentProps(t, tc.props)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestAttachmentIsALeafWithNoActions(t *testing.T) {
	c, ok := registry.Get("ap:attachment")
	require.True(t, ok)
	assert.False(t, c.Structural)
	assert.False(t, c.AcceptsChildren)
	assert.Empty(t, c.ActionProp)
	assert.Empty(t, c.Bindable, "the id is the agent's own claim, checked by the download route")
}
