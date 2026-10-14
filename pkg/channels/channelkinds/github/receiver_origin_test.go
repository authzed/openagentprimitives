package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerOriginURL_ReturnsPRHTMLURL pins that a verified pull_request
// delivery yields the PR's own html_url (host-correct — it is whatever host the
// delivery came from, so GitHub Enterprise is covered). Driven from the real
// payload fixture, same as the owner/facts tests, so a wrong json tag can't pass.
func TestTriggerOriginURL_ReturnsPRHTMLURL(t *testing.T) {
	u, ok, err := receiver{}.TriggerOriginURL(testChannel(t), "pull_request", fixtureBody(t, "pull_request_opened.json"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "https://github.com/demo-org/platform/pull/42", u)
}

func TestTriggerOriginURL_NonPROrMissing(t *testing.T) {
	cases := []struct {
		name  string
		event string
		body  string
	}{
		{name: "non-pull_request event: ok=false", event: "ping", body: `{"zen":"x"}`},
		{name: "empty body: ok=false, no decode error", event: "pull_request", body: ``},
		{name: "pull_request without html_url: ok=false", event: "pull_request",
			body: `{"action":"opened","number":7,"pull_request":{},"repository":{"full_name":"demo-org/platform"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, ok, err := receiver{}.TriggerOriginURL(testChannel(t), tc.event, []byte(tc.body))
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Empty(t, u)
		})
	}
}

func TestTriggerOriginURL_UndecodableBodyErrors(t *testing.T) {
	_, ok, err := receiver{}.TriggerOriginURL(testChannel(t), "pull_request", []byte("not json"))
	require.Error(t, err)
	assert.False(t, ok)
}
