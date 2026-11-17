package providers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rotatingStub struct {
	stub
	token string
}

func (s rotatingStub) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: s.token}}}, nil
}

func TestProviderReadsRotatedCredentialAndFailsClosedOnReadFailure(t *testing.T) {
	providers.RegisterRotating("rotating-test", func(token string) (llm.Provider, error) {
		return rotatingStub{stub: stub{name: "rotating-test"}, token: token}, nil
	})
	initial, err := providers.New("rotating-test", "expired-startup-token")
	require.NoError(t, err)
	token := "fresh-one"
	var readErr error
	p := providers.WithCredentialSource(initial, func(context.Context) (string, error) { return token, readErr })
	first, err := p.Send(context.Background(), llm.Request{})
	require.NoError(t, err)
	assert.Equal(t, "fresh-one", first.Content[0].Text)
	token = "fresh-two"
	second, err := p.Send(context.Background(), llm.Request{})
	require.NoError(t, err)
	assert.Equal(t, "fresh-two", second.Content[0].Text)
	readErr = errors.New("credential removed")
	_, err = p.Send(context.Background(), llm.Request{})
	require.ErrorContains(t, err, "credential removed")
}
