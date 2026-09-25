// pkg/channels/channelkinds/slack/app_home_prefs_test.go
//
// Table-driven tests for renderPreferenceBlocks — a pure Block Kit render
// function with no Slack runtime dependency. Each case asserts on the
// constructed block/element structs directly (type, action_id, option
// values/text, initial state), not on any rendered/serialized form.
package slack

import (
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

func js(s string) apiextv1.JSON   { return apiextv1.JSON{Raw: []byte(s)} }
func jsp(s string) *apiextv1.JSON { v := js(s); return &v }

const testClassRef = "ns/reviewbot"

func TestRenderPreferenceBlocks_Bool(t *testing.T) {
	cases := []struct {
		name        string
		value       *apiextv1.JSON
		wantChecked bool
	}{
		{name: "true value: initial-checked", value: jsp(`true`), wantChecked: true},
		{name: "false value: not initial-checked", value: jsp(`false`), wantChecked: false},
		{name: "unset value: not initial-checked", value: nil, wantChecked: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := []preferences.Resolved{{
				Name: "notifications", Type: "bool", Description: "Email notifications",
				Value: tc.value, Source: preferences.SourceUser,
			}}
			blocks := renderPreferenceBlocks(testClassRef, keys)
			require.Len(t, blocks, 2, "header + one key block")

			ib, ok := blocks[1].(*slackapi.InputBlock)
			require.True(t, ok, "bool key must render as an InputBlock wrapping checkboxes")
			elem, ok := ib.Element.(*slackapi.CheckboxGroupsBlockElement)
			require.True(t, ok, "bool key must render a checkboxes element")

			assert.Equal(t, "home:pref:ns/reviewbot:notifications", elem.ActionID)
			assert.True(t, ib.DispatchAction, "a checkbox change must round-trip to Task 12")
			require.Len(t, elem.Options, 1, "a bool preference is a single checkbox option")

			if tc.wantChecked {
				require.Len(t, elem.InitialOptions, 1)
				assert.Equal(t, elem.Options[0].Value, elem.InitialOptions[0].Value)
			} else {
				assert.Empty(t, elem.InitialOptions)
			}
		})
	}
}

func TestRenderPreferenceBlocks_Enum(t *testing.T) {
	keys := []preferences.Resolved{{
		Name: "tone", Type: "enum", Description: "Reply tone",
		Enum: []v1alpha1.PreferenceEnumValue{
			{Value: "formal", Description: "Formal"},
			{Value: "casual", Description: "Casual"},
			{Value: "terse", Description: ""}, // empty description falls back to value
		},
		Value:  jsp(`"casual"`),
		Source: preferences.SourceUser,
	}}
	blocks := renderPreferenceBlocks(testClassRef, keys)
	require.Len(t, blocks, 2)

	ib, ok := blocks[1].(*slackapi.InputBlock)
	require.True(t, ok, "enum key must render as an InputBlock wrapping a select")
	elem, ok := ib.Element.(*slackapi.SelectBlockElement)
	require.True(t, ok, "enum key must render a static_select element")

	assert.Equal(t, slackapi.OptTypeStatic, elem.Type)
	assert.Equal(t, "home:pref:ns/reviewbot:tone", elem.ActionID)
	assert.True(t, ib.DispatchAction, "a select change must round-trip to Task 12")
	require.Len(t, elem.Options, 3)

	assert.Equal(t, "formal", elem.Options[0].Value)
	assert.Equal(t, "Formal", elem.Options[0].Text.Text)
	assert.Equal(t, "casual", elem.Options[1].Value)
	assert.Equal(t, "Casual", elem.Options[1].Text.Text)
	assert.Equal(t, "terse", elem.Options[2].Value)
	assert.Equal(t, "terse", elem.Options[2].Text.Text, "empty Description falls back to the Value as option text")

	require.NotNil(t, elem.InitialOption, "the resolved value must be the initial option")
	assert.Equal(t, "casual", elem.InitialOption.Value)
}

func TestRenderPreferenceBlocks_String(t *testing.T) {
	keys := []preferences.Resolved{{
		Name: "signature", Type: "string", Description: "Email signature",
		Pattern: "^[A-Za-z ]+$",
		Value:   jsp(`"Jamie"`),
		Source:  preferences.SourceDefault,
	}}
	blocks := renderPreferenceBlocks(testClassRef, keys)
	require.Len(t, blocks, 2)

	ib, ok := blocks[1].(*slackapi.InputBlock)
	require.True(t, ok, "string key must render as an InputBlock wrapping a plain_text_input")
	elem, ok := ib.Element.(*slackapi.PlainTextInputBlockElement)
	require.True(t, ok, "string key must render a plain_text_input element")

	assert.Equal(t, "home:pref:ns/reviewbot:signature", elem.ActionID)
	assert.Equal(t, "Jamie", elem.InitialValue)

	patternSurfaced := (elem.Placeholder != nil && strings.Contains(elem.Placeholder.Text, "^[A-Za-z ]+$")) ||
		(ib.Hint != nil && strings.Contains(ib.Hint.Text, "^[A-Za-z ]+$"))
	assert.True(t, patternSurfaced, "the Pattern must be surfaced in the hint or placeholder")

	require.NotNil(t, ib.Hint, "the resolved value + source should be shown somewhere readable")
	assert.Contains(t, ib.Hint.Text, "Jamie")
}

func TestRenderPreferenceBlocks_Locked(t *testing.T) {
	keys := []preferences.Resolved{{
		Name: "max_tokens", Type: "int", Description: "Max reply length",
		Value:  jsp(`4096`),
		Source: preferences.SourceLocked,
		Locked: true,
		Note:   "your saved value is overridden by admin policy",
	}}
	blocks := renderPreferenceBlocks(testClassRef, keys)
	require.Len(t, blocks, 2)

	// Must NOT be an input block of any kind.
	_, isInput := blocks[1].(*slackapi.InputBlock)
	assert.False(t, isInput, "a locked key must not emit an input element")

	sb, ok := blocks[1].(*slackapi.SectionBlock)
	require.True(t, ok, "a locked key renders as a read-only section")
	require.NotNil(t, sb.Text)
	assert.Contains(t, sb.Text.Text, "4096", "the resolved value must be shown")
	assert.Contains(t, sb.Text.Text, "set by your administrator", "the admin note must be shown")
}

func TestRenderPreferenceBlocks_Header(t *testing.T) {
	keys := []preferences.Resolved{{Name: "notifications", Type: "bool", Value: jsp(`true`)}}
	blocks := renderPreferenceBlocks(testClassRef, keys)
	require.NotEmpty(t, blocks)

	sb, ok := blocks[0].(*slackapi.SectionBlock)
	require.True(t, ok, "the first block is the section header")
	require.NotNil(t, sb.Text)
	assert.Contains(t, sb.Text.Text, "Your preferences", "the header labels the section for the user")
	assert.NotContains(t, sb.Text.Text, testClassRef,
		"the header must not leak the namespace/class ref — the page is already about this one agent")
}

func TestRenderPreferenceBlocks_MultipleKeysOnePerBlock(t *testing.T) {
	keys := []preferences.Resolved{
		{Name: "notifications", Type: "bool", Value: jsp(`true`)},
		{Name: "tone", Type: "enum", Enum: []v1alpha1.PreferenceEnumValue{{Value: "casual"}}, Value: jsp(`"casual"`)},
	}
	blocks := renderPreferenceBlocks(testClassRef, keys)
	assert.Len(t, blocks, 3, "header + one block per key")
}
