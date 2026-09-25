package uicomponents_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

func TestPlatformVocabularyIsRegistered(t *testing.T) {
	want := []string{
		"ap:agentlink", "ap:alert", "ap:attachment", "ap:badge", "ap:button", "ap:card", "ap:chart", "ap:chat", "ap:collapsible", "ap:daterange",
		"ap:empty", "ap:error", "ap:form", "ap:grid", "ap:heading", "ap:markdown",
		"ap:metric", "ap:notice", "ap:progress", "ap:question", "ap:raw_html", "ap:select", "ap:session_view", "ap:skeleton",
		"ap:stack", "ap:status", "ap:steps", "ap:table", "ap:tabs", "ap:text", "oap:generative", "oap:page",
	}
	assert.Equal(t, want, registry.Keys(), "the v1 vocabulary is a closed, reviewed set")
}

func TestEveryComponentIsWellFormed(t *testing.T) {
	for _, c := range registry.All() {
		t.Run(c.Type, func(t *testing.T) {
			if c.Structural {
				assert.True(t, strings.HasPrefix(c.Type, "oap:"),
					"a structural type lives in the oap: namespace, never ap:, so an agent-facing schema can never name it")
			} else {
				assert.True(t, strings.HasPrefix(c.Type, "ap:"),
					"type strings are namespaced so ext:<bundle>/<name> stays available")
			}

			require.NotNil(t, c.Props, "props must be a struct value, never nil")
			assert.Equal(t, reflect.Struct, reflect.TypeOf(c.Props).Kind(),
				"props must be a struct so validation can decode into a fresh copy")

			props := reflect.TypeOf(c.Props)
			for _, name := range c.Bindable {
				found := false
				for i := 0; i < props.NumField(); i++ {
					tag := props.Field(i).Tag.Get("json")
					if strings.Split(tag, ",")[0] == name {
						found = true
						break
					}
				}
				assert.True(t, found,
					"bindable prop %q must exist on %s's props struct", name, c.Type)
			}
		})
	}
}

func TestGenerativeIsStructuralAndAcceptsChildren(t *testing.T) {
	c, ok := registry.Get(uicomponents.GenerativeType)
	require.True(t, ok, "oap:generative must be registered")
	assert.True(t, c.Structural, "a hook is never published to the agent")
	assert.True(t, c.AcceptsChildren, "a hook's author default is its children")
	assert.Empty(t, c.Bindable, "a hook carries no data of its own")
	assert.Empty(t, c.ActionProp, "a hook fires nothing")
}

func TestQuestionAndProgressAreOrdinaryLeaves(t *testing.T) {
	for _, typ := range []string{"ap:question", "ap:progress"} {
		c, ok := registry.Get(typ)
		require.True(t, ok, "%s must be registered", typ)
		assert.False(t, c.Structural, "%s is content the agent writes, never structure", typ)
		assert.False(t, c.AcceptsChildren, "%s is a leaf", typ)
		assert.Empty(t, c.ActionProp, "%s names no declared action: its answer travels the transcript route", typ)
	}
}
