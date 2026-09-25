// Extractor and Composer are the two seams of the two-call LLM pipeline.
// AnthropicExtractor / the Anthropic composer are the production
// implementations; tests inject fakes.
package main

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

// ExtractorInput is the structured input to the LLM extractor call.
// userRequest is the only untrusted field; everything else is authzd-
// curated.
type ExtractorInput struct {
	UserRequest          string
	Requester            string
	AgentClassEnvelope   scope.AgentClassEnvelope
	CurrentScope         scope.Scope
	CurrentDisallows     []scope.ResourceRef
	CurrentBindings      []scope.ResourceRef
	RequesterAccessHints scope.RequesterPerms
}

// Extractor turns a user mention into a ProposedDelta (structured;
// no NL output). Liquid-input boundary: this is the only LLM call
// that sees raw user text.
type Extractor interface {
	Extract(ctx context.Context, in ExtractorInput) (scope.ScopeDelta, error)
}
