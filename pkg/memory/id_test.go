package memory_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// fakeKind is a minimal Kind used across tests in this package. Lives
// in id_test.go so subsequent test files (registry_test.go,
// memory_test.go) can reuse it — all three are in package memory_test.
type fakeKind struct {
	name, prefix string
}

func (k fakeKind) Name() string              { return k.name }
func (k fakeKind) IDPrefix() string          { return k.prefix }
func (fakeKind) Retention() memory.Retention { return memory.Retention{} }
func (fakeKind) ContentSchema() reflect.Type { return nil }
func (fakeKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (fakeKind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (fakeKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return noopHooks{} }

type noopHooks struct{}

func (noopHooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func TestNewID_PrefixesWithKind(t *testing.T) {
	k := fakeKind{name: "test", prefix: "tk-"}
	id1 := memory.NewID(k)
	id2 := memory.NewID(k)

	require.True(t, strings.HasPrefix(id1, "tk-"), "got %q", id1)
	require.True(t, strings.HasPrefix(id2, "tk-"), "got %q", id2)
	assert.NotEqual(t, id1, id2, "two NewID calls returned the same id")
	assert.Equal(t, 3+16, len(id1), "expected prefix(3 chars) + 8 random bytes hex (16 chars)")
}
