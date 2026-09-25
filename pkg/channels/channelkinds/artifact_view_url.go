package channelkinds

import (
	"fmt"
	"net/url"
	"strings"
)

// ComposeArtifactViewURL composes the DURABLE artifact-view page URL for one
// artifact in one session:
//
//	"<base>/artifact-view?artifactId=<id>&sessionRef=<ns>/<name>"
//
// It is the sibling of ComposeSessionViewURL and ComposeAgentUIURL, and the
// ONE place this shape is defined.
//
// # How it differs from a minted artifact-view link
//
// ArtifactViewMinter mints "<base>/artifact-view?d=…&sig=…": a signed payload
// naming ONE subject, with a TTL, minted at the moment a known person clicks.
// That is the right link for a Slack button, where the clicker is identified
// before the URL exists.
//
// This URL names a resource and carries no capability at all — no signature,
// no expiry, no subject. Holding the string grants nothing: webd resolves it
// on ARRIVAL, the visitor authenticates, and the artifact-view page's own
// artifact#view check decides whether they may see it (see
// pkg/web/webui/artifactview's bindView). Both forms reach the same page
// through the same authorization choke point; only the two ways of addressing
// it differ.
//
// That is what makes this one safe to write somewhere whose audience is not
// known in advance and cannot be enumerated — a GitHub check run's
// details_url, read by everyone who can read the pull request. A minted link
// cannot go there: it would either be bound to the agent's own subject, handing
// every reader of the pull request the agent's view, or expire long before a
// maintainer opens it.
//
// Returns ("", nil) when base is empty — webd's external-URL ConfigMap exists
// but is not populated yet — which callers treat as "not addressable yet" and
// skip. Returns an error for a malformed sessionRef or an empty artifactID:
// both are caller bugs, and a link naming only half of the pair would address
// something other than the artifact it claims to.
func ComposeArtifactViewURL(base, sessionRef, artifactID string) (string, error) {
	ns, name, ok := strings.Cut(sessionRef, "/")
	if !ok || ns == "" || name == "" {
		return "", fmt.Errorf("malformed sessionRef %q (want \"ns/name\")", sessionRef)
	}
	if artifactID == "" {
		return "", fmt.Errorf("empty artifactID for session %q", sessionRef)
	}
	trimmed := strings.TrimRight(base, "/")
	if trimmed == "" {
		return "", nil
	}
	// url.Values.Encode sorts by key, so the same inputs always yield the same
	// string — the check run this lands on is rewritten on every redelivery of
	// the same event, and a link that reordered itself would look like a change.
	return trimmed + "/artifact-view?" + url.Values{
		"artifactId": {artifactID},
		"sessionRef": {sessionRef},
	}.Encode(), nil
}
