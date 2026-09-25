package spec

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads and validates a spec YAML file at path.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes parses YAML or JSON bytes into a Spec and runs structural
// validation. It does NOT compile CEL — call Compile separately.
func LoadBytes(data []byte) (*Spec, error) {
	var sp Spec
	if err := yaml.Unmarshal(data, &sp); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if err := validate(&sp); err != nil {
		return nil, err
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
	if sp.Server.URL == "" {
		return fmt.Errorf("server.url is required")
	}
	if sp.Server.Transport == "" {
		return fmt.Errorf("server.transport is required")
	}
	if sp.Server.Transport != TransportStreamableHTTP {
		return fmt.Errorf("server.transport must be %q (got %q)", TransportStreamableHTTP, sp.Server.Transport)
	}
	if len(sp.Tools) == 0 {
		return fmt.Errorf("tools must be non-empty")
	}
	seen := map[string]int{}
	for i, t := range sp.Tools {
		if t.Name == "" {
			return fmt.Errorf("tools[%d].name is required", i)
		}
		if prev, ok := seen[t.Name]; ok {
			return fmt.Errorf("tools[%d].name %q duplicates tools[%d].name", i, t.Name, prev)
		}
		seen[t.Name] = i
		for j, c := range t.Args.Constraints {
			if c.CEL == "" {
				return fmt.Errorf("tools[%d].args.constraints[%d].cel is required", i, j)
			}
		}
	}
	return nil
}
