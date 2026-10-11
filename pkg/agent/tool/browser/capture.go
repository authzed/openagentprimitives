package browser

// capture.go implements Phase C Task C9's `persistent` session-state round trip:
// after each acting call and on graceful stop, a credentialed browser's cookies
// and localStorage are captured through the DevTools Storage + DOMStorage domains
// and written to a per-session Secret (the StateStore seam); on the first attach
// after a pod boot they are re-injected, so a login survives idle reap and wake
// (spec §2.3, §5.5).
//
// The runner is the SOLE writer of that Secret; the operator creates it empty and
// keeps it across reap. An `ephemeral` toolbox passes a nil StateStore to
// Synthesize, so every function here short-circuits and nothing is ever captured
// or injected.
//
// Failure policy (repo rule: never silently drop errors, but a capture/inject
// failure must not crash the turn): every function returns its error with host
// context so the Session's best-effort callers can LOG it and CONTINUE — the next
// acting call, or the SIGTERM capture, retries. Nothing here fails a tool result.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/domstorage"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// StateStore is the runner-side seam the browser Session persists captured state
// through. Load returns the last-saved blob (nil, nil when the Secret is empty —
// a first boot), Save overwrites it. The production implementation
// (internal/cmd/runner) reads and updates the per-session state Secret via the
// Kubernetes client; a nil StateStore means an `ephemeral` toolbox and disables
// capture/inject entirely.
type StateStore interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, data []byte) error
}

// stateCookie is the JSON-serializable projection of one cookie we persist. It
// mirrors the subset of network.Cookie that network.CookieParam can round-trip
// back into Chrome on inject — Size/Session/PartitionKeyOpaque and similar
// read-only fields are intentionally dropped.
type stateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"` // seconds since epoch; <=0 is a session cookie
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

// originStorage is one origin's localStorage snapshot.
type originStorage struct {
	Origin string            `json:"origin"`
	Local  map[string]string `json:"local,omitempty"`
}

// stateBlob is the whole persisted browser session state: the cookie jar plus a
// per-origin localStorage map. It is what gets JSON-marshaled into the state
// Secret. It carries session-bearer material, so like a credential it is never
// logged and lives only in the per-session Secret.
type stateBlob struct {
	Cookies []stateCookie   `json:"cookies,omitempty"`
	Origins []originStorage `json:"origins,omitempty"`
}

// captureInto reads the browser's cookie jar (Storage.getCookies — browser-wide,
// so it covers every allowed origin the session visited) and the CURRENT page's
// localStorage (DOMStorage, scoped to the page we just acted on). ctx MUST already
// be bound to the chromedp browser context. The cookie jar is captured whole: the
// gate constrains egress to the toolbox's effective sites, so every cookie present
// is already an allowed-site cookie, and capturing the full jar avoids dropping a
// login cookie set on a parent domain.
func captureInto(ctx context.Context, out *stateBlob) error {
	return runAction(ctx, func(ac context.Context) error {
		cookies, err := storage.GetCookies().Do(ac)
		if err != nil {
			return fmt.Errorf("get cookies: %w", err)
		}
		out.Cookies = out.Cookies[:0]
		for _, c := range cookies {
			out.Cookies = append(out.Cookies, stateCookie{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   c.Domain,
				Path:     c.Path,
				Expires:  c.Expires,
				HTTPOnly: c.HTTPOnly,
				Secure:   c.Secure,
				SameSite: c.SameSite.String(),
			})
		}

		// Current origin's localStorage. about:blank / no page reports "null", which
		// is not a capturable origin — leave Origins untouched in that case.
		var origin string
		if err := chromedp.Evaluate(`location.origin`, &origin).Do(ac); err != nil {
			return fmt.Errorf("read current origin: %w", err)
		}
		if origin == "" || origin == "null" {
			return nil
		}
		if err := domstorage.Enable().Do(ac); err != nil {
			return fmt.Errorf("enable DOMStorage: %w", err)
		}
		items, err := domstorage.GetDOMStorageItems(&domstorage.StorageID{
			SecurityOrigin: origin,
			IsLocalStorage: true,
		}).Do(ac)
		if err != nil {
			return fmt.Errorf("get localStorage for %q: %w", origin, err)
		}
		local := make(map[string]string, len(items))
		for _, kv := range items {
			if len(kv) == 2 {
				local[kv[0]] = kv[1]
			}
		}
		out.Origins = []originStorage{{Origin: origin, Local: local}}
		return nil
	})
}

// injectFrom writes a saved blob back into a freshly-booted browser. Cookies go in
// via Storage.setCookies (browser-wide, so they land BEFORE any navigation — this
// is the login-bearer path that must be in place before the first tool runs).
// localStorage is restored differently: DOMStorage.setDOMStorageItem only works
// for an origin whose frame is currently loaded ("Frame not found" otherwise), and
// at first attach we are on about:blank, so instead a Page new-document script is
// registered that seeds each origin's localStorage keys the first time that origin
// loads. It seeds SEED-IF-ABSENT (only a key the live localStorage does not
// already have), so a login the model performs mid-session is never clobbered by a
// later navigation re-running the script; capture-after-acting keeps the Secret
// current regardless.
//
// ctx MUST already be bound to the chromedp browser context. Best-effort: a
// failure to set cookies or register the script is returned with context so the
// caller can log and continue — a missing restore means the model re-logs-in
// (spec §5.5), never a crash.
func injectFrom(ctx context.Context, blob stateBlob) error {
	return runAction(ctx, func(ac context.Context) error {
		var firstErr error
		if len(blob.Cookies) > 0 {
			params := make([]*network.CookieParam, 0, len(blob.Cookies))
			for _, c := range blob.Cookies {
				p := &network.CookieParam{
					Name:     c.Name,
					Value:    c.Value,
					Domain:   c.Domain,
					Path:     c.Path,
					Secure:   c.Secure,
					HTTPOnly: c.HTTPOnly,
					SameSite: network.CookieSameSite(c.SameSite),
				}
				if c.Expires > 0 {
					exp := cdp.TimeSinceEpoch(time.Unix(0, int64(c.Expires*float64(time.Second))))
					p.Expires = &exp
				}
				params = append(params, p)
			}
			if err := storage.SetCookies(params).Do(ac); err != nil {
				firstErr = fmt.Errorf("set cookies: %w", err)
			}
		}
		if src := localStorageSeedScript(blob.Origins); src != "" {
			if err := page.Enable().Do(ac); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("enable Page: %w", err)
			}
			if _, err := page.AddScriptToEvaluateOnNewDocument(src).Do(ac); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("register localStorage seed script: %w", err)
			}
		}
		return firstErr
	})
}

// localStorageSeedScript builds the new-document JS that seeds each origin's saved
// localStorage keys (seed-if-absent) on load. Returns "" when there is nothing to
// seed. The per-origin map is JSON-embedded, so arbitrary key/value bytes need no
// manual escaping.
func localStorageSeedScript(origins []originStorage) string {
	data := map[string]map[string]string{}
	for _, o := range origins {
		if len(o.Local) > 0 {
			data[o.Origin] = o.Local
		}
	}
	if len(data) == 0 {
		return ""
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	// SEED-IF-ABSENT: only set a key the page does not already hold, so a
	// mid-session write survives a later navigation that re-runs this script.
	return `(function(){try{var S=` + string(raw) +
		`;var d=S[location.origin];if(d){for(var k in d){if(localStorage.getItem(k)===null){localStorage.setItem(k,d[k]);}}}}catch(e){}})();`
}

// mergeBlob folds a freshly-captured blob into the prior saved blob. Cookies are
// replaced wholesale (captureInto reads the full live jar, so the capture IS the
// current truth), while localStorage is merged by origin: a capture only ever
// carries the CURRENT page's origin, so the other origins' snapshots from prior
// turns must be preserved rather than clobbered.
func mergeBlob(prior, captured stateBlob) stateBlob {
	merged := stateBlob{Cookies: captured.Cookies}
	byOrigin := map[string]map[string]string{}
	order := []string{}
	add := func(o originStorage) {
		if _, seen := byOrigin[o.Origin]; !seen {
			order = append(order, o.Origin)
		}
		byOrigin[o.Origin] = o.Local
	}
	for _, o := range prior.Origins {
		add(o)
	}
	for _, o := range captured.Origins { // captured wins for its origin
		add(o)
	}
	for _, origin := range order {
		merged.Origins = append(merged.Origins, originStorage{Origin: origin, Local: byOrigin[origin]})
	}
	return merged
}

// marshalBlob / unmarshalBlob isolate the JSON contract the state Secret holds.
func marshalBlob(b stateBlob) ([]byte, error) { return json.Marshal(b) }

func unmarshalBlob(data []byte) (stateBlob, error) {
	var b stateBlob
	if len(data) == 0 {
		return b, nil
	}
	err := json.Unmarshal(data, &b)
	return b, err
}
