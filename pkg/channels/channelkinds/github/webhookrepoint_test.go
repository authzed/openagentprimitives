package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// repointRecorder is GitHub's App-configuration API reduced to the one write
// this file is about: it records every URL a PATCH carried.
//
// It answers 200 to everything, deliberately — that is what GitHub does here.
// Reachability is enforced when an App is CREATED, not on
// PATCH /app/hook/config, so a fake that refused a loopback would be testing a
// rule the provider does not have and would hide the reason this guard exists.
type repointRecorder struct {
	srv *httptest.Server

	mu        sync.Mutex
	repointed []string
}

func newRepointRecorder(t *testing.T) *repointRecorder {
	t.Helper()
	r := &repointRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			r.mu.Lock()
			r.repointed = append(r.repointed, req.URL.Path)
			r.mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *repointRecorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.repointed...)
}

func repointSecrets(t *testing.T) channelkinds.WebhookSecrets {
	t.Helper()
	return channelkinds.WebhookSecrets{Data: map[string][]byte{
		"app-id": []byte("12345"), "private-key": testAppPEM(t),
	}}
}

// TestRepointWebhookURL_RefusesAnAddressNoDeliveryCanReach puts the rule where
// the write is, rather than only at the one caller that reaches it today.
//
// The channel controller stands down before calling for its own reasons, and
// that is not enough on its own: this is an outward-facing write against
// somebody's App, GitHub does NOT enforce reachability on it, and a loopback
// PATCHed here returns 200 and silently ends every delivery. A second caller —
// or a controller branch that stopped standing down — must not be able to
// reintroduce that.
func TestRepointWebhookURL_RefusesAnAddressNoDeliveryCanReach(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "loopback: refused, naming the address",
			url:  "http://127.0.0.1:17080/webhooks/github/default/demo-reviewbot-gh",
			want: "loopback",
		},
		{
			name: "localhost: refused, because it names whoever resolves it",
			url:  "http://localhost:8080/webhooks/github/default/demo-reviewbot-gh",
			want: "localhost",
		},
		{
			name: "private range: refused, reachable only inside your own network",
			url:  "http://10.1.2.3/webhooks/github/default/demo-reviewbot-gh",
			want: "private",
		},
		{
			name: "a bare path: refused rather than PATCHed as-is",
			url:  "/webhooks/github/default/demo-reviewbot-gh",
			want: "absolute URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRepointRecorder(t)

			err := Kind{}.RepointWebhookURL(context.Background(),
				demoChannel(), repointSecrets(t), tc.url, rec.srv.URL)

			require.Error(t, err, "an address no delivery can reach is not a webhook URL")
			assert.Contains(t, err.Error(), tc.want,
				"the refusal must say what is wrong with the address, not just that something is")
			assert.Empty(t, rec.writes(), "and nothing may reach the provider")
		})
	}
}

// TestRepointWebhookURL_WritesAReachableAddress is the control: a guard that
// refused everything would satisfy every assertion above and break the feature
// the repoint exists to deliver.
func TestRepointWebhookURL_WritesAReachableAddress(t *testing.T) {
	rec := newRepointRecorder(t)

	err := Kind{}.RepointWebhookURL(context.Background(), demoChannel(), repointSecrets(t),
		"https://demo-tunnel.demo.test/webhooks/github/default/demo-reviewbot-gh", rec.srv.URL)

	require.NoError(t, err)
	assert.Equal(t, []string{"/app/hook/config"}, rec.writes(),
		"a public address is written, and only the hook-config endpoint is touched")
}
