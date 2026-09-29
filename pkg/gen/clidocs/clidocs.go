// Package clidocs generates the site's CLI reference: one MDX page per
// top-level `oap` command family, walked deterministically from the live cobra
// command tree. It takes a *cobra.Command so the heavy rendering is unit-testable
// against a synthetic tree; the real oap tree is passed in by a gated test in
// cmd/oap (NewRootCmd lives in package main and can't be imported here).
//
// Output is committed MDX under the docs guides dir; a change to a command's
// Use/Short/flags surfaces as a docs diff, the same discipline as blockcapture.
package clidocs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/authzed/openagentprimitives/pkg/gen/mdxutil"
)

// The CLI reference is the "CLI" group of the docs' Reference section. Orders sit
// after the primitive Reference groups (which end in the 2600s).
const (
	section       = "Reference"
	group         = "CLI"
	overviewOrder = 2700
	familyStep    = 10
	familyBase    = 2710
)

// Generate writes cli-reference.mdx (an overview) plus oap-<family>.mdx for every
// visible top-level family of root, into outDir. Deterministic: families and
// flags are sorted, so re-running with an unchanged tree is a no-op diff.
func Generate(root *cobra.Command, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	fams := visibleSubcommands(root)
	if len(fams) == 0 {
		return fmt.Errorf("clidocs: root %q has no visible subcommands", root.Name())
	}

	if err := os.WriteFile(filepath.Join(outDir, "cli-reference.mdx"), []byte(overviewPage(root, fams)), 0o644); err != nil {
		return err
	}
	for i, fam := range fams {
		page := familyPage(fam, familyBase+i*familyStep)
		name := "oap-" + fam.Name() + ".mdx"
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(page), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// visibleSubcommands returns a command's non-hidden, runnable-or-parent
// subcommands, sorted by name (excludes cobra's auto help/completion).
func visibleSubcommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, s := range c.Commands() {
		if s.Hidden || s.Name() == "help" || s.Name() == "completion" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func overviewPage(root *cobra.Command, fams []*cobra.Command) string {
	var b bytes.Buffer
	b.WriteString(mdxutil.Frontmatter("oap CLI reference", section, group, overviewOrder,
		"Every oap command family, generated from the CLI itself."))
	b.WriteString("# oap CLI reference\n\n")
	b.WriteString("The `oap` CLI installs, manages, and runs agents. Every command below is generated directly\n")
	b.WriteString("from the CLI, so it matches the binary you have. Pick a family for its subcommands and flags.\n\n")
	b.WriteString("## Families\n\n")
	b.WriteString("| Family | What it does |\n| --- | --- |\n")
	for _, f := range fams {
		fmt.Fprintf(&b, "| [`oap %s`](/docs/oap-%s) | %s |\n", f.Name(), f.Name(), mdxutil.TableText(f.Short))
	}
	b.WriteString("\n## Global flags\n\n")
	b.WriteString("These persistent flags apply to every command:\n\n")
	b.WriteString(flagsFence(root.PersistentFlags()))
	return b.String()
}

func familyPage(fam *cobra.Command, order int) string {
	var b bytes.Buffer
	title := "oap " + fam.Name()
	b.WriteString(mdxutil.Frontmatter(title, section, group, order, mdxutil.FirstLine(fam.Short)))
	fmt.Fprintf(&b, "# %s\n\n", title)
	if d := describe(fam); d != "" {
		b.WriteString(d + "\n\n")
	}
	// Every command in the subtree, depth-first, deepest headings capped at h4.
	writeCommandTree(&b, fam, 2)
	return b.String()
}

// writeCommandTree renders each visible subcommand of cmd as a heading section
// (usage, description, flags, example), then recurses. level is the markdown
// heading level for the direct children (2 = ##), capped at 4.
func writeCommandTree(b *bytes.Buffer, cmd *cobra.Command, level int) {
	h := strings.Repeat("#", min(level, 4))
	for _, sub := range visibleSubcommands(cmd) {
		fmt.Fprintf(b, "%s `%s`\n\n", h, useLine(sub))
		if al := sub.Aliases; len(al) > 0 {
			fmt.Fprintf(b, "Aliases: `%s`.\n\n", strings.Join(al, "`, `"))
		}
		if d := describe(sub); d != "" {
			b.WriteString(d + "\n\n")
		}
		if hasLocalFlags(sub) {
			b.WriteString("Flags:\n\n")
			b.WriteString(flagsFence(sub.LocalFlags()))
		}
		if ex := strings.TrimSpace(sub.Example); ex != "" {
			b.WriteString("```bash\n" + ex + "\n```\n\n")
		}
		writeCommandTree(b, sub, level+1)
	}
}

// --- rendering helpers ---

// useLine is the full invocation (command path + args), without cobra's trailing
// " [flags]" placeholder. Backtick-wrapped by callers so its <args> stay literal.
func useLine(c *cobra.Command) string {
	return strings.TrimSuffix(c.UseLine(), " [flags]")
}

// describe returns the command's Long (preferred) or Short as escaped prose.
func describe(c *cobra.Command) string {
	txt := strings.TrimSpace(c.Long)
	if txt == "" {
		txt = strings.TrimSpace(c.Short)
	}
	if txt == "" {
		return ""
	}
	return mdxutil.EscapeMDX(txt)
}

func hasLocalFlags(c *cobra.Command) bool {
	found := false
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			found = true
		}
	})
	return found
}

// flagsFence renders a flag set as a plain code fence — literal, so flag usage
// containing <, >, { or braces needs no escaping.
func flagsFence(fs *pflag.FlagSet) string {
	type row struct{ name, rest string }
	var rows []row
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", " + name
		}
		if t := f.Value.Type(); t != "bool" {
			name += " " + t
		}
		usage := f.Usage
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" && f.DefValue != "0" {
			usage += fmt.Sprintf(" (default %q)", f.DefValue)
		}
		rows = append(rows, row{name, usage})
	})
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	width := 0
	for _, r := range rows {
		if len(r.name) > width {
			width = len(r.name)
		}
	}
	var b strings.Builder
	b.WriteString("```text\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %s\n", width, r.name, r.rest)
	}
	b.WriteString("```\n\n")
	return b.String()
}
