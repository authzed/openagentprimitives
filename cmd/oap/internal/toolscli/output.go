// Package toolscli holds the CLI-side helpers shared across the
// kind-agnostic `oap tools` verbs: table/detail rendering, in-cluster name
// resolution, and the multi-doc YAML walker for apply.
package toolscli

import (
	"fmt"
	"io"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

// Every renderer here takes the theme rather than resolving one itself: the
// capabilities belong to the stream the *command* writes to, and only the
// command knows whether --no-color was passed. A helper that built its own
// theme would have to guess at both.

// RenderRows prints an `oap tools list`-style table to w. When rows is empty
// the function writes a friendly "no tools" line and returns.
func RenderRows(w io.Writer, th *tui.Theme, rows []contract.Row) error {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no tools")
		return nil
	}
	t := tui.NewTable(th, "KIND", "NAMESPACE", "NAME", "STATUS", "SUMMARY")
	for _, r := range rows {
		ns := r.Namespace
		if ns == "" {
			ns = "-"
		}
		t.Row(r.Kind, ns, r.Name, r.Status, r.Summary)
	}
	fmt.Fprint(w, t.Render())
	return nil
}

// RenderDetail prints an `oap tools get`-style detail block to w.
func RenderDetail(w io.Writer, th *tui.Theme, d contract.Detail) error {
	header := fmt.Sprintf("%s/%s", d.Kind, d.Name)
	if d.Namespace != "" {
		header = fmt.Sprintf("%s in namespace %s", header, d.Namespace)
	}
	fmt.Fprintln(w, th.Render(th.Title, header))
	fmt.Fprintln(w)
	renderSections(w, th, d.Sections)
	return nil
}

// RenderDiagnostics prints validate/lint findings. Returns true if any
// Severity == "error" was rendered (callers exit non-zero in that case).
func RenderDiagnostics(w io.Writer, th *tui.Theme, diags []contract.Diagnostic) bool {
	if len(diags) == 0 {
		fmt.Fprintln(w, "OK (no diagnostics)")
		return false
	}
	anyErr := false
	for _, d := range diags {
		sev := d.Severity
		switch sev {
		case "error":
			anyErr = true
			sev = th.Render(th.Err, "ERROR")
		case "warning":
			sev = th.Render(th.Warn, "WARN")
		default:
			sev = strings.ToUpper(sev)
		}
		path := d.Path
		if path == "" {
			path = "-"
		}
		fmt.Fprintf(w, "%s %s: %s\n", sev, path, d.Message)
	}
	return anyErr
}

// RenderProbeResult prints a kind's probe output. Title is rendered above the
// sections; sections look just like Detail sections.
func RenderProbeResult(w io.Writer, th *tui.Theme, r contract.ProbeResult) error {
	if r.Title != "" {
		fmt.Fprintln(w, th.Render(th.Title, r.Title))
		fmt.Fprintln(w)
	}
	renderSections(w, th, r.Sections)
	return nil
}

// renderSections prints the titled, indented body blocks a Detail and a
// ProbeResult both present. An empty body still gets a line, so a section that
// exists but has nothing in it reads as such rather than vanishing.
func renderSections(w io.Writer, th *tui.Theme, sections []contract.Section) {
	for _, s := range sections {
		fmt.Fprintln(w, th.Render(th.Title, s.Title)+":")
		body := strings.TrimRight(s.Body, "\n")
		if body == "" {
			body = "(empty)"
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintln(w, "  "+line)
		}
		fmt.Fprintln(w)
	}
}
