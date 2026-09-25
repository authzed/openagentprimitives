// Package adminui is the webd plugin for the platform admin UI: the
// /admin React page plus /admin/api/* reverse-proxy routes to the
// operator-mounted admind API. Every route is gated by a SpiceDB
// platform per-area permission; the plugin fails closed (no routes)
// when admind isn't configured.
package adminui

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Deps is the plugin's dependency interface; internal/cmd/webd's concrete deps
// value implements it (cast from webui.Deps, fail-closed on mismatch).
type Deps interface {
	// AdmindBaseURL is the operator service base URL serving /admin/v1/*
	// (e.g. http://spicebox-operator.agentprimitives-system.svc:8082).
	// Empty disables the plugin.
	AdmindBaseURL() string
	// AdmindToken is the spicebox-admind-token value forwarded as the
	// proxy's bearer token. Empty disables the plugin.
	AdmindToken() string
	// CheckPlatformPermission mirrors (*spicedb.Client).CheckPlatformPermission.
	CheckPlatformPermission(ctx context.Context, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// TrustedOrigin is the live trusted-origin base URL, used to pin the Origin
	// header on every STATE-CHANGING admin route (CSRF).
	//
	// The framework already ships this pin and seven other surfaces use it;
	// adminui used it at none of them, so its mutating routes -- kill a session,
	// install an agent, submit channel credentials -- were cookie-authenticated
	// POSTs with nothing binding them to this origin.
	TrustedOrigin() string
	Logger() logr.Logger
}
