// Package desktop implements the cloud.Strategy for `oap desktop`'s guest
// cluster: a network-confined, single-user VM (loopback port-forward only, no
// public ingress), as against `oap init --local`, which tunnels the same
// lightweight dev profile publicly.
//
// The two kinds differ in exactly one profile answer, ServesLocalWebChat, which
// no binary reads — so the installs are in effect identical. What SHOULD
// distinguish them is an open cluster-kind question.
//
// It embeds local.Strategy and overrides ONLY Key, DisplayName, and
// InstallProfile; every other method (Validate included) is inherited verbatim
// rather than re-implemented, so the two kinds cannot drift apart by accident.
// It reports no ProviderIDPrefix, so cloud.Detect can never select it —
// `oap desktop` is the sole caller that selects cloud.KeyDesktop.
package desktop

import (
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
)

func init() {
	cloud.Register(Strategy{}, cloud.KeyDesktop)
}

// Strategy is local.Strategy with Key, DisplayName, and InstallProfile
// overridden; every other method is promoted from the embedding unchanged.
type Strategy struct {
	local.Strategy
}

// Key returns the `desktop` kind key.
func (Strategy) Key() string { return cloud.KeyDesktop }

// DisplayName returns a human-readable label for messages.
func (Strategy) DisplayName() string { return "oap desktop VM" }

// InstallProfile returns local's dev profile with ServesLocalWebChat flipped on
// — the only difference from `local`. See cloud.InstallProfile.ServesLocalWebChat.
func (Strategy) InstallProfile() cloud.InstallProfile { return cloud.DesktopDevProfile }
