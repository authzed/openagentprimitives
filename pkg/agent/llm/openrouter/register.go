package openrouter

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
)

func init() {
	providers.Register("openrouter", func(apiKey string) (llm.Provider, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("openrouter: apiKey must be non-empty")
		}
		return New(apiKey), nil
	})
}
