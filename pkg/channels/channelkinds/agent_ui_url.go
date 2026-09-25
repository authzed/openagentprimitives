package channelkinds

import (
	"fmt"
	"net/url"
	"strings"
)

// SubChannelAgentUIOffer is the sub-channel name carrying
// channelevents.KindAgentUIOffer. Passed to Kind.SubChannelSender and
// resolved by every SubChannelSenderFor implementation; a kind that does not
// implement it returns nil and the outbound relay drops the envelope with a
// logged reason, which is the established degrade for a sub-channel a
// transport has no rendering for.
const SubChannelAgentUIOffer = "agent_ui_offer"

// ComposeAgentUIURL composes the agent-UI shell page URL for sessionRef
// ("ns/name"): "<base>/sessions?session=<ns>/<name>", with the session
// carried as a QUERY parameter rather than a path segment.
//
// The query form is required, not stylistic. The shell serves its own APIs
// under the sibling prefix "/sessions/api/…", so a path form
// ("/sessions/<ns>/<name>") is shadowed for a Kubernetes namespace literally
// named "api" — a legal namespace name that would silently become
// unaddressable.
//
// Like ComposeSessionViewURL, this is the ONE place the shape is defined.
// Returns ("", nil) when base is empty — webd's external-URL ConfigMap exists
// but is not populated — which callers treat as "not configured yet". Returns
// an error only for a malformed sessionRef, a caller bug rather than a gap.
func ComposeAgentUIURL(base, sessionRef string) (string, error) {
	ns, name, ok := strings.Cut(sessionRef, "/")
	if !ok || ns == "" || name == "" {
		return "", fmt.Errorf("malformed sessionRef %q (want \"ns/name\")", sessionRef)
	}
	trimmed := strings.TrimRight(base, "/")
	if trimmed == "" {
		return "", nil
	}
	return trimmed + "/sessions?" + url.Values{"session": {sessionRef}}.Encode(), nil
}

// AgentUIMinter composes the agent-UI shell page URL for a session. Like
// SessionViewMinter and unlike ArtifactViewMinter, the returned URL carries
// NO signed capability and NO TTL: the shell authorizes at open time, so the
// link is durable and safe to bake into a message at post time.
//
// It takes only the sessionRef — a subject and backLink would be embedded
// nowhere. Nil means webd is not configured for this process; senders decide
// whether that is a quiet skip or a loud failure, per how central the offer is
// to their surface.
type AgentUIMinter interface {
	MintAgentUILink(sessionRef string) (string, error)
}
