// Package toolkits embeds the canonical CLI toolkit YAMLs that ship
// with the operator binary. The repo-root /toolkits directory is the
// single source of truth — both the embedded set (this package) and
// any tool that wants to load toolkits from disk read from the same
// location.
//
// Built-ins always exist; users may add SpiceboxToolkit CRs to extend
// with new (name, revision) pairs. A SpiceboxToolkit with the same
// (name, revision) as a built-in is rejected by admission — collisions
// are a misconfiguration.
package toolkits

import (
	"embed"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

//go:embed *.yaml
var fs embed.FS

var (
	once sync.Once
	tks  []toolkit.Toolkit
	raw  map[string][]byte
	err  error
)

// All returns the parsed list of compile-time toolkits. Parses once on
// first call and caches the result. Panics on any parse error — built-ins
// are authored with the operator binary and parse failures are build bugs.
func All() []toolkit.Toolkit {
	once.Do(loadAll)
	if err != nil {
		panic(fmt.Sprintf("built-in toolkits failed to load: %v", err))
	}
	return tks
}

// RawYAML returns the embedded YAML bytes for the named toolkit, or
// (nil, false) if no built-in by that name is registered. Used by the
// catalog wrapper to back FullYAML for built-in toolkits without
// re-marshaling.
func RawYAML(name string) ([]byte, bool) {
	once.Do(loadAll)
	if err != nil {
		panic(fmt.Sprintf("built-in toolkits failed to load: %v", err))
	}
	b, ok := raw[name]
	return b, ok
}

func loadAll() {
	raw = map[string][]byte{}
	entries, derr := fs.ReadDir(".")
	if derr != nil {
		err = derr
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		data, rerr := fs.ReadFile(ent.Name())
		if rerr != nil {
			err = fmt.Errorf("read %s: %w", ent.Name(), rerr)
			return
		}
		tk, perr := toolkit.LoadBytes(data)
		if perr != nil {
			err = fmt.Errorf("parse %s: %w", ent.Name(), perr)
			return
		}
		tks = append(tks, *tk)
		raw[tk.Name] = data
	}
}
