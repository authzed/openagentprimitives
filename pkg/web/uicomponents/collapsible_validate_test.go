package uicomponents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

func TestValidate_Collapsible(t *testing.T) {
	cases := []struct {
		name    string
		props   string
		wantErr string // substring; "" = valid
	}{
		{name: "title alone: open by default", props: `{"title":"Brief"}`},
		{name: "collapsed is a boolean", props: `{"title":"Brief","collapsed":true}`},
		{name: "an empty title fails: a fold with no label cannot be reopened by name", props: `{"title":"  "}`, wantErr: "title is required"},
		{name: "no title fails the same way", props: `{"collapsed":true}`, wantErr: "title is required"},
		{name: "collapsed must be a boolean", props: `{"title":"x","collapsed":"yes"}`, wantErr: "invalid props"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var props map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(tc.props), &props))
			err := uicomponents.Validate(propsNode("ap:collapsible", props), uicomponents.DefaultOptions())
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCollapsibleIsAContainer(t *testing.T) {
	c, ok := registry.Get("ap:collapsible")
	require.True(t, ok)
	assert.True(t, c.AcceptsChildren, "a fold holds the content it folds")
	assert.False(t, c.Structural)
	assert.Empty(t, c.Bindable)
}
