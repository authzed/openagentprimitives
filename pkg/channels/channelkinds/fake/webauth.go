package fake

import (
	"context"
	"net/url"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// webAuthState is the per-process registry tests use to script the
// fake authenticator's responses.
type webAuthState struct {
	mu        sync.Mutex
	canonical map[string]string // state → canonical-subject to return
}

var webAuth = &webAuthState{canonical: map[string]string{}}

// SetCanonicalForState scripts what the fake authenticator returns from
// Complete when called with the given state. Tests call this before
// driving the OIDC flow.
func SetCanonicalForState(state, canonical string) {
	webAuth.mu.Lock()
	defer webAuth.mu.Unlock()
	webAuth.canonical[state] = canonical
}

// ResetCanonicalForState clears the script for a given state. Useful in
// table-driven tests that need clean slates per case.
func ResetCanonicalForState(state string) {
	webAuth.mu.Lock()
	defer webAuth.mu.Unlock()
	delete(webAuth.canonical, state)
}

type fakeAuth struct {
	externalBaseURL string
}

// Begin returns a redirect URL to identityd's own callback (no real
// provider). The test just navigates back with the prepared state, and
// Complete looks up what canonical subject to return.
func (a *fakeAuth) Begin(_ context.Context, state string) (string, error) {
	u := a.externalBaseURL + "/oidc/callback/fake?state=" + url.QueryEscape(state) + "&code=fake-code"
	return u, nil
}

// Complete looks up the canonical the test scripted via SetCanonicalForState.
// Returns ErrAuthenticatorUnavailable if no script exists for this state —
// surfacing the test mistake immediately rather than silently passing.
func (a *fakeAuth) Complete(_ context.Context, cb channelkinds.CallbackParams) (string, error) {
	webAuth.mu.Lock()
	defer webAuth.mu.Unlock()
	if c, ok := webAuth.canonical[cb.State]; ok {
		return c, nil
	}
	return "", channelkinds.ErrAuthenticatorUnavailable
}
