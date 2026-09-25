package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// resolveScope picks the scope string to send to the authorization
// endpoint: explicit Opts.Scope wins, otherwise fall back to the
// Metadata.ScopesSupported list joined with spaces.
func resolveScope(opts Opts) string {
	if opts.Scope != "" {
		return opts.Scope
	}
	if len(opts.Metadata.ScopesSupported) > 0 {
		return strings.Join(opts.Metadata.ScopesSupported, " ")
	}
	return ""
}

// buildAuthURL constructs the authorization URL with all required parameters
// (response_type, client_id, redirect_uri, state, code_challenge[+method])
// plus the resolved scope.
func buildAuthURL(endpoint, clientID, redirectURI string, pkce PKCE, scope string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("state", pkce.State)
	v.Set("code_challenge", pkce.Challenge)
	v.Set("code_challenge_method", "S256")
	if scope != "" {
		v.Set("scope", scope)
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + v.Encode()
}

const callbackSuccessPage = `<!DOCTYPE html>
<html><head><title>Authorization complete</title></head>
<body>
<h1>Authorization complete</h1>
<p>You can close this window.</p>
</body></html>`

func callbackErrorPage(errCode, desc string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>Authorization failed</title></head>
<body>
<h1>Authorization failed</h1>
<p>Error: <code>%s</code></p>
<p>%s</p>
</body></html>`, errCode, desc)
}
