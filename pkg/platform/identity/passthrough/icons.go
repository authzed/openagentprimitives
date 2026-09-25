package passthrough

import "strings"

// IconURL composes the external identityd icon-endpoint URL for a
// credential. Both arguments must be non-empty — an empty result
// signals "no icon available" and callers should omit the image
// element entirely (Slack rejects empty image_url; HTML <img src="">
// fails-open as a broken-image icon).
//
// Trailing slashes on externalBaseURL are trimmed so subpath-rooted
// deployments compose correctly.
func IconURL(externalBaseURL, credName string) string {
	if externalBaseURL == "" || credName == "" {
		return ""
	}
	return strings.TrimRight(externalBaseURL, "/") + "/icon/" + credName
}
