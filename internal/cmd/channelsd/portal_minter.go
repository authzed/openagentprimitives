// channelsdPortalMinter is the channelsd-side implementation of
// channelkinds.PortalLinkMinter. Lives in internal/cmd/channelsd (not in
// pkg/channels/channelkinds) because it depends on passthroughlink.Signer +
// the live externalurl Provider — both channelsd-process concerns
// that pkg/channels/channelkinds deliberately stays free of.
//
// Used by the slack App Home renderer to deep-link "Manage my
// connections" into identityd's /my/accounts portal. The link is a
// purpose="portal" passthroughlink.Payload signed for the clicker's
// canonical subject, valid for 10 minutes (matching
// pipeline.portalAccessTTL). identityd verifies + sets the
// idd_session cookie + renders the portal.
//
// Errors degrade silently: a failed mint just omits the button on
// that App Home render. The next app_home_opened event re-tries.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// portalAccessTTL is the TTL identityd will honor on a portal-purpose
// signed link. Matches pipeline.PortalAccessTriggerer's TTL — keep
// the two in sync if either side moves. Hard-coding here (rather than
// importing the pipeline constant) keeps internal/cmd/channelsd's import
// surface flat.
const portalAccessTTL = 10 * time.Minute

// channelsdPortalMinter mints purpose="portal" signed deep-links
// from the per-process passthroughlink.Signer + the live external
// base URL (which changes when `oap init --local` patches the
// spicebox-webd-external-url ConfigMap; see externalurl
// package for the live-update mechanism).
type channelsdPortalMinter struct {
	signer          *passthroughlink.Signer
	externalBaseURL func() string
}

// MintPortalLink returns "<base>/my/accounts?d=<b64>&sig=<hex>" for
// the given principal. Returns an error when the external base URL is
// empty (channelsd booted before `oap init --local` had a chance to
// populate it) so callers can degrade gracefully.
func (m *channelsdPortalMinter) MintPortalLink(ctx context.Context, subject identity.Principal) (string, error) {
	if m == nil || m.signer == nil {
		return "", fmt.Errorf("portal minter not configured")
	}
	sub, err := subject.Subject()
	if err != nil {
		return "", fmt.Errorf("portal minter: subject unresolved (no verified email): %w", err)
	}
	if sub == "" {
		return "", fmt.Errorf("empty subject")
	}
	base := strings.TrimRight(m.externalBaseURL(), "/")
	if base == "" {
		return "", fmt.Errorf("externalBaseURL not yet known")
	}
	raw, err := m.signer.Mint(passthroughlink.Payload{
		Subject:         sub,
		SubjectVerified: subject.EmailVerified(),
		Purpose:         "portal",
		ExpiresAt:       time.Now().Add(portalAccessTTL).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("mint portal link: %w", err)
	}
	// Mint returns "<b64>.<sig>"; identityd's /my/accounts handler
	// parses ?d= + ?sig= separately so we split the dot here.
	idx := strings.IndexByte(raw, '.')
	if idx <= 0 || idx == len(raw)-1 {
		return "", fmt.Errorf("malformed signed link from Mint")
	}
	return base + "/my/accounts?d=" + raw[:idx] + "&sig=" + raw[idx+1:], nil
}
