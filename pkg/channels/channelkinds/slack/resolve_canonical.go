package slack

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// resolveSlackUserIDFromCanonical translates a SpiceDB subject's
// canonical ObjectID (the base64-RawURL form identity.Principal.Canonical
// emits) into a Slack user_id, calling Slack's users.lookupByEmail
// API when the canonical encodes an email address.
//
// The "approval flow couldn't DM Bob" incident traced to a previous
// helper that string-prefix-checked `canonical` against the literal
// "slack:" and dropped everything else with a log line. SpiceDB
// subjects in this codebase are base64-encoded (either of an email
// or of a `slack:<team>:<id>` triple), so the prefix check never
// matched real-world subjects and the approval ephemeral was silently
// skipped.
//
// This helper is FAIL-LOUD: every unresolvable input — invalid
// base64, an email not in the Slack workspace, a transport-level
// Slack API error, an unrecognized canonical shape — returns an
// error rather than ""+nil. The caller surfaces those errors instead
// of pretending the DM was sent.
func resolveSlackUserIDFromCanonical(ctx context.Context, cli slackClient, canonical identity.CanonicalUserID) (string, error) {
	if canonical.IsZero() {
		return "", fmt.Errorf("resolve slack user: canonical is empty")
	}
	// Tolerate the SpiceDB type prefix "user:" — channelsd's pipeline
	// stamps AnnotationStartedByCanonicalID + downstream envelope
	// payloads (credential_request RecipientCanonical, credential_linked
	// RecipientCanonical, portal_access RecipientCanonical) with the
	// type-prefixed subject form. The resolver only needs the canonical
	// ObjectID part. Strip the prefix here so callers can pass either
	// form without coordinating.
	canonicalStr := strings.TrimPrefix(canonical.String(), "user:")
	raw, err := base64.RawURLEncoding.DecodeString(canonicalStr)
	if err != nil {
		return "", fmt.Errorf("resolve slack user: decode canonical %q: %w", canonicalStr, err)
	}
	decoded := string(raw)

	switch {
	case strings.Contains(decoded, "@"):
		// email-typed canonical: do a live Slack lookup.
		u, err := cli.GetUserByEmailContext(ctx, decoded)
		if err != nil {
			return "", fmt.Errorf("resolve slack user: lookup by email %q: %w", decoded, err)
		}
		if u == nil || u.ID == "" {
			return "", fmt.Errorf("resolve slack user: slack returned no user for email %q", decoded)
		}
		return u.ID, nil

	case strings.HasPrefix(decoded, "slack:"):
		// `slack:<team>:<userID>` (team may be empty: `slack::U…`).
		parts := strings.SplitN(decoded, ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			return "", fmt.Errorf("resolve slack user: malformed slack-typed canonical %q", decoded)
		}
		return parts[2], nil

	default:
		return "", fmt.Errorf("resolve slack user: unrecognized canonical form %q (expected an email or slack:<team>:<id>)", decoded)
	}
}
