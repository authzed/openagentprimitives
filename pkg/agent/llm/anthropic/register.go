package anthropic

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
)

func init() {
	providers.Register("anthropic", func(apiKey string) (llm.Provider, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("anthropic: apiKey must be non-empty")
		}
		return New(apiKey), nil
	})
}
