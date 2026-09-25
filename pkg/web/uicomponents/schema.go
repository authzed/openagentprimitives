package uicomponents

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// VocabularySchema returns a {type: propsSchema} map covering every registered
// component, for embedding in update_view's input schema so the agent learns
// the exact closed set it may write — from the registry, never from a
// hand-maintained list.
func VocabularySchema() (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, c := range registry.All() {
		if c.Structural {
			continue // never published: the agent cannot write a hook
		}
		s, err := c.Schema()
		if err != nil {
			return nil, err
		}
		out[c.Type] = s
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("uicomponents: marshal vocabulary schema: %w", err)
	}
	return raw, nil
}
