package toolkit

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	strictYAML "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// Load reads and validates a toolkit YAML file at path.
func Load(path string) (*Toolkit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes parses YAML bytes into a Toolkit and validates the result.
func LoadBytes(data []byte) (*Toolkit, error) {
	// Duplicate-key pre-pass, BEFORE the real unmarshal.
	//
	// sigs.k8s.io/yaml converts YAML to JSON and keeps the LAST of a duplicated
	// key, silently. A toolkit is an authorization spec — two `resourceIDHint`s,
	// or two `permission` blocks, and the file reads as if it says something it
	// does not, with nothing failing. yaml.v3 rejects duplicates outright, so
	// decoding into a throwaway node is the cheapest way to borrow that
	// behaviour without changing which library builds the struct.
	var probe map[string]any
	if err := strictYAML.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	var tk Toolkit
	if err := yaml.Unmarshal(data, &tk); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if err := validate(&tk); err != nil {
		return nil, err
	}
	return &tk, nil
}

func validate(tk *Toolkit) error {
	if tk.Name == "" {
		return fmt.Errorf("name is required")
	}
	if tk.Version == "" {
		return fmt.Errorf("version is required")
	}
	if tk.ToolkitRevision == "" {
		return fmt.Errorf("toolkitRevision is required")
	}
	if tk.Target.Binary == "" {
		return fmt.Errorf("target.binary is required")
	}
	switch tk.Parser.Kind {
	case "declarative":
		if tk.Parser.Name != "" {
			return fmt.Errorf("parser.name must be empty when parser.kind is declarative")
		}
	case "builtin":
		if tk.Parser.Name == "" {
			return fmt.Errorf("parser.name is required when parser.kind is builtin")
		}
	default:
		return fmt.Errorf(`parser.kind must be "declarative" or "builtin", got %q`, tk.Parser.Kind)
	}
	envNames := map[string]bool{}
	sensitiveEnv := map[string]bool{}
	for _, e := range tk.Env.Allowed {
		if e.Name == "" {
			return fmt.Errorf("env.allowed entry missing name")
		}
		if envNames[e.Name] {
			return fmt.Errorf("duplicate env var %q in env.allowed", e.Name)
		}
		if e.Sensitive && e.Credential == "" {
			return fmt.Errorf("toolkit %q: env %q is sensitive but declares no credential:", tk.Name, e.Name)
		}
		envNames[e.Name] = true
		if e.Sensitive {
			sensitiveEnv[e.Name] = true
		}
	}
	if err := validateEnvDefaults(tk.EnvDefaults, sensitiveEnv); err != nil {
		return fmt.Errorf("toolkit %q: %w", tk.Name, err)
	}
	for i, sc := range tk.Subcommands {
		if err := validateEffects(&sc.Effects, envNames); err != nil {
			return fmt.Errorf("subcommands[%d] (%v): %w", i, sc.Path, err)
		}
		if err := validateFlags(sc.Flags); err != nil {
			return fmt.Errorf("subcommands[%d] (%v) flags: %w", i, sc.Path, err)
		}
		if err := validatePositionals(sc.Positional); err != nil {
			return fmt.Errorf("subcommands[%d] (%v) positional: %w", i, sc.Path, err)
		}
		if err := validateTimeout(sc.Timeout); err != nil {
			return fmt.Errorf("subcommands[%d] (%v): %w", i, sc.Path, err)
		}
		if err := validateConsequentialPermission(tk, &tk.Subcommands[i]); err != nil {
			return fmt.Errorf("toolkit %q subcommands[%d] (%v): %w", tk.Name, i, sc.Path, err)
		}
	}
	if err := validateFlags(tk.GlobalFlags); err != nil {
		return fmt.Errorf("globalFlags: %w", err)
	}
	return nil
}

// posixEnvName is the grammar an environment-variable name must satisfy to be
// exportable by the exec transport. Mirrors pkg/tools/exec/remote.envNameRE and
// pkg/controllers/toolcall.IsValidEnvName; spelled out again here so a bad name
// is caught when the toolkit is authored rather than when a tool call refuses
// to stage its whole environment.
var posixEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvDefaults rejects an envDefaults block the runtime could not honor.
//
// Two classes are refused, both fail-closed:
//
//   - A name outside the POSIX grammar. The remote exec transport errors on the
//     whole request rather than dropping one variable, so every call through
//     this toolkit would fail at dispatch with no hint about which toolkit.
//   - A name that Env.Allowed declares `sensitive`. Those are filled from an
//     AgentIdentity credential at dispatch; a static default for the same key
//     either fails the call outright (ToolCallEnvShadowsAgent) or, where no
//     credential resolves, hands the CLI a fixed string where an operator
//     expects the credential.
//
// Offenders are collected and sorted rather than returned on first hit: Go map
// order would otherwise vary the message between runs of the same input.
func validateEnvDefaults(env map[string]string, sensitive map[string]bool) error {
	var badName, shadowsCred []string
	for k := range env {
		switch {
		case !posixEnvName.MatchString(k):
			badName = append(badName, fmt.Sprintf("%q", k))
		case sensitive[k]:
			shadowsCred = append(shadowsCred, k)
		}
	}
	if len(badName) > 0 {
		sort.Strings(badName)
		return fmt.Errorf("envDefaults key(s) %v are not POSIX environment-variable names", badName)
	}
	if len(shadowsCred) > 0 {
		sort.Strings(shadowsCred)
		return fmt.Errorf("envDefaults key(s) %v are declared sensitive in env.allowed; "+
			"a static default must not shadow a credential-injected value", shadowsCred)
	}
	return nil
}

// validateTimeout fails loudly on a non-parseable or non-positive subcommand
// timeout. Empty is valid (the mode default applies at resolution time).
func validateTimeout(s string) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("timeout %q: %w", s, err)
	}
	if d <= 0 {
		return fmt.Errorf("timeout %q must be positive", s)
	}
	return nil
}

func validateEffects(e *Effects, envNames map[string]bool) error {
	for _, c := range e.Reads {
		if c != "network" && c != "filesystem" {
			return fmt.Errorf("effects.reads: unknown category %q", c)
		}
	}
	for _, c := range e.Writes {
		if c != "network" && c != "filesystem" {
			return fmt.Errorf("effects.writes: unknown category %q", c)
		}
	}
	for _, name := range e.Creds.Required {
		if !envNames[name] {
			return fmt.Errorf("creds.required references %s which is not in env.allowed", name)
		}
	}
	for _, name := range e.Creds.Writes {
		if !envNames[name] {
			return fmt.Errorf("creds.writes references %s which is not in env.allowed", name)
		}
	}
	return nil
}

var validFlagTypes = map[string]bool{
	"bool": true, "string": true, "int": true, "path": true, "url": true, "enum": true, "stringList": true,
}

func validateFlags(flags []Flag) error {
	for _, f := range flags {
		// A flag must be addressable by at least one form. Most flags
		// declare a long name; short-only flags (e.g. git's -C, which
		// has no long form) declare only Short. Both are valid.
		if f.Long == "" && f.Short == "" {
			return fmt.Errorf("flag must declare a long or short name")
		}
		// Label the flag for error messages by whichever name it has.
		label := f.Long
		if label == "" {
			label = "-" + f.Short
		}
		if !validFlagTypes[f.Type] {
			return fmt.Errorf("flag %q: unknown type %q", label, f.Type)
		}
		if f.Type == "enum" && len(f.Values) == 0 {
			return fmt.Errorf("flag %q: enum type requires values", label)
		}
		// A bool carries no value in any form, so "the value is optional" is
		// meaningless on it — and silently accepting the combination would hide
		// a mis-declared flag type from the author.
		if f.OptionalValue && f.Type == "bool" {
			return fmt.Errorf("flag %q: optionalValue is invalid on type bool", label)
		}
	}
	return nil
}

// validatePositionals enforces the single-AfterDashDash rule: the post-`--`
// tail binds starting at one slot, so two candidate slots is ambiguous and the
// parser would have to pick arbitrarily.
func validatePositionals(ps []Positional) error {
	marked := ""
	for _, p := range ps {
		if !p.AfterDashDash {
			continue
		}
		if marked != "" {
			return fmt.Errorf("positionals %q and %q both set afterDashDash; at most one may", marked, p.Name)
		}
		marked = p.Name
	}
	return nil
}
