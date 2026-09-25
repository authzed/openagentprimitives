package spec

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// OverridableRulePaths is the closed set of structured-rule paths an exception may name.
var OverridableRulePaths = map[string]bool{
	"deny.effects.destructive":    true,
	"deny.effects.reads":          true,
	"deny.effects.writes":         true,
	"deny.effects.creds.writes":   true,
	"allow.network.destinations":  true,
	"allow.filesystem.pathsUnder": true,
	"allow.creds.required":        true,
}

// NonOverridablePaths documents paths an exception is explicitly forbidden from targeting.
var NonOverridablePaths = map[string]bool{
	"parse":            true,
	"revision":         true,
	"binaryVersion":    true,
	"allowSubcommands": true,
}

// Load reads and validates a spec YAML file at path.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes parses YAML bytes into a Spec and validates the result.
func LoadBytes(data []byte) (*Spec, error) {
	// First pass: decode into a raw map so we can detect "field present but empty"
	// for the allow.* Set markers.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("yaml unmarshal (raw): %w", err)
	}

	var sp Spec
	if err := yaml.Unmarshal(data, &sp); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	// Populate Set markers from raw.
	if allow, ok := raw["allow"].(map[string]interface{}); ok {
		if net, ok := allow["network"].(map[string]interface{}); ok {
			if _, ok := net["destinations"]; ok {
				sp.Allow.Network.Set = true
			}
		}
		if fs, ok := allow["filesystem"].(map[string]interface{}); ok {
			if _, ok := fs["pathsUnder"]; ok {
				sp.Allow.Filesystem.Set = true
			}
		}
		if cr, ok := allow["creds"].(map[string]interface{}); ok {
			if _, ok := cr["required"]; ok {
				sp.Allow.Creds.Set = true
			}
		}
	}

	// Validate before processing allowSubcommands so we catch name/toolkit errors first
	if err := validate(&sp); err != nil {
		return nil, err
	}

	// Distinguish "allowSubcommands missing" from "allowSubcommands: []"
	if _, ok := raw["allowSubcommands"]; !ok {
		return nil, fmt.Errorf("allowSubcommands is required (use [] to deny everything)")
	}
	if sp.AllowSubcommands == nil {
		sp.AllowSubcommands = []string{}
	}

	return &sp, nil
}

func validate(sp *Spec) error {
	if sp.Name == "" {
		return fmt.Errorf("name is required")
	}
	if sp.Version == "" {
		return fmt.Errorf("version is required")
	}
	if sp.Toolkit.Name == "" {
		return fmt.Errorf("toolkit.name is required")
	}
	if sp.Toolkit.Revision == "" {
		return fmt.Errorf("toolkit.revision is required")
	}
	for i, e := range sp.Exceptions {
		if len(e.Overrides) == 0 {
			return fmt.Errorf("exceptions[%d]: overrides must be non-empty", i)
		}
		for j, path := range e.Overrides {
			if NonOverridablePaths[path] {
				return fmt.Errorf("exceptions[%d].overrides[%d]: %q is not overridable", i, j, path)
			}
			if !OverridableRulePaths[path] {
				return fmt.Errorf("exceptions[%d].overrides[%d]: %q is not a valid overridable rule path", i, j, path)
			}
		}
		if e.When == "" {
			return fmt.Errorf("exceptions[%d]: when is required", i)
		}
	}
	for i, c := range sp.Constraints {
		if c.CEL == "" {
			return fmt.Errorf("constraints[%d]: cel is required", i)
		}
	}
	if sp.SecretOutput != nil {
		if strings.TrimSpace(sp.SecretOutput.Name) == "" {
			return fmt.Errorf("secretOutput.name is required")
		}
		src := sp.SecretOutput.Source
		if src != "stdout" && !strings.HasPrefix(src, "file:/") {
			return fmt.Errorf("secretOutput.source must be \"stdout\" or \"file:<absolute-path>\"; got %q", src)
		}
	}
	return nil
}
