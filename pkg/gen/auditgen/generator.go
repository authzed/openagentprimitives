package auditgen

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/gen/claudeexec"
)

// DefaultModel is the claude model alias used when none is configured.
const DefaultModel = "opus"

// DefaultDiffBase is the git ref the "recent" scope diffs against by default.
const DefaultDiffBase = "master"

// Scope identifies what an audit pass covers.
type Scope string

const (
	ScopeAll    Scope = "all"    // the whole repo (pkg/ + internal/ + cmd/)
	ScopeRecent Scope = "recent" // git diff DefaultDiffBase...HEAD
	ScopePkg    Scope = "pkg"    // a named package/directory
)

// Target is a fully-resolved audit target: the scope, the report-filename slug,
// the date the report is stamped with, and scope-specific inputs (the diff for
// ScopeRecent, the path for ScopePkg).
type Target struct {
	Scope    Scope
	Slug     string // report filename slug: "all", "recent", or the sanitized path
	Date     string // YYYY-MM-DD, stamped into the report filename and header
	Path     string // ScopePkg: the package/dir under audit
	Diff     string // ScopeRecent: the git diff to audit
	DiffBase string // ScopeRecent: the base ref the diff was taken against
}

// Generator drives the claude CLI to run an audit pass and write its report.
// Side-effecting dependencies (claude, git, the clock) are injected so
// orchestration is testable without invoking any of them.
type Generator struct {
	Run      claudeexec.RunFunc  // executes claude; required unless DryRun
	Diff     claudeexec.DiffFunc // computes the "recent" diff; required for ScopeRecent
	Model    string              // claude model alias; defaults to DefaultModel
	DiffBase string              // base ref for the "recent" diff; defaults to DefaultDiffBase
	DryRun   bool                // when true, print the command/prompt instead of running
	Now      func() time.Time    // clock for the report date; defaults to time.Now
	Out      io.Writer           // log / dry-run sink; defaults to os.Stdout
}

func (g *Generator) out() io.Writer {
	if g.Out != nil {
		return g.Out
	}
	return os.Stdout
}

func (g *Generator) model() string {
	if g.Model != "" {
		return g.Model
	}
	return DefaultModel
}

func (g *Generator) diffBase() string {
	if g.DiffBase != "" {
		return g.DiffBase
	}
	return DefaultDiffBase
}

// date returns the report date as YYYY-MM-DD from the injected clock.
func (g *Generator) date() string {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	return now().Format("2006-01-02")
}

// invoke runs (or, in dry-run, prints) a single claude call for the target.
func (g *Generator) invoke(ctx context.Context, t Target) error {
	prompt := AuditPrompt(t)
	args := claudeexec.Args(g.model(), prompt)
	label := fmt.Sprintf("audit %s -> %s", t.Slug, ReportPath(t.Date, t.Slug))
	if g.DryRun {
		fmt.Fprintf(g.out(), "[dry-run] %s\nclaude %s\n--- prompt ---\n%s\n--------------\n",
			label, strings.Join(args[:len(args)-1], " "), prompt)
		return nil
	}
	if g.Run == nil {
		return fmt.Errorf("auditgen: no Run func configured (and not DryRun)")
	}
	fmt.Fprintf(g.out(), "==> %s (claude %s)\n", label, g.model())
	if err := g.Run(ctx, args); err != nil {
		return fmt.Errorf("audit %s: claude run failed: %w", t.Slug, err)
	}
	return nil
}

// All audits the whole repository.
func (g *Generator) All(ctx context.Context) error {
	return g.invoke(ctx, Target{Scope: ScopeAll, Slug: "all", Date: g.date()})
}

// Recent audits the code changes vs the diff base. It is a no-op (not an error)
// when that diff is empty — there is nothing to audit.
func (g *Generator) Recent(ctx context.Context) error {
	if g.Diff == nil {
		return fmt.Errorf("auditgen: no Diff func configured for the recent scope")
	}
	base := g.diffBase()
	diff, err := g.Diff(base)
	if err != nil {
		return fmt.Errorf("audit recent: computing diff against %q: %w", base, err)
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Fprintf(g.out(), "==> audit: recent — no code changes vs %s; skipping\n", base)
		return nil
	}
	return g.invoke(ctx, Target{Scope: ScopeRecent, Slug: "recent", Date: g.date(), Diff: diff, DiffBase: base})
}

// Pkg audits a single package or directory named by its repo-relative path.
func (g *Generator) Pkg(ctx context.Context, path string) error {
	path = strings.TrimSpace(strings.TrimRight(path, "/"))
	if path == "" {
		return fmt.Errorf("auditgen: pkg scope requires a non-empty path")
	}
	return g.invoke(ctx, Target{Scope: ScopePkg, Slug: slugify(path), Date: g.date(), Path: path})
}

// slugify turns a package path into a filename-safe slug:
// pkg/web/webui/chat -> pkg-web-webui-chat. Any run of non-alphanumeric
// characters collapses to a single dash, and leading/trailing dashes are trimmed.
func slugify(path string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
