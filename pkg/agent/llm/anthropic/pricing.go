package anthropic

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
)

// Pricing implements llm.Provider — provider-owned pricing, sourced from the
// shared canonical table (pkg/x/llmpricing) so the runner cost estimate and the
// admin dashboard cannot drift. Unknown model ⇒ ok=false (no fabrication).
func (*Provider) Pricing(model string) (llm.ModelPricing, bool) {
	p, ok := llmpricing.Anthropic[model]
	return p, ok
}
