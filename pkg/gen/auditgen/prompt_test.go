package auditgen

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReportPath(t *testing.T) {
	assert.Equal(t, "docs/audits/2026-07-15-pkg-web-webui-chat-audit.md", ReportPath("2026-07-15", "pkg-web-webui-chat"))
}

// Every lens must survive into the prompt so a run covers the full set and
// re-runs stay consistent.
func TestAuditPrompt_ContainsEveryLens(t *testing.T) {
	p := AuditPrompt(Target{Scope: ScopePkg, Slug: "pkg-x", Date: "2026-07-15", Path: "pkg/x"})
	for _, l := range Lenses {
		assert.Contains(t, p, l.Title, "prompt names lens %q", l.Title)
		assert.Contains(t, p, l.Persona, "prompt carries persona for %q", l.Title)
	}
}

func TestAuditPrompt_CarriesOrchestrationAndGuardrails(t *testing.T) {
	p := AuditPrompt(Target{Scope: ScopePkg, Slug: "pkg-x", Date: "2026-07-15", Path: "pkg/x"})
	assert.Contains(t, p, ReportPath("2026-07-15", "pkg-x"), "names the report it writes")
	assert.Contains(t, p, "2026-07-15", "stamps the date")
	assert.Contains(t, p, "REFUTE", "verify pass refutes candidates")
	assert.Contains(t, p, "REVIEWS only", "review-only guardrail")
	assert.Contains(t, p, "FAN OUT", "fan-out orchestration step")
}

func TestAuditPrompt_ScopeDescription(t *testing.T) {
	cases := []struct {
		name       string
		target     Target
		contains   []string
		notContain []string
	}{
		{
			name:       "pkg scope names the path",
			target:     Target{Scope: ScopePkg, Slug: "pkg-web-webui-chat", Date: "2026-07-15", Path: "pkg/web/webui/chat"},
			contains:   []string{"pkg/web/webui/chat"},
			notContain: []string{"```diff", "WHOLE repository"},
		},
		{
			name:       "recent scope embeds the diff",
			target:     Target{Scope: ScopeRecent, Slug: "recent", Date: "2026-07-15", Diff: "DIFF-SENTINEL-42", DiffBase: "master"},
			contains:   []string{"```diff", "DIFF-SENTINEL-42", "master...HEAD"},
			notContain: []string{"WHOLE repository"},
		},
		{
			name:       "all scope sweeps the whole tree",
			target:     Target{Scope: ScopeAll, Slug: "all", Date: "2026-07-15"},
			contains:   []string{"WHOLE repository", "partition"},
			notContain: []string{"```diff"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := AuditPrompt(tc.target)
			for _, want := range tc.contains {
				assert.Contains(t, p, want)
			}
			for _, notWant := range tc.notContain {
				assert.NotContains(t, p, notWant)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	l, ok := Lookup("state")
	assert.True(t, ok)
	assert.Equal(t, "State management", l.Title)

	_, ok = Lookup("no-such-lens")
	assert.False(t, ok)
}
