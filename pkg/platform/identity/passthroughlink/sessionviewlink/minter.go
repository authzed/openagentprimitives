// Package sessionviewlink mints purpose="session_view" signed deep-links: a
// long-lived (7-day), shareable bearer capability for the session-view page
// (pkg/web/webui/sessionview), the shared-transcript mirror of a source
// channel's conversation.
//
// It mirrors passthroughlink/viewlink's mint shape but is scoped to a SESSION
// rather than an ARTIFACT: no ArtifactID, and no per-path URL building — callers
// compose the URL. This package is mint only; verification goes through the
// shared passthroughlink.Signer.
package sessionviewlink

import (
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// SessionViewLinkTTL is the TTL for session-view deep-links: long-lived and
// shareable (7 days) rather than the 30-minute ArtifactViewLinkTTL. Extending a
// link means re-minting it; nothing here renews one in place.
const SessionViewLinkTTL = 7 * 24 * time.Hour

// Minter mints purpose="session_view" signed deep-links from a
// passthroughlink.Signer.
type Minter struct {
	// Signer is the HMAC signer used to mint the payload. Must be non-nil;
	// callers typically guard on Signer != nil before constructing a Minter.
	Signer *passthroughlink.Signer
}

// Mint returns the raw signed "<b64>.<sig>" token authorizing sessionRef for
// subject, with backLink carried through (tamper-proof, not confidential) for
// the consuming page's "back to origin" affordance. Callers compose the final
// deep-link URL around this token, using the same "?d=<b64>&sig=<hex>" split
// viewlink's minters use.
func (m *Minter) Mint(sessionRef string, subject identity.Principal, backLink string) (string, error) {
	if m == nil || m.Signer == nil {
		return "", fmt.Errorf("session view minter not configured")
	}
	if sessionRef == "" {
		return "", fmt.Errorf("empty sessionRef")
	}
	subj, err := subject.Subject()
	if err != nil {
		return "", fmt.Errorf("mint session view link: subject unresolved (no verified email): %w", err)
	}
	raw, err := m.Signer.Mint(passthroughlink.Payload{
		Purpose:         passthroughlink.PurposeSessionView,
		Audience:        passthroughlink.AudienceWebd, // webd verifies with WithExpectedAudience(AudienceWebd)
		SessionRef:      sessionRef,
		Subject:         subj,
		SubjectVerified: subject.EmailVerified(),
		BackLink:        backLink,
		ExpiresAt:       time.Now().Add(SessionViewLinkTTL).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("mint session view link: %w", err)
	}
	return raw, nil
}
