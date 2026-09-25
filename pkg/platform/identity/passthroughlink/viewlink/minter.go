// Package viewlink mints webd artifact-view deep-links from a
// passthroughlink Signer + a live webd base URL. Shared by channelsd
// (Slack "View live" buttons) and `oap agent chat` (TUI links).
package viewlink

import (
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// ArtifactViewLinkTTL is the TTL for artifact-view deep-links. 30 minutes
// gives the user enough time to follow the link after it is offered.
const ArtifactViewLinkTTL = 30 * time.Minute

// Minter mints purpose="artifact_view" signed deep-links from a
// passthroughlink.Signer and a live webd base URL getter. Implements
// channelkinds.ArtifactViewMinter (matched by method signature).
type Minter struct {
	// Signer is the HMAC signer used to mint the payload. Must be non-nil;
	// callers typically guard on Signer != nil before constructing a Minter.
	Signer *passthroughlink.Signer
	// WebdBaseURL is a live getter for webd's externally reachable base URL.
	// Returns "" when webd is not yet configured; MintArtifactViewLink returns
	// ("", nil) in that case — a clean skip for callers.
	WebdBaseURL func() string
}

// MintArtifactViewLink returns "<base>/artifact-view?d=<b64>&sig=<hex>" for
// the given artifact, session, and subject. Returns ("", nil) when the webd
// base URL is empty (webd not yet installed or ConfigMap not yet populated) —
// callers treat an empty URL as a clean skip (no post, no error log).
func (m *Minter) MintArtifactViewLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return m.mint("/artifact-view", artifactID, sessionRef, subject, backLink)
}

// MintArtifactDownloadLink returns "<base>/artifact-download?d=<b64>&sig=<hex>":
// the SAME signed, view-authorized payload as the live-view link, pointed at
// webd's direct-download endpoint (which serves the artifact bytes with
// Content-Disposition: attachment). A download is a presentation of the same
// view grant, so it reuses PurposeArtifactView and the same CheckView gate —
// no separate authorization. Returns ("", nil) on an empty base URL, same clean
// skip as the view link (the desktop case with no external origin wired).
func (m *Minter) MintArtifactDownloadLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return m.mint("/artifact-download", artifactID, sessionRef, subject, backLink)
}

// mint builds a signed webd deep-link at the given path. The payload is
// identical across link kinds (view/download); only the path differs, so the
// same signature authorizes both under the one CheckView gate.
func (m *Minter) mint(path, artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	if m == nil || m.Signer == nil {
		return "", fmt.Errorf("artifact view minter not configured")
	}
	if artifactID == "" {
		return "", fmt.Errorf("empty artifactID")
	}
	base := strings.TrimRight(m.WebdBaseURL(), "/")
	if base == "" {
		// ConfigMap not yet populated; caller treats "" as a clean skip.
		return "", nil
	}
	subj, err := subject.Subject()
	if err != nil {
		return "", fmt.Errorf("mint artifact link: subject unresolved (no verified email): %w", err)
	}
	raw, err := m.Signer.Mint(passthroughlink.Payload{
		Purpose:         passthroughlink.PurposeArtifactView,
		Audience:        passthroughlink.AudienceWebd, // webd verifies with WithExpectedAudience(AudienceWebd)
		ArtifactID:      artifactID,
		SessionRef:      sessionRef,
		Subject:         subj,
		SubjectVerified: subject.EmailVerified(),
		BackLink:        backLink,
		ExpiresAt:       time.Now().Add(ArtifactViewLinkTTL).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("mint artifact link: %w", err)
	}
	// Mint returns "<b64>.<sig>"; webd's handler parses ?d= + ?sig= separately
	// so we split the dot here. Mirrors how
	// pkg/channels/channelsd/pipeline/credential_request.go's buildLinkURL works.
	idx := strings.IndexByte(raw, '.')
	if idx <= 0 || idx == len(raw)-1 {
		return "", fmt.Errorf("malformed signed link from Mint")
	}
	return base + path + "?d=" + raw[:idx] + "&sig=" + raw[idx+1:], nil
}
