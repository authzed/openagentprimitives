package auditgen

import (
	"fmt"
	"strings"
)

// lensesList renders the canonical lenses as a numbered block injected into the
// audit prompt, so every run uses the same personas and charges.
func lensesList() string {
	var b strings.Builder
	for i, l := range Lenses {
		fmt.Fprintf(&b, "%d. **%s** — respond as %s. Hunt for: %s.\n", i+1, l.Title, l.Persona, l.Charge)
	}
	return b.String()
}

// ReportPath is the report location for a target: docs/audits/<date>-<slug>-audit.md.
func ReportPath(date, slug string) string {
	return fmt.Sprintf("docs/audits/%s-%s-audit.md", date, slug)
}

// AuditPrompt builds the headless-claude prompt that drives one audit pass over
// the target. The prompt is self-contained (it carries the lenses and the
// orchestration steps) so a `claude -p` run reproduces the pass; it also points
// at the canonical "Auditing" section of AGENTS.md for the full rules of thumb.
func AuditPrompt(t Target) string {
	var b strings.Builder

	fmt.Fprintf(
		&b, `You are running an AUDIT PASS over the openagentprimitives ("oap") codebase: a re-runnable, subagent-driven code review. Follow the "Auditing" section of AGENTS.md; this prompt is its operational encoding.

Scope of THIS pass:
%s

The %d lenses (each is one subagent persona):

%s
Orchestration:
1. FAN OUT. Dispatch one subagent per lens (via the Task tool). For a whole-repo pass, first partition the target into coherent subsystem groups and dispatch each lens per group; for a single package or a diff, one subagent per lens is enough. Each lens subagent reads the code (Read/Grep/Glob) and returns STRUCTURED findings only — for each: severity (BLOCKER/MAJOR/MINOR/NIT), a file:line, a one-sentence defect, and a concrete fix.
2. VERIFY. Treat every raw finding as a CANDIDATE, not a conclusion. Hand each candidate to a fresh skeptic subagent charged to REFUTE it against the current code (default to "refuted" when it cannot be reconfirmed). Drop refuted findings; keep survivors with a short confidence note. This mirrors the OWASP generator's "treat claims as untrusted, attempt to refute" discipline.
3. SYNTHESIZE. Dedupe overlapping findings across lenses, then write the report.

Output — write the report to %s with the Write tool (create docs/audits/ if absent). Structure:
- A short header: the target, the date (%s), and a one-line summary count per severity.
- One section per lens, in the order listed above, each ranked BLOCKER -> NIT. Every finding cites file:line and states its fix. Omit a lens's section only if it had zero surviving findings (note it as "none").
- A closing "Themes" paragraph naming the 2-3 cross-cutting patterns worth fixing first.

Rules of thumb:
- Findings are grounded, not speculative: every one cites a file:line you verified against the CURRENT tree; drop any you cannot reconfirm.
- The audit REVIEWS only. Make NO code edits and NO git writes; the ONLY file you write is the report at %s.
- Prefer fewer high-confidence findings over an exhaustive list of maybes.
`,
		t.describe(),
		len(Lenses),
		lensesList(),
		ReportPath(t.Date, t.Slug),
		t.Date,
		ReportPath(t.Date, t.Slug),
	)

	return b.String()
}

// describe renders the human-readable target scope for the prompt.
func (t Target) describe() string {
	switch t.Scope {
	case ScopeRecent:
		return fmt.Sprintf("The code changes in the following git diff (`git diff %s...HEAD`). Audit ONLY the changed code and what it directly touches.\n\n```diff\n%s\n```", t.DiffBase, t.Diff)
	case ScopePkg:
		return fmt.Sprintf("The package/directory `%s` (and the code it directly owns). Review both Go and any co-located frontend/config files.", t.Path)
	default: // ScopeAll
		return "The WHOLE repository — all of `pkg/`, `internal/` and `cmd/`. This is expensive by design: partition the tree into coherent subsystem groups and fan out. Do not skip binaries."
	}
}
