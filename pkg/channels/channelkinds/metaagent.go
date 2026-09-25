// The two metaagent surfaces — a scope-approval prompt with decision actions,
// and a private one-line notice — as sub-channels. Routing them through the
// SubChannelSender seam makes the rendering each kind's own job, and makes
// "this kind cannot render it" an explicit nil Sender rather than a session
// whose scope approval is dropped while the runner blocks the full approval
// window and then halts unscoped.
package channelkinds

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Sub-channel names for the metaagent flow. Passed to Kind.SubChannelSender;
// a kind that cannot render either surface returns nil, which is the
// documented degrade path for every sub-channel.
const (
	// SubChannelMetaagentScopeApproval carries a
	// pkg/authz/scope.MetaagentApprovalPayload: a human-approval prompt for a
	// dynamic scope change, published by authzd. That struct is the single
	// declaration of the prompt's field set, shared by composer and renderer
	// because pkg/authz/scope depends on nothing else in this repo.
	//
	// The rendering MUST offer the decision actions the kind's callback path
	// can route back — on Slack, the buttons whose values
	// MetaagentApprovalButtonValue encodes.
	SubChannelMetaagentScopeApproval = "metaagent_scope_approval"

	// SubChannelMetaagentNotice carries MetaagentNoticePayload: a single-line
	// acknowledgment addressed to ONE user (applied/denied, cannot-address),
	// published by authzd and by the operator's SessionFork deny path.
	SubChannelMetaagentNotice = "metaagent_notice"
)

// MetaagentNoticePayload is the wire format of
// ap.session.<ns>.<name>.out.metaagent_notice, published by authzd and by the
// operator's SessionFork deny path.
//
// Two recipient fields, because the two publishers know different things.
// authzd is handling a channel interaction and already holds the clicker's
// channel-native id, so it sets Requester. The operator is reconciling a CR
// and holds only PendingRestart.TriggeredBy — a canonical SpiceDB subject —
// so it sets RequesterCanonical and the kind resolves it. Addressing a user
// is the channel kind's job; the publisher should not have to guess.
type MetaagentNoticePayload struct {
	// Requester is a CHANNEL-NATIVE user id (Slack "U…"). Kinds pass it to
	// their addressing API as-is, so a canonical subject here cannot be
	// delivered — see ResolveNoticeRecipient, which refuses one rather than
	// pass it through.
	Requester string `json:"requester"`
	// RequesterCanonical is the canonical SpiceDB subject ("user:<base64url>",
	// bare form also accepted) of the user to notify. Resolved to a
	// channel-native id at delivery time. Mirrors the RecipientCanonical field
	// on the credential_request / credential_linked / portal_access payloads.
	RequesterCanonical string `json:"requesterCanonical,omitempty"`
	// Body is the notice text, already rendered for a human; one line.
	Body string `json:"body"`
}

// canonicalSubjectPrefix marks a value as a SpiceDB subject rather than a
// channel-native id. No channel-native user id begins with it.
const canonicalSubjectPrefix = "user:"

// ResolveNoticeRecipient decides which channel-native user id a notice is
// addressed to, resolving a canonical subject through the kind's own resolver
// when needed.
//
// Precedence:
//  1. RequesterCanonical set          → resolve it (explicit beats implicit)
//  2. Requester is a canonical subject → resolve it (defensive; see below)
//  3. Requester set                    → use as-is (the authzd path)
//  4. neither                          → error
//
// Step 2 is defensive: two separate publishers have put a canonical subject in
// the channel-native field and handed it to an API that cannot address it.
// Rather than trust every future publisher, refuse to pass a "user:"-prefixed
// value through and resolve it instead.
//
// Fail-loud: an unresolvable recipient returns an error. Posting to a bogus id
// and silently skipping the notice are both how such a bug stays invisible.
//
// It lives here rather than in a kind because the precedence rule is a
// property of the payload, not of any transport; only `resolve` is
// kind-specific.
func ResolveNoticeRecipient(ctx context.Context, pl MetaagentNoticePayload,
	resolve func(context.Context, identity.CanonicalUserID) (string, error)) (string, error) {

	canonical := pl.RequesterCanonical
	if canonical == "" && strings.HasPrefix(pl.Requester, canonicalSubjectPrefix) {
		canonical = pl.Requester
	}
	if canonical != "" {
		// The canonical came off the metaagent envelope, which the publisher
		// composed — see the audit note on publisher-asserted requesters. This
		// call only resolves it to a display handle for a notice.
		id, err := resolve(ctx, identity.CanonicalFromTrusted(canonical,
			"requester canonical from the metaagent envelope, for display only"))
		if err != nil {
			return "", fmt.Errorf("resolve notice recipient %q: %w", canonical, err)
		}
		if id == "" {
			return "", fmt.Errorf("resolve notice recipient %q: empty channel user id", canonical)
		}
		return id, nil
	}
	if pl.Requester == "" {
		return "", fmt.Errorf("notice has no requester or requesterCanonical")
	}
	return pl.Requester, nil
}
