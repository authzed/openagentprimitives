package apiadapter

// InputSchema derives the MCP tool input schema for this operation — the raw
// JSON-Schema map the go-sdk's mcp.Tool.InputSchema field takes, so a
// config-driven tool advertises exactly the parameters the config declares.
func (o Operation) InputSchema() map[string]any {
	props := map[string]any{}
	var required []any
	for _, p := range o.Params {
		prop := map[string]any{"type": p.Type}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
