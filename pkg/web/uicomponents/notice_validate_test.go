package uicomponents_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// noticePage is a one-hook page declaring two prompt actions, with the notice
// under test as the hook's author default. The notice is validated by the
// SAME pass a fill goes through (ResolveView calls Validate on the composite),
// so a case that fails here fails an agent's update_view the same way.
func noticePage(noticeProps string) string {
	return `{"actions":[{"name":"test_done","prompt":"Done testing."},{"name":"thats_not_right","prompt":"That's not right."}],
	  "view":{"component":"ap:stack","children":[
	    {"component":"oap:generative","props":{"name":"testRun","allowedComponents":["*"]},
	     "children":[{"component":"ap:notice","props":` + noticeProps + `}]}]}}`
}

func TestValidate_Notice(t *testing.T) {
	cases := []struct {
		name    string
		props   string
		wantErr string // substring; "" = accepted
	}{
		{name: "body alone is a valid notice",
			props: `{"body":"Test session started — I'll keep watching."}`},
		{name: "title, tone and two declared buttons are accepted",
			props: `{"title":"Test started","body":"Take your time.","tone":"info","buttons":[{"label":"Done testing","action":"test_done"},{"label":"That's not right","action":"thats_not_right"}]}`},
		{name: "a button naming an undeclared action fails the page, naming it",
			props:   `{"body":"x","buttons":[{"label":"Go","action":"nope"}]}`,
			wantErr: `undeclared action "nope"`},
		{name: "a button with an empty label fails",
			props:   `{"body":"x","buttons":[{"label":"","action":"test_done"}]}`,
			wantErr: "buttons[0]: label is required"},
		{name: "a button with no action fails",
			props:   `{"body":"x","buttons":[{"label":"Go"}]}`,
			wantErr: "buttons[0]: action is required"},
		{name: "five buttons fail: at most four",
			props:   `{"body":"x","buttons":[{"label":"a","action":"test_done"},{"label":"b","action":"test_done"},{"label":"c","action":"test_done"},{"label":"d","action":"test_done"},{"label":"e","action":"test_done"}]}`,
			wantErr: "at most 4 buttons"},
		{name: "an unknown tone fails, naming the legal values",
			props:   `{"body":"x","tone":"danger"}`,
			wantErr: `tone "danger" is not one of info, success, warning`},
		{name: "an empty body fails",
			props:   `{"title":"x"}`,
			wantErr: "body is required"},
		{name: "href is not a prop: a notice carries no navigation",
			props:   `{"body":"x","href":"/admin"}`,
			wantErr: `unknown prop "href"`},
		{name: "a button cannot carry a href either: nested unknown fields are refused by the decoder",
			props:   `{"body":"x","buttons":[{"label":"Go","action":"test_done","href":"/x"}]}`,
			wantErr: `unknown field "href"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := uicomponents.ParseDeclaration([]byte(noticePage(tc.props)))
			require.NoError(t, err)
			err = uicomponents.Validate(d, uicomponents.DefaultOptions())
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A notice is a leaf the agent writes: never structural, never a container.
// It fires actions through a LIST prop, not ActionProp, so the registry must
// say so — that is how ActionRefs finds its buttons without knowing what a
// notice is.
func TestNoticeIsALeafWithAnActionList(t *testing.T) {
	c, ok := registry.Get("ap:notice")
	require.True(t, ok, "ap:notice must be registered")
	assert.False(t, c.Structural)
	assert.False(t, c.AcceptsChildren)
	assert.Empty(t, c.ActionProp, "the buttons are a list, not one action prop")
	assert.Equal(t, "buttons", c.ActionListProp)
	assert.True(t, strings.HasPrefix(c.Type, "ap:"))
}
