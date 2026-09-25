package claude

import "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"

// Factory builds a fresh Claude stream-json Parser per session.
type Factory struct{}

// Kind matches Toolkit.StreamFormat = "claude-stream-json".
func (Factory) Kind() string { return "claude-stream-json" }

// New returns a fresh parser.
func (Factory) New() toolkitstream.Parser { return newParser() }
