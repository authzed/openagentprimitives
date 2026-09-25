package uicomponents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// timeline is one ap:steps with the given steps JSON; hookBound is a hook
// whose `step` names an id. Composed per case so each row differs by the
// fact under test.
func timeline(steps string) string {
	return `{"component":"ap:steps","props":{"steps":` + steps + `}}`
}

func hookBound(name, step string) string {
	return `{"component":"oap:generative","props":{"name":"` + name + `","allowedComponents":["*"],"step":"` + step + `"}}`
}

const twoSteps = `[{"id":"assess","label":"Intake","state":"done"},{"id":"tools","label":"Tools","state":"active"}]`

func TestValidateStepBindings(t *testing.T) {
	cases := []struct {
		name    string
		view    string
		wantErr string // substring; "" = accepted
	}{
		{name: "a hook bound to a declared step id is accepted",
			view: `{"component":"ap:stack","children":[` + timeline(twoSteps) + `,` + hookBound("brief", "assess") + `]}`},
		{name: "two hooks may bind the same step",
			view: `{"component":"ap:stack","children":[` + timeline(twoSteps) + `,` + hookBound("brief", "assess") + `,` + hookBound("intake", "assess") + `]}`},
		{name: "a timeline with ids and no bound hooks is accepted",
			view: `{"component":"ap:stack","children":[` + timeline(twoSteps) + `]}`},
		{name: "steps without ids remain legal: binding is opt-in",
			view: `{"component":"ap:stack","children":[` + timeline(`[{"label":"Intake","state":"active"}]`) + `]}`},
		{name: "a hook binding an id the timeline does not declare fails, listing the ids",
			view:    `{"component":"ap:stack","children":[` + timeline(twoSteps) + `,` + hookBound("brief", "build") + `]}`,
			wantErr: `hook "brief" binds step "build", which the timeline does not declare (ids: assess, tools)`},
		{name: "a bound hook with no timeline fails",
			view:    `{"component":"ap:stack","children":[` + hookBound("brief", "assess") + `]}`,
			wantErr: `hook "brief" binds step "assess", but the page has 0 ap:steps timelines; step binding needs exactly one`},
		{name: "a bound hook with two timelines fails",
			view:    `{"component":"ap:stack","children":[` + timeline(twoSteps) + `,` + timeline(twoSteps) + `,` + hookBound("brief", "assess") + `]}`,
			wantErr: `the page has 2 ap:steps timelines`},
		{name: "a bound hook whose timeline binds its steps fails: ids must be literal",
			view:    `{"component":"ap:stack","children":[{"component":"ap:steps","bindings":{"steps":{"source":"memory","ref":"demo_steps"}}},` + hookBound("brief", "assess") + `]}`,
			wantErr: `the timeline's steps are bound to a source; step ids must be literal`},
		{name: "a step id that is not a label fails",
			view:    `{"component":"ap:stack","children":[` + timeline(`[{"id":"Not A Label","label":"x"}]`) + `]}`,
			wantErr: `ap:steps: steps[0].id "Not A Label"`},
		{name: "duplicate step ids fail",
			view:    `{"component":"ap:stack","children":[` + timeline(`[{"id":"a","label":"x"},{"id":"a","label":"y"}]`) + `]}`,
			wantErr: `ap:steps: steps[1].id "a" is declared twice`},
		{name: "pinned is a boolean prop on the timeline",
			view: `{"component":"ap:stack","children":[{"component":"ap:steps","props":{"pinned":true,"steps":` + twoSteps + `}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := uicomponents.ParseDeclaration([]byte(`{"actions":[],"view":` + tc.view + `}`))
			require.NoError(t, err)
			// A memory-source binding needs no tool grant (validateBindings
			// gates only the tool source), so the bound-timeline row fails on
			// the step rule under the default options, as intended.
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
