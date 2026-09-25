package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

type fakeCap struct {
	name    string
	def     bool
	infra   bool
	parseFn func(json.RawMessage) (Config, error)
}

func (f *fakeCap) Name() string          { return f.name }
func (f *fakeCap) DefaultOn() bool       { return f.def }
func (f *fakeCap) Infrastructural() bool { return f.infra }
func (f *fakeCap) ParseConfig(raw json.RawMessage) (Config, error) {
	if f.parseFn != nil {
		return f.parseFn(raw)
	}
	return nil, nil
}
func (f *fakeCap) Offer(OfferContext) ([]tool.Tool, *SkipReason) { return nil, nil }

func TestRegisterAndLookup(t *testing.T) {
	resetRegistryForTest(t)
	c := &fakeCap{name: "alpha"}
	Register(c)
	got, ok := Lookup("alpha")
	require.True(t, ok, "alpha must be found after Register")
	assert.Equal(t, "alpha", got.Name())
	_, ok = Lookup("missing")
	assert.False(t, ok, "unknown name must not be found")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetRegistryForTest(t)
	Register(&fakeCap{name: "dup"})
	assert.Panics(t, func() { Register(&fakeCap{name: "dup"}) }, "duplicate name must panic")
}

func TestOrderedInfraFirstIntrospectionLast(t *testing.T) {
	resetRegistryForTest(t)
	Register(&fakeCap{name: "memory"})
	Register(&fakeCap{name: "core", infra: true})
	Register(&fakeCap{name: "introspection", infra: true})
	Register(&fakeCap{name: "planning", def: true})
	names := make([]string, 0)
	for _, c := range Ordered() {
		names = append(names, c.Name())
	}
	assert.Equal(t, "core", names[0], "core (infra, non-introspection) must sort first")
	assert.Equal(t, "introspection", names[len(names)-1], "introspection must sort last")
}
