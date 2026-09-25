package builtin

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
)

var registry = map[string]parser.Parser{}

// Register adds a named parser to the registry. Panics on duplicate —
// this is startup code (init-time registration) and a duplicate is a build bug.
func Register(name string, p parser.Parser) {
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("builtin parser %q already registered", name))
	}
	registry[name] = p
}

// Get returns the named parser and true if it exists, or nil and false.
func Get(name string) (parser.Parser, bool) {
	p, ok := registry[name]
	return p, ok
}
