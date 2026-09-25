package identityd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// github-token is declared by the gh toolkit with provider: github-pat;
// Task 1 gave github-pat instructions, so this resolves today.
// The anthropic-oauth credential resolves via the claude-oauth toolkit.
func TestCredInstructionsResolvesProvider(t *testing.T) {
	gInstr, gDocs := credInstructions("github-token")
	assert.NotEmpty(t, gInstr, "github-token row must surface PAT-creation guidance")
	assert.NotEmpty(t, gDocs)

	aInstr, aDocs := credInstructions("anthropic-oauth")
	assert.NotEmpty(t, aInstr, "anthropic-oauth must have instructions")
	assert.NotEmpty(t, aDocs, "anthropic-oauth must have docs URL")
}

func TestCredInstructionsUnknownIsEmpty(t *testing.T) {
	instr, docs := credInstructions("does-not-exist")
	assert.Empty(t, instr)
	assert.Empty(t, docs)
}
