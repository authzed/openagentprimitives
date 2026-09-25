package meta_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

func TestHookLine(t *testing.T) {
	cases := []struct {
		name string
		hook uicomponents.Hook
		want string
	}{
		{"intent and a closed allowlist", uicomponents.Hook{Name: "phase", Intent: "The stage timeline.", AllowedComponents: []string{"ap:steps"}},
			"phase: The stage timeline. (allowed: ap:steps)"},
		{"several allowed, author order kept", uicomponents.Hook{Name: "intake", Intent: "The first form.", AllowedComponents: []string{"ap:markdown", "ap:form"}},
			"intake: The first form. (allowed: ap:markdown, ap:form)"},
		{"star means the registry", uicomponents.Hook{Name: "brief", Intent: "The summary.", AllowedComponents: []string{"*"}},
			"brief: The summary. (allowed: any registered component)"},
		{"no intent says so rather than printing nothing", uicomponents.Hook{Name: "panel", AllowedComponents: []string{"*"}},
			"panel: no instruction from the author (allowed: any registered component)"},
		{"internal whitespace collapses: the line must stay one line in both consumers",
			uicomponents.Hook{Name: "brief", Intent: "a line\n  and another\t tabbed", AllowedComponents: []string{"*"}},
			"brief: a line and another tabbed (allowed: any registered component)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, meta.HookLine(tc.hook))
		})
	}
}
