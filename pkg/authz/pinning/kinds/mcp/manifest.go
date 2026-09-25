// Canonical hashing of an MCP server's tools/list manifest. The hash covers
// exactly the prompt-injection / rug-pull surface — (name, description,
// inputSchema) per tool — and is stable across tool order, JSON key order,
// and insignificant whitespace. outputSchema and annotations are deliberately
// NOT covered (spec §1): they don't reach the LLM prompt.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// canonicalTool is the reduced, hash-covered projection of one tool.
type canonicalTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// CanonicalManifestHash returns "sha256:<hex>" over the canonicalized
// manifest. Tools are sorted by name; each inputSchema is round-tripped
// through encoding/json so map keys serialize sorted and whitespace is
// normalized.
func CanonicalManifestHash(tools []probe.Tool) (string, error) {
	canon := make([]canonicalTool, 0, len(tools))
	for _, t := range tools {
		ct := canonicalTool{Name: t.Name, Description: t.Description}
		if len(t.InputSchema) > 0 {
			var schema any
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return "", fmt.Errorf("canonicalize %s inputSchema: %w", t.Name, err)
			}
			ct.InputSchema = schema
		}
		canon = append(canon, ct)
	}
	// Stable + secondary key: duplicate tool names violate the MCP spec, but
	// when a server serves them anyway the hash must not depend on probe
	// order — order-dependent hashing would read as spurious drift.
	sort.SliceStable(canon, func(i, j int) bool {
		if canon[i].Name != canon[j].Name {
			return canon[i].Name < canon[j].Name
		}
		return canon[i].Description < canon[j].Description
	})
	data, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("marshal canonical manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
