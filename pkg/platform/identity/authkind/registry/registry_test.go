package registry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/registry"
)

// stubKind is a minimal Kind impl for registry tests.
type stubKind struct{ prefix string }

func (s stubKind) Prefix() string { return s.prefix }
func (stubKind) ResolveTarget(context.Context, client.Client, string, string) (authkind.Target, error) {
	return nil, authkind.ErrTargetNotFound
}
func (stubKind) SetupRequirements(context.Context, authkind.Target) []authkind.CredentialRequirement {
	return nil
}

func TestRegisterAndByPrefix(t *testing.T) {
	registry.Reset()
	registry.Register(stubKind{prefix: "cli"})
	got, ok := registry.ByPrefix("cli")
	require.True(t, ok, "ByPrefix(cli) must hit")
	assert.Equal(t, "cli", got.Prefix(), "Prefix")
	_, ok = registry.ByPrefix("nope")
	assert.False(t, ok, "ByPrefix(nope) should miss")
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	registry.Reset()
	registry.Register(stubKind{prefix: "cli"})
	assert.Panics(t, func() {
		registry.Register(stubKind{prefix: "cli"})
	}, "duplicate prefix must panic")
}

// TestParseBindingMatch covers the parser's happy paths (registered
// prefixes) and the four malformed/unknown rejection paths. Bundling
// them in one table makes the matrix easy to extend.
func TestParseBindingMatch(t *testing.T) {
	registry.Reset()
	registry.Register(stubKind{prefix: "cli"})
	registry.Register(stubKind{prefix: "mcp"})

	cases := []struct {
		name     string
		in       string
		wantKind string
		wantSuf  string
		wantErr  bool
	}{
		{"cli:gh: valid registered prefix", "cli:gh", "cli", "gh", false},
		{"mcp:linear-readonly: valid registered prefix", "mcp:linear-readonly", "mcp", "linear-readonly", false},
		{"toolspec:foo: unregistered prefix errors", "toolspec:foo", "", "", true},
		{"no-colon: missing colon errors", "no-colon", "", "", true},
		{":empty-prefix: errors", ":empty-prefix", "", "", true},
		{"cli:: empty suffix errors", "cli:", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, suf, err := registry.ParseBindingMatch(tc.in)
			if tc.wantErr {
				require.Error(t, err, "expected error for %q", tc.in)
				return
			}
			require.NoError(t, err, "unexpected error for %q", tc.in)
			assert.Equal(t, tc.wantKind, k.Prefix(), "Prefix")
			assert.Equal(t, tc.wantSuf, suf, "Suffix")
		})
	}
}

func TestErrTargetNotFoundIsExported(t *testing.T) {
	wrapped := fmt.Errorf("context: %w", authkind.ErrTargetNotFound)
	assert.True(t, errors.Is(wrapped, authkind.ErrTargetNotFound), "ErrTargetNotFound must survive errors.Is unwrapping")
}

func TestParseBindingMatchUnknownPrefixIsSentinel(t *testing.T) {
	registry.Reset()
	registry.Register(stubKind{prefix: "cli"})

	// Unknown prefix must wrap ErrUnknownPrefix.
	_, _, err := registry.ParseBindingMatch("toolspec:foo")
	assert.True(t, errors.Is(err, registry.ErrUnknownPrefix), "unknown prefix wraps ErrUnknownPrefix")

	// Malformed inputs must NOT wrap ErrUnknownPrefix — callers rely on the
	// distinction to give different diagnostics for "you typed it wrong"
	// vs "this prefix doesn't exist".
	for _, in := range []string{"no-colon", ":empty-prefix", "cli:"} {
		_, _, err := registry.ParseBindingMatch(in)
		assert.Falsef(t, errors.Is(err, registry.ErrUnknownPrefix), "malformed %q should not wrap ErrUnknownPrefix", in)
	}
}

func TestAllIsSortedAndStable(t *testing.T) {
	registry.Reset()
	// Register in non-alphabetical order.
	registry.Register(stubKind{prefix: "mcp"})
	registry.Register(stubKind{prefix: "cli"})
	registry.Register(stubKind{prefix: "toolspec"})

	got := registry.All()
	want := []string{"cli", "mcp", "toolspec"}
	require.Len(t, got, len(want), "All() length")
	for i, p := range want {
		assert.Equalf(t, p, got[i].Prefix(), "All()[%d]", i)
	}
	// Snapshot isolation: registering after All() must not mutate the
	// returned slice.
	registry.Register(stubKind{prefix: "zzz"})
	assert.Len(t, got, 3, "All() snapshot must not mutate after subsequent Register")
}
