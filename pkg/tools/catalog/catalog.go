// Package catalog loads a directory of toolkit YAML files and exposes them
// both as in-memory Toolkit structs and as compact summaries for LLM prompts.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// Type aliases to maintain backward compatibility with existing code.
type ToolkitSummary = llm.ToolkitSummary
type SubcommandSummary = llm.SubcommandSummary

// Catalog is a set of toolkits loaded from disk plus any load-time warnings.
type Catalog struct {
	Toolkits []*toolkit.Toolkit
	Warnings []string
	// rawYAML holds the original file bytes keyed by toolkit name, for FullYAML.
	rawYAML map[string][]byte
}

// LoadDir walks dir (non-recursively) for *.yaml / *.yml files, loads each
// with toolkit.LoadBytes, and returns a Catalog. Malformed files produce a
// warning and are skipped (one bad file must not blow the whole load).
//
// A missing directory is a hard error. An empty directory returns an empty
// Catalog with no error.
func LoadDir(dir string) (*Catalog, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", dir, err)
	}

	cat := &Catalog{rawYAML: map[string][]byte{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			cat.Warnings = append(cat.Warnings, fmt.Sprintf("read %s: %v", path, err))
			continue
		}
		tk, err := toolkit.LoadBytes(data)
		if err != nil {
			cat.Warnings = append(cat.Warnings, fmt.Sprintf("load %s: %v", path, err))
			continue
		}
		cat.Toolkits = append(cat.Toolkits, tk)
		cat.rawYAML[tk.Name] = data
	}
	return cat, nil
}

// FullYAML returns the raw YAML bytes for the named toolkit, for splicing into
// a generation prompt. Errors if the toolkit was not loaded.
func (c *Catalog) FullYAML(name string) ([]byte, error) {
	b, ok := c.rawYAML[name]
	if !ok {
		return nil, fmt.Errorf("toolkit %q not in catalog", name)
	}
	return b, nil
}

// LoadBuiltin returns a Catalog backed by the toolkit YAMLs embedded in
// the binary at compile time (pkg/tools/toolspec/toolkit/builtin). No
// production caller today — `oap tools toolspec describe`/`test` load a
// user-supplied directory via LoadDir instead — so this is exercised only
// by its own tests.
func LoadBuiltin() *Catalog {
	cat := &Catalog{rawYAML: map[string][]byte{}}
	for i := range toolkits.All() {
		tk := toolkits.All()[i]
		// Take the address of the slice element rather than &tk so the
		// returned slice doesn't all alias the loop variable.
		tkCopy := tk
		cat.Toolkits = append(cat.Toolkits, &tkCopy)
		if data, ok := toolkits.RawYAML(tk.Name); ok {
			cat.rawYAML[tk.Name] = data
		}
	}
	return cat
}

// LoadBuiltinPlusDir loads built-in toolkits and overlays any extra
// toolkits found in dir on top. Toolkits in dir with the same name as
// a built-in win (caller can patch a built-in by dropping a file with
// the same `name:` in dir). A missing or empty dir is non-fatal — the
// built-in set is returned as-is. No production caller today — the
// current `oap tools toolspec describe`/`test` path loads a directory
// directly via LoadDir, without the built-in overlay — so this is
// exercised only by its own tests.
func LoadBuiltinPlusDir(dir string) (*Catalog, error) {
	cat := LoadBuiltin()
	if dir == "" {
		return cat, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return cat, nil
		}
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	disk, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	cat.Warnings = append(cat.Warnings, disk.Warnings...)
	// Index built-ins by name so disk overlays replace rather than dupe.
	idx := map[string]int{}
	for i, tk := range cat.Toolkits {
		idx[tk.Name] = i
	}
	for _, tk := range disk.Toolkits {
		if i, ok := idx[tk.Name]; ok {
			cat.Toolkits[i] = tk
		} else {
			cat.Toolkits = append(cat.Toolkits, tk)
			idx[tk.Name] = len(cat.Toolkits) - 1
		}
		if data, ok := disk.rawYAML[tk.Name]; ok {
			cat.rawYAML[tk.Name] = data
		}
	}
	return cat, nil
}

// Summarize returns a compact per-toolkit summary suitable for a Phase-1 prompt.
func (c *Catalog) Summarize() []llm.ToolkitSummary {
	out := make([]llm.ToolkitSummary, 0, len(c.Toolkits))
	for _, tk := range c.Toolkits {
		s := llm.ToolkitSummary{
			Name:   tk.Name,
			Binary: tk.Target.Binary,
		}
		for _, sc := range tk.Subcommands {
			s.Subcommands = append(s.Subcommands, llm.SubcommandSummary{
				Path:                append([]string(nil), sc.Path...),
				Description:         sc.Description,
				Destructive:         sc.Effects.Destructive,
				Reads:               append([]string(nil), sc.Effects.Reads...),
				Writes:              append([]string(nil), sc.Effects.Writes...),
				NetworkDestinations: append([]string(nil), sc.Effects.Network.Destinations...),
			})
		}
		out = append(out, s)
	}
	return out
}
