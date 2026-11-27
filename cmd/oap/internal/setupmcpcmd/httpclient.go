package setupmcpcmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// mintMaxRedirects caps redirect hops that stay on the named server's own
// origin; hops anywhere else are counted and vetted by the guarded client.
const mintMaxRedirects = 10

// mintHTTPClient is the HTTP client `--mint` runs its OAuth flow with.
//
// The flow follows URLs it reads out of a remote document (the server's OAuth
// metadata), which is why oauth.Login defaults to the SSRF-guarded
// safehttp.Client. But that guard also refuses loopback and private
// addresses, and on a local or desktop install the MCP server IS one: the
// cluster's own advertised URL is http://localhost:<port>, so a fully guarded
// client can never mint against it.
//
// So exactly one origin is exempted — the scheme, host and port of the server
// URL this command was pointed at, by --server or by the kube context's own
// cluster — and nothing else. The person running the command chose that
// destination; they did not choose wherever its metadata, or a redirect, then
// points. Every request to any other origin, including every redirect hop off
// the named one, still goes through the guard, so a server on the open
// internet cannot use this command to reach the caller's loopback or private
// network.
//
// This is deliberately local to the one user-invoked CLI path that needs it,
// not an option on safehttp: nothing server-side should ever be able to ask
// for it.
func mintHTTPClient(serverURL string) (*http.Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parse server URL %q: %w", serverURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("server URL %q must be an absolute http or https URL", serverURL)
	}
	named := originOf(u)

	guarded := safehttp.Client()
	direct := http.DefaultTransport.(*http.Transport).Clone()

	return &http.Client{
		Transport: &namedOriginTransport{named: named, direct: direct, guarded: guarded.Transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if originOf(req.URL) != named {
				return guarded.CheckRedirect(req, via)
			}
			if len(via) >= mintMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", mintMaxRedirects)
			}
			return nil
		},
		Timeout: guarded.Timeout,
	}, nil
}

// namedOriginTransport sends requests for one named origin directly and
// everything else through the SSRF-guarded transport. See mintHTTPClient.
type namedOriginTransport struct {
	named   string
	direct  http.RoundTripper
	guarded http.RoundTripper
}

func (t *namedOriginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if originOf(req.URL) == t.named {
		return t.direct.RoundTrip(req)
	}
	return t.guarded.RoundTrip(req)
}

// originOf renders u's origin as "scheme://host:port", lower-cased and with
// the scheme's default port filled in, so "https://host" and
// "https://HOST:443" compare equal while a different port on the same host —
// a different service — does not.
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}
