package uicomponents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

func TestParamStatesResolveThroughTheRegistry(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"slots":[{"name":"root","default":{"component":"ap:stack","children":[
	  {"component":"ap:select","props":{"param":"span","value":"30d","options":[{"value":"30d"}]}},
	  {"component":"ap:daterange","props":{"param":"window","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}},
	  {"component":"ap:text","props":{"text":"drives nothing"}}
	]}}]}`))
	require.NoError(t, err)

	got := uicomponents.ParamStates(d)
	assert.Equal(t, []uicomponents.ParamState{
		{Key: "span", Value: "30d"},
		{Key: "window.from", Value: "2026-01-01T00:00:00Z"},
		{Key: "window.to", Value: "2026-02-01T00:00:00Z"},
	}, got, "a daterange drives its EXPANDED keys and never its declared name")
}

func TestParamStatesOmitsNoKeyWhenTheValueIsUnset(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"slots":[{"name":"root","default":
	  {"component":"ap:select","props":{"param":"span","options":[{"value":"30d"}]}}}]}`))
	require.NoError(t, err)
	assert.Equal(t, []uicomponents.ParamState{{Key: "span", Value: ""}}, uicomponents.ParamStates(d),
		"a declared-but-unset parameter is still a KEY the args templates may reference; dropping it would tell read_view the parameter does not exist")
}
