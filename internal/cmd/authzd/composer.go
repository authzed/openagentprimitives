package main

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

// ComposerInput is curated by authzd from the deterministic
// classification + tool/type descriptions. NO user text — clean
// prompt-injection boundary.
type ComposerInput struct {
	Applied                scope.ScopeDelta
	Skipped                []scope.SkippedItem
	Caveats                []scope.CaveatItem
	ToolDescriptions       map[string]string
	ResourceTypeHumanNames map[string]string
}

// ComposerOutput contains LLM-composed prose for the approval block.
type ComposerOutput struct {
	ApproverSummary     string
	SkippedExplanations []string
	CaveatExplanations  []string
}

// Composer renders user-facing prose from structured classification. It MUST
// NOT see raw user text — that is the prompt-injection boundary, enforced by the
// Extractor/Composer split.
type Composer interface {
	Compose(ctx context.Context, in ComposerInput) (ComposerOutput, error)
}
