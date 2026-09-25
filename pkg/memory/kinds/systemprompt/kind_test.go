package systemprompt_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
)

func scope() memory.Scope { return memory.Scope{Kind: "session", ID: "ns/demo-agent-1"} }

func TestKind_Metadata(t *testing.T) {
	k := systemprompt.Kind{}
	assert.Equal(t, "system_prompt", k.Name())
	assert.True(t, k.Retention().AppendOnly,
		"the instructions an agent ran under are evidence; a rewritten prompt would erase why it acted")
	assert.Contains(t, k.IndexedFields(), "digest")
}

// The log recorded which MODEL was asked but never what it was ASKED. A chain
// proving the agent cloned a repo cannot distinguish a prompt regression from a
// model regression, and repoInstructions / skills / artifact kinds all compose
// INTO this text — so an injection that changed behaviour left no trace in an
// otherwise tamper-evident record.
func TestRecord_storesThePromptAndItsDigest(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	const prompt = "You are a fixture agent.\nYou MUST declare phases with update_plan."
	require.NoError(t, systemprompt.Record(ctx, mem, scope(), prompt))

	got, err := systemprompt.List(ctx, mem, scope())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, prompt, got[0].Prompt, "the full text is what an auditor needs")

	sum := sha256.Sum256([]byte(prompt))
	assert.Equal(t, hex.EncodeToString(sum[:]), got[0].Digest,
		"a plain sha256 of the text, so 'is what I shipped what is running' is checkable offline")
}

// Set-once per digest. The prompt is constant across a session's turns, so a
// per-turn write would be pure bloat on every turn of every session.
func TestRecord_theSamePromptTwiceIsOneEntry(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	const prompt = "identical every turn"
	require.NoError(t, systemprompt.Record(ctx, mem, scope(), prompt))
	require.NoError(t, systemprompt.Record(ctx, mem, scope(), prompt),
		"re-recording an unchanged prompt must not error — it is the common case, once per turn")

	got, err := systemprompt.List(ctx, mem, scope())
	require.NoError(t, err)
	assert.Len(t, got, 1, "one entry per DISTINCT prompt, not per call")
}

// ...but a prompt that CHANGES mid-session is exactly what this has to catch:
// repo instructions and skills are session state and can move under the agent.
func TestRecord_aChangedPromptIsANewEntry(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, systemprompt.Record(ctx, mem, scope(), "before"))
	require.NoError(t, systemprompt.Record(ctx, mem, scope(), "after"))

	got, err := systemprompt.List(ctx, mem, scope())
	require.NoError(t, err)
	require.Len(t, got, 2, "a mid-session change must be visible, not overwrite the original")

	digests := map[string]bool{got[0].Digest: true, got[1].Digest: true}
	assert.Len(t, digests, 2, "two distinct prompts must not collapse to one digest")
}

// An empty prompt is a bug worth seeing rather than a row worth writing: it
// means compose produced nothing, and a digest of "" would look like real
// evidence that the agent ran under empty instructions.
func TestRecord_refusesAnEmptyPrompt(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	assert.Error(t, systemprompt.Record(ctx, mem, scope(), ""))

	got, err := systemprompt.List(ctx, mem, scope())
	require.NoError(t, err)
	assert.Empty(t, got)
}
