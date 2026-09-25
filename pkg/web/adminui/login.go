package adminui

import (
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

const loginLinkTTL = 10 * time.Minute

// LoginLink mints the session-less admin-login deep link (d, sig) that
// internal/cmd/webd's beginLogin attaches when a cookie-less browser hits /admin.
// Lives here (not in internal/cmd/webd) so the admin-login knowledge stays in the
// plugin package.
func LoginLink(signer *passthroughlink.Signer) (d, sig string, err error) {
	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeAdminLogin,
		ExpiresAt: time.Now().Add(loginLinkTTL).Unix(),
	})
	if err != nil {
		return "", "", err
	}
	i := strings.LastIndexByte(raw, '.')
	return raw[:i], raw[i+1:], nil
}
