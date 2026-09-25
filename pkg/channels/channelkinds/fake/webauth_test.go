package fake

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// resetWebAuth clears the process-global webAuth state. Call from t.Cleanup
// so each test starts with a clean slate.
func resetWebAuth(t *testing.T) {
	t.Helper()
	webAuth.mu.Lock()
	defer webAuth.mu.Unlock()
	webAuth.canonical = map[string]string{}
}

func newFakeAuth(baseURL string) *fakeAuth {
	return &fakeAuth{externalBaseURL: baseURL}
}

// TestWebAuth_RoundTrip checks that SetCanonicalForState + Begin + Complete
// returns the scripted canonical subject for several distinct states.
func TestWebAuth_RoundTrip(t *testing.T) {
	t.Cleanup(func() { resetWebAuth(t) })

	cases := []struct {
		name      string
		state     string
		canonical string
	}{
		{
			name:      "simple state returns scripted canonical",
			state:     "state-abc",
			canonical: "user:alice@example.com",
		},
		{
			name:      "second state returns its own canonical",
			state:     "state-xyz",
			canonical: "user:bob@example.com",
		},
	}

	auth := newFakeAuth("https://identityd.example.com")

	for _, tc := range cases {
		SetCanonicalForState(tc.state, tc.canonical)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.Complete(context.Background(), channelkinds.CallbackParams{
				State: tc.state,
				Code:  "fake-code",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.canonical, got)
		})
	}
}

// TestWebAuth_BeginContainsStateAndCode checks that Begin returns a URL
// with the state URL-encoded and code=fake-code.
func TestWebAuth_BeginContainsStateAndCode(t *testing.T) {
	cases := []struct {
		name         string
		state        string
		wantRawState string // after url.QueryEscape
	}{
		{
			name:         "plain state is echoed",
			state:        "simple-state",
			wantRawState: "simple-state",
		},
		{
			name:         "state with = is URL-escaped",
			state:        "key=value",
			wantRawState: "key%3Dvalue",
		},
		{
			name:         "state with & is URL-escaped",
			state:        "a&b",
			wantRawState: "a%26b",
		},
	}

	auth := newFakeAuth("https://identityd.example.com")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redirectURL, err := auth.Begin(context.Background(), tc.state)
			require.NoError(t, err)

			parsed, err := url.Parse(redirectURL)
			require.NoError(t, err, "Begin must return a valid URL")

			q := parsed.Query()
			assert.Equal(t, "fake-code", q.Get("code"), "code query param")
			assert.Equal(t, tc.state, q.Get("state"), "state query param round-trips via url.Parse")

			// Also assert the raw encoding contains the escaped form.
			assert.Contains(t, redirectURL, "state="+tc.wantRawState, "state must appear URL-escaped in the raw URL")
		})
	}
}

// TestWebAuth_CompleteUnscriptedState verifies that Complete returns
// ErrAuthenticatorUnavailable when no canonical has been scripted for the state.
func TestWebAuth_CompleteUnscriptedState(t *testing.T) {
	t.Cleanup(func() { resetWebAuth(t) })

	auth := newFakeAuth("https://identityd.example.com")
	_, err := auth.Complete(context.Background(), channelkinds.CallbackParams{
		State: "never-scripted",
		Code:  "fake-code",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, channelkinds.ErrAuthenticatorUnavailable),
		"unscripted state must return ErrAuthenticatorUnavailable, got: %v", err)
}

// TestWebAuth_ResetRemovesScript confirms that ResetCanonicalForState causes a
// subsequent Complete to return ErrAuthenticatorUnavailable.
func TestWebAuth_ResetRemovesScript(t *testing.T) {
	t.Cleanup(func() { resetWebAuth(t) })

	SetCanonicalForState("temp-state", "user:alice@example.com")

	auth := newFakeAuth("https://identityd.example.com")

	// First call succeeds.
	got, err := auth.Complete(context.Background(), channelkinds.CallbackParams{State: "temp-state"})
	require.NoError(t, err)
	assert.Equal(t, "user:alice@example.com", got)

	// After reset, same state should fail.
	ResetCanonicalForState("temp-state")
	_, err = auth.Complete(context.Background(), channelkinds.CallbackParams{State: "temp-state"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, channelkinds.ErrAuthenticatorUnavailable),
		"reset state must return ErrAuthenticatorUnavailable, got: %v", err)
}

// TestWebAuth_SpecialCharsRoundTrip verifies that states containing = and &
// survive the Begin → Complete round-trip via url.Parse.
func TestWebAuth_SpecialCharsRoundTrip(t *testing.T) {
	t.Cleanup(func() { resetWebAuth(t) })

	specialStates := []string{
		"key=value",
		"a&b",
		"foo=bar&baz=qux",
	}

	auth := newFakeAuth("https://identityd.example.com")

	for _, state := range specialStates {
		SetCanonicalForState(state, "user:test@example.com")
	}

	for _, state := range specialStates {
		t.Run("state="+state, func(t *testing.T) {
			redirectURL, err := auth.Begin(context.Background(), state)
			require.NoError(t, err)

			parsed, err := url.Parse(redirectURL)
			require.NoError(t, err)

			gotState := parsed.Query().Get("state")
			assert.Equal(t, state, gotState, "state must survive URL encode/decode")

			canonical, err := auth.Complete(context.Background(), channelkinds.CallbackParams{
				State: gotState,
				Code:  "fake-code",
			})
			require.NoError(t, err)
			assert.Equal(t, "user:test@example.com", canonical)
		})
	}
}

// TestWebAuth_ConcurrentSetCanonical exercises the mutex by calling
// SetCanonicalForState from multiple goroutines simultaneously.
func TestWebAuth_ConcurrentSetCanonical(t *testing.T) {
	t.Cleanup(func() { resetWebAuth(t) })

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := range numGoroutines {
		go func(n int) {
			defer wg.Done()
			state := "concurrent-state"
			canonical := "user:concurrent@example.com"
			SetCanonicalForState(state+string(rune('0'+n%10)), canonical)
		}(i)
	}

	wg.Wait()

	// After all goroutines complete, the map should have entries without
	// any data race. Verify one representative entry is reachable.
	auth := newFakeAuth("https://identityd.example.com")
	SetCanonicalForState("probe", "user:probe@example.com")
	got, err := auth.Complete(context.Background(), channelkinds.CallbackParams{State: "probe"})
	require.NoError(t, err)
	assert.Equal(t, "user:probe@example.com", got)
}

// TestKind_WebAuthenticatorNotNil verifies that Kind.WebAuthenticator returns
// a non-nil authenticator when given deps.
func TestKind_WebAuthenticatorNotNil(t *testing.T) {
	wa := Kind{}.WebAuthenticator(channelkinds.WebAuthDeps{
		ExternalBaseURL: "https://identityd.example.com",
	})
	require.NotNil(t, wa, "fake Kind.WebAuthenticator must return a non-nil authenticator")
}
