package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

type fake struct{}

func (fake) Parse(tk *toolkit.Toolkit, argv []string) (*parser.Call, error) {
	return &parser.Call{Subcommand: "fake"}, nil
}

func TestRegistry_EmptyByDefault(t *testing.T) {
	_, ok := Get("anything")
	assert.False(t, ok, "registry should be empty at MVP")
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	// We can't rely on global state between tests if we persist across; use a temporary
	// registration that we undo. For a plain map-backed registry we just accept that
	// this registration persists within this test binary.
	Register("fake-test", fake{})
	p, ok := Get("fake-test")
	require.True(t, ok, "Get(fake-test) should hit")
	c, err := p.Parse(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "fake", c.Subcommand)
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	Register("dup", fake{})
	require.Panics(t, func() {
		Register("dup", fake{})
	}, "expected panic on duplicate registration")
}
