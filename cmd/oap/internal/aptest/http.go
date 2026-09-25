package aptest

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// RoundTripFunc adapts a plain function to http.RoundTripper, so tests can
// stub a client without a real listener.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// InstallVerifyHTTPStub points builtins.VerifyCredential's live-probe client
// at a canned 200 response for every request, so tests that drive a real setup
// flow through to the store path (which runs the live-verification gate — see
// pkg/platform/identity/setup/engine_test.go's identical helper) don't make a
// real network call to the provider's declared verify: endpoint (e.g.
// https://api.github.com/user for github-pat) with a fixture token that was
// never a genuine credential.
func InstallVerifyHTTPStub(t *testing.T) {
	t.Helper()
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: RoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"login":"octocat"}`)),
			}, nil
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}
