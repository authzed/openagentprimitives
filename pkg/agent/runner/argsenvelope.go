package runner

// unwrapToolArgs returns the inner args map for tools that wrap their
// input via tool.WrapInputSchema (MCP tools, sandbox tools). Authz CEL
// expressions in MCPServer CRs are written against the INNER args, so
// variant `when` clauses and `resourceIDExpr` need to see that shape —
// not the outer {operation_id, _reason, args} envelope.
//
// The unwrap fires only when the canonical envelope shape is intact
// (operation_id present + args is a map). Meta tools have flat input
// schemas and are passed through unchanged; this also makes the
// behavior safe for hand-rolled tool fixtures used by older tests.
func unwrapToolArgs(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if _, ok := m["operation_id"]; !ok {
		return m
	}
	inner, ok := m["args"].(map[string]any)
	if !ok {
		return m
	}
	return inner
}
