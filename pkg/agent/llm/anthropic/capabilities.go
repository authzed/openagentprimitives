package anthropic

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

// Capabilities implements llm.Provider — provider-owned capability data,
// sourced from the shared canonical table (pkg/agent/llm/models) so
// consumers never need a second source of truth. Unknown model ids yield
// the zero-value ModelInfo, whose Capabilities is a nil CapabilitySet
// (Has returns false for every capability, never panics).
func (*Provider) Capabilities(model string) llm.CapabilitySet {
	return models.Anthropic[model].Capabilities
}

// NativeInputMIMEs implements llm.Provider — from the shared canonical table.
// Unknown model ids yield the zero-value MIMESet (Has returns false for
// every MIME, never panics).
func (*Provider) NativeInputMIMEs(model string) llm.MIMESet {
	return models.Anthropic[model].NativeInputMIMEs
}
