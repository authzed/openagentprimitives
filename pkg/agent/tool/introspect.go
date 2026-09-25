package tool

// Introspectable is an optional interface a Tool may implement to expose
// its full, effective capability contract (every allowed subcommand /
// field, flags, positionals, constraint justifications) on demand. The
// introspect_tool meta-tool type-asserts against this — it never
// switches on Tool.Kind(). Tools that don't implement it (meta tools)
// are already fully described by Description() + InputSchema().
type Introspectable interface {
	// Introspect returns a plain-language, LLM-facing rendering of the
	// tool's full contract. Markdown. Pure — no network, no execution.
	Introspect() (string, error)
}
