package install

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conflict fixtures shared by the table below. Fake names only — never a
// name borrowed from the demo bundles under examples.
var (
	cfgMapConflict = Conflict{Kind: "ConfigMap", Namespace: "demo", Name: "prompt"}
	agentConflict  = Conflict{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"}
	secretConflict = Conflict{Kind: "Secret", Namespace: "demo", Name: "demo-token", Secret: true}
	toolkitShared  = Conflict{Kind: "SpiceboxToolkit", Name: "shared-toolkit", ClusterScoped: true}
)

func TestResolveConflicts(t *testing.T) {
	// hookReturning builds an AdoptDecision that records that it ran and
	// returns keys; called reports whether the hook was invoked.
	hookReturning := func(called *bool, keys ...string) func(context.Context, []Conflict) ([]string, error) {
		return func(context.Context, []Conflict) ([]string, error) {
			*called = true
			return keys, nil
		}
	}

	t.Run("no conflicts: no error, nothing adopted", func(t *testing.T) {
		got, err := resolveConflicts(context.Background(), nil, InstallOpts{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("cluster-scoped conflict: refused even with AdoptAll, keeps the clusterDeps hint", func(t *testing.T) {
		_, err := resolveConflicts(context.Background(), []Conflict{toolkitShared}, InstallOpts{AdoptAll: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to overwrite shared infra")
		assert.Contains(t, err.Error(), "requires.clusterDeps")
		assert.Contains(t, err.Error(), "SpiceboxToolkit/shared-toolkit")
	})

	t.Run("no flags and no hook: ConflictError listing every conflict", func(t *testing.T) {
		_, err := resolveConflicts(context.Background(), []Conflict{cfgMapConflict, agentConflict}, InstallOpts{})
		var ce *ConflictError
		require.ErrorAs(t, err, &ce)
		assert.Len(t, ce.Conflicts, 2)
		assert.Contains(t, err.Error(), "refusing to overwrite")
		assert.Contains(t, err.Error(), "ConfigMap demo/prompt")
		assert.Contains(t, err.Error(), "AgentClass demo/demo-agent")
	})

	t.Run("AdoptAll: non-Secret adopted, Secret still refused", func(t *testing.T) {
		_, err := resolveConflicts(context.Background(), []Conflict{agentConflict, secretConflict}, InstallOpts{AdoptAll: true})
		var ce *ConflictError
		require.ErrorAs(t, err, &ce)
		require.Len(t, ce.Conflicts, 1, "only the Secret remains unadopted")
		assert.True(t, ce.Conflicts[0].Secret)
		// The error itself names no remediation flag (see ConflictError's doc
		// comment — that text is now a CLI-only concern, rendered by cmd/oap's
		// wrapConflictError from ce.Conflicts, not by this Error() string). It
		// still names the object and the consequence of adopting it.
		assert.Contains(t, err.Error(), "Secret demo/demo-token")
		assert.NotContains(t, err.Error(), "--adopt", "the library error must not prescribe a CLI flag — see ConflictError's doc comment")
	})

	// Naming a Secret is the individual act adopting one requires — but only
	// from a surface that can actually make it one. AdoptSecretsAllowed is the
	// caller saying so; conflict_secret_adopt_test.go covers the surface that
	// cannot (an HTTP field in the same POST body as everything else).
	t.Run("Secret named explicitly from a surface that may adopt one: adopted", func(t *testing.T) {
		got, err := resolveConflicts(context.Background(), []Conflict{secretConflict}, InstallOpts{
			Adopt:               []string{"Secret/demo-token"},
			AdoptSecretsAllowed: true,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"Secret/demo-token"}, got)
	})

	t.Run("--adopt key naming no conflict: hard error, never a silent no-op", func(t *testing.T) {
		_, err := resolveConflicts(context.Background(), []Conflict{agentConflict}, InstallOpts{Adopt: []string{"AgentClas/demo-agent"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AgentClas/demo-agent")
		assert.Contains(t, err.Error(), "not a conflicting object")
	})

	t.Run("Adopt set: hook is not called, remaining conflict still errors", func(t *testing.T) {
		called := false
		_, err := resolveConflicts(context.Background(), []Conflict{agentConflict, cfgMapConflict}, InstallOpts{
			Adopt:         []string{"AgentClass/demo-agent"},
			AdoptDecision: hookReturning(&called, "ConfigMap/prompt"),
		})
		require.Error(t, err, "flags are authoritative — the unnamed ConfigMap is not adopted")
		assert.False(t, called, "an explicit --adopt must suppress the prompt")
	})

	t.Run("AdoptAll set with only Secret conflicts: hook is not called", func(t *testing.T) {
		called := false
		_, err := resolveConflicts(context.Background(), []Conflict{secretConflict}, InstallOpts{
			AdoptAll:      true,
			AdoptDecision: hookReturning(&called, "Secret/demo-token"),
		})
		require.Error(t, err)
		assert.False(t, called, "AdoptAll is authoritative even when it adopts nothing")
	})

	t.Run("hook selection honored: adopted keys returned sorted", func(t *testing.T) {
		called := false
		got, err := resolveConflicts(context.Background(), []Conflict{agentConflict, cfgMapConflict}, InstallOpts{
			AdoptDecision: hookReturning(&called, "ConfigMap/prompt", "AgentClass/demo-agent"),
		})
		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, []string{"AgentClass/demo-agent", "ConfigMap/prompt"}, got)
	})

	t.Run("hook returns a key that is not a conflict: hard error", func(t *testing.T) {
		called := false
		_, err := resolveConflicts(context.Background(), []Conflict{agentConflict}, InstallOpts{
			AdoptDecision: hookReturning(&called, "Secret/anything"),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Secret/anything")
	})

	t.Run("hook error aborts the install", func(t *testing.T) {
		sentinel := errors.New("user aborted")
		_, err := resolveConflicts(context.Background(), []Conflict{agentConflict}, InstallOpts{
			AdoptDecision: func(context.Context, []Conflict) ([]string, error) { return nil, sentinel },
		})
		require.ErrorIs(t, err, sentinel)
	})
}
