package bravesearch_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/bravesearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
)

func TestBackend_Name(t *testing.T) {
	assert.Equal(t, "bravesearch", bravesearch.Backend{}.Name())
}

// TestBackend_SelfRegisters pins the init()-time registration this package
// promises — the same contract channelkinds and sandboxkinds backends make.
func TestBackend_SelfRegisters(t *testing.T) {
	got, ok := registry.Get(bravesearch.KindName)
	require.True(t, ok, "bravesearch must self-register via init()")
	assert.Equal(t, "bravesearch", got.Name())
}

func TestBackend_New(t *testing.T) {
	cases := []struct {
		name    string
		deps    websearch.Deps
		wantErr string
	}{
		{
			name:    "missing HTTPClient: refuses to construct",
			deps:    websearch.Deps{APIKey: "k"},
			wantErr: "HTTPClient is required",
		},
		{
			name:    "missing APIKey: refuses to construct",
			deps:    websearch.Deps{HTTPClient: http.DefaultClient},
			wantErr: "APIKey is required",
		},
		{
			name: "both present: constructs a Provider",
			deps: websearch.Deps{HTTPClient: http.DefaultClient, APIKey: "k"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := bravesearch.Backend{}.New(tc.deps)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Nil(t, p, "provider must be a genuine nil interface on error")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, p)
			assert.Equal(t, "bravesearch", p.Name())
		})
	}
}
