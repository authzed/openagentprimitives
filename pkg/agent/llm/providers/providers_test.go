package providers_test

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stub is a minimal llm.Provider used to exercise the registry without a real SDK.
type stub struct{ name string }

func (s stub) Name() string                          { return s.name }
func (stub) SupportedFromEnv() bool                  { return true }
func (stub) Pricing(string) (llm.ModelPricing, bool) { return llm.ModelPricing{}, false }
func (stub) Capabilities(string) llm.CapabilitySet   { return llm.NewCapabilitySet() }
func (stub) NativeInputMIMEs(string) llm.MIMESet     { return nil }
func (stub) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, nil
}

func TestNew(t *testing.T) {
	providers.Register("stubprov", func(key string) (llm.Provider, error) {
		return stub{name: "stubprov:" + key}, nil
	})

	cases := []struct {
		name     string
		provider string
		wantName string
		wantErr  bool
	}{
		{name: "known provider selected", provider: "stubprov", wantName: "stubprov:k"},
		{name: "empty defaults to anthropic (registered elsewhere)", provider: "", wantErr: true}, // anthropic not registered in this test binary
		{name: "unknown provider errors", provider: "nope", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := providers.New(tc.provider, "k")
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, p, "provider must be a genuine nil interface on error")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, p)
			assert.Equal(t, tc.wantName, p.Name())
		})
	}
}
