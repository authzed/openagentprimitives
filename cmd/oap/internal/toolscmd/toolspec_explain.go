package toolscmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/validator"
)

func newToolspecExplainCmd() *cobra.Command {
	var (
		toolkitPath   string
		specPath      string
		binaryVersion string
		envKV         []string
		cwd           string
	)
	cmd := &cobra.Command{
		Use:   "explain -- <argv...>",
		Short: "Run validate and print a human-readable trace",
		RunE: func(cmd *cobra.Command, args []string) error {
			tk, err := toolkit.Load(toolkitPath)
			if err != nil {
				return err
			}
			sp, err := spec.Load(specPath)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return fmt.Errorf("missing argv after --")
			}
			env := map[string]string{}
			for _, kv := range envKV {
				if i := strings.IndexByte(kv, '='); i >= 0 {
					env[kv[:i]] = kv[i+1:]
				}
			}
			inv := validator.Invocation{
				Command:       args[0],
				Argv:          args[1:],
				Env:           env,
				Cwd:           cwd,
				BinaryVersion: binaryVersion,
			}
			d, err := validator.Check(tk, sp, inv)
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Allow: %t\n", d.Allow)
			if d.Reason != "" {
				fmt.Fprintf(&b, "Reason: %s\n", d.Reason)
			}
			if d.FailedOn != nil {
				fmt.Fprintf(&b, "FailedOn: %s — %s\n", d.FailedOn.Path, d.FailedOn.Message)
			}
			fmt.Fprintln(&b, "Trace:")
			for _, tr := range d.Trace {
				fmt.Fprintf(&b, "  [%s] %s", tr.Status, tr.Path)
				if tr.Detail != "" {
					fmt.Fprintf(&b, " — %s", tr.Detail)
				}
				b.WriteByte('\n')
				if tr.Rule != "" {
					fmt.Fprintf(&b, "         rule: %s\n", tr.Rule)
				}
			}
			if !d.Allow && d.Parsed != nil {
				pc := d.Parsed
				fmt.Fprintln(&b, "Parsed call:")
				fmt.Fprintf(&b, "  subcommand: %q\n", pc.Subcommand)
				if len(pc.Flags) > 0 {
					fmt.Fprintf(&b, "  flags:      %v\n", pc.Flags)
				}
				if len(pc.Positional) > 0 {
					fmt.Fprintf(&b, "  positional: %v\n", pc.Positional)
				}
				if len(pc.Tail) > 0 {
					fmt.Fprintf(&b, "  tail:       %v\n", pc.Tail)
				}
				fmt.Fprintf(&b, "  cwd:        %q\n", inv.Cwd)
				fmt.Fprintf(&b, "  env:        %v\n", redactedEnv(inv.Env, d.Redactions))
			} else if !d.Allow {
				fmt.Fprintln(&b, "Parsed call: (parse did not complete)")
				fmt.Fprintf(&b, "  argv: %v\n", inv.Argv)
				fmt.Fprintf(&b, "  cwd:  %q\n", inv.Cwd)
				fmt.Fprintf(&b, "  env:  %v\n", redactedEnv(inv.Env, d.Redactions))
			}
			if len(d.Warnings) > 0 {
				fmt.Fprintln(&b, "Warnings:")
				for _, w := range d.Warnings {
					fmt.Fprintf(&b, "  %s: %s\n", w.Kind, w.Message)
				}
			}
			if len(d.Redactions) > 0 {
				fmt.Fprintln(&b, "Redactions:")
				for id, desc := range d.Redactions {
					fmt.Fprintf(&b, "  id=%s kind=%s name=%s — %s\n", id, desc.Kind, desc.Name, desc.Description)
				}
			}
			// One boundary pass over the whole assembled trace, so a field
			// added later cannot be the one that echoes a secret.
			fmt.Fprint(cmd.OutOrStdout(), redactSensitiveValues(b.String(), inv.Env, d.Redactions))
			return nil
		},
	}
	cmd.Flags().StringVar(&toolkitPath, "toolkit", "", "path to toolkit YAML")
	cmd.Flags().StringVar(&specPath, "spec", "", "path to spec YAML")
	cmd.Flags().StringVar(&binaryVersion, "binary-version", "", "binary semver version")
	cmd.Flags().StringSliceVar(&envKV, "env", nil, "env var KEY=VALUE; repeatable")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory")
	_ = cmd.MarkFlagRequired("toolkit")
	_ = cmd.MarkFlagRequired("spec")
	return cmd
}

// redactSensitiveValues replaces every occurrence of a declared-sensitive
// value in the assembled trace with that value's redaction token.
//
// It exists because the trace does NOT arrive uniformly redacted. The
// validator scrubs Reason, FailedOn, every Trace detail, and the Flags /
// Positional maps on Parsed before the Decision leaves its package — but
// three printed things fall outside that:
//
//   - Parsed.Tail. The scrub callback walks Flags and Positional only, so a
//     post-`--` value is printed verbatim two lines under the SAME value
//     masked in positional. That asymmetry is worse than no redaction: it
//     reads as though the masking worked.
//   - A []string leaf inside Flags / Positional. The recursive scrub handles
//     string, map[string]any and []any and returns anything else untouched,
//     so a stringList slot keeps its raw values.
//   - inv.Argv, printed when the parse did not complete. It is the raw
//     invocation and never passed through the validator's redactor at all.
//
// Wrapping each of those individually is how the next added field gets
// missed, so the whole rendered string goes through one pass instead.
//
// The sensitive values are recovered the same way redactedEnv recovers the
// names: Decision.Redactions carries one descriptor per registered value, and
// an env-kind descriptor names an inv.Env key whose value is the secret.
//
// KNOWN LIMIT — flag-kind descriptors cannot be recovered here. The validator
// masks Parsed.Flags before returning (which is the point), so the raw value
// is gone by the time this runs, and re-deriving it would mean re-parsing the
// argv this command exists to explain. A sensitive FLAG value that reaches a
// field the validator did not scrub therefore still needs fixing on the
// validator side, in pkg/tools/toolspec/validator's finalize.
func redactSensitiveValues(s string, env map[string]string, red map[string]redact.Descriptor) string {
	for id, d := range red {
		if d.Kind != "env" {
			continue
		}
		v, ok := env[d.Name]
		if !ok || v == "" {
			// Never register the empty string: it matches everywhere and would
			// shred the whole trace. Same guard the Redactor itself applies.
			continue
		}
		s = strings.ReplaceAll(s, v, fmt.Sprintf("<redacted id=%q/>", id))
	}
	return s
}

// redactedEnv replaces values matching any Redactions entry with the matching token.
//
// Name-keyed, and kept alongside the value-keyed pass above rather than folded
// into it: this renders the env map as an exact per-key substitution, so an
// env value that happens to be a short common string masks its own entry
// cleanly instead of being substring-replaced across the map's rendering.
func redactedEnv(env map[string]string, red map[string]redact.Descriptor) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		masked := v
		for id, d := range red {
			if d.Name == k {
				masked = fmt.Sprintf("<redacted id=%q/>", id)
				break
			}
		}
		out[k] = masked
	}
	return out
}
