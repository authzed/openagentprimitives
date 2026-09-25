// pkg/channels/channelkinds/github/appprovision/manifest.go
//
// GitHub's App-manifest flow lets a caller create a GitHub App without the
// user clicking through settings pages by hand: render a manifest JSON blob,
// have the user confirm it at https://github.com/settings/apps/new (or an
// org variant), and GitHub redirects back to RedirectURL with a one-time
// `code` query parameter. client.go's Convert exchanges that code for the
// App's identity and secrets.
//
// hook_attributes.url IS the webhook registration for this App — GitHub
// creates the webhook as part of creating the App from this manifest. There
// is no separate "register a webhook" call in this flow; a reader should not
// go looking for one.
package appprovision

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Params names the App this manifest requests and where its webhook and
// setup redirect land.
type Params struct {
	// Name is the App's display name. GitHub App names are unique across all
	// of github.com, so the caller (the wizard, next task) is responsible for
	// picking one that doesn't collide.
	Name string
	// ExternalBaseURL is the base URL this cluster is reachable at from
	// GitHub — e.g. "https://ap.example.com", no trailing slash required (a
	// trailing slash is trimmed). Used both as the App's homepage URL and as
	// the prefix for the webhook URL built with channelevents.WebhookPathFor.
	ExternalBaseURL string
	// Namespace and ChannelName identify the Channel CR the webhook route
	// resolves against — see channelevents.WebhookPathFor.
	Namespace   string
	ChannelName string
	// RedirectURL is where GitHub sends the user, with a one-time ?code=...,
	// after they confirm creating the App from this manifest.
	RedirectURL string
}

// manifestDoc is the JSON shape GitHub's App-manifest flow accepts. Field
// names and shapes here are GitHub's own — see
// https://docs.github.com/en/apps/sharing-github-apps-with-your-organization/registering-a-github-app-from-a-manifest
// — not a shape we control.
type manifestDoc struct {
	Name          string            `json:"name"`
	URL           string            `json:"url"`
	RedirectURL   string            `json:"redirect_url"`
	Public        bool              `json:"public"`
	DefaultEvents []string          `json:"default_events"`
	DefaultPerms  map[string]string `json:"default_permissions"`
	// HookAttrs registers the webhook (see the package doc above). There is
	// deliberately no setup_url field alongside it. GitHub only redirects an
	// App *installation* back to a caller — with installation_id,
	// setup_action, and state as query parameters — when the App has a Setup
	// URL configured:
	// docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url.
	// Without one, installing this App just leaves the user on GitHub's own
	// confirmation page. That is why the github kind asks for the
	// installation ID by hand — it prints the install URL and takes the ID
	// back as an answer — rather than capturing it on the same loopback
	// redirect the App-CREATION step uses, whose RedirectURL field above IS
	// honored unconditionally. Adding a setup_url here, pointed at the same
	// kind of loopback listener, is what would let a future run capture that
	// redirect instead of asking for it.
	HookAttrs hookAttrs `json:"hook_attributes"`
}

type hookAttrs struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// WebhookURL is the absolute address GitHub delivers this Channel's events to
// — the value BuildManifest registers as the App's hook_attributes.url.
//
// EXPORTED SO NOTHING RESTATES IT. A caller that has to TELL an operator what
// to configure — the github wizard, for an operator bringing an App this run
// did not create — needs the same string the manifest carries, and a second
// spelling of it is a webhook URL that agrees with the manifest right up until
// one of the two is edited. The path itself is channelevents' to define; this
// only says which base it hangs off.
func WebhookURL(p Params) string {
	return strings.TrimSuffix(p.ExternalBaseURL, "/") +
		channelevents.WebhookPathFor("github", p.Namespace, p.ChannelName)
}

// BuildManifest renders the App-manifest JSON GitHub's manifest flow accepts.
//
// The permission set requested here — contents:read, pull_requests:read,
// metadata:read, checks:write — is not a starting point to be widened later;
// it IS the structural control for the whole reviewbot feature. Under these
// four permissions the agent is UNABLE to modify a repository, merge
// anything, or post a PR comment, no matter what a pull request's title or
// body talks it into (see the prompt-injection framing in
// pkg/channels/channelkinds/github/receiver.go, which is mitigation layered
// on top of this, not a substitute for it). Do not add a permission here
// without a security review of the consequence.
func BuildManifest(p Params) (string, error) {
	base := strings.TrimSuffix(p.ExternalBaseURL, "/")
	doc := manifestDoc{
		Name:          p.Name,
		URL:           base,
		RedirectURL:   p.RedirectURL,
		Public:        false,
		DefaultEvents: []string{"pull_request"},
		DefaultPerms: map[string]string{
			"contents":      "read",
			"pull_requests": "read",
			"metadata":      "read",
			"checks":        "write",
		},
		HookAttrs: hookAttrs{
			URL:    WebhookURL(p),
			Active: true,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("build github app manifest: %w", err)
	}
	return string(raw), nil
}
