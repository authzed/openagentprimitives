package uicomponents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

func parse(t *testing.T, view string) uicomponents.Declaration {
	t.Helper()
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":` + view + `}`))
	require.NoError(t, err)
	return d
}

// hookViewWithIntent is a one-hook page whose intent is the given text, for
// the length cases below. The intent is JSON-encoded so a long generated
// string cannot break the literal.
func hookViewWithIntent(intent string) string {
	b, err := json.Marshal(intent)
	if err != nil {
		panic(err) // marshalling a string cannot fail
	}
	return `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","intent":` + string(b) + `,"allowedComponents":["*"]}}]}`
}

func TestValidateHookRules(t *testing.T) {
	cases := []struct {
		name string
		view string
		want string // substring of the *ValidationError; "" = accepted
	}{
		{name: "a hook with name, intent and an explicit allowlist is accepted",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","intent":"x","allowedComponents":["ap:markdown","ap:form"]}}]}`},
		{name: "* admits the registry",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}`},
		{name: "an author default OUTSIDE the allowlist is accepted: the list bounds the agent, not the author",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"intake","allowedComponents":["ap:markdown","ap:form"]},"children":[
			  {"component":"ap:card","props":{"title":"Describe the agent"},"children":[{"component":"ap:markdown","props":{"body":"hi"}}]}]}]}`},
		{name: "an author default naming an UNREGISTERED component still fails, naming it",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"intake","allowedComponents":["*"]},"children":[{"component":"ap:bogus"}]}]}`,
			want: `unknown component type "ap:bogus"`},
		{name: "missing name fails",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"allowedComponents":["*"]}}]}`,
			want: "missing hook name"},
		{name: "duplicate names fail, at any depth",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}},{"component":"ap:card","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}]}]}`,
			want: `duplicate hook name "x"`},
		{name: "no allowlist fails: the author must say what the agent may put here",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x"}}]}`,
			want: `hook "x" declares no allowedComponents`},
		{name: "an allowlist naming an unregistered component fails the page",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["ap:nope"]}}]}`,
			want: `hook "x" allows unknown component type "ap:nope"`},
		{name: "an allowlist naming the structural type fails: a fill can never contain a hook",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["oap:generative"]}}]}`,
			want: `allows a structural type "oap:generative"`},
		{name: "a hook inside a hook fails",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"outer","allowedComponents":["*"]},"children":[{"component":"oap:generative","props":{"name":"inner","allowedComponents":["*"]}}]}]}`,
			want: `hook "inner" is nested inside hook "outer"`},
		{name: "a hook with no children is accepted (collapsed until filled)",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}]}`},
		{name: "an intent at exactly the limit is accepted",
			view: hookViewWithIntent(strings.Repeat("a", uicomponents.MaxHookIntentLen))},
		{name: "an intent one rune over the limit fails, naming the hook, the length and the limit",
			view: hookViewWithIntent(strings.Repeat("a", uicomponents.MaxHookIntentLen+1)),
			want: `hook "brief": intent is 1001 characters; the limit is 1000`},
		{name: "the cap counts RUNES, not bytes: multi-byte characters at the limit are accepted",
			view: hookViewWithIntent(strings.Repeat("é", uicomponents.MaxHookIntentLen))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(parse(t, tc.view), uicomponents.DefaultOptions())
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateRefusesAnUnnormalizedDeclaration(t *testing.T) {
	n := uicomponents.Node{Component: "ap:text"}
	err := uicomponents.Validate(uicomponents.Declaration{Slots: []uicomponents.Slot{{Name: "root", Default: &n}}}, uicomponents.DefaultOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not normalized")
	err = uicomponents.Validate(uicomponents.Declaration{}, uicomponents.DefaultOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing view")
}
