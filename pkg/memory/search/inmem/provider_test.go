package inmem_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	searchinmem "github.com/authzed/openagentprimitives/pkg/memory/search/inmem"
)

func TestInmemProvider_Name(t *testing.T) {
	p := searchinmem.New(meminmem.NewBackend())
	assert.Equal(t, "inmem", p.Name())
}

func TestInmemProvider_Capabilities(t *testing.T) {
	p := searchinmem.New(meminmem.NewBackend())
	caps := p.SearchCapabilities()
	assert.False(t, caps.TextSearch)
	assert.False(t, caps.VectorSearch)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
}

func TestInmemProvider_SearchReturnsBackendEntries(t *testing.T) {
	b := meminmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "turn", ID: "turn-1", Tags: []string{"role:user"},
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "turn", ID: "turn-2", Tags: []string{"role:assistant"},
	}))

	p := searchinmem.New(b)
	res, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{scope},
		Kinds:  []string{"turn"},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
	for _, se := range res.Entries {
		assert.Equal(t, 1.0, se.Score)
		assert.Equal(t, "inmem", se.Source)
	}
}

func TestInmemProvider_SearchMultipleScopes(t *testing.T) {
	b := meminmem.NewBackend()
	s1 := memory.Scope{Kind: "session", ID: "ns/a"}
	s2 := memory.Scope{Kind: "session", ID: "ns/b"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: s1, Kind: "turn", ID: "turn-1",
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: s2, Kind: "turn", ID: "turn-2",
	}))

	p := searchinmem.New(b)
	res, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{s1, s2},
		Kinds:  []string{"turn"},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
}

func TestInmemProvider_IndexNoOp(t *testing.T) {
	p := searchinmem.New(meminmem.NewBackend())
	assert.NoError(t, p.Index(context.Background(), memory.Scope{}, "", "", nil))
	assert.NoError(t, p.DeleteIndex(context.Background(), memory.Scope{}, "", ""))
	assert.NoError(t, p.DeleteScopeIndex(context.Background(), memory.Scope{}))
}
