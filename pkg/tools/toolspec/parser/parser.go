package parser

import "github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"

// Parser parses argv against a toolkit into a Call.
type Parser interface {
	Parse(tk *toolkit.Toolkit, argv []string) (*Call, error)
}

// Call is the parsed invocation. It's also the top-level CEL variable.
type Call struct {
	Subcommand     string         // space-joined path, e.g. "pr view"
	SubcommandPath []string       // e.g. ["pr", "view"]
	Flags          map[string]any // keyed by long name
	Positional     map[string]any // keyed by name
	Tail           []string       // raw argv after `--`
	Argv           []string       // raw input argv
}
