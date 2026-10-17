package browser

// capture_chrome_test.go exercises the Task C9 persistent-state round trip against
// a REAL Chrome started by the test (startFixture, chrome_test.go): set a cookie
// and a localStorage item on an allowed origin, capture to the StateStore shape,
// clear the browser's state, then prove a fresh Session injects from the store
// and the origin sees both restored. Skipped with a reason when no Chrome binary
// is present (spec §7).

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStateStore is an in-memory StateStore: the test analogue of the runner's
// per-session state Secret.
type fakeStateStore struct {
	mu   sync.Mutex
	data []byte
}

func (f *fakeStateStore) Load(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...), nil
}

func (f *fakeStateStore) Save(_ context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append([]byte(nil), data...)
	return nil
}

// evalOn runs a JS expression on the session's current page and returns the
// result decoded into out.
func evalOn(t *testing.T, s *Session, expr string, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, s.do(ctx, shortTimeout, func(c context.Context) error {
		return runAction(c, func(ac context.Context) error {
			return chromedp.Evaluate(expr, out).Do(ac)
		})
	}))
}

// navSession navigates the session to url.
func navSession(t *testing.T, s *Session, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, s.do(ctx, navigateTimeout, func(c context.Context) error {
		return runAction(c, func(ac context.Context) error {
			return chromedp.Navigate(url).Do(ac)
		})
	}))
}

func TestBrowserPersistentStateCaptureInjectRoundTrip(t *testing.T) {
	fx := startFixture(t, siteHandler())
	store := &fakeStateStore{}

	// --- Session A: establish a login (cookie) + localStorage, then capture. ---
	sessA := newSession(fx.gateURL, chromeToken)
	sessA.store = store
	sessA.logName = "roundtrip"
	t.Cleanup(sessA.Close)

	navSession(t, sessA, fx.siteBase+"/")
	var ignore any
	evalOn(t, sessA, `document.cookie = "sid=secret-login-token; path=/"; localStorage.setItem("prefs", "dark"); 0`, &ignore)

	require.NoError(t, sessA.captureAndPersist(context.Background()), "capture after establishing state")

	// The persisted blob carries the cookie and the origin's localStorage.
	var blob stateBlob
	require.NoError(t, json.Unmarshal(store.data, &blob), "state Secret holds valid JSON")
	assert.True(t, hasCookie(blob, "sid", "secret-login-token"), "captured blob carries the session cookie")
	assert.Equal(t, "dark", localValue(blob, "prefs"), "captured blob carries the origin's localStorage")

	// --- Clear everything, as a cold profile would be. ---
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, sessA.do(ctx, shortTimeout, func(c context.Context) error {
		return runAction(c, func(ac context.Context) error {
			if err := storage.ClearCookies().Do(ac); err != nil {
				return err
			}
			var clr any
			return chromedp.Evaluate(`localStorage.clear(); 0`, &clr).Do(ac)
		})
	}))
	navSession(t, sessA, fx.siteBase+"/")
	var clearedCookie, clearedPref string
	evalOn(t, sessA, `document.cookie`, &clearedCookie)
	evalOn(t, sessA, `localStorage.getItem("prefs") || ""`, &clearedPref)
	require.NotContains(t, clearedCookie, "secret-login-token", "cookie must be cleared before inject")
	require.Empty(t, clearedPref, "localStorage must be cleared before inject")

	// --- Session B (fresh connection): inject from the store on first attach,
	// then prove the origin sees the restored cookie + localStorage. ---
	sessB := newSession(fx.gateURL, chromeToken)
	sessB.store = store
	sessB.logName = "roundtrip"
	t.Cleanup(sessB.Close)

	// First navigation triggers maybeInject (cookies browser-wide + DOMStorage per
	// origin) before the page loads.
	navSession(t, sessB, fx.siteBase+"/")

	var gotCookie, gotPref string
	evalOn(t, sessB, `document.cookie`, &gotCookie)
	evalOn(t, sessB, `localStorage.getItem("prefs") || ""`, &gotPref)
	assert.Contains(t, gotCookie, "sid=secret-login-token", "injected cookie must be restored on the origin")
	assert.Equal(t, "dark", gotPref, "injected localStorage must be restored on the origin")
}

// TestBrowserEphemeralCapturesNothing proves a nil store (ephemeral toolbox)
// never writes anything, even after an explicit capture call.
func TestBrowserEphemeralCapturesNothing(t *testing.T) {
	fx := startFixture(t, siteHandler())
	sess := newSession(fx.gateURL, chromeToken) // no store => ephemeral
	t.Cleanup(sess.Close)
	navSession(t, sess, fx.siteBase+"/")
	var ignore any
	evalOn(t, sess, `document.cookie = "sid=x; path=/"; 0`, &ignore)
	// captureAndPersist is a no-op with a nil store: it returns nil and touches
	// nothing (there is nowhere to write).
	require.NoError(t, sess.captureAndPersist(context.Background()))
}

func siteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Login</h1></body></html>`))
	})
}

func hasCookie(b stateBlob, name, value string) bool {
	for _, c := range b.Cookies {
		if c.Name == name && c.Value == value {
			return true
		}
	}
	return false
}

func localValue(b stateBlob, key string) string {
	for _, o := range b.Origins {
		if v, ok := o.Local[key]; ok {
			return v
		}
	}
	return ""
}
